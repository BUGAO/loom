package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"loom/internal/model"
)

// ACP executes agents over the Agent Client Protocol via coder/acp-go-sdk:
// one agent process + session, cwd = the agent's own home workspace, so
// AGENTS.md and the agent's private .claude/skills are loaded natively by the
// runtime. Tool restriction is enforced by answering permission requests per
// the agent's allowed-tool list. Model selection rides on ANTHROPIC_MODEL.
//
// Sessions are the primitive: Open keeps the process and session alive for as
// many turns as the caller needs (dynamic mode), and Complete is the one-turn
// wrapper the static engine and planner use.
type ACP struct {
	Command string   // path to an ACP agent binary (e.g. claude-code-acp)
	Args    []string // extra args for the agent process
	// ProtectDir is loom's own data directory. Sessions get path-scoped deny
	// rules shielding its control surfaces (agent definitions and homes'
	// AGENTS.md/skills, workflow files, run ledgers) from their file tools —
	// see pathDenyFor. Empty disables the path rules (tests, ad-hoc use).
	ProtectDir string
}

func (a *ACP) Name() string { return "acp" }

var _ SessionBackend = (*ACP)(nil)

// allowedFn maps ACP tool-call kinds/titles onto the agent's tool list.
func allowedFn(tools string) func(title, kind string) bool {
	set := map[string]bool{}
	for _, t := range strings.Split(tools, ",") {
		if t = strings.TrimSpace(t); t != "" {
			set[strings.ToLower(t)] = true
		}
	}
	return func(title, kind string) bool {
		switch kind {
		case "read", "search":
			return set["read"] || set["grep"] || set["glob"]
		case "edit", "delete", "move":
			return set["write"] || set["edit"]
		case "execute":
			return set["bash"]
		case "fetch":
			return set["webfetch"] || set["websearch"]
		default:
			// Unknown kind: fall back to matching the title against the list.
			lt := strings.ToLower(title)
			for name := range set {
				if strings.Contains(lt, name) {
					return true
				}
			}
			return false
		}
	}
}

// ---- tool jail ----
//
// Answering permission requests is not enough: Claude Code never asks
// permission for read-only tools or for Task (its own subagent spawner), so an
// allowlist enforced only at the request_permission surface leaves a session
// free to explore the filesystem and orchestrate outside the ledger. The jail
// closes that hole with the runtime's own permissions.deny rules, written to
// the session cwd before spawn — enforced by Claude Code core, immune to
// prompting.
//
// Two layers, two homes:
//   - an AGENT HOME (under loom's data dir) is loom's own: the jail file
//     there is rewritten wholesale on every open with the full deny list for
//     that agent's allowlist;
//   - a USER WORKSPACE (anywhere else) is not loom's: the jail MERGES into
//     whatever settings.local.json the user has, adds only loom's path rules
//     and hook entries, and removes them again when the last loom session in
//     that cwd closes. Tool-level restriction there is the hook gate's job,
//     which judges per identity — so two sessions with different allowlists
//     can share the workspace without inheriting each other's jail.
//
// The hook entries are static (the loom binary + "gate"); the per-session
// credential travels in the spawned process environment (LOOM_GATE_*), which
// hook commands inherit. That is what keeps concurrent sessions in one cwd
// from clobbering each other's identity.

// denyListFor computes the deny rules implied by an agent's tool allowlist.
func denyListFor(tools string) []string {
	keep := model.GrantedTools(tools)
	var deny []string
	for _, name := range model.CapabilityTools {
		if !keep[name] {
			deny = append(deny, name)
		}
	}
	return deny
}

