package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelegramFormatAndCleanText(t *testing.T) {
	ev := pushEvent{
		GroupID: "g1",
		TabID:   "t1",
		PaneID:  "%42",
		Kind:    "blocked",
	}
	meta := paneNotifyMeta{
		groupName: "work\u202e\x00group",
		tabName:   "api\n\u200btab",
		agent:     "claude\u2066code",
	}
	text, markup := formatTelegramEvent(ev, meta, "https://termote.example.com")
	if strings.ContainsAny(text, "\x00\u202e\u200b\u2066") {
		t.Fatalf("formatTelegramEvent left control or bidi characters in %q", text)
	}
	if !strings.Contains(text, "[workgroup / apitab] claudecode needs you") {
		t.Fatalf("unexpected headline: %q", text)
	}
	wantURL := "https://termote.example.com/#pane=%2542"
	if !strings.Contains(text, wantURL) {
		t.Fatalf("missing deep link %q in %q", wantURL, text)
	}
	if markup == nil || len(markup.InlineKeyboard) != 1 || markup.InlineKeyboard[0][0].URL != wantURL {
		t.Fatalf("unexpected inline keyboard markup: %+v", markup)
	}

	// Loopback publicUrl omits both the deep-link text and inline keyboard.
	for _, loopURL := range []string{"http://localhost:8080", "http://127.0.0.1:7680", "http://[::1]:7680"} {
		localText, localMarkup := formatTelegramEvent(ev, meta, loopURL)
		if strings.Contains(localText, "http") || localMarkup != nil {
			t.Fatalf("expected loopback %q to omit deep link and markup, got text=%q markup=%+v", loopURL, localText, localMarkup)
		}
	}

	// Non-loopback HTTP publicUrl includes the text link but omits inline keyboard (Telegram requires HTTPS buttons).
	lanText, lanMarkup := formatTelegramEvent(ev, meta, "http://192.168.1.50:7680")
	if !strings.Contains(lanText, "http://192.168.1.50:7680/#pane=%2542") || lanMarkup != nil {
		t.Fatalf("expected LAN HTTP URL in text and nil markup, got text=%q markup=%+v", lanText, lanMarkup)
	}
}

func TestTelegramSendAndBackoff(t *testing.T) {
	var hits atomic.Int32
	var gotPath string
	var gotPayload telegramMessagePayload
	status := http.StatusOK

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotPayload)
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "45")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	store := newTestPushStore(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	store.telegramBaseURL = srv.URL
	store.telegramHTTP = srv.Client()

	tTrue := true
	st, err := store.updateTelegram(telegramUpdateRequest{
		BotToken: "123456:ABC-DEF1234",
		ChatID:   "-100987654321",
		Enabled:  &tTrue,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Configured || !st.Enabled || st.ChatIDTail != "4321" {
		t.Fatalf("unexpected telegram status: %+v", st)
	}

	err = store.sendTelegramEvent(context.Background(), pushEvent{
		GroupID: "g",
		TabID:   "t",
		PaneID:  "p1",
		Kind:    "done",
	}, paneNotifyMeta{groupName: "main", tabName: "dev", agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 || gotPath != "/bot123456:ABC-DEF1234/sendMessage" {
		t.Fatalf("hits=%d path=%q", hits.Load(), gotPath)
	}
	if gotPayload.ChatID != "-100987654321" || !strings.Contains(gotPayload.Text, "claude finished its turn") {
		t.Fatalf("unexpected payload: %+v", gotPayload)
	}

	// Rate limit 429 backs off for Retry-After (45s).
	status = http.StatusTooManyRequests
	err = store.sendTelegramEvent(context.Background(), pushEvent{PaneID: "p1", Kind: "blocked"}, paneNotifyMeta{})
	if err == nil {
		t.Fatal("expected error on 429")
	}
	status = http.StatusOK
	now = now.Add(20 * time.Second)
	_ = store.sendTelegramEvent(context.Background(), pushEvent{PaneID: "p1", Kind: "blocked"}, paneNotifyMeta{})
	if hits.Load() != 2 {
		t.Fatalf("expected send to be skipped during backoff, hits=%d", hits.Load())
	}
	now = now.Add(30 * time.Second)
	if err := store.sendTelegramEvent(context.Background(), pushEvent{PaneID: "p1", Kind: "blocked"}, paneNotifyMeta{}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("expected send after backoff expiry, hits=%d", hits.Load())
	}
}

func TestTelegramRedactsTokenOnError(t *testing.T) {
	store := newTestPushStore(t)
	secretToken := "999888777:SECRET_TOKEN_VALUE_XYZ"
	tTrue := true
	if _, err := store.updateTelegram(telegramUpdateRequest{
		BotToken: secretToken,
		ChatID:   "123456789",
		Enabled:  &tTrue,
	}); err != nil {
		t.Fatal(err)
	}
	// Point to an unreachable closed local server so http.Client.Do fails with a *url.Error containing the URL.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	store.telegramBaseURL = srv.URL
	store.telegramHTTP = srv.Client()

	err := store.sendTelegramTest(context.Background())
	if err == nil {
		t.Fatal("expected error from closed server")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error leaked botToken: %v", err)
	}
}

func TestTelegramAndNotifyConfigCredentialBinding(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "push")
	s1, err := newPushStore(dir, "alice", "pass-1", false)
	if err != nil {
		t.Fatal(err)
	}
	tTrue := true
	if _, err := s1.updateTelegram(telegramUpdateRequest{
		BotToken: "12345:tokenABCDEF",
		ChatID:   "987654321",
		Enabled:  &tTrue,
	}); err != nil {
		t.Fatal(err)
	}
	pubURL := "https://termote.example.com"
	if _, err := s1.updateNotifyConfig(notifyConfigUpdate{PublicURL: &pubURL}); err != nil {
		t.Fatal(err)
	}

	// Reopening with the same password preserves both configs.
	s2, err := newPushStore(dir, "alice", "pass-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.telegramStatus().Configured || s2.notifyConfig().PublicURL != pubURL {
		t.Fatalf("configs not preserved on same password: tg=%+v cfg=%+v", s2.telegramStatus(), s2.notifyConfig())
	}

	// Reopening with a different password drops both configs.
	s3, err := newPushStore(dir, "alice", "pass-2", false)
	if err != nil {
		t.Fatal(err)
	}
	if s3.telegramStatus().Configured || s3.notifyConfig().PublicURL != "" {
		t.Fatalf("configs not dropped on password change: tg=%+v cfg=%+v", s3.telegramStatus(), s3.notifyConfig())
	}
}
