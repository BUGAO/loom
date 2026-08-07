package engine

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"loom/internal/hub"
	"loom/internal/llm"
	"loom/internal/model"
	"loom/internal/store"
)

// Dynamic-mode tests run the mock backend as a real MCP client against a real
// hub served over HTTP. Everything except the model's judgment is the
// production path: transport, credentials, policy engine, ledger.

func dynSetup(t *testing.T, budget model.BudgetConfig) (*Engine, *store.Store, *model.Workflow) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"researcher", "builder"} {
		// The mock workers genuinely write artifacts, so the pool honestly
		// declares write capability — contract feasibility checks demand it.
		if err := st.SaveAgent(&model.Agent{Name: name, Description: "test agent", Model: "claude-haiku-4-5", Tools: "Read,Write"}); err != nil {
			t.Fatal(err)
		}
	}
	h := hub.New("http://placeholder", st.ListAgents)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	// Point the hub at its own live address so agents get a dialable URL.
	h = hub.New(srv.URL, st.ListAgents)
	srv.Config.Handler = h.Handler()

	eng := New(st, map[string]llm.Backend{"mock": &llm.Mock{NodeDelay: 10 * time.Millisecond}}, NewBroker(), h)
	eng.SetOutputRoot(t.TempDir())
	wf := &model.Workflow{
		Name: "dynamic test", Mode: model.ModeDynamic,
		Coordinator: &model.CoordinatorConfig{Model: "claude-haiku-4-5"},
		Budget:      &budget,
	}
	if err := st.SaveWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	return eng, st, wf
}

func noApproval() model.BudgetConfig {
	b := model.DefaultBudget()
	b.ApprovalPolicy = model.ApprovalNone
	b.TaskTimeoutSec = 30
	b.RunTimeoutSec = 60
	return b
}

// E2E-1: the coordinator decomposes, delegates in parallel, collects the
// workers' envelopes, and delivers a verdict.
func TestDynamicDelegateAndReport(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, err := eng.StartRun(wf, "build the thing", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("want succeeded, got %s (%s)", final.Status, final.Error)
	}
	if final.Mode != model.ModeDynamic {
		t.Fatalf("run mode should be dynamic, got %q", final.Mode)
	}
	if len(final.Tasks) != 2 {
		t.Fatalf("want 2 delegated tasks, got %d", len(final.Tasks))
	}
	for id, task := range final.Tasks {
		if task.Status != model.TaskCompleted {
			t.Errorf("task %s: status %s (%s)", id, task.Status, task.Error)
		}
		if task.Summary == "" {
			t.Errorf("task %s completed without a summary", id)
		}
		if task.CreatedBy != hub.RoleCoordinator {
			t.Errorf("task %s createdBy = %q, want coordinator", id, task.CreatedBy)
		}
		if task.Depth != 1 {
			t.Errorf("task %s depth = %d, want 1", id, task.Depth)
		}
		// The worker's report_progress call must be on the audit trail.
		found := false
		for _, m := range task.Messages {
			if m.Role == model.MsgProgress {
				found = true
			}
		}
		if !found {
			t.Errorf("task %s has no progress message in its ledger", id)
		}
	}
	if final.Coordinator == nil || final.Coordinator.Status != "done" {
		t.Fatalf("coordinator state not finalized: %+v", final.Coordinator)
	}
}

// E2E-2: an ambiguous task makes the worker ask upward; the task parks in
// input-required and resumes on the coordinator's answer.
func TestDynamicAskCoordinator(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, err := eng.StartRun(wf, "do it, simulate-ask", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("want succeeded, got %s (%s)", final.Status, final.Error)
	}
	asked, answered := false, false
	for _, task := range final.Tasks {
		for _, m := range task.Messages {
			switch m.Role {
			case model.MsgQuestion:
				asked = true
			case model.MsgFollowup:
				answered = true
			}
		}
	}
	if !asked {
		t.Fatal("no question was recorded: the input-required path never ran")
	}
	if !answered {
		t.Fatal("the coordinator's answer was not recorded on the task")
	}
}

// E2E-4: when the task budget is exhausted the coordinator gets a structured
// refusal and converges, rather than looping against the limit forever.
func TestDynamicBudgetConverges(t *testing.T) {
	b := noApproval()
	b.MaxTasks = 3
	b.MaxParallel = 2
	eng, st, wf := dynSetup(t, b)

	run, err := eng.StartRun(wf, "keep going, simulate-budget", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("want succeeded (converged), got %s (%s)", final.Status, final.Error)
	}
	if len(final.Tasks) != b.MaxTasks {
		t.Fatalf("task budget not enforced: %d tasks created, limit was %d", len(final.Tasks), b.MaxTasks)
	}
}

