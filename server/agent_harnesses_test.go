package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentHarnessesCatalogAndOptionValidation(t *testing.T) {
	harnesses := listAgentHarnesses()
	if len(harnesses) < 10 {
		t.Fatalf("expected at least 10 harnesses, got %d", len(harnesses))
	}

	aliasCases := map[string]string{
		"claude":          "claude",
		"codex.exe":       "codex",
		"opencode":        "opencode",
		"pi-coding-agent": "pi",
		"omp":             "omp",
		"gemini":          "gemini",
		"agy":             "antigravity",
		"cursor-agent":    "cursor",
		"grok":            "grok",
		"hermes":          "hermes",
		"openclaw":        "openclaw",
		"unknown-tool":    "",
	}
	for in, want := range aliasCases {
		if got := normalizeHarnessName(in); got != want {
			t.Errorf("normalizeHarnessName(%q) = %q, want %q", in, got, want)
		}
	}

	validFlags := []string{
		"--dangerously-skip-permissions",
		"--continue",
		"--model=claude-sonnet-4-6",
		"-m",
	}
	if !validateLaunchOptions(validFlags) {
		t.Fatalf("expected validFlags to pass: %v", validFlags)
	}

	invalidFlags := []string{
		"",
		"not-a-flag",
		"--flag with space",
		"--flag\ninjected",
		"--flag;rm",
		"--flag$var",
	}
	for _, bad := range invalidFlags {
		if validLaunchOptionFlag(bad) {
			t.Errorf("expected flag %q to be rejected", bad)
		}
	}
}

