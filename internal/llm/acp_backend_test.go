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
