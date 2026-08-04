package hub

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"loom/internal/model"
	"loom/internal/planner"
)

// The hub toolset. Delegation, follow-up and hand-off are exposed to agents as
// ordinary MCP tools, so for a Claude session they are just tool calls inside
// its normal agentic loop — there is no separate orchestration mode to learn.
// Every handler is bound to the caller's identity at connection time; nothing
// here trusts an agent-supplied run or task id.

func (h *Hub) buildServer(rs *RunSession, id identity) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Title:   "loom orchestration hub",
		Version: "2",
	}, nil)
	if id.role == RoleCoordinator {
		h.addCoordinatorTools(srv, rs)
	} else {
		h.addWorkerTools(srv, rs, id.taskID)
	}
	return srv
}

// withNotice appends any pending system notice to a tool result, so a stall
// warning reaches the coordinator wherever it happens to be working.
func (rs *RunSession) withNotice(text string) string {
	if n := rs.TakeNotice(); n != "" {
		return text + "\n\n" + n
	}
	return text
}

func okf(rs *RunSession, format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: rs.withNotice(fmt.Sprintf(format, args...))},
	}}
}

// ---- coordinator tools ----

type listAgentsIn struct{}

type agentCard struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Model       string   `json:"model"`
	Tools       string   `json:"tools"`
	Skills      []string `json:"skills,omitempty"`
}

type listAgentsOut struct {
	Agents []agentCard `json:"agents"`
}

