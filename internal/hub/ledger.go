// Package hub is the dynamic-mode control plane: a per-run task ledger with an
// A2A lifecycle, a policy engine that enforces the run's budget in Go, and the
// two protocol surfaces agents reach it through (MCP for the agents themselves,
// A2A JSON-RPC for anything outside loom).
//
// The ledger is the single source of truth. Delegation from the coordinator,
// peer handoff between workers, and external A2A calls all land in the same
// table and go through the same guards — there is no side door that skips the
// budget or the audit trail.
package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"loom/internal/model"
	"loom/internal/store"
)

// Executor is the engine's half of the contract: the hub decides *that* a task
// should run, the engine knows *how* to run one. StartTask must not block —
// the engine spawns its own goroutine.
type Executor interface {
	StartTask(rs *RunSession, taskID string)
	CancelTask(rs *RunSession, taskID string)
}

// RunConfig wires one dynamic run.
type RunConfig struct {
	Run      *model.Run
	Workflow *model.Workflow
	Pool     []*model.Agent
	// Workspace is the internal fallback exchange directory; once the run's
	// output folder is named (by the coordinator or the auto-freeze), the
	// output folder IS the exchange directory.
	Workspace string
	// OutputRoot is where named output folders live (~/workflow-output).
	OutputRoot string
	Exec       Executor

	// OnChange persists and publishes a run snapshot. It is called with the
	// session lock held, so it must not call back into the RunSession.
	OnChange func(*model.Run)
	// OnCost records a priced unit into the global ledger.
	OnCost func(store.CostEntry)
	// SaveAgent persists a coordinator-created agent into the shared pool.
	SaveAgent func(*model.Agent) error
}

// ErrApprovalPending is returned when work is requested before a human has
// released the run's initial approval gate. Callers that can do something
// about it (the coordinator) match on it to add role-specific guidance.
var ErrApprovalPending = errors.New("approval pending")

// CreatedByExternal marks tasks submitted by outside A2A clients.
const CreatedByExternal = "external"

// reworkRootLocked follows a rework chain back to the original failed task, so
// rework counting and kind routing always key on the root, not the latest link.
func reworkRootLocked(run *model.Run, id string) string {
	for {
		t := run.Tasks[id]
		if t == nil || t.RetryOf == "" {
			return id
		}
		id = t.RetryOf
	}
}

// reworkAdvice tells a coordinator what "back to the planning layer" means for
// each non-reworkable failure kind.
func reworkAdvice(kind string) string {
	switch kind {
	case model.FailSpecUnclear:
		return "The instruction itself was the problem: rewrite it (or the acceptance criteria) before delegating again."
	case model.FailMissingDep:
		return "Something upstream was never provided: produce the missing dependency first, then delegate."
	case model.FailConflict:
		return "The requirement conflicts with other work: resolve the conflict in the plan before anything is redone."
	default:
		return "The failure was not classified as a work obstacle, so repeating the work cannot help."
	}
}

// Verdict is how a dynamic run ends: the coordinator's own declaration.
type Verdict struct {
	Status    string // succeeded | failed
	Summary   string
	Artifacts []string
}

// RunSession is the ledger and policy engine for one dynamic run.
type RunSession struct {
	hub       *Hub
	cfg       RunConfig
	budget    model.BudgetConfig
	workspace string

	mu   sync.Mutex
	run  *model.Run
	pool map[string]*model.Agent

	// dispatched marks tasks handed to the executor. A task occupies a session
	// slot from dispatch, not from the moment it reports working: the executor
	// starts asynchronously, and counting only by status would let a second
	// schedule() pass re-dispatch the same task and overshoot MaxParallel.
	dispatched map[string]bool

	ctrl map[string]*taskCtrl

	// approval gate. Deliberately channel-free: the human may take minutes or
	// hours, and a tool call blocked that long gets killed by the MCP client's
	// timeout — with the approval then lost into a dead channel (observed in a
	// real run). The gate is plain state; the decision arrives as an injected
	// notice that wakes the next round.
	approvalNeeded  bool
	approvalDone    bool
	approvalPending bool
	rejectedReason  string

	// coordinator liveness / stall detection
	lastTransition time.Time
	pendingNotice  string

	// outputFrozen pins the exchange directory once the first task has been
	// dispatched: prompts and contracts reference the path from then on.
	outputFrozen bool
	// outputBaseName is the topic name the coordinator last passed to
	// SetOutputName, BEFORE collision suffixing. Repeating the same name
	// (name_output, then propose_plan carrying output_name again) must be a
	// no-op — re-claiming used to collide with the run's own first claim and
	// silently move the run to "<topic>-2", orphaning the first directory.
	outputBaseName string

	// inspections counts the coordinator's audited reads of run deliverables.
	// finish_run(succeeded) is refused while it is zero and artifacts exist:
	// machine checks decide "correct", but only actually reading the output
	// can catch "correct but off-course".
	inspections int

	// pendingChat holds user messages not yet delivered to a coordinator
	// round. A non-empty queue wakes AwaitRound, and the next round's prompt
	// carries the drained messages.
	pendingChat []model.ChatMessage

	// seq increments on every ledger transition; the round driver compares it
	// across rounds to detect a coordinator that acts without effect.
	seq uint64

	// changed is closed and replaced on every ledger transition; waiters take a
	// reference before checking state, so no transition can be missed.
	changed chan struct{}

	verdict  *Verdict
	finished chan struct{}
	closed   bool
	cancel   context.CancelFunc
}

// taskCtrl is the per-task control block: how the hub talks to a running
// worker without reaching into the engine's goroutine.
type taskCtrl struct {
	// inbox holds steering messages for a working task. They are delivered at
	// the next turn boundary, because ACP has no way to interrupt a turn in
	// flight (see docs/DECISIONS-v2.md D4).
	inbox []string
	// answer unblocks a worker parked in ask_coordinator. Buffered so the
	// answering side never blocks on a worker that gave up.
	answer chan string
	// reported is the work report delivered through report_result, held until
	// the engine collects it at the turn boundary.
	reported *ReportedResult
	cancel   context.CancelFunc
}

// ReportedResult is a worker's final work report, delivered structurally
// through the report_result tool instead of being parsed out of reply text.
type ReportedResult struct {
	Status      string
	FailureKind string
	Summary     string
	Artifacts   []string
}

func (rs *RunSession) newTaskCtrl() *taskCtrl {
	return &taskCtrl{answer: make(chan string, 1)}
}

// ---- lifecycle ----

// Run returns the live run object. Callers must not mutate it.
func (rs *RunSession) Run() *model.Run { return rs.run }

// Workspace is the run's shared exchange directory: the named output folder
// once one exists, the internal fallback until then.
func (rs *RunSession) Workspace() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.run.OutputDir != "" {
		return rs.run.OutputDir
	}
	return rs.workspace
}

// outputNameRe is the shape of a deliverable folder name: short, kebab, no
// path tricks.
var outputNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// SetOutputName names the run's deliverable folder under the output root.
// Refused once frozen (first task dispatched): workers already received the
// old path in their prompts, and a contract's artifact paths must not move
// under a running task.
func (rs *RunSession) SetOutputName(name string) error {
	name = strings.TrimSpace(name)
	if !outputNameRe.MatchString(name) {
		return fmt.Errorf("output name %q is invalid: use a short kebab-case topic name, e.g. \"trading-health-check\"", name)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.outputFrozen {
		return fmt.Errorf("the output folder is already %s and tasks have been dispatched against it; it cannot be renamed", rs.run.OutputDir)
	}
	if rs.run.OutputDir != "" && rs.outputBaseName == name {
		return nil // same topic, same folder — nothing to do
	}
	if rs.cfg.OutputRoot == "" {
		return fmt.Errorf("no output root is configured for this run")
	}
	dir, finalName, err := claimOutputDir(rs.cfg.OutputRoot, name)
	if err != nil {
		return err
	}
	rs.releaseOutputDirLocked(dir)
	rs.outputBaseName = name
	rs.run.OutputName = finalName
	rs.run.OutputDir = dir
	rs.appendEventLocked("info", "", "output folder named: "+dir)
	rs.notifyLocked()
	return nil
}

// releaseOutputDirLocked drops the run's previous pre-freeze output claim when
// it is being replaced by newDir. Only an EMPTY directory is removed —
// os.Remove refuses non-empty ones, which is exactly the safety wanted: a
// rename never deletes work. Must hold rs.mu.
func (rs *RunSession) releaseOutputDirLocked(newDir string) {
	if old := rs.run.OutputDir; old != "" && old != newDir {
		if err := os.Remove(old); err == nil {
			rs.appendEventLocked("info", "", "released empty output folder: "+old)
		}
	}
}

// claimOutputDir creates root/name, suffixing -2, -3… when the name is taken.
// The claim is os.Mkdir itself — atomic at the filesystem, so two sessions (or
// two loom processes) racing for the same topic cannot both win: the loser
// sees EEXIST and moves to the next suffix.
func claimOutputDir(root, name string) (dir, finalName string, err error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}
	for i := 0; i < 50; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", name, i+1)
		}
		dir := filepath.Join(root, candidate)
		switch err := os.Mkdir(dir, 0o755); {
		case err == nil:
			return dir, candidate, nil
		case errors.Is(err, os.ErrExist):
			continue // taken; try the next suffix
		default:
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("could not find a free output folder name for %q", name)
}

