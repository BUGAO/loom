// Package model defines the three core entities of loom:
// Agent (a reusable executor in the shared pool), Workflow (an independent
// orchestrator with its own planner), and Run (one execution of a workflow,
// carrying the assembled DAG plan and per-node state).
package model

import "time"

// Agent is a reusable executor definition from the shared pool.
// Stored on disk as markdown with YAML-ish frontmatter; SystemPrompt is the body.
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Runtime names the execution runtime this agent runs on — how its
	// sessions are hosted, which is a property of the agent itself, not of a
	// run. Empty means DefaultRuntime. Runs only choose real vs dry-run.
	Runtime      string `json:"runtime,omitempty"`
	Model        string `json:"model"`     // sonnet | opus | haiku | full model id
	Tools        string `json:"tools"`     // comma-separated allowed tools, "" = none
	MaxTurns     int    `json:"max_turns"` // 0 = backend default
	SystemPrompt string `json:"system_prompt"`

	// Independent marks a fresh-context verifier (e.g. a reviewer). Its value
	// comes from NOT sharing the author's reasoning: loom refuses delegation
	// context hints for it and withholds upstream self-summaries, so its input
	// is only the requirement, the acceptance criteria, and the artifacts.
	Independent bool `json:"independent,omitempty"`

	// Skills are the names of the agent's private skills (directories under
	// home/.claude/skills). Populated at load time, not stored in agent.md.
	Skills []string `json:"skills,omitempty"`
}

// EffectiveRuntime normalizes the runtime field.
func (a *Agent) EffectiveRuntime() string {
	if a.Runtime == "" {
		return DefaultRuntime
	}
	return a.Runtime
}

// PlannerConfig is the per-workflow main agent that assembles the DAG.
type PlannerConfig struct {
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"` // extra guidance appended to the built-in planner prompt
	MaxNodes     int    `json:"max_nodes"`
}

// Workflow modes. static is the original plan-then-execute pipeline; dynamic
// hands the run to a coordinator agent that decomposes and delegates at
// runtime. The zero value is static, so pre-v2 workflow files keep working.
const (
	ModeStatic  = "static"
	ModeDynamic = "dynamic"
)

// Workflow is an independent orchestrator: its own planner, a subset of the
// shared agent pool, and execution policy.
type Workflow struct {
	ID                 string        `json:"id"`
	Name               string        `json:"name"`
	Description        string        `json:"description"`
	Mode               string        `json:"mode"` // static (default) | dynamic
	Planner            PlannerConfig `json:"planner"`
	AgentPool          []string      `json:"agent_pool"`           // agent names; empty = whole pool
	AllowAgentCreation bool          `json:"allow_agent_creation"` // planner may define new pool agents
	RequireApproval    bool          `json:"require_approval"`     // gate between plan and execution
	ReplanEnabled      bool          `json:"replan_enabled"`       // replan on node failure
	MaxReplans         int           `json:"max_replans"`
	MaxRetries         int           `json:"max_retries"`      // per-node retries before failure/replan
	NodeTimeoutSec     int           `json:"node_timeout_sec"` // 0 = default
	Parallelism        int           `json:"parallelism"`      // 0 = default

	// dynamic mode only
	Coordinator *CoordinatorConfig `json:"coordinator,omitempty"`
	Budget      *BudgetConfig      `json:"budget,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EffectiveMode normalizes the mode field: anything unset or unrecognized is
// static, so an old workflow file can never accidentally become dynamic.
func (w *Workflow) EffectiveMode() string {
	if w.Mode == ModeDynamic {
		return ModeDynamic
	}
	return ModeStatic
}

// CoordinatorConfig is the dynamic-mode main agent that decomposes, delegates,
// follows up, and delivers the verdict.
type CoordinatorConfig struct {
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"` // orchestration style / domain preferences
}

// Approval policies for dynamic runs.
const (
	ApprovalNone    = "none"
	ApprovalInitial = "initial" // the first batch of delegations needs sign-off
)

// BudgetConfig is the hard guardrail set for a dynamic run. In static mode the
// DAG's acyclicity guarantees termination structurally; dynamic mode has no
// such structure, so these limits are what stands between a coordinator and an
// unbounded loop. Every one of them is enforced in Go, never by prompt.
type BudgetConfig struct {
	MaxTasks           int    `json:"max_tasks"`
	MaxDelegationDepth int    `json:"max_delegation_depth"` // coordinator = 0, its delegations = 1
	MaxParallel        int    `json:"max_parallel"`
	TaskTimeoutSec     int    `json:"task_timeout_sec"`
	RunTimeoutSec      int    `json:"run_timeout_sec"` // hard wall clock for the whole run
	MaxTurnsPerTask    int    `json:"max_turns_per_task"`
	MaxReworksPerTask  int    `json:"max_reworks_per_task"` // rework attempts per failed task before forced escalation
	StallSec           int    `json:"stall_sec"`            // no ledger transition for this long → warn
	AllowAgentCreation bool   `json:"allow_agent_creation"`
	AllowPeerHandoff   bool   `json:"allow_peer_handoff"`
	ApprovalPolicy     string `json:"approval_policy"`
}

