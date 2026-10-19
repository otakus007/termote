package main

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	maxCrewClockSkew      = 5 * time.Minute
	headerCrewSignature   = "X-Crew-Signature"
	headerCrewTimestamp   = "X-Crew-Timestamp"
	headerCrewSenderID    = "X-Crew-Sender-Id"
	headerCrewAuthHMAC    = "X-Termote-Crew-Auth"
	headerCrewClientRole  = "X-Termote-Client-Role"
	defaultWarrantHorizon = 7 * 24 * time.Hour
)

// DeputyWarrant is a Lead-signed capability authorizing a designated Standby
// Deputy node to poll and proxy Member nodes during failover.
type DeputyWarrant struct {
	CrewID           string `json:"crewId"`
	LeadID           string `json:"leadId"`
	DeputyID         string `json:"deputyId"`
	DeputyCertSha256 string `json:"deputyCertSha256"`
	IssuedAt         string `json:"issuedAt"`
	ExpiresAt        string `json:"expiresAt"`
	Signature        string `json:"signature"`
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func certFingerprintSha256(rawDER []byte) string {
	return sha256Hex(rawDER)
}

func constantTimeHexEqual(a, b string) bool {
	na := strings.ToLower(strings.TrimSpace(a))
	nb := strings.ToLower(strings.TrimSpace(b))
	if len(na) == 0 || len(na) != len(nb) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(na), []byte(nb)) == 1
}

// canonicalCrewRequest builds the canonical signing string:
//
//	METHOD\n
//	path\n
//	sha256_hex(body)\n
//	timestamp
func canonicalCrewRequest(method, path, bodySha256Hex, timestamp string) string {
	m := strings.ToUpper(strings.TrimSpace(method))
	p := strings.TrimSpace(path)
	if p == "" {
		p = "/"
	}
	h := strings.ToLower(strings.TrimSpace(bodySha256Hex))
	ts := strings.TrimSpace(timestamp)
	return m + "\n" + p + "\n" + h + "\n" + ts
}

// signCrewRequest signs canonicalCrewRequest with an ECDSA P-256 private key
// and returns a base64-encoded ASN.1 DER signature.
func signCrewRequest(priv *ecdsa.PrivateKey, method, path string, body []byte, timestamp string) (string, error) {
	if priv == nil {
		return "", errors.New("nil private key")
	}
	canonical := canonicalCrewRequest(method, path, sha256Hex(body), timestamp)
	digest := sha256.Sum256([]byte(canonical))
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign crew request: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// parseCrewTimestamp parses either a Unix-millisecond integer (or Unix seconds)
// or an RFC3339/RFC3339Nano timestamp string, returning both the normalized
// Unix-millisecond value for monotonic comparison and the corresponding time.Time.
func parseCrewTimestamp(raw string) (int64, time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, time.Time{}, errors.New("missing timestamp")
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return 0, time.Time{}, errors.New("non-positive timestamp")
		}
		// Treat 10-digit values as seconds, 13-digit values as milliseconds.
		if n < 100_000_000_000 {
			n *= 1000
		}
		return n, time.UnixMilli(n).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli(), t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), t.UTC(), nil
	}
	return 0, time.Time{}, fmt.Errorf("invalid timestamp format %q", raw)
}

// verifyCrewRequest verifies an X-Crew-Signature and X-Crew-Timestamp against
// the sender's ECDSA P-256 public key and ±5m clock-skew window.
func verifyCrewRequest(pub *ecdsa.PublicKey, method, path string, body []byte, timestamp, sigB64 string, now time.Time) (int64, error) {
	if pub == nil {
		return 0, errors.New("nil public key")
	}
	tsMs, tsTime, err := parseCrewTimestamp(timestamp)
	if err != nil {
		return 0, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	skew := now.Sub(tsTime)
	if skew < -maxCrewClockSkew || skew > maxCrewClockSkew {
		return 0, fmt.Errorf("crew timestamp skew %s exceeds ±%s", skew.Round(time.Second), maxCrewClockSkew)
	}
	sig, err := decodeBase64Flexible(sigB64)
	if err != nil {
		return 0, fmt.Errorf("invalid signature encoding: %w", err)
	}
	canonical := canonicalCrewRequest(method, path, sha256Hex(body), timestamp)
	digest := sha256.Sum256([]byte(canonical))
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		return 0, errors.New("invalid crew ECDSA signature")
	}
	return tsMs, nil
}

func decodeBase64Flexible(s string) ([]byte, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, errors.New("empty base64 string")
	}
	if b, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(trimmed); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(trimmed); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(trimmed)
}

func decodeSecretBytes(secretHex string) ([]byte, error) {
	trimmed := strings.TrimSpace(secretHex)
	if trimmed == "" {
		return nil, errors.New("empty crew secret")
	}
	if b, err := hex.DecodeString(trimmed); err == nil && len(b) >= 16 {
		return b, nil
	}
	if len(trimmed) >= 16 {
		return []byte(trimmed), nil
	}
	return nil, errors.New("crew secret too short")
}

