// Package model defines the three core entities of loom:
// Agent (a reusable executor in the shared pool), Workflow (an independent
// orchestrator with its own planner), and Run (one execution of a workflow,
// carrying the assembled DAG plan and per-node state).
package model

import (
	"strings"
	"time"
)

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
	// Triage decides each task's collaboration level from the main agent's
	// structured assessment (nil = defaults). Ignored when Coordinator.Level
	// pins the level.
	Triage *TriageConfig `json:"triage,omitempty"`
	// PairAgent names a pool agent as the run's RESIDENT IMPLEMENTER: all its
	// tasks run sequentially in one persistent session whose cwd is the
	// run workspace, so project understanding accumulates across tasks
	// the way a direct Claude Code session's would. Its session runs on the
	// agent's own default model (per-task tiering does not apply).
	PairAgent string `json:"pair_agent,omitempty"`
	// PairAgents generalizes PairAgent: every name here is a RESIDENT partner
	// of the main agent — its own persistent session in the workspace, tasks
	// to it queue on that session. An implementer and a reviewer can both be
	// resident. PairAgent (legacy, single) is folded in by EffectivePairAgents.
	PairAgents []string `json:"pair_agents,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EffectivePairAgents is the resident partner list: PairAgents plus the
// legacy PairAgent, deduplicated, in declaration order.
func (w *Workflow) EffectivePairAgents() []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	add(w.PairAgent)
	for _, n := range w.PairAgents {
		add(n)
	}
	return out
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
	// Tools is the main agent's own allowlist (the pilot's hands): the same
	// comma-separated capability tokens pool agents use. Empty = everything
	// (DefaultPilotTools). The hook gate enforces it per identity, and the
	// run's LEVEL decides when those hands may touch the workspace.
	Tools string `json:"tools,omitempty"`
	// Level pins the collaboration level every run of this workflow starts
	// at (solo | pair | orchestrate). Empty = the engine decides (triage once
	// it exists; until then DefaultLevel).
	Level string `json:"level,omitempty"`
}

// DefaultLevel is where a run opens when nothing pins it: the main agent
// works with its own hands, and the gates (tests, evidence, independent
// review) still decide what "done" means.
const DefaultLevel = LevelSolo

// EffectiveLevel is the workflow's pinned level, or the default; the second
// value names the source for the run's level log.
func (c *CoordinatorConfig) EffectiveLevel() (level, source string) {
	if c != nil {
		switch c.Level {
		case LevelSolo, LevelPair, LevelOrchestrate:
			return c.Level, "workflow"
		}
	}
	return DefaultLevel, "default"
}

// DefaultPilotTools is the main agent's allowlist when the workflow does not
// narrow it: every capability a Claude Code session has.
const DefaultPilotTools = "read,grep,glob,edit,write,bash,webfetch,websearch"

// EffectiveTools is the main agent's allowlist with the default applied.
func (c *CoordinatorConfig) EffectiveTools() string {
	if c == nil || strings.TrimSpace(c.Tools) == "" {
		return DefaultPilotTools
	}
	return c.Tools
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
	CheckArtifactExists   = "artifact_exists"   // Path exists (relative to the workspace)
	CheckArtifactContains = "artifact_contains" // Path's content matches Pattern (regexp)
	CheckCommand          = "command"           // Command exits 0 (run in the workspace)
)

// AcceptanceCheck is one machine-verifiable acceptance criterion, fixed at
// delegation time — before the worker starts, so the worker never gets to
// define its own passing bar.
type AcceptanceCheck struct {
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`        // artifact_* checks; relative to the workspace
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
	ID    string `json:"id"`
	Agent string `json:"agent"`
	// Model is the model this task's session runs on, resolved at delegation
	// time: the coordinator's per-task tier choice, or the agent's own default.
	Model       string `json:"model,omitempty"`
	Title       string `json:"title"`
	Instruction string `json:"instruction"`
	Constraints string `json:"constraints,omitempty"` // cross-domain constraints, fixed at delegation
	// Scope is the set of workspace paths (relative to the run workspace,
	// files or directory prefixes) this task OWNS while it is in flight: the
	// hook gate refuses writes there from anyone else, and refuses this
	// task's worker writes outside it. Empty = unscoped (legacy / free).
	Scope     []string `json:"scope,omitempty"`
	CreatedBy string   `json:"created_by"` // "coordinator" | "external" | upstream task id
	// SubRunID links a TEMPLATE task (Agent = "template:<workflow id>") to
	// the static run the engine drove for it; the template's DAG, nodes and
	// costs live on that run.
	SubRunID    string        `json:"sub_run_id,omitempty"`
	RetryOf     string        `json:"retry_of,omitempty"` // failed task this one reworks
	Depth       int           `json:"depth"`
	Status      string        `json:"status"`
	Messages    []TaskMessage `json:"messages"`
	Summary     string        `json:"summary"`
	Artifacts   []string      `json:"artifacts,omitempty"`
	Error       string        `json:"error,omitempty"`
	FailureKind string        `json:"failure_kind,omitempty"` // set when Status == failed
	Activity    string        `json:"activity,omitempty"`
	// Observations is the worker's dissent channel: things the contract did not
	// cover — a spec that seems wrong, a coupling the coordinator should know
	// about, a default the worker had to invent. "Completed with observations"
	// often means the spec, not the work, needs attention.
	Observations string `json:"observations,omitempty"`

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

