package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"loom/internal/model"
)

// fakeExec records what the ledger asked to be started, without running
// anything — these tests are about the policy engine, not about agents.
type fakeExec struct {
	started  chan string
	canceled chan string
}

func newFakeExec() *fakeExec {
	return &fakeExec{started: make(chan string, 64), canceled: make(chan string, 64)}
}

func (f *fakeExec) StartTask(_ *RunSession, id string)  { f.started <- id }
func (f *fakeExec) CancelTask(_ *RunSession, id string) { f.canceled <- id }

func testSession(t *testing.T, budget model.BudgetConfig) (*RunSession, *fakeExec) {
	t.Helper()
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	exec := newFakeExec()
	run := &model.Run{
		ID: "run_test", WorkflowID: "wf_test", Mode: model.ModeDynamic,
		Tasks: map[string]*model.Task{}, CreatedAt: time.Now(),
	}
	rs := h.OpenRun(context.Background(), RunConfig{
		Run:      run,
		Workflow: &model.Workflow{Mode: model.ModeDynamic, Budget: &budget},
		Pool: []*model.Agent{
			{Name: "alpha", Description: "a"},
			{Name: "beta", Description: "b"},
			{Name: "writer", Description: "w", Tools: "Read,Write"},
			{Name: "vip", Description: "legacy agent declaring a coordinator-only model", Model: "claude-fable-5"},
		},
		Workspace: t.TempDir(),
		Exec:      exec,
		OnChange:  func(*model.Run) {},
	})
	t.Cleanup(rs.Close)
	return rs, exec
}

func openBudget() model.BudgetConfig {
	b := model.DefaultBudget()
	b.ApprovalPolicy = model.ApprovalNone
	return b
}

// okChecks is a minimal valid acceptance contract for policy tests that are
// not about acceptance itself.
func okChecks() []model.AcceptanceCheck {
	return []model.AcceptanceCheck{{Kind: model.CheckCommand, Command: "true"}}
}

var delegateN atomic.Int64

