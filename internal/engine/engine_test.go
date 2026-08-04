package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"loom/internal/llm"
	"loom/internal/model"
	"loom/internal/store"
)

// fake is a scriptable backend: planFn for Kind=plan, nodeFn for Kind=node.
type fake struct {
	planFn func(req llm.Request) (*llm.Result, error)
	nodeFn func(req llm.Request) (*llm.Result, error)
}

func (f *fake) Name() string { return "fake" }
func (f *fake) Complete(_ context.Context, req llm.Request) (*llm.Result, error) {
	if req.Kind == llm.KindPlan {
		return f.planFn(req)
	}
	return f.nodeFn(req)
}

func planText(nodes ...model.PlanNode) *llm.Result {
	data, _ := json.Marshal(model.Plan{Nodes: nodes})
	return &llm.Result{Text: "```json\n" + string(data) + "\n```"}
}

func okNode(summary string) *llm.Result {
	return &llm.Result{Text: fmt.Sprintf("work done\n```json\n{\"status\":\"ok\",\"summary\":%q}\n```", summary)}
}

func setup(t *testing.T, be llm.Backend) (*Engine, *store.Store, *model.Workflow) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := st.SaveAgent(&model.Agent{Name: name, Description: "test agent"}); err != nil {
			t.Fatal(err)
		}
	}
	wf := &model.Workflow{Name: "test", MaxRetries: 0, Parallelism: 2}
	if err := st.SaveWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	// The fake registers as the "claude" runtime: from the engine's point of
	// view it is just whatever backend hosts that runtime's sessions.
	eng := New(st, map[string]llm.Backend{"claude": be}, NewBroker(), nil)
	return eng, st, wf
}

func waitTerminal(t *testing.T, st *store.Store, runID string) *model.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := st.LoadRun(runID)
		if err == nil && r.Terminal() {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	r, _ := st.LoadRun(runID)
	t.Fatalf("run did not reach terminal state, last: %+v", r)
	return nil
}

func TestRunSuccessDiamond(t *testing.T) {
	var calls atomic.Int32
	be := &fake{
		planFn: func(req llm.Request) (*llm.Result, error) {
			return planText(
				model.PlanNode{ID: "n1", Agent: "a", Title: "start", Instruction: "s"},
				model.PlanNode{ID: "n2", Agent: "b", Title: "left", Instruction: "l", DependsOn: []string{"n1"}},
				model.PlanNode{ID: "n3", Agent: "a", Title: "right", Instruction: "r", DependsOn: []string{"n1"}},
				model.PlanNode{ID: "n4", Agent: "b", Title: "join", Instruction: "j", DependsOn: []string{"n2", "n3"}},
			), nil
		},
		nodeFn: func(req llm.Request) (*llm.Result, error) {
			calls.Add(1)
			return okNode("done: " + req.Kind), nil
		},
	}
	eng, st, wf := setup(t, be)
	run, err := eng.StartRun(wf, "test goal", false)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("want succeeded, got %s (%s)", final.Status, final.Error)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("want 4 node executions, got %d", got)
	}
	for id, ns := range final.Nodes {
		if ns.Status != model.NodeSucceeded {
			t.Errorf("node %s: %s", id, ns.Status)
		}
	}
}

func TestUpstreamContextFlows(t *testing.T) {
	var joined atomic.Value
	be := &fake{
		planFn: func(req llm.Request) (*llm.Result, error) {
			return planText(
				model.PlanNode{ID: "n1", Agent: "a", Title: "first", Instruction: "one"},
				model.PlanNode{ID: "n2", Agent: "b", Title: "second", Instruction: "two", DependsOn: []string{"n1"}},
			), nil
		},
		nodeFn: func(req llm.Request) (*llm.Result, error) {
			if strings.Contains(req.Prompt, "two") {
				joined.Store(req.Prompt)
			}
			return okNode("SUMMARY_OF_FIRST"), nil
		},
	}
	eng, st, wf := setup(t, be)
	run, _ := eng.StartRun(wf, "goal", false)
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("run: %s (%s)", final.Status, final.Error)
	}
	prompt, _ := joined.Load().(string)
	if !strings.Contains(prompt, "SUMMARY_OF_FIRST") {
		t.Fatalf("downstream prompt missing upstream summary:\n%s", prompt)
	}
}

