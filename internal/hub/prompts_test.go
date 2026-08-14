package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loom/internal/model"
)

// A fresh-session round prompt must carry the conversation so far — the whole
// point of the history section is that the user never has to repeat what they
// already said — while the messages being delivered as "new" appear only once.
func TestRoundPromptCarriesConversationHistory(t *testing.T) {
	rs, _ := testSession(t, openBudget())

	// An earlier round: user message delivered, coordinator replied.
	if err := rs.UserChat("remember: deploy target is the staging cluster"); err != nil {
		t.Fatal(err)
	}
	rs.TakeUserChat() // consumed by that round
	rs.CoordinatorReply("understood, planning against staging")

	// A new message arrives for the upcoming round.
	if err := rs.UserChat("change of plan: use blue-green rollout"); err != nil {
		t.Fatal(err)
	}
	userMsgs := rs.TakeUserChat()

	p := RoundPrompt(rs.run, rs, 2, nil, userMsgs)

	if !strings.Contains(p, "## Conversation so far") {
		t.Fatalf("no history section in a fresh-session prompt:\n%s", p)
	}
	if !strings.Contains(p, "[user] remember: deploy target is the staging cluster") {
		t.Error("earlier user message missing from history")
	}
	if !strings.Contains(p, "[you] understood, planning against staging") {
		t.Error("earlier coordinator reply missing from history")
	}
	if !strings.Contains(p, "## New messages from the user") ||
		!strings.Contains(p, "- change of plan: use blue-green rollout") {
		t.Error("new message not delivered in the new-messages section")
	}
	if strings.Contains(p, "[user] change of plan: use blue-green rollout") {
		t.Error("new message duplicated into the history section")
	}
}