func mustDelegate(t *testing.T, rs *RunSession, agent string) *model.Task {
	t.Helper()
	task, err := rs.Delegate(DelegateRequest{
		Agent: agent, Title: fmt.Sprintf("t%d", delegateN.Add(1)), Instruction: "do the thing",
		Constraints: "none", Acceptance: okChecks(),
		CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	return task
}

func TestDelegateRejectsUnknownAgent(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	_, err := rs.Delegate(DelegateRequest{Agent: "nobody", Instruction: "x", CreatedBy: RoleCoordinator, Depth: 1})
	if err == nil {
		t.Fatal("expected unknown agent to be refused")
	}
	// The refusal must name the alternatives, or the coordinator can only guess.
	if !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("error should list available agents, got: %v", err)
	}
}

func TestDelegateModelTiering(t *testing.T) {
	rs, _ := testSession(t, openBudget())

	// A tier alias resolves to a real catalog id on the task.
	task, err := rs.Delegate(DelegateRequest{
		Agent: "alpha", Model: "haiku", Title: "tier", Instruction: "do the thing",
		Constraints: "none", Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err != nil {
		t.Fatalf("delegate with tier alias: %v", err)
	}
	if task.Model != "claude-haiku-4-5" {
		t.Fatalf("tier alias should resolve to a catalog id, got %q", task.Model)
	}

	// No model means the agent's own default.
	def := mustDelegate(t, rs, "writer")
	if def.Model != rs.pool["writer"].Model {
		t.Fatalf("empty model should inherit the agent default, got %q", def.Model)
	}

	// An unknown model is refused with tier guidance, not silently accepted.
	_, err = rs.Delegate(DelegateRequest{
		Agent: "alpha", Model: "gpt-9", Instruction: "x",
		Constraints: "none", Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "haiku") {
		t.Fatalf("unknown model should be refused with tier guidance, got: %v", err)
	}

	// The top tier is the coordinator's alone: an explicit request is refused…
	_, err = rs.Delegate(DelegateRequest{
		Agent: "alpha", Model: "fable", Instruction: "x",
		Constraints: "none", Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("coordinator-only model should be refused for workers, got: %v", err)
	}

	// …and an agent whose own default declares it gets capped at the ceiling.
	capped, err := rs.Delegate(DelegateRequest{
		Agent: "vip", Title: "cap", Instruction: "do the thing",
		Constraints: "none", Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err != nil {
		t.Fatalf("delegate to fable-default agent: %v", err)
	}
	if capped.Model != model.WorkerModelCeiling {
		t.Fatalf("inherited coordinator-only model should cap at %s, got %q", model.WorkerModelCeiling, capped.Model)
	}
}

func TestAskUserFlow(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	rs.run.Coordinator = &model.CoordinatorState{Status: "working"}

	if err := rs.AskUser("   "); err == nil {
		t.Fatal("empty question must be refused")
	}
	seq := rs.Seq()
	if err := rs.AskUser("1. Where should deliverables go? 2. Web UI or CLI?"); err != nil {
		t.Fatal(err)
	}
	if rs.Seq() == seq {
		t.Fatal("ask_user must count as a ledger transition (an ask-only round is acting, not stalling)")
	}
	if got := rs.run.Coordinator.Status; got != "awaiting_user" {
		t.Fatalf("coordinator should be awaiting_user, got %q", got)
	}
	last := rs.run.Chat[len(rs.run.Chat)-1]
	if last.From != "coordinator" || !strings.Contains(last.Text, "deliverables") {
		t.Fatalf("question should land in the chat, got %+v", last)
	}

	// The user's answer restores the coordinator and queues for the next round.
	if err := rs.UserChat("默认位置就行;做 Web UI"); err != nil {
		t.Fatal(err)
	}
	if got := rs.run.Coordinator.Status; got != "working" {
		t.Fatalf("answer should flip status back to working, got %q", got)
	}
	if msgs := rs.TakeUserChat(); len(msgs) != 1 {
		t.Fatalf("answer should be queued for the next round, got %v", msgs)
	}
}

// The poe2-build-advisor bug: name_output("topic") followed by propose_plan
// carrying output_name "topic" again claimed a SECOND directory ("topic-2") —
// colliding with the run's own first claim — and orphaned the first.
func TestOutputNameRepeatAndRename(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	root := t.TempDir()
	rs.cfg.OutputRoot = root

	if err := rs.SetOutputName("topic"); err != nil {
		t.Fatal(err)
	}
	first := rs.Workspace()
	if filepath.Base(first) != "topic" {
		t.Fatalf("first claim should be the plain name, got %s", first)
	}

	// Repeating the same topic must be a no-op, not a fresh claim.
	if err := rs.SetOutputName("topic"); err != nil {
		t.Fatal(err)
	}
	if rs.Workspace() != first {
		t.Fatalf("repeat naming moved the folder: %s → %s", first, rs.Workspace())
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("repeat naming should not claim extra directories, root has %d", len(entries))
	}

	// A genuine rename claims the new name and releases the old empty claim.
	if err := rs.SetOutputName("better-topic"); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(rs.Workspace()) != "better-topic" {
		t.Fatalf("rename should take effect, got %s", rs.Workspace())
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("old empty claim should be released, stat err: %v", err)
	}

	// A rename never deletes work: a non-empty old folder stays.
	if err := os.WriteFile(filepath.Join(rs.Workspace(), "x.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept := rs.Workspace()
	if err := rs.SetOutputName("third"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(kept, "x.md")); err != nil {
		t.Fatalf("non-empty old folder must be kept: %v", err)
	}
}

func TestSetOutputDir(t *testing.T) {
	rs, _ := testSession(t, openBudget())

	if err := rs.SetOutputDir("relative/path"); err == nil {
		t.Fatal("relative path must be refused")
	}
	if err := rs.SetOutputDir("/"); err == nil {
		t.Fatal("filesystem root must be refused")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if err := rs.SetOutputDir(home); err == nil {
			t.Fatal("home itself must be refused")
		}
	}

	dir := filepath.Join(t.TempDir(), "my-deliverables")
	if err := rs.SetOutputDir(dir); err != nil {
		t.Fatal(err)
	}
	if rs.Workspace() != dir {
		t.Fatalf("workspace should be the user's dir, got %s", rs.Workspace())
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("user dir should exist: %v", err)
	}

	// The first dispatch freezes the folder; moving it afterwards is refused.
	mustDelegate(t, rs, "alpha")
	if err := rs.SetOutputDir(filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Fatal("moving the output dir after dispatch must be refused")
	}
}

func TestWriteArtifact(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha") // alpha has no file tools of its own
	rs.TaskStarted(task.ID)

	// Escapes are refused regardless of anything else.
	if err := rs.WriteArtifact(task.ID, "../oops.md", "x", false); err == nil {
		t.Fatal("path escape must be refused")
	}

	if err := rs.WriteArtifact(task.ID, "notes/findings.md", "# Findings\n\npart one\n", false); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := rs.WriteArtifact(task.ID, "notes/findings.md", "part two\n", true); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(rs.Workspace(), "notes", "findings.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# Findings\n\npart one\npart two\n" {
		t.Fatalf("unexpected content: %q", data)
	}

	// The delivery is on the task record immediately (once, not per write)…
	v, _ := rs.View(task.ID)
	if len(v.Artifacts) != 1 || v.Artifacts[0] != "notes/findings.md" {
		t.Fatalf("delivered file should be recorded live, got %v", v.Artifacts)
	}
	// …and completion unions the envelope's list instead of overwriting it.
	rs.CompleteTask(task.ID, "done", []string{"extra.md"}, nil)
	v, _ = rs.View(task.ID)
	if len(v.Artifacts) != 2 {
		t.Fatalf("completion must keep hub-written artifacts, got %v", v.Artifacts)
	}

	// The delivery window closes with the task.
	if err := rs.WriteArtifact(task.ID, "late.md", "x", false); err == nil {
		t.Fatal("write after terminal state must be refused")
	}
}

func TestDelegateRejectsEmptyInstruction(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "   ", CreatedBy: RoleCoordinator, Depth: 1}); err == nil {
		t.Fatal("expected an empty instruction to be refused")
	}
}

func TestTaskCountBudget(t *testing.T) {
	b := openBudget()
	b.MaxTasks = 2
	rs, _ := testSession(t, b)

	mustDelegate(t, rs, "alpha")
	mustDelegate(t, rs, "alpha")
	_, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", CreatedBy: RoleCoordinator, Depth: 1})
	if err == nil {
		t.Fatal("third delegation should have been refused")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("refusal should explain the budget, got: %v", err)
	}
}

func TestDepthBudget(t *testing.T) {
	b := openBudget()
	b.MaxDelegationDepth = 2
	rs, _ := testSession(t, b)

	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: "task_parent", Depth: 2}); err != nil {
		t.Fatalf("depth 2 should be allowed with a limit of 2: %v", err)
	}
	_, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", CreatedBy: "task_parent", Depth: 3})
	if err == nil {
		t.Fatal("depth 3 should have been refused with a limit of 2")
	}
}

func TestParallelismQueues(t *testing.T) {
	b := openBudget()
	b.MaxParallel = 2
	rs, exec := testSession(t, b)

	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, mustDelegate(t, rs, "alpha").ID)
	}
	// Only MaxParallel tasks may be handed to the executor before any finish.
	started := drain(exec.started)
	if len(started) != 2 {
		t.Fatalf("want 2 tasks started under a parallelism of 2, got %d", len(started))
	}
	for _, id := range started {
		if err := rs.TaskStarted(id); err != nil {
			t.Fatal(err)
		}
	}
	rs.CompleteTask(started[0], "done", nil, nil)
	if got := drain(exec.started); len(got) != 1 {
		t.Fatalf("completing one task should release exactly one queued task, got %d", len(got))
	}
	_ = ids
}

// A task blocked on a question still owns a live session, so it must keep
// occupying a parallelism slot (docs/DECISIONS-v2.md D10).
func TestInputRequiredOccupiesSlot(t *testing.T) {
	b := openBudget()
	b.MaxParallel = 1
	rs, exec := testSession(t, b)

	a := mustDelegate(t, rs, "alpha")
	mustDelegate(t, rs, "beta")
	if got := drain(exec.started); len(got) != 1 {
		t.Fatalf("want 1 started, got %d", len(got))
	}
	if err := rs.TaskStarted(a.ID); err != nil {
		t.Fatal(err)
	}
	go rs.Ask(context.Background(), a.ID, "which one?")
	waitFor(t, func() bool {
		v, _ := rs.View(a.ID)
		return v.Status == model.TaskInputRequired
	}, "task never reached input-required")

	rs.schedule()
	if got := drain(exec.started); len(got) != 0 {
		t.Fatalf("an input-required task must still hold its slot; %d task(s) were started", len(got))
	}
}

func TestIllegalTransitionRejected(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")

	if err := rs.TaskStarted(task.ID); err != nil {
		t.Fatal(err)
	}
	rs.CompleteTask(task.ID, "done", nil, nil)

	// submitted → working was legal; completed → working is not.
	if err := rs.TaskStarted(task.ID); err == nil {
		t.Fatal("restarting a completed task should be rejected")
	}
	v, _ := rs.View(task.ID)
	if v.Status != model.TaskCompleted {
		t.Fatalf("task status was corrupted by a rejected transition: %s", v.Status)
	}
}

func TestCompletedTaskIsAbsorbing(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)
	rs.CompleteTask(task.ID, "first", nil, nil)
	rs.CompleteTask(task.ID, "second", nil, nil)

	v, _ := rs.View(task.ID)
	if v.Summary != "first" {
		t.Fatalf("a second completion overwrote the first: %q", v.Summary)
	}
}

func TestTurnBudgetBlocksSteering(t *testing.T) {
	b := openBudget()
	b.MaxTurnsPerTask = 2
	rs, _ := testSession(t, b)
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	rs.TurnSpent(task.ID)
	if _, err := rs.Send(task.ID, RoleCoordinator, "steer"); err != nil {
		t.Fatalf("first steering message should be allowed: %v", err)
	}
	rs.TurnSpent(task.ID)
	_, err := rs.Send(task.ID, RoleCoordinator, "steer again")
	if err == nil {
		t.Fatal("steering past the turn budget should be refused")
	}
	if !strings.Contains(err.Error(), "turns") {
		t.Fatalf("refusal should explain the turn budget, got: %v", err)
	}
}

func TestSendToTerminalTaskRefused(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)
	rs.CompleteTask(task.ID, "done", nil, nil)

	if _, err := rs.Send(task.ID, RoleCoordinator, "hello"); err == nil {
		t.Fatal("messaging a completed task should be refused")
	}
}

// Steering a busy worker is queued for its next turn; answering a parked one
// lands immediately. The distinction is reported so a coordinator can tell
// which happened (docs/DECISIONS-v2.md D4).
func TestSendDeliverySemantics(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	res, err := rs.Send(task.ID, RoleCoordinator, "steer")
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivery != "queued_next_turn" {
		t.Fatalf("working task: want queued_next_turn, got %q", res.Delivery)
	}
	if got := rs.TakeInbox(task.ID); len(got) != 1 || got[0] != "steer" {
		t.Fatalf("inbox should hold the queued message, got %v", got)
	}
	if got := rs.TakeInbox(task.ID); len(got) != 0 {
		t.Fatal("inbox should be drained after being taken")
	}

	answered := make(chan string, 1)
	go func() {
		a, _ := rs.Ask(context.Background(), task.ID, "which?")
		answered <- a
	}()
	waitFor(t, func() bool {
		v, _ := rs.View(task.ID)
		return v.Status == model.TaskInputRequired
	}, "never parked in input-required")

	res, err = rs.Send(task.ID, RoleCoordinator, "use the first")
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivery != "immediate" {
		t.Fatalf("parked task: want immediate, got %q", res.Delivery)
	}
	select {
	case a := <-answered:
		if a != "use the first" {
			t.Fatalf("worker got the wrong answer: %q", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker was never unblocked")
	}
	v, _ := rs.View(task.ID)
	if v.Status != model.TaskWorking {
		t.Fatalf("answered task should return to working, got %s", v.Status)
	}
}

func TestAwaitReturnsPartialOnTimeout(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	start := time.Now()
	res, err := rs.Await(context.Background(), []string{task.ID}, "all", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatal("await should report a timeout rather than an error")
	}
	if len(res.Tasks) != 1 || res.Tasks[0].Status != model.TaskWorking {
		t.Fatalf("timed-out await must still return the live state, got %+v", res.Tasks)
	}
	if res.Hint == "" {
		t.Fatal("a timed-out await must tell the coordinator this is normal")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("await honored neither the requested timeout nor the cap: %s", d)
	}
}

func TestAwaitWakesOnCompletion(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	done := make(chan *AwaitResult, 1)
	go func() {
		res, _ := rs.Await(context.Background(), []string{task.ID}, "all", 30)
		done <- res
	}()
	time.Sleep(50 * time.Millisecond)
	rs.CompleteTask(task.ID, "finished", []string{"out.md"}, nil)

	select {
	case res := <-done:
		if res.TimedOut {
			t.Fatal("await should have woken on the transition, not timed out")
		}
		if res.Tasks[0].Summary != "finished" {
			t.Fatalf("await returned a stale view: %+v", res.Tasks[0])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("await did not wake on the task's completion")
	}
}

func TestAwaitSurfacesQuestion(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)
	go rs.Ask(context.Background(), task.ID, "which encoding?")

	res, err := rs.Await(context.Background(), []string{task.ID}, "any", 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tasks[0].Status != model.TaskInputRequired {
		t.Fatalf("want input-required, got %s", res.Tasks[0].Status)
	}
	if res.Tasks[0].Question != "which encoding?" {
		t.Fatalf("await must carry the question itself, got %q", res.Tasks[0].Question)
	}
}

func TestRelatedLineage(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	parent := mustDelegate(t, rs, "alpha")
	sibling := mustDelegate(t, rs, "beta")
	child, err := rs.Delegate(DelegateRequest{
		Agent: "beta", Instruction: "sub", Constraints: "none", Acceptance: okChecks(),
		CreatedBy: parent.ID, Depth: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Related(parent.ID, child.ID) {
		t.Error("parent and child should be related")
	}
	if !rs.Related(child.ID, parent.ID) {
		t.Error("relation should be symmetric")
	}
	if !rs.Related(parent.ID, sibling.ID) {
		t.Error("tasks with the same creator are siblings")
	}
	if rs.Related(child.ID, sibling.ID) {
		t.Error("a child and its parent's sibling are not lineage-adjacent")
	}
}

func TestCancelReleasesWaitingWorker(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	errCh := make(chan error, 1)
	go func() {
		_, err := rs.Ask(context.Background(), task.ID, "waiting forever?")
		errCh <- err
	}()
	waitFor(t, func() bool {
		v, _ := rs.View(task.ID)
		return v.Status == model.TaskInputRequired
	}, "never parked")

	if err := rs.CancelTask(task.ID, "operator canceled"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("a canceled task's pending question should return an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceling did not release the worker parked on a question")
	}
}

// The approval gate is asynchronous state, not a blocked tool call. The
// regression this pins: delegation must stay refused through the whole
// propose→decide window — including after propose_plan has already returned —
// and only flip on an explicit human Approve.
func TestApprovalGateBlocksDelegate(t *testing.T) {
	b := openBudget()
	b.ApprovalPolicy = model.ApprovalInitial
	rs, _ := testSession(t, b)

	_, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", CreatedBy: RoleCoordinator, Depth: 1})
	if err == nil {
		t.Fatal("delegation before approval should be refused")
	}
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("the refusal must be identifiable as the approval gate, got: %v", err)
	}

	if err := rs.Propose(&model.Proposal{Summary: "plan"}); err != nil {
		t.Fatal(err)
	}
	if !rs.AwaitingApproval() {
		t.Fatal("run should be awaiting a human decision after propose")
	}
	// The leak observed in the wild: tasks created while the decision was
	// still pending. Propose has returned; the gate must still hold.
	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1}); !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("delegation between propose and decision must stay gated, got: %v", err)
	}

	if err := rs.Approve(); err != nil {
		t.Fatal(err)
	}
	if n := rs.TakeNotice(); !strings.Contains(n, "APPROVED") {
		t.Fatalf("approval must wake the coordinator with a notice, got %q", n)
	}
	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1}); err != nil {
		t.Fatalf("delegation after approval should be allowed: %v", err)
	}
}

// Rejection keeps the gate closed and tells the coordinator what to do next;
// a revised re-propose is legal.
func TestApprovalRejectionAllowsRepropose(t *testing.T) {
	b := openBudget()
	b.ApprovalPolicy = model.ApprovalInitial
	rs, _ := testSession(t, b)

	if err := rs.Propose(&model.Proposal{Summary: "plan v1"}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Reject("wrong direction"); err != nil {
		t.Fatal(err)
	}
	if n := rs.TakeNotice(); !strings.Contains(n, "REJECTED") || !strings.Contains(n, "wrong direction") {
		t.Fatalf("rejection notice must carry the reason, got %q", n)
	}
	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1}); !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("delegation after rejection must stay gated, got: %v", err)
	}
	if err := rs.Propose(&model.Proposal{Summary: "plan v2"}); err != nil {
		t.Fatalf("re-proposing after rejection should be legal: %v", err)
	}
	if err := rs.Approve(); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1}); err != nil {
		t.Fatalf("delegation after approved re-propose should pass: %v", err)
	}
}

