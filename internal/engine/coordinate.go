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
// The coordinator is driven in ROUNDS over ONE live session per activation:
// the session keeps its own memory between rounds, and each round delivers
// only the delta (settled tasks, new user messages). The ledger is still the
// durable state — a session lost to a crash, a restart or a context overflow
// is rebuilt from it (plus the conversation record and the coordinator's
// notes), which is what keeps a dynamic run resumable without making every
// round pay the rebuild price.

// coordinate drives a dynamic run from coordinator start to verdict. opening
// carries user messages that triggered this activation (a reopened session's
// first message), delivered into the first round.
func (e *Engine) coordinate(ctx context.Context, h *handle, wf *model.Workflow, run *model.Run,
	pool []*model.Agent, dryRun bool, opening []model.ChatMessage) {

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
		run.Chat = append(run.Chat, model.ChatMessage{Ts: time.Now(), From: "user", Text: run.Goal, Images: run.GoalImages})
	}

	d := &dynamicRun{engine: e, run: run, wf: wf, dryRun: dryRun, sessions: sessions,
		workers: map[string]*workerHandle{}, pairs: map[string]*pairHandle{}, runCtx: ctx}
	for _, name := range wf.EffectivePairAgents() {
		d.pairs[name] = &pairHandle{}
	}

	rs := e.hub.OpenRun(ctx, hub.RunConfig{
		Run:           run,
		Workflow:      wf,
		Pool:          pool,
		Workspace:     e.runWorkspace(run),
		ProtectDir:    e.store.Dir(),
		PilotTools:    wf.Coordinator.EffectiveTools(),
		Exec:          d,
		OnChange:      func(r *model.Run) { e.store.SaveRun(r); e.publish(r) },
		OnCost:        func(entry store.CostEntry) { e.store.AppendCost(entry) },
		SaveAgent:     func(a *model.Agent) error { return e.materializeOne(wf, run, a) },
		SaveAmendment: func(am *model.Amendment) error { return e.store.SaveAmendment(am) },
		SaveLesson:    func(l *model.Lesson) error { return e.store.SaveLesson(l) },
		Templates:     e.templates,
	})
	d.rs = rs
	h.setRunSession(rs)
	// A fresh run's goal is a task by construction: the first thing the main
	// agent owes is an assessment. A resumed or reopened run keeps its last
	// one — the listener judges the reopening message like any other.
	if len(run.Assessments) == 0 {
		rs.RequireAssessment("the run has started: assess the goal")
	}
	for _, m := range opening {
		// Recorded, audited, and queued for the first round; feedback and
		// consolidation messages keep their special framing.
		switch m.Kind {
		case model.ChatFeedback:
			rs.UserFeedback(m.Text)
		case model.ChatConsolidate:
			rs.UserConsolidate(m.Text)
		default:
			recent := recentChat(run, 6)
			rs.UserChat(m.Text, m.Images...)
			if len(run.Assessments) > 0 {
				e.listen(ctx, rs, dryRun, recent, m.Text)
			}
		}
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
	return e.reactivate(runID, "", "", true)
}

// ReopenRun continues a finished dynamic session: the run IS the session, and
// a verdict is a milestone, not the end of the conversation. The new message
// wakes a fresh coordinator round over the same ledger, notes and chat.
func (e *Engine) ReopenRun(runID, text string, images ...llm.Image) (*model.Run, error) {
	return e.reactivate(runID, text, "", false, images...)
}

// ReopenFeedback wakes a finished dynamic session with the user's post-run
// feedback: the coordinator's job in that activation is to DIGEST it —
// clarify, persist facts/amendments, record the retrospective via
// conclude_feedback and propose rules — not to resume the work.
func (e *Engine) ReopenFeedback(runID, text string) (*model.Run, error) {
	return e.reactivate(runID, text, model.ChatFeedback, false)
}

