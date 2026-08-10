// Package engine drives workflow runs: planning via the workflow's main agent,
// DAG scheduling of executor nodes with parallelism/retries/timeouts, an
// optional approval gate between plan and execution, and audited replanning on
// node failure.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"loom/internal/hub"
	"loom/internal/llm"
	"loom/internal/model"
	"loom/internal/planner"
	"loom/internal/store"
)

const (
	defaultParallelism = 3
	defaultNodeTimeout = 30 * time.Minute
	defaultMaxNodes    = 8
)

type Engine struct {
	store    *store.Store
	backends map[string]llm.Backend
	broker   *Broker
	hub      *hub.Hub

	// outputRoot is where dynamic runs' named deliverable folders live.
	outputRoot string

	mu     sync.Mutex
	active map[string]*handle
}

// SetOutputRoot points dynamic-run deliverables at dir (~/workflow-output by
// default at the CLI).
func (e *Engine) SetOutputRoot(dir string) { e.outputRoot = dir }

// OutputRoot reports the configured deliverable root (for prompt previews).
func (e *Engine) OutputRoot() string { return e.outputRoot }

// handle is the control surface of one active run. static runs are released
// through approveCh; dynamic runs have no plan to approve up front, so their
// gate lives on the ledger session instead.
type handle struct {
	cancel      context.CancelFunc
	approveOnce sync.Once
	approveCh   chan struct{}
	wfID        string

	mu sync.Mutex
	rs *hub.RunSession
}

func (h *handle) setRunSession(rs *hub.RunSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rs = rs
}

func (h *handle) runSession() *hub.RunSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rs
}

func New(st *store.Store, backends map[string]llm.Backend, broker *Broker, orchestration *hub.Hub) *Engine {
	return &Engine{store: st, backends: backends, broker: broker, hub: orchestration, active: map[string]*handle{}}
}

// RecoverInterrupted marks runs left non-terminal by a previous process as
// interrupted; they can be resumed with RetryNode.
func (e *Engine) RecoverInterrupted() {
	runs, err := e.store.ListRuns()
	if err != nil {
		return
	}
	for _, r := range runs {
		if !r.Terminal() {
			r.Status = model.RunInterrupted
			for _, ns := range r.Nodes {
				if ns.Status == model.NodeRunning {
					ns.Status = model.NodeFailed
					ns.Error = "server restarted mid-node"
				}
			}
			// Dynamic runs: in-flight tasks lost their sessions; accepted work
			// is untouched and the run can be resumed from the ledger.
			for _, t := range r.Tasks {
				if !model.TaskTerminal(t.Status) {
					t.Status = model.TaskFailed
					t.Error = "interrupted by server restart"
					t.FailureKind = model.FailBlocked
					t.EndedAt = time.Now()
				}
			}
			msg := "server restarted mid-run; run marked interrupted"
			if r.EffectiveMode() == model.ModeDynamic {
				msg += " (resumable: completed tasks are preserved in the ledger)"
			}
			r.Events = append(r.Events, model.Event{Ts: time.Now(), Type: "error", Msg: msg})
			e.store.SaveRun(r)
		}
	}
}

// runtimeFor resolves the backend that hosts one agent's sessions. Dry-run
// short-circuits everything to the mock — that is a property of the run, not
// of any agent. Otherwise the agent's declared runtime picks from the
// registry: today only "claude" exists; codex etc. become registry entries.
func (e *Engine) runtimeFor(agentRuntime string, dryRun bool) (llm.Backend, error) {
	if dryRun {
		if be := e.backends["mock"]; be != nil {
			return be, nil
		}
		return nil, fmt.Errorf("dry-run requested but no mock backend is registered")
	}
	name := agentRuntime
	if name == "" {
		name = model.DefaultRuntime
	}
	be := e.backends[name]
	if be == nil {
		return nil, fmt.Errorf("runtime %q is not available on this server", name)
	}
	return be, nil
}