func TestReadAgentBeaconSecurityAndFreshness(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "beacons")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	writeBeacon := func(name string, payload any) string {
		p := filepath.Join(dir, name)
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// 1. Valid fresh beacon with live PID (os.Getpid())
	writeBeacon("%1.json", map[string]any{
		"harness":   "opencode",
		"status":    "working",
		"pid":       os.Getpid(),
		"sessionId": "sess-123",
		"updatedAt": now.Add(-30 * time.Second).Format(time.RFC3339),
	})
	b, ok := readAgentBeacon(dir, "%1", now)
	if !ok || b.Harness != "opencode" || b.Status != "working" || b.SessionID != "sess-123" {
		t.Fatalf("expected valid beacon, got ok=%v b=%+v", ok, b)
	}

	// 2. Stale beacon (> 10 minutes old) is rejected
	writeBeacon("%2.json", map[string]any{
		"harness":   "pi",
		"status":    "working",
		"pid":       os.Getpid(),
		"updatedAt": now.Add(-15 * time.Minute).Format(time.RFC3339),
	})
	if _, ok := readAgentBeacon(dir, "%2", now); ok {
		t.Fatal("expected stale beacon to be rejected")
	}

	// 3. Dead PID is rejected
	origAlive := beaconPIDAlive
	beaconPIDAlive = func(pid int) bool { return pid == os.Getpid() }
	defer func() { beaconPIDAlive = origAlive }()

	writeBeacon("%3.json", map[string]any{
		"harness":   "gemini",
		"status":    "done",
		"pid":       999999,
		"updatedAt": now.Format(time.RFC3339),
	})
	if _, ok := readAgentBeacon(dir, "%3", now); ok {
		t.Fatal("expected dead PID beacon to be rejected")
	}

	// 4. Oversized file (> 4 KB) is rejected
	overPath := filepath.Join(dir, "%4.json")
	if err := os.WriteFile(overPath, bytes.Repeat([]byte("a"), beaconMaxBytes+10), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readAgentBeacon(dir, "%4", now); ok {
		t.Fatal("expected oversized beacon to be rejected")
	}

	// 5. Path traversal paneKey is rejected
	if _, ok := readAgentBeacon(dir, "../%1", now); ok {
		t.Fatal("expected traversal key to be rejected")
	}
}

func TestOpenCodeAndPiTranscriptAdapters(t *testing.T) {
	tmp := t.TempDir()
	ocFile := filepath.Join(tmp, "oc-session.jsonl")
	ocLines := strings.Join([]string{
		`{"id":"m1","role":"user","timestamp":"2026-10-16T12:00:00Z","text":"Explain main.go"}`,
		`{"id":"m2","role":"assistant","timestamp":"2026-10-16T12:00:05Z","model":"claude-sonnet-4-6","parts":[{"type":"text","text":"Reading main.go"},{"type":"tool","tool":"Read","callID":"c1","input":{"file_path":"main.go"},"output":"package main"}],"tokens":{"input":1200,"output":80,"cached_tokens":4000}}`,
		`{"id":"m3","type":"compaction","timestamp":"2026-10-16T12:01:00Z","text":"Summarized history"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(ocFile, []byte(ocLines), 0o600); err != nil {
		t.Fatal(err)
	}

	var oc opencodeJournal
	loc, err := oc.Locate(AgentSession{Agent: "opencode", ID: "oc-session", ClaudeDir: tmp})
	if err != nil || loc != ocFile {
		t.Fatalf("opencode Locate = (%q, %v), want %q", loc, err, ocFile)
	}
	entries, off := oc.Parse(strings.NewReader(ocLines), 0)
	if off != int64(len(ocLines)) || len(entries) != 3 {
		t.Fatalf("opencode Parse = %d entries (off=%d), want 3", len(entries), off)
	}
	if entries[1].usage == nil || entries[1].usage.cacheReadTokens != 4000 {
		t.Fatalf("expected opencode usage with cacheReadTokens=4000, got %+v", entries[1].usage)
	}
	if entries[2].Role != "summary" || entries[2].bustReason != "compaction" {
		t.Fatalf("expected compaction summary row, got %+v", entries[2])
	}

	// Pi transcript adapter
	piFile := filepath.Join(tmp, "pi-session.jsonl")
	piLines := strings.Join([]string{
		`{"id":"p1","type":"message","timestamp":"2026-10-16T12:00:00Z","message":{"role":"user","content":"Run tests"}}`,
		`{"id":"p2","type":"message","timestamp":"2026-10-16T12:00:04Z","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"Running tests now"}],"usage":{"input":500,"output":40,"cacheRead":2500}}}`,
		`{"id":"p3","type":"model_change","timestamp":"2026-10-16T12:00:10Z","modelId":"claude-opus-4-7"}`,
		`{"id":"p4","type":"thinking_level_change","timestamp":"2026-10-16T12:00:15Z","thinkingLevel":"high"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(piFile, []byte(piLines), 0o600); err != nil {
		t.Fatal(err)
	}

	pj := piJournal{agent: "pi"}
	piLoc, err := pj.Locate(AgentSession{Agent: "pi", ID: "pi-session", ClaudeDir: tmp})
	if err != nil || piLoc != piFile {
		t.Fatalf("pi Locate = (%q, %v), want %q", piLoc, err, piFile)
	}
	piEntries, _ := pj.Parse(strings.NewReader(piLines), 0)
	if len(piEntries) != 4 {
		t.Fatalf("pi Parse = %d entries, want 4", len(piEntries))
	}
	if piEntries[1].usage == nil || piEntries[1].usage.cacheReadTokens != 2500 {
		t.Fatalf("expected pi usage cacheReadTokens=2500, got %+v", piEntries[1].usage)
	}
	if piEntries[2].bustReason != "model" || piEntries[3].bustReason != "effort" {
		t.Fatalf("expected model and effort bust reasons, got %q and %q", piEntries[2].bustReason, piEntries[3].bustReason)
	}

	// Confined explicit Rollout inside baseDir succeeds, while outside baseDir is rejected.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.jsonl")
	if err := os.WriteFile(outsideFile, []byte(ocLines), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := oc.Locate(AgentSession{Agent: "opencode", Rollout: outsideFile, ClaudeDir: tmp}); err == nil {
		t.Fatalf("expected opencode Locate to reject Rollout outside baseDir")
	}
	if _, err := pj.Locate(AgentSession{Agent: "pi", Rollout: outsideFile, ClaudeDir: tmp}); err == nil {
		t.Fatalf("expected pi Locate to reject Rollout outside baseDir")
	}
	if got, err := oc.Locate(AgentSession{Agent: "opencode", Rollout: ocFile, ClaudeDir: tmp}); err != nil || got != ocFile {
		t.Fatalf("expected opencode Locate with confined Rollout to succeed, got (%q, %v)", got, err)
	}
	if got, err := pj.Locate(AgentSession{Agent: "pi", Rollout: piFile, ClaudeDir: tmp}); err != nil || got != piFile {
		t.Fatalf("expected pi Locate with confined Rollout to succeed, got (%q, %v)", got, err)
	}
}

func TestCLIHooksInstallAndUninstall(t *testing.T) {
	home := t.TempDir()
	var out, errBuf bytes.Buffer
	stateHome := filepath.Join(home, ".state")
	c := &cli{
		home: home,
		goos: "linux",
		getenv: func(k string) string {
			if k == "XDG_STATE_HOME" {
				return stateHome
			}
			return ""
		},
		out:    &out,
		errOut: &errBuf,
	}

	if err := c.cmdHooks([]string{"install"}); err != nil {
		t.Fatalf("hooks install failed: %v", err)
	}
	scriptPath := filepath.Join(c.beaconsDir(), hookMarker)
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("expected hook script at %s: %v", scriptPath, err)
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil || !strings.Contains(string(data), hookMarker) {
		t.Fatalf("expected %s to contain %s, got err=%v data=%s", settingsPath, hookMarker, err, data)
	}

	if err := c.cmdHooks([]string{"install", "--uninstall"}); err != nil {
		t.Fatalf("hooks uninstall failed: %v", err)
	}
	if _, err := os.Stat(scriptPath); !os.IsNotExist(err) {
		t.Fatalf("expected hook script removed, got err=%v", err)
	}
	dataAfter, err := os.ReadFile(settingsPath)
	if err != nil || strings.Contains(string(dataAfter), hookMarker) {
		t.Fatalf("expected %s to have hookMarker removed, got %s", settingsPath, dataAfter)
	}
}
