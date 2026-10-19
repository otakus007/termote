package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMachineAlertsSustainedWindowAndHysteresis(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "machine")
	c, err := newMachineCollector(dir, nil)
	if err != nil {
		t.Fatalf("newMachineCollector: %v", err)
	}

	var emitted []pushEvent
	c.setEmitPush(func(ev pushEvent) bool {
		emitted = append(emitted, ev)
		return true
	})

	// Configure a 10-minute >= 85% CPU rule.
	rules, err := c.UpdateAlerts([]MachineAlertRule{
		{ID: "cpu-high", Metric: "cpu", ThresholdPct: 85, DurationMin: 10, Enabled: true},
	})
	if err != nil {
		t.Fatalf("UpdateAlerts: %v", err)
	}
	if len(rules) != 1 || rules[0].Firing {
		t.Fatalf("unexpected initial rules: %+v", rules)
	}

	c.mu.Lock()
	c.samples = nil
	c.firing = map[string]bool{}
	c.mu.Unlock()
	emitted = nil

	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	addSample := func(minOffset int, cpu float64) {
		ts := base.Add(time.Duration(minOffset) * time.Minute)
		c.now = func() time.Time { return ts }
		c.AppendSample(MachineSample{
			TS:     ts.Format(time.RFC3339),
			CPUPct: cpu,
			MemPct: 40,
			Disks:  []MachineDiskStat{{Mount: "/", UsedPct: 50}},
		})
	}

	// 1. Transient 3-minute 99% spike followed by normal load must NOT fire.
	addSample(0, 99)
	addSample(1, 99)
	addSample(2, 99)
	for m := 3; m < 10; m++ {
		addSample(m, 40)
	}
	if len(emitted) != 0 {
		t.Fatalf("expected 0 alerts on 3-minute spike, got %d", len(emitted))
	}
	if c.Status("1h", false).Alerts[0].Firing {
		t.Fatal("expected rule not firing after transient spike")
	}

	// 2. Sustained 10-minute load >= 85% triggers exactly one notification.
	for m := 10; m < 19; m++ {
		addSample(m, 88)
	}
	if len(emitted) != 0 {
		t.Fatalf("expected 0 alerts after 9 minutes at 88%%, got %d", len(emitted))
	}
	addSample(19, 86) // 10th consecutive sample >= 85%
	if len(emitted) != 1 || emitted[0].Kind != "machine-load" {
		t.Fatalf("expected 1 machine-load alert on 10th sustained minute, got %+v", emitted)
	}
	if !c.Status("1h", false).Alerts[0].Firing {
		t.Fatal("expected rule to be firing")
	}

	// 3. Subsequent high samples or dip to 81% (>= thresholdPct - 5 = 80%) keep firing without duplicate push.
	addSample(20, 92)
	addSample(21, 81) // above 80% hysteresis floor
	addSample(22, 89)
	if len(emitted) != 1 {
		t.Fatalf("expected no duplicate alerts while firing in hysteresis band, got %d", len(emitted))
	}
	if !c.Status("1h", false).Alerts[0].Firing {
		t.Fatal("expected rule to remain firing at 81% (hysteresis floor 80%)")
	}

	// 4. Dropping below 80% (thresholdPct - 5) resets firing = false.
	addSample(23, 79.5)
	if c.Status("1h", false).Alerts[0].Firing {
		t.Fatal("expected rule firing to reset once sample drops below 80%")
	}

	// 5. Another sustained 10-minute window fires again.
	for m := 24; m < 34; m++ {
		addSample(m, 90)
	}
	if len(emitted) != 2 {
		t.Fatalf("expected second alert after hysteresis recovery and new 10m sustained window, got %d", len(emitted))
	}
}

func TestMachineAlertsValidationAndViewOnlyForbidden(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "machine")
	c, err := newMachineCollector(dir, nil)
	if err != nil {
		t.Fatalf("newMachineCollector: %v", err)
	}

	mux := http.NewServeMux()
	allowed := parseAllowedHosts("", false)
	registerMachineRoutes(mux, c, allowed)

	// View-only client must receive 403 view_only on POST /api/mux/machine/alerts.
	body := `{"alerts":[{"id":"cpu-high","metric":"cpu","thresholdPct":85,"durationMin":10,"enabled":true}]}`
	reqView := httptest.NewRequest(http.MethodPost, "http://localhost/api/mux/machine/alerts", strings.NewReader(body))
	reqView.Header.Set("Content-Type", "application/json")
	reqView = reqView.WithContext(withAuth(reqView.Context(), authInfo{Kind: authDevice, Role: roleView}))
	recView := httptest.NewRecorder()
	mux.ServeHTTP(recView, reqView)
	if recView.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for view-only POST /api/mux/machine/alerts, got %d: %s", recView.Code, recView.Body.String())
	}
	var errResp map[string]string
	_ = json.Unmarshal(recView.Body.Bytes(), &errResp)
	if errResp["code"] != "view_only" {
		t.Fatalf("expected code view_only, got %+v", errResp)
	}

	// Out-of-bounds thresholdPct (< 50 or > 99) and durationMin (< 5 or > 120) must return 400.
	invalidPayloads := []string{
		`{"alerts":[{"id":"r1","metric":"cpu","thresholdPct":40,"durationMin":10,"enabled":true}]}`,
		`{"alerts":[{"id":"r1","metric":"cpu","thresholdPct":100,"durationMin":10,"enabled":true}]}`,
		`{"alerts":[{"id":"r1","metric":"memory","thresholdPct":80,"durationMin":4,"enabled":true}]}`,
		`{"alerts":[{"id":"r1","metric":"disk","thresholdPct":80,"durationMin":121,"enabled":true}]}`,
		`{"alerts":[{"id":"r1","metric":"gpu","thresholdPct":80,"durationMin":10,"enabled":true}]}`,
	}
	for _, payload := range invalidPayloads {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/api/mux/machine/alerts", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(withAuth(req.Context(), authInfo{Kind: authSession, Role: roleFull}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for payload %s, got %d", payload, rec.Code)
		}
	}

	// More than 16 rules must be rejected.
	var manyRules []MachineAlertRule
	for i := range 17 {
		manyRules = append(manyRules, MachineAlertRule{
			ID:           "rule-" + string(rune('a'+i)),
			Metric:       "cpu",
			ThresholdPct: 80,
			DurationMin:  10,
			Enabled:      true,
		})
	}
	manyBytes, _ := json.Marshal(map[string]any{"alerts": manyRules})
	reqMany := httptest.NewRequest(http.MethodPost, "http://localhost/api/mux/machine/alerts", bytes.NewReader(manyBytes))
	reqMany.Header.Set("Content-Type", "application/json")
	reqMany = reqMany.WithContext(withAuth(reqMany.Context(), authInfo{Kind: authSession, Role: roleFull}))
	recMany := httptest.NewRecorder()
	mux.ServeHTTP(recMany, reqMany)
	if recMany.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for >16 rules, got %d", recMany.Code)
	}

	// Valid POST persists and returns updated alerts.
	reqOK := httptest.NewRequest(http.MethodPost, "http://localhost/api/mux/machine/alerts", strings.NewReader(body))
	reqOK.Header.Set("Content-Type", "application/json")
	reqOK = reqOK.WithContext(withAuth(reqOK.Context(), authInfo{Kind: authSession, Role: roleFull}))
	recOK := httptest.NewRecorder()
	mux.ServeHTTP(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid alerts update, got %d: %s", recOK.Code, recOK.Body.String())
	}
}
