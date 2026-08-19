package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"loom/internal/model"
)

// Acceptance execution. The contract is fixed at delegation time and executed
// by the engine in Go — the worker's report is its claim, this is the
// verdict. A task whose worker says "ok" but whose checks fail is a failed
// task, with the check output as evidence.

const (
	defaultCheckTimeout = 120 * time.Second
	maxCheckOutput      = 2000 // bytes of command output kept as evidence
)

// ValidateChecks rejects a malformed acceptance contract at delegation time,
// with errors written for the delegating model to fix.
func ValidateChecks(checks []model.AcceptanceCheck) error {
	if len(checks) == 0 {
		return fmt.Errorf("acceptance is required: define at least one machine-checkable criterion " +
			"(artifact_exists, artifact_contains, or command) BEFORE the work starts — the worker's own " +
			"report never decides whether it passed")
	}
	for i, c := range checks {
		switch c.Kind {
		case model.CheckArtifactExists:
			if err := checkPathOK(c.Path); err != nil {
				return fmt.Errorf("acceptance[%d]: %v", i, err)
			}
		case model.CheckArtifactContains:
			if err := checkPathOK(c.Path); err != nil {
				return fmt.Errorf("acceptance[%d]: %v", i, err)
			}
			if strings.TrimSpace(c.Pattern) == "" {
				return fmt.Errorf("acceptance[%d]: artifact_contains needs a pattern", i)
			}
			if _, err := regexp.Compile(c.Pattern); err != nil {
				return fmt.Errorf("acceptance[%d]: pattern does not compile: %v", i, err)
			}
		case model.CheckCommand:
			if strings.TrimSpace(c.Command) == "" {
				return fmt.Errorf("acceptance[%d]: command check needs a command", i)
			}
			if m := dryRunRe.FindString(c.Command); m != "" {
				return fmt.Errorf("acceptance[%d]: %q exercises a dry-run/mock path (%s). Acceptance runs the REAL path — "+
					"a dry run proves rendering, not delivery. Put the real command here; if the real path needs "+
					"something only the user has (credentials, an account), ask_user for it BEFORE delegating; if it "+
					"cannot be had, the goal stays unmet — an honest \"unverified\" beats a green dry run", i, c.Command, m)
			}
		default:
			return fmt.Errorf("acceptance[%d]: unknown kind %q; use artifact_exists, artifact_contains or command", i, c.Kind)
		}
	}
	// A build/lint check without a test check is the contract that shipped
	// 11k lines with zero tests: "compiles" was the whole bar. Compiling is
	// not working; the contract must run the tests too — which is also what
	// obliges the worker to write them.
	if bi := indexOfCommand(checks, buildCmdRe); bi >= 0 && indexOfCommand(checks, testCmdRe) < 0 {
		return fmt.Errorf("acceptance[%d]: %q is a build/lint check but this contract has no TEST command. A build "+
			"proves \"compiles\", not \"works\": add the project's test command (go test ./..., npm test, pytest, "+
			"cargo test, make test …). For a task that also writes the code, that means the worker writes the tests "+
			"— say so in the instruction. A documentation-only task should not carry a build check at all",
			bi, checks[bi].Command)
	}
	return nil
}

// buildCmdRe recognizes build / typecheck / lint / format checks — the
// "compiles and is tidy" family. testCmdRe recognizes the commands that
// actually execute tests. dryRunRe recognizes flags that swap the real path
// for a simulation.
var (
	buildCmdRe = regexp.MustCompile(`(^|[\s;&|(])(go\s+(build|vet|install)\b|gofmt\b|golangci-lint\b|staticcheck\b|` +
		`(npm|pnpm|yarn|bun)\s+(run\s+)?(build|lint|typecheck|type-check)\b|tsc\b|vite\s+build\b|eslint\b|` +
		`cargo\s+(build|check|clippy)\b|make\s+(build|all|lint)\b|mvn\s+(compile|package)\b|gradle\s+(build|assemble)\b|` +
		`dotnet\s+build\b|(python3?|py)\s+-m\s+(py_compile|compileall)\b|ruff\b|mypy\b|pyright\b)`)
	testCmdRe = regexp.MustCompile(`(^|[\s;&|(])(go\s+test\b|(npm|pnpm|yarn|bun)\s+(run\s+)?test\b|vitest\b|jest\b|mocha\b|` +
		`pytest\b|(python3?|py)\s+-m\s+(pytest|unittest)\b|cargo\s+test\b|make\s+(test|check)\b|mvn\s+(test|verify)\b|` +
		`gradle\s+test\b|dotnet\s+test\b|ctest\b|rspec\b|phpunit\b|swift\s+test\b|mix\s+test\b|dart\s+test\b|` +
		`flutter\s+test\b|deno\s+test\b|bats\b)`)
	dryRunRe = regexp.MustCompile(`--dry[-_]?run\b|\bdry_run\b|--no-?op\b|--simulate\b|--pretend\b|--fake\b|--mock\b|` +
		`--no-send\b|--skip-send\b|--what-if\b`)
)

