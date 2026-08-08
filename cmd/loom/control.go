package main

// Process management, built into the binary: pidfile + port discovery, a
// graceful stop, and a detached start with readiness probing. loom is a
// per-user single binary — its process management should not require the
// user to remember lsof/nohup incantations or to set up launchd.

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "run the server in the background",
		Long: `Start the loom server as a background process, detached from this
terminal (its own session; it survives the shell closing).

Output is appended to <data>/loom.log. The command waits for the HTTP API
to answer before reporting success, so "started" means "serving" — if the
server dies during startup, the last log lines are shown instead.

Refuses to start when a server for this address is already running; use
"loom restart" to replace it.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if pid, ok := findServer(flagData, flagAddr); ok {
				return fmt.Errorf("loom is already running (pid %d); use restart", pid)
			}
			return startServer()
		},
	}
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "stop the background server",
		Long: `Stop the running loom server: SIGTERM first, and SIGKILL only if it has
not exited after 10 seconds.

Any dynamic run in flight is marked interrupted and can be resumed from its
task ledger after the next start (completed tasks are preserved; in-flight
tasks are failed as rework-eligible).

The server is located by its pidfile (<data>/loom.pid), falling back to
whatever loom process is listening on --addr. Processes that are not loom
are never touched.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return stopServer() },
	}
}

func restartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "stop the server if running, then start it in the background",
		Long: `Stop the running server (if any), then start a fresh one in the
background. The typical upgrade flow after changing code:

  go build -o ~/.local/bin/loom ./cmd/loom && loom restart

or, to also refresh dependencies and seed data:

  ./scripts/install.sh && loom restart`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := stopServer(); err != nil {
				fmt.Println(err) // "not running" is not a reason to refuse the start
			}
			return startServer()
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "report whether a server is running and where",
		Long: `Report whether a loom server is running for this address/data dir, and
print its pid. Exit code 0 when running, 1 when not — usable in scripts:

  loom status && echo up || echo down`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if pid, ok := findServer(flagData, flagAddr); ok {
				fmt.Printf("loom is running: pid %d, addr %s, data %s\n", pid, flagAddr, flagData)
				return nil
			}
			return fmt.Errorf("loom is not running (addr %s, data %s)", flagAddr, flagData)
		},
	}
}

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

// stopServer sends SIGTERM and waits; a server that ignores it for 10s gets
// SIGKILL. Either way, interrupted dynamic runs are recovered on next boot.
func stopServer() error {
	pid, ok := findServer(flagData, flagAddr)
	if !ok {
		return fmt.Errorf("loom is not running (addr %s, data %s)", flagAddr, flagData)
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
	os.Remove(pidPath(flagData))
	return nil
}

// startServer re-execs this binary detached, in its own session, with output
// appended to <data>/loom.log, then waits for the API to come up. Every
// relevant flag is passed explicitly with its resolved value, so the child
// can never re-derive a different answer from ITS environment or cwd.
func startServer() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(flagData, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(flagData, "loom.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()

	args := []string{"--addr", flagAddr, "--data", flagData, "--output", flagOutput}
	if flagDry {
		args = append(args, "--dry-run")
	}
	if flagACP != "" {
		args = append(args, "--acp-cmd", flagACP)
	}
	cmd := exec.Command(exe, args...)
	// Anchor the daemon's cwd to the data dir: cwd-relative lookups (the
	// ./.acp adapter probe) must not depend on where `loom start` was typed.
	cmd.Dir = flagData
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive this shell
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap if it dies while we probe

	probe := baseURL(flagAddr) + "/api/meta"
	client := &http.Client{Timeout: time.Second}
	for i := 0; i < 100; i++ {
		if resp, err := client.Get(probe); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				fmt.Printf("loom started: pid %d, %s (log: %s)\n", pid, baseURL(flagAddr), logPath)
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