// History is context, not the work: coordinator replies are clipped and an
// over-long conversation collapses to its tail.
func TestRoundPromptHistoryIsBounded(t *testing.T) {
	rs, _ := testSession(t, openBudget())

	long := strings.Repeat("x", historyCoordCap+500)
	if err := rs.UserChat("kick off"); err != nil {
		t.Fatal(err)
	}
	rs.TakeUserChat()
	rs.CoordinatorReply(long)

	p := RoundPrompt(rs.run, rs, 2, nil, nil)
	if strings.Contains(p, long) {
		t.Error("over-long coordinator reply was not truncated")
	}
	if !strings.Contains(p, historyTruncateNote) {
		t.Error("truncated reply carries no truncation marker")
	}

	// Blow past the message window; the overflow collapses into one line.
	extra := historyMaxMessages + 10
	for i := 0; i < extra; i++ {
		if err := rs.UserChat(fmt.Sprintf("filler message %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	rs.TakeUserChat()
	p = RoundPrompt(rs.run, rs, 3, nil, nil)
	// 2 earlier messages + extra fillers, minus the window.
	wantOmitted := extra + 2 - historyMaxMessages
	if !strings.Contains(p, fmt.Sprintf("(%d earlier message(s) omitted", wantOmitted)) {
		t.Errorf("history window not collapsed (want %d omitted):\n%.400s", wantOmitted, p)
	}
	if strings.Contains(p, "[user] kick off") {
		t.Error("message outside the window survived the collapse")
	}
	if !strings.Contains(p, fmt.Sprintf("filler message %d", extra-1)) {
		t.Error("tail of the conversation missing from the collapsed history")
	}
}

// A continuation round of a live session carries only the delta: new user
// messages and the settled tasks' ledger views — no full ledger, no notes, no
// conversation replay.
func TestContinuationPromptIsDelta(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	if err := rs.TaskStarted(task.ID); err != nil {
		t.Fatal(err)
	}
	rs.CompleteTask(task.ID, "wrote the summary file", []string{"a.txt"}, nil)
	rs.AddNote("strategy: alpha first")

	p := ContinuationPrompt(rs.run, rs, 3, []string{task.ID}, nil)

	if !strings.Contains(p, "(session continues)") {
		t.Fatalf("continuation prompt lost its framing:\n%s", p)
	}
	if !strings.Contains(p, task.ID) || !strings.Contains(p, "wrote the summary file") {
		t.Error("settled task view missing from the delta")
	}
	if !strings.Contains(p, "## Budget status") {
		t.Error("budget status missing")
	}
	for _, section := range []string{"## Task ledger", "## Conversation so far", "## Your notes"} {
		if strings.Contains(p, section) {
			t.Errorf("continuation prompt carries %q — that is fresh-session freight", section)
		}
	}
}

// A worker's observations travel: report_result → task → ledger view — the
// dissent channel must reach the coordinator even though the contract passed.
func TestObservationsReachTheLedgerView(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	task := mustDelegate(t, rs, "alpha")
	if err := rs.TaskStarted(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.ReportResult(task.ID, ReportedResult{
		Status: "ok", Summary: "done as specified",
		Observations: "the selector is cosmetic: league is fixed at boot, switching changes nothing",
	}); err != nil {
		t.Fatal(err)
	}
	v, ok := rs.View(task.ID)
	if !ok {
		t.Fatal("task view missing")
	}
	if !strings.Contains(v.Observations, "cosmetic") {
		t.Fatalf("observations did not reach the task view: %+v", v)
	}
}

// Project facts persist to PROJECT.md in the exchange directory and surface in
// both the coordinator's fresh round prompt and every worker prompt.
func TestProjectMemoryRoundTrip(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if err := rs.RecordProjectFact("leagues last 3+ months — fetch the list once per page load, never poll"); err != nil {
		t.Fatal(err)
	}
	if err := rs.RecordProjectFact("all ports come from the root config.yaml"); err != nil {
		t.Fatal(err)
	}

	mem := rs.ProjectMemory()
	if !strings.Contains(mem, "never poll") || !strings.Contains(mem, "config.yaml") {
		t.Fatalf("PROJECT.md content wrong:\n%s", mem)
	}

	p := RoundPrompt(rs.run, rs, 1, nil, nil)
	if !strings.Contains(p, "## Project memory") || !strings.Contains(p, "never poll") {
		t.Error("fresh round prompt does not carry project memory")
	}

	task := mustDelegate(t, rs, "writer")
	agent := &model.Agent{Name: "writer", Tools: "Read,Write"}
	wp := WorkerPrompt(task, agent, rs.run, rs.Workspace(), "", false)
	if !strings.Contains(wp, "## Project memory") || !strings.Contains(wp, "never poll") {
		t.Error("worker prompt does not carry project memory")
	}
}

// Over-cap PROJECT.md keeps its head AND its tail: the oldest facts are the
// foundational conventions, the newest are the latest corrections — truncation
// must never be the thing that drops a correction the user just recorded.
func TestProjectMemoryTruncationKeepsHeadAndTail(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if err := rs.RecordProjectFact("FOUNDATION: all ports come from the root config.yaml"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if err := rs.RecordProjectFact(fmt.Sprintf("filler fact %03d: %s", i, strings.Repeat("x", 40))); err != nil {
			t.Fatal(err)
		}
	}
	if err := rs.RecordProjectFact("CORRECTION: the league selector must actually switch the league"); err != nil {
		t.Fatal(err)
	}

	mem := rs.ProjectMemory()
	if len(mem) > projectMemoryCap+200 {
		t.Fatalf("clipped memory still too large: %d bytes", len(mem))
	}
	if !strings.Contains(mem, "FOUNDATION:") {
		t.Error("head (foundational fact) lost in truncation")
	}
	if !strings.Contains(mem, "CORRECTION:") {
		t.Error("tail (latest correction) lost in truncation — the pre-fix behavior")
	}
	if !strings.Contains(mem, "middle truncated") {
		t.Error("no elision marker")
	}
}

// An agent's craft memory (home MEMORY.md) rides into its worker prompt; the
// write-side nudge appears only for agents that can actually write files.
func TestWorkerPromptCarriesCraftMemory(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "MEMORY.md"), []byte("- always run the linter before reporting"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := mustDelegate(t, rs, "writer")
	agent := &model.Agent{Name: "writer", Tools: "Read,Write"}
	wp := WorkerPrompt(task, agent, rs.run, rs.Workspace(), home, false)
	if !strings.Contains(wp, "## Your craft memory") || !strings.Contains(wp, "always run the linter") {
		t.Error("worker prompt does not carry the agent's craft memory")
	}
	if !strings.Contains(wp, "MEMORY.md in your private workspace is your durable CRAFT memory") {
		t.Error("file-tool agent got no write-side memory guidance")
	}

	// No file tools → no write nudge (write_artifact cannot reach the home).
	pure := &model.Agent{Name: "thinker"}
	wp = WorkerPrompt(task, pure, rs.run, rs.Workspace(), t.TempDir(), false)
	if strings.Contains(wp, "durable CRAFT memory") {
		t.Error("tool-less agent was told to maintain MEMORY.md it cannot write")
	}
}

// A feedback-kind user message is delivered under its own postmortem framing
// (digest, don't resume work), never as a normal "new message"; the
// distillation the coordinator writes back lands on the run.
func TestFeedbackMessageGetsPostmortemFraming(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if err := rs.UserFeedback("那个表格太乱了,下次先给结论"); err != nil {
		t.Fatal(err)
	}
	userMsgs := rs.TakeUserChat()
	p := RoundPrompt(rs.run, rs, 2, nil, userMsgs)
	if !strings.Contains(p, "POSTMORTEM") || !strings.Contains(p, "conclude_feedback") {
		t.Fatalf("feedback message not framed as postmortem:\n%s", p)
	}
	if strings.Contains(p, "## New messages from the user") {
		t.Error("feedback message leaked into the normal new-messages section")
	}

	if err := rs.ConcludeFeedback("交付报告必须先给结论再展开论据;表格按严重度排序"); err != nil {
		t.Fatal(err)
	}
	if rs.run.Feedback == "" || rs.run.FeedbackAt.IsZero() {
		t.Fatal("conclude_feedback did not land on the run")
	}
	if err := rs.ConcludeFeedback("  "); err == nil {
		t.Error("empty distillation accepted")
	}
}

// Rules distilled from a postmortem are proposals, not effects: each lands as
// a pending lesson carrying its provenance, and nothing is injected from here
// — the user's approval is the only path into future prompts. A supersession
// carries the ids it replaces; a retirement carries only ids.
func TestProposeLessons(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if _, err := rs.ProposeLessons([]proposedRuleIn{{Text: "rule"}}); err == nil {
		t.Fatal("unwired lesson sink accepted a proposal")
	}

	var saved []*model.Lesson
	rs.cfg.SaveLesson = func(l *model.Lesson) error { saved = append(saved, l); return nil }

	n, err := rs.ProposeLessons([]proposedRuleIn{{Text: "  "}, {Text: "lead with the verdict"}, {Text: ""}})
	if err != nil || n != 1 {
		t.Fatalf("ProposeLessons = %d, %v; want 1 rule after trimming", n, err)
	}
	if len(saved) != 1 || saved[0].Text != "lead with the verdict" ||
		saved[0].WorkflowID != "wf_test" || saved[0].RunID != "run_test" || saved[0].Status != "" {
		t.Fatalf("saved lesson malformed: %+v", saved[0])
	}

	// A supersession keeps its targets; a retirement (empty text + replaces)
	// is not dropped by the empty-text filter.
	saved = nil
	n, err = rs.ProposeLessons([]proposedRuleIn{
		{Text: "one merged rule", Replaces: []string{"lesson_a", "lesson_b"}},
		{Replaces: []string{"lesson_c"}},
	})
	if err != nil || n != 2 {
		t.Fatalf("supersession/retirement proposal = %d, %v; want 2", n, err)
	}
	if len(saved[0].Replaces) != 2 || !saved[1].Retirement() {
		t.Fatalf("replaces not carried through: %+v, %+v", saved[0], saved[1])
	}

	if n, err := rs.ProposeLessons(nil); err != nil || n != 0 {
		t.Fatalf("empty proposal should be a no-op, got %d, %v", n, err)
	}
	if _, err := rs.ProposeLessons([]proposedRuleIn{{Text: strings.Repeat("x", maxLessonRuleLen+1)}}); err == nil {
		t.Error("overlong rule accepted — a norm is one directive, not a recap")
	}
	var many []proposedRuleIn
	for i := 0; i < maxRulesPerCall+1; i++ {
		many = append(many, proposedRuleIn{Text: fmt.Sprintf("rule %d", i)})
	}
	if _, err := rs.ProposeLessons(many); err == nil {
		t.Error("more rules than maxRulesPerCall accepted")
	}
}

// The independent-review guidance rides only when the pool actually has an
// independent agent — advising a review nobody can perform would be noise.
func TestCoordinatorPromptIndependentReviewGuidance(t *testing.T) {
	wf := &model.Workflow{Mode: model.ModeDynamic}
	withReviewer := []*model.Agent{
		{Name: "implementer", Description: "codes"},
		{Name: "reviewer", Description: "reviews", Independent: true},
	}
	p := CoordinatorPrompt(&model.Run{}, wf, model.DefaultBudget(), "/out", "", withReviewer, nil)
	if !strings.Contains(p, "## Independent review") || !strings.Contains(p, "BEFORE accepting the milestone") {
		t.Error("independent-review guidance missing despite an independent agent in the pool")
	}
	q := CoordinatorPrompt(&model.Run{}, wf, model.DefaultBudget(), "/out", "",
		[]*model.Agent{{Name: "implementer", Description: "codes"}}, nil)
	if strings.Contains(q, "## Independent review") {
		t.Error("independent-review guidance rendered with no independent agent to perform it")
	}
}

// A consolidation request gets maintenance framing: propose_rules only, no
// verdict, no delegation — and it never leaks into the normal new-messages
// section.
func TestConsolidateMessageGetsMaintenanceFraming(t *testing.T) {
	rs, _ := testSession(t, openBudget())
	if err := rs.UserConsolidate("Consolidate this workflow's standing rules."); err != nil {
		t.Fatal(err)
	}
	p := RoundPrompt(rs.run, rs, 2, nil, rs.TakeUserChat())
	if !strings.Contains(p, "Rule consolidation request") || !strings.Contains(p, "propose_rules") {
		t.Fatalf("consolidation message not framed as maintenance:\n%s", p)
	}
	if strings.Contains(p, "## New messages from the user") {
		t.Error("consolidation message leaked into the normal new-messages section")
	}
}

// The coordinator prompt carries past-run feedback and the amendment channel.
func TestCoordinatorPromptCarriesLessons(t *testing.T) {
	wf := &model.Workflow{Mode: model.ModeDynamic}
	rules := []*model.Lesson{{ID: "lesson_x1", Text: "lead the report with the verdict, argumentation after"}}
	p := CoordinatorPrompt(&model.Run{}, wf, model.DefaultBudget(), "/out", "", nil, rules)
	if !strings.Contains(p, "## Standing rules of this workflow") || !strings.Contains(p, "lead the report with the verdict") {
		t.Error("standing-rules section missing from coordinator prompt")
	}
	if !strings.Contains(p, "[lesson_x1]") {
		t.Error("rule id missing — a postmortem cannot supersede a rule it cannot name")
	}
	if !strings.Contains(p, "propose_agent_amendment") {
		t.Error("amendment guidance missing from coordinator prompt")
	}
	if q := CoordinatorPrompt(&model.Run{}, wf, model.DefaultBudget(), "/out", "", nil, nil); strings.Contains(q, "## Standing rules of this workflow") {
		t.Error("standing-rules section rendered with no lessons")
	}
}