// StartRun creates a run and drives it asynchronously from planning onward.
// dryRun swaps every execution — planner, coordinator, workers — for the mock;
// which real runtime each agent uses is the agent's own declaration. Images
// attached to the goal message are persisted as run uploads and shown to a
// dynamic run's coordinator (static mode has no conversation to carry them).
func (e *Engine) StartRun(wf *model.Workflow, goal string, dryRun bool, images ...llm.Image) (*model.Run, error) {
	if _, err := e.runtimeFor("", dryRun); err != nil {
		return nil, err // fail fast: the default runtime must exist
	}
	pool, err := e.resolvePool(wf)
	if err != nil {
		return nil, err
	}
	backendLabel := model.DefaultRuntime
	if dryRun {
		backendLabel = "mock"
	}
	if wf.EffectiveMode() == model.ModeDynamic {
		if err := e.checkDynamic(dryRun); err != nil {
			return nil, err
		}
	}
	run, err := e.store.NewRun(wf, goal, backendLabel)
	if err != nil {
		return nil, err
	}
	run.DryRun = dryRun
	if names, err := e.saveUploads(run.ID, images); err != nil {
		return nil, err
	} else if len(names) > 0 {
		run.GoalImages = names
		e.store.SaveRun(run)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &handle{cancel: cancel, approveCh: make(chan struct{}), wfID: wf.ID}
	e.mu.Lock()
	e.active[run.ID] = h
	e.mu.Unlock()
	if wf.EffectiveMode() == model.ModeDynamic {
		go e.coordinate(ctx, h, wf, run, pool, dryRun, nil)
	} else {
		go e.drive(ctx, h, wf, run, pool, dryRun, true)
	}
	return run, nil
}

// checkDynamic refuses a dynamic run whose coordinator runtime cannot host the
// hub toolset. Without MCP tools a coordinator has no way to delegate at all,
// so starting one would produce a confusing empty run rather than a clear error.
func (e *Engine) checkDynamic(dryRun bool) error {
	if e.hub == nil {
		return fmt.Errorf("dynamic mode is not available: no orchestration hub configured")
	}
	be, err := e.runtimeFor("", dryRun)
	if err != nil {
		return err
	}
	if _, ok := be.(llm.SessionBackend); !ok {
		return fmt.Errorf("runtime %q cannot run dynamic workflows: it has no session support, "+
			"so the coordinator would have no orchestration tools", be.Name())
	}
	return nil
}

// Approve releases a run parked at an approval gate, in either mode.
func (e *Engine) Approve(runID string) error {
	e.mu.Lock()
	h := e.active[runID]
	e.mu.Unlock()
	if h == nil {
		return fmt.Errorf("run %s is not active", runID)
	}
	if rs := h.runSession(); rs != nil && rs.AwaitingApproval() {
		return rs.Approve()
	}
	h.approveOnce.Do(func() { close(h.approveCh) })
	return nil
}

// Reject refuses a dynamic run's proposed plan. In static mode rejection is
// expressed by canceling, which the UI already does.
func (e *Engine) Reject(runID, reason string) error {
	e.mu.Lock()
	h := e.active[runID]
	e.mu.Unlock()
	if h == nil {
		return fmt.Errorf("run %s is not active", runID)
	}
	rs := h.runSession()
	if rs == nil || !rs.AwaitingApproval() {
		return fmt.Errorf("run %s is not awaiting approval", runID)
	}
	if reason == "" {
		reason = "rejected by the operator"
	}
	return rs.Reject(reason)
}

// SendToTask injects an operator message into a live dynamic task — the human
// equivalent of the coordinator's send_message.
func (e *Engine) SendToTask(runID, taskID, text string) error {
	e.mu.Lock()
	h := e.active[runID]
	e.mu.Unlock()
	if h == nil {
		return fmt.Errorf("run %s is not active", runID)
	}
	rs := h.runSession()
	if rs == nil {
		return fmt.Errorf("run %s is not a dynamic run", runID)
	}
	_, err := rs.Send(taskID, "user", text)
	return err
}

// IsActive reports whether a run is currently being driven by this process.
func (e *Engine) IsActive(runID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active[runID] != nil
}

// Cancel stops an active run.
func (e *Engine) Cancel(runID string) error {
	e.mu.Lock()
	h := e.active[runID]
	e.mu.Unlock()
	if h == nil {
		return fmt.Errorf("run %s is not active", runID)
	}
	h.cancel()
	return nil
}

// RetryNode resumes a terminal run: the given node and everything downstream
// of it are reset to pending and the DAG is re-driven (planning is skipped).
func (e *Engine) RetryNode(runID, nodeID string) (*model.Run, error) {
	e.mu.Lock()
	if e.active[runID] != nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("run %s is still active", runID)
	}
	e.mu.Unlock()

	run, err := e.store.LoadRun(runID)
	if err != nil {
		return nil, err
	}
	if !run.Terminal() {
		return nil, fmt.Errorf("run %s is not in a terminal state", runID)
	}
	if run.EffectiveMode() == model.ModeDynamic {
		return nil, fmt.Errorf("dynamic runs cannot be retried per task: the coordinator session that would " +
			"consume the result is gone. Start a new run instead")
	}
	if run.Plan == nil {
		return nil, fmt.Errorf("run %s has no plan to resume", runID)
	}
	if run.Nodes[nodeID] == nil {
		return nil, fmt.Errorf("unknown node %q", nodeID)
	}
	wf, err := e.store.LoadWorkflow(run.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("workflow of this run no longer exists: %w", err)
	}
	dryRun := run.EffectiveDryRun()
	if _, err := e.runtimeFor("", dryRun); err != nil {
		return nil, err
	}
	pool, err := e.resolvePool(wf)
	if err != nil {
		return nil, err
	}

	for _, id := range e.withDownstream(run.Plan, nodeID) {
		ns := run.Nodes[id]
		if ns.Status != model.NodeSucceeded || id == nodeID {
			run.Nodes[id] = &model.NodeState{Status: model.NodePending}
		}
	}
	run.Status = model.RunRunning
	run.Error = ""
	run.EndedAt = time.Time{}
	run.Events = append(run.Events, model.Event{Ts: time.Now(), Type: "info", Node: nodeID, Msg: "manual retry from node " + nodeID})
	e.store.SaveRun(run)

	ctx, cancel := context.WithCancel(context.Background())
	h := &handle{cancel: cancel, approveCh: make(chan struct{}), wfID: wf.ID}
	e.mu.Lock()
	e.active[run.ID] = h
	e.mu.Unlock()
	go e.drive(ctx, h, wf, run, pool, dryRun, false)
	return run, nil
}