func computeCrewAuthHMAC(secretHex, method, path string, body []byte, timestamp string) (string, error) {
	key, err := decodeSecretBytes(secretHex)
	if err != nil {
		return "", err
	}
	canonical := canonicalCrewRequest(method, path, sha256Hex(body), timestamp)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func verifyCrewAuthHMAC(secretHex, method, path string, body []byte, timestamp, providedHex string, now time.Time) error {
	if _, tsTime, err := parseCrewTimestamp(timestamp); err != nil {
		return err
	} else {
		if now.IsZero() {
			now = time.Now().UTC()
		}
		skew := now.Sub(tsTime)
		if skew < -maxCrewClockSkew || skew > maxCrewClockSkew {
			return fmt.Errorf("crew HMAC timestamp skew %s exceeds ±%s", skew.Round(time.Second), maxCrewClockSkew)
		}
	}
	expected, err := computeCrewAuthHMAC(secretHex, method, path, body, timestamp)
	if err != nil {
		return err
	}
	if !constantTimeHexEqual(providedHex, expected) {
		return errors.New("invalid X-Termote-Crew-Auth HMAC")
	}
	return nil
}

func canonicalDeputyWarrant(crewID, deputyID, deputyCertSha256, issuedAt, expiresAt string) string {
	return fmt.Sprintf(
		"warrant:v1\n%s\n%s\n%s\n%s\n%s",
		strings.TrimSpace(crewID),
		strings.TrimSpace(deputyID),
		strings.ToLower(strings.TrimSpace(deputyCertSha256)),
		strings.TrimSpace(issuedAt),
		strings.TrimSpace(expiresAt),
	)
}

func signDeputyWarrant(priv *ecdsa.PrivateKey, crewID, leadID, deputyID, deputyCertSha256 string, issuedAt, expiresAt time.Time) (*DeputyWarrant, error) {
	if priv == nil {
		return nil, errors.New("nil lead private key")
	}
	issuedStr := issuedAt.UTC().Format(time.RFC3339)
	expiresStr := expiresAt.UTC().Format(time.RFC3339)
	fp := strings.ToLower(strings.TrimSpace(deputyCertSha256))
	canonical := canonicalDeputyWarrant(crewID, deputyID, fp, issuedStr, expiresStr)
	digest := sha256.Sum256([]byte(canonical))
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign deputy warrant: %w", err)
	}
	return &DeputyWarrant{
		CrewID:           crewID,
		LeadID:           leadID,
		DeputyID:         deputyID,
		DeputyCertSha256: fp,
		IssuedAt:         issuedStr,
		ExpiresAt:        expiresStr,
		Signature:        base64.StdEncoding.EncodeToString(sig),
	}, nil
}

func verifyDeputyWarrant(leadPub *ecdsa.PublicKey, w *DeputyWarrant, expectedCrewID string, now time.Time) error {
	if leadPub == nil {
		return errors.New("nil lead public key")
	}
	if w == nil {
		return errors.New("missing deputy warrant")
	}
	if expectedCrewID != "" && w.CrewID != "" && w.CrewID != expectedCrewID {
		return fmt.Errorf("warrant crewId %q does not match %q", w.CrewID, expectedCrewID)
	}
	if w.DeputyID == "" || w.DeputyCertSha256 == "" {
		return errors.New("incomplete deputy warrant")
	}
	_, issuedTime, err := parseCrewTimestamp(w.IssuedAt)
	if err != nil {
		return fmt.Errorf("invalid warrant issuedAt: %w", err)
	}
	_, expiresTime, err := parseCrewTimestamp(w.ExpiresAt)
	if err != nil {
		return fmt.Errorf("invalid warrant expiresAt: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if now.Before(issuedTime.Add(-maxCrewClockSkew)) {
		return errors.New("deputy warrant not yet valid")
	}
	if now.After(expiresTime.Add(maxCrewClockSkew)) {
		return errors.New("deputy warrant expired")
	}
	sig, err := decodeBase64Flexible(w.Signature)
	if err != nil {
		return fmt.Errorf("invalid warrant signature encoding: %w", err)
	}
	canonical := canonicalDeputyWarrant(w.CrewID, w.DeputyID, w.DeputyCertSha256, w.IssuedAt, w.ExpiresAt)
	digest := sha256.Sum256([]byte(canonical))
	if !ecdsa.VerifyASN1(leadPub, digest[:], sig) {
		return errors.New("invalid deputy warrant signature")
	}
	return nil
}

func parseCertPEM(certPEM string) (*x509.Certificate, string, *ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(certPEM)))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, "", nil, errors.New("invalid certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", nil, fmt.Errorf("parse certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, "", nil, errors.New("certificate does not contain an ECDSA public key")
	}
	return cert, certFingerprintSha256(cert.Raw), pub, nil
}
