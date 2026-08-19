package hub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loom/internal/model"
)

// gateSession opens a run with a protect dir and a pilot allowlist, so every
// gate rule has something to bite on.
func gateSession(t *testing.T, pilotTools string) (*RunSession, string, string) {
	t.Helper()
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	ws := t.TempDir()
	data := t.TempDir()
	run := &model.Run{ID: "run_gate", WorkflowID: "wf", Mode: model.ModeDynamic,
		Tasks: map[string]*model.Task{}, CreatedAt: time.Now()}
	b := openBudget()
	rs := h.OpenRun(context.Background(), RunConfig{
		Run: run, Workflow: &model.Workflow{Mode: model.ModeDynamic, Budget: &b, PairAgent: "pairy"},
		Pool: []*model.Agent{
			{Name: "impl", Tools: "read,edit,write,bash"},
			{Name: "reader", Tools: "read,grep"},
			{Name: "pairy", Tools: "read,edit,bash"},
		},
		Workspace: ws, ProtectDir: data, PilotTools: pilotTools,
		Exec: newFakeExec(), OnChange: func(*model.Run) {},
	})
	t.Cleanup(rs.Close)
	declareTestEvidence(t, rs)
	return rs, ws, data
}

func preWrite(tool, path string) GateRequest {
	var r GateRequest
	r.Event, r.Tool = "PreToolUse", tool
	r.Input.FilePath = path
	return r
}

func preBash(cmd string) GateRequest {
	var r GateRequest
	r.Event, r.Tool = "PreToolUse", "Bash"
	r.Input.Command = cmd
	return r
}