// Budget defaults, applied field-by-field so a partially filled form still
// gets sane limits for whatever it left blank.
func DefaultBudget() BudgetConfig {
	return BudgetConfig{
		MaxTasks:           30,
		MaxDelegationDepth: 3,
		MaxParallel:        3,
		TaskTimeoutSec:     1800,
		RunTimeoutSec:      7200,
		MaxTurnsPerTask:    6,
		MaxReworksPerTask:  2,
		StallSec:           600,
		ApprovalPolicy:     ApprovalInitial,
	}
}

// WithDefaults fills unset fields from DefaultBudget.
func (b BudgetConfig) WithDefaults() BudgetConfig {
	d := DefaultBudget()
	if b.MaxTasks <= 0 {
		b.MaxTasks = d.MaxTasks
	}
	if b.MaxDelegationDepth <= 0 {
		b.MaxDelegationDepth = d.MaxDelegationDepth
	}
	if b.MaxParallel <= 0 {
		b.MaxParallel = d.MaxParallel
	}
	if b.TaskTimeoutSec <= 0 {
		b.TaskTimeoutSec = d.TaskTimeoutSec
	}
	if b.RunTimeoutSec <= 0 {
		b.RunTimeoutSec = d.RunTimeoutSec
	}
	if b.MaxTurnsPerTask <= 0 {
		b.MaxTurnsPerTask = d.MaxTurnsPerTask
	}
	if b.MaxReworksPerTask <= 0 {
		b.MaxReworksPerTask = d.MaxReworksPerTask
	}
	if b.StallSec <= 0 {
		b.StallSec = d.StallSec
	}
	if b.ApprovalPolicy != ApprovalNone && b.ApprovalPolicy != ApprovalInitial {
		b.ApprovalPolicy = d.ApprovalPolicy
	}
	return b
}

// EffectiveBudget is the workflow's budget with defaults applied.
func (w *Workflow) EffectiveBudget() BudgetConfig {
	if w.Budget == nil {
		return DefaultBudget()
	}
	return w.Budget.WithDefaults()
}

// PlanNode is one node of an assembled DAG.
type PlanNode struct {
	ID          string   `json:"id"`
	Agent       string   `json:"agent"`
	Title       string   `json:"title"`
	Instruction string   `json:"instruction"`
	DependsOn   []string `json:"depends_on"`
}

// Plan is the DAG a planner assembled for one run. Agents holds new executor
// definitions the planner proposed (allowed only when the workflow enables
// agent creation); they are persisted into the shared pool before execution.
type Plan struct {
	Agents []Agent    `json:"agents,omitempty"`
	Nodes  []PlanNode `json:"nodes"`
}

// Run statuses.
const (
	RunPlanning         = "planning"
	RunAwaitingApproval = "awaiting_approval"
	RunRunning          = "running"
	RunReplanning       = "replanning"
	RunSucceeded        = "succeeded"
	RunFailed           = "failed"
	RunCanceled         = "canceled"
	RunInterrupted      = "interrupted" // server died mid-run
)

// Node statuses.
const (
	NodePending   = "pending"
	NodeRunning   = "running"
	NodeSucceeded = "succeeded"
	NodeFailed    = "failed"
	NodeSkipped   = "skipped"
	NodeCanceled  = "canceled"
)

// NodeState is the runtime state of one plan node. Full output lives in a
// file next to the run; only the parsed envelope is kept here.
type NodeState struct {
	Status     string     `json:"status"`
	Superseded bool       `json:"superseded,omitempty"` // replaced by a later replan generation
	Activity   string     `json:"activity,omitempty"`   // live: what the agent is doing right now
	Attempts   int        `json:"attempts"`
	Summary    string     `json:"summary"`
	Artifacts  []string   `json:"artifacts,omitempty"`
	Error      string     `json:"error,omitempty"`
	CostUSD    float64    `json:"cost_usd"`
	Usage      TokenUsage `json:"usage"`
	DurationMs int64      `json:"duration_ms"`
	StartedAt  time.Time  `json:"started_at,omitempty"`
	EndedAt    time.Time  `json:"ended_at,omitempty"`
}

// Failure kinds — the enumerated vocabulary a worker must use when it fails.
// Free-text failure reasons leave the coordinator guessing whether rework can
// possibly help; these four kinds are what its routing decisions key on.
const (
	FailSpecUnclear = "spec-unclear"       // the instruction itself is ambiguous or wrong
	FailBlocked     = "blocked"            // implementation obstacle; rework may help
	FailMissingDep  = "missing-dependency" // something upstream was never provided
	FailConflict    = "conflict"           // requirement conflicts with other work
	FailUnspecified = "unspecified"        // worker gave no valid kind; treated as NOT rework-eligible
)

