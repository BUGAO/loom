// Package planner builds the per-workflow main-agent prompt, parses the DAG it
// returns, and statically validates it before anything executes.
package planner

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"loom/internal/model"
)

// Guardrails for planner-created agents.
const (
	MaxNewAgents     = 5
	MaxAgentTurns    = 60
	AllowedToolsList = "Read,Write,Edit,Bash,Grep,Glob,WebFetch,WebSearch"
)

var agentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,39}$`)

func allowedToolSet() map[string]bool {
	set := map[string]bool{}
	for _, t := range strings.Split(AllowedToolsList, ",") {
		set[t] = true
	}
	return set
}

// BuildPrompt renders the planning request: the goal, the agent registry the
// planner may draw from, the (optional) agent-creation contract, and the
// output contract.
func BuildPrompt(goal string, pool []*model.Agent, cfg model.PlannerConfig, prior string, allowCreate bool) string {
	var b strings.Builder
	b.WriteString("You are the planner (main agent) of a workflow orchestrator. ")
	b.WriteString("Assemble a DAG of executor-agent nodes that accomplishes the goal. ")
	b.WriteString("Each node is one independently verifiable unit of work assigned to one agent from the registry.\n\n")
	fmt.Fprintf(&b, "## Goal\n%s\n\n", goal)
	b.WriteString("## Agent registry\n")
	for _, a := range pool {
		fmt.Fprintf(&b, "- **%s**: %s\n", a.Name, a.Description)
	}
	maxNodes := cfg.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 8
	}
	if cfg.SystemPrompt != "" {
		fmt.Fprintf(&b, "\n## Workflow-specific guidance\n%s\n", cfg.SystemPrompt)
	}
	if prior != "" {
		fmt.Fprintf(&b, "\n## Prior progress (this is a REPLAN)\n%s\nPlan only the remaining work; completed results will be provided to new nodes as context.\n", prior)
	}
	if allowCreate {
		var models []string
		for _, m := range model.ModelCatalog {
			models = append(models, m.ID)
		}
		fmt.Fprintf(&b, `
## Defining new agents (optional)
If the registry lacks a suitable specialist, you may define new agents in an "agents" array; nodes may then reference them by name. Prefer reusing registry agents — create one only when the goal genuinely needs expertise none of them cover.
Each new agent: {"name":"<kebab-case, unique>","description":"<when the planner should pick this agent>","model":"<one of: %s>","tools":"<comma-separated subset of: %s; empty for pure reasoning>","max_turns":<1-%d>,"system_prompt":"<self-contained role definition: expertise, working style, quality bar>"}
At most %d new agents. Created agents join the shared pool permanently and will be reused by future runs, so write them as reusable specialists, not one-off task holders — the task itself goes in the node instruction.
`, strings.Join(models, ", "), AllowedToolsList, MaxAgentTurns, MaxNewAgents)
	}
	fmt.Fprintf(&b, `
