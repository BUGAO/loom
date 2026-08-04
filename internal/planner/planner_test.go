package planner

import (
	"strings"
	"testing"

	"loom/internal/model"
)

var pool = []*model.Agent{{Name: "a"}, {Name: "b"}}

func TestParseFenced(t *testing.T) {
	text := "Here is the plan:\n```json\n{\"nodes\":[{\"id\":\"n1\",\"agent\":\"a\",\"title\":\"t\",\"instruction\":\"i\",\"depends_on\":[]}]}\n```\ndone"
	p, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Nodes) != 1 || p.Nodes[0].ID != "n1" {
		t.Fatalf("unexpected plan: %+v", p)
	}
}

func TestParseBare(t *testing.T) {
	p, err := Parse(`{"nodes":[{"id":"x","agent":"b","depends_on":[]}]}`)
	if err != nil || len(p.Nodes) != 1 {
		t.Fatalf("bare json should parse: %v %+v", err, p)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		plan    model.Plan
		wantErr string
	}{
		{"ok", model.Plan{Nodes: []model.PlanNode{
			{ID: "n1", Agent: "a"},
			{ID: "n2", Agent: "b", DependsOn: []string{"n1"}},
		}}, ""},
		{"empty", model.Plan{}, "no nodes"},
		{"dup id", model.Plan{Nodes: []model.PlanNode{{ID: "n1", Agent: "a"}, {ID: "n1", Agent: "a"}}}, "duplicate"},
		{"unknown agent", model.Plan{Nodes: []model.PlanNode{{ID: "n1", Agent: "zzz"}}}, "unknown agent"},
		{"unknown dep", model.Plan{Nodes: []model.PlanNode{{ID: "n1", Agent: "a", DependsOn: []string{"nope"}}}}, "unknown node"},
		{"self dep", model.Plan{Nodes: []model.PlanNode{{ID: "n1", Agent: "a", DependsOn: []string{"n1"}}}}, "itself"},
		{"cycle", model.Plan{Nodes: []model.PlanNode{
			{ID: "n1", Agent: "a", DependsOn: []string{"n2"}},
			{ID: "n2", Agent: "a", DependsOn: []string{"n1"}},
		}}, "cycle"},
	}
	for _, c := range cases {
		err := Validate(&c.plan, pool, 8, false)
		if c.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.wantErr, err)
		}
	}
}

func TestValidateMaxNodes(t *testing.T) {
	p := model.Plan{}
	for i := 0; i < 9; i++ {
		p.Nodes = append(p.Nodes, model.PlanNode{ID: string(rune('a' + i)), Agent: "a"})
	}
	if err := Validate(&p, pool, 8, false); err == nil {
		t.Fatal("expected max-nodes error")
	}
}

func newAgent(name, mdl, tools string) model.Agent {
	return model.Agent{Name: name, Description: "d", Model: mdl, Tools: tools, MaxTurns: 10, SystemPrompt: "sp"}
}

func TestValidateAgentCreation(t *testing.T) {
	cases := []struct {
		name        string
		agents      []model.Agent
		allowCreate bool
		wantErr     string
	}{
		{"valid new agent", []model.Agent{newAgent("poet", "claude-haiku-4-5", "Read,Write")}, true, ""},
		{"creation disabled", []model.Agent{newAgent("poet", "claude-haiku-4-5", "")}, false, "does not allow"},
		{"bad name", []model.Agent{newAgent("Poet_X", "claude-haiku-4-5", "")}, true, "invalid"},
		{"duplicate of pool", []model.Agent{newAgent("existing-agent", "claude-haiku-4-5", "")}, true, "duplicates"},
		{"unknown model", []model.Agent{newAgent("poet", "gpt-5", "")}, true, "unknown model"},
		{"disallowed tool", []model.Agent{newAgent("poet", "claude-haiku-4-5", "Read,Agent")}, true, "disallowed tool"},
		{"missing prompt", []model.Agent{{Name: "poet", Description: "d", Model: "claude-haiku-4-5"}}, true, "non-empty"},
	}
	poolWithExisting := append([]*model.Agent{}, pool...)
	poolWithExisting = append(poolWithExisting, &model.Agent{Name: "existing-agent"})
	for _, c := range cases {
		p := model.Plan{
			Agents: c.agents,
			Nodes:  []model.PlanNode{{ID: "n1", Agent: c.agents[0].Name}},
		}
		err := Validate(&p, poolWithExisting, 8, c.allowCreate)
		if c.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.wantErr, err)
		}
	}
}

func TestBuildPromptAgentCreation(t *testing.T) {
	p := BuildPrompt("goal", pool, model.PlannerConfig{}, "", true)
	if !strings.Contains(p, "Defining new agents") || !strings.Contains(p, "claude-opus-5") {
		t.Fatal("agent-creation contract missing from prompt")
	}
	if q := BuildPrompt("goal", pool, model.PlannerConfig{}, "", false); strings.Contains(q, "Defining new agents") {
		t.Fatal("agent-creation contract leaked into non-creating prompt")
	}
}