func indexOfCommand(checks []model.AcceptanceCheck, re *regexp.Regexp) int {
	for i, c := range checks {
		if c.Kind == model.CheckCommand && re.MatchString(c.Command) {
			return i
		}
	}
	return -1
}

// NOTE: there used to be a ChecksFeasibleFor guard here refusing artifact
// contracts on agents without Write/Edit — the trap a real run walked into
// with a read-only reviewer. The hub's write_artifact tool dissolved it:
// every worker can deliver files now, so every artifact contract is feasible.

func checkPathOK(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return fmt.Errorf("artifact check needs a path relative to the workspace")
	}
	if filepath.IsAbs(p) || strings.Contains(p, "..") {
		return fmt.Errorf("artifact path must be relative to the workspace, without '..'")
	}
	return nil
}

// RunChecks executes an acceptance contract against the run's workspace
// directory and reports each check's outcome.
func RunChecks(ctx context.Context, workspace string, checks []model.AcceptanceCheck) (results []model.CheckResult, allPassed bool) {
	allPassed = true
	for _, c := range checks {
		r := runCheck(ctx, workspace, c)
		if !r.Passed {
			allPassed = false
		}
		results = append(results, r)
	}
	return results, allPassed
}

func runCheck(ctx context.Context, workspace string, c model.AcceptanceCheck) model.CheckResult {
	res := model.CheckResult{Check: c}
	switch c.Kind {
	case model.CheckArtifactExists:
		full := filepath.Join(workspace, c.Path)
		if st, err := os.Stat(full); err != nil {
			res.Detail = fmt.Sprintf("artifact %q does not exist in the workspace", c.Path)
		} else if st.IsDir() {
			res.Passed = true
			res.Detail = "directory exists"
		} else {
			res.Passed = true
			res.Detail = fmt.Sprintf("exists (%d bytes)", st.Size())
		}
	case model.CheckArtifactContains:
		full := filepath.Join(workspace, c.Path)
		data, err := os.ReadFile(full)
		if err != nil {
			res.Detail = fmt.Sprintf("artifact %q could not be read: %v", c.Path, err)
			break
		}
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			res.Detail = "pattern does not compile: " + err.Error()
			break
		}
		if re.Match(data) {
			res.Passed = true
			res.Detail = "pattern found"
		} else {
			res.Detail = fmt.Sprintf("pattern %q not found in %q", c.Pattern, c.Path)
		}
	case model.CheckCommand:
		timeout := defaultCheckTimeout
		if c.TimeoutSec > 0 {
			timeout = time.Duration(c.TimeoutSec) * time.Second
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, "sh", "-c", c.Command)
		cmd.Dir = workspace
		// Same discipline as the ACP terminal (acp_backend.go): the check runs
		// in its own process group. A check that backgrounds a server (`bin &`)
		// leaves that child holding the output pipe after sh exits; without a
		// WaitDelay bound, CombinedOutput blocks on the pipe forever — past the
		// check timeout, past the task timeout — and the task never settles.
		// Killing only cmd.Process on cancel misses the same child, so cancel
		// signals the whole group.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cmd.WaitDelay = 2 * time.Second
		out, err := cmd.CombinedOutput()
		// The verdict is decided; nothing the check started may outlive it.
		// ESRCH just means the group is already empty.
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		// WaitDelay expiry with a clean exit means the shell passed but a
		// backgrounded child kept the pipe open — the shell's status is the
		// verdict, not the leftover's.
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil
		}
		tail := strings.TrimSpace(string(out))
		if len(tail) > maxCheckOutput {
			tail = "…" + tail[len(tail)-maxCheckOutput:]
		}
		if cctx.Err() == context.DeadlineExceeded {
			res.Detail = fmt.Sprintf("command timed out after %s; output: %s", timeout, tail)
		} else if err != nil {
			res.Detail = fmt.Sprintf("command failed (%v); output: %s", err, tail)
		} else {
			res.Passed = true
			res.Detail = "exit 0"
			if tail != "" {
				res.Detail = "exit 0; output: " + tail
			}
		}
	default:
		res.Detail = "unknown check kind " + c.Kind
	}
	return res
}

// SummarizeResults renders check outcomes as one line for views and errors.
func SummarizeResults(results []model.CheckResult) string {
	passed := 0
	var firstFail string
	for _, r := range results {
		if r.Passed {
			passed++
		} else if firstFail == "" {
			firstFail = r.Detail
		}
	}
	s := fmt.Sprintf("%d/%d checks passed", passed, len(results))
	if firstFail != "" {
		s += "; first failure: " + firstFail
	}
	return s
}
