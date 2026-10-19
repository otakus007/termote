package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const hookMarker = "termote-beacon.sh"

// beaconsDir returns <stateDir>/beacons.
func (c *cli) beaconsDir() string {
	return filepath.Join(c.stateDir(), "beacons")
}

// cmdHooks handles `termote hooks install [--uninstall]`.
func (c *cli) cmdHooks(args []string) error {
	fs := c.newFlagSet("hooks")
	var uninstall bool
	fs.BoolVar(&uninstall, "uninstall", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagErr(err)
	}
	sub := "install"
	if len(pos) > 0 {
		sub = pos[0]
		pos = pos[1:]
	}
	if len(pos) > 0 {
		return usageError("usage: termote hooks install [--uninstall]")
	}
	switch sub {
	case "install":
		if uninstall {
			return c.uninstallHooks()
		}
		return c.installHooks()
	case "uninstall":
		return c.uninstallHooks()
	default:
		return usageError("usage: termote hooks install [--uninstall]")
	}
}

func (c *cli) installHooks() error {
	dir := c.beaconsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create beacons dir: %w", err)
	}
	if err := checkPrivateDir(dir); err != nil {
		return err
	}
	scriptPath := filepath.Join(dir, hookMarker)
	script := fmt.Sprintf(`#!/usr/bin/env sh
# Managed by termote hooks install — writes a lifecycle beacon to %s.
set -eu
HARNESS="${1:-claude}"
STATUS="${2:-idle}"
PANE_KEY="${TMUX_PANE:-${HERDR_PANE_ID:-${TERMOTE_PANE:-}}}"
[ -n "$PANE_KEY" ] || exit 0
PANE_KEY="$(printf '%%s' "$PANE_KEY" | tr -cd 'A-Za-z0-9_.:%%-')"
[ -n "$PANE_KEY" ] || exit 0
AGENT_PID="${PPID:-$$}"
TS="$(date -u +"%%Y-%%m-%%dT%%H:%%M:%%SZ")"
DIR=%q
umask 077
TMP="$DIR/.beacon-${PANE_KEY}-$$.tmp"
printf '{"harness":"%%s","status":"%%s","pid":%%s,"updatedAt":"%%s"}\n' "$HARNESS" "$STATUS" "$AGENT_PID" "$TS" > "$TMP"
mv -f "$TMP" "$DIR/${PANE_KEY}.json"
`, dir, dir)

	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return fmt.Errorf("write %s: %w", scriptPath, err)
	}
	_ = os.Chmod(scriptPath, 0o700)

	claudeSettings := filepath.Join(c.home, ".claude", "settings.json")
	if err := mergeClaudeHooks(claudeSettings, scriptPath, false); err != nil {
		return err
	}
	c.infof("Installed Termote agent lifecycle hooks in %s", dir)
	return nil
}

func (c *cli) uninstallHooks() error {
	dir := c.beaconsDir()
	scriptPath := filepath.Join(dir, hookMarker)
	_ = os.Remove(scriptPath)

	claudeSettings := filepath.Join(c.home, ".claude", "settings.json")
	if err := mergeClaudeHooks(claudeSettings, scriptPath, true); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c.infof("Removed Termote agent lifecycle hooks")
	return nil
}

func mergeClaudeHooks(settingsPath, scriptPath string, remove bool) error {
	dir := filepath.Dir(settingsPath)
	if remove {
		if _, err := os.Stat(settingsPath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	} else {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	root := map[string]any{}
	if data, err := os.ReadFile(settingsPath); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
		_ = os.WriteFile(settingsPath+".bak", data, 0o600)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		if remove {
			return nil
		}
		hooks = map[string]any{}
	}

	events := map[string]string{
		"UserPromptSubmit": "working",
		"PreToolUse":       "working",
		"Notification":     "blocked",
		"Stop":             "done",
	}

	for ev, st := range events {
		existing, _ := hooks[ev].([]any)
		var kept []any
		for _, item := range existing {
			if !entryContainsMarker(item, hookMarker) {
				kept = append(kept, item)
			}
		}
		if !remove {
			cmd := fmt.Sprintf("%q claude %s", scriptPath, st)
			kept = append(kept, map[string]any{
				"hooks": []any{
					map[string]any{
						"type":    "command",
						"command": cmd,
					},
				},
			})
		}
		if len(kept) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = kept
		}
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(settingsPath, out, 0o600)
}

func entryContainsMarker(v any, marker string) bool {
	b, err := json.Marshal(v)
	return err == nil && strings.Contains(string(b), marker)
}