// freezeOutputLocked pins the exchange directory before the first dispatch,
// auto-naming when the coordinator never did. Must hold rs.mu.
func (rs *RunSession) freezeOutputLocked() {
	if rs.outputFrozen {
		return
	}
	if rs.run.OutputDir == "" && rs.cfg.OutputRoot != "" {
		auto := fmt.Sprintf("%s-%s", time.Now().Format("0102"), shortRunID(rs.run.ID))
		if dir, finalName, err := claimOutputDir(rs.cfg.OutputRoot, auto); err == nil {
			rs.run.OutputName = finalName
			rs.run.OutputDir = dir
			rs.appendEventLocked("info", "", "output folder auto-named: "+dir)
		} else {
			rs.appendEventLocked("error", "", "output folder could not be created ("+err.Error()+"); using the internal exchange directory")
		}
	}
	rs.outputFrozen = true
}

func shortRunID(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}

// Budget is the effective budget for this run.
func (rs *RunSession) Budget() model.BudgetConfig { return rs.budget }

// Done is closed once the run reaches a verdict or is torn down.
func (rs *RunSession) Done() <-chan struct{} { return rs.finished }

// Verdict returns the coordinator's declared outcome, or nil if it never gave one.
func (rs *RunSession) Verdict() *Verdict {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.verdict
}

// Close tears the session down: every non-terminal task is canceled and the
// hub stops routing to this run.
func (rs *RunSession) Close() {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return
	}
	rs.closed = true
	var toCancel []string
	for _, t := range rs.run.Tasks {
		if !model.TaskTerminal(t.Status) {
			toCancel = append(toCancel, t.ID)
		}
	}
	rs.mu.Unlock()

	for _, id := range toCancel {
		// Once the coordinator has delivered its verdict, anything still in
		// flight is work nobody will consume — including tasks an external
		// A2A client injected. Killing it bounds the run; saying so plainly
		// is what keeps that from looking like a crash.
		rs.cancelTask(id, "run ended before this task finished; no one remained to consume its result")
	}
	rs.mu.Lock()
	rs.notifyLocked()
	rs.mu.Unlock()
	close(rs.finished)
	rs.hub.removeRun(rs.run.ID)
}

// ---- change notification ----

// waitChange returns a channel closed on the next ledger transition. Take it
// *before* reading state, then select on it, and no transition can slip by.
func (rs *RunSession) waitChange() <-chan struct{} {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.changed
}

// notifyLocked wakes every waiter and republishes. Must hold rs.mu.
func (rs *RunSession) notifyLocked() {
	rs.lastTransition = time.Now()
	rs.seq++
	close(rs.changed)
	rs.changed = make(chan struct{})
	if rs.cfg.OnChange != nil {
		rs.cfg.OnChange(rs.run)
	}
}

// Seq returns the ledger's transition counter.
func (rs *RunSession) Seq() uint64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.seq
}

// event appends to the run's audit log under the session lock and republishes.
// The session lock is the ONLY lock guarding a dynamic run's state — events,
// tasks, chat, coordinator card, costs — so every mutation and every marshal
// happens under it, and nothing can tear.
func (rs *RunSession) event(typ, taskID, msg string) {
	rs.mu.Lock()
	rs.appendEventLocked(typ, taskID, msg)
	rs.mu.Unlock()
}

func (rs *RunSession) appendEventLocked(typ, taskID, msg string) {
	rs.run.Events = append(rs.run.Events, model.Event{Ts: time.Now(), Type: typ, Node: taskID, Msg: msg})
	if rs.cfg.OnChange != nil {
		rs.cfg.OnChange(rs.run)
	}
}

// AppendEvent is the engine's path into the audit log while the run is live.
func (rs *RunSession) AppendEvent(typ, taskID, msg string) {
	rs.event(typ, taskID, msg)
}

// UpdateCoordinator mutates the coordinator card under the session lock.
func (rs *RunSession) UpdateCoordinator(fn func(*model.CoordinatorState)) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.run.Coordinator != nil {
		fn(rs.run.Coordinator)
		if rs.cfg.OnChange != nil {
			rs.cfg.OnChange(rs.run)
		}
	}
}

// RecordCoordinatorCost prices one coordinator round into the run and the
// global cost ledger — same lock as every other run mutation.
func (rs *RunSession) RecordCoordinatorCost(agent, modelID string, usage model.TokenUsage, cost float64, durMs int64) {
	rs.mu.Lock()
	if c := rs.run.Coordinator; c != nil {
		c.CostUSD += cost
		c.Usage.Add(usage)
		c.DurationMs += durMs
	}
	rs.run.CostUSD += cost
	rs.run.Usage.Add(usage)
	if rs.cfg.OnChange != nil {
		rs.cfg.OnChange(rs.run)
	}
	rs.mu.Unlock()

	if rs.cfg.OnCost != nil && (cost != 0 || !usage.Empty()) {
		rs.cfg.OnCost(store.CostEntry{
			WorkflowID: rs.run.WorkflowID, RunID: rs.run.ID, Unit: "coordinator", UnitID: "coordinator",
			Agent: agent, Model: modelID, Usage: usage, CostUSD: cost,
		})
	}
}

// ---- pool ----

// PoolAgents lists the agents this run may delegate to.
func (rs *RunSession) PoolAgents() []*model.Agent {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]*model.Agent, 0, len(rs.pool))
	for _, a := range rs.pool {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (rs *RunSession) agent(name string) *model.Agent {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.pool[name]
}

// AddAgent extends the run's pool with a newly created agent.
func (rs *RunSession) AddAgent(a *model.Agent) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.pool[a.Name] = a
}

// ---- task creation ----

// DelegateRequest is one request to put work on the ledger, from whatever
// source. Every path — coordinator delegate, peer handoff, external A2A —
// funnels through Delegate so no caller can dodge the budget.
type DelegateRequest struct {
	Agent string
	// Model is the delegator's per-task tier choice (haiku/sonnet/opus/fable
	// or a full catalog id). Empty means the agent's own default model.
	Model       string
	Title       string
	Instruction string
	Constraints string // cross-domain constraints the worker cannot infer; required for internal callers
	ContextHint string
	CreatedBy   string // "coordinator" | "external" | parent task id
	Depth       int

	// Acceptance is the machine-checkable passing bar, fixed before the worker
	// starts. Required for internal callers; external A2A submissions get a
	// default artifact-agnostic contract (the boundary hardening for external
	// callers is tracked separately).
	Acceptance []model.AcceptanceCheck

	// RetryOf marks this delegation as rework of a failed task. Rework is only
	// legal when that task failed as "blocked"; every other failure kind means
	// the plan, not the worker, needs to change.
	RetryOf string
}

