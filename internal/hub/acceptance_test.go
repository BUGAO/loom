package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loom/internal/model"
)

// ---- the delegation contract (B1/A4/A3) ----

func TestDelegateRequiresConstraints(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	_, err := rs.Delegate(DelegateRequest{
		Agent: "alpha", Instruction: "x", Acceptance: okChecks(),
		CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "constraints") {
		t.Fatalf("delegation without constraints must be refused with guidance, got: %v", err)
	}
}

func TestDelegateRequiresAcceptance(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	_, err := rs.Delegate(DelegateRequest{
		Agent: "alpha", Instruction: "x", Constraints: "none",
		CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("delegation without an acceptance contract must be refused, got: %v", err)
	}
}

func TestDelegateValidatesChecks(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	bad := [][]model.AcceptanceCheck{
		{{Kind: "made-up"}},
		{{Kind: model.CheckArtifactExists, Path: "../escape.txt"}},
		{{Kind: model.CheckArtifactExists, Path: "/etc/passwd"}},
		{{Kind: model.CheckArtifactContains, Path: "a.txt", Pattern: "("}},
		{{Kind: model.CheckCommand, Command: "  "}},
	}
	for i, checks := range bad {
		_, err := rs.Delegate(DelegateRequest{
			Agent: "alpha", Instruction: "x", Constraints: "none", Acceptance: checks,
			CreatedBy: RoleCoordinator, Depth: 1,
		})
		if err == nil {
			t.Errorf("malformed contract %d should have been refused", i)
		}
	}
}

// External A2A submissions carry no contract of their own; they must still be
// admitted (boundary hardening is out of scope here).
func TestExternalDelegateNeedsNoContract(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if _, err := rs.Delegate(DelegateRequest{
		Agent: "alpha", Instruction: "x", CreatedBy: CreatedByExternal, Depth: 1,
	}); err != nil {
		t.Fatalf("external submission should not require constraints/acceptance: %v", err)
	}
}

func TestIndependentAgentRejectsContextHint(t *testing.T) {
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	exec := newFakeExec()
	budget := openBudget()
	run := &model.Run{ID: "run_ind", Mode: model.ModeDynamic, Tasks: map[string]*model.Task{}}
	rs := h.OpenRun(context.Background(), RunConfig{
		Run:      run,
		Workflow: &model.Workflow{Mode: model.ModeDynamic, Budget: &budget},
		Pool: []*model.Agent{
			{Name: "reviewer", Description: "r", Independent: true},
		},
		Workspace: t.TempDir(),
		Exec:      exec,
		OnChange:  func(*model.Run) {},
		OnEvent:   func(string, string, string) {},
	})
	t.Cleanup(rs.Close)

	_, err := rs.Delegate(DelegateRequest{
		Agent: "reviewer", Instruction: "review the code", Constraints: "none", Acceptance: okChecks(),
		ContextHint: "the author says it all works and the tricky part is in util.go",
		CreatedBy:   RoleCoordinator, Depth: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "independent") {
		t.Fatalf("context_hint for an independent verifier must be refused, got: %v", err)
	}
	if _, err := rs.Delegate(DelegateRequest{
		Agent: "reviewer", Instruction: "review artifacts a.go, b.go against the requirement",
		Constraints: "none", Acceptance: okChecks(),
		CreatedBy: RoleCoordinator, Depth: 1,
	}); err != nil {
		t.Fatalf("hint-free delegation to the verifier should pass: %v", err)
	}
}

// ---- acceptance execution (B2/B3) ----

func TestRunChecksKinds(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "out.txt"), []byte("hello loom"), 0o644)

	cases := []struct {
		check model.AcceptanceCheck
		want  bool
	}{
		{model.AcceptanceCheck{Kind: model.CheckArtifactExists, Path: "out.txt"}, true},
		{model.AcceptanceCheck{Kind: model.CheckArtifactExists, Path: "missing.txt"}, false},
		{model.AcceptanceCheck{Kind: model.CheckArtifactContains, Path: "out.txt", Pattern: "hello"}, true},
		{model.AcceptanceCheck{Kind: model.CheckArtifactContains, Path: "out.txt", Pattern: "absent"}, false},
		{model.AcceptanceCheck{Kind: model.CheckCommand, Command: "true"}, true},
		{model.AcceptanceCheck{Kind: model.CheckCommand, Command: "exit 3"}, false},
	}
	for i, c := range cases {
		results, pass := RunChecks(context.Background(), ws, []model.AcceptanceCheck{c.check})
		if pass != c.want {
			t.Errorf("case %d (%s): pass=%v want %v (%s)", i, c.check.Kind, pass, c.want, results[0].Detail)
		}
	}
}

// A worker's "ok" is a claim; the verdict comes from the executed contract.
func TestAcceptanceResultRecordedOnCompletion(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	results, pass := RunChecks(context.Background(), rs.Workspace(), task.Acceptance)
	if !pass {
		t.Fatalf("the command:true contract should pass, got %+v", results)
	}
	rs.CompleteTaskWith(task.ID, "did it", nil, nil, "", results)

	v, _ := rs.View(task.ID)
	if v.Status != model.TaskCompleted {
		t.Fatalf("want completed, got %s", v.Status)
	}
	if !strings.Contains(v.Acceptance, "1/1") {
		t.Fatalf("view should carry the executed-check summary, got %q", v.Acceptance)
	}
}

// ---- failure typing and routing (E1/E2/E3) ----

func failTask(t *testing.T, rs *RunSession, agent, title, kind string) *model.Task {
	t.Helper()
	task, err := rs.Delegate(DelegateRequest{
		Agent: agent, Title: title, Instruction: "do", Constraints: "none", Acceptance: okChecks(),
		CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rs.TaskStarted(task.ID)
	rs.CompleteTaskWith(task.ID, "", nil, errTest, kind, nil)
	return task
}

var errTest = errorString("it broke")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestReworkAllowedOnlyForBlocked(t *testing.T) {
	rs, _ := testSession(t, openBudget())

	blocked := failTask(t, rs, "alpha", "task-blocked", model.FailBlocked)
	unclear := failTask(t, rs, "alpha", "task-unclear", model.FailSpecUnclear)

	if _, err := rs.Delegate(DelegateRequest{
		Agent: "alpha", Title: "rework 1", Instruction: "redo", Constraints: "none", Acceptance: okChecks(),
		RetryOf: blocked.ID, CreatedBy: RoleCoordinator, Depth: 1,
	}); err != nil {
		t.Fatalf("rework of a blocked failure should be allowed: %v", err)
	}

	_, err := rs.Delegate(DelegateRequest{
		Agent: "alpha", Title: "rework 2", Instruction: "redo", Constraints: "none", Acceptance: okChecks(),
		RetryOf: unclear.ID, CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err == nil || !strings.Contains(err.Error(), model.FailSpecUnclear) {
		t.Fatalf("rework of a spec-unclear failure must be refused with the kind named, got: %v", err)
	}
}

func TestReworkBudgetForcesEscalation(t *testing.T) {
	b := openBudget()
	b.MaxReworksPerTask = 1
	rs, _ := testSession(t, b)

	orig := failTask(t, rs, "alpha", "flaky", model.FailBlocked)
	first, err := rs.Delegate(DelegateRequest{
		Agent: "alpha", Title: "retry-1", Instruction: "redo", Constraints: "none", Acceptance: okChecks(),
		RetryOf: orig.ID, CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err != nil {
		t.Fatalf("first rework should pass: %v", err)
	}
	rs.TaskStarted(first.ID)
	rs.CompleteTaskWith(first.ID, "", nil, errTest, model.FailBlocked, nil)

	// Second rework — whether chained off the retry or the original — is over
	// the limit and must be refused with escalation guidance.
	_, err = rs.Delegate(DelegateRequest{
		Agent: "alpha", Title: "retry-2", Instruction: "redo again", Constraints: "none", Acceptance: okChecks(),
		RetryOf: first.ID, CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "Escalate") {
		t.Fatalf("rework past the budget must force escalation, got: %v", err)
	}
}

// Omitting retry_of must not bypass the router: same agent + same title as a
// failed task is treated as the rework it is.
func TestImplicitReworkDetected(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	failTask(t, rs, "alpha", "build the parser", model.FailSpecUnclear)

	_, err := rs.Delegate(DelegateRequest{
		Agent: "alpha", Title: "build the parser", Instruction: "try again but harder",
		Constraints: "none", Acceptance: okChecks(),
		CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err == nil || !strings.Contains(err.Error(), model.FailSpecUnclear) {
		t.Fatalf("silent re-delegation of a replan-required failure must be caught, got: %v", err)
	}
}

func TestFailureRouteInView(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	blocked := failTask(t, rs, "alpha", "a", model.FailBlocked)
	unclear := failTask(t, rs, "beta", "b", model.FailSpecUnclear)

	v, _ := rs.View(blocked.ID)
	if v.FailureKind != model.FailBlocked || v.Route != "rework-allowed" {
		t.Fatalf("blocked view: %+v", v)
	}
	v, _ = rs.View(unclear.ID)
	if v.Route != "replan-required" {
		t.Fatalf("unclear view: %+v", v)
	}
}

// ---- inspection gate (B4) ----

func TestFinishSuccessRequiresInspection(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	os.WriteFile(filepath.Join(rs.Workspace(), "report.md"), []byte("the deliverable"), 0o644)

	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)
	rs.CompleteTaskWith(task.ID, "done", []string{"report.md"}, nil, "", nil)

	err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "all good"})
	if err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("success without any inspection must be refused, got: %v", err)
	}

	content, err := rs.Inspect("report.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "the deliverable") {
		t.Fatalf("inspect returned wrong content: %q", content)
	}
	if err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "all good"}); err != nil {
		t.Fatalf("success after inspection should be accepted: %v", err)
	}
}

func TestFinishFailureNeedsNoInspection(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)
	rs.CompleteTaskWith(task.ID, "done", []string{"x.md"}, nil, "", nil)

	if err := rs.Finish(&Verdict{Status: model.RunFailed, Summary: "could not"}); err != nil {
		t.Fatalf("an honest failure verdict must not be blocked on inspection: %v", err)
	}
}

func TestInspectRejectsEscapes(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	for _, p := range []string{"../secret", "/etc/passwd"} {
		if _, err := rs.Inspect(p); err == nil {
			t.Errorf("inspect of %q should be refused", p)
		}
	}
}

// ---- user chat ----

func TestUserChatWakesRound(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	// The round driver has seen everything; only a user message should wake it.
	seen := map[string]string{}
	woke := make(chan string, 1)
	go func() { woke <- rs.AwaitRound(context.Background(), seen) }()
	select {
	case r := <-woke:
		t.Fatalf("AwaitRound returned %q before any wake reason existed", r)
	case <-time.After(150 * time.Millisecond):
	}

	if err := rs.UserChat("加一个深色模式"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-woke:
		if r != "user" {
			t.Fatalf("wake reason should be user, got %q", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a user message did not wake the round driver")
	}

	msgs := rs.TakeUserChat()
	if len(msgs) != 1 || msgs[0] != "加一个深色模式" {
		t.Fatalf("queued chat not delivered: %v", msgs)
	}
	if got := rs.TakeUserChat(); len(got) != 0 {
		t.Fatal("chat queue should drain once")
	}
	chat := rs.Run().Chat
	if len(chat) != 1 || chat[0].From != "user" {
		t.Fatalf("chat log should record the user message: %+v", chat)
	}
}

func TestCoordinatorReplyRecordedWithoutLedgerTransition(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	before := rs.Seq()
	rs.CoordinatorReply("正在拆解,已派出两个任务")
	if rs.Seq() != before {
		t.Fatal("a chat reply must not count as ledger progress")
	}
	chat := rs.Run().Chat
	if len(chat) != 1 || chat[0].From != "coordinator" {
		t.Fatalf("reply missing from chat log: %+v", chat)
	}
}

func TestUserChatRefusedAfterClose(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	rs.Close()
	if err := rs.UserChat("hello?"); err == nil {
		t.Fatal("chat to an ended run must be refused with guidance")
	}
}

// ---- round context reconstruction (D2) ----

// The round prompt is rebuilt from the ledger: for the same ledger state its
// size must not grow with the round number — that is the difference between
// reconstruction and accumulation.
func TestRoundPromptDoesNotGrowWithRounds(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	for i := 0; i < 3; i++ {
		task := mustDelegate(t, rs, "alpha")
		rs.TaskStarted(task.ID)
		rs.CompleteTask(task.ID, "done", nil, nil)
	}
	run := rs.Run()
	p5 := RoundPrompt(run, rs, 5, nil, nil)
	p50 := RoundPrompt(run, rs, 50, nil, nil)
	if diff := len(p50) - len(p5); diff < -2 || diff > 2 {
		t.Fatalf("round prompt size changed by %d bytes between round 5 and 50 on an identical ledger", diff)
	}
}
