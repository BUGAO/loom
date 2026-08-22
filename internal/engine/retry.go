package engine

import (
	"context"
	"fmt"
	"time"

	"loom/internal/llm"
)

// Transient-failure retry for one prompt turn.
//
// A 529 "overloaded" twenty minutes into a task used to fail the task
// outright, and the failure arrived as kind "unspecified" — which the rework
// router reads as "not the worker's fault, not reworkable either", leaving the
// coordinator to re-plan around weather. The session is usually still alive
// when that happens; what it needs is a pause and the same prompt again.

// transientBackoff is the wait before each retry; len() is the retry budget.
// A package variable so tests can shrink it.
var transientBackoff = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}

// retryNote frames the resent prompt. The session may or may not remember
// the interrupted turn (the adapter's failure mode decides), so the note
// covers both: continue from memory if there is one, else start from the
// instruction that follows — which is the original prompt, verbatim.
const retryNote = "SYSTEM: your previous turn on this was interrupted by a transient upstream error (%s); " +
	"this is retry %d of %d after a %s pause. If you remember your progress, continue from it — files you " +
	"already changed are still on disk; do not redo finished steps. If you have no memory of it, start from " +
	"the instruction below.\n\n"

// promptRetry sends prompt through send (one session turn), resending after
// a pause when the failure is transient. Every attempt's partial Result (usage, transcript) is handed
// to record so nothing spent goes unaccounted; the last attempt's result and
// error are returned. onRetry, if set, is told about each pause (for the
// event log). A canceled ctx ends the retries immediately.
func promptRetry(ctx context.Context, send func(ctx context.Context, prompt string) (*llm.Result, error), prompt string,
	record func(*llm.Result), onRetry func(attempt int, wait time.Duration, err error)) (*llm.Result, error) {

	res, err := send(ctx, prompt)
	for attempt := 1; err != nil && ctx.Err() == nil && llm.IsTransient(err) && attempt <= len(transientBackoff); attempt++ {
		if res != nil && record != nil {
			record(res)
		}
		wait := transientBackoff[attempt-1]
		if onRetry != nil {
			onRetry(attempt, wait, err)
		}
		select {
		case <-ctx.Done():
			return res, err
		case <-time.After(wait):
		}
		retryPrompt := fmt.Sprintf(retryNote, llm.CleanError(err), attempt, len(transientBackoff), wait) + prompt
		res, err = send(ctx, retryPrompt)
	}
	return res, err
}