// ReopenConsolidate wakes this workflow's newest finished real dynamic
// session for rule MAINTENANCE: the coordinator reads the standing rules in
// its system prompt and proposes merges/rewrites/retirements via
// propose_rules — every proposal still pending until the user confirms. The
// run is only the vehicle (rules are workflow-scoped); dry runs are skipped
// because their scripted coordinator cannot reason over the set.
func (e *Engine) ReopenConsolidate(wfID string) (*model.Run, error) {
	runs, err := e.store.ListRuns()
	if err != nil {
		return nil, err
	}
	for _, r := range runs { // newest first
		if r.WorkflowID != wfID || r.EffectiveMode() != model.ModeDynamic || r.EffectiveDryRun() || !r.Terminal() {
			continue
		}
		return e.reactivate(r.ID, "Consolidate this workflow's standing rules.", model.ChatConsolidate, false)
	}
	return nil, fmt.Errorf("this workflow has no finished real dynamic session to host the consolidation — complete one run first")
}

// reactivate brings a non-active dynamic run back to life. Completed
// (accepted) tasks are preserved; a fresh coordinator round picks up from the
// task tree — possible precisely because the coordinator carries no session
// state between rounds: the persisted run file is the whole session.
func (e *Engine) reactivate(runID, text, kind string, requireInterrupted bool, images ...llm.Image) (*model.Run, error) {
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
	if err := checkPairAgent(wf, pool); err != nil {
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
		switch kind {
		case model.ChatFeedback:
			msg = "session reopened by post-run feedback (postmortem)"
		case model.ChatConsolidate:
			msg = "session reopened for rule consolidation (maintenance)"
		}
	}
	run.Events = append(run.Events, model.Event{Ts: time.Now(), Type: "info", Msg: msg})
	e.store.SaveRun(run)

	ctx, cancel := context.WithCancel(context.Background())
	h := &handle{cancel: cancel, approveCh: make(chan struct{}), wfID: wf.ID}
	e.mu.Lock()
	e.active[run.ID] = h
	e.mu.Unlock()
	var opening []model.ChatMessage
	names, err := e.saveUploads(runID, images)
	if err != nil {
		return nil, err
	}
	if text != "" || len(names) > 0 {
		opening = []model.ChatMessage{{Kind: kind, Text: text, Images: names}}
	}
	go e.coordinate(ctx, h, wf, run, pool, dryRun, opening)
	return run, nil
}

// coordinatorAgentName is how the coordinator appears in the cost ledger.
const coordinatorAgentName = "(coordinator)"

// checkPairAgent verifies a configured resident implementer actually exists
// in the run's pool — failing at start beats a run whose pair tasks all die
// on "agent not found".
func checkPairAgent(wf *model.Workflow, pool []*model.Agent) error {
	for _, name := range wf.EffectivePairAgents() {
		found := false
		for _, a := range pool {
			if a.Name == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("pair agent %q is not in this workflow's pool", name)
		}
	}
	return nil
}

// dynamicRun holds the engine-side state of one dynamic run and implements
// hub.Executor.
type dynamicRun struct {
	engine   *Engine
	run      *model.Run
	wf       *model.Workflow
	dryRun   bool
	sessions llm.SessionBackend // coordinator's session host
	rs       *hub.RunSession
	runCtx   context.Context // run-scoped; hosts sessions that outlive one task

	mu      sync.Mutex
	workers map[string]*workerHandle
	wg      sync.WaitGroup

	// Resident partners (pair level): one persistent session per configured
	// agent, serving all of that agent's tasks in sequence. Each handle's mu
	// serializes its tasks and guards its session and token.
	pairs map[string]*pairHandle
}

// pairHandle is one resident partner's live session.
type pairHandle struct {
	mu      sync.Mutex
	sess    llm.Session
	backend string
	token   string
}

// isPair reports whether an agent is one of the run's resident partners.
func (d *dynamicRun) isPair(name string) bool {
	_, ok := d.pairs[name]
	return ok
}

type workerHandle struct {
	cancel context.CancelFunc
}

