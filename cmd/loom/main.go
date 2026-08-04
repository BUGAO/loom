// loom — a plan-then-execute agent workflow orchestrator.
//
//	loom -addr :7333 -data ./data -backend acp
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
	"path/filepath"

	"loom/internal/engine"
	"loom/internal/hub"
	"loom/internal/llm"
	"loom/internal/seed"
	"loom/internal/server"
	"loom/internal/store"
)

// resolveACP finds the claude-code-acp binary: explicit flag/env first, then
// PATH, then the project-local install under .acp/.
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
// LOOM_DATA; then ./data. The env var matters once loom is on PATH: a relative
// default would silently create a fresh, empty store in whatever directory the
// user happened to be in, making their agents and run history look lost.
func defaultDataDir() string {
	if d := os.Getenv("LOOM_DATA"); d != "" {
		return d
	}
	return "./data"
}

func main() {
	addr := flag.String("addr", ":7333", "listen address (all interfaces by default; use 127.0.0.1:7333 for loopback only)")
	dataDir := flag.String("data", defaultDataDir(), "data directory (env: LOOM_DATA)")
	dryDefault := flag.Bool("dry-run", false, "pre-check the dry-run switch for new runs (zero-cost demo mode)")
	backendFlag := flag.String("backend", "", "deprecated: 'mock' means -dry-run; other values are ignored (how an agent runs is the agent's runtime field)")
	acpCmd := flag.String("acp-cmd", "", "ACP agent command (default: autodetect claude-code-acp; env: LOOM_ACP_CMD)")
	flag.Parse()
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
	eng.RecoverInterrupted()

	srv := server.New(st, eng, broker, orchestration.Handler(), *dryDefault)
	mode := "real"
	if *dryDefault {
		mode = "dry-run default"
	}
	fmt.Printf("loom listening on %s (data: %s, %s)\n", *addr, st.Dir(), mode)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}
