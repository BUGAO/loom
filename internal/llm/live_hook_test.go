package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// TestLiveHookGate answers whether a PreToolUse command hook in the session
// cwd's project settings (a) runs under the ACP adapter, (b) can deny a tool
// call under bypassPermissions, and (c) gets its reason text back to the
// model — the three facts a mode-aware tool gate would rest on.
func TestLiveHookGate(t *testing.T) {
	if os.Getenv("LOOM_LIVE_REPRO") == "" {
		t.Skip("live experiment against the real runtime; set LOOM_LIVE_REPRO=1 to run")
	}
	home, _ := os.UserHomeDir()
	adapter := filepath.Join(home, ".loom", "acp", "node_modules", ".bin", "claude-code-acp")
	backend := &ACP{Command: adapter}
	t.Setenv("LOOM_GATE_TOKEN", "tok-ENV-9931")

	dir := t.TempDir()
	hook := filepath.Join(dir, "gate.sh")
	script := `#!/bin/sh
in=$(cat)
echo "$in" >> "` + dir + `/hook.log"
echo "ENV-TOKEN=$LOOM_GATE_TOKEN" >> "` + dir + `/hook.log"
case "$in" in
  *'"tool_name":"Write"'*|*'"tool_name": "Write"'*)
    printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"LOOM-GATE-4471: orchestrate mode — this path is owned by task-3; delegate instead of writing."}}'
    ;;
esac
exit 0
`
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"hooks":{"PreToolUse":[{"matcher":"Write|Bash","hooks":[{"type":"command","command":"` + hook + `"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sess, err := backend.Open(ctx, SessionRequest{
		Kind:         KindWorker,
		SystemPrompt: "You are a test worker. Follow the instruction exactly.",
		Model:        "claude-haiku-4-5",
		WorkDir:      dir,
		Tools:        "read,write,edit,bash",
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

	res, err := sess.Prompt(ctx, "Do exactly this, in order: "+
		"1. Use the Write tool to create ./out.txt containing the word done. "+
		"2. If the tool call was refused, do NOT try any other way (no Bash). "+
		"3. Reply with one line: HOOK-PROBE write=<ok|refused> reason=<the exact refusal text you were given, or none>")
	t.Logf("prompt err: %v", err)
	if res != nil {
		t.Logf("text: %q", res.Text)
		t.Logf("transcript:\n%s", res.Transcript)
	}
	if log, err := os.ReadFile(filepath.Join(dir, "hook.log")); err == nil {
		t.Logf("hook saw:\n%s", log)
	} else {
		t.Errorf("HOOK NEVER RAN: no hook.log")
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Errorf("GATE BROKEN: the hook's deny did not stop Write")
	}
	if res == nil || !strings.Contains(res.Text, "LOOM-GATE-4471") {
		t.Errorf("REASON LOST: the model did not see the hook's reason text")
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "hook.log")); !strings.Contains(string(log), "ENV-TOKEN=tok-ENV-9931") {
		t.Errorf("ENV NOT INHERITED: the hook did not see the adapter process env")
	}
}
