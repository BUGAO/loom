package llm

import (
	"context"
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
