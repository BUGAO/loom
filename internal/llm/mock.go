package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Mock is a zero-cost backend for demos and tests.
//
// In static mode the planner call returns a small diamond DAG over the offered
// pool and node calls sleep briefly then return a canned envelope.
//
// In dynamic mode the mock is a *real MCP client*: it connects to the same hub
// endpoint a Claude session would, and calls the same tools in the same order.
// Only the judgment is scripted — the transport, the credential check, the
// policy engine and the ledger are all genuinely exercised. A test back door
// straight into the ledger would have been far less code and would have
// verified almost nothing (see docs/DECISIONS-v2.md D7).
//
// Behavior is steered by markers in the goal, the same way SIMULATE_FAIL works
// for static nodes:
//
//	simulate-fail     one worker returns an error envelope
//	simulate-ask      a worker asks the coordinator a question first
//	simulate-handoff  a worker hands off a sub-task to a peer
//	simulate-budget   the coordinator delegates until the budget refuses it
type Mock struct {
	NodeDelay time.Duration // 0 = random 0.8–2.2s
}

func (m *Mock) Name() string { return "mock" }

var _ SessionBackend = (*Mock)(nil)

func (m *Mock) Complete(ctx context.Context, req Request) (*Result, error) {
	switch req.Kind {
	case KindPlan:
		return m.plan(req)
	default:
		return m.node(ctx, req)
	}
}

func (m *Mock) plan(req Request) (*Result, error) {
	pool := req.Pool
	if len(pool) == 0 {
		pool = []string{"worker"}
	}
	pick := func(i int) string { return pool[i%len(pool)] }
	fail := ""
	// Inject a failing node on demand — but not into a replan, so the replan
	// path can demonstrate recovery.
	if strings.Contains(req.Prompt, "simulate-fail") && !strings.Contains(req.Prompt, "REPLAN") {
		fail = " SIMULATE_FAIL"
	}
	nodes := []map[string]any{
		{"id": "n1", "agent": pick(0), "title": "分析任务", "instruction": "Analyze the goal and outline the work.", "depends_on": []string{}},
		{"id": "n2", "agent": pick(1), "title": "实现 A 部分", "instruction": "Implement part A." + fail, "depends_on": []string{"n1"}},
		{"id": "n3", "agent": pick(2), "title": "实现 B 部分", "instruction": "Implement part B.", "depends_on": []string{"n1"}},
		{"id": "n4", "agent": pick(3), "title": "汇总与验证", "instruction": "Integrate results and verify.", "depends_on": []string{"n2", "n3"}},
	}
	data, _ := json.Marshal(map[string]any{"nodes": nodes})
	return &Result{Text: "```json\n" + string(data) + "\n```", DurationMs: 50}, nil
}

func (m *Mock) delay() time.Duration {
	if m.NodeDelay != 0 {
		return m.NodeDelay
	}
	return time.Duration(800+rand.Intn(1400)) * time.Millisecond
}

func (m *Mock) node(ctx context.Context, req Request) (*Result, error) {
	delay := m.delay()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(delay):
	}
	if strings.Contains(req.Prompt, "SIMULATE_FAIL") {
		return nil, fmt.Errorf("mock: simulated node failure")
	}
	env, _ := json.Marshal(map[string]any{
		"status":  "ok",
		"summary": fmt.Sprintf("(mock) completed in %s", delay.Round(time.Millisecond)),
	})
	text := fmt.Sprintf("Mock execution transcript.\n\n```json\n%s\n```", env)
	return &Result{Text: text, DurationMs: delay.Milliseconds()}, nil
}

// ---- dynamic mode: a scripted MCP client ----

