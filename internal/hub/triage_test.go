package hub

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loom/internal/model"
)

func TestTriageRules(t *testing.T) {
	cfg := model.DefaultTriage()
	cases := []struct {
		name string
		a    model.TaskAssessment
		pair bool
		want string
	}{
		{"small docs", model.TaskAssessment{Steps: 2}, true, model.LevelSolo},
		{"code change with partners", model.TaskAssessment{Steps: 3, ChangesCode: true, EstFiles: 2}, true, model.LevelPair},
		{"code change without partners", model.TaskAssessment{Steps: 3, ChangesCode: true, EstFiles: 2}, false, model.LevelSolo},
		{"many steps", model.TaskAssessment{Steps: 6}, false, model.LevelOrchestrate},
		{"parallel branches", model.TaskAssessment{Steps: 3, ParallelBranches: 2}, false, model.LevelOrchestrate},
		{"many roles", model.TaskAssessment{Steps: 2, Roles: []string{"implementer", "Reviewer", "implementer"}}, false, model.LevelOrchestrate},
		{"many files", model.TaskAssessment{Steps: 2, EstFiles: 8}, false, model.LevelOrchestrate},
	}
	for _, c := range cases {
		got, why := Triage(c.a, cfg, c.pair)
		if got != c.want {
			t.Errorf("%s: got %s (%v), want %s", c.name, got, why, c.want)
		}
		if len(why) == 0 {
			t.Errorf("%s: no reasons", c.name)
		}
	}
	off := cfg
	off.PairOffForCode = true
	if got, _ := Triage(model.TaskAssessment{Steps: 3, ChangesCode: true}, off, true); got != model.LevelSolo {
		t.Errorf("pair opt-out ignored: %s", got)
	}
}

func triageSession(t *testing.T, wf *model.Workflow, pool []*model.Agent) *RunSession {
	t.Helper()
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	run := &model.Run{ID: "run_tri", WorkflowID: "wf", Mode: model.ModeDynamic,
		Tasks: map[string]*model.Task{}, CreatedAt: time.Now(), Level: model.LevelSolo, LevelSource: "default"}
	b := openBudget()
	wf.Mode, wf.Budget = model.ModeDynamic, &b
	rs := h.OpenRun(context.Background(), RunConfig{
		Run: run, Workflow: wf, Pool: pool, Workspace: t.TempDir(),
		PilotTools: "read,edit,write,bash", Exec: newFakeExec(), OnChange: func(*model.Run) {},
	})
	t.Cleanup(rs.Close)
	return rs
}

