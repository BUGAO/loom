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
	switch id.role {
	case RoleCoordinator:
		h.addCoordinatorTools(srv, rs)
	case RolePair:
		// The resident implementer's task binding moves between calls; every
		// handler resolves it fresh from the ledger.
		h.addWorkerTools(srv, rs, rs.PairTask, true)
	default:
		taskID := id.taskID
		h.addWorkerTools(srv, rs, func() string { return taskID }, false)
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
	Model       string `json:"model,omitempty" jsonschema:"model tier for THIS task, chosen by difficulty: haiku (mechanical, low-ambiguity), sonnet (standard work), opus (hard reasoning — the ceiling for workers). Empty = the agent's default model"`
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
	Model        string `json:"model" jsonschema:"the agent's default model: haiku | sonnet | opus at most — the top tier is reserved for the coordinator"`
	Tools        string `json:"tools,omitempty" jsonschema:"comma-separated subset of the allowed tool list; empty for pure reasoning"`
	MaxTurns     int    `json:"max_turns,omitempty"`
	SystemPrompt string `json:"system_prompt" jsonschema:"self-contained role definition"`
}

type proposePlanIn struct {
	Summary string `json:"summary" jsonschema:"how you intend to approach the goal"`
	Tasks   []struct {
		Agent string `json:"agent"`
		Model string `json:"model,omitempty" jsonschema:"intended model tier for this task: haiku | sonnet | opus; empty = the agent's default"`
		Title string `json:"title"`
		Why   string `json:"why,omitempty"`
	} `json:"tasks"`
	Agents     []createAgentIn `json:"agents,omitempty" jsonschema:"new agents you intend to create, if any"`
	OutputName string          `json:"output_name,omitempty" jsonschema:"short kebab-case topic name for the deliverable folder under the output root, e.g. trading-health-check"`
}

type nameOutputIn struct {
	Name string `json:"name,omitempty" jsonschema:"short kebab-case topic name for the deliverable folder under the default output root, e.g. trading-health-check"`
	Dir  string `json:"dir,omitempty" jsonschema:"absolute path (or ~/...) the USER asked the deliverables to land in; overrides the default root. Only when the user explicitly named a location"`
}

type askUserIn struct {
	Questions string `json:"questions" jsonschema:"your questions for the user, batched into ONE message — numbered if several. Ask only what you cannot decide yourself"`
}

type finishRunIn struct {
	Status    string   `json:"status" jsonschema:"succeeded or failed"`
	Summary   string   `json:"summary" jsonschema:"what was achieved, or what is missing and why"`
	Artifacts []string `json:"artifacts,omitempty" jsonschema:"paths relative to the exchange directory"`
}

type inspectIn struct {
	Path string `json:"path" jsonschema:"file path relative to the exchange directory"`
}

type amendAcceptanceIn struct {
	TaskID     string              `json:"task_id"`
	Acceptance []acceptanceCheckIn `json:"acceptance" jsonschema:"the corrected machine-checkable criteria; at least one — a contract cannot be waived"`
}

type recordNoteIn struct {
	Text string `json:"text" jsonschema:"the note; keep it short and decision-relevant"`
}

type recordProjectFactIn struct {
	Text string `json:"text" jsonschema:"one durable fact about the PROJECT (domain constraint, convention, user correction) that future runs must honor; short and declarative"`
}

type concludeFeedbackIn struct {
	Text string `json:"text" jsonschema:"the retrospective conclusion of THIS run: what the feedback taught, references resolved. Stored on the run as its postmortem record — never injected into future runs. Rules the retrospective yields go through propose_rules, not here"`
}

type proposedRuleIn struct {
	Text     string   `json:"text,omitempty" jsonschema:"one self-contained imperative directive ('do X', 'never Y') a future run can follow without this conversation — NOT a recap of events. Empty only for a pure retirement (replaces set, nothing added)"`
	Replaces []string `json:"replaces,omitempty" jsonschema:"ids of existing standing rules this one supersedes (they are listed with ids in your system prompt). Use whenever the new rule overlaps, refines or contradicts an existing one — never pile on a near-duplicate. Approval swaps them atomically"`
}

type proposeRulesIn struct {
	Rules []proposedRuleIn `json:"rules" jsonschema:"the proposed rule changes; only what will change future behavior"`
}

