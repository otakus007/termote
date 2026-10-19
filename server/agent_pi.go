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
	"strings"
)

// piJournal reads Pi (and Oh My Pi) JSONL session transcripts.
type piJournal struct {
	agent string
}

func (j piJournal) Agent() string {
	if j.agent != "" {
		return j.agent
	}
	return "pi"
}

func defaultPiDir(agent string) string {
	if agent == "omp" {
		if dir := os.Getenv("OMP_CODING_AGENT_DIR"); dir != "" && filepath.IsAbs(dir) {
			return dir
		}
		if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
			return filepath.Join(home, ".omp", "agent")
		}
		return ""
	}
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" && filepath.IsAbs(dir) {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		return filepath.Join(home, ".pi", "agent")
	}
	return ""
}

// Locate returns the validated transcript path for a Pi/OMP session, using
// os.Root containment and openNonblock.
func (j piJournal) Locate(s AgentSession) (string, error) {
	baseDir := s.ClaudeDir
	if baseDir == "" {
		baseDir = defaultPiDir(j.Agent())
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

	for _, rel := range []string{s.ID + ".jsonl", filepath.Join("sessions", s.ID+".jsonl")} {
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

type piRow struct {
	ID            string     `json:"id"`
	Type          string     `json:"type"`
	Role          string     `json:"role"`
	Timestamp     string     `json:"timestamp"`
	Model         string     `json:"model"`
	ModelID       string     `json:"modelId"`
	ThinkingLevel string     `json:"thinkingLevel"`
	Summary       string     `json:"summary"`
	Content       any        `json:"content"`
	Usage         *piUsage   `json:"usage"`
	Message       *piMessage `json:"message"`
}

type piMessage struct {
	ID         string   `json:"id"`
	Role       string   `json:"role"`
	Timestamp  any      `json:"timestamp"`
	Model      string   `json:"model"`
	ToolCallID string   `json:"toolCallId"`
	ToolName   string   `json:"toolName"`
	IsError    bool     `json:"isError"`
	Content    any      `json:"content"`
	Usage      *piUsage `json:"usage"`
}

type piUsage struct {
	Input               int64 `json:"input"`
	InputTokens         int64 `json:"input_tokens"`
	Output              int64 `json:"output"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheRead           int64 `json:"cacheRead"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheWrite          int64 `json:"cacheWrite"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	TotalTokens         int64 `json:"totalTokens"`
}

// Parse reads complete JSONL lines from r with agentMaxLine clipping.
func (piJournal) Parse(r io.Reader, start int64) ([]TranscriptEntry, int64) {
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
				if e, ok := parsePiRow(clip.out, lineStart); ok {
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
		if e, ok := parsePiRow(line, lineStart); ok {
			entries = append(entries, e)
		}
	}
}

func parsePiRow(line []byte, off int64) (TranscriptEntry, bool) {
	var row piRow
	if json.Unmarshal(line, &row) != nil {
		return TranscriptEntry{}, false
	}
	id := cmp.Or(row.ID, fmt.Sprintf("line-%d", off))
	switch row.Type {
	case "session":
		return TranscriptEntry{}, false
	case "model_change":
		m := cmp.Or(row.ModelID, row.Model, "unknown")
		return TranscriptEntry{
			ID:         id,
			TS:         row.Timestamp,
			Role:       "note",
			Parts:      []TranscriptPart{textPart("text", "Set model to "+m, claudeMaxText)},
			off:        off,
			bustReason: "model",
		}, true
	case "thinking_level_change":
		lvl := cmp.Or(row.ThinkingLevel, "default")
		return TranscriptEntry{
			ID:         id,
			TS:         row.Timestamp,
			Role:       "note",
			Parts:      []TranscriptPart{textPart("text", "Set effort to "+lvl, claudeMaxText)},
			off:        off,
			bustReason: "effort",
		}, true
	case "compaction", "branch_summary":
		summary := cmp.Or(strings.TrimSpace(row.Summary), "Context compacted")
		return TranscriptEntry{
			ID:         id,
			TS:         row.Timestamp,
			Role:       "summary",
			Parts:      []TranscriptPart{textPart("text", summary, claudeMaxText)},
			off:        off,
			bustReason: "compaction",
		}, true
	}

	msg := row.Message
	if msg == nil {
		msg = &piMessage{
			ID:      row.ID,
			Role:    row.Role,
			Model:   cmp.Or(row.Model, row.ModelID),
			Content: row.Content,
			Usage:   row.Usage,
		}
	}
	if msg.ID != "" && row.ID == "" {
		id = msg.ID
	}
	role := strings.ToLower(strings.TrimSpace(msg.Role))
	e := TranscriptEntry{ID: id, TS: row.Timestamp, off: off}

	switch role {
	case "user":
		e.Role = "user"
		appendPiContent(&e, msg.Content, true)
	case "assistant":
		e.Role = "assistant"
		appendPiContent(&e, msg.Content, false)
		if u := cmp.Or(msg.Usage, row.Usage); u != nil {
			inTok := max(0, u.Input, u.InputTokens)
			cacheRead := max(0, u.CacheRead, u.CacheReadTokens, u.CachedTokens)
			cacheWrite := max(0, u.CacheWrite, u.CacheCreationTokens)
			total := inTok + cacheRead + cacheWrite
			if total == 0 && u.TotalTokens > 0 {
				total = u.TotalTokens
			}
			e.usage = &entryUsage{
				contextTokens:       total,
				model:               strings.TrimSpace(cmp.Or(msg.Model, row.Model, row.ModelID)),
				inputTokens:         inTok,
				cacheReadTokens:     cacheRead,
				cacheCreationTokens: cacheWrite,
			}
		}
	case "toolresult", "tool":
		e.Role = "user"
		res, clipped := clampText(stripANSI(toolResultText(msg.Content)), claudeMaxResult)
		e.Parts = append(e.Parts, TranscriptPart{
			Kind: "tool", Tool: "result", ToolID: msg.ToolCallID,
			Result: res, IsError: msg.IsError, Orphan: true, Clipped: clipped,
		})
	default:
		return TranscriptEntry{}, false
	}

	if len(e.Parts) == 0 && e.usage == nil {
		return TranscriptEntry{}, false
	}
	return e, true
}

func appendPiContent(e *TranscriptEntry, content any, isUser bool) {
	switch c := content.(type) {
	case string:
		t := c
		if isUser {
			t = stripPastedTags(t)
		}
		if strings.TrimSpace(t) != "" {
			e.Parts = append(e.Parts, textPart("text", t, claudeMaxText))
		}
	case []any:
		for _, item := range c {
			b, _ := item.(map[string]any)
			switch jsonStr(b["type"]) {
			case "text":
				t := jsonStr(b["text"])
				if isUser {
					t = stripPastedTags(t)
				}
				if strings.TrimSpace(t) != "" {
					e.Parts = append(e.Parts, textPart("text", t, claudeMaxText))
				}
			case "thinking", "reasoning":
				if t := cmp.Or(jsonStr(b["thinking"]), jsonStr(b["text"])); strings.TrimSpace(t) != "" {
					e.Parts = append(e.Parts, textPart("thinking", t, claudeMaxText))
				}
			case "toolCall", "tool_use", "tool":
				name := cmp.Or(jsonStr(b["name"]), jsonStr(b["tool"]), "tool")
				in, _ := b["arguments"].(map[string]any)
				if in == nil {
					in, _ = b["input"].(map[string]any)
				}
				e.Parts = append(e.Parts, TranscriptPart{
					Kind: "tool", Tool: name, ToolID: cmp.Or(jsonStr(b["id"]), jsonStr(b["toolCallId"])),
					Input: summarizeToolInput(in), Detail: claudeToolDetail(name, in),
					sensitive: claudeInputSensitive(in),
				})
			case "image":
				e.Parts = append(e.Parts, TranscriptPart{Kind: "image"})
			}
		}
	}
}
