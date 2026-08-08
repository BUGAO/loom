// loom — a plan-then-execute agent workflow orchestrator.
//
//	loom                 run the server in the foreground
//	loom start           run the server in the background
//	loom stop            stop the background server
//	loom restart         stop if running, then start in the background
//	loom status          report whether a server is running and where
//	loom doctor          check every runtime dependency
//
// Run `loom help <command>` for details on any command.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"loom/internal/engine"
	"loom/internal/hub"
	"loom/internal/llm"
	"loom/internal/seed"
	"loom/internal/server"
	"loom/internal/store"
)

// Flags shared by every command: the root persistent flag set. Control
// commands use them to find the right server; serve uses them to run one.
var (
	flagAddr    string
	flagData    string
	flagOutput  string
	flagDry     bool
	flagACP     string
	flagBackend string
)

func main() {
	root := &cobra.Command{
		Use:   "loom",
		Short: "agent workflow orchestrator — static DAGs and dynamic coordinator runs",
		Long: `loom orchestrates agent workflows in two modes sharing one agent pool, one
web UI and one execution layer:

  static   a planner assembles a DAG up front; after (optional) human
           approval, a deterministic engine executes it. Termination is
           guaranteed by the DAG's acyclicity.
  dynamic  a coordinator agent decomposes, delegates, follows up and
           converges at runtime; the task tree emerges instead of being
           declared. Termination is guaranteed by hard budget limits.

Execution rides the Agent Client Protocol: every agent is an isolated
Claude Code session with its own home, workspace and skills.

Running loom with no command starts the server in the FOREGROUND (logs to
stdout, Ctrl-C to stop). Use "loom start" for a background server.`,
		Args: cobra.NoArgs,
		Run:  func(*cobra.Command, []string) { serve() },
		Example: `  loom                      # foreground server on :7333
  loom start                # background server
  loom restart              # swap in a freshly built binary
  loom doctor               # verify claude CLI, ACP adapter, dirs
  loom start --dry-run      # zero-cost demo mode default
  loom --addr 127.0.0.1:7333 --data ./data`,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		SilenceUsage:      true,
	}

	pf := root.PersistentFlags()
	pf.StringVar(&flagAddr, "addr", ":7333", "listen address (all interfaces by default; use 127.0.0.1:7333 for loopback only)")
	pf.StringVar(&flagData, "data", defaultDataDir(), "data directory (env: LOOM_DATA)")
	pf.StringVar(&flagOutput, "output", defaultOutputRoot(), "deliverable folder root for dynamic runs (env: LOOM_OUTPUT)")
	pf.BoolVar(&flagDry, "dry-run", false, "pre-check the dry-run switch for new runs (zero-cost demo mode)")
	pf.StringVar(&flagACP, "acp-cmd", "", "ACP agent command (default: autodetect claude-code-acp; env: LOOM_ACP_CMD)")
	pf.StringVar(&flagBackend, "backend", "", "deprecated: 'mock' means --dry-run; other values are ignored")
	pf.MarkHidden("backend")

	root.AddCommand(startCmd(), stopCmd(), restartCmd(), statusCmd(), doctorCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// serve runs the server in the foreground: the root command's job.
func serve() {
	if flagBackend == "mock" {
		flagDry = true
	} else if flagBackend != "" && flagBackend != "acp" {
		log.Printf("note: --backend is deprecated; execution runtime is per-agent now, dry-run is per-run")
	}

	st, err := store.New(flagData)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	if err := seed.EnsureDefaults(st); err != nil {
		log.Fatalf("seed: %v", err)
	}

	// The backend registry is keyed by role, not by "ways to start a run":
	//   mock     — the dry-run executor (a per-run switch, not a runtime)
	//   planner  — CLI single-shot for plan assembly (no session needed)
	//   claude   — the "claude" runtime agents declare; ACP-hosted sessions,
	//              degrading to the CLI when the adapter is missing (static
	//              still works; dynamic then refuses with a clear error)
	// Future runtimes (codex, …) are additional keys matching RuntimeCatalog.
	backends := map[string]llm.Backend{
		"mock":    &llm.Mock{},
		"planner": &llm.Claude{},
	}
	if cmd := resolveACP(flagACP); cmd != "" {
		backends["claude"] = &llm.ACP{Command: cmd}
		fmt.Printf("claude runtime: ACP via %s\n", cmd)
	} else {
		log.Printf("warning: claude-code-acp not found (run `loom doctor`); claude runtime degrades to CLI single-shot — dynamic workflows will refuse to start")
		backends["claude"] = &llm.Claude{}
	}

	broker := engine.NewBroker()
	orchestration := hub.New(baseURL(flagAddr), st.ListAgents)
	eng := engine.New(st, backends, broker, orchestration)
	eng.SetOutputRoot(flagOutput)
	eng.RecoverInterrupted()

	srv := server.New(st, eng, broker, orchestration.Handler(), flagDry)
	mode := "real"
	if flagDry {
		mode = "dry-run default"
	}

	// The pidfile is what `loom stop/restart/status` key on; a SIGTERM/SIGINT
	// cleans it up. Dying without cleanup is fine — stale pidfiles are
	// detected by probing the process.
	pid := pidPath(st.Dir())
	os.WriteFile(pid, []byte(strconv.Itoa(os.Getpid())), 0o644)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-sig
		os.Remove(pid)
		os.Exit(0)
	}()

	fmt.Printf("loom listening on %s (data: %s, %s)\n", flagAddr, st.Dir(), mode)
	log.Fatal(http.ListenAndServe(flagAddr, srv.Handler()))
}

// resolveACP finds the claude-code-acp binary: explicit flag/env first, then
// PATH, then the project-local install under .acp/, then the per-user install
// under ~/.loom/acp (where scripts/install.sh puts it).
func resolveACP(explicit string) string {
	candidates := []string{explicit, os.Getenv("LOOM_ACP_CMD")}
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	if p, err := exec.LookPath("claude-code-acp"); err == nil {
		return p
	}
	local := filepath.Join(".acp", "node_modules", ".bin", "claude-code-acp")
	if _, err := os.Stat(local); err == nil {
		abs, _ := filepath.Abs(local)
		return abs
	}
	if home, err := os.UserHomeDir(); err == nil {
		installed := filepath.Join(home, ".loom", "acp", "node_modules", ".bin", "claude-code-acp")
		if _, err := os.Stat(installed); err == nil {
			return installed
		}
	}
	return ""
}

// baseURL is the address agents use to reach loom's hub. Agents run as local
// subprocesses, so a wildcard listen address is rewritten to loopback rather
// than handed out verbatim — "http://:7333/mcp" is not a dialable URL.
func baseURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "[::]" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// defaultDataDir resolves where loom keeps its state. The flag wins; then
// LOOM_DATA; then the stable per-user store ~/.loom/data. Deliberately NOT
// cwd-relative: loom lives on PATH, and a relative default would silently
// create a fresh, empty store in whatever directory the user happened to be
// in, making their agents and run history look lost. Dev checkouts that want
// a repo-local store pass --data ./data explicitly.
func defaultDataDir() string {
	if d := os.Getenv("LOOM_DATA"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".loom", "data")
	}
	return "./data"
}

// defaultOutputRoot is where dynamic runs deliver: a stable, user-visible
// folder, so artifacts never hide inside loom's internal run directories.
func defaultOutputRoot() string {
	if d := os.Getenv("LOOM_OUTPUT"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./workflow-output"
	}
	return filepath.Join(home, "workflow-output")
}
