package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type tlsOrHTTPListener struct {
	net.Listener
	tlsConfig *tls.Config
}

type peekedConn struct {
	net.Conn
	buf []byte
	err error
}

func (c *peekedConn) Read(p []byte) (int, error) {
	if len(c.buf) > 0 {
		n := copy(p, c.buf)
		c.buf = c.buf[n:]
		if len(c.buf) == 0 && c.err != nil {
			err := c.err
			c.err = nil
			return n, err
		}
		return n, nil
	}
	if c.err != nil {
		err := c.err
		c.err = nil
		return 0, err
	}
	return c.Conn.Read(p)
}

func newTLSOrHTTPListener(ln net.Listener, tlsConfig *tls.Config) net.Listener {
	if tlsConfig == nil {
		return ln
	}
	return &tlsOrHTTPListener{Listener: ln, tlsConfig: tlsConfig}
}

func (l *tlsOrHTTPListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if l.tlsConfig == nil {
		return conn, nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var first [1]byte
	n, rerr := conn.Read(first[:])
	_ = conn.SetReadDeadline(time.Time{})
	if n == 0 && isTimeout(rerr) {
		rerr = nil
	}
	pc := &peekedConn{Conn: conn, buf: first[:n], err: rerr}
	if n == 1 && first[0] == 0x16 {
		return tls.Server(pc, l.tlsConfig), nil
	}
	return pc, nil
}

const (
	maxCrewControlBody = 1 << 20  // 1 MiB for control payloads (join/warrant/leave)
	maxCrewProxyBody   = 16 << 20 // 16 MiB cap on proxied HTTP payloads (R5.7)
)

type crewJoinRequest struct {
	InviteID   string `json:"inviteId"`
	SecretHex  string `json:"secretHex"`
	MemberID   string `json:"memberId"`
	MemberName string `json:"memberName"`
	MemberURL  string `json:"memberUrl"`
	CertPEM    string `json:"certPem"`
	CertSha256 string `json:"certSha256"`
}

type crewJoinResponse struct {
	OK             bool               `json:"ok"`
	CrewID         string             `json:"crewId"`
	LeadID         string             `json:"leadId"`
	LeadName       string             `json:"leadName"`
	LeadCertPEM    string             `json:"leadCertPem"`
	LeadCertSha256 string             `json:"leadCertSha256"`
	CrewSecretHex  string             `json:"crewSecretHex"`
	Warrant        *DeputyWarrant     `json:"warrant,omitempty"`
	Roster         []CrewMemberPublic `json:"roster,omitempty"`
}

type crewWarrantSyncRequest struct {
	CrewID  string             `json:"crewId"`
	Warrant *DeputyWarrant     `json:"warrant,omitempty"`
	Roster  []CrewMemberPublic `json:"roster,omitempty"`
}

type crewLeaveRequest struct {
	MemberID string `json:"memberId"`
}

// PeerTLSConfig returns a TLS 1.3 server configuration with dynamic client
// certificate verification via GetConfigForClient.
//
// When this node is a joined Member or Standby Deputy, incoming peer
// connections must present a client certificate whose SHA-256 digest matches
// (in constant time) either the pinned Lead certificate or a Deputy certificate
// backed by a valid Lead-signed DeputyWarrant.
func (s *CrewStore) PeerTLSConfig() *tls.Config {
	ident := s.Identity()
	base := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{ident.TLSCert},
	}
	base.GetConfigForClient = func(_ *tls.ClientHelloInfo) (*tls.Config, error) {
		curIdent := s.Identity()
		st := s.StateSnapshot()
		cfg := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{curIdent.TLSCert},
		}
		if st.Lead != nil && st.Role != CrewRoleLead {
			cfg.ClientAuth = tls.RequireAnyClientCert
			cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) == 0 {
					return errors.New("missing mTLS client certificate")
				}
				if constantTimeHexEqual(certFingerprintSha256(rawCerts[0]), curIdent.CertSha256) {
					return nil
				}
				_, _, _, err := s.VerifyControllerCert(rawCerts[0], time.Now().UTC())
				return err
			}
		} else {
			cfg.ClientAuth = tls.RequestClientCert
		}
		return cfg, nil
	}
	return base
}