// withDownstream returns nodeID plus every node transitively depending on it.
func (e *Engine) withDownstream(p *model.Plan, nodeID string) []string {
	dependents := map[string][]string{}
	for _, n := range p.Nodes {
		for _, d := range n.DependsOn {
			dependents[d] = append(dependents[d], n.ID)
		}
	}
	seen := map[string]bool{nodeID: true}
	queue := []string{nodeID}
	out := []string{nodeID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range dependents[cur] {
			if !seen[d] {
				seen[d] = true
				queue = append(queue, d)
				out = append(out, d)
			}
		}
	}
	return out
}

func (e *Engine) resolvePool(wf *model.Workflow) ([]*model.Agent, error) {
	all, err := e.store.ListAgents()
	if err != nil {
		return nil, err
	}
	if len(wf.AgentPool) == 0 {
		if len(all) == 0 {
			return nil, fmt.Errorf("agent pool is empty")
		}
		return all, nil
	}
	byName := map[string]*model.Agent{}
	for _, a := range all {
		byName[a.Name] = a
	}
	var out []*model.Agent
	for _, name := range wf.AgentPool {
		a, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("workflow references unknown agent %q", name)
		}
		out = append(out, a)
	}
	return out, nil
}

// ---- run driving ----

// event appends to the run's audit log, persists, and publishes a snapshot.
func (e *Engine) event(run *model.Run, typ, node, msg string) {
	run.Events = append(run.Events, model.Event{Ts: time.Now(), Type: typ, Node: node, Msg: msg})
	e.store.SaveRun(run)
	e.publish(run)
}

// publish broadcasts a snapshot without persisting — for high-frequency
// live-activity updates that don't belong in the audit log.
func (e *Engine) publish(run *model.Run) {
	if data, err := json.Marshal(run); err == nil {
		e.broker.Publish(run.ID, data)
	}
}

// plannerFor picks the backend used for plan assembly: dry runs plan on the
// mock; real runs plan via the CLI single-shot backend (a tool-less completion
// needs no live session), falling back to the default runtime.
func (e *Engine) plannerFor(dryRun bool) llm.Backend {
	if dryRun {
		return e.backends["mock"]
	}
	if be, ok := e.backends["planner"]; ok {
		return be
	}
	return e.backends[model.DefaultRuntime]
}

