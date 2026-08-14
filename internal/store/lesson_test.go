package store

import (
	"testing"

	"loom/internal/model"
)

// The lesson lifecycle mirrors amendments: a coordinator-proposed rule is
// inert (pending) data until the user decides; only an approved rule belongs
// in the injection set, and a user-authored rule may be born approved — the
// human writing it IS the approval.
func TestLessonLifecycle(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	l := &model.Lesson{WorkflowID: "wf_1", RunID: "run_x", Text: "lead the report with the verdict"}
	if err := s.SaveLesson(l); err != nil {
		t.Fatal(err)
	}
	if l.ID == "" || l.Status != model.AmendmentPending {
		t.Fatalf("SaveLesson did not initialize the record: %+v", l)
	}

	// A user-authored rule starts approved when the caller says so.
	manual := &model.Lesson{WorkflowID: "wf_1", Text: "never poll data X", Status: model.AmendmentApproved}
	if err := s.SaveLesson(manual); err != nil {
		t.Fatal(err)
	}
	if manual.Status != model.AmendmentApproved {
		t.Fatalf("preset status not honored: %+v", manual)
	}

	// Approve the proposal; deciding twice is refused.
	decided, err := s.DecideLesson(l.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if decided.Status != model.AmendmentApproved || decided.DecidedAt.IsZero() {
		t.Fatalf("approval not recorded: %+v", decided)
	}
	if _, err := s.DecideLesson(l.ID, false); err == nil {
		t.Fatal("re-deciding a settled lesson succeeded")
	}

	// Edit in place keeps status and provenance.
	edited, err := s.UpdateLessonText(l.ID, "lead every report with the verdict")
	if err != nil {
		t.Fatal(err)
	}
	if edited.Status != model.AmendmentApproved || edited.RunID != "run_x" {
		t.Fatalf("edit changed more than the text: %+v", edited)
	}

	all, err := s.ListLessons()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListLessons: %v, %d entries", err, len(all))
	}

	// Delete removes it for good.
	if err := s.DeleteLesson(manual.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadLesson(manual.ID); err == nil {
		t.Fatal("deleted lesson still loads")
	}
	if err := s.DeleteLesson(manual.ID); err == nil {
		t.Fatal("deleting a missing lesson succeeded")
	}
}

// Supersession: a proposal naming existing approved rules snapshots them at
// save time; approval swaps atomically; a target edited since (or already
// gone) makes the proposal stale, and stale is refused — the human's newer
// decision wins. Retirement removes targets and injects nothing.
func TestLessonSupersession(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &model.Lesson{WorkflowID: "wf", Text: "rule A", Status: model.AmendmentApproved}
	b := &model.Lesson{WorkflowID: "wf", Text: "rule B", Status: model.AmendmentApproved}
	for _, l := range []*model.Lesson{a, b} {
		if err := s.SaveLesson(l); err != nil {
			t.Fatal(err)
		}
	}

	// Replacing a non-existent or non-approved target is refused at save time.
	if err := s.SaveLesson(&model.Lesson{WorkflowID: "wf", Text: "x", Replaces: []string{"lesson_ghost"}}); err == nil {
		t.Fatal("proposal replacing a ghost rule accepted")
	}
	if err := s.SaveLesson(&model.Lesson{WorkflowID: "other", Text: "x", Replaces: []string{a.ID}}); err == nil {
		t.Fatal("cross-workflow replacement accepted")
	}

	merged := &model.Lesson{WorkflowID: "wf", Text: "rule A+B merged", Replaces: []string{a.ID, b.ID}}
	if err := s.SaveLesson(merged); err != nil {
		t.Fatal(err)
	}
	if len(merged.ReplacedTexts) != 2 || merged.ReplacedTexts[0] != "rule A" {
		t.Fatalf("targets not snapshotted: %+v", merged)
	}

	// The user edits target A after the proposal → approval is stale-refused.
	if _, err := s.UpdateLessonText(a.ID, "rule A, human-edited"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideLesson(merged.ID, true); err == nil {
		t.Fatal("stale supersession applied over a newer human edit")
	}
	// Rejecting the stale proposal still works, and targets survive.
	if _, err := s.DecideLesson(merged.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadLesson(a.ID); err != nil {
		t.Fatal("rejected supersession removed its target")
	}

	// A fresh proposal against the current texts swaps cleanly.
	merged2 := &model.Lesson{WorkflowID: "wf", Text: "rule A+B v2", Replaces: []string{a.ID, b.ID}}
	if err := s.SaveLesson(merged2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideLesson(merged2.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadLesson(a.ID); err == nil {
		t.Fatal("superseded rule still exists after the swap")
	}

	// Retirement: empty text + replaces removes without adding.
	retire := &model.Lesson{WorkflowID: "wf", Replaces: []string{merged2.ID}}
	if err := s.SaveLesson(retire); err != nil {
		t.Fatal(err)
	}
	if !retire.Retirement() {
		t.Fatalf("retirement not recognized: %+v", retire)
	}
	if _, err := s.DecideLesson(retire.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadLesson(merged2.ID); err == nil {
		t.Fatal("retired rule still exists")
	}
}

func TestLessonValidation(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveLesson(&model.Lesson{WorkflowID: "wf", Text: "  "}); err == nil {
		t.Fatal("empty lesson text accepted")
	}
	if err := s.SaveLesson(&model.Lesson{Text: "rule"}); err == nil {
		t.Fatal("lesson without workflow accepted")
	}
}
