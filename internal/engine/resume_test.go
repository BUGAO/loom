package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loom/internal/model"
)

// D3: a dynamic run interrupted by a process death resumes from the persisted
// ledger — accepted work is preserved, in-flight work is failed as blocked,
// and a fresh coordinator drives the run to a verdict. This is only possible
// because the coordinator holds no session state between rounds.
func TestDynamicResumeFromInterrupted(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())

	run, err := st.NewRun(wf, "build the thing", "mock")
	if err != nil {
		t.Fatal(err)
	}
	run.DryRun = true
	run.Mode = model.ModeDynamic
	run.Tasks = map[string]*model.Task{}

	// One task was accepted before the crash; one was mid-flight.
	done := &model.Task{
		ID: "task_done", Agent: "researcher", Title: "finished before crash",
		Status: model.TaskCompleted, Summary: "accepted result",
		Artifacts: []string{"pre.md"}, CreatedBy: "coordinator", Depth: 1,
		CreatedAt: time.Now(), EndedAt: time.Now(),
	}
	inflight := &model.Task{
		ID: "task_inflight", Agent: "researcher", Title: "was running",
		Status: model.TaskWorking, CreatedBy: "coordinator", Depth: 1,
		CreatedAt: time.Now(),
	}
	run.Tasks[done.ID] = done
	run.Tasks[inflight.ID] = inflight
	run.TaskOrder = []string{done.ID, inflight.ID}
	run.Coordinator = &model.CoordinatorState{Agent: "(coordinator)", Model: "claude-haiku-4-5", Rounds: 3}
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	// The previous process died; recovery marks the run interrupted.
	eng.RecoverInterrupted()
	loaded, err := st.LoadRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != model.RunInterrupted {
		t.Fatalf("recovery should mark the run interrupted, got %s", loaded.Status)
	}
	if got := loaded.Tasks["task_inflight"]; got.Status != model.TaskFailed || got.FailureKind != model.FailBlocked {
		t.Fatalf("in-flight task should be failed as blocked, got %s/%s", got.Status, got.FailureKind)
	}

	if _, err := eng.ResumeRun(run.ID); err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("resumed run should reach a verdict, got %s (%s)", final.Status, final.Error)
	}
	// The accepted result survived the restart untouched.
	if got := final.Tasks["task_done"]; got.Status != model.TaskCompleted || got.Summary != "accepted result" {
		t.Fatalf("accepted work was not preserved across resume: %+v", got)
	}
	// The resumed coordinator continued the round count rather than resetting.
	if final.Coordinator.Rounds <= 3 {
		t.Fatalf("resumed coordinator should continue past round 3, got %d", final.Coordinator.Rounds)
	}
}

