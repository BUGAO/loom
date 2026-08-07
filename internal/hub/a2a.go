package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"loom/internal/model"
)

// The A2A gateway. Every pool agent gets an Agent Card and a JSON-RPC endpoint
// on the hub — logically point-to-point, physically multiplexed through one
// process, which is what keeps the ledger free of blind spots.
//
// The RequestHandler here is implemented against the ledger rather than the
// SDK's built-in task store. That is deliberate: a task created by the
// coordinator over MCP and a task created by an outside client over A2A are
// the same row in the same table, so tasks/get and tasks/list see all of them.
// A second store would mean two truths (see docs/DECISIONS-v2.md D1).

const (
	a2aPrefix   = "/a2a/agents/"
	cardSuffix  = "/.well-known/agent-card.json"
	contextHint = "Set contextId to the id of an active loom dynamic run."
)

func (h *Hub) mountA2A(mux *http.ServeMux) {
	// One pattern serves every agent; the name is a path variable, so agents
	// created at runtime are reachable without re-registering routes.
	mux.HandleFunc("GET "+a2aPrefix+"{name}"+cardSuffix, h.serveAgentCard)
	mux.Handle(a2aPrefix+"{name}", h.a2aRPC())
	mux.HandleFunc("GET /a2a/agents", h.listAgentCards)
}

// agentCardFor renders a pool agent as an A2A Agent Card: capabilities from
// what loom can actually do for it, skills from its private skill set.
func (h *Hub) agentCardFor(a *model.Agent) *a2a.AgentCard {
	url := fmt.Sprintf("%s%s%s", h.baseURL, a2aPrefix, a.Name)
	card := &a2a.AgentCard{
		Name:               a.Name,
		Description:        a.Description,
		ProtocolVersion:    "0.3.0",
		Version:            "2",
		URL:                url,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		Capabilities: a2a.AgentCapabilities{
			Streaming:              true,
			StateTransitionHistory: true,
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}
	for _, sk := range a.Skills {
		card.Skills = append(card.Skills, a2a.AgentSkill{
			ID: sk, Name: sk,
			Description: fmt.Sprintf("private skill %q of agent %s", sk, a.Name),
			Tags:        []string{"skill"},
		})
	}
	if len(card.Skills) == 0 {
		// A card with no skills tells a client nothing; describe the agent's
		// tool budget instead, which is the honest capability statement.
		tools := a.Tools
		if tools == "" {
			tools = "none (pure reasoning)"
		}
		card.Skills = []a2a.AgentSkill{{
			ID: "execute", Name: "execute task",
			Description: fmt.Sprintf("%s — tools: %s", a.Description, tools),
			Tags:        []string{"task"},
		}}
	}
	return card
}

func (h *Hub) lookupAgent(name string) *model.Agent {
	agents, err := h.agents()
	if err != nil {
		return nil
	}
	for _, a := range agents {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func (h *Hub) serveAgentCard(w http.ResponseWriter, r *http.Request) {
	a := h.lookupAgent(r.PathValue("name"))
	if a == nil {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.agentCardFor(a))
}

func (h *Hub) listAgentCards(w http.ResponseWriter, r *http.Request) {
	agents, err := h.agents()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cards := make([]*a2a.AgentCard, 0, len(agents))
	for _, a := range agents {
		cards = append(cards, h.agentCardFor(a))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"agents": cards, "active_runs": h.ActiveRuns()})
}

// a2aRPC serves the JSON-RPC methods for whichever agent the path names.
func (h *Hub) a2aRPC() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if h.lookupAgent(name) == nil {
			http.Error(w, "unknown agent", http.StatusNotFound)
			return
		}
		a2asrv.NewJSONRPCHandler(&ledgerHandler{hub: h, agent: name}).ServeHTTP(w, r)
	})
}

// ledgerHandler implements a2asrv.RequestHandler directly over the task
// ledger.
type ledgerHandler struct {
	hub   *Hub
	agent string
}

var _ a2asrv.RequestHandler = (*ledgerHandler)(nil)

// findSession locates the active run session holding a task. Task state is
// never returned from here: callers take a locked snapshot, because worker
// goroutines mutate live tasks under rs.mu while A2A clients poll.
func (lh *ledgerHandler) findSession(id a2a.TaskID) *RunSession {
	lh.hub.mu.Lock()
	runs := make([]*RunSession, 0, len(lh.hub.runs))
	for _, rs := range lh.hub.runs {
		runs = append(runs, rs)
	}
	lh.hub.mu.Unlock()
	for _, rs := range runs {
		if _, ok := rs.TaskSnapshot(string(id)); ok {
			return rs
		}
	}
	return nil
}

// locate resolves a task as a safe-to-marshal copy: a locked snapshot from an
// active run, or the (no longer mutated) record of a finished one.
func (lh *ledgerHandler) locate(id a2a.TaskID) (runID string, t model.Task, ok bool) {
	if rs := lh.findSession(id); rs != nil {
		if snap, ok := rs.TaskSnapshot(string(id)); ok {
			return rs.run.ID, snap, true
		}
	}
	if run, task := lh.hub.retiredTask(string(id)); task != nil {
		return run.ID, *task, true
	}
	return "", model.Task{}, false
}

