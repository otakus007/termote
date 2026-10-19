package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pushHandler builds a server whose push store lives in a temp dir; noStore
// builds one without push.
func pushHandler(t *testing.T, noStore bool) (http.Handler, *pushStore) {
	t.Helper()
	cfg := testConfig(t)
	if !noStore {
		cfg.PushDir = filepath.Join(t.TempDir(), "push")
	}
	h, _, store, err := buildServer(cfg, &fakeMux{})
	if err != nil {
		t.Fatal(err)
	}
	if (store == nil) != noStore {
		t.Fatalf("store = %v, want one: %v", store, !noStore)
	}
	return h, store
}

func subscribeBody(t *testing.T, sub pushSub) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"endpoint": sub.Endpoint,
		"keys":     map[string]string{"p256dh": sub.P256dh, "auth": sub.Auth},
	})
	return string(b)
}

func TestPushRoutesSubscribe(t *testing.T) {
	h, store := pushHandler(t, false)
	sub := fcmSub(t, 1)
	rec := serve(h, apiRequest("POST", "/api/mux/push/subscribe", subscribeBody(t, sub)))
	if rec.Code != http.StatusOK || decodeBody(t, rec)["ok"] != true {
		t.Fatalf("subscribe = %d %s", rec.Code, rec.Body)
	}
	if got := store.list(); len(got) != 1 || got[0].Endpoint != sub.Endpoint {
		t.Fatalf("stored = %v", got)
	}
	// An upsert, as the PWA repeats it.
	serve(h, apiRequest("POST", "/api/mux/push/subscribe", subscribeBody(t, sub)))
	if n := len(store.list()); n != 1 {
		t.Fatalf("after a repeat: %d subscriptions", n)
	}

	del := `{"endpoint":"` + sub.Endpoint + `"}`
	rec = serve(h, apiRequest("DELETE", "/api/mux/push/subscribe", del))
	if rec.Code != http.StatusOK || len(store.list()) != 0 {
		t.Fatalf("delete = %d %s, left %d", rec.Code, rec.Body, len(store.list()))
	}
	// Unknown endpoints are no error: a device may already be gone.
	rec = serve(h, apiRequest("DELETE", "/api/mux/push/subscribe", del))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete unknown = %d %s", rec.Code, rec.Body)
	}
}

func TestPushRoutesRejects(t *testing.T) {
	h, store := pushHandler(t, false)
	good := fcmSub(t, 1)
	bad := good
	bad.Endpoint = "https://evil.test/push"
	badKeys := good
	badKeys.Auth = "AAAA"
	for _, tc := range []struct {
		name   string
		req    *http.Request
		status int
		code   string
	}{
		{"invalid endpoint", apiRequest("POST", "/api/mux/push/subscribe", subscribeBody(t, bad)), 400, "invalid_endpoint"},
		{"invalid keys", apiRequest("POST", "/api/mux/push/subscribe", subscribeBody(t, badKeys)), 400, "invalid_keys"},
		{"oversized body", apiRequest("POST", "/api/mux/push/subscribe", `{"endpoint":"`+strings.Repeat("a", 9000)+`"}`), 400, ""},
		{"GET", apiRequest("GET", "/api/mux/push/subscribe", ""), 405, ""},
		{"PUT", apiRequest("PUT", "/api/mux/push/subscribe", subscribeBody(t, good)), 405, ""},
		{"POST on key", apiRequest("POST", "/api/mux/push/key", "{}"), 405, ""},
		{"no auth", func() *http.Request {
			r := apiRequest("POST", "/api/mux/push/subscribe", subscribeBody(t, good))
			r.Header.Del("Authorization")
			return r
		}(), 401, ""},
		{"cross site", func() *http.Request {
			r := apiRequest("POST", "/api/mux/push/subscribe", subscribeBody(t, good))
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Origin", "https://evil.test")
			return r
		}(), 403, ""},
		{"not JSON", func() *http.Request {
			r := apiRequest("POST", "/api/mux/push/subscribe", subscribeBody(t, good))
			r.Header.Set("Content-Type", "text/plain")
			return r
		}(), 415, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(h, tc.req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body)
			}
			if tc.code != "" {
				if got := decodeBody(t, rec)["code"]; got != tc.code {
					t.Errorf("code = %v, want %s", got, tc.code)
				}
			}
		})
	}
	if n := len(store.list()); n != 0 {
		t.Fatalf("a refused request stored %d subscriptions", n)
	}
}