func (e *Engine) finish(run *model.Run, status, errMsg string) {
	run.Status = status
	run.Error = errMsg
	run.EndedAt = time.Now()
	msg := "run " + status
	if errMsg != "" {
		msg += ": " + errMsg
	}
	e.event(run, "run_status", "", msg)
	e.mu.Lock()
	delete(e.active, run.ID)
	e.mu.Unlock()
}

func (e *Engine) drive(ctx context.Context, h *handle, wf *model.Workflow, run *model.Run,
	pool []*model.Agent, dryRun bool, fromPlanning bool) {

	if fromPlanning {
		plan, err := e.plan(ctx, wf, run, pool, e.plannerFor(dryRun), "")
		if err != nil {
			if ctx.Err() != nil {
				e.finish(run, model.RunCanceled, "")
			} else {
				e.finish(run, model.RunFailed, "planning failed: "+err.Error())
			}
			return
		}
		run.Plan = plan
		for _, n := range plan.Nodes {
			run.Nodes[n.ID] = &model.NodeState{Status: model.NodePending}
		}
		e.event(run, "run_status", "", fmt.Sprintf("plan assembled: %d nodes", len(plan.Nodes)))

		if wf.RequireApproval {
			run.Status = model.RunAwaitingApproval
			e.event(run, "run_status", "", "awaiting approval")
			select {
			case <-ctx.Done():
				e.finish(run, model.RunCanceled, "")
				return
			case <-h.approveCh:
				e.event(run, "info", "", "plan approved")
			}
		}
	}

	if err := e.materializeAgents(wf, run); err != nil {
		e.finish(run, model.RunFailed, "agent creation failed: "+err.Error())
		return
	}

	run.Status = model.RunRunning
	e.event(run, "run_status", "", "executing")
	e.execute(ctx, wf, run, pool, dryRun)
}

// materializeAgents persists planner-proposed agents into the shared pool
// (idempotent — an already-existing agent is reused untouched) and, when the
// workflow uses an explicit pool subset, extends it so future runs see them.
func (e *Engine) materializeAgents(wf *model.Workflow, run *model.Run) error {
	if run.Plan == nil {
		return nil
	}
	return e.materialize(wf, run, run.Plan.Agents)
}

func (e *Engine) materialize(wf *model.Workflow, run *model.Run, proposed []model.Agent) error {
	if len(proposed) == 0 {
		return nil
	}
	var added []string
	for i := range proposed {
		a := proposed[i]
		if _, err := e.store.LoadAgent(a.Name); err == nil {
			e.event(run, "info", "", fmt.Sprintf("agent %q already in pool; reusing existing definition", a.Name))
			continue
		}
		if err := e.store.SaveAgent(&a); err != nil {
			return err
		}
		added = append(added, a.Name)
		e.event(run, "agent_created", "", fmt.Sprintf("planner created agent %q (model %s, tools %q)", a.Name, a.Model, a.Tools))
	}
	if len(added) > 0 && len(wf.AgentPool) > 0 {
		wf.AgentPool = append(wf.AgentPool, added...)
		if err := e.store.SaveWorkflow(wf); err != nil {
			return err
		}
		e.event(run, "info", "", "workflow agent pool extended with: "+strings.Join(added, ", "))
	}
	return nil
}

// plan asks the workflow's main agent for a DAG; one retry with the error fed
// back if the first reply fails parsing or validation.
func (e *Engine) plan(ctx context.Context, wf *model.Workflow, run *model.Run,
	pool []*model.Agent, backend llm.Backend, prior string) (*model.Plan, error) {

	poolNames := make([]string, len(pool))
	for i, a := range pool {
		poolNames[i] = a.Name
	}
	maxNodes := wf.Planner.MaxNodes
	if maxNodes <= 0 {
		maxNodes = defaultMaxNodes
	}
	prompt := planner.BuildPrompt(run.Goal, pool, wf.Planner, prior, wf.AllowAgentCreation)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			prompt += fmt.Sprintf("\n\nYour previous reply was rejected: %v. Reply again following the output contract exactly.", lastErr)
		}
		res, err := backend.Complete(ctx, llm.Request{
			Kind:   llm.KindPlan,
			Prompt: prompt,
			Model:  wf.Planner.Model,
			Pool:   poolNames,
		})
		if res != nil {
			modelID := res.Model
			if modelID == "" {
				modelID = wf.Planner.Model
			}
			e.recordCost(run, "planner", "planner", "(planner)", modelID, res.Usage, res.CostUSD)
		}
		if err != nil {
			return nil, err
		}
		p, err := planner.Parse(res.Text)
		if err == nil {
			err = planner.Validate(p, pool, maxNodes, wf.AllowAgentCreation)
		}
		if err == nil {
			return p, nil
		}
		lastErr = err
		e.event(run, "error", "", "planner reply rejected: "+err.Error())
	}
	return nil, lastErr
}

