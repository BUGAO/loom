package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"loom/internal/llm"
)

func fastBackoff(t *testing.T) {
	t.Helper()
	old := transientBackoff
	transientBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { transientBackoff = old })
}

const overloaded = `acp prompt: {"code":-32603,"message":"Internal error: API Error: 529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}"}`

// Two overloads, then success: the same session is reused, the retry prompt
// carries the original instruction, and every failed attempt is recorded.
func TestPromptRetryResendsOnTransient(t *testing.T) {
	fastBackoff(t)
	var prompts []string
	send := func(_ context.Context, p string) (*llm.Result, error) {
		prompts = append(prompts, p)
		if len(prompts) < 3 {
			return &llm.Result{Text: "partial"}, errors.New(overloaded)
		}
		return &llm.Result{Text: "ok"}, nil
	}
	recorded, retries := 0, 0
	res, err := promptRetry(context.Background(), send, "do the task", func(*llm.Result) { recorded++ },
		func(attempt int, _ time.Duration, err error) {
			retries = attempt
			if !strings.Contains(llm.CleanError(err), "API error 529 overloaded_error: Overloaded") {
				t.Errorf("retry saw an unclean error: %q", llm.CleanError(err))
			}
		})
	if err != nil || res.Text != "ok" {
		t.Fatalf("want success after retries, got %v / %+v", err, res)
	}
	if len(prompts) != 3 || retries != 2 || recorded != 2 {
		t.Fatalf("prompts=%d retries=%d recorded=%d", len(prompts), retries, recorded)
	}
	if prompts[0] != "do the task" {
		t.Fatalf("first prompt altered: %q", prompts[0])
	}
	if !strings.HasPrefix(prompts[1], "SYSTEM: your previous turn") || !strings.HasSuffix(prompts[1], "do the task") ||
		!strings.Contains(prompts[1], "retry 1 of 3") {
		t.Fatalf("retry prompt: %q", prompts[1])
	}
}

// A real failure is not retried; a transient one is given up after the budget.
func TestPromptRetryStopsWhenItShould(t *testing.T) {
	fastBackoff(t)
	calls := 0
	_, err := promptRetry(context.Background(), func(context.Context, string) (*llm.Result, error) {
		calls++
		return nil, errors.New("acp prompt stopped: refusal")
	}, "x", nil, nil)
	if err == nil || calls != 1 {
		t.Fatalf("non-transient: calls=%d err=%v", calls, err)
	}
	calls = 0
	_, err = promptRetry(context.Background(), func(context.Context, string) (*llm.Result, error) {
		calls++
		return nil, errors.New(overloaded)
	}, "x", nil, nil)
	if !llm.IsTransient(err) || calls != 1+len(transientBackoff) {
		t.Fatalf("exhausted: calls=%d err=%v", calls, err)
	}
	// A canceled context ends it at once.
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	_, err = promptRetry(ctx, func(context.Context, string) (*llm.Result, error) {
		calls++
		cancel()
		return nil, errors.New(overloaded)
	}, "x", nil, nil)
	if calls != 1 || err == nil {
		t.Fatalf("canceled: calls=%d err=%v", calls, err)
	}
}
