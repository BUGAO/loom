// Package server exposes the REST + SSE API and serves the embedded web UI.
package server

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"loom/internal/engine"
	"loom/internal/hub"
	"loom/internal/llm"
	"loom/internal/model"
	"loom/internal/planner"
	"loom/internal/store"
)

//go:embed web
var webFS embed.FS

type Server struct {
	store         *store.Store
	engine        *engine.Engine
	broker        *engine.Broker
	hub           http.Handler // dynamic-mode MCP + A2A surfaces
	defaultDryRun bool         // pre-check the dry-run switch in the UI (-backend mock)
}

func New(st *store.Store, eng *engine.Engine, broker *engine.Broker, hubHandler http.Handler, defaultDryRun bool) *Server {
	return &Server{store: st, engine: eng, broker: broker, hub: hubHandler, defaultDryRun: defaultDryRun}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/meta", s.getMeta)

	mux.HandleFunc("GET /api/agents", s.listAgents)
	mux.HandleFunc("POST /api/agents", s.saveAgent)
	mux.HandleFunc("GET /api/agents/{name}", s.getAgent)
	mux.HandleFunc("DELETE /api/agents/{name}", s.deleteAgent)
	mux.HandleFunc("GET /api/agents/{name}/skills/{skill}", s.getSkill)
	mux.HandleFunc("POST /api/agents/{name}/skills/{skill}", s.saveSkill)
	mux.HandleFunc("DELETE /api/agents/{name}/skills/{skill}", s.deleteSkill)

	mux.HandleFunc("GET /api/workflows", s.listWorkflows)
	mux.HandleFunc("POST /api/workflows", s.saveWorkflow)
	mux.HandleFunc("POST /api/workflows/prompt-preview", s.promptPreview)
	mux.HandleFunc("GET /api/workflows/{id}", s.getWorkflow)
	mux.HandleFunc("DELETE /api/workflows/{id}", s.deleteWorkflow)
	mux.HandleFunc("POST /api/workflows/{id}/runs", s.startRun)
	mux.HandleFunc("POST /api/workflows/{id}/chat", s.chatWorkflow)
	mux.HandleFunc("POST /api/runs/{id}/chat", s.chatRun)

	mux.HandleFunc("GET /api/costs/summary", s.costSummary)

	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("DELETE /api/runs/{id}", s.deleteRun)
	mux.HandleFunc("POST /api/runs/{id}/approve", s.approveRun)
	mux.HandleFunc("POST /api/runs/{id}/reject", s.rejectRun)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancelRun)
	mux.HandleFunc("POST /api/runs/{id}/retry/{node}", s.retryNode)
	mux.HandleFunc("POST /api/runs/{id}/resume", s.resumeRun)
	mux.HandleFunc("POST /api/runs/{id}/feedback", s.setRunFeedback)
	mux.HandleFunc("POST /api/runs/{id}/tasks/{task}/message", s.sendTaskMessage)

	mux.HandleFunc("GET /api/amendments", s.listAmendments)
	mux.HandleFunc("POST /api/amendments/{id}/approve", s.approveAmendment)
	mux.HandleFunc("POST /api/amendments/{id}/reject", s.rejectAmendment)

	mux.HandleFunc("GET /api/lessons", s.listLessons)
	mux.HandleFunc("POST /api/workflows/{id}/lessons", s.addLesson)
	mux.HandleFunc("POST /api/workflows/{id}/lessons/consolidate", s.consolidateLessons)
	mux.HandleFunc("POST /api/lessons/{id}/approve", s.approveLesson)
	mux.HandleFunc("POST /api/lessons/{id}/reject", s.rejectLesson)
	mux.HandleFunc("PUT /api/lessons/{id}", s.editLesson)
	mux.HandleFunc("DELETE /api/lessons/{id}", s.deleteLesson)
	mux.HandleFunc("GET /api/runs/{id}/events", s.streamRun)
	mux.HandleFunc("GET /api/runs/{id}/nodes/{node}/output", s.nodeOutput)
	mux.HandleFunc("GET /api/runs/{id}/uploads/{name}", s.getUpload)

	// The hub's own surfaces: MCP for loom's agents, A2A for outside clients.
	if s.hub != nil {
		mux.Handle("/mcp", s.hub)
		mux.Handle("/mcp/", s.hub)
		mux.Handle("/a2a/", s.hub)
		mux.Handle("/a2a/agents", s.hub)
	}

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServerFS(sub))
	return mux
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readBody[T any](w http.ResponseWriter, r *http.Request) (*T, bool) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeErr(w, 400, fmt.Errorf("bad request body: %w", err))
		return nil, false
	}
	return &v, true
}

