package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var opencodeSessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// opencodeJournal reads OpenCode JSONL transcripts.
type opencodeJournal struct{}

func (opencodeJournal) Agent() string { return "opencode" }

func defaultOpenCodeDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "opencode")
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

// Locate returns the validated transcript path for an OpenCode session, using
// os.Root containment when resolving by session ID inside the OpenCode data
// directory.
func (opencodeJournal) Locate(s AgentSession) (string, error) {
	baseDir := s.ClaudeDir
	if baseDir == "" {
		baseDir = defaultOpenCodeDir()
	}
	if baseDir == "" || !filepath.IsAbs(baseDir) {
		return "", errNoTranscript
	}
	rt, err := os.OpenRoot(baseDir)
	if err != nil {
		return "", errNoTranscript
	}
	defer rt.Close()

	if s.Rollout != "" {
		if !filepath.IsAbs(s.Rollout) {
			return "", errNoTranscript
		}
		cleanRollout := filepath.Clean(s.Rollout)
		ext := strings.ToLower(filepath.Ext(cleanRollout))
		if ext != ".json" && ext != ".jsonl" {
			return "", errNoTranscript
		}
		rel, err := filepath.Rel(filepath.Clean(baseDir), cleanRollout)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", errNoTranscript
		}
		f, err := rt.OpenFile(rel, os.O_RDONLY|openNonblock, 0)
		if err != nil {
			return "", errNoTranscript
		}
		fi, err := f.Stat()
		f.Close()
		if err != nil || !fi.Mode().IsRegular() {
			return "", errNoTranscript
		}
		return filepath.Join(baseDir, rel), nil
	}
	if !opencodeSessionIDRe.MatchString(s.ID) {
		return "", errNoTranscript
	}

	candidates := []string{
		s.ID + ".jsonl",
		filepath.Join("sessions", s.ID+".jsonl"),
		filepath.Join("storage", "session", s.ID+".jsonl"),
	}
	for _, rel := range candidates {
		f, err := rt.OpenFile(rel, os.O_RDONLY|openNonblock, 0)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		f.Close()
		if err == nil && fi.Mode().IsRegular() {
			return filepath.Join(baseDir, rel), nil
		}
	}
	return "", errNoTranscript
}

type opencodeRow struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Role      string `json:"role"`
	Timestamp string `json:"timestamp"`
	Time      string `json:"time"`
	Model     string `json:"model"`
	ModelID   string `json:"modelID"`
	Summary   bool   `json:"summary"`
	Text      string `json:"text"`
	Content   any    `json:"content"`
	Parts     []struct {
		Type   string         `json:"type"`
		Text   string         `json:"text"`
		Tool   string         `json:"tool"`
		Name   string         `json:"name"`
		CallID string         `json:"callID"`
		ID     string         `json:"id"`
		Input  map[string]any `json:"input"`
		Output any            `json:"output"`
		State  *struct {
			Status string         `json:"status"`
			Input  map[string]any `json:"input"`
			Output any            `json:"output"`
			Error  string         `json:"error"`
		} `json:"state"`
	} `json:"parts"`
	Tokens *opencodeTokens `json:"tokens"`
	Usage  *opencodeTokens `json:"usage"`
	Info   *struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		ModelID string          `json:"modelID"`
		Summary bool            `json:"summary"`
		Tokens  *opencodeTokens `json:"tokens"`
	} `json:"info"`
}

