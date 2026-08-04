package engine

import (
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