func TestAssessmentGatesWritesAndDelegate(t *testing.T) {
	rs := triageSession(t, &model.Workflow{}, []*model.Agent{{Name: "impl", Tools: "read,edit"}, {Name: "rev", Tools: "read", Independent: true}})
	declareTestEvidence(t, rs)
	rs.RequireAssessment("the run has started")
	pilot := identity{role: RoleCoordinator}
	ws := rs.Workspace()
	if d := rs.Gate(pilot, preWrite("Write", filepath.Join(ws, "a.go"))); d.Allow || !strings.Contains(d.Reason, "assess_task") {
		t.Fatalf("pending assessment must refuse writes: %+v", d)
	}
	if d := rs.Gate(pilot, preBash("echo x > a.go")); d.Allow || !strings.Contains(d.Reason, "assess_task") {
		t.Fatalf("pending assessment must refuse shell writes: %+v", d)
	}
	if d := rs.Gate(pilot, preBash("go test ./...")); !d.Allow {
		t.Fatalf("reading/testing is fine while pending: %+v", d)
	}
	_, err := rs.Delegate(DelegateRequest{Agent: "impl", Instruction: "x", Constraints: "none", Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1})
	if err == nil || !strings.Contains(err.Error(), "assess_task") {
		t.Fatalf("pending assessment must refuse delegate: %v", err)
	}
	// Bad assessments are refused.
	if _, err := rs.Assess(model.TaskAssessment{Summary: "", Steps: 1}); err == nil {
		t.Fatal("empty summary accepted")
	}
	// A code change with an independent agent in the pool → pair.
	a, err := rs.Assess(model.TaskAssessment{Summary: "add endpoint", Steps: 3, ChangesCode: true, EstFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	if a.Level != model.LevelPair || !a.Applied || rs.Level() != model.LevelPair {
		t.Fatalf("triage should apply pair: %+v level=%s", a, rs.Level())
	}
	if rs.AssessmentPending() {
		t.Fatal("assessment still pending")
	}
	if d := rs.Gate(pilot, preWrite("Write", filepath.Join(ws, "a.go"))); !d.Allow {
		t.Fatalf("writes free again: %+v", d)
	}
	// The card landed in the chat.
	run := rs.Run()
	last := run.Chat[len(run.Chat)-1]
	if last.From != "system" || last.Kind != model.ChatTriage || !strings.Contains(last.Text, "pair") {
		t.Fatalf("triage card missing: %+v", last)
	}
	if len(run.Assessments) != 1 || run.LevelSource != "triage" {
		t.Fatalf("assessment record: %+v %s", run.Assessments, run.LevelSource)
	}
	// Re-assessing as bigger work raises to orchestrate.
	rs.RequireAssessment("new task")
	if _, err := rs.Assess(model.TaskAssessment{Summary: "big", Steps: 9, ChangesCode: true}); err != nil {
		t.Fatal(err)
	}
	if rs.Level() != model.LevelOrchestrate {
		t.Fatalf("expected orchestrate, got %s", rs.Level())
	}
}

func TestTriageDoesNotOverrideUserOrPin(t *testing.T) {
	// User-set level wins.
	rs := triageSession(t, &model.Workflow{}, []*model.Agent{{Name: "rev", Tools: "read", Independent: true}})
	rs.SetLevel(model.LevelSolo, "user", "I want to drive")
	rs.RequireAssessment("start")
	a, err := rs.Assess(model.TaskAssessment{Summary: "huge", Steps: 12, ChangesCode: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.Applied || a.Level != model.LevelOrchestrate || rs.Level() != model.LevelSolo {
		t.Fatalf("user's level must stand: %+v level=%s", a, rs.Level())
	}
	// Pinned workflow level wins too.
	rs2 := triageSession(t, &model.Workflow{Coordinator: &model.CoordinatorConfig{Level: model.LevelOrchestrate}}, nil)
	rs2.SetLevel(model.LevelOrchestrate, "workflow", "")
	rs2.RequireAssessment("start")
	a2, _ := rs2.Assess(model.TaskAssessment{Summary: "tiny", Steps: 1})
	if a2.Applied || rs2.Level() != model.LevelOrchestrate {
		t.Fatalf("pinned level must stand: %+v level=%s", a2, rs2.Level())
	}
}

func TestRequestLevelOnlyRaises(t *testing.T) {
	rs := triageSession(t, &model.Workflow{}, nil)
	if err := rs.RequestLevel(model.LevelSolo, "x"); err == nil {
		t.Fatal("same level should be refused")
	}
	if err := rs.RequestLevel(model.LevelOrchestrate, "three independent modules"); err != nil {
		t.Fatal(err)
	}
	if rs.Level() != model.LevelOrchestrate || rs.Run().LevelSource != "pilot" {
		t.Fatalf("raise not applied: %s %s", rs.Level(), rs.Run().LevelSource)
	}
	if err := rs.RequestLevel(model.LevelPair, "calmer"); err == nil || !strings.Contains(err.Error(), "HIGHER") {
		t.Fatalf("lowering must be refused: %v", err)
	}
	rs.SetLevel(model.LevelSolo, "user", "")
	if err := rs.RequestLevel(model.LevelOrchestrate, "x"); err == nil || !strings.Contains(err.Error(), "user set") {
		t.Fatalf("user-set level must block requests: %v", err)
	}
}

func TestReassessOnOwnWrites(t *testing.T) {
	wf := &model.Workflow{Triage: &model.TriageConfig{ReassessFiles: 3}}
	rs := triageSession(t, wf, nil)
	rs.RequireAssessment("start")
	if _, err := rs.Assess(model.TaskAssessment{Summary: "small fix", Steps: 2, ChangesCode: true, EstFiles: 1}); err != nil {
		t.Fatal(err)
	}
	pilot := identity{role: RoleCoordinator}
	ws := rs.Workspace()
	post := func(p string) {
		r := preWrite("Edit", filepath.Join(ws, p))
		r.Event = "PostToolUse"
		rs.Gate(pilot, r)
	}
	post("a.go")
	post("a.go") // same file twice: one distinct
	post("b.go")
	post("README.md") // docs do not count
	if rs.AssessmentPending() {
		t.Fatal("2 distinct code files should not trigger re-assessment at threshold 3")
	}
	post("c.go")
	if !rs.AssessmentPending() {
		t.Fatal("3 distinct code files should require a re-assessment")
	}
	if d := rs.Gate(pilot, preWrite("Edit", filepath.Join(ws, "d.go"))); d.Allow {
		t.Fatal("writes must be refused until re-assessed")
	}
	if n := rs.TakeNotice(); !strings.Contains(n, "assess_task") {
		t.Fatalf("the main agent should be told: %q", n)
	}
	// Re-filing clears it and resets the counter.
	if _, err := rs.Assess(model.TaskAssessment{Summary: "bigger than thought", Steps: 4, ChangesCode: true, EstFiles: 6}); err != nil {
		t.Fatal(err)
	}
	post("e.go")
	post("f.go")
	if rs.AssessmentPending() {
		t.Fatal("counter should have reset at re-assessment")
	}
}

func TestReassessOnAcceptanceFailures(t *testing.T) {
	wf := &model.Workflow{Triage: &model.TriageConfig{ReassessTestFailures: 2}}
	rs := triageSession(t, wf, []*model.Agent{{Name: "impl", Tools: "read,edit"}})
	declareTestEvidence(t, rs)
	rs.RequireAssessment("start")
	rs.Assess(model.TaskAssessment{Summary: "x", Steps: 2, ChangesCode: true})
	fail := func() {
		task, err := rs.Delegate(DelegateRequest{Agent: "impl", Instruction: "x", Constraints: "none", Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1})
		if err != nil {
			t.Fatal(err)
		}
		rs.TaskStarted(task.ID)
		rs.CompleteTaskWith(task.ID, "", nil, errors.New("tests failed"), model.FailBlocked,
			[]model.CheckResult{{Check: model.AcceptanceCheck{Kind: model.CheckCommand, Command: "go test"}, Passed: false}})
	}
	fail()
	if rs.AssessmentPending() {
		t.Fatal("one failure is not a pattern")
	}
	fail()
	if !rs.AssessmentPending() {
		t.Fatal("two acceptance failures should require a re-assessment")
	}
}

func TestListenerFailuresSurfaceAfterThree(t *testing.T) {
	rs := triageSession(t, &model.Workflow{}, nil)
	rs.ListenerResult("", errors.New("boom"))
	rs.ListenerResult("", errors.New("boom"))
	if n := len(rs.Run().Chat); n != 0 {
		t.Fatalf("two failures stay quiet, got %d chat messages", n)
	}
	rs.ListenerResult("", errors.New("boom"))
	chat := rs.Run().Chat
	if len(chat) != 1 || chat[0].Kind != model.ChatNotice || !strings.Contains(chat[0].Text, "3 次") {
		t.Fatalf("third failure must be surfaced: %+v", chat)
	}
	rs.ListenerResult("", errors.New("boom"))
	if len(rs.Run().Chat) != 2 {
		t.Fatal("every failure after the third keeps surfacing")
	}
	rs.ListenerResult("continuation", nil)
	rs.ListenerResult("", errors.New("boom"))
	if len(rs.Run().Chat) != 2 {
		t.Fatal("a success resets the streak")
	}
}

func TestDelegateTemplateGates(t *testing.T) {
	rs := triageSession(t, &model.Workflow{}, nil)
	rs.cfg.Templates = func() []*model.Workflow {
		return []*model.Workflow{{ID: "tpl", Name: "T", Mode: model.ModeStatic}}
	}
	if _, err := rs.DelegateTemplate(TemplateRequest{TemplateID: "nope", Goal: "x"}); err == nil || !strings.Contains(err.Error(), "unknown template") {
		t.Fatalf("unknown template: %v", err)
	}
	rs.RequireAssessment("start")
	if _, err := rs.DelegateTemplate(TemplateRequest{TemplateID: "tpl", Goal: "x"}); err == nil || !strings.Contains(err.Error(), "assess_task") {
		t.Fatalf("assessment gate: %v", err)
	}
	rs.Assess(model.TaskAssessment{Summary: "x", Steps: 2})
	if _, err := rs.DelegateTemplate(TemplateRequest{TemplateID: "tpl", Goal: "x"}); err == nil || !strings.Contains(err.Error(), "declare_evidence") {
		t.Fatalf("evidence gate: %v", err)
	}
	declareTestEvidence(t, rs)
	task, err := rs.DelegateTemplate(TemplateRequest{TemplateID: "tpl", Goal: "do the pipeline"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Agent != model.TemplateAgentPrefix+"tpl" || task.Status != model.TaskSubmitted || !strings.HasPrefix(task.Title, "T: ") {
		t.Fatalf("template task: %+v", task)
	}
	rs.SetSubRun(task.ID, "run_child")
	if got, _ := rs.TaskSnapshot(task.ID); got.SubRunID != "run_child" {
		t.Fatalf("sub run not linked: %+v", got)
	}
}
