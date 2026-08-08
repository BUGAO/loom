package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

	res, err := sess.Prompt(ctx, "Use the Bash tool to run exactly `echo hello-loom-repro`, "+
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