func TestApprovalGate(t *testing.T) {
	be := &fake{
		planFn: func(req llm.Request) (*llm.Result, error) {
			return planText(model.PlanNode{ID: "n1", Agent: "a", Title: "only", Instruction: "x"}), nil
		},
		nodeFn: func(req llm.Request) (*llm.Result, error) { return okNode("ok"), nil },
	}
	eng, st, wf := setup(t, be)
	wf.RequireApproval = true
	st.SaveWorkflow(wf)

	run, _ := eng.StartRun(wf, "goal", false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _ := st.LoadRun(run.ID)
		if r != nil && r.Status == model.RunAwaitingApproval {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached awaiting_approval")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := eng.Approve(run.ID); err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("after approval want succeeded, got %s", final.Status)
	}
}

func TestReplanOnFailure(t *testing.T) {
	var planCalls atomic.Int32
	be := &fake{
		planFn: func(req llm.Request) (*llm.Result, error) {
			if planCalls.Add(1) == 1 {
				return planText(
					model.PlanNode{ID: "n1", Agent: "a", Title: "will-fail", Instruction: "BOOM"},
					model.PlanNode{ID: "n2", Agent: "b", Title: "downstream", Instruction: "after", DependsOn: []string{"n1"}},
				), nil
			}
			if !strings.Contains(req.Prompt, "REPLAN") {
				t.Error("replan prompt should mention prior progress section")
			}
			return planText(model.PlanNode{ID: "n1", Agent: "b", Title: "recovery", Instruction: "recover"}), nil
		},
		nodeFn: func(req llm.Request) (*llm.Result, error) {
			if strings.Contains(req.Prompt, "BOOM") {
				return nil, fmt.Errorf("kaboom")
			}
			return okNode("recovered"), nil
		},
	}
	eng, st, wf := setup(t, be)
	wf.ReplanEnabled = true
	wf.MaxReplans = 1
	st.SaveWorkflow(wf)

	run, _ := eng.StartRun(wf, "goal", false)
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("recovered replan should succeed, got %s (%s)", final.Status, final.Error)
	}
	if !final.Nodes["n1"].Superseded {
		t.Fatal("failed node n1 should be marked superseded after replan")
	}
	if final.Replans != 1 {
		t.Fatalf("want 1 replan, got %d", final.Replans)
	}
	if ns := final.Nodes["r1.n1"]; ns == nil || ns.Status != model.NodeSucceeded {
		t.Fatalf("grafted node should have succeeded: %+v", ns)
	}
	if final.Nodes["n2"].Status != model.NodeSkipped {
		t.Fatalf("n2 should be skipped, got %s", final.Nodes["n2"].Status)
	}
}

func TestPlannerCreatedAgent(t *testing.T) {
	be := &fake{
		planFn: func(req llm.Request) (*llm.Result, error) {
			return planText2(model.Plan{
				Agents: []model.Agent{{Name: "poet", Description: "writes poems", Model: "claude-haiku-4-5",
					MaxTurns: 5, SystemPrompt: "You are a poet."}},
				Nodes: []model.PlanNode{{ID: "n1", Agent: "poet", Title: "poem", Instruction: "write"}},
			}), nil
		},
		nodeFn: func(req llm.Request) (*llm.Result, error) { return okNode("poem done"), nil },
	}
	eng, st, wf := setup(t, be)
	wf.AllowAgentCreation = true
	wf.AgentPool = []string{"a", "b"} // explicit subset → expect extension
	st.SaveWorkflow(wf)

	run, err := eng.StartRun(wf, "write a poem", false)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunSucceeded {
		t.Fatalf("want succeeded, got %s (%s)", final.Status, final.Error)
	}
	poet, err := st.LoadAgent("poet")
	if err != nil {
		t.Fatal("planner-created agent not persisted:", err)
	}
	if poet.SystemPrompt != "You are a poet." {
		t.Fatalf("agent definition mangled: %+v", poet)
	}
	wf2, _ := st.LoadWorkflow(wf.ID)
	found := false
	for _, n := range wf2.AgentPool {
		if n == "poet" {
			found = true
		}
	}
	if !found {
		t.Fatalf("workflow pool not extended: %v", wf2.AgentPool)
	}
}

func TestAgentCreationRejectedWhenDisabled(t *testing.T) {
	be := &fake{
		planFn: func(req llm.Request) (*llm.Result, error) {
			return planText2(model.Plan{
				Agents: []model.Agent{{Name: "poet", Description: "d", Model: "claude-haiku-4-5", SystemPrompt: "sp"}},
				Nodes:  []model.PlanNode{{ID: "n1", Agent: "poet"}},
			}), nil
		},
		nodeFn: func(req llm.Request) (*llm.Result, error) { return okNode("x"), nil },
	}
	eng, st, wf := setup(t, be) // AllowAgentCreation defaults to false
	run, _ := eng.StartRun(wf, "goal", false)
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunFailed {
		t.Fatalf("want failed (creation not allowed), got %s", final.Status)
	}
	if _, err := st.LoadAgent("poet"); err == nil {
		t.Fatal("agent must not be created when the workflow disallows it")
	}
}

func planText2(p model.Plan) *llm.Result {
	data, _ := json.Marshal(p)
	return &llm.Result{Text: "```json\n" + string(data) + "\n```"}
}

func TestCancel(t *testing.T) {
	started := make(chan struct{})
	be := &fake{
		planFn: func(req llm.Request) (*llm.Result, error) {
			return planText(model.PlanNode{ID: "n1", Agent: "a", Title: "slow", Instruction: "slow"}), nil
		},
		nodeFn: func(req llm.Request) (*llm.Result, error) {
			close(started)
			time.Sleep(5 * time.Second)
			return okNode("late"), nil
		},
	}
	eng, st, wf := setup(t, be)
	run, _ := eng.StartRun(wf, "goal", false)
	<-started
	if err := eng.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, st, run.ID)
	if final.Status != model.RunCanceled {
		t.Fatalf("want canceled, got %s", final.Status)
	}
}
