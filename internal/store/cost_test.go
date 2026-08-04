package store

import (
	"testing"

	"loom/internal/model"
)

func TestCostPricing(t *testing.T) {
	// Sonnet 5 is $3/$15 per Mtok; cache write 1.25×input, cache read 0.1×input.
	u := model.TokenUsage{Input: 1_000_000, Output: 1_000_000, CacheWrite: 1_000_000, CacheRead: 1_000_000}
	got := model.CostOf("claude-sonnet-5", u)
	want := 3.0 + 15.0 + 3.0*1.25 + 3.0*0.10
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %.6f, want %.6f", got, want)
	}
}

// An unknown model prices to zero rather than to a guess: a fabricated cost is
// worse than a missing one.
func TestCostUnknownModelIsZero(t *testing.T) {
	if got := model.CostOf("some-future-model", model.TokenUsage{Input: 1e6, Output: 1e6}); got != 0 {
		t.Fatalf("unknown model should cost 0, got %v", got)
	}
}

func TestCostLedgerAggregates(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries := []CostEntry{
		{WorkflowID: "wf1", RunID: "r1", Unit: "node", UnitID: "n1", Agent: "alpha",
			Model: "claude-sonnet-5", Usage: model.TokenUsage{Input: 1000, Output: 500}, CostUSD: 0.01},
		{WorkflowID: "wf1", RunID: "r1", Unit: "node", UnitID: "n2", Agent: "beta",
			Model: "claude-haiku-4-5", Usage: model.TokenUsage{Input: 2000, Output: 100}, CostUSD: 0.002},
		{WorkflowID: "wf2", RunID: "r2", Unit: "task", UnitID: "t1", Agent: "alpha",
			Model: "claude-sonnet-5", Usage: model.TokenUsage{Input: 500, Output: 250}, CostUSD: 0.005},
	}
	for _, e := range entries {
		if err := st.AppendCost(e); err != nil {
			t.Fatal(err)
		}
	}
	// A zero-cost, zero-usage unit (a mock run) must not pollute the ledger.
	if err := st.AppendCost(CostEntry{WorkflowID: "wf3", Agent: "mock"}); err != nil {
		t.Fatal(err)
	}

	sum, err := st.CostSummaryData()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Entries != 3 {
		t.Fatalf("want 3 ledger entries, got %d", sum.Entries)
	}
	if diff := sum.TotalUSD - 0.017; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("total = %v, want 0.017", sum.TotalUSD)
	}
	find := func(bs []CostBucket, key string) CostBucket {
		for _, b := range bs {
			if b.Key == key {
				return b
			}
		}
		t.Fatalf("no bucket %q in %v", key, bs)
		return CostBucket{}
	}
	// The same agent across two workflows rolls up into one agent bucket —
	// that cross-workflow view is the reason the ledger exists at all.
	alpha := find(sum.Agents, "alpha")
	if alpha.Units != 2 {
		t.Fatalf("alpha units = %d, want 2", alpha.Units)
	}
	if alpha.Usage.Input != 1500 {
		t.Fatalf("alpha input tokens = %d, want 1500", alpha.Usage.Input)
	}
	if wf1 := find(sum.Workflows, "wf1"); wf1.Units != 2 {
		t.Fatalf("wf1 units = %d, want 2", wf1.Units)
	}
	if son := find(sum.Models, "claude-sonnet-5"); son.Units != 2 {
		t.Fatalf("sonnet units = %d, want 2", son.Units)
	}
	// Buckets are ordered most expensive first, which is what a reader wants.
	if len(sum.Agents) > 1 && sum.Agents[0].CostUSD < sum.Agents[1].CostUSD {
		t.Fatal("buckets should be sorted by cost descending")
	}
}

func TestCostSummaryOnEmptyLedger(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sum, err := st.CostSummaryData()
	if err != nil {
		t.Fatalf("an absent ledger is not an error: %v", err)
	}
	if sum.Entries != 0 || sum.Workflows == nil {
		t.Fatalf("empty summary should have empty, non-nil groupings: %+v", sum)
	}
}