type nodeResult struct {
	id        string
	agent     string
	model     string
	attempts  int
	summary   string
	artifacts []string
	cost      float64
	usage     model.TokenUsage
	durMs     int64
	err       error
}

// recordCost prices one unit of work into the run and the global ledger. The
// ledger is what makes per-agent and per-workflow roll-ups cheap; the run
// totals are what the run page shows.
func (e *Engine) recordCost(run *model.Run, unit, unitID, agent, modelID string, usage model.TokenUsage, cost float64) {
	run.CostUSD += cost
	run.Usage.Add(usage)
	e.store.AppendCost(store.CostEntry{
		WorkflowID: run.WorkflowID, RunID: run.ID, Unit: unit, UnitID: unitID,
		Agent: agent, Model: modelID, Usage: usage, CostUSD: cost,
	})
}

// nodeActivity is a live "what is this node doing" signal (tool calls etc.),
// published to the UI but not persisted or audited.
type nodeActivity struct {
	id   string
	text string
}

// execute schedules the DAG until completion, cancellation, or failure,
// replanning on failure when the workflow allows it.
func (e *Engine) execute(ctx context.Context, wf *model.Workflow, run *model.Run,
	pool []*model.Agent, dryRun bool) {

	parallelism := wf.Parallelism
	if parallelism <= 0 {
		parallelism = defaultParallelism
	}
	timeout := defaultNodeTimeout
	if wf.NodeTimeoutSec > 0 {
		timeout = time.Duration(wf.NodeTimeoutSec) * time.Second
	}
	// Agent resolution is lazy: planner-created agents (including ones grafted
	// by a replan) live in the store, not necessarily in the workflow's pool.
	agents := map[string]*model.Agent{}
	for _, a := range pool {
		agents[a.Name] = a
	}
	resolveAgent := func(name string) *model.Agent {
		if a, ok := agents[name]; ok {
			return a
		}
		a, err := e.store.LoadAgent(name)
		if err != nil {
			return nil
		}
		agents[name] = a
		return a
	}

	results := make(chan nodeResult)
	activities := make(chan nodeActivity, 16)
	running := 0
	replanNeeded := false
	failedNode := ""

	nodeByID := func(id string) *model.PlanNode {
		for i := range run.Plan.Nodes {
			if run.Plan.Nodes[i].ID == id {
				return &run.Plan.Nodes[i]
			}
		}
		return nil
	}

	for {
		// Cascade skips: a pending node with a dead dependency can never run.
		for changed := true; changed; {
			changed = false
			for _, n := range run.Plan.Nodes {
				ns := run.Nodes[n.ID]
				if ns.Status != model.NodePending {
					continue
				}
				for _, d := range n.DependsOn {
					switch run.Nodes[d].Status {
					case model.NodeFailed, model.NodeSkipped, model.NodeCanceled:
						ns.Status = model.NodeSkipped
						e.event(run, "node_status", n.ID, "skipped: upstream "+d+" did not succeed")
						changed = true
					}
				}
			}
		}

		// Launch ready nodes unless we're draining toward a replan/cancel.
		if ctx.Err() == nil && !replanNeeded {
			for _, n := range run.Plan.Nodes {
				if running >= parallelism {
					break
				}
				ns := run.Nodes[n.ID]
				if ns.Status != model.NodePending {
					continue
				}
				ready := true
				for _, d := range n.DependsOn {
					if run.Nodes[d].Status != model.NodeSucceeded {
						ready = false
						break
					}
				}
				if !ready {
					continue
				}
				agent := resolveAgent(n.Agent)
				if agent == nil {
					ns.Status = model.NodeFailed
					ns.Error = fmt.Sprintf("agent %q not found in pool", n.Agent)
					e.event(run, "node_status", n.ID, ns.Error)
					continue
				}
				backend, err := e.runtimeFor(agent.Runtime, dryRun)
				if err != nil {
					ns.Status = model.NodeFailed
					ns.Error = err.Error()
					e.event(run, "node_status", n.ID, ns.Error)
					continue
				}
				ns.Status = model.NodeRunning
				ns.StartedAt = time.Now()
				running++
				e.event(run, "node_status", n.ID, fmt.Sprintf("running (agent %s)", n.Agent))
				prompt := e.buildNodePrompt(run, n, agent, nodeByID)
				go e.execNode(ctx, run, n, agent, backend, timeout, wf.MaxRetries, prompt, results, activities)
			}
		}

		if running == 0 {
			if ctx.Err() != nil {
				e.cancelRemaining(run)
				e.finish(run, model.RunCanceled, "")
				return
			}
			if replanNeeded {
				if !e.replan(ctx, wf, run, pool, failedNode) {
					e.finish(run, model.RunFailed, "node "+failedNode+" failed and replanning did not recover")
					return
				}
				replanNeeded = false
				failedNode = ""
				continue
			}
			// No running, nothing launchable: the run is complete. Nodes
			// superseded by a replan don't count against success.
			allOK := true
			for _, ns := range run.Nodes {
				if ns.Status != model.NodeSucceeded && !ns.Superseded {
					allOK = false
					break
				}
			}
			if allOK {
				e.finish(run, model.RunSucceeded, "")
			} else {
				e.finish(run, model.RunFailed, "one or more nodes did not succeed")
			}
			return
		}

		var res nodeResult
		select {
		case act := <-activities:
			if ns := run.Nodes[act.id]; ns != nil && ns.Status == model.NodeRunning {
				ns.Activity = act.text
				e.publish(run)
			}
			continue
		case res = <-results:
		}
		running--
		ns := run.Nodes[res.id]
		ns.Activity = ""
		ns.Attempts = res.attempts
		ns.CostUSD += res.cost
		ns.Usage.Add(res.usage)
		ns.DurationMs += res.durMs
		ns.EndedAt = time.Now()
		e.recordCost(run, "node", res.id, res.agent, res.model, res.usage, res.cost)
		if res.err != nil {
			ns.Status = model.NodeFailed
			ns.Error = res.err.Error()
			e.event(run, "node_status", res.id, fmt.Sprintf("failed after %d attempt(s): %v", res.attempts, res.err))
			if ctx.Err() == nil && wf.ReplanEnabled && run.Replans < wf.MaxReplans && !replanNeeded {
				replanNeeded = true
				failedNode = res.id
				e.event(run, "info", res.id, "will replan after in-flight nodes finish")
			}
		} else {
			ns.Status = model.NodeSucceeded
			ns.Summary = res.summary
			ns.Artifacts = res.artifacts
			e.event(run, "node_status", res.id, "succeeded")
		}
	}
}

