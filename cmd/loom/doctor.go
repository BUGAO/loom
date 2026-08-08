package main

// loom doctor — dependency and environment checks. Every failure this week
// traced back to the runtime chain (CLI ↔ adapter ↔ ACP), so the doctor does
// not stop at "file exists": it runs the real ACP initialize handshake.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"loom/internal/llm"
)

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "check every runtime dependency and report what is broken",
		Long: `Check everything a real loom run depends on:

  claude CLI      the Claude Code CLI ("claude") — planning and execution
  node            the ACP adapter is a Node program
  acp adapter     claude-code-acp, resolved the same way the server does
  acp handshake   a REAL ACP initialize round-trip against the adapter
                  (spawns it, speaks the protocol, no model call, no cost)
  data dir        exists / writable — agents, workflows, run history
  output root     writable — where dynamic runs deliver
  server          whether a loom server is currently running (informational)
  npm             only needed to install or update the adapter (warning)

Exit code is 0 when everything a run needs is healthy, 1 otherwise.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return runDoctor() },
	}
}

type checkResult struct {
	name   string
	ok     bool
	warn   bool // true = degraded but not run-breaking
	detail string
}

func runDoctor() error {
	var results []checkResult
	add := func(name string, ok, warn bool, detail string) {
		results = append(results, checkResult{name, ok, warn, detail})
	}

	// claude CLI — the planner and every real session need it.
	if path, err := exec.LookPath("claude"); err != nil {
		add("claude CLI", false, false, "not found on PATH — install Claude Code; only dry-run works without it")
	} else {
		add("claude CLI", true, false, fmt.Sprintf("%s (%s)", cmdVersion(path, "--version"), path))
	}

	// node — the adapter is a Node program.
	nodeOK := false
	if path, err := exec.LookPath("node"); err != nil {
		add("node", false, false, "not found on PATH — the ACP adapter cannot run without it")
	} else {
		nodeOK = true
		add("node", true, false, fmt.Sprintf("%s (%s)", cmdVersion(path, "--version"), path))
	}

	// adapter — resolved exactly like the server resolves it.
	adapter := resolveACP(flagACP)
	if adapter == "" {
		add("acp adapter", false, false,
			"claude-code-acp not found (flag/env/PATH/.acp/~/.loom/acp) — dynamic workflows will refuse to start; run ./scripts/install.sh")
	} else {
		add("acp adapter", true, false, fmt.Sprintf("%s (%s)", adapterVersion(adapter), adapter))
	}

	// handshake — the deep check: actually speak ACP to the adapter.
	switch {
	case adapter == "":
		add("acp handshake", false, false, "skipped: no adapter")
	case !nodeOK:
		add("acp handshake", false, false, "skipped: no node")
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		proto, err := llm.ProbeACP(ctx, adapter)
		cancel()
		if err != nil {
			add("acp handshake", false, false, err.Error())
		} else {
			add("acp handshake", true, false, proto)
		}
	}

	// directories.
	add(dirCheck("data dir", flagData))
	add(dirCheck("output root", flagOutput))

	// server — informational: not running is a fine state for doctor.
	if pid, ok := findServer(flagData, flagAddr); ok {
		add("server", true, false, fmt.Sprintf("running (pid %d, %s)", pid, flagAddr))
	} else {
		add("server", true, false, fmt.Sprintf("not running (start: loom start, addr %s)", flagAddr))
	}

	// npm — only needed at install time.
	if path, err := exec.LookPath("npm"); err != nil {
		add("npm", true, true, "not found — only needed to install/update the ACP adapter")
	} else {
		add("npm", true, false, fmt.Sprintf("%s (%s)", cmdVersion(path, "--version"), path))
	}

	failures := 0
	fmt.Println("loom doctor")
	fmt.Println()
	for _, r := range results {
		mark := "✓"
		if r.warn {
			mark = "!"
		}
		if !r.ok {
			mark = "✗"
			failures++
		}
		fmt.Printf("  %s %-14s %s\n", mark, r.name, r.detail)
	}
	fmt.Println()
	if failures > 0 {
		return fmt.Errorf("%d problem(s) found", failures)
	}
	fmt.Println("all checks passed")
	return nil
}

// dirCheck verifies a directory exists (creating it if needed) and is
// actually writable — a permission problem here fails every run late and
// confusingly, so doctor fails it early and clearly.
func dirCheck(name, dir string) (string, bool, bool, string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return name, false, false, fmt.Sprintf("%s — cannot create: %v", dir, err)
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return name, false, false, fmt.Sprintf("%s — not writable: %v", dir, err)
	}
	f.Close()
	os.Remove(f.Name())
	return name, true, false, dir + " (writable)"
}

// cmdVersion runs `bin arg` briefly and returns its first output line.
func cmdVersion(bin, arg string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, arg).Output()
	if err != nil {
		return "version unknown"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}

// adapterVersion digs the adapter package's version out of its package.json.
// The bin path is a symlink into the package; resolve it and walk upward.
func adapterVersion(bin string) string {
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		resolved = bin
	}
	for dir := filepath.Dir(resolved); ; dir = filepath.Dir(dir) {
		if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
			var pkg struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			if json.Unmarshal(b, &pkg) == nil && strings.Contains(pkg.Name, "claude-code-acp") {
				return "v" + pkg.Version
			}
		}
		if dir == filepath.Dir(dir) {
			return "version unknown"
		}
	}
}