// pathDenyFor computes path-scoped deny rules shielding loom's own state from
// a session's file tools. The boundary is drawn per-path, not per-tree: agent
// homes live under the data dir and legitimately serve as scratch space, so
// only the control surfaces are denied — the files that would let a session
// rewrite an agent's identity (agent.md, AGENTS.md, private skills), a
// workflow's configuration, a run's ledger, or its own jail. `//` prefixes an
// absolute filesystem path in Claude Code's rule syntax. Bash is deliberately
// absent: permission rules cannot see into a shell command line, so for a
// Bash-granted agent the path rules stop the structured tools' casual
// overreach while the shell remains a trust boundary.
func pathDenyFor(protectDir string) []string {
	pats := []string{"**/.claude/settings.local.json"}
	if protectDir != "" {
		if abs, err := filepath.Abs(protectDir); err == nil {
			protectDir = abs
		}
		root := "//" + strings.TrimPrefix(protectDir, "/")
		pats = append(pats,
			root+"/workflows/**",
			root+"/agents/*/agent.md",
			root+"/agents/*/home/.claude/**",
			root+"/runs/*/run.json",
			// AGENTS.md / CLAUDE.md anywhere under the data dir are loaded as
			// instructions by some future session — planting one is injection.
			root+"/**/AGENTS.md",
			root+"/**/CLAUDE.md",
		)
	}
	rules := make([]string, 0, len(model.WriteTools)*len(pats))
	for _, tool := range model.WriteTools {
		for _, p := range pats {
			rules = append(rules, tool+"("+p+")")
		}
	}
	return rules
}

// gateExe overrides the binary the hook runs (tests build a real loom binary;
// the test process itself is not one).
var gateExe string

// gateHookCommand is the hook command line: this very binary, subcommand
// gate. Single-quoted so a path with spaces survives the shell.
func gateHookCommand() string {
	exe := gateExe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			exe = "loom"
		}
	}
	return "'" + strings.ReplaceAll(exe, "'", `'\''`) + "' gate"
}

// isGateHookEntry recognizes a hook entry loom wrote (any loom binary).
func isGateHookEntry(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	hooks, _ := m["hooks"].([]any)
	for _, h := range hooks {
		hm, _ := h.(map[string]any)
		cmd, _ := hm["command"].(string)
		if strings.HasSuffix(cmd, "' gate") || strings.HasSuffix(cmd, "loom gate") {
			return true
		}
	}
	return false
}

func gateHookEntries() map[string][]any {
	cmd := gateHookCommand()
	entry := func(matcher string) any {
		return map[string]any{
			"matcher": matcher,
			"hooks":   []any{map[string]any{"type": "command", "command": cmd, "timeout": 15}},
		}
	}
	return map[string][]any{
		"PreToolUse":  {entry("^(" + strings.Join(model.CapabilityTools, "|") + ")$")},
		"PostToolUse": {entry("^(" + strings.Join(append(append([]string{}, model.WriteTools...), "Bash"), "|") + ")$")},
	}
}

// jailRef tracks how many live loom sessions share one cwd's jail and what
// loom created there, so the last one out can clean up a user workspace.
type jailRef struct {
	n           int
	createdDir  bool
	createdFile bool
	added       []string // deny entries loom put into a user workspace's file
}

var (
	jailMu   sync.Mutex
	jailRefs = map[string]*jailRef{}
)

func jailPath(workDir string) string {
	return filepath.Join(workDir, ".claude", "settings.local.json")
}

// underDir reports whether p is inside dir (both cleaned; symlink-agnostic).
func underDir(p, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// writeToolJail materializes the jail into the session cwd's
// .claude/settings.local.json and registers the session against it. hooked
// says whether the session carries a gate credential: only then are hook
// entries written, and only then may a user workspace skip the tool-level
// deny list (the gate enforces it per identity). Without a gate every cwd
// gets the full list — the pre-gate behavior, never weaker.
func writeToolJail(workDir, tools, protectDir string, hooked bool) error {
	jailMu.Lock()
	defer jailMu.Unlock()
	home := underDir(workDir, protectDir)
	dir := filepath.Dir(jailPath(workDir))
	ref := jailRefs[workDir]
	if ref == nil {
		ref = &jailRef{}
		jailRefs[workDir] = ref
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		ref.createdDir = true
	}
	settings := map[string]any{}
	raw, err := os.ReadFile(jailPath(workDir))
	switch {
	case os.IsNotExist(err):
		ref.createdFile = true
	case err != nil:
		return err
	case home:
		// loom's own file: rewritten wholesale.
	default:
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("%s is not valid JSON; refusing to merge loom's jail into it: %w", jailPath(workDir), err)
		}
	}
	var deny []string
	if home || !hooked {
		deny = append(deny, denyListFor(tools)...)
	}
	deny = append(deny, pathDenyFor(protectDir)...)
	// Sessions sharing a workspace may differ in allowlist: the union of what
	// loom has put there is what gets replaced, so a stricter earlier entry
	// never survives to jail a later, wider session — and vice versa the
	// gate, not this file, is what keeps the narrower one narrow.
	mergeJail(settings, ref.added, deny, hooked)
	ref.added = deny
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(jailPath(workDir), data, 0o644); err != nil {
		return err
	}
	ref.n++
	return nil
}

