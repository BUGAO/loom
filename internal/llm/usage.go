package llm

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"loom/internal/model"
)

// Token accounting for ACP sessions.
//
// ACP has no cost or usage field in the protocol, so the numbers come from the
// Claude Code session transcript that the runtime writes to disk. The join key
// is the session id: `session/new` hands it to us, and the transcript is named
// after it. We glob for the file rather than deriving the project directory
// from the agent home path — the directory-name encoding is an undocumented
// CLI detail, the filename is not (see docs/DECISIONS-v2.md D2).
//
// Everything here is best-effort by design: an unreadable or reshaped
// transcript yields a zero usage and a false ok, and the caller records the
// task as cost-unavailable. A cost number is never worth failing a task over,
// and a guessed one is worse than none.

// transcriptEntry is the subset of a transcript line we care about.
type transcriptEntry struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// modelUsage is per-model usage accumulated from a transcript.
type modelUsage map[string]*model.TokenUsage

func (m modelUsage) add(id string, u model.TokenUsage) {
	if m[id] == nil {
		m[id] = &model.TokenUsage{}
	}
	m[id].Add(u)
}

// total collapses per-model usage and prices each model separately.
func (m modelUsage) total() (model.TokenUsage, float64) {
	var sum model.TokenUsage
	var cost float64
	for id, u := range m {
		sum.Add(*u)
		cost += model.CostOf(id, *u)
	}
	return sum, cost
}

// transcriptRoot is where the Claude Code runtime keeps session transcripts.
func transcriptRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// findTranscript locates the jsonl for a session id, or "" if there is none.
func findTranscript(sessionID string) string {
	root := transcriptRoot()
	if root == "" || sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// readUsage parses a session transcript into per-model token usage.
//
// Streaming writes several partial records under one message id; the last one
// carries the settled numbers, so entries are deduplicated by id with
// last-write-wins. Summing them instead would multiply-count every message.
func readUsage(sessionID string) (modelUsage, bool) {
	path := findTranscript(sessionID)
	if path == "" {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	byID := map[string]transcriptEntry{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // transcript lines can be large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e transcriptEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // a line we can't read is a line we skip, not a failure
		}
		if e.Type != "assistant" || e.Message.ID == "" {
			continue
		}
		if _, seen := byID[e.Message.ID]; !seen {
			order = append(order, e.Message.ID)
		}
		byID[e.Message.ID] = e
	}
	if len(byID) == 0 {
		return nil, false
	}
	mu := modelUsage{}
	for _, id := range order {
		e := byID[id]
		mu.add(e.Message.Model, model.TokenUsage{
			Input:      e.Message.Usage.InputTokens,
			Output:     e.Message.Usage.OutputTokens,
			CacheWrite: e.Message.Usage.CacheCreationInputTokens,
			CacheRead:  e.Message.Usage.CacheReadInputTokens,
		})
	}
	return mu, true
}

// usageDelta reports the usage accumulated in a session since a previous
// snapshot, so a multi-turn session can be costed turn by turn without
// double-counting earlier turns.
func usageDelta(sessionID string, prev modelUsage) (delta modelUsage, now modelUsage, ok bool) {
	cur, ok := readUsage(sessionID)
	if !ok {
		return nil, prev, false
	}
	delta = modelUsage{}
	for id, u := range cur {
		d := *u
		if p := prev[id]; p != nil {
			d.Input -= p.Input
			d.Output -= p.Output
			d.CacheWrite -= p.CacheWrite
			d.CacheRead -= p.CacheRead
		}
		if d.Empty() {
			continue
		}
		delta.add(id, d)
	}
	return delta, cur, true
}