func (e *Engine) cancelRemaining(run *model.Run) {
	for id, ns := range run.Nodes {
		if ns.Status == model.NodePending || ns.Status == model.NodeRunning {
			ns.Status = model.NodeCanceled
			e.event(run, "node_status", id, "canceled")
		}
	}
}

// replan marks leftover pending nodes skipped, asks the planner for a new plan
// over the remaining work, and grafts it onto the run under a new generation
// prefix. Returns false if replanning itself failed.
func (e *Engine) replan(ctx context.Context, wf *model.Workflow, run *model.Run,
	pool []*model.Agent, failedNode string) bool {

	for id, ns := range run.Nodes {
		if ns.Status == model.NodePending {
			ns.Status = model.NodeSkipped
			e.event(run, "node_status", id, "skipped for replan")
		}
	}

	var prior strings.Builder
	prior.WriteString("Already completed:\n")
	for _, n := range run.Plan.Nodes {
		ns := run.Nodes[n.ID]
		if ns.Status == model.NodeSucceeded {
			fmt.Fprintf(&prior, "- %s (%s): %s\n", n.Title, n.Agent, ns.Summary)
		}
	}
	if fn := run.Nodes[failedNode]; fn != nil {
		var title string
		for _, n := range run.Plan.Nodes {
			if n.ID == failedNode {
				title = n.Title
			}
		}
		fmt.Fprintf(&prior, "\nFailed node: %s — error: %s\n", title, fn.Error)
	}

	run.Status = model.RunReplanning
	e.event(run, "replan", failedNode, "replanning after failure of "+failedNode)

	// The replanner's registry includes agents created by earlier generations.
	effectivePool := append([]*model.Agent{}, pool...)
	for i := range run.Plan.Agents {
		effectivePool = append(effectivePool, &run.Plan.Agents[i])
	}
	plan, err := e.plan(ctx, wf, run, effectivePool, e.plannerFor(run.EffectiveDryRun()), prior.String())
	if err != nil {
		e.event(run, "error", "", "replan failed: "+err.Error())
		return false
	}
	if err := e.materialize(wf, run, plan.Agents); err != nil {
		e.event(run, "error", "", "agent creation failed during replan: "+err.Error())
		return false
	}
	run.Plan.Agents = append(run.Plan.Agents, plan.Agents...)

	gen := run.Replans + 1
	prefix := fmt.Sprintf("r%d.", gen)
	for i := range plan.Nodes {
		n := &plan.Nodes[i]
		n.ID = prefix + n.ID
		for j := range n.DependsOn {
			n.DependsOn[j] = prefix + n.DependsOn[j]
		}
		run.Plan.Nodes = append(run.Plan.Nodes, *n)
		run.Nodes[n.ID] = &model.NodeState{Status: model.NodePending}
	}
	// The failed node and everything skipped because of it are now covered by
	// the new generation; mark them superseded so they don't fail the run.
	for _, ns := range run.Nodes {
		if !ns.Superseded && (ns.Status == model.NodeFailed || ns.Status == model.NodeSkipped) {
			ns.Superseded = true
		}
	}
	run.Replans = gen
	run.Status = model.RunRunning
	e.event(run, "replan", "", fmt.Sprintf("replan #%d grafted %d new nodes", gen, len(plan.Nodes)))
	return true
}