// toA2A projects a ledger task onto the A2A wire type. The lifecycle states
// are shared, so this is a projection, not a translation.
func toA2A(runID string, t *model.Task) *a2a.Task {
	out := &a2a.Task{
		ID:        a2a.TaskID(t.ID),
		ContextID: runID,
		Status: a2a.TaskStatus{
			State:     a2a.TaskState(t.Status),
			Timestamp: timePtr(t.EndedAt, t.CreatedAt),
		},
		Metadata: map[string]any{
			"agent": t.Agent, "title": t.Title, "depth": t.Depth,
			"created_by": t.CreatedBy, "cost_usd": t.CostUSD,
		},
	}
	for _, m := range t.Messages {
		role := a2a.MessageRoleAgent
		if m.Role == model.MsgInstruction || m.Role == model.MsgFollowup {
			role = a2a.MessageRoleUser
		}
		out.History = append(out.History, &a2a.Message{
			ID: fmt.Sprintf("%s-%d", t.ID, m.Ts.UnixNano()), TaskID: a2a.TaskID(t.ID),
			ContextID: runID, Role: role, Parts: a2a.ContentParts{&a2a.TextPart{Text: m.Text}},
			Metadata: map[string]any{"role": m.Role, "from": m.From},
		})
	}
	if t.Summary != "" {
		out.Artifacts = append(out.Artifacts, &a2a.Artifact{
			ID: a2a.ArtifactID(t.ID + "-result"), Name: "result",
			Description: "task result envelope",
			Parts:       a2a.ContentParts{&a2a.TextPart{Text: t.Summary}},
			Metadata:    map[string]any{"files": t.Artifacts},
		})
	}
	if t.Error != "" {
		out.Status.Message = &a2a.Message{
			ID: t.ID + "-error", TaskID: a2a.TaskID(t.ID), ContextID: runID,
			Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{&a2a.TextPart{Text: t.Error}},
		}
	}
	return out
}

func timePtr(primary, fallback time.Time) *time.Time {
	t := primary
	if t.IsZero() {
		t = fallback
	}
	return &t
}

func (lh *ledgerHandler) OnGetTask(_ context.Context, q *a2a.TaskQueryParams) (*a2a.Task, error) {
	runID, t, ok := lh.locate(q.ID)
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	return toA2A(runID, &t), nil
}

func (lh *ledgerHandler) OnListTasks(_ context.Context, _ *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	lh.hub.mu.Lock()
	runs := make([]*RunSession, 0, len(lh.hub.runs))
	for _, rs := range lh.hub.runs {
		runs = append(runs, rs)
	}
	lh.hub.mu.Unlock()

	resp := &a2a.ListTasksResponse{}
	for _, rs := range runs {
		rs.mu.Lock()
		for _, id := range rs.run.TaskOrder {
			if t := rs.run.Tasks[id]; t != nil && t.Agent == lh.agent {
				resp.Tasks = append(resp.Tasks, toA2A(rs.run.ID, t))
			}
		}
		rs.mu.Unlock()
	}
	// Finished runs are listed too: a client polling for its submitted work
	// should not lose sight of it the instant the run ends.
	for _, run := range lh.hub.retiredRuns() {
		for _, id := range run.TaskOrder {
			if t := run.Tasks[id]; t != nil && t.Agent == lh.agent {
				resp.Tasks = append(resp.Tasks, toA2A(run.ID, t))
			}
		}
	}
	return resp, nil
}

func (lh *ledgerHandler) OnCancelTask(_ context.Context, p *a2a.TaskIDParams) (*a2a.Task, error) {
	rs := lh.findSession(p.ID)
	if rs == nil {
		// A task in a finished run is still readable, but nothing can act on it.
		if _, _, ok := lh.locate(p.ID); ok {
			return nil, a2a.ErrTaskNotCancelable
		}
		return nil, a2a.ErrTaskNotFound
	}
	t, ok := rs.TaskSnapshot(string(p.ID))
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	if model.TaskTerminal(t.Status) {
		return nil, a2a.ErrTaskNotCancelable
	}
	if err := rs.CancelTask(string(p.ID), "canceled over A2A"); err != nil {
		return nil, clientErr(a2a.ErrInvalidRequest, "%v", err)
	}
	t, ok = rs.TaskSnapshot(string(p.ID))
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	return toA2A(rs.run.ID, &t), nil
}

