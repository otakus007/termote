package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	maxMachineAlertRules    = 16
	minAlertThresholdPct    = 50
	maxAlertThresholdPct    = 99
	minAlertDurationMin     = 5
	maxAlertDurationMin     = 120
	alertHysteresisDeltaPct = 5
)

var (
	machineAlertIDRe        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	errInvalidMachineAlerts = &codedError{"invalid_alerts", "invalid machine alert rules", http.StatusBadRequest}
	errTooManyMachineAlerts = &codedError{"too_many_alerts", "at most 16 machine alert rules may be configured", http.StatusBadRequest}
)

// MachineAlertRule defines a sustained high-load alert rule stored in
// <stateDir>/machine/machine-alerts.json.
type MachineAlertRule struct {
	ID           string `json:"id"`
	Metric       string `json:"metric"`       // "cpu" | "memory" | "disk"
	ThresholdPct int    `json:"thresholdPct"` // 50..99
	DurationMin  int    `json:"durationMin"`  // 5..120
	Enabled      bool   `json:"enabled"`
	Firing       bool   `json:"firing"`
}

func defaultMachineAlertRules() []MachineAlertRule {
	return []MachineAlertRule{
		{ID: "cpu-high", Metric: "cpu", ThresholdPct: 90, DurationMin: 15, Enabled: true, Firing: false},
		{ID: "mem-high", Metric: "memory", ThresholdPct: 90, DurationMin: 10, Enabled: true, Firing: false},
		{ID: "disk-high", Metric: "disk", ThresholdPct: 90, DurationMin: 5, Enabled: true, Firing: false},
	}
}

func validateMachineAlertRules(rules []MachineAlertRule) ([]MachineAlertRule, error) {
	if len(rules) > maxMachineAlertRules {
		return nil, errTooManyMachineAlerts
	}
	seen := make(map[string]bool, len(rules))
	out := make([]MachineAlertRule, 0, len(rules))
	for i, r := range rules {
		id := strings.TrimSpace(r.ID)
		metric := strings.ToLower(strings.TrimSpace(r.Metric))
		switch metric {
		case "cpu", "memory", "disk":
		default:
			return nil, errInvalidMachineAlerts
		}
		if id == "" {
			id = fmt.Sprintf("%s-%d", metric, i+1)
		}
		if !machineAlertIDRe.MatchString(id) || seen[id] {
			return nil, errInvalidMachineAlerts
		}
		if r.ThresholdPct < minAlertThresholdPct || r.ThresholdPct > maxAlertThresholdPct {
			return nil, errInvalidMachineAlerts
		}
		if r.DurationMin < minAlertDurationMin || r.DurationMin > maxAlertDurationMin {
			return nil, errInvalidMachineAlerts
		}
		seen[id] = true
		out = append(out, MachineAlertRule{
			ID:           id,
			Metric:       metric,
			ThresholdPct: r.ThresholdPct,
			DurationMin:  r.DurationMin,
			Enabled:      r.Enabled,
			Firing:       false,
		})
	}
	return out, nil
}

func (c *machineCollector) loadAlerts() error {
	if c.dir == "" {
		return nil
	}
	path := filepath.Join(c.dir, machineAlertsFile)
	data, err := readStateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", machineAlertsFile, err)
	}
	var raw []MachineAlertRule
	if err := json.Unmarshal(data, &raw); err != nil {
		var wrapped struct {
			Alerts []MachineAlertRule `json:"alerts"`
		}
		if err2 := json.Unmarshal(data, &wrapped); err2 != nil {
			log.Printf("machine: %s is corrupt, using defaults: %v", machineAlertsFile, err)
			_ = os.Rename(path, path+".bad")
			return nil
		}
		raw = wrapped.Alerts
	}
	clean, err := validateMachineAlertRules(raw)
	if err != nil {
		log.Printf("machine: %s has invalid rules, using defaults: %v", machineAlertsFile, err)
		return nil
	}
	c.alerts = clean
	return nil
}

func (c *machineCollector) saveAlertsLocked() error {
	if c.dir == "" {
		return nil
	}
	persisted := make([]MachineAlertRule, len(c.alerts))
	for i, r := range c.alerts {
		persisted[i] = r
		persisted[i].Firing = false
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		return err
	}
	return writeStateFile("machine", c.dir, "machine-alerts", filepath.Join(c.dir, machineAlertsFile), data, false)
}

func sampleMetricPct(s MachineSample, metric string) float64 {
	switch metric {
	case "cpu":
		return s.CPUPct
	case "memory":
		return s.MemPct
	case "disk":
		var maxPct float64
		for _, d := range s.Disks {
			if d.UsedPct > maxPct {
				maxPct = d.UsedPct
			}
		}
		return maxPct
	default:
		return 0
	}
}

