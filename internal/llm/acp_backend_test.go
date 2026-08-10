package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// Permission prompts are granted even for tools outside the allowlist: a
// rejection makes the runtime tell the model to stop and wait for a human,
// which kills unattended tasks. What actually stops an ungranted tool is the
// settings jail (TestDenyListFor) and the terminal allowlist check
// (TestTerminalRefusedWithoutBash) — layers that fail as readable errors.
func TestACPPromptPermissionAlwaysGranted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := fakeACP(t).Complete(ctx, Request{
		Kind:    KindNode,
		Prompt:  "do the thing",
		WorkDir: t.TempDir(),
		Tools:   "Read", // execute not in the allowlist — still granted here
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, `"status":"ok"`) {
		t.Fatalf("permission should be granted regardless of allowlist, got: %s", res.Text)
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

// ---- permission decisions ----

// The responder allows EVERYTHING, by design: a permission rejection makes
// Claude Code tell the model to "STOP and wait for the user", which kills an
// unattended task with no envelope (five tasks of the 2026-08-08 review run
// died that way). Enforcement lives in the layers that fail loudly instead —
// the settings jail for built-ins and CreateTerminal's allowlist check for
// shell (TestTerminalRefusedWithoutBash covers the latter).
func TestRequestPermissionAlwaysAllows(t *testing.T) {
	str := func(s string) *string { return &s }
	decide := func(c *acpClient, title string, raw any, opts []acp.PermissionOption) string {
		resp, err := c.RequestPermission(context.Background(), acp.RequestPermissionRequest{
			Options:  opts,
			ToolCall: acp.ToolCallUpdate{Title: str(title), RawInput: raw},
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Outcome.Selected == nil {
			return "cancelled"
		}
		return string(resp.Outcome.Selected.OptionId)
	}
	std := []acp.PermissionOption{
		{OptionId: "always", Name: "Always Allow", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
	}

	// Even a tool-less agent's prompts are allowed here — the jail and the
	// terminal check are what actually stop it, with readable errors.
	none := termClient("")
	if got := decide(none, "`npm install`", map[string]any{"command": "npm install"}, std); got != "allow" {
		t.Fatalf("responder must allow (allow_once preferred), got %s", got)
	}
	if got := decide(none, "mcp__loom__report_progress", map[string]any{"text": "hi"}, std); got != "allow" {
		t.Fatalf("hub tools allowed, got %s", got)
	}

	// Without an allow_once option it falls back to allow_always, then to
	// whatever exists.
	if got := decide(none, "x", nil, std[:1]); got != "always" {
		t.Fatalf("allow_always fallback, got %s", got)
	}
	if got := decide(none, "x", nil, std[2:]); got != "reject" {
		t.Fatalf("last-resort first option, got %s", got)
	}
	if got := decide(none, "x", nil, nil); got != "cancelled" {
		t.Fatalf("no options → cancelled, got %s", got)
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

// The adapter's real shape: the ENTIRE shell command line arrives as a single
// Command string with no Args. This must run under shell semantics — treating
// it as argv[0] made every non-trivial Bash call die with ENOENT, which is
// exactly how the poe2-build-advisor run lost all four implementer tasks.
func TestTerminalRunsBareCommandLineThroughShell(t *testing.T) {
	c := termClient("Bash")
	res, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
		Command: "echo one && echo two | tr a-z A-Z",
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
	out, _ := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{TerminalId: res.TerminalId})
	if !strings.Contains(out.Output, "one") || !strings.Contains(out.Output, "TWO") {
		t.Fatalf("shell operators must work (&&, |), got %q", out.Output)
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

// startBackgroundedChild runs the incident's shape — `server & …` where the
// shell exits at once, leaving only the backgrounded child — and returns that
// child's pid once it is confirmed alive.
func startBackgroundedChild(t *testing.T, c *acpClient) int {
	t.Helper()
	pidfile := filepath.Join(t.TempDir(), "pid")
	res, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
		Command: fmt.Sprintf("sleep 60 & echo $! > %s", pidfile),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{TerminalId: res.TerminalId}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("bad pidfile %q: %v", b, err)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("background child %d should be alive after its shell exits", pid)
	}
	return pid
}

// waitProcessGone fails the test (and reaps the straggler) if pid survives.
func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("pid %d survived the group kill", pid)
}

// The poe2_trade incident, distilled: a worker ran `server &`, the shell
// exited, and the server outlived the session to squat on its port — the old
// cleanup skipped terminals whose shell had exited, and could only reach the
// shell's own pid anyway. Session close must reap the whole process group.
func TestKillTerminalsReapsBackgroundedGrandchild(t *testing.T) {
	c := termClient("Bash")
	pid := startBackgroundedChild(t, c)
	c.killTerminals()
	waitProcessGone(t, pid)
}

// ReleaseTerminal had the same exited-shell short-circuit; it too must reap
// the group, not just the (long-gone) shell.
func TestReleaseTerminalReapsBackgroundedGrandchild(t *testing.T) {
	c := termClient("Bash")
	pid := startBackgroundedChild(t, c)
	c.mu.Lock()
	var id string
	for k := range c.terminals {
		id = k
	}
	c.mu.Unlock()
	if _, err := c.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{TerminalId: id}); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, pid)
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