// ClientTLSConfigForPeer builds a TLS 1.3 client configuration that presents
// this node's ECDSA P-256 certificate and pins the remote peer's SHA-256
// certificate digest using constant-time comparison.
func (s *CrewStore) ClientTLSConfigForPeer(expectedCertSha256 string) *tls.Config {
	ident := s.Identity()
	expected := strings.ToLower(strings.TrimSpace(expectedCertSha256))
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{ident.TLSCert},
		InsecureSkipVerify: true, // Verified explicitly via VerifyPeerCertificate below.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("peer presented no TLS certificate")
			}
			actual := certFingerprintSha256(rawCerts[0])
			if !constantTimeHexEqual(actual, expected) {
				return fmt.Errorf("peer certificate SHA-256 %s does not match pinned %s", actual, expected)
			}
			return nil
		},
	}
}

func (m *CrewManager) registerProtocolRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /crew/v1/join", m.handleCrewJoin)
	mux.HandleFunc("POST /crew/v1/leave", m.handleCrewLeave)
	mux.HandleFunc("POST /crew/v1/heartbeat", m.handleCrewHeartbeat)
	mux.HandleFunc("POST /crew/v1/promote", m.handleCrewPromotePeer)

	mux.HandleFunc("GET /crew/v1/snapshot", m.handleCrewPeerSnapshot)
	mux.HandleFunc("GET /crew/v1/overview", m.handleCrewPeerOverview)
	mux.HandleFunc("GET /crew/v1/machine", m.handleCrewPeerMachine)
	mux.HandleFunc("POST /crew/v1/warrant", m.handleCrewPeerWarrant)
	mux.HandleFunc("GET /crew/v1/stream", m.handleCrewPeerStream)
	mux.HandleFunc("/crew/v1/proxy/", m.handleCrewPeerProxy)
}

// authenticateControllerRequest verifies that an incoming Lead/Deputy -> Member
// request arrived over TLS 1.3 mTLS from a pinned Lead or warranted Deputy,
// carries a valid ECDSA P-256 signature + fresh monotonic timestamp, and
// includes a valid X-Termote-Crew-Auth HMAC. Returns the propagated client role
// ("view" or "full").
func (m *CrewManager) authenticateControllerRequest(r *http.Request, body []byte) (role, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", errors.New("mTLS client certificate required")
	}
	if r.TLS.Version < tls.VersionTLS13 {
		return "", errors.New("TLS 1.3 required for crew peer connections")
	}
	now := time.Now().UTC()
	senderID, pub, sharedSecret, err := m.store.VerifyControllerCert(r.TLS.PeerCertificates[0].Raw, now)
	if err != nil {
		return "", err
	}

	tsHeader := strings.TrimSpace(r.Header.Get(headerCrewTimestamp))
	sigHeader := strings.TrimSpace(r.Header.Get(headerCrewSignature))
	authHeader := strings.TrimSpace(r.Header.Get(headerCrewAuthHMAC))
	if tsHeader == "" || sigHeader == "" {
		return "", errors.New("missing X-Crew-Timestamp or X-Crew-Signature")
	}
	reqPath := r.URL.RequestURI()
	tsMs, err := verifyCrewRequest(pub, r.Method, reqPath, body, tsHeader, sigHeader, now)
	if err != nil {
		return "", err
	}
	if sharedSecret != "" {
		if authHeader == "" {
			return "", errors.New("missing X-Termote-Crew-Auth HMAC")
		}
		if err := verifyCrewAuthHMAC(sharedSecret, r.Method, reqPath, body, tsHeader, authHeader, now); err != nil {
			return "", err
		}
	}
	if err := m.store.CheckAndAdvanceTimestamp(senderID, tsMs, now); err != nil {
		return "", err
	}

	clientRole := roleView
	if strings.EqualFold(strings.TrimSpace(r.Header.Get(headerCrewClientRole)), string(roleFull)) {
		clientRole = roleFull
	}
	return clientRole, nil
}

