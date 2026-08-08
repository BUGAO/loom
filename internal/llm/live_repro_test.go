package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveBashOutsideDirs mirrors the 2026-08-08 review-run failures: same
// session shape as a real implementer task (agent-home cwd, an AddDirs
// workspace), with a Bash command that reads a directory OUTSIDE both. The
// control repro (echo in cwd) passes, so whatever kills these turns is in the
// out-of-scope path handling.
func TestLiveBashOutsideDirs(t *testing.T) {
	if os.Getenv("LOOM_LIVE_REPRO") == "" {
		t.Skip("live repro against the real runtime; set LOOM_LIVE_REPRO=1 to run")
	}
	home, _ := os.UserHomeDir()
	adapter := filepath.Join(home, ".loom", "acp", "node_modules", ".bin", "claude-code-acp")
	acp := &ACP{Command: adapter}

	// NOT t.TempDir(): that path embeds the test name, and any "bash"/"loom"
	// substring in the command line used to flip the permission decision —
	// exactly the coin-flip bug this repro must be able to catch.
	outside, err := os.MkdirTemp("", "wf-review-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })
	os.WriteFile(filepath.Join(outside, "marker.txt"), []byte("outside-marker\n"), 0o644)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sess, err := acp.Open(ctx, SessionRequest{
		Kind:         KindWorker,
		SystemPrompt: "You are a test worker. Follow the instruction exactly.",
		Model:        "claude-haiku-4-5",
		WorkDir:      t.TempDir(),
		AddDirs:      []string{t.TempDir()},
		Tools:        "Read,Write,Edit,Bash",
		OnActivity:   func(s string) { t.Logf("activity: %s", s) },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	res, err := sess.Prompt(ctx, "Use the Bash tool to run exactly `ls -la "+outside+" 2>&1 | head -20`, "+
		"then reply with one line: OUTSIDE-DONE <number of entries you saw>")
	t.Logf("prompt err: %v", err)
	if res != nil {
		t.Logf("stop_reason: %q", res.StopReason)
		t.Logf("text: %q", res.Text)
		t.Logf("transcript:\n%s", res.Transcript)
	}
	if s, ok := sess.(*acpSession); ok {
		t.Logf("cli stderr tail: %s", s.stderrTail())
	}
	if err == nil && res != nil && res.Text == "" {
		t.Errorf("REPRODUCED: turn ended (stop=%q) with no message text", res.StopReason)
	}
}

// TestLiveBashRepro reproduces the poe2-build-advisor failure mode against the
// real adapter + CLI: a worker session with Bash allowed dies the moment it
// runs a shell command, ending the turn with no text and no envelope.
//
// It talks to the real Claude runtime (a few cents of haiku), so it only runs
// when explicitly asked for: LOOM_LIVE_REPRO=1 go test ./internal/llm/ -run LiveBashRepro -v
func TestLiveBashRepro(t *testing.T) {
	if os.Getenv("LOOM_LIVE_REPRO") == "" {
		t.Skip("live repro against the real runtime; set LOOM_LIVE_REPRO=1 to run")
	}
	home, _ := os.UserHomeDir()
	adapter := filepath.Join(home, ".loom", "acp", "node_modules", ".bin", "claude-code-acp")
	if _, err := os.Stat(adapter); err != nil {
		t.Fatalf("adapter not found: %v", err)
	}
	acp := &ACP{Command: adapter}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sess, err := acp.Open(ctx, SessionRequest{
		Kind:         KindWorker,
		SystemPrompt: "You are a test worker. Follow the instruction exactly.",
		Model:        "claude-haiku-4-5",
		WorkDir:      t.TempDir(),
		Tools:        "Read,Write,Edit,Bash",
		OnActivity:   func(s string) { t.Logf("activity: %s", s) },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	// "hello-check-42" deliberately contains neither "loom" nor "bash": the
	// old title-matching permission path approved commands by such accidents.
	res, err := sess.Prompt(ctx, "Use the Bash tool to run exactly `echo hello-check-42`, "+
		"then reply with one line: REPRO-DONE <the command output>")
	t.Logf("prompt err: %v", err)
	if res != nil {
		t.Logf("stop_reason: %q", res.StopReason)
		t.Logf("text: %q", res.Text)
		t.Logf("transcript:\n%s", res.Transcript)
	}
	if s, ok := sess.(*acpSession); ok {
		t.Logf("cli stderr tail: %s", s.stderrTail())
	}
	if err == nil && res != nil && res.Text == "" {
		t.Errorf("REPRODUCED: turn ended (stop=%q) with no message text after the Bash call", res.StopReason)
	}
}