func delegateScoped(t *testing.T, rs *RunSession, agent string, scope ...string) string {
	t.Helper()
	task, err := rs.Delegate(DelegateRequest{Agent: agent, Title: "t " + strings.Join(scope, ","), Instruction: "do",
		Constraints: "none", Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return task.ID
}

func TestGateAllowlistIsPerIdentity(t *testing.T) {
	rs, ws, _ := gateSession(t, "")
	impl := delegateScoped(t, rs, "impl")
	reader := delegateScoped(t, rs, "reader")
	p := filepath.Join(ws, "a.go")

	if d := rs.Gate(identity{role: RoleWorker, taskID: impl}, preWrite("Edit", p)); !d.Allow {
		t.Fatalf("impl has edit: %s", d.Reason)
	}
	d := rs.Gate(identity{role: RoleWorker, taskID: reader}, preWrite("Edit", p))
	if d.Allow || !strings.Contains(d.Reason, "not in agent reader's allowlist") {
		t.Fatalf("reader must not edit: %+v", d)
	}
	// Task is never granted, whatever the allowlist.
	var task GateRequest
	task.Event, task.Tool = "PreToolUse", "Task"
	if d := rs.Gate(identity{role: RoleWorker, taskID: impl}, task); d.Allow || !strings.Contains(d.Reason, "delegate") {
		t.Fatalf("Task must be refused with a pointer to delegate: %+v", d)
	}
	// The pre-pilot coordinator has no file tools at all.
	if d := rs.Gate(identity{role: RoleCoordinator}, preWrite("Write", p)); d.Allow || !strings.Contains(d.Reason, "the main agent") {
		t.Fatalf("coordinator without tools must be refused: %+v", d)
	}
	// Unmanaged tools (hub MCP tools, TodoWrite) are never gated.
	var mcp GateRequest
	mcp.Event, mcp.Tool = "PreToolUse", "mcp__loom__delegate"
	if d := rs.Gate(identity{role: RoleWorker, taskID: reader}, mcp); !d.Allow {
		t.Fatalf("unmanaged tool gated: %+v", d)
	}
}

func TestGateProtectsLoomState(t *testing.T) {
	rs, ws, data := gateSession(t, "read,edit,write,bash")
	impl := delegateScoped(t, rs, "impl")
	id := identity{role: RoleWorker, taskID: impl}
	for _, p := range []string{
		filepath.Join(data, "workflows", "wf.json"),
		filepath.Join(data, "agents", "impl", "agent.md"),
		filepath.Join(data, "agents", "impl", "home", ".claude", "skills", "x", "SKILL.md"),
		filepath.Join(data, "runs", "run_gate", "run.json"),
		filepath.Join(data, "agents", "impl", "home", "AGENTS.md"),
		filepath.Join(ws, ".claude", "settings.local.json"),
	} {
		if d := rs.Gate(id, preWrite("Write", p)); d.Allow || !strings.Contains(d.Reason, "loom's own state") {
			t.Errorf("%s should be protected: %+v", p, d)
		}
	}
	// Scratch in the agent home is fine.
	if d := rs.Gate(id, preWrite("Write", filepath.Join(data, "agents", "impl", "home", "notes.md"))); !d.Allow {
		t.Errorf("agent home scratch refused: %+v", d)
	}
}

func TestGateScopeOwnership(t *testing.T) {
	rs, ws, _ := gateSession(t, "read,edit,write,bash")
	a := delegateScoped(t, rs, "impl", "src/a", "README.md")
	b := delegateScoped(t, rs, "impl", "src/b/")
	free := delegateScoped(t, rs, "impl")
	wa := identity{role: RoleWorker, taskID: a}
	wb := identity{role: RoleWorker, taskID: b}
	wf := identity{role: RoleWorker, taskID: free}

	if d := rs.Gate(wa, preWrite("Edit", filepath.Join(ws, "src/a/x.go"))); !d.Allow {
		t.Fatalf("inside own scope: %+v", d)
	}
	if d := rs.Gate(wa, preWrite("Edit", filepath.Join(ws, "README.md"))); !d.Allow {
		t.Fatalf("exact file in scope: %+v", d)
	}
	// Relative paths resolve against the hook's cwd.
	rel := preWrite("Edit", "src/a/y.go")
	rel.Cwd = ws
	if d := rs.Gate(wa, rel); !d.Allow {
		t.Fatalf("relative path inside scope: %+v", d)
	}
	d := rs.Gate(wa, preWrite("Edit", filepath.Join(ws, "src/c/z.go")))
	if d.Allow || !strings.Contains(d.Reason, "outside your task's scope") || !strings.Contains(d.Reason, "src/a") {
		t.Fatalf("outside own scope: %+v", d)
	}
	d = rs.Gate(wa, preWrite("Edit", filepath.Join(ws, "src/b/q.go")))
	if d.Allow || !strings.Contains(d.Reason, "owned by task "+b) {
		t.Fatalf("another task's scope: %+v", d)
	}
	// The unscoped worker may write anywhere not owned by someone else.
	if d := rs.Gate(wf, preWrite("Edit", filepath.Join(ws, "src/c/z.go"))); !d.Allow {
		t.Fatalf("unscoped free path: %+v", d)
	}
	if d := rs.Gate(wf, preWrite("Edit", filepath.Join(ws, "src/a/x.go"))); d.Allow {
		t.Fatalf("unscoped worker must not enter an in-flight scope: %+v", d)
	}
	// The coordinator, at any level, must not enter an in-flight scope either…
	rs.SetLevel(model.LevelSolo, "test", "")
	if d := rs.Gate(identity{role: RoleCoordinator}, preWrite("Edit", filepath.Join(ws, "src/b/q.go"))); d.Allow {
		t.Fatalf("coordinator entered task %s's scope: %+v", b, d)
	}
	// …but once that task settles, the scope is released.
	rs.TaskStarted(b)
	rs.CompleteTask(b, "done", nil, nil)
	if d := rs.Gate(wa, preWrite("Edit", filepath.Join(ws, "src/b/q.go"))); d.Allow {
		t.Fatalf("a's own scope still binds a: %+v", d) // a is scoped to src/a — still outside
	}
	if d := rs.Gate(wf, preWrite("Edit", filepath.Join(ws, "src/b/q.go"))); !d.Allow {
		t.Fatalf("settled task's scope should be released: %+v", d)
	}
	_ = wb
	// Writes outside the workspace are not scope business.
	if d := rs.Gate(wa, preWrite("Write", filepath.Join(t.TempDir(), "scratch.txt"))); !d.Allow {
		t.Fatalf("outside workspace: %+v", d)
	}
}

func TestGateOrchestrateTiesThePilotsHands(t *testing.T) {
	rs, ws, _ := gateSession(t, "read,edit,write,bash")
	pilot := identity{role: RoleCoordinator}
	// Legacy default (no level) is orchestrate.
	if rs.Level() != model.LevelOrchestrate {
		t.Fatalf("default level = %s", rs.Level())
	}
	d := rs.Gate(pilot, preWrite("Write", filepath.Join(ws, "main.go")))
	if d.Allow || !strings.Contains(d.Reason, "ORCHESTRATE") || !strings.Contains(d.Reason, "delegate") {
		t.Fatalf("orchestrate must refuse and point at delegate: %+v", d)
	}
	for _, cmd := range []string{"go test ./...", "ls -la src", "git status", "git diff", "cat main.go | grep x", "npm test 2>&1 | tail", "go build ./... > /dev/null"} {
		if d := rs.Gate(pilot, preBash(cmd)); !d.Allow {
			t.Errorf("verification command refused: %q → %s", cmd, d.Reason)
		}
	}
	for _, cmd := range []string{"echo x > main.go", "cat <<EOF > a.txt\nhi\nEOF", "sed -i '' s/a/b/ main.go", "rm -rf dist", "git commit -am x", "npm install left-pad", "go test ./... && mv a b", "printf 'x' | tee out.txt"} {
		if d := rs.Gate(pilot, preBash(cmd)); d.Allow || !strings.Contains(d.Reason, "ORCHESTRATE") {
			t.Errorf("write-ish shell allowed at orchestrate: %q", cmd)
		}
	}
	// Drop to solo: hands free.
	if err := rs.SetLevel(model.LevelSolo, "user", "small task"); err != nil {
		t.Fatal(err)
	}
	if d := rs.Gate(pilot, preWrite("Write", filepath.Join(ws, "main.go"))); !d.Allow {
		t.Fatalf("solo write refused: %+v", d)
	}
	if d := rs.Gate(pilot, preBash("echo x > main.go")); !d.Allow {
		t.Fatalf("solo shell write refused: %+v", d)
	}
	run := rs.Run()
	if run.Level != model.LevelSolo || run.LevelSource != "user" || len(run.LevelLog) != 1 {
		t.Fatalf("level not recorded: %+v", run.LevelLog)
	}
	if err := rs.SetLevel("turbo", "user", ""); err == nil {
		t.Fatal("unknown level accepted")
	}
	// Workers are unaffected by the pilot's level for shell writes.
	impl := delegateScoped(t, rs, "impl")
	rs.SetLevel(model.LevelOrchestrate, "user", "")
	if d := rs.Gate(identity{role: RoleWorker, taskID: impl}, preBash("echo x > main.go")); !d.Allow {
		t.Fatalf("worker shell write refused: %+v", d)
	}
}

func TestGateRecordsAttribution(t *testing.T) {
	rs, ws, _ := gateSession(t, "read,edit,write,bash")
	rs.SetLevel(model.LevelSolo, "test", "")
	impl := delegateScoped(t, rs, "impl")
	post := func(id identity, r GateRequest) {
		r.Event = "PostToolUse"
		if d := rs.Gate(id, r); !d.Allow {
			t.Fatalf("post must always allow: %+v", d)
		}
	}
	post(identity{role: RoleCoordinator}, preWrite("Write", filepath.Join(ws, "main.go")))
	post(identity{role: RoleWorker, taskID: impl}, preWrite("Edit", filepath.Join(ws, "src", "x.go")))
	post(identity{role: RoleWorker, taskID: impl}, preBash("go test ./...")) // not a write: not recorded
	post(identity{role: RoleWorker, taskID: impl}, preBash("sed -i '' s/a/b/ y.go"))
	post(identity{role: RoleCoordinator}, preWrite("Write", filepath.Join(t.TempDir(), "scratch"))) // outside: not recorded
	post(identity{role: RolePair, agent: "pairy"}, preWrite("Write", filepath.Join(ws, "p.go")))
	w := rs.Run().Writes
	if len(w) != 4 {
		t.Fatalf("expected 4 records, got %+v", w)
	}
	if w[0].By != RoleCoordinator || w[0].Path != "main.go" || w[0].Tool != "Write" {
		t.Errorf("pilot write: %+v", w[0])
	}
	if w[1].By != impl || w[1].Path != "src/x.go" {
		t.Errorf("worker write: %+v", w[1])
	}
	if w[2].Tool != "Bash" || !strings.HasPrefix(w[2].Command, "sed -i") || w[2].By != impl {
		t.Errorf("worker shell: %+v", w[2])
	}
	if w[3].By != "pair:pairy" {
		t.Errorf("pair write unbound should attribute to the partner: %+v", w[3])
	}
}

func TestHubGateEnvelope(t *testing.T) {
	rs, ws, _ := gateSession(t, "")
	tok := rs.hub.IssueCoordinatorToken(rs.Run().ID)
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": ws,
		"tool_input": map[string]any{"file_path": "x.txt", "content": "hi"},
	})
	out, err := rs.hub.Gate(tok, payload)
	if err != nil {
		t.Fatal(err)
	}
	hso, _ := out["hookSpecificOutput"].(map[string]any)
	if hso["permissionDecision"] != "deny" || hso["hookEventName"] != "PreToolUse" || hso["permissionDecisionReason"] == "" {
		t.Fatalf("deny envelope wrong: %v", out)
	}
	// Allowed → empty object.
	payload, _ = json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "TodoWrite", "cwd": ws,
		"tool_input": map[string]any{"todos": []any{}}})
	out, _ = rs.hub.Gate(tok, payload)
	if len(out) != 0 {
		t.Fatalf("allow should be an empty object: %v", out)
	}
	// Unknown credential → ErrUnknownCredential (the HTTP face turns it into allow).
	if _, err := rs.hub.Gate("nope", payload); err != ErrUnknownCredential {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestBashWritesHeuristic(t *testing.T) {
	yes := []string{"rm x", "mkdir -p a/b", "touch a", "cp a b", "mv a b", "sudo rm x", "ls; rm x", "ls && mv a b",
		"echo hi > f", "echo hi >> f", "cmd 2>&1 > out.log", "tee f", "sed -i.bak s/a/b/ f", "perl -pi -e s/a/b/ f",
		"git checkout -- .", "git reset --hard", "git stash", "npm i", "pnpm add x", "pip install x", "cargo add x",
		"go get x", "go mod tidy", "python3 -c 'open(\"f\",\"w\")'", "find . -name '*.tmp' | xargs rm", "chmod +x f"}
	no := []string{"ls", "go test ./...", "go build ./...", "npm test", "git status", "git log --oneline", "git diff HEAD~1",
		"cat f | grep x", "echo 'a > b'", "echo \"x > y\"", "go vet ./... 2>&1", "curl -s http://x", "make test",
		"pytest -q", "grep -rn foo .", "ls > /dev/null", "cmd 2>/dev/null", "cmd >&2", "sed -n 1,5p f", "sed s/a/b/ f"}
	for _, c := range yes {
		if !bashWrites(c) {
			t.Errorf("should look like a write: %q", c)
		}
	}
	for _, c := range no {
		if bashWrites(c) {
			t.Errorf("should NOT look like a write: %q", c)
		}
	}
}

func TestNormalizeScope(t *testing.T) {
	ws := t.TempDir()
	got := NormalizeScope([]string{" ./src/a/ ", "src/a", filepath.Join(ws, "docs"), "", "/etc/passwd"}, ws)
	want := []string{"src/a", "docs", "/etc/passwd"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := NormalizeScope([]string{"src", "."}, ws); len(got) != 1 || got[0] != "." {
		t.Fatalf("whole-workspace scope: %v", got)
	}
	if got := NormalizeScope(nil, ws); got != nil {
		t.Fatalf("empty in, nil out: %v", got)
	}
	if !underScope("src/a/b.go", []string{"src/a"}) || underScope("src/ab/c.go", []string{"src/a"}) {
		t.Fatal("prefix must be a path boundary")
	}
	if !underScope("anything", []string{"."}) {
		t.Fatal("dot is the whole workspace")
	}
	// Symlinked workspace (macOS /var → /private/var) still resolves.
	os.Symlink(ws, filepath.Join(t.TempDir(), "link"))
}

// The review gate sees the main agent's own hands: code it wrote after the
// last independent review blocks finish(succeeded); documents do not.
func TestReviewGateCountsPilotWrites(t *testing.T) {
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	ws := t.TempDir()
	run := &model.Run{ID: "run_rv", WorkflowID: "wf", Mode: model.ModeDynamic,
		Tasks: map[string]*model.Task{}, CreatedAt: time.Now(), Level: model.LevelSolo}
	b := openBudget()
	rs := h.OpenRun(context.Background(), RunConfig{
		Run: run, Workflow: &model.Workflow{Mode: model.ModeDynamic, Budget: &b},
		Pool: []*model.Agent{
			{Name: "reviewer", Tools: "read,grep", Independent: true},
		},
		Workspace: ws, PilotTools: "read,edit,write,bash",
		Exec: newFakeExec(), OnChange: func(*model.Run) {},
	})
	t.Cleanup(rs.Close)
	declareTestEvidence(t, rs)
	pilot := identity{role: RoleCoordinator}
	post := func(r GateRequest) { r.Event = "PostToolUse"; rs.Gate(pilot, r) }

	// Docs only: no review needed.
	post(preWrite("Write", filepath.Join(ws, "README.md")))
	post(preWrite("Write", filepath.Join(ws, "docs", "plan.txt")))
	if err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "docs", Evidence: testEvidenceMet()}); err != nil {
		t.Fatalf("docs-only finish refused: %v", err)
	}
}

