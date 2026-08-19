package llm

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"loom/internal/hub"
	"loom/internal/model"
)

// TestLiveGatePipeline runs the whole gate end to end against the real
// runtime: a freshly built loom binary as the hook, the hub's /gate endpoint
// behind it, and a session whose credential rides in its environment. At
// ORCHESTRATE the main agent's Write and its shell fallback are both refused
// with the reason in front of the model; at SOLO the same write goes through
// and lands in the run's attribution record.
func TestLiveGatePipeline(t *testing.T) {
	if os.Getenv("LOOM_LIVE_REPRO") == "" {
		t.Skip("live experiment against the real runtime; set LOOM_LIVE_REPRO=1 to run")
	}
	// A real loom binary for the hook.
	bin := filepath.Join(t.TempDir(), "loom")
	build := exec.Command("go", "build", "-o", bin, "loom/cmd/loom")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build loom: %v\n%s", err, out)
	}
	gateExe = bin
	t.Cleanup(func() { gateExe = "" })

	// The hub behind an HTTP server.
	h := hub.New("", func() ([]*model.Agent, error) { return nil, nil })
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	h2 := hub.New(srv.URL, func() ([]*model.Agent, error) { return nil, nil })
	_ = h2 // baseURL only matters for GateEndpoint; reuse h's handler but h2's URL below
	ws := t.TempDir()
	data := t.TempDir()
	run := &model.Run{ID: "run_live_gate", WorkflowID: "wf", Mode: model.ModeDynamic,
		Tasks: map[string]*model.Task{}, CreatedAt: time.Now()}
	b := model.DefaultBudget()
	b.ApprovalPolicy = model.ApprovalNone
	rs := h.OpenRun(context.Background(), hub.RunConfig{
		Run: run, Workflow: &model.Workflow{Mode: model.ModeDynamic, Budget: &b},
		Workspace: ws, ProtectDir: data, PilotTools: "read,edit,write,bash",
		Exec: nopExec{}, OnChange: func(*model.Run) {},
	})
	defer rs.Close()
	tok := h.IssueCoordinatorToken(run.ID)

	home, _ := os.UserHomeDir()
	adapter := filepath.Join(home, ".loom", "acp", "node_modules", ".bin", "claude-code-acp")
	backend := &ACP{Command: adapter, ProtectDir: data}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	sess, err := backend.Open(ctx, SessionRequest{
		Kind:         KindCoordinator,
		SystemPrompt: "You are a test agent. Follow the instruction exactly and literally.",
		Model:        "claude-haiku-4-5",
		WorkDir:      ws,
		Tools:        "read,edit,write,bash",
		Gate:         &GateHook{URL: srv.URL + "/gate", Token: tok},
		OnActivity:   func(s string) { t.Logf("activity: %s", s) },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	s := sess.(*acpSession)
	if _, err := s.conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
		SessionId: s.id, ModeId: acp.SessionModeId("bypassPermissions"),
	}); err != nil {
		t.Logf("set bypassPermissions: %v", err)
	}

	// Round 1: ORCHESTRATE (the default). Both hands refused.
	res, err := sess.Prompt(ctx, "Do exactly this, in order, and do not improvise other approaches: "+
		"1. Use the Write tool to create ./out.txt containing the word done. "+
		"2. If that was refused, use the Bash tool to run exactly: echo done > out2.txt "+
		"3. Then run the Bash tool: ls "+
		"4. Reply with one line: GATE-PROBE write=<ok|refused> bash=<ok|refused> reasons=<the refusal texts you were given, verbatim>")
	t.Logf("r1 err=%v text=%q", err, textOf(res))
	if _, err := os.Stat(filepath.Join(ws, "out.txt")); err == nil {
		t.Error("ORCHESTRATE: Write went through")
	}
	if _, err := os.Stat(filepath.Join(ws, "out2.txt")); err == nil {
		t.Error("ORCHESTRATE: shell redirect went through")
	}
	if res == nil || !strings.Contains(res.Text, "ORCHESTRATE") {
		t.Error("the model did not see the level reason")
	}
	if n := len(rs.Run().Writes); n != 0 {
		t.Errorf("no write should be attributed yet, got %d", n)
	}

	// Round 2: SOLO. Hands free, write attributed.
	if err := rs.SetLevel(model.LevelSolo, "test", ""); err != nil {
		t.Fatal(err)
	}
	res, err = sess.Prompt(ctx, "Now use the Write tool to create ./ok.txt containing the word done, then reply GATE-PROBE-2 write=<ok|refused>.")
	t.Logf("r2 err=%v text=%q", err, textOf(res))
	if _, err := os.Stat(filepath.Join(ws, "ok.txt")); err != nil {
		t.Error("SOLO: Write was refused")
	}
	w := rs.Run().Writes
	if len(w) == 0 || w[len(w)-1].Path != "ok.txt" || w[len(w)-1].By != hub.RoleCoordinator {
		t.Errorf("write not attributed to the main agent: %+v", w)
	}
	// The workspace jail is loom's only while loom is there.
	sess.Close()
	if _, err := os.Stat(filepath.Join(ws, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Error("jail left behind in the user workspace after the last session closed")
	}
}

type nopExec struct{}

func (nopExec) StartTask(*hub.RunSession, string)  {}
func (nopExec) CancelTask(*hub.RunSession, string) {}

func textOf(r *Result) string {
	if r == nil {
		return ""
	}
	return r.Text
}
