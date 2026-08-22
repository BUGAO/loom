package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"loom/internal/model"
)

// TokenHeader carries a session's one-time hub credential. It rides in an HTTP
// header rather than a config file under the agent's home, because homes are
// persistent and shared by concurrent tasks of the same agent.
const TokenHeader = "X-Loom-Token"

// ServerName is the MCP server name agents see; tool titles contain it, which
// is how the ACP permission policy recognizes orchestration calls.
const ServerName = "loom"

// Role of a hub credential.
const (
	RoleCoordinator = "coordinator"
	RoleWorker      = "worker"
	// RolePair is a resident implementer's credential: one session serving
	// many tasks in sequence, so the task it acts for is resolved at call
	// time from the ledger instead of being fixed at token issue.
	RolePair = "pair"
)

// identity is what a hub token resolves to.
type identity struct {
	runID  string
	role   string
	taskID string // worker only
	agent  string // pair only: which resident partner this session is
}

// Hub owns every dynamic run's control plane and the two protocol surfaces
// onto it: MCP for loom's own agents, A2A for anything outside the process.
type Hub struct {
	baseURL string
	agents  func() ([]*model.Agent, error)

	mu      sync.Mutex
	runs    map[string]*RunSession
	tokens  map[string]identity
	servers map[string]*mcp.Server // token → its bound MCP server

	// retired keeps finished runs readable over A2A. An external client that
	// submitted a task must still be able to fetch its result after the run
	// ends; dropping the run the moment it finishes would make every such
	// task look like it vanished. Bounded, newest-first.
	retired      []*model.Run
	retiredIndex map[string]*model.Run // task id → its run
}

// maxRetiredRuns bounds the finished-run cache. Older runs stay in the store
// and in loom's own UI; only the A2A view forgets them.
const maxRetiredRuns = 50

func New(baseURL string, agents func() ([]*model.Agent, error)) *Hub {
	return &Hub{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		agents:       agents,
		runs:         map[string]*RunSession{},
		tokens:       map[string]identity{},
		servers:      map[string]*mcp.Server{},
		retiredIndex: map[string]*model.Run{},
	}
}

// BaseURL is where agents reach this hub.
func (h *Hub) BaseURL() string { return h.baseURL }

// MCPEndpoint is the URL handed to agent sessions.
func (h *Hub) MCPEndpoint() string { return h.baseURL + "/mcp" }

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// OpenRun registers a dynamic run and returns its ledger session.
func (h *Hub) OpenRun(ctx context.Context, cfg RunConfig) *RunSession {
	budget := cfg.Workflow.EffectiveBudget()
	rs := &RunSession{
		hub:        h,
		cfg:        cfg,
		budget:     budget,
		workspace:  cfg.Workspace,
		run:        cfg.Run,
		pool:       map[string]*model.Agent{},
		ctrl:       map[string]*taskCtrl{},
		dispatched: map[string]bool{},
		// A resumed run whose plan was already approved must not re-gate.
		approvalNeeded: budget.ApprovalPolicy == model.ApprovalInitial && !cfg.Run.PlanApproved,
		changed:        make(chan struct{}),
		finished:       make(chan struct{}),
		lastTransition: time.Now(),
	}
	for _, a := range cfg.Pool {
		rs.pool[a.Name] = a
	}
	if rs.run.Tasks == nil {
		rs.run.Tasks = map[string]*model.Task{}
	}
	// Resume: tasks carried over from a previous process need control blocks.
	for id, t := range rs.run.Tasks {
		if !model.TaskTerminal(t.Status) {
			rs.ctrl[id] = rs.newTaskCtrl()
		}
	}
	h.mu.Lock()
	h.runs[cfg.Run.ID] = rs
	h.mu.Unlock()

	go rs.watchStall(ctx)
	go rs.watchClock(ctx)
	return rs
}

// removeRun retires a finished run: it stops accepting new work and its
// credentials are revoked, but its tasks stay readable over A2A.
func (h *Hub) removeRun(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rs := h.runs[runID]; rs != nil {
		h.retireLocked(rs.run)
	}
	delete(h.runs, runID)
	for tok, id := range h.tokens {
		if id.runID == runID {
			delete(h.tokens, tok)
			delete(h.servers, tok)
		}
	}
}

func (h *Hub) retireLocked(run *model.Run) {
	h.retired = append([]*model.Run{run}, h.retired...)
	for len(h.retired) > maxRetiredRuns {
		old := h.retired[len(h.retired)-1]
		h.retired = h.retired[:len(h.retired)-1]
		for id := range old.Tasks {
			delete(h.retiredIndex, id)
		}
	}
	for id := range run.Tasks {
		h.retiredIndex[id] = run
	}
}

