package hub

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// A check that backgrounds a server must not hang the settle: the child
// inherits the output pipe, and before WaitDelay was set, CombinedOutput
// blocked on it forever — a real run froze at "working" this way, with the
// worker's report_result already delivered. The shell's own clean exit is the
// verdict, and the leftover child must be reaped with the group.
func TestRunChecksBackgroundChildNeitherHangsNorSurvives(t *testing.T) {
	ws := t.TempDir()
	pidFile := filepath.Join(ws, "child.pid")
	check := model.AcceptanceCheck{
		Kind:       model.CheckCommand,
		Command:    fmt.Sprintf("sleep 300 & echo $! > %s; exit 0", pidFile),
		TimeoutSec: 10,
	}
	done := make(chan bool, 1)
	go func() {
		_, pass := RunChecks(context.Background(), ws, []model.AcceptanceCheck{check})
		done <- pass
	}()
	select {
	case pass := <-done:
		if !pass {
			t.Fatalf("the shell exited 0; the backgrounded child must not fail the check")
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("RunChecks hung on the backgrounded child's inherited pipe")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("check did not record the child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("bad pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("backgrounded child %d outlived its check", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A check that overruns its timeout is killed as a group, so a stuck child
// cannot pin the settle past the check's own deadline either.
func TestRunChecksTimeoutKillsGroup(t *testing.T) {
	ws := t.TempDir()
	check := model.AcceptanceCheck{
		Kind:       model.CheckCommand,
		Command:    "sleep 300",
		TimeoutSec: 1,
	}
	start := time.Now()
	results, pass := RunChecks(context.Background(), ws, []model.AcceptanceCheck{check})
	if pass {
		t.Fatalf("a timed-out check must fail, got %+v", results)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("timed-out check took %s to settle", elapsed)
	}
	if !strings.Contains(results[0].Detail, "timed out") {
		t.Fatalf("detail should say the command timed out, got %q", results[0].Detail)
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

// ---- contract feasibility and amendment (P2/P3) ----

// Artifact contracts are feasible for EVERY agent: a writeless one delivers
// through the hub's write_artifact, so the old "impossible contract" refusal
// would now block exactly the delegations we want (md deliverables from
// pure-reasoning agents).
func TestArtifactContractFeasibleForWritelessAgent(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if _, err := rs.Delegate(DelegateRequest{
		Agent: "alpha" /* no tools */, Instruction: "review and write a report", Constraints: "none",
		Acceptance: []model.AcceptanceCheck{{Kind: model.CheckArtifactExists, Path: "report.md"}},
		CreatedBy:  RoleCoordinator, Depth: 1,
	}); err != nil {
		t.Fatalf("artifact contract on a writeless agent should be accepted (write_artifact makes it feasible): %v", err)
	}
}

func TestAmendAcceptance(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task, err := rs.Delegate(DelegateRequest{
		Agent: "writer", Title: "amendable", Instruction: "do", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rs.TaskStarted(task.ID)

	newContract := []model.AcceptanceCheck{{Kind: model.CheckArtifactExists, Path: "final.md"}}
	if err := rs.AmendAcceptance(task.ID, newContract); err != nil {
		t.Fatal(err)
	}
	// The engine judges by the amended contract…
	got := rs.AcceptanceOf(task.ID)
	if len(got) != 1 || got[0].Path != "final.md" {
		t.Fatalf("amended contract not in effect: %+v", got)
	}
	// …and the worker hears about it at its next turn boundary.
	inbox := rs.TakeInbox(task.ID)
	if len(inbox) != 1 || !strings.Contains(inbox[0], "AMENDED") || !strings.Contains(inbox[0], "final.md") {
		t.Fatalf("worker was not notified of the amendment: %v", inbox)
	}
}

func TestAmendRefusals(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	// Empty contract = waiving; never allowed.
	if err := rs.AmendAcceptance(task.ID, nil); err == nil {
		t.Fatal("amending to an empty contract must be refused")
	}
	// Terminal task: the contract has already judged.
	rs.CompleteTask(task.ID, "done", nil, nil)
	if err := rs.AmendAcceptance(task.ID, okChecks()); err == nil {
		t.Fatal("amending a terminal task must be refused")
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
	if len(msgs) != 1 || msgs[0].Text != "加一个深色模式" {
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

// A quiet ledger parks the round driver: no "nothing happened" wake, no hot
// loop of empty rounds, and no round-count death sentence. It wakes only for
// a reason — here, an injected system notice.
func TestAwaitRoundParksUntilNotice(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)
	rs.CompleteTask(task.ID, "done", nil, nil)

	seen := map[string]string{}
	for _, v := range rs.Views(nil) {
		if fp := SettledFingerprint(v); fp != "" {
			seen[v.ID] = fp
		}
	}

	woke := make(chan string, 1)
	go func() { woke <- rs.AwaitRound(context.Background(), seen) }()
	select {
	case r := <-woke:
		t.Fatalf("AwaitRound must park on a fully-seen quiet ledger, returned %q", r)
	case <-time.After(200 * time.Millisecond):
	}

	rs.InjectNotice("SYSTEM: act or finish")
	select {
	case r := <-woke:
		if r != "notice" {
			t.Fatalf("wake reason should be notice, got %q", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an injected notice did not wake the round driver")
	}
	if n := rs.TakeNotice(); n == "" {
		t.Fatal("the notice should be deliverable to the next round")
	}
}

// ---- deliverable folder naming (workflow-output) ----

func outputSession(t *testing.T, root string) (*RunSession, *fakeExec) {
	t.Helper()
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	exec := newFakeExec()
	budget := openBudget()
	run := &model.Run{ID: "run_out_" + fmt.Sprint(delegateN.Add(1)), Mode: model.ModeDynamic, Tasks: map[string]*model.Task{}}
	rs := h.OpenRun(context.Background(), RunConfig{
		Run:      run,
		Workflow: &model.Workflow{Mode: model.ModeDynamic, Budget: &budget},
		Pool: []*model.Agent{
			{Name: "alpha", Description: "a"},
		},
		Workspace:  t.TempDir(),
		OutputRoot: root,
		Exec:       exec,
		OnChange:   func(*model.Run) {},
	})
	t.Cleanup(rs.Close)
	return rs, exec
}

func TestOutputNameValidatedAndClaimed(t *testing.T) {
	root := t.TempDir()
	rs, _ := outputSession(t, root)

	for _, bad := range []string{"", "UPPER", "has space", "../escape", "-lead", strings.Repeat("x", 60)} {
		if err := rs.SetOutputName(bad); err == nil {
			t.Errorf("name %q should be refused", bad)
		}
	}
	if err := rs.SetOutputName("trading-health-check"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "trading-health-check")
	if rs.Workspace() != want {
		t.Fatalf("workspace should be the named folder, got %s", rs.Workspace())
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal("the folder must exist on naming")
	}

	// A second run with the same topic gets a suffixed folder, not a collision.
	rs2, _ := outputSession(t, root)
	if err := rs2.SetOutputName("trading-health-check"); err != nil {
		t.Fatal(err)
	}
	if got := rs2.Workspace(); got != filepath.Join(root, "trading-health-check-2") {
		t.Fatalf("colliding name should be suffixed, got %s", got)
	}
}

func TestOutputFreezesOnFirstDispatch(t *testing.T) {
	root := t.TempDir()
	rs, _ := outputSession(t, root)
	mustDelegate(t, rs, "alpha") // dispatch → auto-name + freeze

	ws := rs.Workspace()
	if filepath.Dir(ws) != root {
		t.Fatalf("auto-named workspace should live under the output root, got %s", ws)
	}
	if err := rs.SetOutputName("late-name"); err == nil {
		t.Fatal("renaming after the first dispatch must be refused")
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

// A legacy session (tasks exist, no named folder) keeps its internal exchange
// directory: deliverables must not move mid-conversation.
func TestLegacyRunKeepsInternalWorkspace(t *testing.T) {
	root := t.TempDir()
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	internalWS := t.TempDir()
	run := &model.Run{ID: "run_legacy", Mode: model.ModeDynamic, Tasks: map[string]*model.Task{
		"task_old": {ID: "task_old", Agent: "alpha", Status: model.TaskCompleted},
	}, TaskOrder: []string{"task_old"}}
	budget := openBudget()
	rs := h.OpenRun(context.Background(), RunConfig{
		Run:        run,
		Workflow:   &model.Workflow{Mode: model.ModeDynamic, Budget: &budget},
		Pool:       []*model.Agent{{Name: "alpha", Description: "a"}},
		Workspace:  internalWS,
		OutputRoot: root,
		Exec:       newFakeExec(),
		OnChange:   func(*model.Run) {},
	})
	t.Cleanup(rs.Close)

	if rs.Workspace() != internalWS {
		t.Fatalf("legacy run should keep the internal workspace, got %s", rs.Workspace())
	}
	if err := rs.SetOutputName("late"); err == nil {
		t.Fatal("naming a legacy run with dispatched tasks must be refused")
	}
}

// Image attachments are files on disk; the round prompt carries their names so
// the coordinator can connect the inline images to the words around them.
func TestRoundPromptNamesAttachedImages(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	run := rs.Run()
	run.Goal = "看看这张截图"
	run.GoalImages = []string{"img-1.png"}
	msgs := []model.ChatMessage{{Text: "再看这两张", Images: []string{"img-2.png", "img-3.png"}}}
	p := RoundPrompt(run, rs, 1, nil, msgs)
	for _, want := range []string{"img-1.png", "img-2.png", "img-3.png", "2 attached image(s)"} {
		if !strings.Contains(p, want) {
			t.Fatalf("round prompt should mention %q:\n%s", want, p)
		}
	}
}