// runCoordinator drives decision rounds until a verdict or an error. One live
// session serves the whole activation: its first round carries the full
// context (conversation, notes, ledger), later rounds carry only the delta. A
// session that dies mid-activation is rebuilt from the ledger and the round
// retried once — the restart path, paid only when actually needed. There is no
// round limit — the run's wall clock is the termination guarantee; a
// coordinator that stops moving the ledger gets one corrective notice and then
// simply parks until something real happens.
func (d *dynamicRun) runCoordinator(ctx context.Context, rs *hub.RunSession, pool []*model.Agent, budget model.BudgetConfig) error {
	e := d.engine
	token := e.hub.IssueCoordinatorToken(d.run.ID)
	defer e.hub.RevokeToken(token)

	// The main agent LIVES in the run workspace — it is the pilot: its cwd is
	// the project, its hands are the configured tools, and the hook gate (not
	// the prompt) decides, per the run's level, when those hands may touch the
	// work. Its transcript still lands beside the run's records.
	workspace := rs.Workspace()
	transcriptDir := e.store.CoordinatorDir(d.run.ID)
	os.MkdirAll(transcriptDir, 0o755)
	pilotTools := d.wf.Coordinator.EffectiveTools()
	sysPrompt := hub.CoordinatorPrompt(d.run, d.wf, budget, rs.Workspace(), pool, e.LessonsFor(d.wf.ID))

	// The transcript survives reopens: earlier activations' rounds are the
	// audit trail of this session, not scratch to overwrite.
	var transcript strings.Builder
	if prev, err := os.ReadFile(e.store.NodeOutputPath(d.run.ID, "coordinator")); err == nil {
		transcript.Write(prev)
	}

	// The live session, opened lazily and kept across rounds. sess == nil
	// means the next round starts a fresh one, whose first prompt must carry
	// the full rebuilt context.
	var sess llm.Session
	defer func() {
		if sess != nil {
			sess.Close()
		}
	}()

	seen := map[string]string{}
	var changed []string                   // "id → status" summary for a rebuild prompt
	var changedIDs []string                // settled task ids for a continuation prompt
	quiet := 0                             // consecutive rounds without a ledger transition
	startRound := d.run.Coordinator.Rounds // > 0 when resuming
	userMsgs := rs.TakeUserChat()          // messages queued before the first round

	for i := 1; ; i++ {
		round := startRound + i
		rs.UpdateCoordinator(func(c *model.CoordinatorState) { c.Rounds = round })
		seqBefore := rs.Seq()

		fresh := sess == nil
		if fresh {
			s, err := d.sessions.Open(ctx, llm.SessionRequest{
				Kind:         llm.KindCoordinator,
				SystemPrompt: sysPrompt,
				Model:        d.run.Coordinator.Model,
				WorkDir:      workspace,
				AddDirs:      []string{workspace},
				Tools:        pilotTools,
				MCPServers:   e.hubServers(token),
				Gate:         e.gateHook(token),
				OnActivity:   func(text string) { rs.CoordinatorTrace(text) },
				OnText:       func(text string) { rs.CoordinatorDraft(text) },
			})
			if err != nil {
				rs.UpdateCoordinator(func(c *model.CoordinatorState) { c.Status = "failed" })
				return fmt.Errorf("open coordinator session: %w", err)
			}
			sess = s
		}
		start := time.Now()
		var prompt string
		if fresh {
			prompt = hub.RoundPrompt(d.run, rs, round, changed, userMsgs)
		} else {
			prompt = hub.ContinuationPrompt(d.run, rs, round, changedIDs, userMsgs)
		}
		// Deliver any pending system notice in the round prompt itself. Relying
		// on the tool-result piggyback (withNotice) alone hot-loops: a round
		// that makes no tool call leaves the notice pending, and AwaitRound
		// treats a pending notice as an immediate wake reason — forever.
		notice := rs.TakeNotice()
		if notice != "" {
			prompt += "\n## System notice\n" + notice + "\n"
		}
		// Goal images ride on the first prompt of each session (a fresh session
		// has never seen them; a live one still remembers them); chat images
		// ride on the round their message is delivered in.
		var imgNames []string
		if fresh {
			imgNames = append(imgNames, d.run.GoalImages...)
		}
		for _, m := range userMsgs {
			imgNames = append(imgNames, m.Images...)
		}
		res, perr := e.promptWithUploads(ctx, sess, d.run.ID, prompt, imgNames)
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
			// A LIVE session failing mid-activation (adapter crash, context
			// overflow) is exactly what the ledger rebuild exists for: drop the
			// session and retry this round fresh. Only a failure of the rebuilt
			// session itself — or a canceled run — fails the coordinator.
			// userMsgs stays undrained and the notice is re-queued: the failed
			// prompt delivered neither.
			if ctx.Err() == nil && !fresh {
				sess.Close()
				sess = nil
				if notice != "" {
					rs.InjectNotice(notice)
				}
				rs.AppendEvent("run_status", "", "coordinator session lost ("+firstLineOf(perr.Error())+"); rebuilding from the ledger")
				continue
			}
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
		changed, changedIDs = nil, nil
		for _, v := range rs.Views(nil) {
			if fp := hub.SettledFingerprint(v); fp != "" && seen[v.ID] != fp {
				changed = append(changed, v.ID+" → "+v.Status)
				changedIDs = append(changedIDs, v.ID)
			}
		}
	}
}