// mergeJail edits a settings object in place: the deny entries loom added
// earlier (remove) go, the current ones (add) come, gate hook entries are
// replaced by the current set (none when !hooked). User entries — including
// a user's own bare "WebFetch" deny — are left alone, which is why removal
// is by exact bookkeeping, not by pattern.
func mergeJail(settings map[string]any, remove, add []string, hooked bool) {
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	drop := map[string]bool{}
	for _, d := range remove {
		drop[d] = true
	}
	var kept []any
	if old, _ := perms["deny"].([]any); old != nil {
		for _, d := range old {
			if ds, ok := d.(string); ok && drop[ds] {
				continue
			}
			kept = append(kept, d)
		}
	}
	for _, d := range add {
		kept = append(kept, d)
	}
	if len(kept) > 0 {
		perms["deny"] = kept
		settings["permissions"] = perms
	} else {
		delete(perms, "deny")
		if len(perms) == 0 {
			delete(settings, "permissions")
		} else {
			settings["permissions"] = perms
		}
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	ours := map[string][]any{}
	if hooked {
		ours = gateHookEntries()
	}
	for _, ev := range []string{"PreToolUse", "PostToolUse"} {
		var keptH []any
		if old, _ := hooks[ev].([]any); old != nil {
			for _, e := range old {
				if !isGateHookEntry(e) {
					keptH = append(keptH, e)
				}
			}
		}
		keptH = append(keptH, ours[ev]...)
		if len(keptH) > 0 {
			hooks[ev] = keptH
		} else {
			delete(hooks, ev)
		}
	}
	if len(hooks) > 0 {
		settings["hooks"] = hooks
	} else {
		delete(settings, "hooks")
	}
}

// releaseToolJail is the session-close counterpart: the last loom session in
// a USER workspace removes loom's entries (and the file / .claude dir if loom
// created them). Agent homes keep their jail — loom owns them.
func releaseToolJail(workDir, protectDir string) {
	jailMu.Lock()
	defer jailMu.Unlock()
	ref := jailRefs[workDir]
	if ref == nil {
		return
	}
	ref.n--
	if ref.n > 0 {
		return
	}
	delete(jailRefs, workDir)
	if underDir(workDir, protectDir) {
		return
	}
	path := jailPath(workDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	settings := map[string]any{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return
	}
	mergeJail(settings, ref.added, nil, false)
	if len(settings) == 0 && ref.createdFile {
		os.Remove(path)
		if ref.createdDir {
			os.Remove(filepath.Dir(path)) // only succeeds if empty
		}
		return
	}
	if data, err := json.MarshalIndent(settings, "", "  "); err == nil {
		os.WriteFile(path, data, 0o644)
	}
}

// acpClient implements acp.Client for one session: it streams session updates
// into the collectors, answers permission requests by policy, and hosts the
// session's terminals (the adapter runs Bash through them). fs methods are
// never expected — that capability is advertised off.
type acpClient struct {
	allow func(title, kind string) bool

	mu         sync.Mutex
	onMessage  func(text string)
	onThought  func(text string)
	onToolCall func(title, kind string)

	// terminals hosts the session's shell processes (see terminal support).
	terminals   map[string]*termProc
	termSeq     int
	termsClosed bool
}

var _ acp.Client = (*acpClient)(nil)

// RequestPermission answers every permission prompt with "allow".
//
// This is a deliberate retreat from answering by allowlist. Permission
// prompts are designed for a human at a keyboard, and their rejection
// semantics are poison for unattended sessions: Claude Code renders a
// rejection as "the user doesn't want to proceed, STOP and wait" — after
// which the model obediently ends its turn with no envelope and the task
// dies. Worse, the adapter sends these prompts without a tool kind, so any
// answering policy here is guesswork over display titles (one incarnation
// approved `echo loom` as a hub tool and refused `npm install`).
//
// Enforcement does not live here and never did — it lives in the layers that
// fail LOUDLY as tool errors the model can react to:
//   - the tool jail: ungranted built-ins are denied by Claude Code core via
//     settings.local.json (written before every session; open fails if it
//     cannot be written);
//   - CreateTerminal below: shell for an agent without Bash is refused with
//     an explanatory error, whatever this responder says.
func (c *acpClient) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	var optionID acp.PermissionOptionId
	for _, want := range []acp.PermissionOptionKind{
		acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways,
	} {
		for _, o := range p.Options {
			if o.Kind == want {
				optionID = o.OptionId
				break
			}
		}
		if optionID != "" {
			break
		}
	}
	if optionID == "" && len(p.Options) > 0 {
		optionID = p.Options[0].OptionId
	}
	if optionID == "" {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: optionID}}}, nil
}