func TestFinishIsOnce(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Finish(&Verdict{Status: model.RunFailed, Summary: "no"}); err == nil {
		t.Fatal("finish_run should be refused the second time")
	}
	if v := rs.Verdict(); v.Status != model.RunSucceeded {
		t.Fatalf("the first verdict must stand, got %s", v.Status)
	}
}

func TestCloseCancelsLiveTasks(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)
	rs.Close()

	v, _ := rs.View(task.ID)
	if v.Status != model.TaskCanceled {
		t.Fatalf("closing the run must cancel live tasks, got %s", v.Status)
	}
}

// ---- helpers ----

func drain(ch chan string) []string {
	var out []string
	for {
		select {
		case v := <-ch:
			out = append(out, v)
		case <-time.After(150 * time.Millisecond):
			return out
		}
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func waitForLong(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

func waitForNoT(cond func() bool) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A dynamic run has no structural progress guarantee, so a quiet ledger is the
// only signal that separates deep work from a wedged coordinator. The warning
// must reach the coordinator wherever it is — including inside a blocking await.
func TestStallWarningBreaksAwait(t *testing.T) {
	b := openBudget()
	b.StallSec = 1
	rs, _ := testSession(t, b)
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	// The task never moves again; the watcher should notice and interrupt.
	res, err := rs.Await(context.Background(), []string{task.ID}, "all", 60)
	if err != nil {
		t.Fatal(err)
	}
	if res.Notice == "" {
		t.Fatal("a stalled await must come back carrying the stall notice")
	}
	if !strings.Contains(res.Notice, "SYSTEM") {
		t.Fatalf("notice should be marked as a system message, got: %q", res.Notice)
	}
	if !res.TimedOut {
		t.Fatal("the tasks have not settled, so the result must say so")
	}
	if len(res.Tasks) != 1 || res.Tasks[0].Status != model.TaskWorking {
		t.Fatalf("the live state must still be reported: %+v", res.Tasks)
	}
}

// The notice is consumed once: it should not repeat on every later tool call.
func TestStallNoticeTakenOnce(t *testing.T) {
	b := openBudget()
	b.StallSec = 1
	rs, _ := testSession(t, b)
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	waitForLong(t, func() bool { return rs.peekNotice() != "" }, "stall was never detected")
	if n := rs.TakeNotice(); n == "" {
		t.Fatal("first take should yield the notice")
	}
	if n := rs.TakeNotice(); n != "" {
		t.Fatalf("notice should be consumed once, got it again: %q", n)
	}
}