## Output contract
Reply with ONLY a fenced json block:
`+"```json"+`
{%s"nodes":[{"id":"n1","agent":"<agent name>","title":"<short title>","instruction":"<self-contained task for the agent>","depends_on":[]}]}
`+"```"+`
Rules: at most %d nodes; ids unique; depends_on references node ids only; the graph must be acyclic; use parallel branches where work is independent; every node's "agent" must exist in the registry%s; write each instruction so the agent can act without seeing this conversation.
`, map[bool]string{true: `"agents":[...],`, false: ""}[allowCreate], maxNodes,
		map[bool]string{true: " or in your agents array", false: ""}[allowCreate])
	return b.String()
}

var fenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// Parse extracts the last fenced json block (or a bare JSON object) from the
// planner's reply.
func Parse(text string) (*model.Plan, error) {
	raw := ""
	if ms := fenceRe.FindAllStringSubmatch(text, -1); len(ms) > 0 {
		raw = ms[len(ms)-1][1]
	} else if t := strings.TrimSpace(text); strings.HasPrefix(t, "{") {
		raw = t
	}
	if raw == "" {
		return nil, fmt.Errorf("no json plan found in planner reply")
	}
	var p model.Plan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("plan json invalid: %w", err)
	}
	return &p, nil
}

// Validate checks the plan against the pool, the agent-creation guardrails,
// and structural rules.
func Validate(p *model.Plan, pool []*model.Agent, maxNodes int, allowCreate bool) error {
	if len(p.Nodes) == 0 {
		return fmt.Errorf("plan has no nodes")
	}
	if maxNodes <= 0 {
		maxNodes = 8
	}
	if len(p.Nodes) > maxNodes {
		return fmt.Errorf("plan has %d nodes, max is %d", len(p.Nodes), maxNodes)
	}
	agents := map[string]bool{}
	for _, a := range pool {
		agents[a.Name] = true
	}
	if len(p.Agents) > 0 && !allowCreate {
		return fmt.Errorf("plan defines new agents but this workflow does not allow agent creation")
	}
	if len(p.Agents) > MaxNewAgents {
		return fmt.Errorf("plan defines %d new agents, max is %d", len(p.Agents), MaxNewAgents)
	}
	tools := allowedToolSet()
	for i := range p.Agents {
		a := &p.Agents[i]
		if !agentNameRe.MatchString(a.Name) {
			return fmt.Errorf("new agent name %q invalid: kebab-case, 2-40 chars", a.Name)
		}
		if agents[a.Name] {
			return fmt.Errorf("new agent %q duplicates a registry agent — reference it instead", a.Name)
		}
		if !model.ValidModel(a.Model) {
			return fmt.Errorf("new agent %q uses unknown model %q", a.Name, a.Model)
		}
		if !model.ValidRuntime(a.Runtime) {
			return fmt.Errorf("new agent %q uses unknown runtime %q", a.Name, a.Runtime)
		}
		for _, t := range strings.Split(a.Tools, ",") {
			if t = strings.TrimSpace(t); t != "" && !tools[t] {
				return fmt.Errorf("new agent %q requests disallowed tool %q (allowed: %s)", a.Name, t, AllowedToolsList)
			}
		}
		if a.MaxTurns < 0 || a.MaxTurns > MaxAgentTurns {
			return fmt.Errorf("new agent %q max_turns %d out of range (1-%d)", a.Name, a.MaxTurns, MaxAgentTurns)
		}
		if strings.TrimSpace(a.SystemPrompt) == "" || strings.TrimSpace(a.Description) == "" {
			return fmt.Errorf("new agent %q needs a non-empty description and system_prompt", a.Name)
		}
		agents[a.Name] = true
	}
	ids := map[string]bool{}
	for _, n := range p.Nodes {
		if n.ID == "" {
			return fmt.Errorf("node with empty id")
		}
		if ids[n.ID] {
			return fmt.Errorf("duplicate node id %q", n.ID)
		}
		ids[n.ID] = true
		if !agents[n.Agent] {
			return fmt.Errorf("node %q references unknown agent %q", n.ID, n.Agent)
		}
	}
	deps := map[string][]string{}
	for _, n := range p.Nodes {
		for _, d := range n.DependsOn {
			if !ids[d] {
				return fmt.Errorf("node %q depends on unknown node %q", n.ID, d)
			}
			if d == n.ID {
				return fmt.Errorf("node %q depends on itself", n.ID)
			}
		}
		deps[n.ID] = n.DependsOn
	}
	// cycle check via DFS coloring
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		color[id] = gray
		for _, d := range deps[id] {
			switch color[d] {
			case gray:
				return fmt.Errorf("cycle detected involving node %q", id)
			case white:
				if err := visit(d); err != nil {
					return err
				}
			}
		}
		color[id] = black
		return nil
	}
	for id := range deps {
		if color[id] == white {
			if err := visit(id); err != nil {
				return err
			}
		}
	}
	return nil
}
