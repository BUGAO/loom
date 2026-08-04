// Package server exposes the REST + SSE API and serves the embedded web UI.
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"loom/internal/engine"
	"loom/internal/model"
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
	mux.HandleFunc("GET /api/workflows/{id}", s.getWorkflow)
	mux.HandleFunc("DELETE /api/workflows/{id}", s.deleteWorkflow)
	mux.HandleFunc("POST /api/workflows/{id}/runs", s.startRun)

	mux.HandleFunc("GET /api/costs/summary", s.costSummary)

	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("POST /api/runs/{id}/approve", s.approveRun)
	mux.HandleFunc("POST /api/runs/{id}/reject", s.rejectRun)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancelRun)
	mux.HandleFunc("POST /api/runs/{id}/retry/{node}", s.retryNode)
	mux.HandleFunc("POST /api/runs/{id}/resume", s.resumeRun)
	mux.HandleFunc("POST /api/runs/{id}/tasks/{task}/message", s.sendTaskMessage)
	mux.HandleFunc("GET /api/runs/{id}/events", s.streamRun)
	mux.HandleFunc("GET /api/runs/{id}/nodes/{node}/output", s.nodeOutput)

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