// ValidFailureKind reports whether k is one of the worker-declarable kinds.
func ValidFailureKind(k string) bool {
	switch k {
	case FailSpecUnclear, FailBlocked, FailMissingDep, FailConflict:
		return true
	}
	return false
}

// Acceptance check kinds. Every check is machine-executable by the engine —
// the worker's envelope is a claim, never the verdict.
const (
	CheckArtifactExists   = "artifact_exists"   // Path exists (relative to the exchange dir)
	CheckArtifactContains = "artifact_contains" // Path's content matches Pattern (regexp)
	CheckCommand          = "command"           // Command exits 0 (run in the exchange dir)
)

// AcceptanceCheck is one machine-verifiable acceptance criterion, fixed at
// delegation time — before the worker starts, so the worker never gets to
// define its own passing bar.
type AcceptanceCheck struct {
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`        // artifact_* checks; relative to the exchange dir
	Pattern    string `json:"pattern,omitempty"`     // artifact_contains: regexp the content must match
	Command    string `json:"command,omitempty"`     // command: shell command, exit 0 = pass
	TimeoutSec int    `json:"timeout_sec,omitempty"` // command only; 0 = default
}

// CheckResult is the engine's record of executing one acceptance check.
type CheckResult struct {
	Check  AcceptanceCheck `json:"check"`
	Passed bool            `json:"passed"`
	Detail string          `json:"detail,omitempty"`
}

// Task statuses — the A2A task lifecycle, adopted verbatim rather than
// reinvented, so the ledger and the A2A gateway never need translating.
const (
	TaskSubmitted     = "submitted"
	TaskWorking       = "working"
	TaskInputRequired = "input-required"
	TaskCompleted     = "completed"
	TaskFailed        = "failed"
	TaskCanceled      = "canceled"
)

// TaskTerminal reports whether a task state is final.
func TaskTerminal(status string) bool {
	switch status {
	case TaskCompleted, TaskFailed, TaskCanceled:
		return true
	}
	return false
}

// Task message roles.
const (
	MsgInstruction = "instruction" // the delegating instruction
	MsgFollowup    = "followup"    // steering / an answer to a question
	MsgQuestion    = "question"    // worker asked upward
	MsgProgress    = "progress"    // worker reported mid-flight
	MsgResult      = "result"      // final envelope summary
	MsgPeer        = "peer"        // worker-to-worker exchange
)

// TaskMessage is one entry of a task's message history — the audit trail of
// everything said to and by the worker.
type TaskMessage struct {
	Ts   time.Time `json:"ts"`
	From string    `json:"from"` // "coordinator" | task id | "external" | agent name
	Role string    `json:"role"`
	Text string    `json:"text"`
}

// Task is the structural unit of a dynamic run: what a PlanNode is to static
// mode, except it comes into existence at runtime rather than being declared
// up front.
type Task struct {
	ID          string        `json:"id"`
	Agent       string        `json:"agent"`
	Title       string        `json:"title"`
	Instruction string        `json:"instruction"`
	Constraints string        `json:"constraints,omitempty"` // cross-domain constraints, fixed at delegation
	CreatedBy   string        `json:"created_by"`            // "coordinator" | "external" | upstream task id
	RetryOf     string        `json:"retry_of,omitempty"`    // failed task this one reworks
	Depth       int           `json:"depth"`
	Status      string        `json:"status"`
	Messages    []TaskMessage `json:"messages"`
	Summary     string        `json:"summary"`
	Artifacts   []string      `json:"artifacts,omitempty"`
	Error       string        `json:"error,omitempty"`
	FailureKind string        `json:"failure_kind,omitempty"` // set when Status == failed
	Activity    string        `json:"activity,omitempty"`

	// Acceptance is the machine-executable contract fixed at delegation time;
	// AcceptanceResults is what actually happened when the engine ran it.
	Acceptance        []AcceptanceCheck `json:"acceptance,omitempty"`
	AcceptanceResults []CheckResult     `json:"acceptance_results,omitempty"`

	Turns      int        `json:"turns"` // prompt turns spent on this task
	CostUSD    float64    `json:"cost_usd"`
	Usage      TokenUsage `json:"usage"`
	DurationMs int64      `json:"duration_ms"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  time.Time  `json:"started_at,omitempty"`
	EndedAt    time.Time  `json:"ended_at,omitempty"`
}

// ChatMessage is one turn of the user ↔ coordinator conversation.
type ChatMessage struct {
	Ts   time.Time `json:"ts"`
	From string    `json:"from"` // "user" | "coordinator"
	Text string    `json:"text"`
}