// Delegate creates a task after checking every applicable budget rule. The
// errors it returns are written to be *read by a model*: they say what the
// limit was and what to do instead, because a coordinator that understands the
// refusal converges, and one that doesn't retries forever.
func (rs *RunSession) Delegate(req DelegateRequest) (*model.Task, error) {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return nil, fmt.Errorf("this run has ended; no further tasks can be created")
	}
	if rs.approvalNeeded && !rs.approvalDone {
		rs.mu.Unlock()
		// Role-neutral wording: this path is also reached by external A2A
		// callers, which have no propose_plan of their own to call. The
		// coordinator's tool adds that hint itself.
		return nil, fmt.Errorf("%w: this run is awaiting human approval of its initial plan, so no task can be created yet", ErrApprovalPending)
	}
	if n := len(rs.run.Tasks); n >= rs.budget.MaxTasks {
		rs.mu.Unlock()
		return nil, fmt.Errorf("task budget exhausted: %d of %d tasks already created. "+
			"Do not create more — converge with what you have, or finish_run with what is achievable", n, rs.budget.MaxTasks)
	}
	if req.Depth > rs.budget.MaxDelegationDepth {
		rs.mu.Unlock()
		return nil, fmt.Errorf("delegation depth %d exceeds the limit of %d: this work must be done at the current level, not delegated further",
			req.Depth, rs.budget.MaxDelegationDepth)
	}
	agent := rs.pool[req.Agent]
	if agent == nil {
		names := make([]string, 0, len(rs.pool))
		for n := range rs.pool {
			names = append(names, n)
		}
		sort.Strings(names)
		rs.mu.Unlock()
		return nil, fmt.Errorf("unknown agent %q; available: %s", req.Agent, strings.Join(names, ", "))
	}
	if strings.TrimSpace(req.Instruction) == "" {
		rs.mu.Unlock()
		return nil, fmt.Errorf("instruction is required and must be self-contained: the worker cannot see your context")
	}
	taskModel, ok := model.ResolveModel(strings.TrimSpace(req.Model))
	if !ok {
		rs.mu.Unlock()
		return nil, fmt.Errorf("unknown model %q: use a tier alias (haiku for mechanical work, sonnet for standard work, "+
			"opus for hard reasoning) or omit the field to use the agent's default", req.Model)
	}
	if model.CoordinatorOnlyModel(taskModel) {
		rs.mu.Unlock()
		return nil, fmt.Errorf("model %q is reserved for the coordinator role; worker tasks cap at opus — "+
			"use \"opus\" for the hardest delegations", req.Model)
	}
	modelCapped := ""
	if taskModel == "" {
		taskModel = agent.Model
		// A legacy or hand-edited pool agent may declare a coordinator-only
		// default; the ceiling is enforced here, where the session model is
		// actually decided, and audited below.
		if model.CoordinatorOnlyModel(taskModel) {
			modelCapped = taskModel
			taskModel = model.WorkerModelCeiling
		}
	}
	internal := req.CreatedBy != CreatedByExternal
	if internal {
		if strings.TrimSpace(req.Constraints) == "" {
			rs.mu.Unlock()
			return nil, fmt.Errorf("constraints is required: state the cross-domain constraints this worker cannot infer " +
				"(interfaces to honor, formats, style rules, boundaries with other tasks). Write \"none\" only if there " +
				"genuinely are none")
		}
		// Feasibility needs no per-agent check anymore: every worker can
		// deliver files through the hub's write_artifact, whatever its own
		// tool allowlist says.
		if err := ValidateChecks(req.Acceptance); err != nil {
			rs.mu.Unlock()
			return nil, err
		}
	}
	if agent.Independent && strings.TrimSpace(req.ContextHint) != "" {
		rs.mu.Unlock()
		return nil, fmt.Errorf("agent %q is an independent verifier: it must form its own view from the artifacts. "+
			"Do not pass a context_hint — put the requirement and the artifact paths in the instruction, and nothing "+
			"about how the work was done or what its author concluded", req.Agent)
	}

	// Rework routing: an explicit retry_of, or an implicit one when this
	// delegation targets the same agent and title as an already-failed task.
	// Detecting the implicit case is what stops "just don't say retry_of"
	// from becoming the bypass.
	retryOf := req.RetryOf
	if retryOf == "" && internal {
		for _, id := range rs.run.TaskOrder {
			prev := rs.run.Tasks[id]
			if prev.Status == model.TaskFailed && prev.Agent == req.Agent &&
				strings.EqualFold(strings.TrimSpace(prev.Title), strings.TrimSpace(req.Title)) && prev.Title != "" {
				retryOf = id
			}
		}
	}
	if retryOf != "" {
		orig := rs.run.Tasks[retryOf]
		if orig == nil {
			rs.mu.Unlock()
			return nil, fmt.Errorf("retry_of names unknown task %q", retryOf)
		}
		if orig.Status != model.TaskFailed {
			rs.mu.Unlock()
			return nil, fmt.Errorf("task %s is %s, not failed; only failed tasks can be reworked", retryOf, orig.Status)
		}
		root := reworkRootLocked(rs.run, retryOf)
		if kind := rs.run.Tasks[root].FailureKind; kind != model.FailBlocked {
			rs.mu.Unlock()
			return nil, fmt.Errorf("task %s failed as %q — rework cannot fix that. %s Do not re-send the same task; "+
				"change the plan instead", root, kind, reworkAdvice(kind))
		}
		reworks := 0
		for _, t := range rs.run.Tasks {
			if t.RetryOf != "" && reworkRootLocked(rs.run, t.RetryOf) == root {
				reworks++
			}
		}
		if reworks >= rs.budget.MaxReworksPerTask {
			rs.mu.Unlock()
			return nil, fmt.Errorf("task %s has already been reworked %d time(s), the limit for one task. Escalate: "+
				"revise the plan (different agent, smaller scope, different approach) or finish_run with what is "+
				"honestly achievable", root, reworks)
		}
	}

	now := time.Now()
	t := &model.Task{
		ID:          store.NewTaskID(),
		Agent:       req.Agent,
		Model:       taskModel,
		Title:       strings.TrimSpace(req.Title),
		Instruction: req.Instruction,
		Constraints: strings.TrimSpace(req.Constraints),
		CreatedBy:   req.CreatedBy,
		RetryOf:     retryOf,
		Depth:       req.Depth,
		Status:      model.TaskSubmitted,
		Acceptance:  req.Acceptance,
		CreatedAt:   now,
	}
	if t.Title == "" {
		t.Title = firstLine(req.Instruction, 60)
	}
	instruction := req.Instruction
	if h := strings.TrimSpace(req.ContextHint); h != "" {
		instruction += "\n\nContext from the delegator: " + h
	}
	t.Instruction = instruction
	t.Messages = append(t.Messages, model.TaskMessage{
		Ts: now, From: req.CreatedBy, Role: model.MsgInstruction, Text: instruction,
	})
	rs.run.Tasks[t.ID] = t
	rs.run.TaskOrder = append(rs.run.TaskOrder, t.ID)
	rs.ctrl[t.ID] = rs.newTaskCtrl()
	rs.notifyLocked()
	rs.mu.Unlock()

	rs.event("task_created", t.ID, fmt.Sprintf("%s → %s [%s]: %s (depth %d)", req.CreatedBy, req.Agent, taskModel, t.Title, t.Depth))
	if modelCapped != "" {
		rs.event("model_capped", t.ID, fmt.Sprintf("agent %q declares %s, which is reserved for the coordinator; task runs on %s",
			req.Agent, modelCapped, taskModel))
	}
	rs.schedule()
	return t, nil
}

// schedule dispatches as many queued tasks as the parallelism budget allows.
//
// A slot is held from dispatch until the task is terminal — including while it
// sits in input-required, because a task blocked on a question still owns a
// live agent session and the memory behind it (docs/DECISIONS-v2.md D10).
// Occupancy is recomputed from the ledger on every pass so it can never drift.
func (rs *RunSession) schedule() {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return
	}
	rs.freezeOutputLocked() // the first dispatch pins the exchange directory
	occupied := 0
	var queue []string
	for _, id := range rs.run.TaskOrder {
		t := rs.run.Tasks[id]
		switch {
		case model.TaskTerminal(t.Status):
			delete(rs.dispatched, id)
		case rs.dispatched[id] || t.Status == model.TaskWorking || t.Status == model.TaskInputRequired:
			occupied++
		case t.Status == model.TaskSubmitted:
			queue = append(queue, id)
		}
	}
	var start []string
	for _, id := range queue {
		if occupied >= rs.budget.MaxParallel {
			break
		}
		occupied++
		rs.dispatched[id] = true
		start = append(start, id)
	}
	rs.mu.Unlock()

	// StartTask is called outside the lock: the engine may touch the ledger.
	for _, id := range start {
		rs.cfg.Exec.StartTask(rs, id)
	}
}