// firstLineOf clips an error message to its first line for the event log.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ChatToRun routes a user message into an active dynamic run's conversation.
// Attached images are persisted as run uploads and delivered to the
// coordinator inline with the round that carries the message.
func (e *Engine) ChatToRun(runID, text string, images ...llm.Image) error {
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
	names, err := e.saveUploads(runID, images)
	if err != nil {
		return err
	}
	recent := recentChat(rs.Run(), 6)
	if err := rs.UserChat(text, names...); err != nil {
		return err
	}
	// Classified in the background; delivery does not wait for the verdict.
	e.listen(context.Background(), rs, rs.Run().DryRun, recent, text)
	return nil
}

// recentChat is the last n messages of a run's conversation (for the
// listener's context). Callers must not mutate the result.
func recentChat(run *model.Run, n int) []model.ChatMessage {
	c := run.Chat
	if len(c) > n {
		c = c[len(c)-n:]
	}
	return append([]model.ChatMessage(nil), c...)
}

// saveUploads persists incoming chat images and returns their stored names.
func (e *Engine) saveUploads(runID string, images []llm.Image) ([]string, error) {
	if len(images) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(images))
	for i, img := range images {
		ext, ok := uploadExt[img.MimeType]
		if !ok {
			return nil, fmt.Errorf("unsupported image type %q (accepted: png, jpeg, webp, gif)", img.MimeType)
		}
		name := fmt.Sprintf("img-%d-%d%s", time.Now().UnixNano(), i+1, ext)
		if err := e.store.SaveUpload(runID, name, img.Data); err != nil {
			return nil, fmt.Errorf("store image: %w", err)
		}
		names = append(names, name)
	}
	return names, nil
}

// uploadExt maps the accepted image MIME types onto stored file extensions;
// mimeOfUpload is its inverse, keyed on the stored name.
var uploadExt = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif",
}

func mimeOfUpload(name string) string {
	for mime, ext := range uploadExt {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			return mime
		}
	}
	if strings.HasSuffix(strings.ToLower(name), ".jpeg") {
		return "image/jpeg"
	}
	return ""
}