type acceptanceCheckIn struct {
	Kind       string `json:"kind" jsonschema:"artifact_exists | artifact_contains | command"`
	Path       string `json:"path,omitempty" jsonschema:"artifact checks: path relative to the exchange directory"`
	Pattern    string `json:"pattern,omitempty" jsonschema:"artifact_contains: regexp the file content must match"`
	Command    string `json:"command,omitempty" jsonschema:"command check: shell command run in the exchange directory; exit 0 = pass"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
}

func toChecks(in []acceptanceCheckIn) []model.AcceptanceCheck {
	out := make([]model.AcceptanceCheck, 0, len(in))
	for _, c := range in {
		out = append(out, model.AcceptanceCheck{
			Kind: c.Kind, Path: c.Path, Pattern: c.Pattern, Command: c.Command, TimeoutSec: c.TimeoutSec,
		})
	}
	return out
}

type delegateIn struct {
	Agent       string `json:"agent" jsonschema:"name of an agent from list_agents"`
	Title       string `json:"title,omitempty" jsonschema:"short label for the task tree"`
	Instruction string `json:"instruction" jsonschema:"self-contained task; the worker cannot see your context"`
	Constraints string `json:"constraints" jsonschema:"cross-domain constraints the worker cannot infer: interfaces, formats, style, boundaries with other tasks. 'none' if genuinely none"`
	ContextHint string `json:"context_hint,omitempty" jsonschema:"background the worker needs, e.g. which upstream artifacts to read. Never for independent verifiers"`

	Acceptance []acceptanceCheckIn `json:"acceptance" jsonschema:"machine-checkable passing criteria, fixed now, executed by the engine when the worker finishes. The worker's own report never decides"`
	RetryOf    string              `json:"retry_of,omitempty" jsonschema:"id of a failed task this delegation reworks; only allowed when it failed as 'blocked'"`
}

type delegateOut struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

type awaitIn struct {
	TaskIDs    []string `json:"task_ids,omitempty" jsonschema:"tasks to wait for; empty means all tasks"`
	Mode       string   `json:"mode,omitempty" jsonschema:"any (default) or all"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

type sendMessageIn struct {
	TaskID string `json:"task_id"`
	Text   string `json:"text"`
}

type progressIn struct{}

type progressOut struct {
	Tasks  []TaskView   `json:"tasks"`
	Budget BudgetStatus `json:"budget"`
}

type createAgentIn struct {
	Name         string `json:"name" jsonschema:"kebab-case, unique in the pool"`
	Description  string `json:"description" jsonschema:"when a planner should pick this agent"`
	Model        string `json:"model"`
	Tools        string `json:"tools,omitempty" jsonschema:"comma-separated subset of the allowed tool list; empty for pure reasoning"`
	MaxTurns     int    `json:"max_turns,omitempty"`
	SystemPrompt string `json:"system_prompt" jsonschema:"self-contained role definition"`
}

type proposePlanIn struct {
	Summary string `json:"summary" jsonschema:"how you intend to approach the goal"`
	Tasks   []struct {
		Agent string `json:"agent"`
		Title string `json:"title"`
		Why   string `json:"why,omitempty"`
	} `json:"tasks"`
	Agents []createAgentIn `json:"agents,omitempty" jsonschema:"new agents you intend to create, if any"`
}

type finishRunIn struct {
	Status    string   `json:"status" jsonschema:"succeeded or failed"`
	Summary   string   `json:"summary" jsonschema:"what was achieved, or what is missing and why"`
	Artifacts []string `json:"artifacts,omitempty" jsonschema:"paths relative to the exchange directory"`
}

type inspectIn struct {
	Path string `json:"path" jsonschema:"file path relative to the exchange directory"`
}

type recordNoteIn struct {
	Text string `json:"text" jsonschema:"the note; keep it short and decision-relevant"`
}

func (h *Hub) addCoordinatorTools(srv *mcp.Server, rs *RunSession) {
	activity := func(name string) { rs.CoordinatorActivity(name) }

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_agents",
		Description: "List the executor agents available to this run, with their capabilities and tool budgets.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listAgentsIn) (*mcp.CallToolResult, listAgentsOut, error) {
		activity("list_agents")
		var out listAgentsOut
		for _, a := range rs.PoolAgents() {
			out.Agents = append(out.Agents, agentCard{
				Name: a.Name, Description: a.Description, Model: a.Model,
				Tools: a.Tools, Skills: a.Skills,
			})
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "propose_plan",
		Description: "Submit your initial plan for human approval. Required before the first delegate when this " +
			"workflow gates on approval. Blocks until a human approves or rejects.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in proposePlanIn) (*mcp.CallToolResult, any, error) {
		activity("propose_plan")
		p := &model.Proposal{Summary: in.Summary}
		for _, t := range in.Tasks {
			p.Tasks = append(p.Tasks, model.ProposedTask{Agent: t.Agent, Title: t.Title, Why: t.Why})
		}
		for _, a := range in.Agents {
			p.Agents = append(p.Agents, model.Agent{
				Name: a.Name, Description: a.Description, Model: a.Model,
				Tools: a.Tools, MaxTurns: a.MaxTurns, SystemPrompt: a.SystemPrompt,
			})
		}
		if err := rs.Propose(ctx, p); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Plan approved. You may now delegate."), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "delegate",
		Description: "Hand a task to an agent. Returns immediately with a task id — the work runs in the " +
			"background. Use await to collect results. The instruction must be self-contained, constraints must " +
			"state what the worker cannot infer, and acceptance must define the machine-checkable passing bar " +
			"up front — the engine executes those checks itself; the worker's report never decides.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in delegateIn) (*mcp.CallToolResult, any, error) {
		activity("delegate → " + in.Agent)
		t, err := rs.Delegate(DelegateRequest{
			Agent: in.Agent, Title: in.Title, Instruction: in.Instruction,
			Constraints: in.Constraints, ContextHint: in.ContextHint,
			Acceptance: toChecks(in.Acceptance), RetryOf: in.RetryOf,
			CreatedBy: RoleCoordinator, Depth: 1,
		})
		if err != nil {
			if errors.Is(err, ErrApprovalPending) {
				return toolErr("%v. Call propose_plan first and wait for it to return.", err), nil, nil
			}
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Delegated to %s as task %s (status: submitted). Call await to collect its result.", in.Agent, t.ID),
			delegateOut{TaskID: t.ID, Status: t.Status}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "await",
		Description: "Wait for delegated tasks to settle (complete, fail, or ask you a question). Returns a " +
			"partial snapshot if they are still running — that is normal, just call it again.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in awaitIn) (*mcp.CallToolResult, *AwaitResult, error) {
		activity("await")
		mode := in.Mode
		if mode != "all" {
			mode = "any"
		}
		res, err := rs.Await(ctx, in.TaskIDs, mode, in.TimeoutSec)
		if err != nil {
			return toolErr("%v", err), nil, nil
		}
		if res.Notice == "" {
			res.Notice = rs.TakeNotice()
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "send_message",
		Description: "Send a message to a running task: answer a question it asked, or steer it. Answering a " +
			"question reaches the worker immediately; steering a busy worker lands at its next turn.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendMessageIn) (*mcp.CallToolResult, *SendResult, error) {
		activity("send_message → " + in.TaskID)
		res, err := rs.Send(in.TaskID, RoleCoordinator, in.Text)
		if err != nil {
			return toolErr("%v", err), nil, nil
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "progress",
		Description: "Snapshot of every task and how much of the run's budget is left.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ progressIn) (*mcp.CallToolResult, progressOut, error) {
		activity("progress")
		return nil, progressOut{Tasks: rs.Views(nil), Budget: rs.BudgetStatus()}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_agent",
		Description: "Define a new reusable specialist and add it to the pool. Only when no existing agent covers " +
			"the needed expertise. New agents are permanent and will be reused by future runs.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createAgentIn) (*mcp.CallToolResult, any, error) {
		activity("create_agent " + in.Name)
		if !rs.Budget().AllowAgentCreation {
			return toolErr("this workflow does not allow creating agents; delegate to one of the existing pool agents"), nil, nil
		}
		a := &model.Agent{
			Name: in.Name, Description: in.Description, Model: in.Model,
			Tools: in.Tools, MaxTurns: in.MaxTurns, SystemPrompt: in.SystemPrompt,
		}
		if err := rs.CreateAgent(a); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Agent %q created and added to the pool. You can delegate to it now.", a.Name), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "inspect",
		Description: "Read a deliverable from the exchange directory (audited, truncated at 16KB). This is your " +
			"ONLY read access to the work: use it to verify what workers actually produced. Declaring success " +
			"requires having inspected at least one deliverable.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inspectIn) (*mcp.CallToolResult, any, error) {
		activity("inspect " + in.Path)
		content, err := rs.Inspect(in.Path)
		if err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "## %s\n\n%s", in.Path, content), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "record_note",
		Description: "Persist a short note to yourself across decision rounds (strategy, what you tried, what to " +
			"avoid). You start each round with NO memory beyond the task ledger and these notes — record anything " +
			"a future round must know.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recordNoteIn) (*mcp.CallToolResult, any, error) {
		activity("record_note")
		rs.AddNote(in.Text)
		return okf(rs, "Note recorded."), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "finish_run",
		Description: "Declare the run finished. Call this exactly once, when the goal is met or when you have " +
			"concluded it cannot be. Success requires having inspected at least one deliverable. After calling " +
			"it, end your turn.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in finishRunIn) (*mcp.CallToolResult, any, error) {
		activity("finish_run")
		status := model.RunSucceeded
		if strings.EqualFold(in.Status, "failed") {
			status = model.RunFailed
		}
		if err := rs.Finish(&Verdict{Status: status, Summary: in.Summary, Artifacts: in.Artifacts}); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Run recorded as %s. Stop working and end your turn now.", status), nil, nil
	})
}

// CreateAgent validates a coordinator-proposed agent against the same
// guardrails static mode applies to planner-created ones, then persists it.
func (rs *RunSession) CreateAgent(a *model.Agent) error {
	plan := &model.Plan{
		Agents: []model.Agent{*a},
		Nodes:  []model.PlanNode{{ID: "n1", Agent: a.Name}},
	}
	if err := planner.Validate(plan, rs.PoolAgents(), 1, true); err != nil {
		return err
	}
	if rs.cfg.SaveAgent == nil {
		return fmt.Errorf("agent creation is not wired for this run")
	}
	if err := rs.cfg.SaveAgent(a); err != nil {
		return err
	}
	rs.AddAgent(a)
	rs.event("agent_created", "", fmt.Sprintf("coordinator created agent %q (model %s, tools %q)", a.Name, a.Model, a.Tools))
	return nil
}

// ---- worker tools ----

type reportProgressIn struct {
	Text string `json:"text" jsonschema:"what you have done and what is left"`
}

type askCoordinatorIn struct {
	Question string `json:"question" jsonschema:"the specific decision or fact you need"`
}

type askCoordinatorOut struct {
	Answer string `json:"answer"`
}

type handoffIn struct {
	Agent       string              `json:"agent"`
	Title       string              `json:"title,omitempty"`
	Instruction string              `json:"instruction" jsonschema:"self-contained task for the receiving agent"`
	Constraints string              `json:"constraints" jsonschema:"cross-domain constraints the receiver cannot infer; 'none' if genuinely none"`
	ContextHint string              `json:"context_hint,omitempty"`
	Acceptance  []acceptanceCheckIn `json:"acceptance" jsonschema:"machine-checkable passing criteria for the sub-task"`
}

type askAgentIn struct {
	TaskID   string `json:"task_id" jsonschema:"a task in your own lineage: your parent, your child, or a sibling"`
	Question string `json:"question"`
}

func (h *Hub) addWorkerTools(srv *mcp.Server, rs *RunSession, taskID string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "report_progress",
		Description: "Report progress mid-task so the coordinator can see where you are. Does not end your task " +
			"and does not require a reply.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in reportProgressIn) (*mcp.CallToolResult, any, error) {
		if err := rs.Progress(taskID, in.Text); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Progress recorded."}}}, nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "ask_coordinator",
		Description: "Ask the coordinator a question when the task is genuinely ambiguous and guessing would waste " +
			"the work. Blocks until it answers. Use this instead of inventing an assumption.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in askCoordinatorIn) (*mcp.CallToolResult, askCoordinatorOut, error) {
		answer, err := rs.Ask(ctx, taskID, in.Question)
		if err != nil {
			return toolErr("%v", err), askCoordinatorOut{}, nil
		}
		return nil, askCoordinatorOut{Answer: answer}, nil
	})

	if !rs.Budget().AllowPeerHandoff {
		return
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name: "handoff",
		Description: "Hand a sub-task to another agent directly. Use only for work that is genuinely outside your " +
			"own remit; the coordinator is notified and sees the result.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in handoffIn) (*mcp.CallToolResult, *delegateOut, error) {
		self, ok := rs.View(taskID)
		if !ok {
			return toolErr("your task is no longer active"), nil, nil
		}
		t, err := rs.Delegate(DelegateRequest{
			Agent: in.Agent, Title: in.Title, Instruction: in.Instruction,
			Constraints: in.Constraints, ContextHint: in.ContextHint,
			Acceptance: toChecks(in.Acceptance), CreatedBy: taskID, Depth: self.Depth + 1,
		})
		if err != nil {
			return toolErr("%v", err), nil, nil
		}
		rs.event("handoff", taskID, fmt.Sprintf("handed off to %s as %s", in.Agent, t.ID))
		return okf(rs, "Handed off to %s as task %s. It runs independently; you are not blocked on it.", in.Agent, t.ID),
			&delegateOut{TaskID: t.ID, Status: t.Status}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "ask_agent",
		Description: "Send a question to a task in your own lineage (your parent, your child, or a sibling). " +
			"The exchange is recorded on both tasks.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in askAgentIn) (*mcp.CallToolResult, any, error) {
		if in.TaskID == taskID {
			return toolErr("that is your own task"), nil, nil
		}
		if !rs.Related(taskID, in.TaskID) {
			return toolErr("task %s is not in your lineage; you may only ask your parent, your children, or your siblings. "+
				"For anything else, ask_coordinator", in.TaskID), nil, nil
		}
		if err := rs.PeerMessage(taskID, in.TaskID, in.Question); err != nil {
			return toolErr("%v", err), nil, nil
		}
		res, err := rs.Send(in.TaskID, taskID, in.Question)
		if err != nil {
			return toolErr("%v", err), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("Question recorded and delivered (%s). Peer answers are not synchronous — "+
				"continue with your task; if you cannot proceed without the answer, use ask_coordinator.", res.Delivery),
		}}}, nil, nil
	})
}