// ---- task state transitions ----

// legalTransitions is the A2A lifecycle, spelled out so an out-of-order
// report from a worker goroutine is rejected loudly instead of corrupting the
// ledger.
var legalTransitions = map[string]map[string]bool{
	model.TaskSubmitted:     {model.TaskWorking: true, model.TaskCanceled: true, model.TaskFailed: true},
	model.TaskWorking:       {model.TaskInputRequired: true, model.TaskCompleted: true, model.TaskFailed: true, model.TaskCanceled: true},
	model.TaskInputRequired: {model.TaskWorking: true, model.TaskCompleted: true, model.TaskFailed: true, model.TaskCanceled: true},
	model.TaskCompleted:     {},
	model.TaskFailed:        {},
	model.TaskCanceled:      {},
}

// transitionLocked applies a state change if the lifecycle permits it.
func (rs *RunSession) transitionLocked(t *model.Task, to string) error {
	if t.Status == to {
		return nil
	}
	if !legalTransitions[t.Status][to] {
		return fmt.Errorf("illegal task transition %s → %s for %s", t.Status, to, t.ID)
	}
	t.Status = to
	return nil
}

// TaskStarted marks a task as working; called by the engine when the worker
// session is live.
func (rs *RunSession) TaskStarted(taskID string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		return fmt.Errorf("unknown task %s", taskID)
	}
	if err := rs.transitionLocked(t, model.TaskWorking); err != nil {
		return err
	}
	t.StartedAt = time.Now()
	rs.notifyLocked()
	return nil
}

// TaskActivity records what the worker is doing right now. This is live UI
// signal only: it neither persists nor counts as a ledger transition, so it
// can't reset the stall detector.
func (rs *RunSession) TaskActivity(taskID, text string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if t := rs.run.Tasks[taskID]; t != nil && !model.TaskTerminal(t.Status) {
		t.Activity = text
		if rs.cfg.OnChange != nil {
			rs.cfg.OnChange(rs.run)
		}
	}
}

// RecordTaskCost prices one turn of a task.
func (rs *RunSession) RecordTaskCost(taskID, modelID string, usage model.TokenUsage, cost float64, durMs int64) {
	rs.mu.Lock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		rs.mu.Unlock()
		return
	}
	t.CostUSD += cost
	t.Usage.Add(usage)
	t.DurationMs += durMs
	rs.run.CostUSD += cost
	rs.run.Usage.Add(usage)
	agent := t.Agent
	rs.mu.Unlock()

	if rs.cfg.OnCost != nil && (cost != 0 || !usage.Empty()) {
		rs.cfg.OnCost(store.CostEntry{
			WorkflowID: rs.run.WorkflowID, RunID: rs.run.ID, Unit: "task", UnitID: taskID,
			Agent: agent, Model: modelID, Usage: usage, CostUSD: cost,
		})
	}
}

// CompleteTask puts a task into a terminal state and frees its session slot.
// Failures land with kind "unspecified", which the rework router treats as
// not rework-eligible; use CompleteTaskWith when the kind is known.
func (rs *RunSession) CompleteTask(taskID, summary string, artifacts []string, taskErr error) {
	rs.CompleteTaskWith(taskID, summary, artifacts, taskErr, "", nil)
}

// CompleteTaskWith is CompleteTask plus the failure classification and the
// executed acceptance results — the two fields the coordinator's routing and
// the audit trail key on.
func (rs *RunSession) CompleteTaskWith(taskID, summary string, artifacts []string, taskErr error,
	failureKind string, acceptance []model.CheckResult) {
	rs.mu.Lock()
	t := rs.run.Tasks[taskID]
	if t == nil || model.TaskTerminal(t.Status) {
		rs.mu.Unlock()
		return
	}
	if acceptance != nil {
		t.AcceptanceResults = acceptance
	}
	to := model.TaskCompleted
	if taskErr != nil {
		to = model.TaskFailed
		t.Error = taskErr.Error()
		if failureKind == "" {
			failureKind = model.FailUnspecified
		}
		t.FailureKind = failureKind
	}
	// A worker can be terminated from working or input-required; force the
	// transition table's hand only through the legal edges.
	if err := rs.transitionLocked(t, to); err != nil {
		t.Status = to // terminal states are absorbing; never leave a zombie
	}
	t.Summary = summary
	// Union, not overwrite: files already delivered through write_artifact
	// stay on the record even when the final report forgets to list them.
	for _, a := range artifacts {
		t.Artifacts = appendUnique(t.Artifacts, a)
	}
	t.Activity = ""
	t.EndedAt = time.Now()
	if summary != "" {
		t.Messages = append(t.Messages, model.TaskMessage{
			Ts: t.EndedAt, From: t.ID, Role: model.MsgResult, Text: summary,
		})
	}
	rs.notifyLocked()
	rs.mu.Unlock()

	if taskErr != nil {
		rs.event("task_status", taskID, fmt.Sprintf("failed (%s): %s", failureKind, taskErr.Error()))
	} else {
		rs.event("task_status", taskID, "completed: "+firstLine(summary, 120))
	}
	rs.schedule()
}

// cancelTask stops a task and its worker session.
func (rs *RunSession) cancelTask(taskID, reason string) {
	rs.mu.Lock()
	t := rs.run.Tasks[taskID]
	if t == nil || model.TaskTerminal(t.Status) {
		rs.mu.Unlock()
		return
	}
	t.Status = model.TaskCanceled
	t.Error = reason
	t.Activity = ""
	t.EndedAt = time.Now()
	ctrl := rs.ctrl[taskID]
	rs.notifyLocked()
	rs.mu.Unlock()

	if ctrl != nil {
		select { // release a worker parked on a question
		case ctrl.answer <- "":
		default:
		}
	}
	rs.cfg.Exec.CancelTask(rs, taskID)
	rs.event("task_status", taskID, "canceled: "+reason)
}

// CancelTask is the exported entry point (A2A tasks/cancel, run teardown).
func (rs *RunSession) CancelTask(taskID, reason string) error {
	rs.mu.Lock()
	_, ok := rs.run.Tasks[taskID]
	rs.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown task %s", taskID)
	}
	rs.cancelTask(taskID, reason)
	rs.schedule()
	return nil
}

// ---- messaging ----

// Ask parks a task in input-required and blocks until an answer arrives, the
// task dies, or the run ends. The worker stays inside its MCP tool call for
// the whole wait, so answering costs no extra turn and loses no context.
func (rs *RunSession) Ask(ctx context.Context, taskID, question string) (string, error) {
	rs.mu.Lock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		rs.mu.Unlock()
		return "", fmt.Errorf("unknown task %s", taskID)
	}
	if err := rs.transitionLocked(t, model.TaskInputRequired); err != nil {
		rs.mu.Unlock()
		return "", err
	}
	t.Messages = append(t.Messages, model.TaskMessage{
		Ts: time.Now(), From: taskID, Role: model.MsgQuestion, Text: question,
	})
	ctrl := rs.ctrl[taskID]
	rs.notifyLocked()
	rs.mu.Unlock()

	rs.event("task_message", taskID, "asked the coordinator: "+firstLine(question, 120))

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-rs.finished:
		return "", fmt.Errorf("run ended while waiting for an answer")
	case answer := <-ctrl.answer:
		if answer == "" {
			return "", fmt.Errorf("task was canceled while waiting for an answer")
		}
		rs.mu.Lock()
		if t := rs.run.Tasks[taskID]; t != nil && t.Status == model.TaskInputRequired {
			rs.transitionLocked(t, model.TaskWorking)
		}
		rs.notifyLocked()
		rs.mu.Unlock()
		return answer, nil
	}
}

// SendResult tells the sender how a message was delivered, so a coordinator
// can tell "it heard you" from "it will hear you at the next turn".
type SendResult struct {
	Delivery string `json:"delivery"` // immediate | queued_next_turn
	Note     string `json:"note,omitempty"`
}

