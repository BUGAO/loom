package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"loom/internal/hub"
	"loom/internal/llm"
	"loom/internal/model"
	"loom/internal/store"
)

// Dynamic-mode driving.
//
// static mode's engine decides everything: which node runs when, what happens
// on failure. dynamic mode inverts that — the coordinator decides, and the
// engine is reduced to five jobs it must never delegate: run the sessions,
// enforce the budget, execute the acceptance contracts, keep the ledger, and
// stream it out. The engine below is deliberately dumb about strategy and
// strict about limits.
//
// The coordinator is driven in ROUNDS, each a fresh session whose context is
// rebuilt from the ledger (plus its own recorded notes). The coordinator holds
// no conversation history across rounds — the ledger is the state, which is
// also what makes a dynamic run resumable after a process restart.

// coordinate drives a dynamic run from coordinator start to verdict. opening
// carries user messages that triggered this activation (a reopened session's
// first message), delivered into the first round.
func (e *Engine) coordinate(ctx context.Context, h *handle, wf *model.Workflow, run *model.Run,
	pool []*model.Agent, dryRun bool, opening []string) {

	budget := wf.EffectiveBudget()

	// The run wall clock is a hard ceiling, not a suggestion: it is the last
	// guarantee of termination once the DAG's acyclicity is gone.
	ctx, cancelRun := context.WithTimeout(ctx, time.Duration(budget.RunTimeoutSec)*time.Second)
	defer cancelRun()

	// The coordinator runs on the default runtime (it is loom's own role, not
	// a pool agent); dry-run swaps it for the mock like everything else.
	coordBackend, err := e.runtimeFor("", dryRun)
	if err != nil {
		e.finish(run, model.RunFailed, err.Error())
		return
	}
	sessions := llm.Sessions(coordBackend)
	coordModel := model.DefaultModel
	if wf.Coordinator != nil && wf.Coordinator.Model != "" {
		coordModel = wf.Coordinator.Model
	}
	// A resumed run keeps its coordinator state (cost, rounds so far); a fresh
	// run starts one.
	if run.Coordinator == nil {
		run.Coordinator = &model.CoordinatorState{Agent: coordinatorAgentName, Model: coordModel}
	}
	run.Coordinator.Status = "working"
	run.Coordinator.Model = coordModel

	// The goal is the conversation's opening message; a resumed run keeps its
	// existing chat.
	if len(run.Chat) == 0 {
		run.Chat = append(run.Chat, model.ChatMessage{Ts: time.Now(), From: "user", Text: run.Goal})
	}

	d := &dynamicRun{engine: e, run: run, wf: wf, dryRun: dryRun, sessions: sessions, workers: map[string]*workerHandle{}}

	rs := e.hub.OpenRun(ctx, hub.RunConfig{
		Run:        run,
		Workflow:   wf,
		Pool:       pool,
		Workspace:  e.store.RunWorkspace(run.ID),
		OutputRoot: e.outputRoot,
		Exec:       d,
		OnChange:   func(r *model.Run) { e.store.SaveRun(r); e.publish(r) },
		OnCost:     func(entry store.CostEntry) { e.store.AppendCost(entry) },
		SaveAgent:  func(a *model.Agent) error { return e.materializeOne(wf, run, a) },
	})
	d.rs = rs
	h.setRunSession(rs)
	for _, m := range opening {
		rs.UserChat(m) // recorded, audited, and queued for the first round
	}

	rs.AppendEvent("run_status", "", fmt.Sprintf("coordinator started (%s)", coordModel))
	err = d.runCoordinator(ctx, rs, pool, budget)

	// The coordinator is done talking. Everything still in flight is now
	// unattended work — nobody will consume its result — so it is stopped.
	rs.Close()
	d.waitWorkers()

	verdict := rs.Verdict()
	switch {
	case verdict != nil:
		status := verdict.Status
		msg := verdict.Summary
		if len(verdict.Artifacts) > 0 {
			msg += " | artifacts: " + strings.Join(verdict.Artifacts, ", ")
		}
		if status == model.RunSucceeded {
			e.finish(run, model.RunSucceeded, "")
			e.event(run, "info", "", "verdict: "+msg)
		} else {
			e.finish(run, model.RunFailed, msg)
		}
	case ctx.Err() == context.DeadlineExceeded:
		e.finish(run, model.RunFailed, fmt.Sprintf("run exceeded its %ds wall clock before the coordinator delivered a verdict", budget.RunTimeoutSec))
	case ctx.Err() != nil:
		e.finish(run, model.RunCanceled, "")
	case err != nil:
		e.finish(run, model.RunFailed, "coordinator failed: "+err.Error())
	default:
		// A coordinator that stops without finish_run has abandoned the run.
		// Calling that a success because nothing crashed would be the dynamic
		// equivalent of treating a missing envelope as "ok".
		e.finish(run, model.RunFailed, "coordinator ended without calling finish_run — no verdict was delivered")
	}
}

