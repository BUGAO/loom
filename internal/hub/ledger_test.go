package hub

import (
	"context"
	"errors"
	"fmt"
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
		},
		Workspace: t.TempDir(),
		Exec:      exec,
		OnChange:  func(*model.Run) {},
		OnEvent:   func(string, string, string) {},
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

	go func() {
		waitForNoT(func() bool { return rs.AwaitingApproval() })
		rs.Approve()
	}()
	if err := rs.Propose(context.Background(), &model.Proposal{Summary: "plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1}); err != nil {
		t.Fatalf("delegation after approval should be allowed: %v", err)
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