// Send delivers a message to a task. An input-required task is unblocked in
// place; a working task gets it at its next turn boundary, which is the best
// ACP allows (docs/DECISIONS-v2.md D4).
func (rs *RunSession) Send(taskID, from, text string) (*SendResult, error) {
	rs.mu.Lock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		rs.mu.Unlock()
		return nil, fmt.Errorf("unknown task %s", taskID)
	}
	if model.TaskTerminal(t.Status) {
		st := t.Status
		rs.mu.Unlock()
		return nil, fmt.Errorf("task %s is already %s; it cannot receive messages. Delegate a new task instead", taskID, st)
	}
	if t.Turns >= rs.budget.MaxTurnsPerTask {
		turns := t.Turns
		rs.mu.Unlock()
		return nil, fmt.Errorf("task %s has used all %d of its allowed turns: no more back-and-forth is possible. "+
			"Decide based on what it has produced, or delegate a fresh task with a sharper instruction", taskID, turns)
	}
	role := model.MsgFollowup
	t.Messages = append(t.Messages, model.TaskMessage{Ts: time.Now(), From: from, Role: role, Text: text})
	ctrl := rs.ctrl[taskID]
	waiting := t.Status == model.TaskInputRequired
	if !waiting {
		ctrl.inbox = append(ctrl.inbox, text)
	}
	rs.notifyLocked()
	rs.mu.Unlock()

	rs.event("task_message", taskID, from+" → task: "+firstLine(text, 120))
	if waiting {
		select {
		case ctrl.answer <- text:
		default: // worker already moved on; the message stays in the audit log
		}
		return &SendResult{Delivery: "immediate"}, nil
	}
	return &SendResult{
		Delivery: "queued_next_turn",
		Note:     "the worker is mid-turn; it will receive this as a follow-up turn once the current one ends",
	}, nil
}

// TakeInbox drains queued follow-up messages for a task. The engine calls this
// at a turn boundary: a non-empty result means the task continues for another
// turn instead of finalizing.
func (rs *RunSession) TakeInbox(taskID string) []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	ctrl := rs.ctrl[taskID]
	if ctrl == nil || len(ctrl.inbox) == 0 {
		return nil
	}
	out := ctrl.inbox
	ctrl.inbox = nil
	return out
}

// TurnSpent counts a prompt turn against the task's turn budget.
func (rs *RunSession) TurnSpent(taskID string) (used, max int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		return 0, rs.budget.MaxTurnsPerTask
	}
	t.Turns++
	return t.Turns, rs.budget.MaxTurnsPerTask
}

// Progress records a mid-flight report from a worker.
func (rs *RunSession) Progress(taskID, text string) error {
	rs.mu.Lock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		rs.mu.Unlock()
		return fmt.Errorf("unknown task %s", taskID)
	}
	t.Messages = append(t.Messages, model.TaskMessage{
		Ts: time.Now(), From: taskID, Role: model.MsgProgress, Text: text,
	})
	rs.notifyLocked()
	rs.mu.Unlock()
	rs.event("task_message", taskID, "progress: "+firstLine(text, 120))
	return nil
}

// ReportResult records the worker's final report. The task does not settle
// here — the engine collects the report at the turn boundary and still runs
// the acceptance contract itself. Calling again in the same turn replaces the
// earlier report.
func (rs *RunSession) ReportResult(taskID string, r ReportedResult) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	t := rs.run.Tasks[taskID]
	ctrl := rs.ctrl[taskID]
	if t == nil || ctrl == nil || model.TaskTerminal(t.Status) {
		return fmt.Errorf("task %s is not active", taskID)
	}
	ctrl.reported = &r
	return nil
}

// TakeReportedResult hands the engine the report of the turn just ended, if
// any, and clears it — a task continued by steering starts the next turn with
// a clean slate, because a report made before the steering arrived no longer
// describes a finished task.
func (rs *RunSession) TakeReportedResult(taskID string) (ReportedResult, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	ctrl := rs.ctrl[taskID]
	if ctrl == nil || ctrl.reported == nil {
		return ReportedResult{}, false
	}
	r := *ctrl.reported
	ctrl.reported = nil
	return r, true
}

// PeerMessage records a worker-to-worker exchange on both tasks' ledgers.
func (rs *RunSession) PeerMessage(fromTask, toTask, text string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	src, dst := rs.run.Tasks[fromTask], rs.run.Tasks[toTask]
	if src == nil || dst == nil {
		return fmt.Errorf("unknown task in peer exchange")
	}
	m := model.TaskMessage{Ts: time.Now(), From: fromTask, Role: model.MsgPeer, Text: text}
	src.Messages = append(src.Messages, m)
	dst.Messages = append(dst.Messages, m)
	rs.notifyLocked()
	return nil
}

// Related reports whether two tasks are lineage-adjacent: parent, child, or
// sibling under the same creator. ask_agent is limited to these so a worker
// can consult the work it actually depends on, not broadcast run-wide.
func (rs *RunSession) Related(a, b string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	ta, tb := rs.run.Tasks[a], rs.run.Tasks[b]
	if ta == nil || tb == nil {
		return false
	}
	return ta.CreatedBy == b || tb.CreatedBy == a || (ta.CreatedBy == tb.CreatedBy && ta.CreatedBy != "")
}

// ---- awaiting ----

// TaskView is the summary shape returned to the coordinator: enough to decide,
// small enough not to blow up its context. Full message history stays in the
// ledger for the UI and the audit trail.
type TaskView struct {
	ID         string   `json:"task_id"`
	Agent      string   `json:"agent"`
	Model      string   `json:"model,omitempty"`
	Title      string   `json:"title"`
	Status     string   `json:"status"`
	Summary    string   `json:"summary,omitempty"`
	Question   string   `json:"question,omitempty"` // set when status is input-required
	Error      string   `json:"error,omitempty"`
	Artifacts  []string `json:"artifacts,omitempty"`
	Activity   string   `json:"activity,omitempty"`
	Depth      int      `json:"depth"`
	Turns      int      `json:"turns"`
	CostUSD    float64  `json:"cost_usd"`
	Acceptance string   `json:"acceptance,omitempty"` // executed-check summary, e.g. "2/2 checks passed"

	// Failure routing, filled when status is failed. Route says what the
	// budget engine will actually permit: "rework-allowed" only for kind
	// "blocked"; everything else must go back to the plan.
	FailureKind string `json:"failure_kind,omitempty"`
	Route       string `json:"route,omitempty"`
}

func (rs *RunSession) viewLocked(t *model.Task) TaskView {
	v := TaskView{
		ID: t.ID, Agent: t.Agent, Model: t.Model, Title: t.Title, Status: t.Status,
		Summary: t.Summary, Error: t.Error, Artifacts: t.Artifacts,
		Activity: t.Activity, Depth: t.Depth, Turns: t.Turns, CostUSD: t.CostUSD,
	}
	if len(t.AcceptanceResults) > 0 {
		v.Acceptance = SummarizeResults(t.AcceptanceResults)
	}
	if t.Status == model.TaskFailed {
		v.FailureKind = t.FailureKind
		if t.FailureKind == model.FailBlocked {
			v.Route = "rework-allowed"
		} else {
			v.Route = "replan-required"
		}
	}
	if t.Status == model.TaskInputRequired {
		for i := len(t.Messages) - 1; i >= 0; i-- {
			if t.Messages[i].Role == model.MsgQuestion {
				v.Question = t.Messages[i].Text
				break
			}
		}
	}
	return v
}

// AmendAcceptance replaces an in-flight task's acceptance contract. The
// coordinator can never waive a contract — but a contract that was wrong at
// delegation time (e.g. it demanded the wrong path) can be corrected,
// audited, with the worker told at its next turn boundary. Artifact checks
// are feasible for every agent — the hub's write_artifact sees to that.
func (rs *RunSession) AmendAcceptance(taskID string, checks []model.AcceptanceCheck) error {
	if err := ValidateChecks(checks); err != nil {
		return err
	}
	rs.mu.Lock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		rs.mu.Unlock()
		return fmt.Errorf("unknown task %s", taskID)
	}
	if model.TaskTerminal(t.Status) {
		st := t.Status
		rs.mu.Unlock()
		return fmt.Errorf("task %s is already %s; its contract has been judged and cannot be amended", taskID, st)
	}
	t.Acceptance = checks
	note := "The acceptance contract of your task was AMENDED. Your work now passes only if the new checks pass:\n" +
		describeChecks(checks)
	t.Messages = append(t.Messages, model.TaskMessage{
		Ts: time.Now(), From: RoleCoordinator, Role: model.MsgFollowup, Text: note,
	})
	if ctrl := rs.ctrl[taskID]; ctrl != nil {
		ctrl.inbox = append(ctrl.inbox, note) // delivered at the next turn boundary
	}
	rs.appendEventLocked("acceptance_amended", taskID, fmt.Sprintf("contract amended to %d check(s)", len(checks)))
	rs.notifyLocked()
	rs.mu.Unlock()
	return nil
}