func (c *acpClient) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	onMessage, onThought, onToolCall := c.onMessage, c.onThought, c.onToolCall
	c.mu.Unlock()
	if onMessage == nil {
		return nil // between turns: nothing is collecting
	}
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if t := u.AgentMessageChunk.Content.Text; t != nil {
			onMessage(t.Text)
		}
	case u.AgentThoughtChunk != nil:
		if t := u.AgentThoughtChunk.Content.Text; t != nil {
			onThought(t.Text)
		}
	case u.ToolCall != nil:
		onToolCall(u.ToolCall.Title, string(u.ToolCall.Kind))
	}
	return nil
}

func (c *acpClient) setCollectors(msg, thought func(string), tool func(string, string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessage, c.onThought, c.onToolCall = msg, thought, tool
}

func (c *acpClient) ReadTextFile(ctx context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, fmt.Errorf("fs.readTextFile capability not advertised")
}

func (c *acpClient) WriteTextFile(ctx context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, fmt.Errorf("fs.writeTextFile capability not advertised")
}

// ---- terminal support ----
//
// claude-code-acp executes Bash through the ACP terminal methods, so a client
// without them silently kills every shell-using agent: the tool call dies,
// the turn collapses, and the missing envelope fails the task. The client
// therefore implements a real terminal: one process group per terminal id,
// output in a bounded buffer, exit status reported honestly.

// termProc is one terminal-hosted process group.
type termProc struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	pgid      int // the shell's process group; descendants live and die with it
	buf       bytes.Buffer
	limit     int
	truncated bool
	done      chan struct{}
	exit      *acp.TerminalExitStatus
}

// Write implements io.Writer, retaining at most limit bytes (newest kept),
// which is the truncation direction the protocol specifies.
func (t *termProc) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.limit > 0 && t.buf.Len() > t.limit {
		over := t.buf.Len() - t.limit
		b := t.buf.Bytes()[over:]
		// Keep the cut on a UTF-8 boundary so the output stays a valid string.
		for len(b) > 0 && b[0]&0xC0 == 0x80 {
			b = b[1:]
		}
		trimmed := append([]byte(nil), b...)
		t.buf.Reset()
		t.buf.Write(trimmed)
		t.truncated = true
	}
	return len(p), nil
}

func (t *termProc) snapshot() (string, bool, *acp.TerminalExitStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String(), t.truncated, t.exit
}

// signalGroup signals the terminal's entire process group, not just the shell.
// The leader having exited says nothing about the group: `server &` leaves the
// backgrounded server in the group after sh exits, and that is exactly the
// process that must not outlive the session (a leftover from one run squatted
// on its port and derailed the next run's verification for a full task
// timeout). ESRCH just means the group is already empty.
func (t *termProc) signalGroup(sig syscall.Signal) {
	if t.pgid > 0 {
		syscall.Kill(-t.pgid, sig)
	}
}

// groupAlive reports whether any process in the terminal's group survives.
func (t *termProc) groupAlive() bool {
	return t.pgid > 0 && syscall.Kill(-t.pgid, 0) == nil
}

func (c *acpClient) terminal(id string) *termProc {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminals[id]
}

