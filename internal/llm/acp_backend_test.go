package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"
)

var (
	fakeBinOnce sync.Once
	fakeBin     string
	fakeBinErr  error
)

// fakeACP returns an ACP backend wired to the raw-protocol fake agent fixture,
// exercising the SDK client against a wire-level peer. The fixture is compiled
// once per test run (the node's WorkDir is outside the module, so `go run`
// can't be used directly).
func fakeACP(t *testing.T) *ACP {
	t.Helper()
	fakeBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "loom-fakeagent")
		if err != nil {
			fakeBinErr = err
			return
		}
		fakeBin = filepath.Join(dir, "fakeagent")
		out, err := exec.Command("go", "build", "-o", fakeBin, "./testdata/fakeagent").CombinedOutput()
		if err != nil {
			fakeBinErr = fmt.Errorf("build fakeagent: %v: %s", err, out)
		}
	})
	if fakeBinErr != nil {
		t.Fatalf("fake agent fixture build failed: %v", fakeBinErr)
	}
	return &ACP{Command: fakeBin}
}

func TestACPPromptAllowed(t *testing.T) {
	var mu sync.Mutex
	var acts []string
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := fakeACP(t).Complete(ctx, Request{
		Kind:    KindNode,
		Prompt:  "do the thing",
		WorkDir: t.TempDir(),
		Tools:   "Read,Bash", // execute is allowed
		OnActivity: func(s string) {
			mu.Lock()
			acts = append(acts, s)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, `"status":"ok"`) {
		t.Fatalf("expected ok envelope in message text, got: %s", res.Text)
	}
	if !strings.Contains(res.Transcript, "[tool:execute] run tests") {
		t.Fatalf("transcript missing tool line:\n%s", res.Transcript)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(acts) != 1 || acts[0] != "run tests" {
		t.Fatalf("activity callbacks: %v", acts)
	}
}

func TestACPPromptRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := fakeACP(t).Complete(ctx, Request{
		Kind:    KindNode,
		Prompt:  "do the thing",
		WorkDir: t.TempDir(),
		Tools:   "Read", // execute NOT allowed → permission rejected
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "not permitted") {
		t.Fatalf("expected rejection path, got: %s", res.Text)
	}
}

// The tool jail: permission answers alone cannot stop read-only tools or Task
// (Claude Code never asks), so the allowlist must also materialize as deny
// rules the runtime core enforces.
func TestDenyListFor(t *testing.T) {
	has := func(list []string, name string) bool {
		for _, n := range list {
			if n == name {
				return true
			}
		}
		return false
	}

	// Empty allowlist (the coordinator): everything is denied, Task included.
	deny := denyListFor("")
	for _, name := range []string{"Task", "Read", "Grep", "Glob", "Bash", "Write", "Edit", "WebFetch", "WebSearch"} {
		if !has(deny, name) {
			t.Errorf("empty allowlist should deny %s", name)
		}
	}

	// A worker allowlist keeps exactly what it grants — and never Task.
	deny = denyListFor("Read,Write,Edit,Bash")
	for _, name := range []string{"Read", "Write", "Edit", "MultiEdit", "NotebookEdit", "Bash", "BashOutput"} {
		if has(deny, name) {
			t.Errorf("allowlist Read,Write,Edit,Bash should not deny %s", name)
		}
	}
	for _, name := range []string{"Task", "Grep", "Glob", "WebFetch", "WebSearch"} {
		if !has(deny, name) {
			t.Errorf("allowlist Read,Write,Edit,Bash should still deny %s", name)
		}
	}
}

func TestOpenWritesToolJail(t *testing.T) {
	dir := t.TempDir()
	if err := writeToolJail(dir, "Read"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dir + "/.claude/settings.local.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	deny := strings.Join(cfg.Permissions.Deny, ",")
	if strings.Contains(deny, "Read") {
		t.Fatalf("granted Read must not be denied: %s", deny)
	}
	if !strings.Contains(deny, "Task") || !strings.Contains(deny, "Bash") {
		t.Fatalf("jail must deny Task and Bash: %s", deny)
	}
}

// ---- terminal support (the channel Bash actually rides on) ----

func termClient(tools string) *acpClient {
	return &acpClient{allow: allowedFn(tools)}
}

func TestTerminalRunsCommandToExit(t *testing.T) {
	c := termClient("Bash")
	res, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
		Command: "sh", Args: []string{"-c", "echo hello-loom; exit 0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wait, err := c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{TerminalId: res.TerminalId})
	if err != nil {
		t.Fatal(err)
	}
	if wait.ExitCode == nil || *wait.ExitCode != 0 {
		t.Fatalf("want exit 0, got %+v", wait)
	}
	out, err := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{TerminalId: res.TerminalId})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "hello-loom") {
		t.Fatalf("output missing: %q", out.Output)
	}
	if out.ExitStatus == nil || out.ExitStatus.ExitCode == nil || *out.ExitStatus.ExitCode != 0 {
		t.Fatalf("output should carry the exit status: %+v", out.ExitStatus)
	}
	if _, err := c.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{TerminalId: res.TerminalId}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{TerminalId: res.TerminalId}); err == nil {
		t.Fatal("released terminal should be unknown")
	}
}

func TestTerminalReportsNonZeroExit(t *testing.T) {
	c := termClient("Bash")
	res, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
		Command: "sh", Args: []string{"-c", "echo boom >&2; exit 3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wait, _ := c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{TerminalId: res.TerminalId})
	if wait.ExitCode == nil || *wait.ExitCode != 3 {
		t.Fatalf("want exit 3, got %+v", wait)
	}
	out, _ := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{TerminalId: res.TerminalId})
	if !strings.Contains(out.Output, "boom") {
		t.Fatalf("stderr should be captured: %q", out.Output)
	}
}

func TestTerminalKill(t *testing.T) {
	c := termClient("Bash")
	res, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
		Command: "sh", Args: []string{"-c", "sleep 30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.KillTerminal(context.Background(), acp.KillTerminalRequest{TerminalId: res.TerminalId}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{TerminalId: res.TerminalId})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("killed terminal never reported exit")
	}
}

func TestTerminalOutputBounded(t *testing.T) {
	c := termClient("Bash")
	limit := 200
	res, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
		Command: "sh", Args: []string{"-c", "i=0; while [ $i -lt 200 ]; do echo 0123456789; i=$((i+1)); done"},
		OutputByteLimit: &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{TerminalId: res.TerminalId})
	out, _ := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{TerminalId: res.TerminalId})
	if len(out.Output) > limit {
		t.Fatalf("output exceeds the byte limit: %d > %d", len(out.Output), limit)
	}
	if !out.Truncated {
		t.Fatal("truncation must be reported")
	}
}