// E2E-3: peer handoff creates a child task with correct lineage and depth.
func TestDynamicPeerHandoff(t *testing.T) {
	b := noApproval()
	b.AllowPeerHandoff = true
	eng, st, wf := dynSetup(t, b)

	run, err := eng.StartRun(wf, "split it, simulate-handoff", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("want succeeded, got %s (%s)", final.Status, final.Error)
	}
	var child *model.Task
	for _, task := range final.Tasks {
		if strings.HasPrefix(task.CreatedBy, "task_") {
			child = task
		}
	}
	if child == nil {
		t.Fatalf("no handoff task found; tasks: %d", len(final.Tasks))
	}
	if child.Depth != 2 {
		t.Fatalf("handoff task depth = %d, want 2 (parent was 1)", child.Depth)
	}
	if parent := final.Tasks[child.CreatedBy]; parent == nil {
		t.Fatalf("handoff task's parent %q is not in the ledger", child.CreatedBy)
	}
}

// With peer handoff disabled the tool must not exist at all — the guardrail is
// the absence of the capability, not a prompt asking the agent not to use it.
func TestDynamicHandoffRejectedWhenDisabled(t *testing.T) {
	b := noApproval() // AllowPeerHandoff defaults to false
	eng, st, wf := dynSetup(t, b)

	run, err := eng.StartRun(wf, "split it, simulate-handoff", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if len(final.Tasks) != 2 {
		t.Fatalf("handoff should not have created a task: got %d tasks, want 2", len(final.Tasks))
	}
	for _, task := range final.Tasks {
		if strings.HasPrefix(task.CreatedBy, "task_") {
			t.Fatalf("task %s was created by a handoff that should have been impossible", task.ID)
		}
	}
}

// The approval gate blocks the first delegation until a human releases it.
func TestDynamicApprovalGate(t *testing.T) {
	b := noApproval()
	b.ApprovalPolicy = model.ApprovalInitial
	eng, st, wf := dynSetup(t, b)

	run, err := eng.StartRun(wf, "build the thing", true)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, _ := st.LoadRun(run.ID)
		if r != nil && r.Status == model.RunAwaitingApproval {
			if r.Proposal == nil || len(r.Proposal.Tasks) == 0 {
				t.Fatal("parked for approval without a proposal to review")
			}
			if len(r.Tasks) != 0 {
				t.Fatalf("tasks were created before approval: %d", len(r.Tasks))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached awaiting_approval (status %v)", r.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := eng.Approve(run.ID); err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("after approval want succeeded, got %s (%s)", final.Status, final.Error)
	}
	if len(final.Tasks) == 0 {
		t.Fatal("approval released but nothing was delegated")
	}
}

// Rejecting the plan ends the run without any task ever running.
func TestDynamicApprovalRejected(t *testing.T) {
	b := noApproval()
	b.ApprovalPolicy = model.ApprovalInitial
	eng, st, wf := dynSetup(t, b)

	run, err := eng.StartRun(wf, "build the thing", true)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, _ := st.LoadRun(run.ID)
		if r != nil && r.Status == model.RunAwaitingApproval {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never reached awaiting_approval")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := eng.Reject(run.ID, "not what I asked for"); err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunFailed {
		t.Fatalf("rejected plan should fail the run, got %s", final.Status)
	}
	if len(final.Tasks) != 0 {
		t.Fatalf("rejected plan still created %d tasks", len(final.Tasks))
	}
}

// A dynamic run has no per-task retry: the coordinator that would consume the
// result is gone, so offering one would be a lie.
func TestDynamicRetryRefused(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	run, _ := eng.StartRun(wf, "build the thing", true)
	final := waitTerminal(t, st, run.ID)
	var anyTask string
	for id := range final.Tasks {
		anyTask = id
	}
	if _, err := eng.RetryNode(run.ID, anyTask); err == nil {
		t.Fatal("expected per-task retry to be refused for a dynamic run")
	}
}

// A runtime with no session support cannot host the hub toolset; starting a
// real dynamic run on it must fail loudly rather than produce an empty run.
func TestDynamicRefusesSessionlessRuntime(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	// Install a sessionless backend as the "claude" runtime and ask for a
	// real (non-dry) run: the coordinator would have no orchestration tools.
	eng.backends["claude"] = &fake{
		planFn: func(llm.Request) (*llm.Result, error) { return okNode("x"), nil },
		nodeFn: func(llm.Request) (*llm.Result, error) { return okNode("x"), nil },
	}
	if _, err := eng.StartRun(wf, "goal", false); err == nil {
		t.Fatal("expected dynamic mode to refuse a sessionless runtime")
	}
	// Dry-run on the same workflow still works: mock bypasses the runtime.
	run, err := eng.StartRun(wf, "goal", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("dry-run should succeed regardless of runtime: %s (%s)", final.Status, final.Error)
	}
}
