package llm

import (
	"os"
	"path/filepath"
	"testing"

	"loom/internal/model"
)

// The transcript parser is the one part of cost accounting that reads a format
// loom does not own, so it is tested against the shapes that format actually
// produces: repeated ids from streaming, interleaved models, junk lines.

func writeTranscript(t *testing.T, sessionID string, lines []string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "-some-encoded-path")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var body []byte
	for _, l := range lines {
		body = append(body, l...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Streaming writes several partial records per message; only the last one is
// settled. Summing them would multiply-count every message.
func TestReadUsageDeduplicatesByMessageID(t *testing.T) {
	writeTranscript(t, "sess-1", []string{
		`{"type":"user","message":{"content":"hi"}}`,
		`{"type":"assistant","message":{"id":"msg_a","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"assistant","message":{"id":"msg_a","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":40,"cache_creation_input_tokens":100,"cache_read_input_tokens":7}}}`,
		`not json at all`,
		`{"type":"assistant","message":{"id":"msg_b","model":"claude-haiku-4-5","usage":{"input_tokens":3,"output_tokens":2}}}`,
	})
	mu, ok := readUsage("sess-1")
	if !ok {
		t.Fatal("transcript should have been found and parsed")
	}
	son := mu["claude-sonnet-5"]
	if son == nil {
		t.Fatal("no sonnet usage")
	}
	want := model.TokenUsage{Input: 10, Output: 40, CacheWrite: 100, CacheRead: 7}
	if *son != want {
		t.Fatalf("last record should win: got %+v, want %+v", *son, want)
	}
	if hk := mu["claude-haiku-4-5"]; hk == nil || hk.Output != 2 {
		t.Fatalf("second model not accounted: %+v", hk)
	}
	// Per-model pricing, not one blended rate.
	_, cost := mu.total()
	expect := model.CostOf("claude-sonnet-5", want) +
		model.CostOf("claude-haiku-4-5", model.TokenUsage{Input: 3, Output: 2})
	if diff := cost - expect; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost = %v, want %v", cost, expect)
	}
}

// A missing transcript is a normal outcome, not a failure: the caller records
// the unit as cost-unavailable and carries on.
func TestReadUsageMissingTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, ok := readUsage("nope"); ok {
		t.Fatal("a missing transcript must report not-ok, not fabricate usage")
	}
}

func TestReadUsageRejectsPathTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, ok := readUsage("../../etc/passwd"); ok {
		t.Fatal("a session id containing path separators must be refused")
	}
}

// Multi-turn sessions are costed per turn by diffing against the previous
// snapshot, so turn two is not charged for turn one.
func TestUsageDeltaPerTurn(t *testing.T) {
	writeTranscript(t, "sess-2", []string{
		`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":50}}}`,
	})
	d1, snap, ok := usageDelta("sess-2", nil)
	if !ok {
		t.Fatal("first read failed")
	}
	if got := d1["claude-sonnet-5"]; got.Output != 50 {
		t.Fatalf("turn 1 delta = %+v", got)
	}

	writeTranscript(t, "sess-2", []string{
		`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"assistant","message":{"id":"m2","model":"claude-sonnet-5","usage":{"input_tokens":200,"output_tokens":30}}}`,
	})
	d2, _, ok := usageDelta("sess-2", snap)
	if !ok {
		t.Fatal("second read failed")
	}
	got := d2["claude-sonnet-5"]
	if got == nil || got.Input != 200 || got.Output != 30 {
		t.Fatalf("turn 2 delta should exclude turn 1: %+v", got)
	}
}
