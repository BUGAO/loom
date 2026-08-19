package engine

import (
	"context"
	"testing"

	"loom/internal/model"
)

func TestParseListenKind(t *testing.T) {
	for in, want := range map[string]string{
		"task": "task", "Continuation.": "continuation", " question\n": "question", "meta": "meta",
		"I'd say this is a continuation of the work": "continuation",
	} {
		got, err := parseListenKind(in)
		if err != nil || got != want {
			t.Errorf("%q → %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := parseListenKind("dunno"); err == nil {
		t.Error("garbage should fail")
	}
}

// The listener on a dry run: the mock classifies by a few verbs, and a new
// task makes an assessment pending — which the mock coordinator then files,
// so the run still converges.
func TestListenerMakesAssessmentPending(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	kind, err := eng.classify(context.Background(), true, nil, "做一个 CLI 工具")
	if err != nil || kind != listenTask {
		t.Fatalf("mock listener: %q %v", kind, err)
	}
	kind, err = eng.classify(context.Background(), true, nil, "把按钮改成蓝色")
	if err != nil || kind != listenContinuation {
		t.Fatalf("mock listener: %q %v", kind, err)
	}
	if _, err := eng.classify(context.Background(), true, nil, "simulate-listener-fail"); err == nil {
		t.Fatal("simulated failure should surface")
	}

	run, err := eng.StartRun(wf, "build the thing", "", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("want succeeded, got %s (%s)", final.Status, final.Error)
	}
	// The opening assessment was required and filed; triage set a level.
	if len(final.Assessments) == 0 {
		t.Fatal("the run should carry the opening assessment")
	}
	if final.Level == "" || final.LevelSource == "" {
		t.Fatalf("level not set: %q %q", final.Level, final.LevelSource)
	}
	var sawCard, sawRequired bool
	for _, m := range final.Chat {
		if m.Kind == model.ChatTriage {
			sawCard = true
		}
	}
	for _, e := range final.Events {
		if e.Type == "triage" {
			sawRequired = true
		}
	}
	if !sawCard || !sawRequired {
		t.Fatalf("triage card %v / events %v missing", sawCard, sawRequired)
	}
}

// A static workflow template runs as ONE task of a dynamic run: the child
// run is planned and executed by the static engine in the same workspace,
// linked both ways, and the task settles with its outcome.
func TestTemplateRunsAsTask(t *testing.T) {
	eng, st, wf := dynSetup(t, noApproval())
	tpl := &model.Workflow{ID: "wf-tpl", Name: "pipeline", Description: "a fixed pipeline", Mode: model.ModeStatic,
		Planner: model.PlannerConfig{MaxNodes: 4}}
	if err := st.SaveWorkflow(tpl); err != nil {
		t.Fatal(err)
	}
	run, err := eng.StartRun(wf, "simulate-template build the thing", "", true)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("want succeeded, got %s (%s)", final.Status, final.Error)
	}
	var tplTask *model.Task
	for _, task := range final.Tasks {
		if task.Agent == model.TemplateAgentPrefix+"wf-tpl" {
			tplTask = task
		}
	}
	if tplTask == nil {
		t.Fatalf("no template task in %+v", final.TaskOrder)
	}
	if tplTask.Status != model.TaskCompleted || tplTask.SubRunID == "" {
		t.Fatalf("template task: %+v", tplTask)
	}
	child := waitTerminal(t, st, tplTask.SubRunID)
	if child.Status != model.RunSucceeded || child.Mode != model.ModeStatic {
		t.Fatalf("child run: %s %s", child.Status, child.Mode)
	}
	if child.ParentRunID != run.ID || child.ParentTaskID != tplTask.ID {
		t.Fatalf("child not linked to parent: %s/%s", child.ParentRunID, child.ParentTaskID)
	}
	if child.Workspace != final.Workspace {
		t.Fatalf("child must share the workspace: %s vs %s", child.Workspace, final.Workspace)
	}
	if len(child.Nodes) == 0 {
		t.Fatal("child run has no nodes")
	}
}