// A session whose allowlist grants no shell gets no terminal — the second
// line of defense behind the settings jail.
func TestTerminalRefusedWithoutBash(t *testing.T) {
	c := termClient("") // the coordinator's allowlist
	if _, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
		Command: "sh", Args: []string{"-c", "true"},
	}); err == nil {
		t.Fatal("terminal without Bash in the allowlist must be refused")
	}
}

// Truncation must cut on a UTF-8 boundary: the retained output stays a valid
// string even when the cut lands mid-rune.
func TestTerminalTruncationKeepsValidUTF8(t *testing.T) {
	c := termClient("Bash")
	limit := 100
	res, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
		Command: "sh", Args: []string{"-c", `i=0; while [ $i -lt 50 ]; do printf '中文字符串测试'; i=$((i+1)); done`},
		OutputByteLimit: &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{TerminalId: res.TerminalId})
	out, _ := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{TerminalId: res.TerminalId})
	if !out.Truncated {
		t.Fatal("truncation must be reported")
	}
	if !utf8.ValidString(out.Output) {
		t.Fatalf("truncated output is not valid UTF-8: %q", out.Output[:20])
	}
	if len(out.Output) > limit {
		t.Fatalf("output exceeds limit: %d", len(out.Output))
	}
}

// A session without a requested limit still gets a bounded buffer.
func TestTerminalDefaultOutputBound(t *testing.T) {
	c := termClient("Bash")
	res, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
		Command: "sh", Args: []string{"-c", "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{TerminalId: res.TerminalId})
	tp := c.terminal(res.TerminalId)
	if tp == nil || tp.limit != defaultTermOutputLimit {
		t.Fatalf("default output bound not applied: %+v", tp)
	}
}