// buildNodePrompt renders the executor prompt: instruction, upstream context
// (dependency summaries; for replanned nodes also all earlier successes), the
// workspace note, and the output-envelope contract.
func (e *Engine) buildNodePrompt(run *model.Run, n model.PlanNode, agent *model.Agent, nodeByID func(string) *model.PlanNode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are executor agent %q running node %q (%s) of a workflow run.\n\n", n.Agent, n.Title, n.ID)
	fmt.Fprintf(&b, "## Overall goal\n%s\n\n## Your task\n%s\n", run.Goal, n.Instruction)

	wrote := false
	writeCtx := func(id string) {
		dep := nodeByID(id)
		ns := run.Nodes[id]
		if dep == nil || ns == nil || ns.Status != model.NodeSucceeded {
			return
		}
		if !wrote {
			if agent.Independent {
				b.WriteString("\n## Upstream artifacts to verify\n" +
					"You are an independent verifier: form your own view from the artifacts alone.\n")
			} else {
				b.WriteString("\n## Context from completed upstream nodes\n")
			}
			wrote = true
		}
		if agent.Independent {
			// No upstream self-summaries: an author's report of its own work is
			// exactly the contamination a fresh-context verifier exists to avoid.
			if len(ns.Artifacts) > 0 {
				fmt.Fprintf(&b, "- %s (%s): %s\n", dep.Title, dep.Agent, strings.Join(ns.Artifacts, ", "))
			}
			return
		}
		fmt.Fprintf(&b, "### %s (%s)\n%s\n", dep.Title, dep.Agent, ns.Summary)
		if len(ns.Artifacts) > 0 {
			fmt.Fprintf(&b, "Artifacts: %s\n", strings.Join(ns.Artifacts, ", "))
		}
	}
	deps := map[string]bool{}
	for _, d := range n.DependsOn {
		deps[d] = true
		writeCtx(d)
	}
	// Replanned nodes also see all earlier successes: their instructions were
	// written against that prior progress.
	if strings.HasPrefix(n.ID, "r") && strings.Contains(n.ID, ".") {
		for _, other := range run.Plan.Nodes {
			if other.ID != n.ID && !deps[other.ID] {
				writeCtx(other.ID)
			}
		}
	}

	toolNote := "You have NO tools: complete the task entirely in your reply text."
	if agent.Tools != "" {
		toolNote = fmt.Sprintf(`You have EXACTLY these tools: %s. Nothing else.
- Access the exchange directory with your file tools using ABSOLUTE paths (e.g. Read/Write the full path directly). Do not try to list it first if you lack a shell.
- Any tool not listed above (including Bash/Terminal) will be REJECTED — do not attempt it; work within your granted tools.`, agent.Tools)
	}
	fmt.Fprintf(&b, `
## Your tools
%s

## Directories
- Your current directory is your OWN persistent workspace (private to you as an agent; survives across runs). Use it for notes and scratch work.
- This run's shared exchange directory is: %s
  Upstream artifacts are there. Every deliverable of this task MUST be written there.

## Output contract
End your reply with a fenced json block:
`+"```json"+`
{"status": "ok", "summary": "<self-contained summary of what you did and produced>", "artifacts": ["<paths relative to the exchange directory>"]}
`+"```"+`
Use "status": "error" with an explanatory summary if you could not complete the task. This envelope is REQUIRED — a reply without it is treated as a failed node.
`, toolNote, e.store.RunWorkspace(run.ID))
	return b.String()
}