// defaultTermOutputLimit caps a terminal's retained output when the agent
// does not ask for a limit: the field is optional in the protocol, and an
// unbounded buffer would let one chatty command grow loom's memory forever.
const defaultTermOutputLimit = 1 << 20 // 1 MiB

func (c *acpClient) CreateTerminal(ctx context.Context, p acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	// Second line of defense behind the settings jail: a session whose
	// allowlist grants no shell gets no terminal, full stop.
	if !c.allow("Terminal", "execute") {
		return acp.CreateTerminalResponse{}, fmt.Errorf("terminal refused: this agent's tool allowlist does not include Bash")
	}
	c.mu.Lock()
	if c.termsClosed {
		c.mu.Unlock()
		return acp.CreateTerminalResponse{}, fmt.Errorf("session is closing; no new terminals")
	}
	c.mu.Unlock()
	t := &termProc{done: make(chan struct{}), limit: defaultTermOutputLimit}
	if p.OutputByteLimit != nil && *p.OutputByteLimit > 0 {
		t.limit = *p.OutputByteLimit
	}
	// claude-code-acp sends the ENTIRE shell command line as a single string
	// in Command, with no Args — `cd x && npm install | tail -20` arrives as
	// one token. Executing that as a bare argv[0] means looking for a binary
	// literally named "cd x && npm install | tail -20": ENOENT on every
	// non-trivial command, which is how a whole run's Bash silently died.
	// A terminal's contract is shell semantics, so give it a shell.
	var cmd *exec.Cmd
	if len(p.Args) > 0 {
		cmd = exec.Command(p.Command, p.Args...)
	} else {
		cmd = exec.Command("/bin/sh", "-c", p.Command)
	}
	if p.Cwd != nil && *p.Cwd != "" {
		cmd.Dir = *p.Cwd
	}
	if len(p.Env) > 0 {
		env := os.Environ()
		for _, ev := range p.Env {
			env = append(env, ev.Name+"="+ev.Value)
		}
		cmd.Env = env
	}
	cmd.Stdout = t
	cmd.Stderr = t
	// Each terminal is its own process group so cleanup can reach descendants.
	// A non-interactive sh has no job control: even `&`-backgrounded children
	// stay in this group, where a kill(-pgid) still finds them after sh exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A backgrounded child inherits the output pipe; without a bound, Wait
	// blocks until that child exits too, and `server &` hangs the tool call
	// until the task times out. Once the shell itself has exited, give the
	// pipe a short drain and report the exit.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return acp.CreateTerminalResponse{}, fmt.Errorf("start %s: %w", p.Command, err)
	}
	t.cmd = cmd
	t.pgid = cmd.Process.Pid

	c.mu.Lock()
	if c.terminals == nil {
		c.terminals = map[string]*termProc{}
	}
	c.termSeq++
	id := fmt.Sprintf("term_%d_%d", cmd.Process.Pid, c.termSeq)
	c.terminals[id] = t
	c.mu.Unlock()

	go func() {
		err := cmd.Wait()
		status := &acp.TerminalExitStatus{}
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			status.ExitCode = &code
		} else if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig := ws.Signal().String() // conventional name ("killed"), not Go's error text
			status.Signal = &sig
		} else if err != nil {
			msg := err.Error()
			status.Signal = &msg
		}
		t.mu.Lock()
		t.exit = status
		t.mu.Unlock()
		close(t.done)
	}()
	return acp.CreateTerminalResponse{TerminalId: id}, nil
}

func (c *acpClient) TerminalOutput(ctx context.Context, p acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	t := c.terminal(p.TerminalId)
	if t == nil {
		return acp.TerminalOutputResponse{}, fmt.Errorf("unknown terminal %s", p.TerminalId)
	}
	out, truncated, exit := t.snapshot()
	return acp.TerminalOutputResponse{Output: out, Truncated: truncated, ExitStatus: exit}, nil
}

func (c *acpClient) WaitForTerminalExit(ctx context.Context, p acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	t := c.terminal(p.TerminalId)
	if t == nil {
		return acp.WaitForTerminalExitResponse{}, fmt.Errorf("unknown terminal %s", p.TerminalId)
	}
	select {
	case <-ctx.Done():
		return acp.WaitForTerminalExitResponse{}, ctx.Err()
	case <-t.done:
	}
	_, _, exit := t.snapshot()
	res := acp.WaitForTerminalExitResponse{}
	if exit != nil {
		res.ExitCode = exit.ExitCode
		res.Signal = exit.Signal
	}
	return res, nil
}

