package model

import "testing"

// Runtimes report dated model ids; the catalog holds families. Pricing must
// bridge that gap — the poe2-build-advisor run booked $0 for every session
// that reported a dated id.
func TestCostOfDatedModelID(t *testing.T) {
	u := TokenUsage{Input: 1_000_000}
	family := CostOf("claude-sonnet-5", u)
	if family <= 0 {
		t.Fatalf("catalog family must price, got %v", family)
	}
	if got := CostOf("claude-sonnet-5-20250929", u); got != family {
		t.Fatalf("dated id should price like its family: %v vs %v", got, family)
	}
	if got := CostOf("claude-opus-5-1", u); got != CostOf("claude-opus-5", u) {
		t.Fatalf("dated opus should price like opus, got %v", got)
	}
	// Unknown stays 0 — a made-up number is worse than none.
	if got := CostOf("gpt-9", u); got != 0 {
		t.Fatalf("unknown model must cost 0, got %v", got)
	}
	// A bare prefix without the dash separator is not a family match.
	if got := CostOf("claude-sonnet-5x", u); got != 0 {
		t.Fatalf("non-dated suffix must not match, got %v", got)
	}
}

func TestCoordinatorOnlyModelDatedID(t *testing.T) {
	for _, id := range []string{"fable", "claude-fable-5", "claude-fable-5-20260101"} {
		if !CoordinatorOnlyModel(id) {
			t.Fatalf("%s should be coordinator-only", id)
		}
	}
	for _, id := range []string{"", "opus", "claude-opus-5-20251101", "gpt-9"} {
		if CoordinatorOnlyModel(id) {
			t.Fatalf("%s should not be coordinator-only", id)
		}
	}
}