// handleCrewJoin handles POST /crew/v1/join on the Lead's front door.
func (m *CrewManager) handleCrewJoin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCrewControlBody))
	if err != nil {
		jsonError(w, "read request body failed", http.StatusBadRequest)
		return
	}
	var req crewJoinRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid join JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.InviteID) == "" || strings.TrimSpace(req.SecretHex) == "" {
		jsonError(w, "inviteId and secretHex are required", http.StatusBadRequest)
		return
	}
	_, fp, memberPub, err := parseCertPEM(req.CertPEM)
	if err != nil {
		jsonError(w, fmt.Sprintf("invalid member certificate: %v", err), http.StatusBadRequest)
		return
	}
	if req.CertSha256 != "" && !constantTimeHexEqual(fp, req.CertSha256) {
		jsonError(w, "member certSha256 does not match certificate PEM", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	tsHeader := strings.TrimSpace(r.Header.Get(headerCrewTimestamp))
	sigHeader := strings.TrimSpace(r.Header.Get(headerCrewSignature))
	tsMs, err := verifyCrewRequest(memberPub, r.Method, r.URL.RequestURI(), body, tsHeader, sigHeader, now)
	if err != nil {
		jsonError(w, fmt.Sprintf("invalid join signature: %v", err), http.StatusUnauthorized)
		return
	}

	memberID := strings.TrimSpace(req.MemberID)
	if memberID == "" {
		memberID = "node-" + fp[:12]
	}
	if err := m.store.CheckAndAdvanceTimestamp(memberID, tsMs, now); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}

	member := CrewMember{
		ID:              memberID,
		Name:            strings.TrimSpace(req.MemberName),
		URL:             strings.TrimRight(strings.TrimSpace(req.MemberURL), "/"),
		CertSha256:      fp,
		CertPEM:         strings.TrimSpace(req.CertPEM),
		Role:            CrewRoleMember,
		JoinedAt:        now.Format(time.RFC3339),
		LastSeen:        now.Format(time.RFC3339),
		LastTimestampMs: tsMs,
	}
	crewID, crewSecretHex, err := m.store.ConsumeInviteAndRegisterMember(req.InviteID, req.SecretHex, member, now)
	if err != nil {
		jsonError(w, err.Error(), http.StatusForbidden)
		return
	}

	ident := m.store.Identity()
	st := m.store.StateSnapshot()
	resp := crewJoinResponse{
		OK:             true,
		CrewID:         crewID,
		LeadID:         ident.ID,
		LeadName:       ident.Name,
		LeadCertPEM:    ident.CertPEM,
		LeadCertSha256: ident.CertSha256,
		CrewSecretHex:  crewSecretHex,
		Warrant:        st.Warrant,
		Roster:         m.store.PublicRoster(),
	}

	// Mark cache dirty so the next AugmentSnapshot or background tick polls fresh state.
	m.mu.Lock()
	m.polled = false
	m.mu.Unlock()
	if st.Warrant != nil {
		go m.replicateWarrantAndRoster()
	}

	jsonOK(w, resp)
}

func (m *CrewManager) verifyMemberSignedFrontDoor(r *http.Request, body []byte) (CrewMember, error) {
	senderID := strings.TrimSpace(r.Header.Get(headerCrewSenderID))
	if senderID == "" {
		return CrewMember{}, errors.New("missing X-Crew-Sender-Id")
	}
	member, ok := m.store.GetMember(senderID)
	if !ok {
		return CrewMember{}, fmt.Errorf("unknown crew member %q", senderID)
	}
	_, fp, pub, err := parseCertPEM(member.CertPEM)
	if err != nil {
		return CrewMember{}, fmt.Errorf("member certificate unavailable: %w", err)
	}
	if !constantTimeHexEqual(fp, member.CertSha256) {
		return CrewMember{}, errors.New("member certificate fingerprint mismatch")
	}
	now := time.Now().UTC()
	tsHeader := strings.TrimSpace(r.Header.Get(headerCrewTimestamp))
	sigHeader := strings.TrimSpace(r.Header.Get(headerCrewSignature))
	tsMs, err := verifyCrewRequest(pub, r.Method, r.URL.RequestURI(), body, tsHeader, sigHeader, now)
	if err != nil {
		return CrewMember{}, err
	}
	if err := m.store.CheckAndAdvanceTimestamp(member.ID, tsMs, now); err != nil {
		return CrewMember{}, err
	}
	return member, nil
}

func (m *CrewManager) handleCrewLeave(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCrewControlBody))
	if err != nil {
		jsonError(w, "read body failed", http.StatusBadRequest)
		return
	}
	member, err := m.verifyMemberSignedFrontDoor(r, body)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	_, _, _ = m.store.RemoveMember(member.ID)
	m.removeMemberCache(member.ID)
	go m.replicateWarrantAndRoster()
	jsonOK(w, map[string]any{"ok": true})
}

func (m *CrewManager) handleCrewHeartbeat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCrewControlBody))
	if err != nil {
		jsonError(w, "read body failed", http.StatusBadRequest)
		return
	}
	if _, err := m.verifyMemberSignedFrontDoor(r, body); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	st := m.store.StateSnapshot()
	jsonOK(w, crewWarrantSyncRequest{
		CrewID:  st.CrewID,
		Warrant: st.Warrant,
		Roster:  m.store.PublicRoster(),
	})
}

