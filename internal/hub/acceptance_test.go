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
	declareTestEvidence(t, rs)

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

	err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "all good", Evidence: testEvidenceMet()})
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
	if err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "all good", Evidence: testEvidenceMet()}); err != nil {
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

// ---- workspace (one directory per run) ----

// The workspace is fixed at open and is what every prompt names: the run's
// deliverables and its project are the same folder, and nothing renames it.
func TestWorkspaceIsFixedAtOpen(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	ws := rs.Workspace()
	if ws == "" {
		t.Fatal("a session must have a workspace")
	}
	mustDelegate(t, rs, "alpha")
	if rs.Workspace() != ws {
		t.Fatalf("dispatch must not move the workspace: %s → %s", ws, rs.Workspace())
	}
	run := rs.Run()
	if run.OutputDir != "" || run.OutputName != "" {
		t.Fatalf("the hub must not name output folders any more: %q %q", run.OutputDir, run.OutputName)
	}
	p := RoundPrompt(run, rs, 1, nil, nil)
	if !strings.Contains(p, "## Workspace\n"+ws) {
		t.Fatalf("round prompt should name the workspace:\n%s", p)
	}
	if strings.Contains(p, "name_output") || strings.Contains(p, "xchange director") {
		t.Fatalf("round prompt still speaks of output naming / exchange directories:\n%s", p)
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

// ---- contract gates: build needs test, no dry-run acceptance ----

// The newspush run shipped 11k lines with zero tests because every contract's
// bar was "go build". A build/lint check now requires a test command beside
// it; a dry-run/mock flag in an acceptance command is refused outright.
func TestValidateChecksBuildNeedsTestAndNoDryRun(t *testing.T) {
	cmd := func(c string) model.AcceptanceCheck {
		return model.AcceptanceCheck{Kind: model.CheckCommand, Command: c}
	}
	exists := model.AcceptanceCheck{Kind: model.CheckArtifactExists, Path: "README.md"}

	// build without test → refused, naming the offending check.
	err := ValidateChecks([]model.AcceptanceCheck{exists, cmd("cd /p && go build ./... && go vet ./...")})
	if err == nil || !strings.Contains(err.Error(), "no TEST command") {
		t.Fatalf("build-only contract should be refused, got %v", err)
	}
	// build + test → fine.
	if err := ValidateChecks([]model.AcceptanceCheck{cmd("go build ./..."), cmd("go test ./...")}); err != nil {
		t.Fatalf("build+test contract should pass: %v", err)
	}
	// Other ecosystems.
	if err := ValidateChecks([]model.AcceptanceCheck{cmd("cd web && npm run build")}); err == nil {
		t.Fatal("npm build without test should be refused")
	}
	if err := ValidateChecks([]model.AcceptanceCheck{cmd("cd web && npm run build"), cmd("cd web && npm test -- --run")}); err != nil {
		t.Fatalf("npm build+test should pass: %v", err)
	}
	if err := ValidateChecks([]model.AcceptanceCheck{cmd("cargo build"), cmd("cargo test")}); err != nil {
		t.Fatalf("cargo build+test should pass: %v", err)
	}
	// No build check at all (docs task) → no test demanded.
	if err := ValidateChecks([]model.AcceptanceCheck{exists}); err != nil {
		t.Fatalf("artifact-only contract should pass: %v", err)
	}
	// A test-only contract is fine too.
	if err := ValidateChecks([]model.AcceptanceCheck{cmd("pytest -q")}); err != nil {
		t.Fatalf("test-only contract should pass: %v", err)
	}
	// "test" as a shell builtin is not a test runner.
	if err := ValidateChecks([]model.AcceptanceCheck{cmd("test -f install.sh")}); err != nil {
		t.Fatalf("shell test builtin must not trip the build rule: %v", err)
	}

	// dry-run / mock flags → refused whatever else is there.
	for _, c := range []string{"./newspush send --dry-run", "./app deploy --dry_run", "./x --mock", "tool --no-send"} {
		if err := ValidateChecks([]model.AcceptanceCheck{cmd(c), cmd("go test ./...")}); err == nil || !strings.Contains(err.Error(), "dry-run/mock") {
			t.Errorf("%q should be refused as a dry-run acceptance, got %v", c, err)
		}
	}
	// A dry-run flag inside a task INSTRUCTION is not this rule's business;
	// only contracts are checked. Ensure a plain command still passes.
	if err := ValidateChecks([]model.AcceptanceCheck{cmd("./newspush send"), cmd("go test ./...")}); err != nil {
		t.Fatalf("real send should pass: %v", err)
	}
}

// ---- definition of done (evidence) ----

// No proofs, no tasks; a proof that needs the user forces an ask_user first;
// re-declaration replaces; the round prompt shows the list.
func TestEvidenceGatesDelegate(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	rs.mu.Lock()
	rs.run.Evidence = nil // undo the helper's declaration: this test is about the gate
	rs.mu.Unlock()

	_, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1})
	if err == nil || !strings.Contains(err.Error(), "declare_evidence") {
		t.Fatalf("delegate without a definition of done must be refused, got %v", err)
	}
	// External (A2A) callers are not the coordinator; they are not gated.
	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", CreatedBy: CreatedByExternal, Depth: 1}); err != nil {
		t.Fatalf("external delegate should not be gated on evidence: %v", err)
	}

	if err := rs.DeclareEvidence(nil); err == nil {
		t.Fatal("empty declaration must be refused")
	}
	if err := rs.DeclareEvidence([]EvidenceItem{{Claim: "a digest email arrives in the inbox", NeedsFromUser: "SMTP credentials"}}); err != nil {
		t.Fatal(err)
	}
	_, err = rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1})
	if err == nil || !strings.Contains(err.Error(), "ask_user") {
		t.Fatalf("a proof needing the user must force an ask_user before building, got %v", err)
	}
	if err := rs.AskUser("Which credentials can I use to send mail?"); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Delegate(DelegateRequest{Agent: "alpha", Instruction: "x", Constraints: "none",
		Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1}); err != nil {
		t.Fatalf("after asking, delegate should pass: %v", err)
	}
	p := RoundPrompt(rs.Run(), rs, 2, nil, nil)
	if !strings.Contains(p, "## Definition of done (declared proofs)") || !strings.Contains(p, "digest email arrives") ||
		!strings.Contains(p, "needs from user: SMTP credentials") {
		t.Fatalf("round prompt should list the proofs:\n%s", p)
	}
}

