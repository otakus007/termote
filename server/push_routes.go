package main

import (
	"errors"
	"log"
	"net/http"
)

var errPushUnavailable = &codedError{"push_unavailable", "push notifications are not available on this server", http.StatusServiceUnavailable}

// registerPushRoutes mounts /api/mux/push/* and /api/mux/notify/*. store is
// nil when the server cannot persist push state; the routes then answer 503
// push_unavailable. No route lists the subscriptions, echoes an endpoint, or
// returns the Telegram botToken or full chatId.
func registerPushRoutes(mux *http.ServeMux, store *pushStore, allowed hostAllowlist) {
	mux.HandleFunc("/api/mux/push/key", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if store == nil {
			pushError(w, errPushUnavailable)
			return
		}
		pub, _ := store.keys()
		jsonOK(w, map[string]string{"publicKey": pub})
	})
	mux.HandleFunc("/api/mux/push/subscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			w.Header().Set("Allow", "POST, DELETE")
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireWriteRole(w, r) {
			return
		}
		var body struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if store == nil {
			pushError(w, errPushUnavailable)
			return
		}
		var err error
		if r.Method == http.MethodPost {
			err = store.add(pushSub{Endpoint: body.Endpoint, P256dh: body.Keys.P256dh, Auth: body.Keys.Auth})
		} else {
			err = store.remove(body.Endpoint)
		}
		if err != nil {
			pushError(w, err)
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/mux/notify/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if msg := crossSiteRejection(allowed, r); msg != "" {
				jsonError(w, msg, http.StatusForbidden)
				return
			}
			if !requireWriteRole(w, r) {
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			if store == nil {
				pushError(w, errPushUnavailable)
				return
			}
			cfg := store.notifyConfig()
			jsonOK(w, notifyConfigResponse{
				BlockedSettleSec: cfg.BlockedSettleSec,
				DoneSettleSec:    cfg.DoneSettleSec,
				CooldownSec:      cfg.CooldownSec,
				SkipWhileViewing: cfg.SkipWhileViewing,
				MutedUntil:       cfg.MutedUntil,
				Categories:       cfg.Categories,
				PublicURL:        cfg.PublicURL,
				Telegram:         store.telegramStatus(),
			})
		case http.MethodPut, http.MethodPost:
			if !requireWriteRole(w, r) {
				return
			}
			var upd notifyConfigUpdate
			if !decodeJSON(w, r, &upd) {
				return
			}
			if store == nil {
				pushError(w, errPushUnavailable)
				return
			}
			cfg, err := store.updateNotifyConfig(upd)
			if err != nil {
				pushError(w, err)
				return
			}
			jsonOK(w, notifyConfigResponse{
				BlockedSettleSec: cfg.BlockedSettleSec,
				DoneSettleSec:    cfg.DoneSettleSec,
				CooldownSec:      cfg.CooldownSec,
				SkipWhileViewing: cfg.SkipWhileViewing,
				MutedUntil:       cfg.MutedUntil,
				Categories:       cfg.Categories,
				PublicURL:        cfg.PublicURL,
				Telegram:         store.telegramStatus(),
			})
		default:
			w.Header().Set("Allow", "GET, PUT, POST")
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/mux/notify/mute", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireWriteRole(w, r) {
			return
		}
		var body struct {
			DurationMinutes int `json:"durationMinutes"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if store == nil {
			pushError(w, errPushUnavailable)
			return
		}
		until, err := store.setMute(body.DurationMinutes)
		if err != nil {
			pushError(w, err)
			return
		}
		jsonOK(w, map[string]string{"mutedUntil": until})
	})
	mux.HandleFunc("/api/mux/notify/telegram", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut && r.Method != http.MethodPost && r.Method != http.MethodDelete {
			w.Header().Set("Allow", "PUT, POST, DELETE")
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireWriteRole(w, r) {
			return
		}
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			var req telegramUpdateRequest
			if !decodeJSON(w, r, &req) {
				return
			}
			if store == nil {
				pushError(w, errPushUnavailable)
				return
			}
			st, err := store.updateTelegram(req)
			if err != nil {
				pushError(w, err)
				return
			}
			jsonOK(w, st)
			return
		}
		if r.ContentLength > 0 {
			var body struct{}
			if !decodeJSON(w, r, &body) {
				return
			}
		}
		if store == nil {
			pushError(w, errPushUnavailable)
			return
		}
		if err := store.removeTelegram(); err != nil {
			pushError(w, err)
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/mux/notify/telegram/test", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireWriteRole(w, r) {
			return
		}
		var body telegramUpdateRequest
		if r.ContentLength > 0 {
			if !decodeJSON(w, r, &body) {
				return
			}
		}
		if store == nil {
			pushError(w, errPushUnavailable)
			return
		}
		if err := store.sendTelegramTestWith(r.Context(), body.BotToken, body.ChatID); err != nil {
			pushError(w, err)
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	})
}

// pushError answers a codedError as it is, anything else (a failed save)
// with a generic 500, logged.
func pushError(w http.ResponseWriter, err error) {
	var ce *codedError
	if errors.As(err, &ce) {
		jsonErrorCode(w, ce.code, ce.msg, ce.status)
		return
	}
	log.Printf("push subscriptions: %v", err)
	jsonError(w, "could not save the subscription", http.StatusInternalServerError)
}