func TestReviewGateRefusesUnreviewedPilotCode(t *testing.T) {
	h := New("http://test", func() ([]*model.Agent, error) { return nil, nil })
	ws := t.TempDir()
	run := &model.Run{ID: "run_rv2", WorkflowID: "wf", Mode: model.ModeDynamic,
		Tasks: map[string]*model.Task{}, CreatedAt: time.Now(), Level: model.LevelSolo}
	b := openBudget()
	exec := newFakeExec()
	rs := h.OpenRun(context.Background(), RunConfig{
		Run: run, Workflow: &model.Workflow{Mode: model.ModeDynamic, Budget: &b},
		Pool: []*model.Agent{
			{Name: "reviewer", Tools: "read,grep", Independent: true},
		},
		Workspace: ws, PilotTools: "read,edit,write,bash",
		Exec: exec, OnChange: func(*model.Run) {},
	})
	t.Cleanup(rs.Close)
	declareTestEvidence(t, rs)
	pilot := identity{role: RoleCoordinator}
	post := func(r GateRequest) { r.Event = "PostToolUse"; rs.Gate(pilot, r) }

	post(preWrite("Edit", filepath.Join(ws, "main.go")))
	err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "done", Evidence: testEvidenceMet()})
	if err == nil || !strings.Contains(err.Error(), "your own code changes") || !strings.Contains(err.Error(), "main.go") {
		t.Fatalf("unreviewed pilot code must be refused: %v", err)
	}
	// A review after the change satisfies it.
	task, err := rs.Delegate(DelegateRequest{Agent: "reviewer", Title: "review", Instruction: "review main.go",
		Constraints: "none", Acceptance: okChecks(), CreatedBy: RoleCoordinator, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	rs.TaskStarted(task.ID)
	time.Sleep(2 * time.Millisecond)
	rs.CompleteTask(task.ID, "looks fine", nil, nil)
	if err := rs.Finish(&Verdict{Status: model.RunSucceeded, Summary: "done", Evidence: testEvidenceMet()}); err != nil {
		t.Fatalf("reviewed: %v", err)
	}
}