// ResumeRun restarts an interrupted dynamic run from its persisted ledger.
func (e *Engine) ResumeRun(runID string) (*model.Run, error) {
	return e.reactivate(runID, "", true)
}

// ReopenRun continues a finished dynamic session: the run IS the session, and
// a verdict is a milestone, not the end of the conversation. The new message
// wakes a fresh coordinator round over the same ledger, notes and chat.
func (e *Engine) ReopenRun(runID, text string) (*model.Run, error) {
	return e.reactivate(runID, text, false)
}

// reactivate brings a non-active dynamic run back to life. Completed
// (accepted) tasks are preserved; a fresh coordinator round picks up from the
// task tree — possible precisely because the coordinator carries no session
// state between rounds: the persisted run file is the whole session.
func (e *Engine) reactivate(runID, text string, requireInterrupted bool) (*model.Run, error) {
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
	if run.EffectiveMode() != model.ModeDynamic {
		return nil, fmt.Errorf("run %s is not a dynamic run; use retry-from-node instead", runID)
	}
	if requireInterrupted && run.Status != model.RunInterrupted {
		return nil, fmt.Errorf("run %s is %s; only interrupted runs can be resumed", runID, run.Status)
	}
	if !run.Terminal() {
		return nil, fmt.Errorf("run %s is %s; it is not in a reopenable state", runID, run.Status)
	}
	wf, err := e.store.LoadWorkflow(run.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("workflow of this run no longer exists: %w", err)
	}
	dryRun := run.EffectiveDryRun()
	if err := e.checkDynamic(dryRun); err != nil {
		return nil, err
	}
	pool, err := e.resolvePool(wf)
	if err != nil {
		return nil, err
	}

	// Any task that was in flight when the previous activation died has lost
	// its session; it is failed as blocked (rework-eligible) so the
	// coordinator can decide whether to redo it.
	for _, t := range run.Tasks {
		if !model.TaskTerminal(t.Status) {
			t.Status = model.TaskFailed
			t.Error = "interrupted by server restart"
			t.FailureKind = model.FailBlocked
			t.EndedAt = time.Now()
		}
	}
	run.Status = model.RunRunning
	run.Error = ""
	run.EndedAt = time.Time{}
	msg := "run resumed from the task ledger"
	if !requireInterrupted {
		msg = "session reopened by a user message"
	}
	run.Events = append(run.Events, model.Event{Ts: time.Now(), Type: "info", Msg: msg})
	e.store.SaveRun(run)

	ctx, cancel := context.WithCancel(context.Background())
	h := &handle{cancel: cancel, approveCh: make(chan struct{}), wfID: wf.ID}
	e.mu.Lock()
	e.active[run.ID] = h
	e.mu.Unlock()
	var opening []string
	if text != "" {
		opening = []string{text}
	}
	go e.coordinate(ctx, h, wf, run, pool, dryRun, opening)
	return run, nil
}

// coordinatorAgentName is how the coordinator appears in the cost ledger.
const coordinatorAgentName = "(coordinator)"

// dynamicRun holds the engine-side state of one dynamic run and implements
// hub.Executor.
type dynamicRun struct {
	engine   *Engine
	run      *model.Run
	wf       *model.Workflow
	dryRun   bool
	sessions llm.SessionBackend // coordinator's session host
	rs       *hub.RunSession

	mu      sync.Mutex
	workers map[string]*workerHandle
	wg      sync.WaitGroup
}

type workerHandle struct {
	cancel context.CancelFunc
}

