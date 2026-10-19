package main

import (
	"strings"
	"sync"
	"time"
)

const (
	// cacheDefaultTTLSec is the standard 5-minute prompt-cache TTL across
	// Claude, Codex, OpenCode, and Pi.
	cacheDefaultTTLSec = 300
	// cacheExtendedTTLSec is Claude's 1-hour ephemeral prompt-cache TTL.
	cacheExtendedTTLSec = 3600
	// cacheWarnSec is the remaining TTL window at or below which a
	// cache-cold push alert is emitted (at most once per warm cycle).
	cacheWarnSec = 60
)

// agentCacheInfo reports the prompt-cache warmth of an agent session for
// /api/mux/overview and the PWA's CacheChip.
type agentCacheInfo struct {
	State       string `json:"state"`                 // "warm" | "cooling" | "cold" | "busted"
	LastTouchAt string `json:"lastTouchAt,omitempty"` // RFC3339
	TTLSec      int    `json:"ttlSec,omitempty"`
	BustReason  string `json:"bustReason,omitempty"` // "model" | "effort" | "compaction"
}

// detectCacheBustText returns the cache-busting reason ("model", "effort", or
// "compaction") when a user command or local-command note resets the prompt
// cache, or "" otherwise.
func detectCacheBustText(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	lower := strings.ToLower(t)
	first, rest, _ := strings.Cut(lower, " ")
	rest = strings.TrimSpace(rest)
	switch first {
	case "/model":
		if rest != "" && rest != "status" && rest != "list" {
			return "model"
		}
	case "/effort", "/thinking":
		if rest != "" && rest != "status" {
			return "effort"
		}
	case "/compact":
		return "compaction"
	}
	switch {
	case strings.HasPrefix(lower, "set model to "), strings.HasPrefix(lower, "switched model to "):
		return "model"
	case strings.HasPrefix(lower, "set effort to "), strings.HasPrefix(lower, "reasoning effort set to "):
		return "effort"
	case strings.HasPrefix(lower, "compacted "), lower == "context compacted":
		return "compaction"
	}
	return ""
}

func parseCacheTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// computeAgentCache evaluates prompt-cache warmth across a transcript's
// entries at time now. It returns nil when the transcript has neither
// touched the prompt cache nor recorded a cache-busting event.
func computeAgentCache(entries []TranscriptEntry, now time.Time) *agentCacheInfo {
	var (
		lastTouch    time.Time
		lastTouchRaw string
		ttlSec       = cacheDefaultTTLSec
		coldTurn     bool
		bustReason   string
		lastModel    string
	)
	for _, e := range entries {
		reason := e.bustReason
		if reason == "" && e.Role == "summary" {
			reason = "compaction"
		}
		if reason == "" && (e.Role == "user" || e.Role == "note") {
			for _, p := range e.Parts {
				if p.Kind == "text" {
					if r := detectCacheBustText(p.Text); r != "" {
						reason = r
						break
					}
				}
			}
		}
		if reason != "" {
			bustReason = reason
			coldTurn = false
		}
		u := e.usage
		if u == nil {
			continue
		}
		model := strings.TrimSpace(u.model)
		modelChanged := lastModel != "" && model != "" && model != "<synthetic>" && model != lastModel
		if model != "" && model != "<synthetic>" {
			lastModel = model
		}
		touched := u.cacheReadTokens > 0 || u.cacheCreationTokens > 0
		if modelChanged && u.cacheReadTokens == 0 {
			bustReason = "model"
			coldTurn = false
			continue
		}
		if touched {
			if ts, ok := parseCacheTime(e.TS); ok {
				lastTouch = ts
				lastTouchRaw = ts.UTC().Format(time.RFC3339)
			} else if !now.IsZero() {
				lastTouch = now
				lastTouchRaw = now.UTC().Format(time.RFC3339)
			}
			if u.ephemeral1hTokens > 0 {
				ttlSec = cacheExtendedTTLSec
			} else {
				ttlSec = cacheDefaultTTLSec
			}
			bustReason = ""
			coldTurn = false
		} else if u.inputTokens > 0 && !lastTouch.IsZero() {
			coldTurn = true
		}
	}

	if bustReason != "" {
		return &agentCacheInfo{
			State:       "busted",
			LastTouchAt: lastTouchRaw,
			TTLSec:      ttlSec,
			BustReason:  bustReason,
		}
	}
	if lastTouch.IsZero() {
		if coldTurn {
			return &agentCacheInfo{State: "cold", TTLSec: ttlSec}
		}
		return nil
	}
	if coldTurn {
		return &agentCacheInfo{
			State:       "cold",
			LastTouchAt: lastTouchRaw,
			TTLSec:      ttlSec,
		}
	}
	if now.IsZero() {
		now = time.Now()
	}
	remaining := float64(ttlSec) - now.Sub(lastTouch).Seconds()
	state := "warm"
	switch {
	case remaining <= 0:
		state = "cold"
	case remaining <= float64(ttlSec)/4:
		state = "cooling"
	}
	return &agentCacheInfo{
		State:       state,
		LastTouchAt: lastTouchRaw,
		TTLSec:      ttlSec,
	}
}

// cacheWarnTracker tracks which warm cycle (by LastTouchAt) has already fired
// a cache-cold notification per pane so alerts fire at most once per cycle.
type cacheWarnTracker struct {
	mu     sync.Mutex
	warned map[string]string // paneKey -> LastTouchAt
}

func newCacheWarnTracker() *cacheWarnTracker {
	return &cacheWarnTracker{warned: map[string]string{}}
}

// check evaluates whether info is within cacheWarnSec of expiring and, if
// not yet warned for this warm cycle (info.LastTouchAt), emits a cache-cold
// pushEvent via emit.
func (t *cacheWarnTracker) check(paneKey string, ev pushEvent, info *agentCacheInfo, now time.Time, emit func(pushEvent) bool) bool {
	if t == nil || info == nil || info.LastTouchAt == "" || info.BustReason != "" {
		return false
	}
	if info.State != "warm" && info.State != "cooling" {
		return false
	}
	lastTouch, ok := parseCacheTime(info.LastTouchAt)
	if !ok {
		return false
	}
	ttl := info.TTLSec
	if ttl <= 0 {
		ttl = cacheDefaultTTLSec
	}
	if now.IsZero() {
		now = time.Now()
	}
	remaining := float64(ttl) - now.Sub(lastTouch).Seconds()
	if remaining <= 0 || remaining > float64(cacheWarnSec) {
		return false
	}
	key := paneKey
	if key == "" {
		key = ev.PaneID
	}
	t.mu.Lock()
	if t.warned[key] == info.LastTouchAt {
		t.mu.Unlock()
		return false
	}
	if len(t.warned) >= 1024 {
		clear(t.warned)
	}
	t.warned[key] = info.LastTouchAt
	t.mu.Unlock()

	ev.Kind = "cache-cold"
	if emit != nil {
		return emit(ev)
	}
	return true
}