// ChatTriage marks a system chat message carrying a triage verdict card:
// the level the engine chose for the task just assessed, and why.
const ChatTriage = "triage"

// ChatNotice marks a system chat message the engine posts for the user
// (e.g. the listener failing repeatedly).
const ChatNotice = "notice"

// ChatConsolidate marks a chat message as a rule-consolidation request: the
// coordinator's job in that activation is to tidy the workflow's standing
// rules via propose_rules (merge, rewrite, retire) — maintenance, not work,
// and not a verdict on this run's delivery.
const ChatConsolidate = "consolidate"

// ChatFeedback marks a chat message as post-run feedback: the coordinator is
// asked to DIGEST it (clarify, persist, distill), not to resume working.
const ChatFeedback = "feedback"

// ChatMessage is one turn of the user ↔ coordinator conversation.
type ChatMessage struct {
	Ts   time.Time `json:"ts"`
	From string    `json:"from"` // "user" | "coordinator" | "system"
	// Kind distinguishes special messages; "" is a normal chat turn,
	// ChatFeedback is a post-run verdict to digest, ChatTriage a level
	// verdict card, ChatNotice an engine notice shown inline.
	Kind string `json:"kind,omitempty"`
	Text string `json:"text"`
	// Images are upload filenames attached to this message. The bytes live as
	// files under the run's uploads directory, never inline in the run record.
	Images []string `json:"images,omitempty"`
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
	ID           string `json:"id"`
	WorkflowID   string `json:"workflow_id"`
	WorkflowName string `json:"workflow_name"`
	Goal         string `json:"goal"`
	Backend      string `json:"backend"` // display: runtime name, or "mock" for dry runs (legacy runs: acp|claude|mock)
	DryRun       bool   `json:"dry_run,omitempty"`
	// Workspace is THE directory of this run: the project the agents read and
	// modify AND where every deliverable lands — one folder, chosen by the
	// user in the UI when the run started (default: the output root,
	// ~/workflow-output). Never empty on a run started after the workspace
	// unification; the engine fills it for legacy runs on their next
	// activation (see Engine.runWorkspace).
	Workspace string                `json:"workspace,omitempty"`
	Mode      string                `json:"mode"`
	Status    string                `json:"status"`
	Plan      *Plan                 `json:"plan,omitempty"`
	Nodes     map[string]*NodeState `json:"nodes"`
	Events    []Event               `json:"events"`
	Replans   int                   `json:"replans"`
	CostUSD   float64               `json:"cost_usd"`
	Usage     TokenUsage            `json:"usage"`
	Error     string                `json:"error,omitempty"`
	CreatedAt time.Time             `json:"created_at"`
	EndedAt   time.Time             `json:"ended_at,omitempty"`

	// dynamic mode
	Tasks       map[string]*Task  `json:"tasks,omitempty"`
	TaskOrder   []string          `json:"task_order,omitempty"` // creation order; maps have none
	Coordinator *CoordinatorState `json:"coordinator,omitempty"`
	Proposal    *Proposal         `json:"proposal,omitempty"` // pending approval payload

	// PlanApproved persists the release of the initial approval gate, so a
	// resumed run does not ask a human to approve a plan they already approved.
	PlanApproved bool `json:"plan_approved,omitempty"`
	// OutputName / OutputDir are LEGACY (pre-unification): dynamic runs used
	// to deliver into a coordinator-named folder under the output root while
	// Workspace pointed at a separate project. New runs never set them; on
	// an old run OutputDir is where the work is, so it becomes the workspace
	// when that run is next activated.
	OutputName string `json:"output_name,omitempty"`
	OutputDir  string `json:"output_dir,omitempty"`
	// ParentRunID / ParentTaskID mark a static run driven as a TEMPLATE TASK
	// of a dynamic run (see Task.SubRunID); the UI links the two.
	ParentRunID  string `json:"parent_run_id,omitempty"`
	ParentTaskID string `json:"parent_task_id,omitempty"`
	// Chat is the running conversation between the user and the coordinator:
	// the goal is its first message, later user messages wake new decision
	// rounds, and each round's reply lands here. Persisted with the run, so a
	// resumed run keeps its conversation.
	Chat []ChatMessage `json:"chat,omitempty"`
	// GoalImages are upload filenames attached to the goal itself (a run
	// started from a message with images). Delivered to the coordinator's
	// first round of each activation, alongside the goal text.
	GoalImages []string `json:"goal_images,omitempty"`
	// CoordinatorNotes is the coordinator's externalized memory: bounded notes
	// it chose to persist across rounds. This — plus the task ledger — is ALL
	// the state a new round starts from; there is no accumulated conversation.
	CoordinatorNotes []string `json:"coordinator_notes,omitempty"`

	// Evidence is the run's DEFINITION OF DONE: the observable proofs the
	// coordinator declared, before delegating anything, that would show the
	// goal is met (e.g. "a digest email arrives in the user's inbox"). No task
	// can be created until it exists, and finish_run(succeeded) is refused
	// until every item is reported met with a stated verification. Items
	// that need something only the user can supply force an ask_user first.
	Evidence []GoalEvidence `json:"evidence,omitempty"`

	// Level is the run's current collaboration level — solo | pair |
	// orchestrate — the engine's decision (triage, user override, or the
	// workflow's pinned floor) about HOW the main agent may work: alone with
	// its own hands, beside resident partner sessions, or only by delegating.
	// It is enforced by the hook gate, not by prompting. Empty on legacy runs
	// (treated as orchestrate: the old coordinator had no hands anyway).
	Level       string        `json:"level,omitempty"`
	LevelSource string        `json:"level_source,omitempty"` // triage | user | workflow | default
	LevelLog    []LevelChange `json:"level_log,omitempty"`
	// Writes is the hook gate's attribution record: every file the main agent
	// or a worker wrote through Edit/Write (exact path), and every shell
	// command they ran (time window only — a shell's writes are not
	// observable). The review gate reads it to know whose change was last.
	Writes []WriteRecord `json:"writes,omitempty"`
	// Assessments is every assess_task the main agent filed (newest last)
	// with triage's verdict on each.
	Assessments []TaskAssessment `json:"assessments,omitempty"`

	// Feedback is this run's RETROSPECTIVE RECORD — the coordinator's digested
	// conclusion of the user's postmortem (written via conclude_feedback; the
	// raw words stay in Chat). It is stored and shown, never injected: what
	// future runs open with are the workflow's user-APPROVED Lessons, which the
	// coordinator proposes from this retrospective. Static and dry runs, which
	// have no conversational agent, store the user's text verbatim here.
	Feedback   string    `json:"feedback,omitempty"`
	FeedbackAt time.Time `json:"feedback_at,omitempty"`
}