// runCoordinator drives decision rounds until a verdict or an error. Each
// round is a fresh session: the coordinator's only state is the ledger and its
// recorded notes. There is no round limit — the run's wall clock is the
// termination guarantee; a coordinator that stops moving the ledger gets one
// corrective notice and then simply parks until something real happens.
func (d *dynamicRun) runCoordinator(ctx context.Context, rs *hub.RunSession, pool []*model.Agent, budget model.BudgetConfig) error {
	e := d.engine
	token := e.hub.IssueCoordinatorToken(d.run.ID)
	defer e.hub.RevokeToken(token)

	workspace := e.store.RunWorkspace(d.run.ID)
	outDir := ""
	if dir, named := rs.OutputInfo(); named {
		outDir = dir
	}
	sysPrompt := hub.CoordinatorPrompt(d.run, d.wf, budget, e.outputRoot, outDir, pool)

	// The transcript survives reopens: earlier activations' rounds are the
	// audit trail of this session, not scratch to overwrite.
	var transcript strings.Builder
	if prev, err := os.ReadFile(e.store.NodeOutputPath(d.run.ID, "coordinator")); err == nil {
		transcript.Write(prev)
	}

	seen := map[string]string{}
	var changed []string
	quiet := 0                             // consecutive rounds without a ledger transition
	startRound := d.run.Coordinator.Rounds // > 0 when resuming
	userMsgs := rs.TakeUserChat()          // messages queued before the first round

	for i := 1; ; i++ {
		round := startRound + i
		rs.UpdateCoordinator(func(c *model.CoordinatorState) { c.Rounds = round })
		seqBefore := rs.Seq()

		sess, err := d.sessions.Open(ctx, llm.SessionRequest{
			Kind:         llm.KindCoordinator,
			SystemPrompt: sysPrompt,
			Model:        d.run.Coordinator.Model,
			WorkDir:      workspace,
			AddDirs:      []string{workspace},
			// No file tools at all: the coordinator's only read access to the
			// work is the hub's audited inspect tool, which is also what makes
			// "verified before sign-off" a checkable fact instead of a hope.
			Tools:      "",
			MCPServers: e.hubServers(token),
			OnActivity: func(text string) { rs.CoordinatorActivity(text) },
		})
		if err != nil {
			rs.UpdateCoordinator(func(c *model.CoordinatorState) { c.Status = "failed" })
			return fmt.Errorf("open coordinator session: %w", err)
		}
		start := time.Now()
		res, perr := sess.Prompt(ctx, hub.RoundPrompt(d.run, rs, round, changed, userMsgs))
		sess.Close()
		if res != nil {
			modelID := res.Model
			if modelID == "" {
				modelID = d.run.Coordinator.Model
			}
			rs.RecordCoordinatorCost(coordinatorAgentName, modelID, res.Usage, res.CostUSD, time.Since(start).Milliseconds())
			body := res.Transcript
			if body == "" {
				body = res.Text
			}
			fmt.Fprintf(&transcript, "## Round %d\n\n%s\n\n", round, body)
			e.store.WriteNodeOutput(d.run.ID, "coordinator", transcript.String())
			// The round's final text is the coordinator's chat reply.
			rs.CoordinatorReply(res.Text)
		}
		rs.UpdateCoordinator(func(c *model.CoordinatorState) { c.Activity = "" })
		if perr != nil {
			rs.UpdateCoordinator(func(c *model.CoordinatorState) { c.Status = "failed" })
			return perr
		}
		if rs.Verdict() != nil {
			rs.UpdateCoordinator(func(c *model.CoordinatorState) { c.Status = "done" })
			return nil
		}
		if ctx.Err() != nil {
			rs.UpdateCoordinator(func(c *model.CoordinatorState) { c.Status = "failed" })
			return nil // outer switch reports cancellation/timeout
		}

		// Mark everything settled as seen by this round.
		for _, v := range rs.Views(nil) {
			if fp := hub.SettledFingerprint(v); fp != "" {
				seen[v.ID] = fp
			}
		}

		// A round that neither finished nor touched the ledger gets exactly one
		// immediate corrective round; if that one is quiet too, the loop parks
		// in AwaitRound until the user, a worker, or the stall watcher speaks.
		// Failing the run here would kill sessions over a recoverable slip
		// (e.g. a refused delegate the model didn't retry).
		if rs.Seq() == seqBefore {
			quiet++
		} else {
			quiet = 0
		}
		if quiet == 1 {
			rs.InjectNotice("SYSTEM: your last round ended without delegating, messaging, or finishing. " +
				"If a tool call was refused, fix it per the error and retry. Delegate the next piece of work, " +
				"answer what is pending, or call finish_run — do not end another round without acting.")
		}

		rs.AppendEvent("round", "", fmt.Sprintf("round %d ended without a verdict; waiting for the ledger", round))
		if reason := rs.AwaitRound(ctx, seen); reason == "" {
			rs.UpdateCoordinator(func(c *model.CoordinatorState) { c.Status = "failed" })
			return nil // run closed or ctx done; outer switch decides
		}

		userMsgs = rs.TakeUserChat()
		changed = nil
		for _, v := range rs.Views(nil) {
			if fp := hub.SettledFingerprint(v); fp != "" && seen[v.ID] != fp {
				changed = append(changed, v.ID+" → "+v.Status)
			}
		}
	}
}