// succeeded needs every proof reported met with how; anything else is a
// failed run — which is always allowed.
func TestFinishSettlesEvidence(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if err := rs.DeclareEvidence([]EvidenceItem{{Claim: "email arrives"}, {Claim: "go test passes"}}); err != nil {
		t.Fatal(err)
	}
	// Unreported proof.
	err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "done",
		Evidence: []EvidenceResult{{Claim: "go test passes", Met: true, How: "task_1 acceptance"}}})
	if err == nil || !strings.Contains(err.Error(), "email arrives") {
		t.Fatalf("succeeded with an unreported proof must be refused, got %v", err)
	}
	// Unmet proof.
	err = rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "done", Evidence: []EvidenceResult{
		{Claim: "go test passes", Met: true, How: "task_1 acceptance"},
		{Claim: "email arrives", Met: false, How: "no SMTP"},
	}})
	if err == nil || !strings.Contains(err.Error(), "not met") {
		t.Fatalf("succeeded with an unmet proof must be refused, got %v", err)
	}
	// Met without how.
	err = rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "done", Evidence: []EvidenceResult{
		{Claim: "go test passes", Met: true, How: "task_1 acceptance"},
		{Claim: "email arrives", Met: true},
	}})
	if err == nil || !strings.Contains(err.Error(), "no verification") {
		t.Fatalf("met without how must be refused, got %v", err)
	}
	// Failed with the gap named is always fine, and records the results.
	if err := rs.Finish(&Verdict{Status: model.RunFailed, Summary: "no way to send mail", Evidence: []EvidenceResult{
		{Claim: "go test passes", Met: true, How: "task_1 acceptance"},
		{Claim: "email arrives", Met: false, How: "user has no credentials"},
	}}); err != nil {
		t.Fatalf("honest failure must be accepted: %v", err)
	}
	ev := rs.Evidence()
	if len(ev) != 2 || !ev[1].Met || ev[0].Met || ev[0].How != "user has no credentials" {
		t.Fatalf("results should be recorded on the run: %+v", ev)
	}
}

// ---- independent review gate ----

func TestFinishRequiresIndependentReviewAfterLastCodeChange(t *testing.T) {
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	budget := openBudget()
	run := &model.Run{ID: "run_review", Mode: model.ModeDynamic, Tasks: map[string]*model.Task{}}
	rs := h.OpenRun(context.Background(), RunConfig{
		Run: run, Workflow: &model.Workflow{Mode: model.ModeDynamic, Budget: &budget},
		Pool: []*model.Agent{
			{Name: "implementer", Description: "codes", Tools: "Read,Write,Edit,Bash"},
			{Name: "doc-writer", Description: "docs", Tools: "Read,Write"},
			{Name: "reviewer", Description: "reviews", Tools: "Read", Independent: true},
		},
		Workspace: t.TempDir(), Exec: newFakeExec(), OnChange: func(*model.Run) {},
	})
	t.Cleanup(rs.Close)
	declareTestEvidence(t, rs)
	os.WriteFile(filepath.Join(rs.Workspace(), "x.go"), []byte("package x"), 0o644)
	finish := func() error {
		rs.Inspect("x.go")
		return rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "ok", Evidence: testEvidenceMet()})
	}
	complete := func(agent string) *model.Task {
		task := mustDelegate(t, rs, agent)
		rs.TaskStarted(task.ID)
		rs.CompleteTaskWith(task.ID, "done", []string{"x.go"}, nil, "", nil)
		time.Sleep(2 * time.Millisecond) // EndedAt ordering
		return task
	}

	complete("implementer")
	if err := finish(); err == nil || !strings.Contains(err.Error(), "independently reviewed") {
		t.Fatalf("success after unreviewed implementation must be refused, got %v", err)
	}
	complete("reviewer")
	complete("implementer") // fixes after the review → review is stale
	if err := finish(); err == nil || !strings.Contains(err.Error(), "independently reviewed") {
		t.Fatalf("a review older than the last code change must not count, got %v", err)
	}
	complete("reviewer")
	complete("doc-writer") // documents after the review do not invalidate it
	if err := finish(); err != nil {
		t.Fatalf("reviewed implementation should be allowed to succeed: %v", err)
	}
}

// Without an independent agent in the pool nobody could review, so the gate
// does not apply (the prompt already says review is impossible there).
func TestReviewGateNeedsAnIndependentAgent(t *testing.T) {
	rs, _ := testSession(t, openBudget()) // pool: alpha, beta, writer, vip — none independent
	os.WriteFile(filepath.Join(rs.Workspace(), "x.go"), []byte("package x"), 0o644)
	task := mustDelegate(t, rs, "writer")
	rs.TaskStarted(task.ID)
	rs.CompleteTaskWith(task.ID, "done", []string{"x.go"}, nil, "", nil)
	rs.Inspect("x.go")
	if err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "ok", Evidence: testEvidenceMet()}); err != nil {
		t.Fatalf("no independent agent → no review gate: %v", err)
	}
}