type opencodeTokens struct {
	Input               int64 `json:"input"`
	InputTokens         int64 `json:"input_tokens"`
	Output              int64 `json:"output"`
	OutputTokens        int64 `json:"output_tokens"`
	Reasoning           int64 `json:"reasoning"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	Cache               *struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// Parse reads complete JSONL lines from r with the same agentMaxLine clipping
// bounds as claudeJournal.
func (opencodeJournal) Parse(r io.Reader, start int64) ([]TranscriptEntry, int64) {
	br := lineReaders.Get().(*bufio.Reader)
	br.Reset(r)
	defer func() {
		br.Reset(nil)
		lineReaders.Put(br)
	}()
	var entries []TranscriptEntry
	off := start
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			lineStart, n := off, int64(len(line))
			var clip longLineClipper
			clip.write(line)
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = br.ReadSlice('\n')
				n += int64(len(line))
				clip.write(line)
			}
			if err != nil {
				return entries, off
			}
			off = lineStart + n
			if !clip.overflow {
				if e, ok := parseOpenCodeRow(clip.out, lineStart); ok {
					entries = append(entries, e)
				}
				continue
			}
			entries = append(entries, TranscriptEntry{
				ID: fmt.Sprintf("clipped-%d", lineStart), Role: "note", off: lineStart,
				Parts: []TranscriptPart{{Kind: "text", Clipped: true}},
			})
			continue
		}
		if err != nil {
			return entries, off
		}
		lineStart := off
		off += int64(len(line))
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if e, ok := parseOpenCodeRow(line, lineStart); ok {
			entries = append(entries, e)
		}
	}
}

func parseOpenCodeRow(line []byte, off int64) (TranscriptEntry, bool) {
	var row opencodeRow
	if json.Unmarshal(line, &row) != nil {
		return TranscriptEntry{}, false
	}
	role := strings.ToLower(strings.TrimSpace(row.Role))
	if role == "" && row.Info != nil {
		role = strings.ToLower(strings.TrimSpace(row.Info.Role))
	}
	rowType := strings.ToLower(strings.TrimSpace(row.Type))
	if rowType == "compaction" || row.Summary || (row.Info != nil && row.Info.Summary) {
		id := cmp.Or(row.ID, fmt.Sprintf("line-%d", off))
		text := cmp.Or(strings.TrimSpace(row.Text), "Context compacted")
		return TranscriptEntry{
			ID:         id,
			TS:         cmp.Or(row.Timestamp, row.Time),
			Role:       "summary",
			Parts:      []TranscriptPart{textPart("text", text, claudeMaxText)},
			off:        off,
			bustReason: "compaction",
		}, true
	}
	if role == "" {
		switch rowType {
		case "user", "user_message":
			role = "user"
		case "assistant", "assistant_message", "text", "tool", "step-finish", "message":
			role = "assistant"
		default:
			return TranscriptEntry{}, false
		}
	}
	if role != "user" && role != "assistant" {
		return TranscriptEntry{}, false
	}
	id := cmp.Or(row.ID, fmt.Sprintf("line-%d", off))
	if row.Info != nil && row.Info.ID != "" && row.ID == "" {
		id = row.Info.ID
	}
	e := TranscriptEntry{
		ID:   id,
		TS:   cmp.Or(row.Timestamp, row.Time),
		Role: role,
		off:  off,
	}
	if strings.TrimSpace(row.Text) != "" {
		t := row.Text
		if role == "user" {
			t = stripPastedTags(t)
		}
		e.Parts = append(e.Parts, textPart("text", t, claudeMaxText))
	}
	switch c := row.Content.(type) {
	case string:
		if strings.TrimSpace(c) != "" && len(e.Parts) == 0 {
			e.Parts = append(e.Parts, textPart("text", c, claudeMaxText))
		}
	case []any:
		for _, item := range c {
			b, _ := item.(map[string]any)
			switch jsonStr(b["type"]) {
			case "text":
				if t := jsonStr(b["text"]); strings.TrimSpace(t) != "" {
					e.Parts = append(e.Parts, textPart("text", t, claudeMaxText))
				}
			case "thinking", "reasoning":
				if t := cmp.Or(jsonStr(b["thinking"]), jsonStr(b["text"])); strings.TrimSpace(t) != "" {
					e.Parts = append(e.Parts, textPart("thinking", t, claudeMaxText))
				}
			case "tool_use", "tool":
				name := cmp.Or(jsonStr(b["name"]), jsonStr(b["tool"]), "tool")
				in, _ := b["input"].(map[string]any)
				e.Parts = append(e.Parts, TranscriptPart{
					Kind: "tool", Tool: name, ToolID: cmp.Or(jsonStr(b["id"]), jsonStr(b["callID"])),
					Input: summarizeToolInput(in), Detail: claudeToolDetail(name, in),
					sensitive: claudeInputSensitive(in),
				})
			}
		}
	}
	for _, p := range row.Parts {
		switch p.Type {
		case "text":
			if strings.TrimSpace(p.Text) != "" {
				e.Parts = append(e.Parts, textPart("text", p.Text, claudeMaxText))
			}
		case "reasoning", "thinking":
			if strings.TrimSpace(p.Text) != "" {
				e.Parts = append(e.Parts, textPart("thinking", p.Text, claudeMaxText))
			}
		case "tool", "tool_use", "tool-call":
			name := cmp.Or(p.Tool, p.Name, "tool")
			in := p.Input
			var out any = p.Output
			var isErr bool
			if p.State != nil {
				if len(in) == 0 {
					in = p.State.Input
				}
				if out == nil {
					out = p.State.Output
				}
				if p.State.Error != "" {
					out = p.State.Error
					isErr = true
				} else if p.State.Status == "error" || p.State.Status == "failed" {
					isErr = true
				}
			}
			res, clipped := clampText(stripANSI(toolResultText(out)), claudeMaxResult)
			e.Parts = append(e.Parts, TranscriptPart{
				Kind: "tool", Tool: name, ToolID: cmp.Or(p.CallID, p.ID),
				Input: summarizeToolInput(in), Result: res, IsError: isErr, Clipped: clipped,
				Detail: claudeToolDetail(name, in), sensitive: claudeInputSensitive(in),
			})
		}
	}

	tok := cmp.Or(row.Tokens, row.Usage)
	if tok == nil && row.Info != nil {
		tok = row.Info.Tokens
	}
	if tok != nil && role == "assistant" {
		inTok := max(0, tok.Input, tok.InputTokens)
		cacheRead := max(0, tok.CachedTokens, tok.CacheReadTokens)
		cacheWrite := max(0, tok.CacheCreationTokens)
		if tok.Cache != nil {
			cacheRead = max(cacheRead, tok.Cache.Read)
			cacheWrite = max(cacheWrite, tok.Cache.Write)
		}
		model := cmp.Or(row.Model, row.ModelID)
		if model == "" && row.Info != nil {
			model = row.Info.ModelID
		}
		e.usage = &entryUsage{
			contextTokens:       inTok + cacheRead + cacheWrite,
			model:               strings.TrimSpace(model),
			inputTokens:         inTok,
			cacheReadTokens:     cacheRead,
			cacheCreationTokens: cacheWrite,
		}
	}
	if len(e.Parts) == 0 && e.usage == nil {
		return TranscriptEntry{}, false
	}
	return e, true
}