// retiredTask looks up a task belonging to a finished run.
func (h *Hub) retiredTask(taskID string) (*model.Run, *model.Task) {
	h.mu.Lock()
	defer h.mu.Unlock()
	run := h.retiredIndex[taskID]
	if run == nil {
		return nil, nil
	}
	return run, run.Tasks[taskID]
}

// retiredRuns snapshots the finished-run cache.
func (h *Hub) retiredRuns() []*model.Run {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*model.Run(nil), h.retired...)
}

// Run looks up an active dynamic run.
func (h *Hub) Run(runID string) *RunSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs[runID]
}

// ActiveRuns lists the dynamic runs currently accepting work — what an
// external A2A client needs in order to address one.
func (h *Hub) ActiveRuns() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.runs))
	for id := range h.runs {
		out = append(out, id)
	}
	return out
}

// IssueCoordinatorToken mints the credential for a run's coordinator session.
func (h *Hub) IssueCoordinatorToken(runID string) string {
	return h.issue(identity{runID: runID, role: RoleCoordinator})
}

// IssueWorkerToken mints the credential for one task's worker session. It is
// scoped to that task: a worker cannot report progress for, answer for, or
// hand off from anyone else's task.
func (h *Hub) IssueWorkerToken(runID, taskID string) string {
	return h.issue(identity{runID: runID, role: RoleWorker, taskID: taskID})
}

// IssuePairToken mints the credential for one of a run's resident partner
// sessions (agent names it). It outlives any one task; the task it acts for
// at each call is whatever the engine has currently bound for that agent via
// RunSession.SetPairTask.
func (h *Hub) IssuePairToken(runID, agent string) string {
	return h.issue(identity{runID: runID, role: RolePair, agent: agent})
}

func (h *Hub) issue(id identity) string {
	tok := newToken()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokens[tok] = id
	return tok
}

// RevokeToken retires a credential once its session is over.
func (h *Hub) RevokeToken(tok string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.tokens, tok)
	delete(h.servers, tok)
}

// MCPServers renders the endpoint list for an agent session.
func (h *Hub) MCPServers(token string) []mcpServerSpec {
	return []mcpServerSpec{{Name: ServerName, URL: h.MCPEndpoint(), Token: token}}
}

// mcpServerSpec is a transport-agnostic description of the hub endpoint; the
// engine converts it into whatever its backend needs.
type mcpServerSpec struct {
	Name  string
	URL   string
	Token string
}

// resolve maps a request's credential onto a run session and role.
func (h *Hub) resolve(tok string) (*RunSession, identity, bool) {
	h.mu.Lock()
	id, ok := h.tokens[tok]
	var rs *RunSession
	if ok {
		rs = h.runs[id.runID]
	}
	h.mu.Unlock()
	if !ok || rs == nil {
		return nil, identity{}, false
	}
	return rs, id, true
}

// MCPHandler serves the hub toolset over streamable HTTP. Each credential gets
// its own server instance whose tools are bound to that identity, so authority
// is carried by the connection rather than by arguments an agent could forge.
func (h *Hub) MCPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		tok := r.Header.Get(TokenHeader)
		if tok == "" {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		rs, id, ok := h.resolve(tok)
		if !ok {
			return nil // the SDK answers 404 for an unknown session
		}
		h.mu.Lock()
		srv := h.servers[tok]
		h.mu.Unlock()
		if srv != nil {
			return srv
		}
		srv = h.buildServer(rs, id)
		h.mu.Lock()
		if existing := h.servers[tok]; existing != nil {
			srv = existing
		} else {
			h.servers[tok] = srv
		}
		h.mu.Unlock()
		return srv
	}, &mcp.StreamableHTTPOptions{
		// Agents connect from a spawned subprocess on this machine; the tools
		// are all request/response, so no server→client calls are needed.
		Stateless: true,
	})
}

// Handler mounts every hub surface under one mux.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", h.MCPHandler())
	mux.Handle("/mcp/", h.MCPHandler())
	mux.HandleFunc("POST /gate", h.serveGate)
	h.mountA2A(mux)
	return mux
}

// GateEndpoint is where a session's hook posts its tool calls.
func (h *Hub) GateEndpoint() string { return h.baseURL + "/gate" }

// serveGate is the hook gate's HTTP face: bearer token, hook JSON in, hook
// JSON out. An unknown token (the run is over) answers allow — the hook is
// fail-open by design, and an ended run has nothing left to protect.
func (h *Hub) serveGate(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.Header.Get(TokenHeader)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := h.Gate(tok, body)
	switch {
	case errors.Is(err, ErrUnknownCredential):
		out = map[string]any{}
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// toolErr renders an error as a tool result the model can act on, rather than
// a protocol failure it can only be confused by. Budget refusals in particular
// must read as instructions, not as breakage.
func toolErr(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}
