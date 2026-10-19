package main

import (
	"cmp"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// beaconMaxBytes caps the size of a lifecycle beacon JSON file.
	beaconMaxBytes = 4096
	// beaconMaxAge is how long a beacon file remains fresh without an update.
	beaconMaxAge = 10 * time.Minute
)

var (
	// beaconKeyRe matches a safe pane key segment used for <paneKey>.json.
	beaconKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.:%-]{1,64}$`)
	// beaconFileRe matches valid beacon file names in <stateDir>/beacons.
	beaconFileRe = regexp.MustCompile(`^[A-Za-z0-9_.:%-]{1,64}\.json$`)
	// launchOptionFlagRe validates individual CLI option flags passed at agent start.
	launchOptionFlagRe = regexp.MustCompile(`^--?[a-zA-Z0-9][a-zA-Z0-9=_.:/-]{0,63}$`)
)

// defaultBeaconsDir is <stateDir>/beacons when configured by serve; tests
// may override it.
var defaultBeaconsDir string

// beaconPIDAlive reports whether pid is a live process; tests may override it.
var beaconPIDAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	_, ok := procStartTime(pid)
	return ok
}

// agentLaunchOption is a toggleable launch flag chip for a harness.
type agentLaunchOption struct {
	Flag        string `json:"flag"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	NoPrompts   bool   `json:"noPrompts,omitempty"`
}

// agentHarness describes a supported CLI coding agent harness.
type agentHarness struct {
	Name          string              `json:"name"`
	Label         string              `json:"label"`
	Command       string              `json:"command"`
	Binaries      []string            `json:"binaries"`
	Transcript    bool                `json:"transcript"`
	Options       []agentLaunchOption `json:"options,omitempty"`
	SlashCommands []agentCommand      `json:"slashCommands,omitempty"`
}

// agentBeacon is the JSON payload written by a lifecycle hook to
// <stateDir>/beacons/<paneKey>.json.
type agentBeacon struct {
	Harness    string `json:"harness"`
	Agent      string `json:"agent"`
	Status     string `json:"status"`
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Transcript string `json:"transcript"`
	Cwd        string `json:"cwd"`
	UpdatedAt  string `json:"updatedAt"`
	Timestamp  int64  `json:"ts"`
}