// ChatToRun routes a user message into an active dynamic run's conversation.
func (e *Engine) ChatToRun(runID, text string) error {
	e.mu.Lock()
	h := e.active[runID]
	e.mu.Unlock()
	if h == nil {
		return fmt.Errorf("run %s is not active; message the workflow to start a new conversation", runID)
	}
	rs := h.runSession()
	if rs == nil {
		return fmt.Errorf("run %s is not a dynamic run", runID)
	}
	return rs.UserChat(text)
}

// ActiveDynamicRun returns the id of the workflow's currently active dynamic
// run, or "" — the chat endpoint uses it to decide between continuing a
// conversation and starting one.
func (e *Engine) ActiveDynamicRun(wfID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, h := range e.active {
		if h.wfID == wfID && h.runSession() != nil {
			return id
		}
	}
	return ""
}

// hubServers renders the hub MCP endpoint for a session.
func (e *Engine) hubServers(token string) []llm.MCPServer {
	return []llm.MCPServer{{
		Name:    hub.ServerName,
		URL:     e.hub.MCPEndpoint(),
		Headers: map[string]string{hub.TokenHeader: token},
	}}
}

// StartTask implements hub.Executor: the ledger decided this task may run.
func (d *dynamicRun) StartTask(rs *hub.RunSession, taskID string) {
	tctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	if d.workers[taskID] != nil {
		d.mu.Unlock()
		cancel()
		return
	}
	d.workers[taskID] = &workerHandle{cancel: cancel}
	d.mu.Unlock()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer cancel()
		d.execTask(tctx, rs, taskID)
	}()
}

// CancelTask implements hub.Executor.
func (d *dynamicRun) CancelTask(_ *hub.RunSession, taskID string) {
	d.mu.Lock()
	w := d.workers[taskID]
	d.mu.Unlock()
	if w != nil {
		w.cancel()
	}
}

func (d *dynamicRun) waitWorkers() {
	d.mu.Lock()
	for _, w := range d.workers {
		w.cancel()
	}
	d.mu.Unlock()
	d.wg.Wait()
}

