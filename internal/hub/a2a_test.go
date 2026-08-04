package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"loom/internal/model"
)

// The A2A gateway must present internally-created tasks and externally-created
// ones identically — that equivalence is the whole reason the ledger, rather
// than the SDK's own store, backs the handler.

func a2aSetup(t *testing.T) (*Hub, *RunSession, *httptest.Server, *fakeExec) {
	t.Helper()
	agents := []*model.Agent{
		{Name: "alpha", Description: "does alpha things", Model: "claude-haiku-4-5", Tools: "Read,Write", Skills: []string{"probe"}},
		{Name: "beta", Description: "does beta things", Model: "claude-haiku-4-5"},
	}
	h := New("http://test", func() ([]*model.Agent, error) { return agents, nil })
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	exec := newFakeExec()
	budget := openBudget()
	run := &model.Run{
		ID: "run_a2a", WorkflowID: "wf", Mode: model.ModeDynamic,
		Tasks: map[string]*model.Task{}, CreatedAt: time.Now(),
	}
	rs := h.OpenRun(context.Background(), RunConfig{
		Run:       run,
		Workflow:  &model.Workflow{Mode: model.ModeDynamic, Budget: &budget},
		Pool:      agents,
		Workspace: t.TempDir(),
		Exec:      exec,
		OnChange:  func(*model.Run) {},
		OnEvent:   func(string, string, string) {},
	})
	t.Cleanup(rs.Close)
	return h, rs, srv, exec
}

func rpc(t *testing.T, url, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	res, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s response: %v", method, err)
	}
	return out
}

func resultOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	if e, ok := resp["error"]; ok {
		t.Fatalf("rpc error: %v", e)
	}
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result object in %v", resp)
	}
	return r
}

func TestAgentCardServed(t *testing.T) {
	_, _, srv, _ := a2aSetup(t)
	res, err := http.Get(srv.URL + "/a2a/agents/alpha/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("agent card status %d", res.StatusCode)
	}
	var card map[string]any
	json.NewDecoder(res.Body).Decode(&card)
	if card["name"] != "alpha" {
		t.Fatalf("card name = %v", card["name"])
	}
	if card["url"] != "http://test/a2a/agents/alpha" {
		t.Fatalf("card url = %v", card["url"])
	}
	caps, _ := card["capabilities"].(map[string]any)
	if caps["streaming"] != true {
		t.Fatalf("card should advertise streaming: %v", caps)
	}
	skills, _ := card["skills"].([]any)
	if len(skills) == 0 {
		t.Fatal("card has no skills; a client learns nothing from it")
	}
	// The agent's private skill must surface as an A2A skill.
	first, _ := skills[0].(map[string]any)
	if first["id"] != "probe" {
		t.Fatalf("private skill not exposed on the card: %v", first)
	}
}

func TestUnknownAgentCardIs404(t *testing.T) {
	_, _, srv, _ := a2aSetup(t)
	res, err := http.Get(srv.URL + "/a2a/agents/nobody/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("want 404 for an unknown agent, got %d", res.StatusCode)
	}
}

// A task the coordinator created over MCP must be readable over A2A.
func TestInternalTaskVisibleOverA2A(t *testing.T) {
	_, rs, srv, _ := a2aSetup(t)
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)
	rs.CompleteTask(task.ID, "all done", []string{"out.md"}, nil)

	got := resultOf(t, rpc(t, srv.URL+"/a2a/agents/alpha", "tasks/get", map[string]any{"id": task.ID}))
	if got["id"] != task.ID {
		t.Fatalf("tasks/get returned %v", got["id"])
	}
	if got["contextId"] != "run_a2a" {
		t.Fatalf("contextId should be the run id, got %v", got["contextId"])
	}
	status, _ := got["status"].(map[string]any)
	if status["state"] != "completed" {
		t.Fatalf("state = %v", status["state"])
	}
	arts, _ := got["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("result envelope should appear as an artifact, got %v", arts)
	}
	hist, _ := got["history"].([]any)
	if len(hist) < 2 {
		t.Fatalf("history should carry the instruction and the result, got %d entries", len(hist))
	}
}