var defaultAgentHarnesses = []agentHarness{
	{
		Name:       "claude",
		Label:      "Claude Code",
		Command:    "claude",
		Binaries:   []string{"claude"},
		Transcript: true,
		Options: []agentLaunchOption{
			{Flag: "--dangerously-skip-permissions", Label: "Skip permissions", Description: "Bypass all permission prompts", NoPrompts: true},
			{Flag: "--continue", Label: "Continue", Description: "Resume the most recent conversation"},
			{Flag: "--verbose", Label: "Verbose", Description: "Enable verbose logging"},
			{Flag: "--chrome", Label: "Chrome", Description: "Enable browser integration"},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Switch the active Claude model", Source: "system", Kind: "command"},
			{Name: "effort", Description: "Adjust reasoning effort level", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Compact conversation context", Source: "system", Kind: "command"},
			{Name: "resume", Description: "Resume a previous session", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "codex",
		Label:      "Codex",
		Command:    "codex",
		Binaries:   []string{"codex"},
		Transcript: true,
		Options: []agentLaunchOption{
			{Flag: "--yolo", Label: "YOLO", Description: "Run commands without approvals or sandboxing", NoPrompts: true},
			{Flag: "--full-auto", Label: "Full auto", Description: "Auto-approve workspace-write commands", NoPrompts: true},
			{Flag: "--no-daemon", Label: "No daemon", Description: "Write rollout directly from the TUI process"},
			{Flag: "--search", Label: "Web search", Description: "Enable live web search tool"},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Switch the active Codex model", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Compact conversation context", Source: "system", Kind: "command"},
			{Name: "resume", Description: "Resume a saved rollout", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "opencode",
		Label:      "OpenCode",
		Command:    "opencode",
		Binaries:   []string{"opencode"},
		Transcript: true,
		Options: []agentLaunchOption{
			{Flag: "--continue", Label: "Continue", Description: "Continue the last OpenCode session"},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Switch model", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Summarize and compact context", Source: "system", Kind: "command"},
			{Name: "resume", Description: "Resume session", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "pi",
		Label:      "Pi",
		Command:    "pi",
		Binaries:   []string{"pi", "pi-coding-agent"},
		Transcript: true,
		Options: []agentLaunchOption{
			{Flag: "--continue", Label: "Continue", Description: "Continue the most recent session"},
			{Flag: "--no-session", Label: "Ephemeral", Description: "Run without saving session history"},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Select model", Source: "system", Kind: "command"},
			{Name: "effort", Description: "Set thinking level", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Compact context", Source: "system", Kind: "command"},
			{Name: "tree", Description: "Browse session branch tree", Source: "system", Kind: "command"},
			{Name: "resume", Description: "Switch session", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "omp",
		Label:      "Oh My Pi",
		Command:    "omp",
		Binaries:   []string{"omp"},
		Transcript: true,
		Options: []agentLaunchOption{
			{Flag: "--continue", Label: "Continue", Description: "Continue the most recent session"},
			{Flag: "--no-session", Label: "Ephemeral", Description: "Run without saving session history"},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Select model", Source: "system", Kind: "command"},
			{Name: "effort", Description: "Set thinking level", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Compact context", Source: "system", Kind: "command"},
			{Name: "tree", Description: "Browse session branch tree", Source: "system", Kind: "command"},
			{Name: "resume", Description: "Switch session", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "gemini",
		Label:      "Gemini CLI",
		Command:    "gemini",
		Binaries:   []string{"gemini"},
		Transcript: false,
		Options: []agentLaunchOption{
			{Flag: "--yolo", Label: "YOLO", Description: "Automatically accept all actions", NoPrompts: true},
			{Flag: "--sandbox", Label: "Sandbox", Description: "Run tools in a sandbox"},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Switch Gemini model", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Compress chat history", Source: "system", Kind: "command"},
			{Name: "resume", Description: "Resume saved checkpoint", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "antigravity",
		Label:      "Antigravity",
		Command:    "agy",
		Binaries:   []string{"agy", "antigravity"},
		Transcript: false,
		Options: []agentLaunchOption{
			{Flag: "--yolo", Label: "YOLO", Description: "Automatically approve actions", NoPrompts: true},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Switch model", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Compact conversation context", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "cursor",
		Label:      "Cursor Agent",
		Command:    "cursor-agent",
		Binaries:   []string{"cursor-agent"},
		Transcript: false,
		Options: []agentLaunchOption{
			{Flag: "--force", Label: "Force", Description: "Run without interactive confirmation", NoPrompts: true},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Switch model", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Compact context", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "grok",
		Label:      "Grok CLI",
		Command:    "grok",
		Binaries:   []string{"grok"},
		Transcript: false,
		Options: []agentLaunchOption{
			{Flag: "--yolo", Label: "YOLO", Description: "Skip confirmation prompts", NoPrompts: true},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Switch Grok model", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "hermes",
		Label:      "Hermes",
		Command:    "hermes",
		Binaries:   []string{"hermes"},
		Transcript: false,
		Options: []agentLaunchOption{
			{Flag: "--continue", Label: "Continue", Description: "Resume previous session"},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Switch model", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Compact context", Source: "system", Kind: "command"},
		},
	},
	{
		Name:       "openclaw",
		Label:      "OpenClaw",
		Command:    "openclaw",
		Binaries:   []string{"openclaw"},
		Transcript: false,
		Options: []agentLaunchOption{
			{Flag: "--continue", Label: "Continue", Description: "Resume previous session"},
		},
		SlashCommands: []agentCommand{
			{Name: "model", Description: "Switch model", Source: "system", Kind: "command"},
			{Name: "compact", Description: "Compact context", Source: "system", Kind: "command"},
		},
	},
}

// listAgentHarnesses returns a copy of the built-in harness catalog.
func listAgentHarnesses() []agentHarness {
	out := make([]agentHarness, len(defaultAgentHarnesses))
	for i, h := range defaultAgentHarnesses {
		h.Binaries = slices.Clone(h.Binaries)
		h.Options = slices.Clone(h.Options)
		h.SlashCommands = slices.Clone(h.SlashCommands)
		out[i] = h
	}
	return out
}

// normalizeHarnessName maps binary aliases (such as "agy" or "cursor-agent")
// to their canonical harness name, or "" when unknown.
func normalizeHarnessName(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimSuffix(s, ".exe")
	switch s {
	case "agy":
		return "antigravity"
	case "cursor-agent":
		return "cursor"
	case "pi-coding-agent":
		return "pi"
	}
	for _, h := range defaultAgentHarnesses {
		if h.Name == s || slices.Contains(h.Binaries, s) {
			return h.Name
		}
	}
	return ""
}

// lookupAgentHarness returns the harness definition for name, if supported.
func lookupAgentHarness(name string) (agentHarness, bool) {
	canon := normalizeHarnessName(name)
	for _, h := range defaultAgentHarnesses {
		if h.Name == canon {
			return h, true
		}
	}
	return agentHarness{}, false
}

// harnessSlashCommands returns the built-in slash commands of harness name.
func harnessSlashCommands(name string) []agentCommand {
	h, ok := lookupAgentHarness(name)
	if !ok {
		return []agentCommand{}
	}
	return slices.Clone(h.SlashCommands)
}

// validLaunchOptionFlag checks that flag matches the strict flag pattern and
// contains no control characters or whitespace.
func validLaunchOptionFlag(flag string) bool {
	if flag == "" || strings.ContainsAny(flag, "\x00\r\n\x1b\t ") {
		return false
	}
	return launchOptionFlagRe.MatchString(flag)
}

// validateLaunchOptions checks a slice of option flags (at most 16).
func validateLaunchOptions(flags []string) bool {
	if len(flags) > 16 {
		return false
	}
	for _, f := range flags {
		if !validLaunchOptionFlag(f) {
			return false
		}
	}
	return true
}

// normalizeBeaconStatus maps raw beacon status strings to Termote's canonical
// statuses ("working", "idle", "blocked", "done").
func normalizeBeaconStatus(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "working", "running", "busy", "thinking", "tool_use":
		return "working", true
	case "blocked", "waiting", "permission", "approval", "input", "needs_input":
		return "blocked", true
	case "done", "completed", "finished", "stop":
		return "done", true
	case "idle", "ready":
		return "idle", true
	}
	return "", false
}

// sanitizeBeaconFile returns "<paneKey>.json" when paneKey is a safe beacon
// key, or ("", false) otherwise.
func sanitizeBeaconFile(paneKey string) (string, bool) {
	k := strings.TrimSpace(paneKey)
	if strings.HasSuffix(k, ".json") {
		if !beaconFileRe.MatchString(k) {
			return "", false
		}
		return k, true
	}
	if !beaconKeyRe.MatchString(k) {
		return "", false
	}
	name := k + ".json"
	if !beaconFileRe.MatchString(name) {
		return "", false
	}
	return name, true
}

func parseBeaconTime(b agentBeacon, modTime time.Time) time.Time {
	if t, ok := parseCacheTime(b.UpdatedAt); ok {
		return t
	}
	if b.Timestamp > 1e11 {
		return time.UnixMilli(b.Timestamp)
	}
	if b.Timestamp > 0 {
		return time.Unix(b.Timestamp, 0)
	}
	return modTime
}

// readAgentBeacon reads and validates <beaconsDir>/<paneKey>.json using
// os.Root and nonblocking I/O. It rejects symlinks, non-regular files, files
// larger than 4 KB, files not owned by the server user, beacons older than
// 10 minutes, and beacons whose PID is dead.
func readAgentBeacon(beaconsDir, paneKey string, now time.Time) (agentBeacon, bool) {
	if beaconsDir == "" {
		return agentBeacon{}, false
	}
	fileName, ok := sanitizeBeaconFile(paneKey)
	if !ok {
		return agentBeacon{}, false
	}
	dirInfo, err := os.Lstat(beaconsDir)
	if err != nil || !dirInfo.IsDir() {
		return agentBeacon{}, false
	}
	if !privateUploadFile(dirInfo) || !fileOwnedByServer(dirInfo) {
		return agentBeacon{}, false
	}
	rt, err := os.OpenRoot(beaconsDir)
	if err != nil {
		return agentBeacon{}, false
	}
	defer rt.Close()

	f, err := rt.OpenFile(fileName, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return agentBeacon{}, false
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() <= 0 || fi.Size() > beaconMaxBytes || !fileOwnedByServer(fi) {
		return agentBeacon{}, false
	}
	data, err := io.ReadAll(io.LimitReader(f, beaconMaxBytes+1))
	if err != nil || int64(len(data)) > beaconMaxBytes {
		return agentBeacon{}, false
	}
	var b agentBeacon
	if json.Unmarshal(data, &b) != nil {
		return agentBeacon{}, false
	}
	harness := normalizeHarnessName(cmp.Or(b.Harness, b.Agent))
	if harness == "" {
		return agentBeacon{}, false
	}
	status, ok := normalizeBeaconStatus(b.Status)
	if !ok {
		return agentBeacon{}, false
	}
	if b.PID <= 0 || !beaconPIDAlive(b.PID) {
		return agentBeacon{}, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	ts := parseBeaconTime(b, fi.ModTime())
	if ts.IsZero() || now.Sub(ts) > beaconMaxAge {
		return agentBeacon{}, false
	}
	b.Harness = harness
	b.Agent = harness
	b.Status = status
	return b, true
}

// otherHarnessProcOf checks whether pid runs one of the non-Claude/non-Codex
// harnesses (opencode, pi, omp, gemini, antigravity, cursor, grok, hermes,
// openclaw).
func otherHarnessProcOf(pid int) (claudeProc, bool) {
	if !agentProcSupported {
		return claudeProc{}, false
	}
	exe := strings.ToLower(strings.TrimSuffix(procExeBase(pid), ".exe"))
	if exe == "" || exe == "claude" || exe == codexExeName {
		return claudeProc{}, false
	}
	harness := normalizeHarnessName(exe)
	if harness == "" && (exe == "node" || exe == "bun" || exe == "deno" || strings.HasPrefix(exe, "python")) {
		for _, arg := range procArgs(pid) {
			base := strings.ToLower(strings.TrimSuffix(filepath.Base(arg), ".js"))
			if h := normalizeHarnessName(base); h != "" && h != "claude" && h != "codex" {
				harness = h
				break
			}
		}
	}
	if harness == "" || harness == "claude" || harness == "codex" {
		return claudeProc{}, false
	}
	start, ok := procStartTime(pid)
	if !ok {
		return claudeProc{}, false
	}
	return claudeProc{agent: harness, pid: pid, procStart: start}, true
}