func (c *acpClient) KillTerminal(ctx context.Context, p acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	t := c.terminal(p.TerminalId)
	if t == nil {
		return acp.KillTerminalResponse{}, fmt.Errorf("unknown terminal %s", p.TerminalId)
	}
	t.signalGroup(syscall.SIGKILL)
	return acp.KillTerminalResponse{}, nil
}

func (c *acpClient) ReleaseTerminal(ctx context.Context, p acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	c.mu.Lock()
	t := c.terminals[p.TerminalId]
	delete(c.terminals, p.TerminalId)
	c.mu.Unlock()
	if t != nil {
		// Unconditional: the shell having exited doesn't mean its group is
		// empty — see signalGroup.
		t.signalGroup(syscall.SIGKILL)
	}
	return acp.ReleaseTerminalResponse{}, nil
}

// killTerminals reaps every process group still owned by this session; called
// on session close so nothing an agent shell started outlives its task — not
// the shells, and not their backgrounded descendants. It also latches the
// session closed, so a CreateTerminal racing the close cannot register a
// process after the reaping ran.
//
// SIGTERM first with a short grace so servers can flush (same courtesy
// `loom stop` extends to the daemon), then SIGKILL for whatever remains.
func (c *acpClient) killTerminals() {
	c.mu.Lock()
	c.termsClosed = true
	terms := c.terminals
	c.terminals = nil
	c.mu.Unlock()
	for _, t := range terms {
		t.signalGroup(syscall.SIGTERM)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, t := range terms {
			if t.groupAlive() {
				alive = true
				break
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, t := range terms {
		t.signalGroup(syscall.SIGKILL)
	}
}

// scrubEnv drops inherited Claude Code session markers: loom is a standalone
// orchestrator, and without this the adapter refuses to start when loom itself
// was launched from inside a Claude Code session (nested-session guard).
func scrubEnv(model string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
			"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT", "CLAUDE_PID":
			continue
		}
		env = append(env, kv)
	}
	if model != "" {
		env = append(env, "ANTHROPIC_MODEL="+model)
	}
	return env
}

// acpSession is one live agent process + ACP session.
type acpSession struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	conn    *acp.ClientSideConnection
	client  *acpClient
	id      acp.SessionId
	req     SessionRequest
	stderr  *bytes.Buffer
	closeMu sync.Mutex
	closed  bool
	// protectDir is the backend's data dir, needed to release the jail.
	protectDir string

	usageMu   sync.Mutex
	lastUsage modelUsage // per-model totals as of the previous turn

	// imageOK mirrors the agent's advertised promptCapabilities.image; images
	// are only sent when the runtime declared it can receive them.
	imageOK bool
}

var _ Session = (*acpSession)(nil)
var _ ImageSession = (*acpSession)(nil)