// promptWithUploads sends one coordinator turn, attaching the named uploads
// inline when the session's transport can carry them. A backend without image
// support still gets the text — with an honest note instead of a silent drop.
func (e *Engine) promptWithUploads(ctx context.Context, sess llm.Session, runID, prompt string, names []string) (*llm.Result, error) {
	var imgs []llm.Image
	for _, name := range names {
		mime := mimeOfUpload(name)
		if mime == "" {
			continue
		}
		data, err := e.store.ReadUpload(runID, name)
		if err != nil {
			continue // recorded in chat but gone from disk: nothing to attach
		}
		imgs = append(imgs, llm.Image{Name: name, MimeType: mime, Data: data})
	}
	if len(imgs) == 0 {
		return sess.Prompt(ctx, prompt)
	}
	if is, ok := sess.(llm.ImageSession); ok {
		prompt += "\n## Attached images\nThe attached image(s) named above accompany this message.\n"
		return is.PromptImages(ctx, prompt, imgs)
	}
	prompt += fmt.Sprintf("\n## Attached images\nNOTE: the user attached %d image(s), but this runtime "+
		"cannot receive images — ask the user to describe the content instead.\n", len(imgs))
	return sess.Prompt(ctx, prompt)
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

// gateHook is a session's hook-gate credential: the same token that
// authenticates its hub tools, presented to the gate endpoint by the hook.
func (e *Engine) gateHook(token string) *llm.GateHook {
	return &llm.GateHook{URL: e.hub.GateEndpoint(), Token: token}
}

// hubServers renders the hub MCP endpoint for a session.
func (e *Engine) hubServers(token string) []llm.MCPServer {
	return []llm.MCPServer{{
		Name:    hub.ServerName,
		URL:     e.hub.MCPEndpoint(),
		Headers: map[string]string{hub.TokenHeader: token},
	}}
}

// sessionDirs is the directory allowlist for a worker session: the run
// workspace. Without this the prompt could name it and the runtime would still
// refuse to open it. Kept as a function so the allowlist has one definition.
func sessionDirs(workspace string) []string {
	return []string{workspace}
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
	for _, p := range d.pairs {
		p.mu.Lock()
		if p.sess != nil {
			p.sess.Close()
			p.sess = nil
		}
		if p.token != "" {
			d.engine.hub.RevokeToken(p.token)
			p.token = ""
		}
		p.mu.Unlock()
	}
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
	// A template task is a static sub-run, not a session.
	if strings.HasPrefix(view.Agent, model.TemplateAgentPrefix) {
		task, _ := rs.TaskSnapshot(taskID)
		d.execTemplateTask(ctx, rs, taskID, strings.TrimPrefix(view.Agent, model.TemplateAgentPrefix), task.Instruction)
		return
	}
	agent := d.resolveAgent(view.Agent)
	if agent == nil {
		rs.CompleteTask(taskID, "", nil, fmt.Errorf("agent %q not found in the pool", view.Agent))
		return
	}
	// A resident partner's tasks run on its own persistent session.
	if d.isPair(agent.Name) {
		d.execPairTask(ctx, rs, taskID, agent)
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
		AddDirs:      sessionDirs(workspace),
		Tools:        agent.Tools,
		MaxTurns:     agent.MaxTurns,
		MCPServers:   e.hubServers(token),
		Gate:         e.gateHook(token),
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
	prompt := hub.WorkerPrompt(&task, agent, d.run, workspace, e.store.AgentHome(agent.Name), budget.AllowPeerHandoff)
	d.runTaskTurns(taskCtx, ctx, rs, sess, taskID, taskModel, workerBackend.Name(), workspace, prompt, budget)
}

// templates lists the static workflows a main agent may run as one task.
func (e *Engine) templates() []*model.Workflow {
	wfs, err := e.store.ListWorkflows()
	if err != nil {
		return nil
	}
	var out []*model.Workflow
	for _, w := range wfs {
		if w.EffectiveMode() == model.ModeStatic {
			out = append(out, w)
		}
	}
	return out
}

// execTemplateTask drives a static workflow as ONE task of the dynamic run:
// a child run in the same workspace, planned and executed by the static
// engine; the task settles with the child's outcome. The dynamic run's
// budget counts it as one task (the template's own structure is bounded by
// its DAG), and a canceled parent cancels the child.
func (d *dynamicRun) execTemplateTask(ctx context.Context, rs *hub.RunSession, taskID, templateID, goal string) {
	e := d.engine
	wf, err := e.store.LoadWorkflow(templateID)
	if err != nil {
		rs.CompleteTask(taskID, "", nil, fmt.Errorf("template %q: %v", templateID, err))
		return
	}
	if wf.EffectiveMode() != model.ModeStatic {
		rs.CompleteTask(taskID, "", nil, fmt.Errorf("workflow %q is not a static template", templateID))
		return
	}
	child, err := e.startRun(wf, goal, rs.Workspace(), d.dryRun, d.run.ID, taskID)
	if err != nil {
		rs.CompleteTask(taskID, "", nil, fmt.Errorf("start template run: %v", err))
		return
	}
	rs.SetSubRun(taskID, child.ID)
	if err := rs.TaskStarted(taskID); err != nil {
		e.Cancel(child.ID)
		rs.CompleteTask(taskID, "", nil, err)
		return
	}
	rs.AppendEvent("template", taskID, fmt.Sprintf("template %s running as run %s", templateID, child.ID))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for e.IsActive(child.ID) {
		select {
		case <-ctx.Done():
			e.Cancel(child.ID)
			rs.CompleteTaskWith(taskID, "", nil, fmt.Errorf("template run canceled with its parent"), model.FailBlocked, nil)
			return
		case <-ticker.C:
		}
	}
	final, err := e.store.LoadRun(child.ID)
	if err != nil {
		rs.CompleteTask(taskID, "", nil, fmt.Errorf("template run %s: %v", child.ID, err))
		return
	}
	done, total := 0, 0
	for _, n := range final.Nodes {
		total++
		if n.Status == model.NodeSucceeded {
			done++
		}
	}
	summary := fmt.Sprintf("template %s (%s) → run %s: %s, %d/%d nodes succeeded", templateID, wf.Name, child.ID, final.Status, done, total)
	if final.Status == model.RunSucceeded {
		rs.CompleteTask(taskID, summary, nil, nil)
		return
	}
	msg := summary
	if final.Error != "" {
		msg += ": " + final.Error
	}
	rs.CompleteTaskWith(taskID, summary, nil, fmt.Errorf("%s", msg), model.FailBlocked, nil)
}

// pairNote frames a resident implementer's task on top of the standard worker
// prompt: same contract, plus the session's continuity.
const pairNote = `
## Resident session
You are one of this run's RESIDENT PARTNERS: this session persists across tasks. Your working
directory is the project itself — build on what you already know of that codebase from earlier
tasks instead of re-exploring it. Your task binding changes per task — report_result always settles
the CURRENT task described above, so call it exactly once per task.
`

// execPairTask runs one of the resident implementer's tasks on the shared
// pair session. Pair tasks serialize on pairMu: one session is one
// conversation — that is the point (context accumulates) and the constraint.
// The session opens lazily against the RUN context, survives task boundaries,
// and is dropped and rebuilt if a prompt fails.
func (d *dynamicRun) execPairTask(ctx context.Context, rs *hub.RunSession, taskID string, agent *model.Agent) {
	e := d.engine
	p := d.pairs[agent.Name]
	p.mu.Lock()
	defer p.mu.Unlock()

	budget := rs.Budget()
	taskCtx, cancelTask := context.WithTimeout(ctx, time.Duration(budget.TaskTimeoutSec)*time.Second)
	defer cancelTask()

	if p.token == "" {
		p.token = e.hub.IssuePairToken(d.run.ID, agent.Name)
	}
	workspace := rs.Workspace()
	if p.sess == nil {
		backend, err := e.runtimeFor(agent.Runtime, d.dryRun)
		if err != nil {
			rs.CompleteTask(taskID, "", nil, err)
			return
		}
		sess, err := llm.Sessions(backend).Open(d.runCtx, llm.SessionRequest{
			Kind:         llm.KindWorker,
			SystemPrompt: agent.SystemPrompt,
			// One session, one model: the agent's own default. The
			// coordinator's per-task tiering cannot switch a live session.
			Model: agent.Model,
			// The pair session lives IN the workspace: that is what makes a
			// resident implementer accumulate real understanding of the
			// codebase across tasks.
			WorkDir:    workspace,
			AddDirs:    sessionDirs(workspace),
			Tools:      agent.Tools,
			MaxTurns:   agent.MaxTurns,
			MCPServers: e.hubServers(p.token),
			Gate:       e.gateHook(p.token),
			OnActivity: func(text string) {
				if id := rs.PairTask(agent.Name); id != "" {
					rs.TaskActivity(id, text)
				}
			},
		})
		if err != nil {
			rs.CompleteTask(taskID, "", nil, fmt.Errorf("open pair session: %w", err))
			return
		}
		p.sess = sess
		p.backend = backend.Name()
	}

	if err := rs.TaskStarted(taskID); err != nil {
		rs.CompleteTask(taskID, "", nil, err)
		return
	}
	task, ok := rs.TaskSnapshot(taskID)
	if !ok {
		return
	}
	prompt := hub.WorkerPrompt(&task, agent, d.run, workspace, e.store.AgentHome(agent.Name), budget.AllowPeerHandoff) + pairNote

	rs.SetPairTask(agent.Name, taskID)
	defer rs.SetPairTask(agent.Name, "")
	if broken := d.runTaskTurns(taskCtx, ctx, rs, p.sess, taskID, agent.Model, p.backend, workspace, prompt, budget); broken {
		// The task was already settled by runTaskTurns; only the vehicle is
		// gone. The next pair task gets a fresh session (with its own cold
		// start — the price of the failure, not of the design).
		p.sess.Close()
		p.sess = nil
		rs.AppendEvent("run_status", taskID, "pair session lost; the next pair task will rebuild it")
	}
}

// runTaskTurns drives one task's prompt turns on an open session until the
// task settles: initial instruction, a turn per steering batch, then the
// report/envelope/acceptance settlement. It returns true when the session
// itself failed (prompt error, timeout, cancellation) — a per-task session
// just closes then, a resident session must be dropped and rebuilt.
//
// The envelope contract is the same as static mode's — no envelope means the
// task failed, never "probably fine". And an "ok" envelope is only a claim:
// the task completes when its acceptance checks pass, and not before.
func (d *dynamicRun) runTaskTurns(taskCtx, ctx context.Context, rs *hub.RunSession, sess llm.Session,
	taskID, taskModel, backendName, workspace, prompt string, budget model.BudgetConfig) bool {

	e := d.engine
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
			if res.Usage.Empty() && res.CostUSD == 0 && backendName == "acp" {
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
				return true
			}
			if ctx.Err() != nil {
				rs.CompleteTask(taskID, "", nil, fmt.Errorf("canceled"))
				return true
			}
			rs.CompleteTask(taskID, "", nil, err)
			return true
		}
		// The structured report_result call is the primary channel; a trailing
		// envelope in the reply text is the fallback for sessions that never
		// made the call (older agents, tool trouble).
		if rep, ok := rs.TakeReportedResult(taskID); ok {
			lastEnv = envelope{Status: rep.Status, FailureKind: rep.FailureKind,
				Summary: rep.Summary, Artifacts: rep.Artifacts}
			haveEnv = true
		} else {
			lastEnv, haveEnv = parseEnvelope(res.Text)
		}

		// Steering that arrived mid-turn is delivered now: the task continues
		// for another turn and any result from this turn is superseded,
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
				"agent ended its turn (stop reason: %s) without reporting a result (no report_result call, no trailing envelope) — likely stopped early, e.g. after repeated tool errors; output tail: %s", stop, tail),
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
		return false
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