func TestPushRoutesKeyAndCaps(t *testing.T) {
	h, store := pushHandler(t, false)
	sub := fcmSub(t, 1)
	serve(h, apiRequest("POST", "/api/mux/push/subscribe", subscribeBody(t, sub)))

	rec := serve(h, apiRequest("GET", "/api/mux/push/key", ""))
	body := decodeBody(t, rec)
	pub, _ := store.keys()
	if rec.Code != http.StatusOK || body["publicKey"] != pub || len(pub) != 87 || len(body) != 1 {
		t.Fatalf("key = %d %v", rec.Code, body)
	}
	rec = serve(h, apiRequest("GET", "/api/mux/snapshot", ""))
	snap := rec.Body.String()
	if !strings.Contains(snap, `"push":true`) {
		t.Errorf("snapshot caps lack push: %s", snap)
	}
	// No route returns a stored endpoint.
	for _, path := range []string{"/api/mux/push/key", "/api/mux/snapshot", "/api/mux/health"} {
		if b := serve(h, apiRequest("GET", path, "")).Body.String(); strings.Contains(b, "fcm.googleapis.com") {
			t.Errorf("%s echoes an endpoint: %s", path, b)
		}
	}
}

func TestPushRoutesWithoutStore(t *testing.T) {
	h, _ := pushHandler(t, true)
	for _, req := range []*http.Request{
		apiRequest("GET", "/api/mux/push/key", ""),
		apiRequest("POST", "/api/mux/push/subscribe", subscribeBody(t, fcmSub(t, 1))),
	} {
		rec := serve(h, req)
		if rec.Code != http.StatusServiceUnavailable || decodeBody(t, rec)["code"] != "push_unavailable" {
			t.Errorf("%s %s = %d %s", req.Method, req.URL.Path, rec.Code, rec.Body)
		}
	}
	if snap := serve(h, apiRequest("GET", "/api/mux/snapshot", "")).Body.String(); !strings.Contains(snap, `"push":false`) {
		t.Errorf("snapshot caps: %s", snap)
	}
}

// The push dir sits in the state dir, which the Files view never serves:
// a pane in the home dir cannot read the private key.
func TestPushDirDeniedToFiles(t *testing.T) {
	home := t.TempDir()
	c := &cli{goos: "linux", home: home}
	cfg := testConfig(t)
	cfg.FilesDenyDirs = filesDenyDirs(c.configDir(), c.stateDir())
	cfg.PushDir = filepath.Join(c.stateDir(), "push")
	h, _, store, err := buildServer(cfg, &filesFakeMux{dir: home, files: true})
	if err != nil || store == nil {
		t.Fatalf("buildServer: %v, store %v", err, store)
	}
	rel, _ := filepath.Rel(home, filepath.Join(cfg.PushDir, vapidFile))
	if _, err := os.Stat(filepath.Join(home, rel)); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"path": {filepath.ToSlash(rel)}, "reveal": {"1"}}
	rec := serve(h, apiRequest("GET", "/api/mux/panes/0/files/content?"+q.Encode(), ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("files/content served the push key: %d %s", rec.Code, rec.Body)
	}
}

// peekMux records which read the snapshot route used.
type peekMux struct {
	fakeMux
	peeked, snapped int
}

func (p *peekMux) Snapshot(ctx context.Context) (Snapshot, error) {
	p.snapped++
	return p.fakeMux.Snapshot(ctx)
}

func (p *peekMux) peekSnapshot(ctx context.Context) (Snapshot, error) {
	p.peeked++
	return p.fakeMux.Snapshot(ctx)
}

// The service worker names a push from snapshot?peek=1, which never makes
// tmux's default session again while no page is open.
func TestSnapshotPeekCreatesNothing(t *testing.T) {
	m := &peekMux{}
	h := newTestHandler(t, m)
	if rec := serve(h, apiRequest("GET", "/api/mux/snapshot?peek=1", "")); rec.Code != http.StatusOK {
		t.Fatalf("peek = %d %s", rec.Code, rec.Body)
	}
	serve(h, apiRequest("GET", "/api/mux/snapshot", ""))
	if m.peeked != 1 || m.snapped != 1 {
		t.Fatalf("peeked %d, snapped %d", m.peeked, m.snapped)
	}
}