func (a *ACP) Open(ctx context.Context, req SessionRequest) (Session, error) {
	// Fail closed: without the jail on disk, the allowlist is advisory.
	if err := writeToolJail(req.WorkDir, req.Tools, a.ProtectDir, req.Gate != nil); err != nil {
		return nil, fmt.Errorf("write tool jail: %w", err)
	}
	cmd := exec.Command(a.Command, a.Args...)
	cmd.Dir = req.WorkDir
	cmd.Env = scrubEnv(req.Model)
	if req.Gate != nil {
		// The gate credential rides in the process environment: hook commands
		// inherit it, and nothing per-session lands on disk in the cwd.
		cmd.Env = append(cmd.Env, "LOOM_GATE_URL="+req.Gate.URL, "LOOM_GATE_TOKEN="+req.Gate.Token)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", a.Command, err)
	}

	s := &acpSession{cmd: cmd, stdin: stdin, stderr: stderr, req: req, protectDir: a.ProtectDir, client: &acpClient{allow: allowedFn(req.Tools)}}
	s.conn = acp.NewClientSideConnection(s.client, stdin, stdout)
	s.conn.SetLogger(slog.New(slog.DiscardHandler)) // teardown noise isn't loom's log

	initCtx, cancelInit := context.WithTimeout(ctx, 60*time.Second)
	defer cancelInit()
	init, err := s.conn.Initialize(initCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs: acp.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			// Real terminal support: the adapter runs Bash through it, and it
			// calls these methods whether or not we advertise them — so the
			// honest capability is true, backed by a real implementation.
			Terminal: true,
		},
	})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("acp initialize: %w (stderr: %s)", err, s.stderrTail())
	}
	s.imageOK = init.AgentCapabilities.PromptCapabilities.Image
	sess, err := s.conn.NewSession(initCtx, acp.NewSessionRequest{
		Cwd:        req.WorkDir,
		McpServers: mcpServers(req.MCPServers),
	})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("acp session: %w (stderr: %s)", err, s.stderrTail())
	}
	s.id = sess.SessionId
	// Unattended sessions bypass permission prompts entirely. Every prompt
	// this suppresses would be auto-approved by RequestPermission anyway —
	// except the prompt round trip is one more thing to break, and its
	// rejection path ("STOP and wait for the user") kills turns. Enforcement
	// is unaffected: the deny jail and the terminal allowlist check both hold
	// under bypass — verified live by TestLiveBypassJail, where a tool-less
	// bypassed agent could neither read nor write nor shell. When bypass is
	// unavailable (running as root without IS_SANDBOX), fall back to
	// acceptEdits + the always-allow responder.
	if _, err := s.conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
		SessionId: sess.SessionId, ModeId: acp.SessionModeId("bypassPermissions"),
	}); err != nil && s.client.allow("", "edit") {
		s.conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
			SessionId: sess.SessionId, ModeId: acp.SessionModeId("acceptEdits")})
	}
	return s, nil
}

// mcpServers renders loom's MCP endpoints as ACP HTTP transport entries. The
// adapter advertises mcpCapabilities.http, and passes name/url/headers straight
// through to the Claude Code SDK.
func mcpServers(in []MCPServer) []acp.McpServer {
	out := make([]acp.McpServer, 0, len(in))
	for _, m := range in {
		headers := make([]acp.HttpHeader, 0, len(m.Headers))
		for k, v := range m.Headers {
			headers = append(headers, acp.HttpHeader{Name: k, Value: v})
		}
		out = append(out, acp.McpServer{Http: &acp.McpServerHttpInline{
			Type: "http", Name: m.Name, Url: m.URL, Headers: headers,
		}})
	}
	return out
}

func (s *acpSession) stderrTail() string {
	str := strings.TrimSpace(s.stderr.String())
	if len(str) > 500 {
		str = "…" + str[len(str)-500:]
	}
	return str
}

func (s *acpSession) Prompt(ctx context.Context, text string) (*Result, error) {
	return s.promptBlocks(ctx, []acp.ContentBlock{acp.TextBlock(text)})
}

// PromptImages sends one turn carrying inline images. When the agent did not
// advertise image support the images are withheld and the model is told so in
// plain text — a silent drop would leave it answering about pictures it never
// saw.
func (s *acpSession) PromptImages(ctx context.Context, text string, images []Image) (*Result, error) {
	if len(images) == 0 || !s.imageOK {
		if len(images) > 0 {
			text += fmt.Sprintf("\n\n[NOTE: the user attached %d image(s), but this agent runtime does not "+
				"accept image input — ask the user to describe the content instead.]", len(images))
		}
		return s.promptBlocks(ctx, []acp.ContentBlock{acp.TextBlock(text)})
	}
	blocks := make([]acp.ContentBlock, 0, len(images)+1)
	blocks = append(blocks, acp.TextBlock(text))
	for _, img := range images {
		blocks = append(blocks, acp.ImageBlock(base64.StdEncoding.EncodeToString(img.Data), img.MimeType))
	}
	return s.promptBlocks(ctx, blocks)
}

