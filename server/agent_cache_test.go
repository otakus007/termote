package main

import (
	"testing"
	"time"
)

func TestComputeAgentCacheWarmCoolingColdAndExtendedTTL(t *testing.T) {
	touch := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	entries := []TranscriptEntry{
		{
			ID:   "a1",
			Role: "assistant",
			TS:   touch.Format(time.RFC3339),
			usage: &entryUsage{
				inputTokens:     1000,
				cacheReadTokens: 50000,
				model:           "claude-sonnet-4-6",
			},
		},
	}

	// 60s elapsed of 300s -> warm
	warm := computeAgentCache(entries, touch.Add(60*time.Second))
	if warm == nil || warm.State != "warm" || warm.TTLSec != 300 {
		t.Fatalf("expected warm (300s), got %+v", warm)
	}

	// 240s elapsed of 300s (60s left <= 75s) -> cooling
	cooling := computeAgentCache(entries, touch.Add(240*time.Second))
	if cooling == nil || cooling.State != "cooling" {
		t.Fatalf("expected cooling, got %+v", cooling)
	}

	// 305s elapsed of 300s -> cold
	cold := computeAgentCache(entries, touch.Add(305*time.Second))
	if cold == nil || cold.State != "cold" {
		t.Fatalf("expected cold, got %+v", cold)
	}

	// Extended 1h cache (ephemeral1hTokens > 0) -> 3600s TTL
	extEntries := []TranscriptEntry{
		{
			ID:   "a2",
			Role: "assistant",
			TS:   touch.Format(time.RFC3339),
			usage: &entryUsage{
				inputTokens:         500,
				cacheCreationTokens: 80000,
				ephemeral1hTokens:   80000,
				model:               "claude-opus-4-7",
			},
		},
	}
	extWarm := computeAgentCache(extEntries, touch.Add(20*time.Minute))
	if extWarm == nil || extWarm.State != "warm" || extWarm.TTLSec != 3600 {
		t.Fatalf("expected extended warm (3600s), got %+v", extWarm)
	}
}

func TestComputeAgentCacheBustDetection(t *testing.T) {
	touch := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	base := TranscriptEntry{
		ID:   "a1",
		Role: "assistant",
		TS:   touch.Format(time.RFC3339),
		usage: &entryUsage{
			inputTokens:     1000,
			cacheReadTokens: 40000,
			model:           "claude-sonnet-4-6",
		},
	}

	cases := []struct {
		name   string
		follow TranscriptEntry
		reason string
	}{
		{
			name:   "slash compact",
			follow: TranscriptEntry{ID: "u1", Role: "user", Parts: []TranscriptPart{{Kind: "text", Text: "/compact"}}},
			reason: "compaction",
		},
		{
			name:   "slash model switch",
			follow: TranscriptEntry{ID: "u2", Role: "user", Parts: []TranscriptPart{{Kind: "text", Text: "/model opus"}}},
			reason: "model",
		},
		{
			name:   "slash effort switch",
			follow: TranscriptEntry{ID: "u3", Role: "user", Parts: []TranscriptPart{{Kind: "text", Text: "/effort high"}}},
			reason: "effort",
		},
		{
			name:   "summary row",
			follow: TranscriptEntry{ID: "s1", Role: "summary"},
			reason: "compaction",
		},
		{
			name: "model change on turn without cache read",
			follow: TranscriptEntry{
				ID:   "a2",
				Role: "assistant",
				TS:   touch.Add(10 * time.Second).Format(time.RFC3339),
				usage: &entryUsage{
					inputTokens: 2000,
					model:       "claude-opus-4-7",
				},
			},
			reason: "model",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeAgentCache([]TranscriptEntry{base, tc.follow}, touch.Add(30*time.Second))
			if got == nil || got.State != "busted" || got.BustReason != tc.reason {
				t.Fatalf("expected busted (%s), got %+v", tc.reason, got)
			}
		})
	}

	// Non-mutating "/model status" should not bust the cache
	nonBust := TranscriptEntry{ID: "u4", Role: "user", Parts: []TranscriptPart{{Kind: "text", Text: "/model status"}}}
	stillWarm := computeAgentCache([]TranscriptEntry{base, nonBust}, touch.Add(30*time.Second))
	if stillWarm == nil || stillWarm.State != "warm" {
		t.Fatalf("expected /model status to keep cache warm, got %+v", stillWarm)
	}
}