func (m *Mock) Open(ctx context.Context, req SessionRequest) (Session, error) {
	if len(req.MCPServers) == 0 {
		return &singleShotSession{be: m, req: req}, nil
	}
	spec := req.MCPServers[0]
	client := mcp.NewClient(&mcp.Implementation{Name: "loom-mock", Version: "2"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             spec.URL,
		HTTPClient:           &http.Client{Transport: headerTransport{headers: spec.Headers}},
		DisableStandaloneSSE: true, // stateless hub: no server-initiated messages
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("mock: connect to hub MCP endpoint: %w", err)
	}
	return &mockSession{mock: m, req: req, cs: cs}, nil
}

// headerTransport attaches the hub credential to every MCP request, which is
// exactly what the ACP adapter does for a real agent session.
type headerTransport struct{ headers map[string]string }

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

type mockSession struct {
	mock *Mock
	req  SessionRequest
	cs   *mcp.ClientSession
	turn int
}

func (s *mockSession) Cancel(context.Context) error { return nil }
func (s *mockSession) Close() error                 { return s.cs.Close() }

func (s *mockSession) Prompt(ctx context.Context, text string) (*Result, error) {
	s.turn++
	start := time.Now()
	var log strings.Builder
	call := func(name string, args map[string]any) (string, error) {
		if s.req.OnActivity != nil {
			s.req.OnActivity(name)
		}
		res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return "", err
		}
		out := contentText(res)
		fmt.Fprintf(&log, "[tool:%s] %s\n", name, clip(out, 160))
		if res.IsError {
			return out, fmt.Errorf("%s", out)
		}
		return out, nil
	}

	var reply string
	var err error
	if s.req.Kind == KindCoordinator {
		reply, err = s.coordinate(ctx, text, call, &log)
	} else {
		reply, err = s.work(ctx, text, call)
	}
	if err != nil {
		return &Result{Transcript: log.String(), DurationMs: time.Since(start).Milliseconds()}, err
	}
	return &Result{
		Text:       reply,
		Transcript: log.String() + "\n" + reply,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

type callFn func(name string, args map[string]any) (string, error)

// coordinate scripts a coordinator: inspect the pool, clear the approval gate
// if there is one, delegate, collect, answer any question, and finish.
func (s *mockSession) coordinate(ctx context.Context, goal string, call callFn, log *strings.Builder) (string, error) {
	marker := func(name string) bool { return strings.Contains(goal, name) }

	if _, err := call("list_agents", nil); err != nil {
		return "", err
	}
	agent := s.pickAgent(ctx)

	// The approval gate is asynchronous: propose ends the round; the human's
	// decision wakes the next one. Each round re-reads the gate state, exactly
	// as a real coordinator is instructed to.
	progress, _ := call("progress", nil)
	stateOf := func(s, name string) bool {
		return strings.Contains(s, `"approval_state":"`+name+`"`) || strings.Contains(s, `"approval_state": "`+name+`"`)
	}
	switch {
	case stateOf(progress, "pending"):
		if _, err := call("propose_plan", map[string]any{
			"summary": "(mock) split the goal into two parallel tasks",
			"tasks": []map[string]any{
				{"agent": agent, "title": "part A", "why": "first half"},
				{"agent": agent, "title": "part B", "why": "second half"},
			},
		}); err != nil {
			return "", err
		}
		return "(mock) plan submitted; ending the round to wait for the human decision", nil
	case stateOf(progress, "awaiting_decision"):
		return "(mock) still waiting for the human decision", nil
	case stateOf(progress, "rejected"):
		return s.finish(call, "failed", "(mock) the human rejected the plan")
	}

	// simulate-budget: keep delegating until the policy engine refuses, then
	// converge — the behavior a well-behaved coordinator must show.
	if marker("simulate-budget") {
		refused := ""
		for i := 0; i < 100; i++ {
			out, err := call("delegate", map[string]any{
				"agent": agent, "title": fmt.Sprintf("burst %d", i+1),
				"instruction": "MOCK_QUICK do a trivial unit of work",
				"constraints": "none",
				"acceptance":  []map[string]any{{"kind": "command", "command": "true"}},
			})
			if err != nil {
				refused = out
				break
			}
		}
		if refused == "" {
			return "", fmt.Errorf("mock: budget was never enforced")
		}
		fmt.Fprintf(log, "[converge] budget refusal received, stopping delegation\n")
		call("await", map[string]any{"mode": "all", "timeout_sec": 30})
		return s.finish(call, "succeeded", "(mock) converged after the task budget refused further delegation")
	}

	// Name the deliverable folder by topic before delegating, like a
	// disciplined coordinator; on a reopened session the name is frozen and
	// the refusal is expected.
	call("name_output", map[string]any{"name": "mock-run"})

	instrA := "MOCK_QUICK MOCK_WRITE mock-a.txt part A of the goal"
	if marker("simulate-fail") {
		instrA = "MOCK_FAIL part A of the goal"
	}
	if marker("simulate-reject") {
		// The worker will claim success without producing the artifact; the
		// engine's acceptance run is what catches the lie.
		instrA = "MOCK_QUICK MOCK_SKIP MOCK_WRITE mock-a.txt part A of the goal"
	}
	if marker("simulate-ask") {
		instrA = "MOCK_ASK MOCK_WRITE mock-a.txt part A of the goal"
	}
	instrB := "MOCK_QUICK MOCK_WRITE mock-b.txt part B of the goal"
	if marker("simulate-handoff") {
		instrB = "MOCK_HANDOFF MOCK_WRITE mock-b.txt part B of the goal"
	}

	// The delegation package carries the full contract: cross-domain
	// constraints and a machine-checked acceptance bar fixed up front.
	acceptanceFor := func(instr string) []map[string]any {
		if f := mockWriteTarget(instr); f != "" {
			return []map[string]any{{"kind": "artifact_exists", "path": f}}
		}
		return []map[string]any{{"kind": "command", "command": "true"}}
	}
	var ids []string
	for i, instr := range []string{instrA, instrB} {
		out, err := call("delegate", map[string]any{
			"agent": agent, "title": fmt.Sprintf("part %c", 'A'+i), "instruction": instr,
			"constraints": "(mock) write plain text; do not touch other tasks' artifacts",
			"acceptance":  acceptanceFor(instr),
		})
		if err != nil {
			return "", err
		}
		if id := extractTaskID(out); id != "" {
			ids = append(ids, id)
		}
	}

	// Collect, answering questions as they come, until everything settles.
	var settledOut string
	for i := 0; i < 20; i++ {
		out, err := call("await", map[string]any{"task_ids": ids, "mode": "all", "timeout_sec": 30})
		if err != nil {
			return "", err
		}
		asking := tasksNeedingInput(out)
		for _, id := range asking {
			if _, err := call("send_message", map[string]any{
				"task_id": id, "text": "(mock) proceed with the straightforward interpretation",
			}); err != nil {
				return "", err
			}
		}
		if len(asking) == 0 && allSettled(out) {
			settledOut = out
			break
		}
	}

	if marker("simulate-fail") {
		return s.finish(call, "failed", "(mock) one task reported an error and could not be recovered")
	}
	if marker("simulate-reject") {
		return s.finish(call, "failed", "(mock) a worker claimed success but its acceptance contract failed")
	}
	// Verify before declaring success: read one produced deliverable. The
	// engine refuses success with zero inspections, so a real coordinator has
	// to do exactly this too.
	if strings.Contains(settledOut+" ", "mock-a.txt") || settledOut == "" {
		if _, err := call("inspect", map[string]any{"path": "mock-a.txt"}); err != nil {
			return "", fmt.Errorf("mock: inspect of the deliverable failed: %w", err)
		}
	}
	call("record_note", map[string]any{"text": "(mock) both parts delivered and inspected"})
	return s.finish(call, "succeeded", "(mock) all delegated tasks completed and verified")
}

// mockWriteTarget extracts the artifact name a MOCK_WRITE instruction asks for.
func mockWriteTarget(instr string) string {
	if i := strings.Index(instr, "MOCK_WRITE "); i >= 0 {
		rest := instr[i+len("MOCK_WRITE "):]
		if j := strings.IndexAny(rest, " \n"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return ""
}

func (s *mockSession) finish(call callFn, status, summary string) (string, error) {
	if _, err := call("finish_run", map[string]any{"status": status, "summary": summary}); err != nil {
		return "", err
	}
	return "Coordination complete: " + summary, nil
}

// pickAgent reads the first agent name out of list_agents' structured output.
func (s *mockSession) pickAgent(ctx context.Context) string {
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}})
	if err != nil {
		return "researcher"
	}
	var out struct {
		Agents []struct {
			Name string `json:"name"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(contentText(res)), &out); err == nil && len(out.Agents) > 0 {
		return out.Agents[0].Name
	}
	return "researcher"
}

// work scripts a worker: react to the markers the coordinator embedded in the
// instruction, then return the same result envelope a real agent would.
func (s *mockSession) work(ctx context.Context, prompt string, call callFn) (string, error) {
	call("report_progress", map[string]any{"text": "(mock) started"})

	if strings.Contains(prompt, "MOCK_ASK") && s.turn == 1 {
		if _, err := call("ask_coordinator", map[string]any{
			"question": "(mock) the task is ambiguous — which interpretation should I use?",
		}); err != nil {
			return "", err
		}
	}
	if strings.Contains(prompt, "MOCK_HANDOFF") && s.turn == 1 {
		call("handoff", map[string]any{
			"agent": s.peerAgent(ctx), "title": "(mock) delegated sub-part",
			"instruction": "MOCK_QUICK handle the sub-part that is outside my remit",
			"constraints": "none",
			"acceptance":  []map[string]any{{"kind": "command", "command": "true"}},
		})
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(s.mock.delay()):
	}

	if strings.Contains(prompt, "MOCK_FAIL") {
		env, _ := json.Marshal(map[string]any{
			"status": "error", "failure_kind": "blocked", "summary": "(mock) simulated task failure",
		})
		return "Mock worker transcript.\n\n```json\n" + string(env) + "\n```", nil
	}

	// A MOCK_WRITE instruction is honored for real: the artifact lands in the
	// exchange directory, so the engine's acceptance checks (and the
	// coordinator's inspect) run against something that actually exists.
	// MOCK_SKIP simulates a lying worker: it claims the artifact without
	// writing it, and must be caught by the engine's acceptance run.
	var artifacts []string
	if f := mockWriteTarget(prompt); f != "" {
		if strings.Contains(prompt, "MOCK_SKIP") {
			artifacts = append(artifacts, f)
		} else if dir := exchangeDirOf(prompt); dir != "" {
			os.MkdirAll(dir, 0o755)
			if err := os.WriteFile(filepath.Join(dir, f), []byte("(mock) deliverable "+f+"\n"), 0o644); err == nil {
				artifacts = append(artifacts, f)
			}
		}
	}
	env, _ := json.Marshal(map[string]any{
		"status":    "ok",
		"summary":   "(mock) task completed",
		"artifacts": artifacts,
	})
	return "Mock worker transcript.\n\n```json\n" + string(env) + "\n```", nil
}

// exchangeDirOf extracts the run's exchange directory from a worker prompt.
var exchangeDirRe = regexp.MustCompile(`exchange directory is: (\S+)`)

func exchangeDirOf(prompt string) string {
	if m := exchangeDirRe.FindStringSubmatch(prompt); m != nil {
		return m[1]
	}
	return ""
}

// peerAgent picks a handoff target; workers cannot list the pool, so the mock
// reuses its own agent name, which is enough to exercise the lineage rules.
func (s *mockSession) peerAgent(context.Context) string {
	if s.req.SystemPrompt != "" {
		return "researcher"
	}
	return "researcher"
}

// ---- small parsers over tool output ----

func contentText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if b.Len() == 0 && res.StructuredContent != nil {
		if data, err := json.Marshal(res.StructuredContent); err == nil {
			return string(data)
		}
	}
	return b.String()
}

func extractTaskID(s string) string {
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(s), &out); err == nil && out.TaskID != "" {
		return out.TaskID
	}
	// Fall back to the human-readable form: "… as task task_xxx (…"
	if i := strings.Index(s, "task task_"); i >= 0 {
		rest := s[i+len("task "):]
		end := strings.IndexAny(rest, " \n(.,")
		if end < 0 {
			end = len(rest)
		}
		return rest[:end]
	}
	return ""
}

type awaitPayload struct {
	Tasks []struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	} `json:"tasks"`
	TimedOut bool `json:"timed_out"`
}

func parseAwait(s string) awaitPayload {
	var p awaitPayload
	json.Unmarshal([]byte(s), &p)
	return p
}

func tasksNeedingInput(s string) []string {
	var out []string
	for _, t := range parseAwait(s).Tasks {
		if t.Status == "input-required" {
			out = append(out, t.TaskID)
		}
	}
	return out
}

func allSettled(s string) bool {
	p := parseAwait(s)
	if len(p.Tasks) == 0 {
		return false
	}
	for _, t := range p.Tasks {
		switch t.Status {
		case "completed", "failed", "canceled":
		default:
			return false
		}
	}
	return true
}

// clip renders a tool result's first line, short enough for a transcript.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