// textOf flattens a message's text parts. Decoded parts arrive as values while
// locally constructed ones are pointers, so both shapes are accepted.
func textOf(m *a2a.Message) string {
	var b strings.Builder
	write := func(s string) {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	for _, p := range m.Parts {
		switch tp := p.(type) {
		case a2a.TextPart:
			write(tp.Text)
		case *a2a.TextPart:
			write(tp.Text)
		}
	}
	return b.String()
}

// clientErr returns an error whose message survives the JSON-RPC boundary.
// A bare error is flattened to "internal error", which tells an external
// caller nothing about how to fix its request.
func clientErr(kind error, format string, args ...any) error {
	return &a2a.Error{Err: kind, Message: fmt.Sprintf(format, args...)}
}

// OnSendMessage either continues an existing task or creates a new one on the
// run named by contextId. loom has no notion of an ambient conversation, so a
// message that names neither is refused with instructions rather than guessed at.
func (lh *ledgerHandler) OnSendMessage(_ context.Context, p *a2a.MessageSendParams) (a2a.SendMessageResult, error) {
	msg := p.Message
	text := textOf(msg)
	if strings.TrimSpace(text) == "" {
		return nil, clientErr(a2a.ErrInvalidParams, "message has no text content")
	}
	if msg.TaskID != "" {
		rs := lh.findSession(msg.TaskID)
		if rs == nil {
			return nil, a2a.ErrTaskNotFound
		}
		if _, err := rs.Send(string(msg.TaskID), "external", text); err != nil {
			return nil, clientErr(a2a.ErrInvalidRequest, "%v", err)
		}
		t, ok := rs.TaskSnapshot(string(msg.TaskID))
		if !ok {
			return nil, a2a.ErrTaskNotFound
		}
		return toA2A(rs.run.ID, &t), nil
	}

	rs := lh.hub.Run(msg.ContextID)
	if rs == nil {
		return nil, clientErr(a2a.ErrInvalidParams,
			"no active dynamic run with contextId %q. %s Active runs: %s",
			msg.ContextID, contextHint, strings.Join(lh.hub.ActiveRuns(), ", "))
	}
	t, err := rs.Delegate(DelegateRequest{
		Agent: lh.agent, Instruction: text, CreatedBy: CreatedByExternal, Depth: 1,
	})
	if err != nil {
		// Budget and validation refusals are the caller's problem to act on,
		// so they must arrive as readable text, not as "internal error".
		return nil, clientErr(a2a.ErrInvalidRequest, "%v", err)
	}
	snap, ok := rs.TaskSnapshot(t.ID)
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	return toA2A(rs.run.ID, &snap), nil
}

// OnSendMessageStream drives the same path and then streams the task's status
// transitions until it settles.
func (lh *ledgerHandler) OnSendMessageStream(ctx context.Context, p *a2a.MessageSendParams) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		res, err := lh.OnSendMessage(ctx, p)
		if err != nil {
			yield(nil, err)
			return
		}
		task, ok := res.(*a2a.Task)
		if !ok {
			yield(res, nil)
			return
		}
		if !yield(task, nil) {
			return
		}
		lh.streamTask(ctx, a2a.TaskID(task.ID), yield)
	}
}

func (lh *ledgerHandler) OnResubscribeToTask(ctx context.Context, p *a2a.TaskIDParams) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		rs := lh.findSession(p.ID)
		if rs == nil {
			yield(nil, a2a.ErrTaskNotFound)
			return
		}
		t, ok := rs.TaskSnapshot(string(p.ID))
		if !ok {
			yield(nil, a2a.ErrTaskNotFound)
			return
		}
		if !yield(toA2A(rs.run.ID, &t), nil) {
			return
		}
		lh.streamTask(ctx, p.ID, yield)
	}
}

// streamTask emits a status-update event on every transition until terminal.
func (lh *ledgerHandler) streamTask(ctx context.Context, id a2a.TaskID, yield func(a2a.Event, error) bool) {
	rs := lh.findSession(id)
	if rs == nil {
		return
	}
	last := ""
	for {
		ch := rs.waitChange()
		t, ok := rs.TaskSnapshot(string(id))
		if !ok {
			return
		}
		if t.Status != last {
			last = t.Status
			final := model.TaskTerminal(t.Status)
			ev := &a2a.TaskStatusUpdateEvent{
				TaskID: id, ContextID: rs.run.ID, Final: final,
				Status: a2a.TaskStatus{State: a2a.TaskState(t.Status), Timestamp: timePtr(t.EndedAt, t.CreatedAt)},
			}
			if !yield(ev, nil) || final {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-rs.finished:
			return
		case <-ch:
		}
	}
}

// Push notifications are not offered: loom's own UI streams over SSE, and a
// callback surface would be a second, unaudited egress path.
func (lh *ledgerHandler) OnGetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigParams) (*a2a.TaskPushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

func (lh *ledgerHandler) OnListTaskPushConfig(context.Context, *a2a.ListTaskPushConfigParams) ([]*a2a.TaskPushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

func (lh *ledgerHandler) OnSetTaskPushConfig(context.Context, *a2a.TaskPushConfig) (*a2a.TaskPushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

func (lh *ledgerHandler) OnDeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigParams) error {
	return a2a.ErrPushNotificationNotSupported
}

func (lh *ledgerHandler) OnGetExtendedAgentCard(context.Context) (*a2a.AgentCard, error) {
	a := lh.hub.lookupAgent(lh.agent)
	if a == nil {
		return nil, clientErr(a2a.ErrTaskNotFound, "unknown agent %q", lh.agent)
	}
	return lh.hub.agentCardFor(a), nil
}