// describeChecks renders a contract for human/worker consumption.
func describeChecks(checks []model.AcceptanceCheck) string {
	var b strings.Builder
	for _, c := range checks {
		switch c.Kind {
		case model.CheckArtifactExists:
			fmt.Fprintf(&b, "- artifact exists: %s\n", c.Path)
		case model.CheckArtifactContains:
			fmt.Fprintf(&b, "- artifact %s matches pattern: %s\n", c.Path, c.Pattern)
		case model.CheckCommand:
			fmt.Fprintf(&b, "- command exits 0: %s\n", c.Command)
		}
	}
	return b.String()
}

// OutputInfo reports the current exchange directory and whether it carries a
// coordinator-chosen name — the locked read the round prompt builds from.
func (rs *RunSession) OutputInfo() (dir string, named bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.run.OutputDir != "" {
		return rs.run.OutputDir, true
	}
	return rs.workspace, false
}

// Notes returns a copy of the coordinator's persisted notes.
func (rs *RunSession) Notes() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.run.CoordinatorNotes...)
}

// TaskCount reports how many tasks the ledger holds.
func (rs *RunSession) TaskCount() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.run.Tasks)
}

// CoordinatorDecision returns the last recorded verdict summary, if any.
func (rs *RunSession) CoordinatorDecision() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.run.Coordinator == nil {
		return ""
	}
	return rs.run.Coordinator.Decision
}

// AcceptanceOf reads a task's current acceptance contract under the session
// lock. The engine calls it at completion time rather than using a stale
// snapshot, so an amended contract is the one that judges the work.
func (rs *RunSession) AcceptanceOf(taskID string) []model.AcceptanceCheck {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		return nil
	}
	return append([]model.AcceptanceCheck(nil), t.Acceptance...)
}

// TaskSnapshot returns a copy of one task under the session lock — the safe
// way for engine goroutines to read task fields while the ledger is live.
func (rs *RunSession) TaskSnapshot(taskID string) (model.Task, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		return model.Task{}, false
	}
	return *t, true
}

// View returns the summary of one task.
func (rs *RunSession) View(taskID string) (TaskView, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		return TaskView{}, false
	}
	return rs.viewLocked(t), true
}

// Views returns summaries for the given tasks, or all of them if ids is empty.
func (rs *RunSession) Views(ids []string) []TaskView {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(ids) == 0 {
		ids = rs.run.TaskOrder
	}
	out := make([]TaskView, 0, len(ids))
	for _, id := range ids {
		if t := rs.run.Tasks[id]; t != nil {
			out = append(out, rs.viewLocked(t))
		}
	}
	return out
}

// settled reports whether a task has reached a state the coordinator can act
// on: terminal, or blocked on a question only it can answer.
func settled(status string) bool {
	return model.TaskTerminal(status) || status == model.TaskInputRequired
}

// SettledFingerprint distinguishes settled states a round has already seen
// from new ones. It includes the question text so a task that asks a second
// question — same status, different content — still wakes a new round.
func SettledFingerprint(v TaskView) string {
	if !settled(v.Status) {
		return ""
	}
	if v.Status == model.TaskInputRequired {
		return v.Status + "|" + v.Question
	}
	return v.Status
}

func settledFingerprintLocked(t *model.Task) string {
	if !settled(t.Status) {
		return ""
	}
	if t.Status == model.TaskInputRequired {
		q := ""
		for i := len(t.Messages) - 1; i >= 0; i-- {
			if t.Messages[i].Role == model.MsgQuestion {
				q = t.Messages[i].Text
				break
			}
		}
		return t.Status + "|" + q
	}
	return t.Status
}

// awaitHardCap bounds how long a single await call blocks. The MCP client owns
// the tool-call timeout and loom does not, so await is built to always return
// an honest snapshot well inside any plausible client limit, and to be called
// again (docs/DECISIONS-v2.md D5).
const awaitHardCap = 120 * time.Second

// AwaitResult is what the coordinator gets back from await.
type AwaitResult struct {
	Tasks    []TaskView `json:"tasks"`
	TimedOut bool       `json:"timed_out"`
	Hint     string     `json:"hint,omitempty"`
	Notice   string     `json:"notice,omitempty"`
}

// Await blocks until the requested tasks are settled (mode "all") or at least
// one is (mode "any"), the cap elapses, or the run ends.
func (rs *RunSession) Await(ctx context.Context, ids []string, mode string, timeoutSec int) (*AwaitResult, error) {
	rs.mu.Lock()
	if len(ids) == 0 {
		ids = append([]string(nil), rs.run.TaskOrder...)
	}
	for _, id := range ids {
		if rs.run.Tasks[id] == nil {
			rs.mu.Unlock()
			return nil, fmt.Errorf("unknown task %q", id)
		}
	}
	rs.mu.Unlock()
	if len(ids) == 0 {
		return nil, fmt.Errorf("there are no tasks to await; delegate something first")
	}

	limit := awaitHardCap
	if timeoutSec > 0 && time.Duration(timeoutSec)*time.Second < limit {
		limit = time.Duration(timeoutSec) * time.Second
	}
	deadline := time.NewTimer(limit)
	defer deadline.Stop()

	for {
		ch := rs.waitChange()

		rs.mu.Lock()
		done, any := true, false
		for _, id := range ids {
			if settled(rs.run.Tasks[id].Status) {
				any = true
			} else {
				done = false
			}
		}
		notice := rs.takeNoticeLocked()
		rs.mu.Unlock()

		ready := done || (mode == "any" && any)
		if ready || notice != "" {
			res := &AwaitResult{Tasks: rs.Views(ids), Notice: notice}
			if !ready {
				res.TimedOut = true
				res.Hint = "returned early to deliver a notice; the tasks below are still in flight"
			}
			return res, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-rs.finished:
			return &AwaitResult{Tasks: rs.Views(ids), Hint: "the run has ended"}, nil
		case <-deadline.C:
			return &AwaitResult{
				Tasks:    rs.Views(ids),
				TimedOut: true,
				Hint: "not settled yet — this is a normal partial result, not an error. " +
					"Call await again to keep waiting, or act on what has settled.",
			}, nil
		case <-ch:
		}
	}
}

// ---- stall detection ----

// takeNoticeLocked pops a pending system notice for delivery to the coordinator.
func (rs *RunSession) takeNoticeLocked() string {
	n := rs.pendingNotice
	rs.pendingNotice = ""
	return n
}

// joinNotice accumulates notices instead of overwriting: a stall warning must
// never erase an approval decision that landed a moment earlier.
func joinNotice(old, add string) string {
	if old == "" {
		return add
	}
	return old + "\n\n" + add
}

// peekNotice reports a pending notice without consuming it (tests only).
func (rs *RunSession) peekNotice() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.pendingNotice
}

// TakeNotice pops a pending notice; hub tools attach it to their result so the
// coordinator sees it wherever it happens to be.
func (rs *RunSession) TakeNotice() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.takeNoticeLocked()
}

// watchStall warns when the whole ledger goes quiet. A dynamic run has no
// structural progress guarantee, so "nothing has changed in ten minutes" is
// the only signal that distinguishes deep work from a wedged coordinator.
func (rs *RunSession) watchStall(ctx context.Context) {
	// Poll at a quarter of the stall threshold so the warning lands within
	// ~25% of it, clamped to keep the timer sane at either extreme. Without
	// the lower bound tracking the threshold, a small StallSec would silently
	// do nothing — a setting that lies is worse than no setting.
	interval := time.Duration(rs.budget.StallSec) * time.Second / 4
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	warned := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-rs.finished:
			return
		case <-tick.C:
			rs.mu.Lock()
			// Waiting on a human is not a stall: a coordinator parked on
			// ask_user (or the approval gate) has done its part, and a stall
			// notice here would push it to abandon a legitimate wait.
			waitingOnHuman := rs.approvalPending ||
				(rs.run.Coordinator != nil && rs.run.Coordinator.Status == "awaiting_user")
			idle := time.Since(rs.lastTransition)
			last := rs.lastTransition
			stalled := !waitingOnHuman && idle > time.Duration(rs.budget.StallSec)*time.Second && last.After(warned)
			if stalled {
				warned = time.Now()
				rs.pendingNotice = joinNotice(rs.pendingNotice, fmt.Sprintf(
					"SYSTEM: no task has changed state for %s. Assess progress now: either report where each "+
						"in-flight task stands and change strategy, or call finish_run with what has been achieved.",
					idle.Round(time.Second)))
			}
			rs.mu.Unlock()
			if stalled {
				rs.event("stall_warning", "", fmt.Sprintf("no ledger transition for %s", idle.Round(time.Second)))
				rs.mu.Lock()
				rs.notifyLocked() // wake any await so the notice lands immediately
				rs.mu.Unlock()
			}
		}
	}
}