// GoalEvidence is one observable proof that the goal is met — declared up
// front, settled at finish. Claim is what an outsider could check; How is the
// verification recorded at finish (a task id, a command output, an inspected
// file); NeedsFromUser names what the user must supply for the claim to be
// checkable at all (credentials, an account), empty when nothing is needed.
type GoalEvidence struct {
	Claim         string `json:"claim"`
	NeedsFromUser string `json:"needs_from_user,omitempty"`
	Met           bool   `json:"met,omitempty"`
	How           string `json:"how,omitempty"`
}

// TemplateAgentPrefix marks a ledger task whose "agent" is a static
// workflow template run by the engine rather than a pool agent.
const TemplateAgentPrefix = "template:"

// Collaboration levels. The zero value is deliberately NOT a level: a legacy
// run (no level) behaves as orchestrate, which is what its coordinator was.
const (
	LevelSolo        = "solo"        // the main agent works with its own hands
	LevelPair        = "pair"        // plus resident partner sessions the engine opens
	LevelOrchestrate = "orchestrate" // the main agent may only delegate; its writes are refused
)

// LevelChange is one entry of a run's level history.
type LevelChange struct {
	Ts     time.Time `json:"ts"`
	Level  string    `json:"level"`
	Source string    `json:"source"` // triage | user | workflow | default | pilot
	Reason string    `json:"reason,omitempty"`
}

