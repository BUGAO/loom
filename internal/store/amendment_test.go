package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loom/internal/model"
)

// The amendment lifecycle: a proposal is inert data until approved; approval
// applies the prompt through SaveAgent (so AGENTS.md regenerates), and a stale
// proposal — the agent's prompt changed since — is refused rather than
// silently overwriting the newer edit.
func TestAmendmentLifecycle(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent := &model.Agent{Name: "reviewer", Description: "reviews", Model: "sonnet",
		Tools: "Read,Write", SystemPrompt: "You review code."}
	if err := s.SaveAgent(agent); err != nil {
		t.Fatal(err)
	}

	am := &model.Amendment{Agent: "reviewer", RunID: "run_x", Rationale: "missed the same class of bug twice",
		Current: "You review code.", Proposed: "You review code. Always check error paths first."}
	if err := s.SaveAmendment(am); err != nil {
		t.Fatal(err)
	}
	if am.ID == "" || am.Status != model.AmendmentPending {
		t.Fatalf("SaveAmendment did not initialize the record: %+v", am)
	}

	// The proposal alone changed nothing.
	a, err := s.LoadAgent("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if a.SystemPrompt != "You review code." {
		t.Fatalf("a pending amendment mutated the agent: %q", a.SystemPrompt)
	}

	// Approval applies it and regenerates AGENTS.md.
	decided, err := s.DecideAmendment(am.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if decided.Status != model.AmendmentApproved || decided.DecidedAt.IsZero() {
		t.Fatalf("approval not recorded: %+v", decided)
	}
	a, _ = s.LoadAgent("reviewer")
	if !strings.Contains(a.SystemPrompt, "error paths") {
		t.Fatalf("approved prompt not applied: %q", a.SystemPrompt)
	}
	agentsMD, err := os.ReadFile(filepath.Join(s.AgentHome("reviewer"), "AGENTS.md"))
	if err != nil || !strings.Contains(string(agentsMD), "error paths") {
		t.Fatalf("AGENTS.md not regenerated on approval: %v", err)
	}

	// Deciding twice is refused.
	if _, err := s.DecideAmendment(am.ID, false); err == nil {
		t.Fatal("re-deciding a settled amendment succeeded")
	}
}

func TestAmendmentStaleIsRefused(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent := &model.Agent{Name: "writer", Description: "writes", Model: "haiku",
		SystemPrompt: "v1"}
	if err := s.SaveAgent(agent); err != nil {
		t.Fatal(err)
	}
	am := &model.Amendment{Agent: "writer", Rationale: "why", Current: "v1", Proposed: "v2"}
	if err := s.SaveAmendment(am); err != nil {
		t.Fatal(err)
	}

	// A human edits the agent after the proposal was made.
	agent.SystemPrompt = "v1-human-edited"
	if err := s.SaveAgent(agent); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DecideAmendment(am.ID, true); err == nil {
		t.Fatal("stale amendment was applied over a newer human edit")
	}
	a, _ := s.LoadAgent("writer")
	if a.SystemPrompt != "v1-human-edited" {
		t.Fatalf("stale refusal still mutated the agent: %q", a.SystemPrompt)
	}
	// Rejecting the stale proposal still works.
	if _, err := s.DecideAmendment(am.ID, false); err != nil {
		t.Fatal(err)
	}
}

// The rendered AGENTS.md contract mentions MEMORY.md only for agents that can
// actually write files with their own tools.
func TestAgentsMDMemoryContract(t *testing.T) {
	withTools := agentsMD(&model.Agent{Name: "coder", Tools: "Read,Write", SystemPrompt: "code"})
	if !strings.Contains(withTools, "MEMORY.md") {
		t.Error("file-tool agent's AGENTS.md lacks the craft-memory contract")
	}
	pure := agentsMD(&model.Agent{Name: "thinker", SystemPrompt: "think"})
	if strings.Contains(pure, "MEMORY.md") {
		t.Error("tool-less agent's AGENTS.md promises a MEMORY.md it cannot write")
	}
}