// execTask runs one worker task to a terminal state.
//
// A task is one live session across possibly several turns: the initial
// instruction, plus a turn for any steering that arrived while it was busy.
// The envelope contract is the same as static mode's — no envelope means the
// task failed, never "probably fine". And an "ok" envelope is only a claim:
// the task completes when its acceptance checks pass, and not before.
func (d *dynamicRun) execTask(ctx context.Context, rs *hub.RunSession, taskID string) {
	e := d.engine
	view, ok := rs.View(taskID)
	if !ok {
		return
	}
	agent := d.resolveAgent(view.Agent)
	if agent == nil {
		rs.CompleteTask(taskID, "", nil, fmt.Errorf("agent %q not found in the pool", view.Agent))
		return
	}
	budget := rs.Budget()
	taskCtx, cancelTask := context.WithTimeout(ctx, time.Duration(budget.TaskTimeoutSec)*time.Second)
	defer cancelTask()

	token := e.hub.IssueWorkerToken(d.run.ID, taskID)
	defer e.hub.RevokeToken(token)

	// The worker's session host is the agent's own declaration; the run only
	// decided real vs dry.
	workerBackend, err := e.runtimeFor(agent.Runtime, d.dryRun)
	if err != nil {
		rs.CompleteTask(taskID, "", nil, err)
		return
	}
	// The exchange directory is frozen by the dispatch that started this task.
	workspace := rs.Workspace()
	// The task carries the model resolved at delegation (the coordinator's
	// per-task tier choice, or the agent default); legacy tasks fall back to
	// the agent's model.
	taskModel := view.Model
	if taskModel == "" {
		taskModel = agent.Model
	}
	sess, err := llm.Sessions(workerBackend).Open(taskCtx, llm.SessionRequest{
		Kind:         llm.KindWorker,
		SystemPrompt: agent.SystemPrompt,
		Model:        taskModel,
		WorkDir:      e.store.AgentHome(agent.Name),
		AddDirs:      []string{workspace},
		Tools:        agent.Tools,
		MaxTurns:     agent.MaxTurns,
		MCPServers:   e.hubServers(token),
		OnActivity:   func(text string) { rs.TaskActivity(taskID, text) },
	})
	if err != nil {
		rs.CompleteTask(taskID, "", nil, fmt.Errorf("open worker session: %w", err))
		return
	}
	defer sess.Close()

	if err := rs.TaskStarted(taskID); err != nil {
		rs.CompleteTask(taskID, "", nil, err)
		return
	}

	task, ok := rs.TaskSnapshot(taskID)
	if !ok {
		return
	}
	prompt := hub.WorkerPrompt(&task, agent, d.run, workspace, budget.AllowPeerHandoff)

	var transcript strings.Builder
	var lastEnv envelope
	var haveEnv bool
	for {
		used, max := rs.TurnSpent(taskID)
		res, err := sess.Prompt(taskCtx, prompt)
		if res != nil {
			modelID := res.Model
			if modelID == "" {
				modelID = taskModel
			}
			rs.RecordTaskCost(taskID, modelID, res.Usage, res.CostUSD, res.DurationMs)
			if res.Usage.Empty() && res.CostUSD == 0 && workerBackend.Name() == "acp" {
				rs.AppendEvent("cost_unavailable", taskID, "token usage could not be read from the session transcript; cost recorded as 0")
			}
			body := res.Transcript
			if body == "" {
				body = res.Text
			}
			fmt.Fprintf(&transcript, "## Turn %d\n\n%s\n\n", used, body)
		}
		if err != nil {
			e.store.WriteNodeOutput(d.run.ID, taskID, transcript.String())
			if taskCtx.Err() == context.DeadlineExceeded {
				rs.CompleteTaskWith(taskID, "", nil,
					fmt.Errorf("task exceeded its %ds timeout", budget.TaskTimeoutSec), model.FailBlocked, nil)
				return
			}
			if ctx.Err() != nil {
				rs.CompleteTask(taskID, "", nil, fmt.Errorf("canceled"))
				return
			}
			rs.CompleteTask(taskID, "", nil, err)
			return
		}
		lastEnv, haveEnv = parseEnvelope(res.Text)

		// Steering that arrived mid-turn is delivered now: the task continues
		// for another turn and any envelope from this turn is superseded,
		// because the task plainly is not finished (docs/DECISIONS-v2.md D4).
		if inbox := rs.TakeInbox(taskID); len(inbox) > 0 && used < max {
			prompt = hub.FollowupPrompt(inbox)
			continue
		}
		e.store.WriteNodeOutput(d.run.ID, taskID, transcript.String())
		switch {
		case !haveEnv:
			tail := strings.TrimSpace(res.Text)
			if len(tail) > 300 {
				tail = "…" + tail[len(tail)-300:]
			}
			stop := res.StopReason
			if stop == "" {
				stop = "unknown"
			}
			rs.CompleteTaskWith(taskID, "", nil, fmt.Errorf(
				"agent ended its turn (stop reason: %s) without a result envelope — likely stopped early, e.g. after repeated tool errors; output tail: %s", stop, tail),
				model.FailUnspecified, nil)
		case lastEnv.Status != "ok":
			kind := lastEnv.FailureKind
			if !model.ValidFailureKind(kind) {
				kind = model.FailUnspecified
			}
			rs.CompleteTaskWith(taskID, lastEnv.Summary, lastEnv.Artifacts,
				fmt.Errorf("agent reported error: %s", lastEnv.Summary), kind, nil)
		default:
			// The claim is "ok"; the verdict is the acceptance contract's —
			// read at completion time, so an amended contract judges the work.
			results, pass := hub.RunChecks(taskCtx, workspace, rs.AcceptanceOf(taskID))
			if pass {
				rs.CompleteTaskWith(taskID, lastEnv.Summary, lastEnv.Artifacts, nil, "", results)
			} else {
				rs.CompleteTaskWith(taskID, lastEnv.Summary, lastEnv.Artifacts,
					fmt.Errorf("worker claimed success but the acceptance contract failed: %s", hub.SummarizeResults(results)),
					model.FailBlocked, results)
			}
		}
		return
	}
}

func (d *dynamicRun) resolveAgent(name string) *model.Agent {
	for _, a := range d.rs.PoolAgents() {
		if a.Name == name {
			return a
		}
	}
	a, err := d.engine.store.LoadAgent(name)
	if err != nil {
		return nil
	}
	return a
}

// materializeOne persists a coordinator-created agent, mirroring what static
// mode does for planner-created ones (including extending an explicit pool).
func (e *Engine) materializeOne(wf *model.Workflow, run *model.Run, a *model.Agent) error {
	if _, err := e.store.LoadAgent(a.Name); err == nil {
		return fmt.Errorf("agent %q already exists in the pool; delegate to it instead", a.Name)
	}
	if err := e.store.SaveAgent(a); err != nil {
		return err
	}
	if len(wf.AgentPool) > 0 {
		wf.AgentPool = append(wf.AgentPool, a.Name)
		if err := e.store.SaveWorkflow(wf); err != nil {
			return err
		}
	}
	return nil
}