// TriageConfig is how the engine turns an assessment into a level. Zero
// fields take the defaults (DefaultTriage), so a partially filled form stays
// sane; PairOffForCode is the one opt-out flag (zero = pair on code changes).
type TriageConfig struct {
	// Any of these reached → orchestrate.
	OrchestrateSteps    int `json:"orchestrate_steps"`
	OrchestrateBranches int `json:"orchestrate_branches"` // independent parallel branches
	OrchestrateRoles    int `json:"orchestrate_roles"`    // distinct roles needed
	OrchestrateFiles    int `json:"orchestrate_files"`    // estimated files touched
	// Code changes lift a task to at least pair (when the workflow has
	// resident partners or an independent agent) unless opted out.
	PairOffForCode bool `json:"pair_off_for_code,omitempty"`
	// Mid-run re-assessment triggers: the main agent's own distinct code
	// files written since the last assessment, and acceptance test failures
	// since the last assessment.
	ReassessFiles        int `json:"reassess_files"`
	ReassessTestFailures int `json:"reassess_test_failures"`
}

// DefaultTriage is the engine's thresholds.
func DefaultTriage() TriageConfig {
	return TriageConfig{
		OrchestrateSteps: 6, OrchestrateBranches: 2, OrchestrateRoles: 2, OrchestrateFiles: 8,
		ReassessFiles: 8, ReassessTestFailures: 3,
	}
}

// EffectiveTriage applies defaults field by field.
func (w *Workflow) EffectiveTriage() TriageConfig {
	d := DefaultTriage()
	if w == nil || w.Triage == nil {
		return d
	}
	t := *w.Triage
	if t.OrchestrateSteps <= 0 {
		t.OrchestrateSteps = d.OrchestrateSteps
	}
	if t.OrchestrateBranches <= 0 {
		t.OrchestrateBranches = d.OrchestrateBranches
	}
	if t.OrchestrateRoles <= 0 {
		t.OrchestrateRoles = d.OrchestrateRoles
	}
	if t.OrchestrateFiles <= 0 {
		t.OrchestrateFiles = d.OrchestrateFiles
	}
	if t.ReassessFiles <= 0 {
		t.ReassessFiles = d.ReassessFiles
	}
	if t.ReassessTestFailures <= 0 {
		t.ReassessTestFailures = d.ReassessTestFailures
	}
	return t
}

// TaskAssessment is the main agent's structured read of a task BEFORE it
// touches anything — the input triage turns into a level. It is the
// engine's, not the user's, so it stays terse and machine-shaped.
type TaskAssessment struct {
	Ts               time.Time `json:"ts"`
	Summary          string    `json:"summary"`
	Steps            int       `json:"steps"`
	Modules          []string  `json:"modules,omitempty"`
	ParallelBranches int       `json:"parallel_branches"`
	Roles            []string  `json:"roles,omitempty"`
	ChangesCode      bool      `json:"changes_code"`
	EstFiles         int       `json:"est_files"`
	// Verdict is what triage concluded from it.
	Level   string   `json:"level"`
	Reasons []string `json:"reasons,omitempty"`
	Applied bool     `json:"applied"` // false when a user override kept the level
}

// LevelRank orders levels: solo < pair < orchestrate. Unknown → -1.
func LevelRank(level string) int {
	switch level {
	case LevelSolo:
		return 0
	case LevelPair:
		return 1
	case LevelOrchestrate:
		return 2
	}
	return -1
}