type proposeAmendmentIn struct {
	Agent          string `json:"agent" jsonschema:"name of the pool agent whose standing definition needs revision"`
	Rationale      string `json:"rationale" jsonschema:"the concrete evidence: which failure, observation or user feedback showed the agent's DEFINITION (not this run's spec) to be the problem"`
	ProposedPrompt string `json:"proposed_prompt" jsonschema:"the agent's COMPLETE revised system prompt — a full replacement, not a diff. Change only what the rationale justifies"`
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
			"workflow gates on approval. Returns immediately: END YOUR TURN after calling it — you will be " +
			"woken with the human's decision.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in proposePlanIn) (*mcp.CallToolResult, any, error) {
		activity("propose_plan")
		p := &model.Proposal{Summary: in.Summary}
		for _, t := range in.Tasks {
			m, _ := model.ResolveModel(t.Model) // display only; an invalid tier just shows empty
			p.Tasks = append(p.Tasks, model.ProposedTask{Agent: t.Agent, Model: m, Title: t.Title, Why: t.Why})
		}
		for _, a := range in.Agents {
			p.Agents = append(p.Agents, model.Agent{
				Name: a.Name, Description: a.Description, Model: a.Model,
				Tools: a.Tools, MaxTurns: a.MaxTurns, SystemPrompt: a.SystemPrompt,
			})
		}
		if in.OutputName != "" {
			if err := rs.SetOutputName(in.OutputName); err != nil {
				return toolErr("%v", err), nil, nil
			}
		}
		if err := rs.Propose(p); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Plan submitted for human approval. End your turn now — you will be woken when the human decides."), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "ask_user",
		Description: "Ask the user clarifying questions BEFORE committing to a plan — scope, priorities, and always " +
			"the deliverable location. Batch every open question into ONE call, then END YOUR TURN; the user's " +
			"answer arrives as a message in your next round. Never ask what you can decide yourself, and never " +
			"ask twice what was already answered.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in askUserIn) (*mcp.CallToolResult, any, error) {
		activity("ask_user")
		if err := rs.AskUser(in.Questions); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Question delivered to the user. End your turn now — their answer will wake your next round."), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "name_output",
		Description: "Set this run's deliverable folder. Default: a short kebab-case topic name under the output " +
			"root. When the USER named a location, pass it as dir (absolute or ~/ path) and it is honored verbatim. " +
			"Do this BEFORE delegating — the folder freezes at the first dispatch, and an unnamed run gets an " +
			"automatic name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nameOutputIn) (*mcp.CallToolResult, any, error) {
		switch {
		case in.Dir != "":
			activity("name_output → " + in.Dir)
			if err := rs.SetOutputDir(in.Dir); err != nil {
				return toolErr("%v", err), nil, nil
			}
		case in.Name != "":
			activity("name_output " + in.Name)
			if err := rs.SetOutputName(in.Name); err != nil {
				return toolErr("%v", err), nil, nil
			}
		default:
			return toolErr("pass name (topic under the default root) or dir (user-chosen absolute path)"), nil, nil
		}
		return okf(rs, "Deliverable folder: %s — every artifact of this run lands there.", rs.Workspace()), nil, nil
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
			Agent: in.Agent, Model: in.Model, Title: in.Title, Instruction: in.Instruction,
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
		Name: "amend_acceptance",
		Description: "Replace an in-flight task's acceptance contract when the original was wrong (e.g. it names " +
			"the wrong artifact path). You can NEVER waive acceptance — only correct it; the " +
			"engine still executes the checks itself. The worker is notified at its next turn.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in amendAcceptanceIn) (*mcp.CallToolResult, any, error) {
		activity("amend_acceptance " + in.TaskID)
		if err := rs.AmendAcceptance(in.TaskID, toChecks(in.Acceptance)); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Contract of %s amended; the engine will judge the task by the new checks.", in.TaskID), nil, nil
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
		Description: "Persist a short RUN-scoped note to yourself (strategy, dead ends, decisions). Your live " +
			"session remembers on its own, but it does not survive a restart — a rebuilt session knows only the " +
			"ledger, the conversation record, and these notes. For durable PROJECT facts use record_project_fact " +
			"instead.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recordNoteIn) (*mcp.CallToolResult, any, error) {
		activity("record_note")
		rs.AddNote(in.Text)
		return okf(rs, "Note recorded."), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "record_project_fact",
		Description: "Append one durable fact to PROJECT.md in the exchange directory — the cross-run memory of " +
			"the PROJECT itself: domain constraints, conventions, and user corrections (e.g. \"data X changes " +
			"quarterly, never poll it\", \"the user wants options staged for review before integration\"). When " +
			"the user corrects a wrong assumption, record the correction IMMEDIATELY. Workers see PROJECT.md in " +
			"every task prompt; future runs load it too.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recordProjectFactIn) (*mcp.CallToolResult, any, error) {
		activity("record_project_fact")
		if err := rs.RecordProjectFact(in.Text); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Project fact recorded in PROJECT.md."), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "conclude_feedback",
		Description: "Record the retrospective conclusion of THIS run after digesting a postmortem — stored on " +
			"the run, shown to the user, never injected anywhere. Calling again replaces it. Behavior rules the " +
			"retrospective yields go through propose_rules; durable project facts belong in " +
			"record_project_fact, definition flaws in propose_agent_amendment.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in concludeFeedbackIn) (*mcp.CallToolResult, any, error) {
		activity("conclude_feedback")
		if err := rs.ConcludeFeedback(in.Text); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Retrospective conclusion recorded (a record, not an injection — rules go through propose_rules)."), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "propose_rules",
		Description: "Propose changes to this workflow's standing behavior rules — new rules distilled from a " +
			"postmortem, or merges/rewrites/retirements of the existing ones (they are listed with ids in your " +
			"system prompt). Each rule is one short, self-contained directive; a rule overlapping an existing " +
			"one must carry replaces instead of duplicating it, and an empty text with replaces retires rules " +
			"outright. Everything lands PENDING: the user confirms each change, and only confirmed rules are " +
			"injected into future runs. If nothing would change future behavior, propose nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in proposeRulesIn) (*mcp.CallToolResult, any, error) {
		activity("propose_rules")
		n, err := rs.ProposeLessons(in.Rules)
		if err != nil {
			return toolErr("%v", err), nil, nil
		}
		if n == 0 {
			return okf(rs, "No rules proposed — nothing changes."), nil, nil
		}
		return okf(rs, "%d rule change(s) await the user's confirmation; nothing is injected until they approve.", n), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "propose_agent_amendment",
		Description: "Propose a revision of a pool agent's standing definition (its system prompt) for HUMAN review. " +
			"Use it when user feedback or a worker's observations show the agent's DEFINITION — not this run's " +
			"instruction — caused a failure that will recur. The proposal changes NOTHING now: it lands in a " +
			"review queue, a human applies or rejects it later. Continue the run with the agent as it is. Never " +
			"use this to work around a bad task spec — fix the spec instead.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in proposeAmendmentIn) (*mcp.CallToolResult, any, error) {
		activity("propose_agent_amendment " + in.Agent)
		if err := rs.ProposeAmendment(in.Agent, in.Rationale, in.ProposedPrompt); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return okf(rs, "Amendment for %q recorded for human review. It does not change the agent now — continue "+
			"the run with the agent as it is.", in.Agent), nil, nil
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
	// Tier aliases are legal here too; Validate then judges the resolved id
	// (unknown models, and the coordinator-only tier, are refused).
	if resolved, ok := model.ResolveModel(a.Model); ok && resolved != "" {
		a.Model = resolved
	}
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

// maxAmendmentPrompt bounds a proposed system prompt: a role definition, not a
// manual — and the human has to read the whole thing to approve it.
const maxAmendmentPrompt = 16000

// ProposeAmendment records a pending revision of a pool agent's definition.
// It writes a proposal record and nothing else — the agent is untouched until
// a human approves. That asymmetry is the design: agents surface evidence,
// humans change identities.
func (rs *RunSession) ProposeAmendment(agentName, rationale, proposed string) error {
	rationale = strings.TrimSpace(rationale)
	proposed = strings.TrimSpace(proposed)
	if rationale == "" || proposed == "" {
		return fmt.Errorf("rationale and proposed_prompt are both required")
	}
	if len(proposed) > maxAmendmentPrompt {
		return fmt.Errorf("proposed_prompt exceeds %d characters — an agent definition is a role, not a manual", maxAmendmentPrompt)
	}
	var target *model.Agent
	for _, a := range rs.PoolAgents() {
		if a.Name == agentName {
			target = a
			break
		}
	}
	if target == nil {
		return fmt.Errorf("agent %q is not in this run's pool", agentName)
	}
	if strings.TrimSpace(target.SystemPrompt) == proposed {
		return fmt.Errorf("the proposed prompt is identical to the current one — nothing to amend")
	}
	if rs.cfg.SaveAmendment == nil {
		return fmt.Errorf("amendment proposals are not wired for this run")
	}
	am := &model.Amendment{
		Agent: agentName, RunID: rs.run.ID, Rationale: rationale,
		Current: target.SystemPrompt, Proposed: proposed,
	}
	if err := rs.cfg.SaveAmendment(am); err != nil {
		return err
	}
	rs.event("amendment_proposed", "", fmt.Sprintf("coordinator proposed revising agent %q: %s", agentName, firstLine(rationale, 120)))
	return nil
}

// Bounds for proposed behavior rules: each is one norm the user must read to
// approve, and the approved set is injected into every future run's prompt.
// The per-call cap leaves room for a consolidation pass over a full set.
const (
	maxLessonRuleLen = 400
	maxRulesPerCall  = 8
)

// ProposeLessons records the coordinator's proposed rule changes — new rules,
// supersessions, retirements — as PENDING records only. Nothing is injected
// or removed until the user approves each one: agents surface lessons, the
// user decides what becomes standing instruction (the amendment asymmetry,
// applied to feedback). Replacement targets are validated and snapshotted by
// the store at save time, so a bad reference is refused here, loudly, while
// the coordinator can still fix it.
func (rs *RunSession) ProposeLessons(rules []proposedRuleIn) (int, error) {
	if rs.cfg.SaveLesson == nil {
		return 0, fmt.Errorf("lesson proposals are not wired for this run")
	}
	var clean []*model.Lesson
	for _, r := range rules {
		text := strings.TrimSpace(r.Text)
		if text == "" && len(r.Replaces) == 0 {
			continue
		}
		if len(text) > maxLessonRuleLen {
			return 0, fmt.Errorf("a rule exceeds %d characters — a behavior norm is one directive, not a recap", maxLessonRuleLen)
		}
		clean = append(clean, &model.Lesson{
			WorkflowID: rs.run.WorkflowID, RunID: rs.run.ID, Text: text, Replaces: r.Replaces,
		})
	}
	if len(clean) == 0 {
		return 0, nil
	}
	if len(clean) > maxRulesPerCall {
		return 0, fmt.Errorf("%d rules in one call — distill to at most %d; only what will change future behavior", len(clean), maxRulesPerCall)
	}
	for _, l := range clean {
		if err := rs.cfg.SaveLesson(l); err != nil {
			return 0, err
		}
	}
	first := clean[0].Text
	if first == "" {
		first = "retire " + strings.Join(clean[0].Replaces, ", ")
	}
	rs.event("lessons_proposed", "", fmt.Sprintf("coordinator proposed %d rule change(s) for user confirmation: %s",
		len(clean), firstLine(first, 120)))
	return len(clean), nil
}

// ---- worker tools ----

type reportProgressIn struct {
	Text string `json:"text" jsonschema:"what you have done and what is left — a short status line, never report content"`
}

type writeArtifactIn struct {
	Path    string `json:"path" jsonschema:"file path relative to the exchange directory, e.g. research-findings.md"`
	Content string `json:"content" jsonschema:"the file content (UTF-8); for a large document, write it in chunks with append=true"`
	Append  bool   `json:"append,omitempty" jsonschema:"append to the file instead of overwriting it"`
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

type reportResultIn struct {
	Status       string   `json:"status" jsonschema:"ok | error"`
	FailureKind  string   `json:"failure_kind,omitempty" jsonschema:"required when status=error: spec-unclear | blocked | missing-dependency | conflict"`
	Summary      string   `json:"summary" jsonschema:"SHORT: what you did and which files you delivered, or what stopped you — the substance lives in the artifacts, not here"`
	Artifacts    []string `json:"artifacts,omitempty" jsonschema:"paths relative to the exchange directory"`
	Observations string   `json:"observations,omitempty" jsonschema:"anything the contract did not cover that the coordinator should know: a spec that seems wrong, a coupling you noticed, a default you had to invent. Speaking up here is part of the job"`
}

// addWorkerTools registers the worker toolset. taskOf resolves the acting
// task at call time: fixed for a per-task worker session, ledger-bound for a
// resident (pair) session that serves many tasks. resident additionally
// grants record_project_fact — the implementer is the role that actually
// sees the code, so it is the first to learn durable project facts.
func (h *Hub) addWorkerTools(srv *mcp.Server, rs *RunSession, taskOf func() string, resident bool) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "write_artifact",
		Description: "Write a deliverable file into the run's exchange directory. Substantial text output — a " +
			"report, analysis, spec, review — is delivered as a Markdown file this way (or with your own file " +
			"tools), NEVER pasted into messages or the result summary. Works even if you have no file tools. " +
			"Overwrites unless append=true; large documents go in chunks.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in writeArtifactIn) (*mcp.CallToolResult, any, error) {
		taskID := taskOf()
		if taskID == "" {
			return toolErr("no task is currently bound to this session"), nil, nil
		}
		if err := rs.WriteArtifact(taskID, in.Path, in.Content, in.Append); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
			"Wrote %s (%d bytes) into the exchange directory. List it in your report_result artifacts.",
			in.Path, len(in.Content))}}}, nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "report_progress",
		Description: "Report progress mid-task so the coordinator can see where you are. Does not end your task " +
			"and does not require a reply.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in reportProgressIn) (*mcp.CallToolResult, any, error) {
		taskID := taskOf()
		if taskID == "" {
			return toolErr("no task is currently bound to this session"), nil, nil
		}
		if err := rs.Progress(taskID, in.Text); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Progress recorded."}}}, nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "report_result",
		Description: "Deliver your final work report: status, a SHORT summary, and the artifact list. Call this " +
			"when the task is done (or definitively stuck), then end your turn — the engine settles your task " +
			"from this report and runs the acceptance checks itself. Calling again replaces the earlier report.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in reportResultIn) (*mcp.CallToolResult, any, error) {
		taskID := taskOf()
		if taskID == "" {
			return toolErr("no task is currently bound to this session"), nil, nil
		}
		status := strings.ToLower(strings.TrimSpace(in.Status))
		if status != "ok" && status != "error" {
			return toolErr("status must be \"ok\" or \"error\", got %q", in.Status), nil, nil
		}
		kind := in.FailureKind
		if status == "error" && !model.ValidFailureKind(kind) {
			return toolErr("status \"error\" requires failure_kind: spec-unclear | blocked | missing-dependency | conflict"), nil, nil
		}
		if status == "ok" {
			kind = ""
		}
		if err := rs.ReportResult(taskID, ReportedResult{
			Status: status, FailureKind: kind, Summary: in.Summary, Artifacts: in.Artifacts,
			Observations: strings.TrimSpace(in.Observations),
		}); err != nil {
			return toolErr("%v", err), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Result recorded. " +
			"End your turn now — the engine settles your task from this report."}}}, nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "ask_coordinator",
		Description: "Ask the coordinator a question when the task is genuinely ambiguous and guessing would waste " +
			"the work. Blocks until it answers. Use this instead of inventing an assumption.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in askCoordinatorIn) (*mcp.CallToolResult, askCoordinatorOut, error) {
		taskID := taskOf()
		if taskID == "" {
			return toolErr("no task is currently bound to this session"), askCoordinatorOut{}, nil
		}
		answer, err := rs.Ask(ctx, taskID, in.Question)
		if err != nil {
			return toolErr("%v", err), askCoordinatorOut{}, nil
		}
		return nil, askCoordinatorOut{Answer: answer}, nil
	})

	if resident {
		mcp.AddTool(srv, &mcp.Tool{
			Name: "record_project_fact",
			Description: "Append one durable fact to PROJECT.md in the exchange directory — the cross-run memory " +
				"of the PROJECT: domain constraints, conventions, corrections. You are the role that actually reads " +
				"the code; when you learn something every future task must honor, record it.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in recordProjectFactIn) (*mcp.CallToolResult, any, error) {
			if err := rs.RecordProjectFact(in.Text); err != nil {
				return toolErr("%v", err), nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Project fact recorded in PROJECT.md."}}}, nil, nil
		})
	}

	if !rs.Budget().AllowPeerHandoff {
		return
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name: "handoff",
		Description: "Hand a sub-task to another agent directly. Use only for work that is genuinely outside your " +
			"own remit; the coordinator is notified and sees the result.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in handoffIn) (*mcp.CallToolResult, *delegateOut, error) {
		taskID := taskOf()
		if taskID == "" {
			return toolErr("no task is currently bound to this session"), nil, nil
		}
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
		taskID := taskOf()
		if taskID == "" {
			return toolErr("no task is currently bound to this session"), nil, nil
		}
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