// evaluateAlertsLocked checks each rule against the trailing durationMin
// samples in c.samples. Must be called with c.mu held.
func (c *machineCollector) evaluateAlertsLocked() []pushEvent {
	if len(c.samples) == 0 {
		return nil
	}
	latest := c.samples[len(c.samples)-1]
	var events []pushEvent

	for i := range c.alerts {
		rule := &c.alerts[i]
		if !rule.Enabled {
			delete(c.firing, rule.ID)
			rule.Firing = false
			continue
		}

		latestVal := sampleMetricPct(latest, rule.Metric)
		wasFiring := c.firing[rule.ID]

		if wasFiring {
			recoveryThreshold := float64(rule.ThresholdPct - alertHysteresisDeltaPct)
			if latestVal < recoveryThreshold {
				delete(c.firing, rule.ID)
				rule.Firing = false
			} else {
				rule.Firing = true
			}
			continue
		}

		if len(c.samples) < rule.DurationMin {
			rule.Firing = false
			continue
		}

		window := c.samples[len(c.samples)-rule.DurationMin:]
		if !windowSpanValid(window, rule.DurationMin) {
			rule.Firing = false
			continue
		}

		sustained := true
		threshold := float64(rule.ThresholdPct)
		for _, s := range window {
			if sampleMetricPct(s, rule.Metric) < threshold {
				sustained = false
				break
			}
		}
		if sustained {
			c.firing[rule.ID] = true
			rule.Firing = true
			events = append(events, pushEvent{Kind: "machine-load"})
		} else {
			rule.Firing = false
		}
	}
	return events
}

func windowSpanValid(window []MachineSample, durationMin int) bool {
	if len(window) == 0 {
		return false
	}
	firstTS, err1 := time.Parse(time.RFC3339, window[0].TS)
	lastTS, err2 := time.Parse(time.RFC3339, window[len(window)-1].TS)
	if err1 != nil || err2 != nil {
		return true
	}
	if lastTS.Before(firstTS) {
		return false
	}
	maxSpan := time.Duration(durationMin+2) * time.Minute
	return lastTS.Sub(firstTS) <= maxSpan
}

func (c *machineCollector) emitMachineLoad(ev pushEvent) {
	c.mu.RLock()
	emitFn := c.emitPush
	push := c.push
	c.mu.RUnlock()

	if emitFn != nil {
		emitFn(ev)
		return
	}
	if push != nil {
		push.EmitExternal(ev)
	}
}

// UpdateAlerts validates, saves, and applies a new list of alert rules.
func (c *machineCollector) UpdateAlerts(rules []MachineAlertRule) ([]MachineAlertRule, error) {
	clean, err := validateMachineAlertRules(rules)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	prevAlerts := c.alerts
	prevFiring := make(map[string]bool, len(c.firing))
	for k, v := range c.firing {
		prevFiring[k] = v
	}

	nextFiring := make(map[string]bool, len(clean))
	for i := range clean {
		id := clean[i].ID
		if clean[i].Enabled && prevFiring[id] {
			nextFiring[id] = true
			clean[i].Firing = true
		}
	}
	c.alerts = clean
	c.firing = nextFiring
	if err := c.saveAlertsLocked(); err != nil {
		c.alerts = prevAlerts
		c.firing = prevFiring
		c.mu.Unlock()
		return nil, err
	}
	events := c.evaluateAlertsLocked()
	out := slices.Clone(c.alerts)
	c.mu.Unlock()

	for _, ev := range events {
		c.emitMachineLoad(ev)
	}
	return out, nil
}

func decodeMachineAlertsBody(w http.ResponseWriter, r *http.Request) ([]MachineAlertRule, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return nil, false
	}
	if trimmed[0] == '[' {
		var rules []MachineAlertRule
		if err := json.Unmarshal(trimmed, &rules); err != nil {
			jsonError(w, "invalid JSON body", http.StatusBadRequest)
			return nil, false
		}
		return rules, true
	}
	var body struct {
		Alerts []MachineAlertRule `json:"alerts"`
	}
	if err := json.Unmarshal(trimmed, &body); err != nil || body.Alerts == nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return nil, false
	}
	return body.Alerts, true
}

func (c *machineCollector) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireWriteRole(w, r) {
		return
	}
	rules, ok := decodeMachineAlertsBody(w, r)
	if !ok {
		return
	}
	updated, err := c.UpdateAlerts(rules)
	if err != nil {
		var ce *codedError
		if errors.As(err, &ce) {
			jsonErrorCode(w, ce.code, ce.msg, ce.status)
			return
		}
		log.Printf("machine: save alerts failed: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"ok": true, "alerts": updated})
}