// ---- approval gate ----

// Propose submits the coordinator's initial plan for human approval and
// returns immediately: the coordinator ends its round, and the human's
// decision arrives as a notice that wakes the next one. Re-proposing after a
// rejection is legal — that is what "revise the plan" means.
func (rs *RunSession) Propose(p *model.Proposal) error {
	rs.mu.Lock()
	if rs.approvalDone {
		rs.mu.Unlock()
		return fmt.Errorf("the plan has already been approved; delegate directly from here on")
	}
	rs.run.Proposal = p
	rs.run.Status = model.RunAwaitingApproval
	rs.approvalPending = true
	rs.rejectedReason = ""
	if rs.run.Coordinator != nil {
		rs.run.Coordinator.Status = "awaiting_approval"
	}
	rs.appendEventLocked("run_status", "", fmt.Sprintf("awaiting approval of initial plan (%d tasks)", len(p.Tasks)))
	rs.notifyLocked()
	rs.mu.Unlock()
	return nil
}

// Approve releases the approval gate and wakes the coordinator with the news.
func (rs *RunSession) Approve() error {
	rs.mu.Lock()
	if !rs.approvalPending {
		rs.mu.Unlock()
		return fmt.Errorf("run is not awaiting approval")
	}
	rs.approvalPending = false
	rs.approvalDone = true
	rs.run.PlanApproved = true // persisted, so a resumed run never re-asks
	rs.run.Status = model.RunRunning
	if rs.run.Coordinator != nil {
		rs.run.Coordinator.Status = "working"
	}
	rs.pendingNotice = joinNotice(rs.pendingNotice, "SYSTEM: the human APPROVED your plan. Delegate the proposed tasks now.")
	rs.appendEventLocked("info", "", "initial plan approved")
	rs.notifyLocked()
	rs.mu.Unlock()
	return nil
}

// Reject refuses the proposed plan. Delegation stays gated; the coordinator is
// woken to either revise and re-propose, or finish the run as failed.
func (rs *RunSession) Reject(reason string) error {
	rs.mu.Lock()
	if !rs.approvalPending {
		rs.mu.Unlock()
		return fmt.Errorf("run is not awaiting approval")
	}
	rs.approvalPending = false
	rs.rejectedReason = reason
	rs.run.Status = model.RunRunning
	if rs.run.Coordinator != nil {
		rs.run.Coordinator.Status = "working"
	}
	rs.pendingNotice = joinNotice(rs.pendingNotice, "SYSTEM: the human REJECTED your plan: "+reason+
		". Either revise it and call propose_plan again, or call finish_run with status failed.")
	rs.appendEventLocked("run_status", "", "plan rejected: "+reason)
	rs.notifyLocked()
	rs.mu.Unlock()
	return nil
}

// AwaitingApproval reports whether a human decision is pending.
func (rs *RunSession) AwaitingApproval() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.approvalPending
}

// ---- worker artifact delivery ----

// maxArtifactWriteBytes bounds one write_artifact call; append lets a larger
// document arrive in chunks without any single call ballooning the transport.
const maxArtifactWriteBytes = 512 * 1024

// WriteArtifact is the hub's delivery channel for worker output: any worker —
// including a pure-reasoning agent with no file tools at all — can persist a
// document into the exchange directory through it. It exists so that "large
// text goes into md files, never into messages" is an enforceable rule rather
// than advice a no-tool agent cannot follow.
func (rs *RunSession) WriteArtifact(taskID, path, content string, appendTo bool) error {
	if err := checkPathOK(path); err != nil {
		return err
	}
	if len(content) > maxArtifactWriteBytes {
		return fmt.Errorf("content is %d bytes; one write_artifact call caps at %d — split the document and "+
			"send the rest with append=true", len(content), maxArtifactWriteBytes)
	}
	rs.mu.Lock()
	t := rs.run.Tasks[taskID]
	if t == nil {
		rs.mu.Unlock()
		return fmt.Errorf("unknown task %s", taskID)
	}
	if model.TaskTerminal(t.Status) {
		st := t.Status
		rs.mu.Unlock()
		return fmt.Errorf("task %s is %s; its delivery window has closed", taskID, st)
	}
	rs.mu.Unlock()

	full := filepath.Join(rs.Workspace(), path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(full, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// The delivery is recorded on the task immediately: the UI shows artifacts
	// live, and completion can only add to this list, never lose it.
	rs.mu.Lock()
	if t := rs.run.Tasks[taskID]; t != nil {
		t.Artifacts = appendUnique(t.Artifacts, path)
	}
	rs.notifyLocked()
	rs.mu.Unlock()
	mode := ""
	if appendTo {
		mode = ", append"
	}
	rs.event("artifact_written", taskID, fmt.Sprintf("%s (%d bytes%s)", path, len(content), mode))
	return nil
}

// appendUnique adds s to list unless it is already there.
func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// ---- coordinator round support ----

// Inspect is the coordinator's audited read of a run deliverable: a file under
// the exchange directory, returned truncated, counted, and logged. It is the
// only read access the coordinator has — its session gets no file tools — so
// every look at the work leaves a ledger trace.
const maxInspectBytes = 16 * 1024

func (rs *RunSession) Inspect(path string) (string, error) {
	if err := checkPathOK(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(rs.Workspace(), path))
	if err != nil {
		return "", fmt.Errorf("cannot read %q: %v. Inspect paths are relative to the exchange directory; "+
			"check the artifact list of the task that claimed to produce it", path, err)
	}
	truncated := false
	if len(data) > maxInspectBytes {
		data = data[:maxInspectBytes]
		truncated = true
	}
	rs.mu.Lock()
	rs.inspections++
	rs.mu.Unlock()
	rs.event("inspection", "", fmt.Sprintf("coordinator read %s (%d bytes)", path, len(data)))
	out := string(data)
	if truncated {
		out += "\n\n[truncated at 16KB]"
	}
	return out, nil
}

// Inspections reports how many deliverables the coordinator has actually read.
func (rs *RunSession) Inspections() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.inspections
}

// maxCoordinatorNotes bounds the externalized memory: enough to carry strategy
// across rounds, small enough that it can never become a second transcript.
const maxCoordinatorNotes = 20

// AddNote persists one coordinator note across rounds.
func (rs *RunSession) AddNote(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	rs.mu.Lock()
	rs.run.CoordinatorNotes = append(rs.run.CoordinatorNotes, text)
	if n := len(rs.run.CoordinatorNotes); n > maxCoordinatorNotes {
		rs.run.CoordinatorNotes = rs.run.CoordinatorNotes[n-maxCoordinatorNotes:]
	}
	rs.notifyLocked()
	rs.mu.Unlock()
}

// ---- user chat ----

// UserChat records a message from the user and wakes the round driver: the
// user talking to the coordinator is a first-class wake reason, same as a
// task settling.
func (rs *RunSession) UserChat(text string, images ...string) error {
	text = strings.TrimSpace(text)
	if text == "" && len(images) == 0 {
		return fmt.Errorf("message is empty")
	}
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return fmt.Errorf("this run has ended; message the workflow to start a new one")
	}
	m := model.ChatMessage{Ts: time.Now(), From: "user", Text: text, Images: images}
	rs.run.Chat = append(rs.run.Chat, m)
	rs.pendingChat = append(rs.pendingChat, m)
	// An answer arrived: the coordinator is no longer waiting on the user.
	if rs.run.Coordinator != nil && rs.run.Coordinator.Status == "awaiting_user" {
		rs.run.Coordinator.Status = "working"
	}
	rs.notifyLocked()
	rs.mu.Unlock()
	rs.event("chat", "", "user → coordinator: "+firstLine(text, 120))
	return nil
}