func (m *CrewManager) handleCrewPromotePeer(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCrewControlBody))
	if err != nil {
		jsonError(w, "read body failed", http.StatusBadRequest)
		return
	}
	role, err := m.authenticateControllerRequest(r, body)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if role != roleFull {
		jsonError(w, "view-only role cannot promote deputy", http.StatusForbidden)
		return
	}
	if err := m.Promote(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"ok": true, "role": m.store.Role()})
}

func (m *CrewManager) handleCrewPeerSnapshot(w http.ResponseWriter, r *http.Request) {
	clientRole, err := m.authenticateControllerRequest(r, nil)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	r2 := r.WithContext(withAuth(r.Context(), authInfo{Kind: authDevice, Role: clientRole}))
	snap, err := m.buildLocalSnapshot(r2)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonOK(w, snap)
}

func (m *CrewManager) handleCrewPeerOverview(w http.ResponseWriter, r *http.Request) {
	clientRole, err := m.authenticateControllerRequest(r, nil)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	r2 := r.WithContext(withAuth(r.Context(), authInfo{Kind: authDevice, Role: clientRole}))
	resp, err := m.buildLocalOverview(r2)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonOK(w, resp)
}

func (m *CrewManager) handleCrewPeerMachine(w http.ResponseWriter, r *http.Request) {
	clientRole, err := m.authenticateControllerRequest(r, nil)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	r2 := r.WithContext(withAuth(r.Context(), authInfo{Kind: authDevice, Role: clientRole}))
	resp := m.buildLocalMachineStatus(r2)
	jsonOK(w, resp)
}

func (m *CrewManager) handleCrewPeerWarrant(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCrewControlBody))
	if err != nil {
		jsonError(w, "read body failed", http.StatusBadRequest)
		return
	}
	clientRole, err := m.authenticateControllerRequest(r, body)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if clientRole != roleFull {
		jsonError(w, "view-only role cannot update warrant", http.StatusForbidden)
		return
	}
	var req crewWarrantSyncRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid warrant sync payload", http.StatusBadRequest)
		return
	}
	if err := m.store.ApplyWarrantUpdate(req.Warrant, req.Roster, time.Now().UTC()); err != nil {
		jsonError(w, err.Error(), http.StatusForbidden)
		return
	}
	jsonOK(w, map[string]any{"ok": true, "role": m.store.Role()})
}

func (m *CrewManager) handleCrewPeerProxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCrewProxyBody+1))
	if err != nil {
		jsonError(w, "read proxy body failed", http.StatusBadRequest)
		return
	}
	if len(body) > maxCrewProxyBody {
		jsonError(w, "proxied request body exceeds 16 MiB limit", http.StatusRequestEntityTooLarge)
		return
	}
	clientRole, err := m.authenticateControllerRequest(r, body)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if m.innerMux == nil {
		jsonError(w, "member mux unavailable", http.StatusServiceUnavailable)
		return
	}

	targetPath := strings.TrimPrefix(r.URL.Path, "/crew/v1/proxy")
	if !strings.HasPrefix(targetPath, "/api/mux/") {
		jsonError(w, "crew proxy only permits /api/mux/* targets", http.StatusForbidden)
		return
	}

	r2 := r.Clone(withAuth(r.Context(), authInfo{Kind: authDevice, Role: clientRole}))
	r2.URL.Path = targetPath
	r2.URL.RawPath = ""
	r2.RequestURI = r2.URL.RequestURI()
	r2.Header.Del("Origin")
	r2.Header.Del("Sec-Fetch-Site")
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))

	// R5.5: Enforce view-only write denial on the Member before dispatching.
	if denyViewWrites(w, r2) {
		return
	}
	m.innerMux.ServeHTTP(w, r2)
}

func (m *CrewManager) handleCrewPeerStream(w http.ResponseWriter, r *http.Request) {
	clientRole, err := m.authenticateControllerRequest(r, nil)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	r2 := r.WithContext(withAuth(r.Context(), authInfo{Kind: authDevice, Role: clientRole}))
	// Support both ?pane= and ?paneId= query parameters.
	q := r2.URL.Query()
	if q.Get("pane") == "" && q.Get("paneId") != "" {
		q.Set("pane", q.Get("paneId"))
		r2.URL.RawQuery = q.Encode()
	}
	handleStream(w, r2, m.mux, nil, hostAllowlist{}, m.hub, m.activity)
}