func TestCacheWarnTrackerEmitsOncePerWarmCycle(t *testing.T) {
	tracker := newCacheWarnTracker()
	touch1 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	info1 := &agentCacheInfo{
		State:       "cooling",
		LastTouchAt: touch1.Format(time.RFC3339),
		TTLSec:      300,
	}
	var emitted []pushEvent
	emit := func(ev pushEvent) bool {
		emitted = append(emitted, ev)
		return true
	}

	// 120s left (> 60s cacheWarnSec): does not emit
	if tracker.check("pane-1", pushEvent{PaneID: "0"}, info1, touch1.Add(180*time.Second), emit) {
		t.Fatal("should not warn when 120s remain")
	}

	// 45s left (<= 60s cacheWarnSec): emits once
	if !tracker.check("pane-1", pushEvent{PaneID: "0"}, info1, touch1.Add(255*time.Second), emit) {
		t.Fatal("expected cache-cold warning at 45s remaining")
	}
	if len(emitted) != 1 || emitted[0].Kind != "cache-cold" {
		t.Fatalf("unexpected emitted events: %+v", emitted)
	}

	// Second check in the same warm cycle: suppressed
	if tracker.check("pane-1", pushEvent{PaneID: "0"}, info1, touch1.Add(270*time.Second), emit) {
		t.Fatal("expected duplicate warning in same warm cycle to be suppressed")
	}

	// New warm cycle (touch2): warns again when entering warning window
	touch2 := touch1.Add(10 * time.Minute)
	info2 := &agentCacheInfo{
		State:       "cooling",
		LastTouchAt: touch2.Format(time.RFC3339),
		TTLSec:      300,
	}
	if !tracker.check("pane-1", pushEvent{PaneID: "0"}, info2, touch2.Add(250*time.Second), emit) {
		t.Fatal("expected warning for new warm cycle")
	}
	if len(emitted) != 2 {
		t.Fatalf("expected 2 emitted events across 2 cycles, got %d", len(emitted))
	}
}

func TestActivityTrackerLocalAckFoldsDoneToIdle(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tr := newActivityTracker(func() time.Time { return now })

	snapDone := Snapshot{
		Backend: "herdr",
		Groups: []Group{{
			ID:   "w1",
			Name: "repo",
			Tabs: []Tab{{
				ID:   "w1:t1",
				Name: "main",
				Panes: []Pane{{
					ID:    "w1:p1",
					Agent: &AgentInfo{Name: "claude", Status: "done"},
				}},
			}},
		}},
	}
	gen := tr.begin()
	observed := tr.observe(gen, snapDone)
	if got := observed.Groups[0].Tabs[0].Panes[0].Agent.Status; got != "done" {
		t.Fatalf("expected initial status done, got %q", got)
	}

	// Local Ack transitions done -> idle without touching mux focus
	if !tr.Ack("w1:p1") {
		t.Fatal("expected Ack(w1:p1) to succeed")
	}

	now = now.Add(2 * time.Second)
	gen2 := tr.begin()
	observedAfterAck := tr.observe(gen2, snapDone)
	if got := observedAfterAck.Groups[0].Tabs[0].Panes[0].Agent.Status; got != "idle" {
		t.Fatalf("expected status folded to idle after Ack, got %q", got)
	}

	// Next turn (working -> done) clears the previous Ack
	snapWorking := Snapshot{
		Backend: "herdr",
		Groups: []Group{{
			ID:   "w1",
			Name: "repo",
			Tabs: []Tab{{
				ID:   "w1:t1",
				Name: "main",
				Panes: []Pane{{
					ID:    "w1:p1",
					Agent: &AgentInfo{Name: "claude", Status: "working"},
				}},
			}},
		}},
	}
	now = now.Add(3 * time.Second)
	gen3 := tr.begin()
	tr.observe(gen3, snapWorking)

	now = now.Add(5 * time.Second)
	gen4 := tr.begin()
	observedNewDone := tr.observe(gen4, snapDone)
	if got := observedNewDone.Groups[0].Tabs[0].Panes[0].Agent.Status; got != "done" {
		t.Fatalf("expected new done turn after working to report done, got %q", got)
	}
}