// Event is one entry of a run's append-only audit log.
type Event struct {
	Ts   time.Time `json:"ts"`
	Type string    `json:"type"` // run_status | node_status | replan | info | error
	Node string    `json:"node,omitempty"`
	Msg  string    `json:"msg"`
}

// Run is one execution of a workflow. static runs carry Plan/Nodes; dynamic
// runs carry Tasks (plus the coordinator's own live state).
type Run struct {
	ID           string                `json:"id"`
	WorkflowID   string                `json:"workflow_id"`
	WorkflowName string                `json:"workflow_name"`
	Goal         string                `json:"goal"`
	Backend      string                `json:"backend"` // display: runtime name, or "mock" for dry runs (legacy runs: acp|claude|mock)
	DryRun       bool                  `json:"dry_run,omitempty"`
	Mode         string                `json:"mode"`
	Status       string                `json:"status"`
	Plan         *Plan                 `json:"plan,omitempty"`
	Nodes        map[string]*NodeState `json:"nodes"`
	Events       []Event               `json:"events"`
	Replans      int                   `json:"replans"`
	CostUSD      float64               `json:"cost_usd"`
	Usage        TokenUsage            `json:"usage"`
	Error        string                `json:"error,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
	EndedAt      time.Time             `json:"ended_at,omitempty"`

	// dynamic mode
	Tasks       map[string]*Task  `json:"tasks,omitempty"`
	TaskOrder   []string          `json:"task_order,omitempty"` // creation order; maps have none
	Coordinator *CoordinatorState `json:"coordinator,omitempty"`
	Proposal    *Proposal         `json:"proposal,omitempty"` // pending approval payload

	// PlanApproved persists the release of the initial approval gate, so a
	// resumed run does not ask a human to approve a plan they already approved.
	PlanApproved bool `json:"plan_approved,omitempty"`
	// OutputName / OutputDir: dynamic runs deliver into a topic-named folder
	// under the output root (~/workflow-output by default). The coordinator
	// names it; an unnamed run gets an automatic name when the first task
	// dispatches, and the name freezes from then on.
	OutputName string `json:"output_name,omitempty"`
	OutputDir  string `json:"output_dir,omitempty"`
	// Chat is the running conversation between the user and the coordinator:
	// the goal is its first message, later user messages wake new decision
	// rounds, and each round's reply lands here. Persisted with the run, so a
	// resumed run keeps its conversation.
	Chat []ChatMessage `json:"chat,omitempty"`
	// CoordinatorNotes is the coordinator's externalized memory: bounded notes
	// it chose to persist across rounds. This — plus the task ledger — is ALL
	// the state a new round starts from; there is no accumulated conversation.
	CoordinatorNotes []string `json:"coordinator_notes,omitempty"`
}

// CoordinatorState is the live state of the run's coordinator, shown as a
// pinned card in the UI.
type CoordinatorState struct {
	Agent      string     `json:"agent"` // synthetic name, for cost attribution
	Model      string     `json:"model"`
	Status     string     `json:"status"`   // working | awaiting_approval | done | failed
	Activity   string     `json:"activity"` // last tool it called
	Decision   string     `json:"decision"` // last delegation/verdict, for the card
	Rounds     int        `json:"rounds"`   // decision rounds driven so far (each = a fresh context)
	CostUSD    float64    `json:"cost_usd"`
	Usage      TokenUsage `json:"usage"`
	DurationMs int64      `json:"duration_ms"`
}

// Proposal is what the coordinator submits at the initial approval gate.
type Proposal struct {
	Summary string         `json:"summary"`
	Tasks   []ProposedTask `json:"tasks"`
	Agents  []Agent        `json:"agents,omitempty"`
}

type ProposedTask struct {
	Agent string `json:"agent"`
	Title string `json:"title"`
	Why   string `json:"why"`
}

// EffectiveDryRun reports whether this run executes for real. Legacy runs
// recorded dry-run as backend=="mock", so both spellings count.
func (r *Run) EffectiveDryRun() bool {
	return r.DryRun || r.Backend == "mock"
}

// EffectiveMode normalizes a run's mode the same way Workflow does.
func (r *Run) EffectiveMode() string {
	if r.Mode == ModeDynamic {
		return ModeDynamic
	}
	return ModeStatic
}

// OrderedTasks returns tasks in creation order.
func (r *Run) OrderedTasks() []*Task {
	out := make([]*Task, 0, len(r.Tasks))
	for _, id := range r.TaskOrder {
		if t := r.Tasks[id]; t != nil {
			out = append(out, t)
		}
	}
	return out
}

// Terminal reports whether the run reached a final state.
func (r *Run) Terminal() bool {
	switch r.Status {
	case RunSucceeded, RunFailed, RunCanceled, RunInterrupted:
		return true
	}
	return false
}
