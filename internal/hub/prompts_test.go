package hub

import (
	"fmt"
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
	wp := WorkerPrompt(task, agent, rs.run, rs.Workspace(), false)
	if !strings.Contains(wp, "## Project memory") || !strings.Contains(wp, "never poll") {
		t.Error("worker prompt does not carry project memory")
	}
}
