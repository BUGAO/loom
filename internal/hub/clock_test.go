package hub

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"loom/internal/model"
)

// fastClock shrinks the work clock's timing knobs for a test and restores them.
func fastClock(t *testing.T) {
	t.Helper()
	tick, ckpt, idle := clockTick, checkpointMin, deadlineIdleGrace
	clockTick, checkpointMin, deadlineIdleGrace = 20*time.Millisecond, 500*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { clockTick, checkpointMin, deadlineIdleGrace = tick, ckpt, idle })
}

func clockSession(t *testing.T, budget model.BudgetConfig, onExpire func()) (*RunSession, *fakeExec) {
	t.Helper()
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	exec := newFakeExec()
	run := &model.Run{
		ID: "run_clock", WorkflowID: "wf_test", Mode: model.ModeDynamic,
		Tasks: map[string]*model.Task{}, CreatedAt: time.Now(),
		Coordinator: &model.CoordinatorState{Status: "working"},
	}
	rs := h.OpenRun(context.Background(), RunConfig{
		Run:       run,
		Workflow:  &model.Workflow{Mode: model.ModeDynamic, Budget: &budget},
		Pool:      []*model.Agent{{Name: "alpha", Description: "a"}},
		Workspace: t.TempDir(),
		Exec:      exec,
		OnChange:  func(*model.Run) {},
		OnExpire:  onExpire,
	})
	t.Cleanup(rs.Close)
	declareTestEvidence(t, rs)
	return rs, exec
}

// The clock pauses while the run is parked on the approval gate: a plan that
// waits an hour for a human costs the run nothing.
func TestWorkClockPausesOnHuman(t *testing.T) {
	fastClock(t)
	b := openBudget()
	b.ApprovalPolicy = model.ApprovalInitial
	b.RunTimeoutSec = 3600
	rs, _ := clockSession(t, b, nil)

	waitFor(t, func() bool { return rs.WorkClock() > 0 }, "clock to start")
	if err := rs.Propose(&model.Proposal{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let a pending tick land
	before := rs.WorkClock()
	time.Sleep(300 * time.Millisecond)
	if after := rs.WorkClock(); after != before {
		t.Fatalf("clock advanced %v while awaiting approval", after-before)
	}
	if err := rs.Approve(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return rs.WorkClock() > before }, "clock to resume")

	// ask_user parks it the same way.
	rs.run.Coordinator.Status = "awaiting_user"
	time.Sleep(50 * time.Millisecond)
	before = rs.WorkClock()
	time.Sleep(200 * time.Millisecond)
	if after := rs.WorkClock(); after != before {
		t.Fatalf("clock advanced %v while awaiting the user", after-before)
	}
}

// Checkpoint before the deadline; at the deadline delegation closes but the
// in-flight task is NOT canceled; the hard stop only comes after the grace.
func TestWorkClockSoftDeadlineThenHardStop(t *testing.T) {
	fastClock(t)
	b := openBudget()
	b.RunTimeoutSec = 2
	b.TaskTimeoutSec = 1
	var expired atomic.Int32
	rs, exec := clockSession(t, b, func() { expired.Add(1) })

	id := delegateScoped(t, rs, "alpha")
	<-exec.started
	if err := rs.TaskStarted(id); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return rs.hasEvent("checkpoint") }, "checkpoint notice")
	if rs.DeadlineReached() {
		t.Fatalf("checkpoint must precede the deadline")
	}
	notice := rs.TakeNotice()
	if !strings.Contains(notice, "CHECKPOINT") {
		t.Fatalf("checkpoint notice missing: %q", notice)
	}

	waitFor(t, rs.DeadlineReached, "soft deadline")
	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "more", Constraints: "none",
		CreatedBy: RoleCoordinator, Depth: 1, Acceptance: okChecks()}); err == nil || !strings.Contains(err.Error(), "working-time budget") {
		t.Fatalf("delegate after the deadline: %v", err)
	}
	select {
	case c := <-exec.canceled:
		t.Fatalf("in-flight task %s was canceled at the soft deadline", c)
	case <-time.After(200 * time.Millisecond):
	}
	if st := rs.BudgetStatus(); !st.DeadlineReached || st.RunElapsedSec < 2 {
		t.Fatalf("budget status: %+v", st)
	}
	// Nothing is in flight once the task settles; after the idle grace with no
	// verdict, the hard stop fires.
	rs.CompleteTask(id, "done", nil, nil)
	waitFor(t, func() bool { return expired.Load() > 0 }, "hard stop")
	if !rs.hasEvent("run_status") {
		t.Fatalf("hard stop must be on the audit trail")
	}
}

func (rs *RunSession) hasEvent(typ string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, e := range rs.run.Events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// amend_scope is the coordinator's real answer to "widen my scope": the
// gate enforces the stored list immediately, and overlap with another
// in-flight task is refused.
func TestAmendScopeWidensWhatTheGateEnforces(t *testing.T) {
	rs, ws, _ := gateSession(t, "read,edit,write,bash")
	a := delegateScoped(t, rs, "impl", "src/a")
	b := delegateScoped(t, rs, "impl", "src/b/")
	wa := identity{role: RoleWorker, taskID: a}

	wire := filepath.Join(ws, "internal/app/wire.go")
	if d := rs.Gate(wa, preWrite("Edit", wire)); d.Allow || !strings.Contains(d.Reason, "amend_scope") {
		t.Fatalf("before widening: %+v", d)
	}
	// Overlap with b's in-flight scope is refused, both directions.
	if err := rs.AmendScope(a, []string{"src/a", "src/b/q.go"}); err == nil || !strings.Contains(err.Error(), b) {
		t.Fatalf("overlap into b should be refused: %v", err)
	}
	if err := rs.AmendScope(a, []string{"src"}); err == nil || !strings.Contains(err.Error(), b) {
		t.Fatalf("covering b should be refused: %v", err)
	}
	if err := rs.AmendScope(a, nil); err == nil {
		t.Fatalf("empty scope should be refused")
	}
	if err := rs.AmendScope(a, []string{"src/a", "internal/app/wire.go"}); err != nil {
		t.Fatal(err)
	}
	if d := rs.Gate(wa, preWrite("Edit", wire)); !d.Allow {
		t.Fatalf("after widening: %+v", d)
	}
	if d := rs.Gate(wa, preWrite("Edit", filepath.Join(ws, "internal/app/other.go"))); d.Allow {
		t.Fatalf("widening one file must not open the directory: %+v", d)
	}
	// The worker hears about it at its next turn, and the change is audited.
	if inbox := rs.TakeInbox(a); len(inbox) != 1 || !strings.Contains(inbox[0], "internal/app/wire.go") {
		t.Fatalf("worker inbox: %v", inbox)
	}
	if !rs.hasEvent("scope_amended") {
		t.Fatalf("scope change must be on the audit trail")
	}
	rs.TaskStarted(a)
	rs.CompleteTask(a, "done", nil, nil)
	if err := rs.AmendScope(a, []string{"."}); err == nil {
		t.Fatalf("settled task's scope must not be amendable")
	}
}