// AskUser records a clarifying question from the coordinator to the user —
// the plan-mode interaction: gather what only the user knows, THEN propose.
// The question lands in the chat, the coordinator card flips to
// "awaiting_user", and the round driver parks until the user answers (their
// reply is a first-class wake reason). Counts as a ledger transition, so an
// ask-only round is acting, not stalling.
func (rs *RunSession) AskUser(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("question is empty: ask the user something concrete")
	}
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return fmt.Errorf("this run has ended")
	}
	rs.run.Chat = append(rs.run.Chat, model.ChatMessage{Ts: time.Now(), From: "coordinator", Text: text})
	if rs.run.Coordinator != nil {
		rs.run.Coordinator.Status = "awaiting_user"
	}
	rs.notifyLocked()
	rs.mu.Unlock()
	rs.event("user_question", "", "coordinator asked the user: "+firstLine(text, 120))
	return nil
}

// SetOutputDir points the run's deliverable folder at a user-chosen absolute
// path instead of the default output root — the user said where the work
// should land, and that wish is honored verbatim (no -2 suffixing: an existing
// directory is used as-is). Same freeze rules as SetOutputName.
func (rs *RunSession) SetOutputDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot resolve ~: %v", err)
		}
		dir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/"))
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("output dir must be an absolute path (or start with ~/); got %q", dir)
	}
	dir = filepath.Clean(dir)
	if home, err := os.UserHomeDir(); dir == "/" || (err == nil && dir == home) {
		return fmt.Errorf("output dir %q is too broad; use a dedicated subdirectory", dir)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.outputFrozen {
		return fmt.Errorf("the output folder is already %s and tasks have been dispatched against it; it cannot be moved", rs.run.OutputDir)
	}
	if rs.run.OutputDir == dir {
		return nil // already there
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %v", dir, err)
	}
	rs.releaseOutputDirLocked(dir)
	rs.outputBaseName = ""
	rs.run.OutputName = filepath.Base(dir)
	rs.run.OutputDir = dir
	rs.appendEventLocked("info", "", "output folder set by user request: "+dir)
	rs.notifyLocked()
	return nil
}

// TakeUserChat drains queued user messages for delivery into a round prompt.
func (rs *RunSession) TakeUserChat() []model.ChatMessage {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := rs.pendingChat
	rs.pendingChat = nil
	return out
}

// CoordinatorReply records the coordinator's round reply into the chat. It
// persists and publishes but deliberately does NOT count as a ledger
// transition: a coordinator that only talks, without moving the ledger, must
// still trip the no-progress guard.
func (rs *RunSession) CoordinatorReply(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	rs.mu.Lock()
	rs.run.Chat = append(rs.run.Chat, model.ChatMessage{Ts: time.Now(), From: "coordinator", Text: text})
	if rs.cfg.OnChange != nil {
		rs.cfg.OnChange(rs.run)
	}
	rs.mu.Unlock()
}

// InjectNotice queues a one-shot system notice for the coordinator and wakes
// the round driver, exactly like the stall watcher does. The engine uses it to
// give a quiet round one prompt correction before parking the loop.
func (rs *RunSession) InjectNotice(text string) {
	rs.mu.Lock()
	rs.pendingNotice = joinNotice(rs.pendingNotice, text)
	rs.notifyLocked()
	rs.mu.Unlock()
}

// AwaitRound blocks until there is something a fresh coordinator round should
// act on: a task settled into a state the previous round has not seen, a new
// user message, or a pending system notice. It returns the reason, or "" when
// the run ended or ctx expired. There is deliberately no "nothing happened"
// wake: a coordinator that ends its round without moving the ledger parks
// here until the user, the stall watcher, or a worker gives it a reason to
// think — never a hot loop of empty rounds. seen maps task id → the
// SettledFingerprint the previous round observed.
func (rs *RunSession) AwaitRound(ctx context.Context, seen map[string]string) string {
	for {
		ch := rs.waitChange()

		rs.mu.Lock()
		reason := ""
		if rs.pendingNotice != "" {
			reason = "notice"
		}
		if len(rs.pendingChat) > 0 {
			reason = "user"
		}
		for _, id := range rs.run.TaskOrder {
			t := rs.run.Tasks[id]
			if fp := settledFingerprintLocked(t); fp != "" && seen[id] != fp {
				reason = "settled"
			}
		}
		closed := rs.closed
		rs.mu.Unlock()

		if closed {
			return ""
		}
		if reason != "" {
			return reason
		}
		select {
		case <-ctx.Done():
			return ""
		case <-rs.finished:
			return ""
		case <-ch:
		}
	}
}

// ---- verdict ----

// Finish records the coordinator's verdict and ends the run.
func (rs *RunSession) Finish(v *Verdict) error {
	rs.mu.Lock()
	if rs.verdict != nil {
		rs.mu.Unlock()
		return fmt.Errorf("finish_run was already called for this run")
	}
	// Success may not be declared sight-unseen: if any completed task produced
	// artifacts, at least one must have been read via inspect. This is the
	// mechanical floor under "the coordinator verifies before signing off" —
	// a prompt can ask for more, but it can never deliver less.
	if v.Status == model.RunSucceeded && rs.inspections == 0 {
		for _, t := range rs.run.Tasks {
			if t.Status == model.TaskCompleted && len(t.Artifacts) > 0 {
				rs.mu.Unlock()
				return fmt.Errorf("cannot declare success without having read any deliverable: use the inspect tool " +
					"on at least one artifact in the exchange directory first, then call finish_run again")
			}
		}
	}
	rs.verdict = v
	if rs.run.Coordinator != nil {
		rs.run.Coordinator.Status = "done"
		rs.run.Coordinator.Decision = v.Summary
	}
	rs.notifyLocked()
	rs.mu.Unlock()
	rs.event("run_status", "", "coordinator verdict: "+v.Status+" — "+firstLine(v.Summary, 160))
	return nil
}

// CoordinatorActivity records what the coordinator is doing, for its card.
func (rs *RunSession) CoordinatorActivity(text string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.run.Coordinator != nil {
		rs.run.Coordinator.Activity = text
		if rs.cfg.OnChange != nil {
			rs.cfg.OnChange(rs.run)
		}
	}
}

// Snapshot of budget consumption, for the progress tool and the UI.
type BudgetStatus struct {
	TasksUsed       int    `json:"tasks_used"`
	TasksMax        int    `json:"tasks_max"`
	Running         int    `json:"running"`
	MaxParallel     int    `json:"max_parallel"`
	MaxDepth        int    `json:"max_delegation_depth"`
	MaxTurnsPerTask int    `json:"max_turns_per_task"`
	RunElapsedSec   int    `json:"run_elapsed_sec"`
	RunTimeoutSec   int    `json:"run_timeout_sec"`
	PeerHandoff     bool   `json:"peer_handoff_allowed"`
	AgentCreation   bool   `json:"agent_creation_allowed"`
	ApprovalState   string `json:"approval_state"`
}

func (rs *RunSession) BudgetStatus() BudgetStatus {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	running := 0
	for _, t := range rs.run.Tasks {
		if t.Status == model.TaskWorking || t.Status == model.TaskInputRequired {
			running++
		}
	}
	state := "not required"
	if rs.approvalNeeded {
		switch {
		case rs.approvalDone:
			state = "approved"
		case rs.approvalPending:
			state = "awaiting_decision"
		case rs.rejectedReason != "":
			state = "rejected"
		default:
			state = "pending" // propose_plan has not been called yet
		}
	}
	return BudgetStatus{
		TasksUsed: len(rs.run.Tasks), TasksMax: rs.budget.MaxTasks,
		Running: running, MaxParallel: rs.budget.MaxParallel,
		MaxDepth: rs.budget.MaxDelegationDepth, MaxTurnsPerTask: rs.budget.MaxTurnsPerTask,
		RunElapsedSec: int(time.Since(rs.run.CreatedAt).Seconds()), RunTimeoutSec: rs.budget.RunTimeoutSec,
		PeerHandoff: rs.budget.AllowPeerHandoff, AgentCreation: rs.budget.AllowAgentCreation,
		ApprovalState: state,
	}
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len([]rune(s)) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}