// The body match is lazy, not [^`]*: a backtick inside a JSON string (e.g. a
// summary quoting `a command`) must not break the match. Non-envelope fenced
// JSON is filtered below by requiring "status" and a clean unmarshal.
var envelopeRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

type envelope struct {
	Status      string   `json:"status"`
	FailureKind string   `json:"failure_kind"` // dynamic mode: enumerated failure classification
	Summary     string   `json:"summary"`
	Artifacts   []string `json:"artifacts"`
}

// parseEnvelope extracts the trailing result envelope. found=false means the
// agent never delivered one — callers must treat that as a failed node, not a
// success: a turn that dies early (e.g. after a rejected tool call) produces
// exactly this shape, and defaulting it to "ok" silently green-lights a node
// that did nothing.
func parseEnvelope(text string) (envelope, bool) {
	ms := envelopeRe.FindAllStringSubmatch(text, -1)
	for i := len(ms) - 1; i >= 0; i-- {
		body := ms[i][1]
		if !strings.Contains(body, `"status"`) {
			continue
		}
		env := envelope{Status: "ok"}
		if err := json.Unmarshal([]byte(body), &env); err == nil {
			if env.Status == "" {
				env.Status = "ok"
			}
			return env, true
		}
	}
	return envelope{Status: "ok"}, false
}

// execNode runs one node with retries and reports on the results channel.
// It never touches run state directly — the execute loop owns that.
func (e *Engine) execNode(ctx context.Context, run *model.Run, n model.PlanNode,
	agent *model.Agent, backend llm.Backend, timeout time.Duration, maxRetries int,
	prompt string, results chan<- nodeResult, activities chan<- nodeActivity) {

	out := nodeResult{id: n.ID, agent: n.Agent, model: agent.Model}
	onActivity := func(text string) {
		select { // never block the agent on a slow UI
		case activities <- nodeActivity{id: n.ID, text: text}:
		default:
		}
	}
	var transcript strings.Builder
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		out.attempts = attempt
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		res, err := backend.Complete(attemptCtx, llm.Request{
			Kind:         llm.KindNode,
			SystemPrompt: agent.SystemPrompt,
			Prompt:       prompt,
			Model:        agent.Model,
			WorkDir:      e.store.AgentHome(agent.Name),
			AddDirs:      []string{e.store.RunWorkspace(run.ID)},
			Tools:        agent.Tools,
			MaxTurns:     agent.MaxTurns,
			OnActivity:   onActivity,
		})
		cancel()
		if res != nil {
			out.cost += res.CostUSD
			out.usage.Add(res.Usage)
			out.durMs += res.DurationMs
			if res.Model != "" {
				out.model = res.Model
			}
			body := res.Transcript
			if body == "" {
				body = res.Text
			}
			fmt.Fprintf(&transcript, "## Attempt %d\n\n%s\n\n", attempt, body)
		}
		if err == nil {
			env, found := parseEnvelope(res.Text)
			switch {
			case !found:
				tail := strings.TrimSpace(res.Text)
				if len(tail) > 300 {
					tail = "…" + tail[len(tail)-300:]
				}
				err = fmt.Errorf("agent ended without a result envelope (likely stopped early, e.g. after a rejected tool call); output tail: %s", tail)
			case env.Status != "ok":
				err = fmt.Errorf("agent reported error: %s", env.Summary)
			default:
				out.summary = env.Summary
				out.artifacts = env.Artifacts
			}
			if err == nil {
				break
			}
		}
		out.err = err
		if ctx.Err() != nil {
			break
		}
		if attempt <= maxRetries {
			out.err = nil
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
	e.store.WriteNodeOutput(run.ID, n.ID, transcript.String())
	results <- out
}