// An external client can create a task by naming the run as contextId; it then
// runs through exactly the same ledger and budget as an internal delegation.
func TestExternalMessageCreatesTask(t *testing.T) {
	_, rs, srv, exec := a2aSetup(t)
	resp := rpc(t, srv.URL+"/a2a/agents/beta", "message/send", map[string]any{
		"message": map[string]any{
			"messageId": "m1", "role": "user", "contextId": "run_a2a",
			"parts": []any{map[string]any{"kind": "text", "text": "please handle this"}},
		},
	})
	got := resultOf(t, resp)
	id, _ := got["id"].(string)
	if id == "" {
		t.Fatalf("no task id in %v", got)
	}
	v, ok := rs.View(id)
	if !ok {
		t.Fatal("externally created task is missing from the ledger")
	}
	if v.Agent != "beta" {
		t.Fatalf("task went to %q, want the agent whose endpoint was called", v.Agent)
	}
	if started := drain(exec.started); len(started) != 1 || started[0] != id {
		t.Fatalf("external task was not scheduled: %v", started)
	}
	full := rs.Run().Tasks[id]
	if full.CreatedBy != "external" {
		t.Fatalf("lineage should mark the external origin, got %q", full.CreatedBy)
	}
}

// Without a valid contextId there is no run to attach to. The error must say
// what to do rather than silently picking a run.
func TestExternalMessageWithoutRunIsRefused(t *testing.T) {
	_, _, srv, _ := a2aSetup(t)
	resp := rpc(t, srv.URL+"/a2a/agents/beta", "message/send", map[string]any{
		"message": map[string]any{
			"messageId": "m1", "role": "user", "contextId": "run_nope",
			"parts": []any{map[string]any{"kind": "text", "text": "hi"}},
		},
	})
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error, got %v", resp)
	}
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "contextId") {
		t.Fatalf("error should explain contextId, got %q", msg)
	}
}

// The budget applies to external callers too — there is no side door.
func TestExternalMessageObeysBudget(t *testing.T) {
	agents := []*model.Agent{{Name: "alpha", Description: "a"}}
	h := New("http://test", func() ([]*model.Agent, error) { return agents, nil })
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	budget := openBudget()
	budget.MaxTasks = 1
	run := &model.Run{ID: "run_lim", Mode: model.ModeDynamic, Tasks: map[string]*model.Task{}, CreatedAt: time.Now()}
	rs := h.OpenRun(context.Background(), RunConfig{
		Run: run, Workflow: &model.Workflow{Mode: model.ModeDynamic, Budget: &budget},
		Pool: agents, Workspace: t.TempDir(), Exec: newFakeExec(),
		OnChange: func(*model.Run) {}, OnEvent: func(string, string, string) {},
	})
	defer rs.Close()

	send := func() map[string]any {
		return rpc(t, srv.URL+"/a2a/agents/alpha", "message/send", map[string]any{
			"message": map[string]any{
				"messageId": "m", "role": "user", "contextId": "run_lim",
				"parts": []any{map[string]any{"kind": "text", "text": "work"}},
			},
		})
	}
	resultOf(t, send()) // first one fits the budget
	if _, ok := send()["error"]; !ok {
		t.Fatal("an external caller must not be able to exceed the task budget")
	}
}

func TestA2ACancel(t *testing.T) {
	_, rs, srv, _ := a2aSetup(t)
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)

	got := resultOf(t, rpc(t, srv.URL+"/a2a/agents/alpha", "tasks/cancel", map[string]any{"id": task.ID}))
	status, _ := got["status"].(map[string]any)
	if status["state"] != "canceled" {
		t.Fatalf("state after cancel = %v", status["state"])
	}
	v, _ := rs.View(task.ID)
	if v.Status != model.TaskCanceled {
		t.Fatalf("ledger not updated by an A2A cancel: %s", v.Status)
	}
}

func TestA2ACancelTerminalRefused(t *testing.T) {
	_, rs, srv, _ := a2aSetup(t)
	task := mustDelegate(t, rs, "alpha")
	rs.TaskStarted(task.ID)
	rs.CompleteTask(task.ID, "done", nil, nil)

	if _, ok := rpc(t, srv.URL+"/a2a/agents/alpha", "tasks/cancel", map[string]any{"id": task.ID})["error"]; !ok {
		t.Fatal("canceling a completed task should be an error")
	}
}

// tasks/list is scoped to the agent whose endpoint was called.
func TestA2AListScopedToAgent(t *testing.T) {
	_, rs, srv, _ := a2aSetup(t)
	mustDelegate(t, rs, "alpha")
	mustDelegate(t, rs, "beta")
	mustDelegate(t, rs, "beta")

	got := resultOf(t, rpc(t, srv.URL+"/a2a/agents/beta", "tasks/list", map[string]any{}))
	tasks, _ := got["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("want beta's 2 tasks, got %d", len(tasks))
	}
}

func TestPushConfigUnsupported(t *testing.T) {
	_, rs, srv, _ := a2aSetup(t)
	task := mustDelegate(t, rs, "alpha")
	resp := rpc(t, srv.URL+"/a2a/agents/alpha", "tasks/pushNotificationConfig/get",
		map[string]any{"id": task.ID})
	if _, ok := resp["error"]; !ok {
		t.Fatal("push notification config should be reported as unsupported")
	}
}
