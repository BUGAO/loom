// loom — a plan-then-execute agent workflow orchestrator.
//
//	loom                 run the server in the foreground
//	loom start           run the server in the background (logs: <data>/loom.log)
//	loom stop            stop the background server (SIGTERM, then SIGKILL)
//	loom restart         stop if running, then start in the background
//	loom status          report whether a server is running and where
//
// Flags (-addr, -data, …) go after the command and apply to both the control
// action (which server to find) and the server being started.
//
// Backends:
//
//	acp    — executor nodes run over the Agent Client Protocol; each agent gets
//	         its own home (AGENTS.md, private workspace, private skills)
//	claude — Claude Code CLI headless (single-shot per node)
//	mock   — zero-cost demo
//
// All available backends are always registered; -backend picks the default for
// new runs. Planning always uses the Claude CLI (mock plans for itself).
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"loom/internal/engine"
	"loom/internal/hub"
	"loom/internal/llm"
	"loom/internal/seed"
	"loom/internal/server"
	"loom/internal/store"
)

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
// a repo-local store pass -data ./data explicitly.
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

func main() {
	addr := flag.String("addr", ":7333", "listen address (all interfaces by default; use 127.0.0.1:7333 for loopback only)")
	dataDir := flag.String("data", defaultDataDir(), "data directory (env: LOOM_DATA)")
	outputRoot := flag.String("output", defaultOutputRoot(), "deliverable folder root for dynamic runs (env: LOOM_OUTPUT)")
	dryDefault := flag.Bool("dry-run", false, "pre-check the dry-run switch for new runs (zero-cost demo mode)")
	backendFlag := flag.String("backend", "", "deprecated: 'mock' means -dry-run; other values are ignored (how an agent runs is the agent's runtime field)")
	acpCmd := flag.String("acp-cmd", "", "ACP agent command (default: autodetect claude-code-acp; env: LOOM_ACP_CMD)")

	// An optional control command precedes the flags: `loom restart -addr :8080`.
	args := os.Args[1:]
	command := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	flag.CommandLine.Parse(args)

	switch command {
	case "":
		// fall through to the foreground server below
	case "start", "stop", "restart", "status":
		if err := control(command, *addr, *dataDir, args); err != nil {
			log.Fatal(err)
		}
		return
	default:
		log.Fatalf("unknown command %q — use start, stop, restart, status, or no command to run in the foreground", command)
	}

	if *backendFlag == "mock" {
		*dryDefault = true
	} else if *backendFlag != "" && *backendFlag != "acp" {
		log.Printf("note: -backend is deprecated; execution runtime is per-agent now, dry-run is per-run")
	}

	st, err := store.New(*dataDir)
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
	if cmd := resolveACP(*acpCmd); cmd != "" {
		backends["claude"] = &llm.ACP{Command: cmd}
		fmt.Printf("claude runtime: ACP via %s\n", cmd)
	} else {
		log.Printf("warning: claude-code-acp not found (install: npm install --prefix .acp @zed-industries/claude-code-acp); claude runtime degrades to CLI single-shot — dynamic workflows will refuse to start")
		backends["claude"] = &llm.Claude{}
	}

	broker := engine.NewBroker()
	orchestration := hub.New(baseURL(*addr), st.ListAgents)
	eng := engine.New(st, backends, broker, orchestration)
	eng.SetOutputRoot(*outputRoot)
	eng.RecoverInterrupted()

	srv := server.New(st, eng, broker, orchestration.Handler(), *dryDefault)
	mode := "real"
	if *dryDefault {
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

	fmt.Printf("loom listening on %s (data: %s, %s)\n", *addr, st.Dir(), mode)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}

// ---- control commands: start / stop / restart / status ----

func pidPath(dataDir string) string { return filepath.Join(dataDir, "loom.pid") }

// findServer locates a running loom for this data dir / addr: the pidfile
// first, then the port (covers servers started before pidfiles existed, or
// after a crash that left no file).
func findServer(dataDir, addr string) (pid int, ok bool) {
	if b, err := os.ReadFile(pidPath(dataDir)); err == nil {
		if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && processAlive(p) {
			return p, true
		}
		os.Remove(pidPath(dataDir)) // stale
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	out, err := exec.Command("lsof", "-tnP", "-iTCP:"+port, "-sTCP:LISTEN").Output()
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, false
	}
	p, err := strconv.Atoi(fields[0])
	if err != nil || !processAlive(p) {
		return 0, false
	}
	// Refuse to manage a process that is not loom — the port could be held
	// by anything, and `loom stop` must never kill an innocent bystander.
	comm, _ := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(p)).Output()
	if !strings.Contains(filepath.Base(strings.TrimSpace(string(comm))), "loom") {
		return 0, false
	}
	return p, true
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func control(command, addr, dataDir string, serverArgs []string) error {
	switch command {
	case "status":
		if pid, ok := findServer(dataDir, addr); ok {
			fmt.Printf("loom is running: pid %d, addr %s, data %s\n", pid, addr, dataDir)
		} else {
			fmt.Printf("loom is not running (addr %s, data %s)\n", addr, dataDir)
		}
		return nil
	case "stop":
		return stopServer(dataDir, addr)
	case "start":
		if pid, ok := findServer(dataDir, addr); ok {
			return fmt.Errorf("loom is already running (pid %d); use restart", pid)
		}
		return startServer(dataDir, addr, serverArgs)
	case "restart":
		if err := stopServer(dataDir, addr); err != nil {
			fmt.Println(err) // "not running" is not a reason to refuse the start
		}
		return startServer(dataDir, addr, serverArgs)
	}
	return nil
}

// stopServer sends SIGTERM and waits; a server that ignores it for 10s gets
// SIGKILL. Either way, interrupted dynamic runs are recovered on next boot.
func stopServer(dataDir, addr string) error {
	pid, ok := findServer(dataDir, addr)
	if !ok {
		return fmt.Errorf("loom is not running (addr %s, data %s)", addr, dataDir)
	}
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 100; i++ {
		if !processAlive(pid) {
			fmt.Printf("stopped loom (pid %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	fmt.Printf("stopped loom (pid %d, forced)\n", pid)
	os.Remove(pidPath(dataDir))
	return nil
}

// startServer re-execs this binary detached, in its own session, with output
// appended to <data>/loom.log, then waits for the API to come up. The child
// inherits the environment, so launcher-provided defaults (LOOM_DATA,
// LOOM_ACP_CMD) carry over.
func startServer(dataDir, addr string, serverArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(dataDir, "loom.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()

	// The resolved addr and data dir are passed explicitly so the child can
	// never re-derive them from ITS environment or cwd; the caller's explicit
	// flags come last and win if they duplicate these.
	args := append([]string{"-addr", addr, "-data", dataDir}, serverArgs...)
	cmd := exec.Command(exe, args...)
	// Anchor the daemon's cwd to the data dir: cwd-relative lookups (the
	// ./.acp adapter probe) must not depend on where `loom restart` was typed.
	cmd.Dir = dataDir
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive this shell
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap if it dies while we probe

	probe := baseURL(addr) + "/api/meta"
	client := &http.Client{Timeout: time.Second}
	for i := 0; i < 100; i++ {
		if resp, err := client.Get(probe); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				fmt.Printf("loom started: pid %d, %s (log: %s)\n", pid, baseURL(addr), logPath)
				return nil
			}
		}
		if !processAlive(pid) {
			tail, _ := exec.Command("tail", "-5", logPath).Output()
			return fmt.Errorf("loom exited during startup; last log lines:\n%s", tail)
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("loom (pid %d) did not answer on %s within 15s; check %s", pid, probe, logPath)
}