func (s *acpSession) promptBlocks(ctx context.Context, blocks []acp.ContentBlock) (*Result, error) {
	var msgText, transcript strings.Builder
	lastThought := false
	afterTool := false // a text block resuming after a tool call starts a new paragraph
	s.client.setCollectors(
		func(t string) {
			if afterTool && msgText.Len() > 0 && !strings.HasSuffix(msgText.String(), "\n") && !strings.HasPrefix(t, "\n") {
				msgText.WriteString("\n\n")
			}
			afterTool = false
			msgText.WriteString(t)
			transcript.WriteString(t)
			lastThought = false
			if s.req.OnText != nil {
				s.req.OnText(msgText.String())
			}
		},
		func(t string) {
			if !lastThought {
				transcript.WriteString("\n> [thinking] ")
				lastThought = true
			}
			transcript.WriteString(strings.ReplaceAll(t, "\n", " "))
		},
		func(title, kind string) {
			fmt.Fprintf(&transcript, "\n[tool:%s] %s\n", kind, title)
			lastThought = false
			afterTool = true
			if s.req.OnActivity != nil && title != "" {
				s.req.OnActivity(title)
			}
		},
	)
	defer s.client.setCollectors(nil, nil, nil)

	start := time.Now()
	// Drive the prompt on an inner context so that on cancellation we can send
	// session/cancel and give the agent a grace period to wind down the turn.
	promptCtx, cancelPrompt := context.WithCancel(context.Background())
	defer cancelPrompt()
	go func() {
		select {
		case <-ctx.Done():
			c, cc := context.WithTimeout(context.Background(), 5*time.Second)
			s.conn.Cancel(c, acp.CancelNotification{SessionId: s.id})
			cc()
			select {
			case <-promptCtx.Done():
			case <-time.After(5 * time.Second):
				cancelPrompt()
			}
		case <-promptCtx.Done():
		}
	}()

	resp, err := s.conn.Prompt(promptCtx, acp.PromptRequest{
		SessionId: s.id,
		Prompt:    blocks,
	})
	res := &Result{
		Text:       msgText.String(),
		Transcript: transcript.String(),
		Model:      s.req.Model,
		StopReason: string(resp.StopReason),
		DurationMs: time.Since(start).Milliseconds(),
	}
	s.collectUsage(res)
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		// Classified: a provider overload / 5xx is transient (the engine
		// retries on this same session), everything else is a real failure.
		// Either way the message is one readable line, not a JSON-RPC
		// envelope around a provider envelope around a stderr dump.
		return res, classifyPromptError(fmt.Errorf("acp prompt: %w", err), s.stderrTail())
	}
	switch resp.StopReason {
	case acp.StopReasonEndTurn, acp.StopReasonMaxTurnRequests, "":
		return res, nil
	case acp.StopReasonCancelled:
		return res, context.Canceled
	default: // refusal, max_tokens, …
		return res, fmt.Errorf("acp prompt stopped: %s", resp.StopReason)
	}
}

// collectUsage attributes this turn's tokens by diffing the session transcript
// against the previous turn's totals. Failure is silent and lossless: Usage
// stays zero and the caller reports cost_unavailable.
func (s *acpSession) collectUsage(res *Result) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	delta, now, ok := usageDelta(string(s.id), s.lastUsage)
	if !ok {
		return
	}
	s.lastUsage = now
	usage, cost := delta.total()
	res.Usage = usage
	res.CostUSD = cost
	// Report the model the transcript actually names, which beats our own
	// request field when the runtime substituted something.
	for id := range delta {
		if id != "" {
			res.Model = id
			break
		}
	}
}

func (s *acpSession) Cancel(ctx context.Context) error {
	return s.conn.Cancel(ctx, acp.CancelNotification{SessionId: s.id})
}

func (s *acpSession) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.client.killTerminals() // no agent shell outlives its session
	s.stdin.Close()
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.cmd.Wait()
	releaseToolJail(s.req.WorkDir, s.protectDir)
	return nil
}

// Complete runs a single-turn session: the static engine's and planner's path.
func (a *ACP) Complete(ctx context.Context, req Request) (*Result, error) {
	sess, err := a.Open(ctx, SessionRequest{
		Kind:         req.Kind,
		SystemPrompt: req.SystemPrompt,
		Model:        req.Model,
		WorkDir:      req.WorkDir,
		AddDirs:      req.AddDirs,
		Tools:        req.Tools,
		MaxTurns:     req.MaxTurns,
		OnActivity:   req.OnActivity,
	})
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return sess.Prompt(ctx, req.Prompt)
}