func statusFor(err error) int {
	if errors.Is(err, os.ErrNotExist) {
		return 404
	}
	return 500
}

// ---- meta ----

func (s *Server) getMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"models":          model.ModelCatalog,
		"default_model":   model.DefaultModel,
		"runtimes":        model.RuntimeCatalog,
		"default_runtime": model.DefaultRuntime,
		"default_dry_run": s.defaultDryRun,
		"max_lessons":     engine.MaxLessons,
		"lessons_nudge":   engine.LessonsConsolidateNudge,
	})
}

// ---- agents ----

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.store.ListAgents()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if agents == nil {
		agents = []*model.Agent{}
	}
	writeJSON(w, 200, agents)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.LoadAgent(r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) saveAgent(w http.ResponseWriter, r *http.Request) {
	a, ok := readBody[model.Agent](w, r)
	if !ok {
		return
	}
	// Pool agents are workers: the top tier stays reserved for the main agent.
	if model.CoordinatorOnlyModel(a.Model) {
		writeErr(w, 400, fmt.Errorf("model %q is reserved for the main agent (planner/coordinator); pool agents cap at opus", a.Model))
		return
	}
	if err := s.store.SaveAgent(a); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAgent(r.PathValue("name")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- per-agent skills ----

func (s *Server) getSkill(w http.ResponseWriter, r *http.Request) {
	content, err := s.store.ReadSkill(r.PathValue("name"), r.PathValue("skill"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, map[string]string{"name": r.PathValue("skill"), "content": content})
}

func (s *Server) saveSkill(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[struct {
		Content string `json:"content"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.store.WriteSkill(r.PathValue("name"), r.PathValue("skill"), body.Content); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSkill(r.PathValue("name"), r.PathValue("skill")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- workflows ----

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request) {
	wfs, err := s.store.ListWorkflows()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if wfs == nil {
		wfs = []*model.Workflow{}
	}
	writeJSON(w, 200, wfs)
}

func (s *Server) getWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, err := s.store.LoadWorkflow(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, wf)
}

// promptPreview assembles the EFFECTIVE main-agent system prompt for a draft
// workflow — exactly what the coordinator (dynamic) or planner (static) will
// receive at run time, minus run-specific values (goal, resolved output dir).
// The draft is not persisted: the settings page posts the current form state
// so the user sees the real configuration, not a description of it.
func (s *Server) promptPreview(w http.ResponseWriter, r *http.Request) {
	wf, ok := readBody[model.Workflow](w, r)
	if !ok {
		return
	}
	all, err := s.store.ListAgents()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	pool := all
	if len(wf.AgentPool) > 0 {
		byName := map[string]*model.Agent{}
		for _, a := range all {
			byName[a.Name] = a
		}
		pool = nil
		for _, name := range wf.AgentPool {
			if a, ok := byName[name]; ok {
				pool = append(pool, a)
			}
		}
	}
	var prompt string
	if wf.EffectiveMode() == model.ModeDynamic {
		prompt = hub.CoordinatorPrompt(&model.Run{}, wf, wf.EffectiveBudget(), s.engine.OutputRoot(), "", pool, s.engine.LessonsFor(wf.ID))
	} else {
		prompt = planner.BuildPrompt("<用户发起运行时输入的目标>", pool, wf.Planner, "", wf.AllowAgentCreation, engine.LessonTexts(s.engine.LessonsFor(wf.ID)))
	}
	writeJSON(w, 200, map[string]string{"mode": wf.EffectiveMode(), "prompt": prompt})
}

func (s *Server) saveWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := readBody[model.Workflow](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(wf.Name) == "" {
		writeErr(w, 400, fmt.Errorf("workflow name is required"))
		return
	}
	if err := s.store.SaveWorkflow(wf); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, wf)
}

func (s *Server) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteWorkflow(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- costs ----

// costSummary rolls up the append-only cost ledger. `by` narrows the response
// to one grouping; without it the caller gets all three in one round trip,
// which is what the UI wants.
func (s *Server) costSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.store.CostSummaryData()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	switch r.URL.Query().Get("by") {
	case "workflow":
		writeJSON(w, 200, map[string]any{"workflows": sum.Workflows, "total_usd": sum.TotalUSD})
	case "agent":
		writeJSON(w, 200, map[string]any{"agents": sum.Agents, "total_usd": sum.TotalUSD})
	case "model":
		writeJSON(w, 200, map[string]any{"models": sum.Models, "total_usd": sum.TotalUSD})
	default:
		writeJSON(w, 200, sum)
	}
}

// ---- runs ----

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	wf, err := s.store.LoadWorkflow(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	body, ok := readBody[struct {
		Goal   string `json:"goal"`
		DryRun bool   `json:"dry_run"`
		// Backend is the pre-v2.1 spelling; "mock" meant what dry_run means
		// now. Accepted so existing scripts keep working.
		Backend string `json:"backend"`
	}](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(body.Goal) == "" {
		writeErr(w, 400, fmt.Errorf("goal is required"))
		return
	}
	dryRun := body.DryRun || body.Backend == "mock"
	run, err := s.engine.StartRun(wf, body.Goal, dryRun)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, run)
}

// chatImage is one image attached to a chat message: base64 payload plus its
// declared MIME type. Decoding and limits live in decodeImages.
type chatImage struct {
	Mime string `json:"mime"`
	Data string `json:"data"` // base64 (no data: URL prefix)
}

const (
	maxChatImages    = 10
	maxChatImageSize = 8 << 20 // 8 MiB decoded, per image
)

// decodeImages validates and decodes chat image attachments.
func decodeImages(in []chatImage) ([]llm.Image, error) {
	if len(in) > maxChatImages {
		return nil, fmt.Errorf("at most %d images per message", maxChatImages)
	}
	out := make([]llm.Image, 0, len(in))
	for i, img := range in {
		switch img.Mime {
		case "image/png", "image/jpeg", "image/webp", "image/gif":
		default:
			return nil, fmt.Errorf("image %d: unsupported type %q (accepted: png, jpeg, webp, gif)", i+1, img.Mime)
		}
		data, err := base64.StdEncoding.DecodeString(img.Data)
		if err != nil {
			return nil, fmt.Errorf("image %d: invalid base64: %v", i+1, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("image %d is empty", i+1)
		}
		if len(data) > maxChatImageSize {
			return nil, fmt.Errorf("image %d exceeds %d MB", i+1, maxChatImageSize>>20)
		}
		out = append(out, llm.Image{MimeType: img.Mime, Data: data})
	}
	return out, nil
}

// chatWorkflow is the conversational entry point: a message to a workflow's
// main agent. A session IS a run: messages continue the addressed session —
// reopening it if it had finished — and only new_session (or a workflow with
// no session yet) starts a fresh one. Static workflows have no conversational
// coordinator, so each message simply starts a run.
func (s *Server) chatWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, err := s.store.LoadWorkflow(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	body, ok := readBody[struct {
		Text       string      `json:"text"`
		Images     []chatImage `json:"images"`
		DryRun     bool        `json:"dry_run"`
		RunID      string      `json:"run_id"`      // the session to continue; empty = active or latest
		NewSession bool        `json:"new_session"` // force a fresh session
	}](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(body.Text) == "" && len(body.Images) == 0 {
		writeErr(w, 400, fmt.Errorf("text is required"))
		return
	}
	images, err := decodeImages(body.Images)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if wf.EffectiveMode() != model.ModeDynamic || body.NewSession {
		run, err := s.engine.StartRun(wf, body.Text, body.DryRun, images...)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, run)
		return
	}

	target := body.RunID
	if target == "" {
		target = s.engine.ActiveDynamicRun(wf.ID)
	}
	if target == "" {
		// No addressed session and none active: continue the latest one, so a
		// conversation survives the coordinator finishing (or dying) between
		// messages. Only a workflow with no history starts fresh implicitly.
		if runs, err := s.store.ListRuns(); err == nil {
			for _, run := range runs { // newest first
				if run.WorkflowID == wf.ID && run.EffectiveMode() == model.ModeDynamic {
					target = run.ID
					break
				}
			}
		}
	}
	if target == "" {
		run, err := s.engine.StartRun(wf, body.Text, body.DryRun, images...)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, run)
		return
	}

	// Continue the session: live runs get the message directly; finished ones
	// are reopened by it.
	if err := s.engine.ChatToRun(target, body.Text, images...); err != nil {
		if _, rerr := s.engine.ReopenRun(target, body.Text, images...); rerr != nil {
			writeErr(w, 400, fmt.Errorf("cannot continue session %s: %v", target, rerr))
			return
		}
	}
	run, err := s.store.LoadRun(target)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, run)
}

// chatRun continues a specific active run's conversation.
func (s *Server) chatRun(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[struct {
		Text   string      `json:"text"`
		Images []chatImage `json:"images"`
	}](w, r)
	if !ok {
		return
	}
	images, err := decodeImages(body.Images)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.engine.ChatToRun(r.PathValue("id"), body.Text, images...); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// getUpload serves one stored chat attachment (the UI renders chat images
// from here). The store's name validation refuses path traversal.
func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.ReadUpload(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	w.Write(data)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if wfID := r.URL.Query().Get("workflow_id"); wfID != "" {
		filtered := runs[:0]
		for _, run := range runs {
			if run.WorkflowID == wfID {
				filtered = append(filtered, run)
			}
		}
		runs = filtered
	}
	// List view: drop events to keep the payload small.
	type slim struct {
		*model.Run
		Events []model.Event `json:"events,omitempty"`
	}
	out := make([]slim, len(runs))
	for i, run := range runs {
		out[i] = slim{Run: run}
	}
	writeJSON(w, 200, out)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.LoadRun(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, run)
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.engine.IsActive(id) {
		writeErr(w, 400, fmt.Errorf("run %s is active; cancel it before deleting", id))
		return
	}
	if err := s.store.DeleteRun(id); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) approveRun(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Approve(r.PathValue("id")); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) rejectRun(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[struct {
		Reason string `json:"reason"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.engine.Reject(r.PathValue("id"), body.Reason); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// setRunFeedback lands the user's post-run verdict. For a real dynamic run
// this is CONVERSATIONAL: the session reopens in postmortem mode and the
// coordinator digests the feedback — clarifies, persists facts/amendments,
// writes the retrospective RECORD via conclude_feedback (kept on the run,
// never injected), and proposes the behavior rules it yields, which await
// the user's confirmation in the lessons queue. Static and dry runs have no
// conversational agent, so they keep the verbatim record. Active runs refuse
// it — the live chat is the channel then. An empty text clears the stored
// record without waking anyone, and direct=true edits the stored record
// verbatim (the manager UI's path: the human revising the record is the
// record's owner — no coordinator needs to interpret that).
func (s *Server) setRunFeedback(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[struct {
		Text   string `json:"text"`
		Direct bool   `json:"direct"`
	}](w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if s.engine.IsActive(id) {
		writeErr(w, 400, fmt.Errorf("run %s is active — tell the main agent directly in the session chat", id))
		return
	}
	run, err := s.store.LoadRun(id)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	text := strings.TrimSpace(body.Text)

	if text != "" && !body.Direct && run.EffectiveMode() == model.ModeDynamic && !run.EffectiveDryRun() {
		reopened, err := s.engine.ReopenFeedback(id, text)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, reopened)
		return
	}

	run.Feedback = text
	run.FeedbackAt = time.Now()
	msg := "user feedback recorded"
	if run.Feedback == "" {
		run.FeedbackAt = time.Time{}
		msg = "user feedback cleared"
	}
	run.Events = append(run.Events, model.Event{Ts: time.Now(), Type: "info", Msg: msg})
	if err := s.store.SaveRun(run); err != nil {
		writeErr(w, 500, err)
		return
	}
	if data, err := json.Marshal(run); err == nil {
		s.broker.Publish(run.ID, data)
	}
	writeJSON(w, 200, run)
}

// ---- amendments ----

func (s *Server) listAmendments(w http.ResponseWriter, r *http.Request) {
	ams, err := s.store.ListAmendments()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if ams == nil {
		ams = []*model.Amendment{}
	}
	writeJSON(w, 200, ams)
}

func (s *Server) approveAmendment(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.DecideAmendment(r.PathValue("id"), true)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) rejectAmendment(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.DecideAmendment(r.PathValue("id"), false)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, a)
}

// ---- lessons ----
// Behavior rules distilled from run retrospectives. The coordinator proposes
// (pending), the user decides; only approved rules are injected into future
// runs. A user-authored rule is born approved — writing it IS the approval.

func (s *Server) listLessons(w http.ResponseWriter, r *http.Request) {
	ls, err := s.store.ListLessons()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if wfID := r.URL.Query().Get("workflow_id"); wfID != "" {
		var filtered []*model.Lesson
		for _, l := range ls {
			if l.WorkflowID == wfID {
				filtered = append(filtered, l)
			}
		}
		ls = filtered
	}
	if ls == nil {
		ls = []*model.Lesson{}
	}
	writeJSON(w, 200, ls)
}

func (s *Server) addLesson(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[struct {
		Text string `json:"text"`
	}](w, r)
	if !ok {
		return
	}
	wfID := r.PathValue("id")
	if _, err := s.store.LoadWorkflow(wfID); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	l := &model.Lesson{WorkflowID: wfID, Text: strings.TrimSpace(body.Text), Status: model.AmendmentApproved, DecidedAt: time.Now()}
	if err := s.store.SaveLesson(l); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, l)
}

// consolidateLessons wakes the workflow's newest finished real dynamic
// session as a maintenance pass over the standing rules; everything it
// proposes still queues for the user's confirmation.
func (s *Server) consolidateLessons(w http.ResponseWriter, r *http.Request) {
	run, err := s.engine.ReopenConsolidate(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, run)
}

func (s *Server) approveLesson(w http.ResponseWriter, r *http.Request) {
	l, err := s.store.DecideLesson(r.PathValue("id"), true)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, l)
}

func (s *Server) rejectLesson(w http.ResponseWriter, r *http.Request) {
	l, err := s.store.DecideLesson(r.PathValue("id"), false)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, l)
}

func (s *Server) editLesson(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[struct {
		Text string `json:"text"`
	}](w, r)
	if !ok {
		return
	}
	l, err := s.store.UpdateLessonText(r.PathValue("id"), strings.TrimSpace(body.Text))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, l)
}

func (s *Server) deleteLesson(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteLesson(r.PathValue("id")); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// sendTaskMessage lets a human steer a live task, using the same ledger path
// the coordinator's send_message uses — so operator interventions are audited
// identically to agent ones.
func (s *Server) sendTaskMessage(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[struct {
		Text string `json:"text"`
	}](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, 400, fmt.Errorf("text is required"))
		return
	}
	if err := s.engine.SendToTask(r.PathValue("id"), r.PathValue("task"), body.Text); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Cancel(r.PathValue("id")); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) retryNode(w http.ResponseWriter, r *http.Request) {
	run, err := s.engine.RetryNode(r.PathValue("id"), r.PathValue("node"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, run)
}

func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.engine.ResumeRun(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, run)
}

func (s *Server) nodeOutput(w http.ResponseWriter, r *http.Request) {
	path := s.store.NodeOutputPath(r.PathValue("id"), r.PathValue("node"))
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write(data)
}

// streamRun is the SSE endpoint: a full run snapshot on connect, then one
// snapshot per engine event.
func (s *Server) streamRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	run, err := s.store.LoadRun(runID)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.broker.Subscribe(runID)
	defer s.broker.Unsubscribe(runID, ch)

	send := func(data []byte) {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	if snapshot, err := json.Marshal(run); err == nil {
		send(snapshot)
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			send(data)
		}
	}
}