// WriteRecord is one attributed write (or shell window) seen by the hook gate.
type WriteRecord struct {
	Ts      time.Time `json:"ts"`
	By      string    `json:"by"`                // "coordinator" | "pair" | task id
	Tool    string    `json:"tool"`              // Edit | Write | MultiEdit | NotebookEdit | Bash
	Path    string    `json:"path,omitempty"`    // workspace-relative when under the workspace, else absolute
	Command string    `json:"command,omitempty"` // Bash only, truncated
}

// CoordinatorState is the live state of the run's coordinator, shown as a
// pinned card in the UI.
type CoordinatorState struct {
	Agent    string `json:"agent"` // synthetic name, for cost attribution
	Model    string `json:"model"`
	Status   string `json:"status"`   // working | awaiting_user | awaiting_approval | done | failed
	Activity string `json:"activity"` // last tool it called
	// Draft is the reply text of the round in progress, streamed as it is
	// produced; committed to Chat when the round ends.
	Draft string `json:"draft,omitempty"`
	// Trace is the round in progress's tool activity, newest last (capped):
	// what the main agent is doing with its hands, live.
	Trace      []string   `json:"trace,omitempty"`
	Decision   string     `json:"decision"` // last delegation/verdict, for the card
	Rounds     int        `json:"rounds"`   // decision rounds driven so far (each = a fresh context)
	CostUSD    float64    `json:"cost_usd"`
	Usage      TokenUsage `json:"usage"`
	DurationMs int64      `json:"duration_ms"`
}

// Amendment statuses.
const (
	AmendmentPending  = "pending"
	AmendmentApproved = "approved"
	AmendmentRejected = "rejected"
)

// Amendment is a proposed revision of an agent's standing definition (its
// system prompt), created by a run's coordinator when feedback or observations
// show the DEFINITION — not one run's spec — caused a failure. It is data
// until a human approves it: agents can never change an identity themselves
// (path-deny stands), and approval is the only path from proposal to effect.
type Amendment struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	RunID     string `json:"run_id,omitempty"` // provenance: the run whose evidence motivated this
	Rationale string `json:"rationale"`
	// Current snapshots the agent's system prompt at proposal time; approval
	// refuses to apply over a prompt that changed since (a stale proposal was
	// reasoned against text that no longer exists).
	Current   string    `json:"current"`
	Proposed  string    `json:"proposed"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	DecidedAt time.Time `json:"decided_at,omitempty"`
}

// Lesson is one concrete behavior rule distilled from a run's retrospective —
// "do X", "avoid Y" — proposed by the coordinator (or written by the user) at
// workflow scope. It is data until the user approves it: only approved lessons
// are injected into the opening prompt of the workflow's future runs. The
// retrospective narrative itself stays on Run.Feedback and is never injected.
//
// A proposal may SUPERSEDE existing approved rules instead of piling on a
// near-duplicate: Replaces names them, and approval swaps them out atomically.
// An empty Text with a non-empty Replaces is a pure retirement — approving it
// removes the targets and injects nothing. ReplacedTexts snapshots the targets
// at proposal time; approval over a target the user has since edited is
// refused (same stale rule as amendments — the proposal was reasoned against
// text that no longer exists).
type Lesson struct {
	ID            string    `json:"id"`
	WorkflowID    string    `json:"workflow_id"`
	RunID         string    `json:"run_id,omitempty"` // provenance: the retrospective that produced it
	Text          string    `json:"text"`
	Replaces      []string  `json:"replaces,omitempty"`       // ids of approved rules this supersedes
	ReplacedTexts []string  `json:"replaced_texts,omitempty"` // their texts at proposal time, parallel to Replaces
	Status        string    `json:"status"`                   // pending | approved | rejected (Amendment* constants)
	CreatedAt     time.Time `json:"created_at"`
	DecidedAt     time.Time `json:"decided_at,omitempty"`
}

// Retirement reports whether this proposal only removes rules: approving it
// deletes the Replaces targets and adds nothing to the injection set.
func (l *Lesson) Retirement() bool { return l.Text == "" && len(l.Replaces) > 0 }

// Proposal is what the coordinator submits at the initial approval gate.
type Proposal struct {
	Summary string         `json:"summary"`
	Tasks   []ProposedTask `json:"tasks"`
	Agents  []Agent        `json:"agents,omitempty"`
}

type ProposedTask struct {
	Agent string `json:"agent"`
	Model string `json:"model,omitempty"` // intended tier/model; display only, the delegate call decides
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