// B2/B3 end to end: a worker that claims success without producing the
// contracted artifact is failed by the engine's acceptance run — its own
// envelope never had the power to pass it.
func TestDynamicAcceptanceRejectsFalseClaim(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, err := eng.StartRun(wf, "build the thing, simulate-reject", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunFailed {
		t.Fatalf("a run whose worker lied should fail, got %s", final.Status)
	}
	var caught *model.Task
	for _, task := range final.Tasks {
		if task.Status == model.TaskFailed {
			caught = task
		}
	}
	if caught == nil {
		t.Fatal("no task was failed by the acceptance run")
	}
	if caught.FailureKind != model.FailBlocked {
		t.Fatalf("acceptance failure should route as blocked, got %q", caught.FailureKind)
	}
	if len(caught.AcceptanceResults) == 0 || caught.AcceptanceResults[0].Passed {
		t.Fatalf("the failed check must be on the record: %+v", caught.AcceptanceResults)
	}
	if !strings.Contains(caught.Error, "acceptance") {
		t.Fatalf("the error should name the acceptance contract, got %q", caught.Error)
	}
}

// The conversational surface: the goal opens the chat, and every coordinator
// round leaves a reply the user can read.
func TestChatCarriesGoalAndReplies(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, err := eng.StartRun(wf, "build the thing", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("setup run failed: %s (%s)", final.Status, final.Error)
	}
	if len(final.Chat) < 2 {
		t.Fatalf("chat should hold the goal and at least one reply, got %d messages", len(final.Chat))
	}
	if final.Chat[0].From != "user" || final.Chat[0].Text != "build the thing" {
		t.Fatalf("the goal must open the conversation: %+v", final.Chat[0])
	}
	sawReply := false
	for _, m := range final.Chat {
		if m.From == "coordinator" && m.Text != "" {
			sawReply = true
		}
	}
	if !sawReply {
		t.Fatal("no coordinator reply landed in the chat")
	}
	// A terminal run refuses further chat, pointing at the workflow entry.
	if err := eng.ChatToRun(run.ID, "再改一下"); err == nil {
		t.Fatal("chat to a finished run should be refused")
	}
}

// A session IS a run: a message to a finished session reopens it — same
// ledger, same notes, same conversation — instead of starting a new run.
func TestReopenFinishedSessionContinues(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, err := eng.StartRun(wf, "build the thing", true)
	if err != nil {
		t.Fatal(err)
	}
	first := waitTerminal(t, st, run.ID)
	if first.Status != model.RunSucceeded {
		t.Fatalf("setup run failed: %s (%s)", first.Status, first.Error)
	}
	tasksBefore := len(first.Tasks)
	roundsBefore := first.Coordinator.Rounds

	if _, err := eng.ReopenRun(run.ID, "在同一会话里再推进一步"); err != nil {
		t.Fatal(err)
	}
	second := waitTerminal(t, st, run.ID)
	if second.ID != run.ID {
		t.Fatal("reopening must not mint a new run")
	}
	if second.Status != model.RunSucceeded {
		t.Fatalf("reopened session should reach a verdict again, got %s (%s)", second.Status, second.Error)
	}
	if len(second.Tasks) <= tasksBefore {
		t.Fatalf("the reopened session should have delegated more work on the same ledger (%d → %d tasks)",
			tasksBefore, len(second.Tasks))
	}
	if second.Coordinator.Rounds <= roundsBefore {
		t.Fatalf("rounds should continue, not reset (%d → %d)", roundsBefore, second.Coordinator.Rounds)
	}
	found := false
	for _, m := range second.Chat {
		if m.From == "user" && m.Text == "在同一会话里再推进一步" {
			found = true
		}
	}
	if !found {
		t.Fatal("the reopening message must be part of the session's chat")
	}
	if second.Chat[0].Text != "build the thing" {
		t.Fatal("the original conversation must be preserved")
	}
}

func TestReopenRefusedWhileActive(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, err := eng.StartRun(wf, "build the thing", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ReopenRun(run.ID, "hello"); err == nil {
		t.Fatal("reopening an active run must be refused")
	}
	waitTerminal(t, st, run.ID)
}

// The workflow-output convention end to end: the coordinator names the
// folder, artifacts land inside it, and deleting the session leaves the
// deliverables alone.
func TestOutputDirConvention(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, err := eng.StartRun(wf, "build the thing", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("setup run failed: %s (%s)", final.Status, final.Error)
	}
	if final.OutputName != "mock-run" && !strings.HasPrefix(final.OutputName, "mock-run-") {
		t.Fatalf("coordinator-chosen output name missing: %q", final.OutputName)
	}
	artifact := filepath.Join(final.OutputDir, "mock-a.txt")
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("deliverable should be in the named output folder: %v", err)
	}

	// Deleting the session removes the run record but never the deliverables.
	if err := st.DeleteRun(run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatal("deleting the session must not delete the deliverables")
	}
}

func TestResumeRefusedForNonInterrupted(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, err := eng.StartRun(wf, "build the thing", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("setup run failed: %s (%s)", final.Status, final.Error)
	}
	if _, err := eng.ResumeRun(run.ID); err == nil {
		t.Fatal("resuming a succeeded run must be refused")
	}
}

// A3 (static mode): an independent verifier's node prompt carries upstream
// artifact paths but never upstream self-summaries.
func TestIndependentNodePromptOmitsUpstreamSummaries(t *testing.T) {
	eng, _, wf := dynSetup(t, noApproval())
	_ = wf

	run := &model.Run{
		ID: "run_prompt", Goal: "verify the widget",
		Plan: &model.Plan{Nodes: []model.PlanNode{
			{ID: "n1", Agent: "author", Title: "write it", Instruction: "write"},
			{ID: "n2", Agent: "critic", Title: "review it", Instruction: "review", DependsOn: []string{"n1"}},
		}},
		Nodes: map[string]*model.NodeState{
			"n1": {Status: model.NodeSucceeded, Summary: "AUTHOR-SELF-REPORT: everything works perfectly",
				Artifacts: []string{"widget.go"}},
			"n2": {Status: model.NodePending},
		},
	}
	nodeByID := func(id string) *model.PlanNode {
		for i := range run.Plan.Nodes {
			if run.Plan.Nodes[i].ID == id {
				return &run.Plan.Nodes[i]
			}
		}
		return nil
	}

	independent := &model.Agent{Name: "critic", Independent: true, Tools: "Read"}
	prompt := eng.buildNodePrompt(run, run.Plan.Nodes[1], independent, nodeByID)
	if strings.Contains(prompt, "AUTHOR-SELF-REPORT") {
		t.Fatal("independent verifier's prompt leaked the author's self-summary")
	}
	if !strings.Contains(prompt, "widget.go") {
		t.Fatal("independent verifier's prompt should still point at the artifacts")
	}

	regular := &model.Agent{Name: "critic", Tools: "Read"}
	prompt = eng.buildNodePrompt(run, run.Plan.Nodes[1], regular, nodeByID)
	if !strings.Contains(prompt, "AUTHOR-SELF-REPORT") {
		t.Fatal("a regular agent should still receive upstream summaries")
	}
}

// P6: the coordinator transcript survives reopens — earlier activations'
// rounds are audit trail, not scratch.
func TestReopenAppendsCoordinatorTranscript(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, err := eng.StartRun(wf, "build the thing", true)
	if err != nil {
		t.Fatal(err)
	}
	if r := waitTerminal(t, st, run.ID); r.Status != model.RunSucceeded {
		t.Fatalf("setup run failed: %s (%s)", r.Status, r.Error)
	}
	first, err := os.ReadFile(st.NodeOutputPath(run.ID, "coordinator"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "## Round 1") {
		t.Fatalf("first activation transcript missing round 1:\n%s", first)
	}

	if _, err := eng.ReopenRun(run.ID, "继续"); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, st, run.ID)
	second, err := os.ReadFile(st.NodeOutputPath(run.ID, "coordinator"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), "## Round 1") {
		t.Fatal("reopen overwrote the first activation's transcript")
	}
	if strings.Count(string(second), "## Round ") <= strings.Count(string(first), "## Round ") {
		t.Fatal("reopen did not append new rounds to the transcript")
	}
}