func TestNotifyRoutesConfigMuteAndTelegram(t *testing.T) {
	h, store := pushHandler(t, false)

	// GET /api/mux/notify/config returns defaults with Cache-Control: no-store.
	rec := serve(h, apiRequest("GET", "/api/mux/notify/config", ""))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET config = %d %s (cc=%q)", rec.Code, rec.Body, rec.Header().Get("Cache-Control"))
	}
	var cfgResp notifyConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cfgResp); err != nil {
		t.Fatal(err)
	}
	if cfgResp.BlockedSettleSec != 5 || cfgResp.DoneSettleSec != 15 || cfgResp.CooldownSec != 30 || !cfgResp.SkipWhileViewing {
		t.Fatalf("unexpected defaults: %+v", cfgResp)
	}

	// Cross-site GET on /api/mux/notify/config is rejected.
	crossGet := apiRequest("GET", "/api/mux/notify/config", "")
	crossGet.Header.Set("Sec-Fetch-Site", "cross-site")
	if r := serve(h, crossGet); r.Code != http.StatusForbidden {
		t.Fatalf("cross-site GET config = %d, want 403", r.Code)
	}

	// PUT /api/mux/notify/config updates preferences and validates bounds/publicUrl.
	rec = serve(h, apiRequest("PUT", "/api/mux/notify/config", `{"blockedSettleSec":10,"publicUrl":"https://termote.example.com","categories":{"update":true}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT config = %d %s", rec.Code, rec.Body)
	}
	if store.notifyConfig().BlockedSettleSec != 10 || !store.notifyConfig().Categories.Update {
		t.Fatalf("updated config = %+v", store.notifyConfig())
	}

	// Invalid settle window and non-localhost HTTP publicUrl are rejected with 400 invalid_config.
	for _, badBody := range []string{
		`{"blockedSettleSec":99}`,
		`{"doneSettleSec":-1}`,
		`{"cooldownSec":999}`,
		`{"publicUrl":"http://evil.example.com"}`,
		`{"publicUrl":"https://user:pass@termote.example.com"}`,
	} {
		r := serve(h, apiRequest("PUT", "/api/mux/notify/config", badBody))
		if r.Code != http.StatusBadRequest || decodeBody(t, r)["code"] != "invalid_config" {
			t.Fatalf("PUT invalid (%s) = %d %s", badBody, r.Code, r.Body)
		}
	}

	// POST /api/mux/notify/mute sets and clears mute.
	rec = serve(h, apiRequest("POST", "/api/mux/notify/mute", `{"durationMinutes":60}`))
	if rec.Code != http.StatusOK || decodeBody(t, rec)["mutedUntil"] == "" {
		t.Fatalf("POST mute = %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, apiRequest("POST", "/api/mux/notify/mute", `{"durationMinutes":0}`))
	if rec.Code != http.StatusOK || decodeBody(t, rec)["mutedUntil"] != "" {
		t.Fatalf("POST unmute = %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, apiRequest("POST", "/api/mux/notify/mute", `{"durationMinutes":2000}`))
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["code"] != "invalid_duration" {
		t.Fatalf("POST invalid mute = %d %s", rec.Code, rec.Body)
	}

	// PUT /api/mux/notify/telegram saves credentials and never echoes botToken or full chatId.
	secretToken := "123456:SECRET_BOT_TOKEN_ABC"
	fullChatID := "-100123456789"
	rec = serve(h, apiRequest("PUT", "/api/mux/notify/telegram", `{"botToken":"`+secretToken+`","chatId":"`+fullChatID+`","enabled":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT telegram = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), secretToken) || strings.Contains(rec.Body.String(), fullChatID) {
		t.Fatalf("PUT telegram leaked secret: %s", rec.Body.String())
	}

	rec = serve(h, apiRequest("GET", "/api/mux/notify/config", ""))
	if strings.Contains(rec.Body.String(), secretToken) || strings.Contains(rec.Body.String(), fullChatID) {
		t.Fatalf("GET config leaked telegram secret: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"chatIdTail":"6789"`) {
		t.Fatalf("GET config missing chatIdTail: %s", rec.Body.String())
	}

	// DELETE /api/mux/notify/telegram removes configuration.
	rec = serve(h, apiRequest("DELETE", "/api/mux/notify/telegram", `{}`))
	if rec.Code != http.StatusOK || store.telegramStatus().Configured {
		t.Fatalf("DELETE telegram = %d %s", rec.Code, rec.Body)
	}
}

func TestSnapshotViewingHeartbeat(t *testing.T) {
	h, store := pushHandler(t, false)
	rec := serve(h, apiRequest("GET", "/api/mux/snapshot?viewing=pane-7", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot = %d %s", rec.Code, rec.Body)
	}
	if !store.isViewing("pane-7", store.now()) {
		t.Fatal("expected pane-7 to be marked as viewing after snapshot poll")
	}
}
