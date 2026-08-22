package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The exact shape claude-code-acp relayed for a real 529 (run
// run_20260821t03080248aeed49, task M3).
const real529 = `acp prompt: {"code":-32603,"message":"Internal error: API Error: 529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"},\"request_id\":\"req_011CeGjVnZo7sDUQSUAM3LrZ\"}"}`

func TestClassifyOverloaded(t *testing.T) {
	err := classifyPromptError(errors.New(real529), "No onPostToolUseHook found for tool use ID: toolu_01X\nError handling request {")
	if !IsTransient(err) {
		t.Fatalf("529 overloaded must be transient: %v", err)
	}
	want := "API error 529 overloaded_error: Overloaded (request_id req_011CeGjVnZo7sDUQSUAM3LrZ)"
	if got := CleanError(err); got != want {
		t.Fatalf("clean message:\n got %q\nwant %q", got, want)
	}
	// The raw error stays reachable for logs.
	var pe *PromptError
	if !errors.As(err, &pe) || pe.Raw == nil || pe.Raw.Error() != real529 {
		t.Fatalf("raw error not preserved: %+v", pe)
	}
}

func TestClassifyNonTransient(t *testing.T) {
	cases := []struct {
		err       error
		transient bool
	}{
		{errors.New(`API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"too long"}}`), false},
		{errors.New(`API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"bad key"}}`), false},
		{errors.New("API Error: 503 Service Unavailable"), true},
		{errors.New("API Error: 429 rate limited"), true},
		{errors.New("fetch failed: ECONNRESET"), true},
		{errors.New("acp prompt stopped: refusal"), false},
		{context.Canceled, false},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), false},
	}
	for _, c := range cases {
		if got := IsTransient(c.err); got != c.transient {
			t.Errorf("%v: transient=%v, want %v", c.err, got, c.transient)
		}
	}
}

func TestClassifyUnknownKeepsStderr(t *testing.T) {
	err := classifyPromptError(errors.New("acp prompt: connection closed"), "line one\nadapter exited with code 1")
	if IsTransient(err) {
		t.Fatalf("unknown failure must not be transient")
	}
	got := CleanError(err)
	if got != "acp prompt: connection closed (stderr: adapter exited with code 1)" {
		t.Fatalf("got %q", got)
	}
}
