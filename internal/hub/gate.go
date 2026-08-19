package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"loom/internal/model"
)

// ---- the hook gate ----
//
// Every loom session runs with a PreToolUse/PostToolUse hook that calls back
// into the hub with the tool name and its input. The hub answers with allow
// or deny-with-reason; the reason is fed back to the model verbatim, which is
// what makes the gate a teaching device rather than a wall: a refused Write
// that says "this path is owned by task-3; delegate" gets a delegate next.
//
// What the gate enforces, in order:
//  1. the session's tool allowlist (identity-bound, so sessions sharing a cwd
//     cannot inherit each other's jail);
//  2. loom's own control surfaces (agent definitions, workflow files, run
//     ledgers, the jail itself) are never written by any session;
//  3. task scope ownership — a worker writes only inside its task's scope,
//     nobody writes inside another in-flight task's scope;
//  4. the run's collaboration level — at orchestrate the main agent's hands
//     are tied: writes into the workspace, structured or via a shell, are
//     refused and pointed at delegate.
//
// The PostToolUse leg records attribution (who wrote which path, who ran a
// shell) for the review gate. The gate is fail-open at the transport: if the
// hub cannot be reached the hook prints nothing and the call proceeds — a
// dead hub means a dead run anyway.

// GateRequest is the slice of the Claude Code hook payload the gate reads.
type GateRequest struct {
	Event string `json:"hook_event_name"` // PreToolUse | PostToolUse
	Tool  string `json:"tool_name"`
	Cwd   string `json:"cwd"`
	Input struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Command      string `json:"command"`
	} `json:"tool_input"`
}

// GateDecision is the gate's answer to one PreToolUse call.
type GateDecision struct {
	Allow  bool
	Reason string
}

var allow = GateDecision{Allow: true}

func deny(format string, args ...any) GateDecision {
	return GateDecision{Reason: fmt.Sprintf(format, args...)}
}

// ErrUnknownCredential is returned when a gate call carries no live token:
// the session's run is over (or it was never loom's). Callers treat it as
// allow — there is nothing left to protect.
var ErrUnknownCredential = errors.New("unknown credential")

// Gate answers a hook call. payload is the raw hook JSON from stdin. The
// returned object is what the hook prints to stdout — an empty object for
// allow, Claude Code's permissionDecision envelope for deny.
func (h *Hub) Gate(token string, payload []byte) (map[string]any, error) {
	rs, id, ok := h.resolve(token)
	if !ok {
		return nil, ErrUnknownCredential
	}
	var req GateRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("hook payload: %w", err)
	}
	d := rs.Gate(id, req)
	if req.Event == "PreToolUse" && !d.Allow {
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "deny",
				"permissionDecisionReason": d.Reason,
			},
		}, nil
	}
	return map[string]any{}, nil
}

// Gate judges one hook event for one identity. PreToolUse returns the policy
// decision; PostToolUse records attribution and always allows.
func (rs *RunSession) Gate(id identity, req GateRequest) GateDecision {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	switch req.Event {
	case "PreToolUse":
		return rs.gateLocked(id, req)
	case "PostToolUse":
		rs.recordWriteLocked(id, req)
	}
	return allow
}

// gatePath resolves the file a structured write targets: absolute, or
// relative to the session cwd the hook reported.
func (req GateRequest) gatePath() string {
	p := req.Input.FilePath
	if p == "" {
		p = req.Input.NotebookPath
	}
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) && req.Cwd != "" {
		p = filepath.Join(req.Cwd, p)
	}
	return filepath.Clean(p)
}

func (rs *RunSession) gateLocked(id identity, req GateRequest) GateDecision {
	// 1. allowlist
	tools, who := rs.toolsForLocked(id)
	if !model.ToolGranted(tools, req.Tool) {
		if req.Tool == "Task" {
			return deny("Task (subagents) is never available inside loom: work is delegated through the hub's delegate tool so it lands in the ledger")
		}
		granted := grantedList(tools)
		if granted == "" {
			granted = "none"
		}
		return deny("tool %s is not in %s's allowlist (granted: %s)", req.Tool, who, granted)
	}

	// 2. loom's own state
	path := req.gatePath()
	if model.IsWriteTool(req.Tool) && path != "" {
		if why := protectedPath(path, rs.cfg.ProtectDir); why != "" {
			return deny("%s is loom's own state (%s); sessions never write it", path, why)
		}
	}

	// 3/4. scope and level — workspace writes only
	switch {
	case model.IsWriteTool(req.Tool) && path != "":
		rel, inWS := rs.workspaceRel(path)
		if !inWS {
			return allow // scratch outside the workspace is the session's business
		}
		if id.role == RoleCoordinator && rs.assessPending {
			return deny("%s", rs.assessGateLocked().Error())
		}
		return rs.gateWorkspaceWriteLocked(id, rel, req.Tool)
	case req.Tool == "Bash" && req.Input.Command != "":
		if !bashWrites(req.Input.Command) {
			return allow
		}
		if id.role == RoleCoordinator && rs.assessPending {
			return deny("%s", rs.assessGateLocked().Error())
		}
		return rs.gateShellWriteLocked(id, req.Input.Command)
	}
	return allow
}

// gateWorkspaceWriteLocked applies ownership and level to one workspace-
// relative path.
func (rs *RunSession) gateWorkspaceWriteLocked(id identity, rel, tool string) GateDecision {
	ownTask := rs.ownTaskLocked(id)
	// Someone else's in-flight scope is off limits to everyone.
	if owner := rs.scopeOwnerLocked(rel, ownTask); owner != "" {
		t := rs.run.Tasks[owner]
		return deny("%s is owned by task %s (%q, %s) while it is in flight — do not write there; "+
			"wait for that task or message it (send_message) if the contract must change", rel, owner, t.Title, t.Status)
	}
	switch id.role {
	case RoleCoordinator:
		if rs.levelLocked() == model.LevelOrchestrate {
			return deny("level is ORCHESTRATE: you do not write into the workspace yourself — delegate this to a worker "+
				"(delegate) and verify afterwards (inspect, or a read-only shell command). Path: %s", rel)
		}
	case RoleWorker, RolePair:
		if ownTask != "" {
			t := rs.run.Tasks[ownTask]
			if t != nil && len(t.Scope) > 0 && !underScope(rel, t.Scope) {
				return deny("%s is outside your task's scope (%s). Stay inside it; if the task genuinely needs this path, "+
					"ask_coordinator to widen the scope before writing", rel, strings.Join(t.Scope, ", "))
			}
		}
	}
	return allow
}

// gateShellWriteLocked judges a shell command that looks like it writes.
// Shells are a trust boundary for workers (the command line is opaque to
// path rules); for the main agent at orchestrate they are the obvious way
// around a refused Write, so the obvious patterns are refused too.
func (rs *RunSession) gateShellWriteLocked(id identity, cmd string) GateDecision {
	if id.role == RoleCoordinator && rs.levelLocked() == model.LevelOrchestrate {
		return deny("level is ORCHESTRATE: this shell command writes files (%s) — you do not modify the workspace yourself. "+
			"Delegate the change; use the shell only to verify (build, test, inspect)", firstWords(cmd, 8))
	}
	return allow
}

// recordWriteLocked is the PostToolUse leg: attribution for the review gate.
func (rs *RunSession) recordWriteLocked(id identity, req GateRequest) {
	by := rs.writerLocked(id)
	var rec model.WriteRecord
	switch {
	case model.IsWriteTool(req.Tool):
		path := req.gatePath()
		if path == "" {
			return
		}
		if rel, inWS := rs.workspaceRel(path); inWS {
			path = rel
		} else {
			return // scratch outside the workspace is not a change of the work
		}
		rec = model.WriteRecord{Ts: time.Now(), By: by, Tool: req.Tool, Path: path}
	case req.Tool == "Bash":
		if !bashWrites(req.Input.Command) {
			return
		}
		rec = model.WriteRecord{Ts: time.Now(), By: by, Tool: "Bash", Command: truncate(req.Input.Command, 160)}
	default:
		return
	}
	rs.run.Writes = append(rs.run.Writes, rec)
	if n := len(rs.run.Writes); n > maxWriteRecords {
		drop := n - maxWriteRecords
		rs.run.Writes = append([]model.WriteRecord(nil), rs.run.Writes[drop:]...)
		if rs.writesAtAssess -= drop; rs.writesAtAssess < 0 {
			rs.writesAtAssess = 0
		}
	}
	rs.noteReassessLocked()
	rs.persistLocked()
}

const maxWriteRecords = 400

// persistLocked saves and publishes the run without counting a ledger
// transition: attribution and level are state, not work the coordinator's
// round loop should wake for.
func (rs *RunSession) persistLocked() {
	if rs.cfg.OnChange != nil {
		rs.cfg.OnChange(rs.run)
	}
}

// ---- identity helpers ----

// toolsForLocked is the allowlist an identity's session was opened with, and
// a name for it in refusals.
func (rs *RunSession) toolsForLocked(id identity) (tools, who string) {
	switch id.role {
	case RoleCoordinator:
		return rs.cfg.PilotTools, "the main agent"
	case RoleWorker:
		if t := rs.run.Tasks[id.taskID]; t != nil {
			if a := rs.pool[t.Agent]; a != nil {
				return a.Tools, "agent " + a.Name
			}
		}
		return "", "this worker"
	case RolePair:
		if a := rs.pool[id.agent]; a != nil {
			return a.Tools, "agent " + a.Name
		}
		return "", "the resident session"
	}
	return "", "this session"
}

// ownTaskLocked is the task an identity acts for: its own for a worker, the
// currently bound one for the pair session, none for the main agent.
func (rs *RunSession) ownTaskLocked(id identity) string {
	switch id.role {
	case RoleWorker:
		return id.taskID
	case RolePair:
		return rs.pairTasks[id.agent]
	}
	return ""
}

// writerLocked names an identity in the attribution record.
func (rs *RunSession) writerLocked(id identity) string {
	switch id.role {
	case RoleWorker:
		return id.taskID
	case RolePair:
		if t := rs.pairTasks[id.agent]; t != "" {
			return t
		}
		return RolePair + ":" + id.agent
	}
	return RoleCoordinator
}

// scopeOwnerLocked returns the in-flight task (other than self) whose scope
// covers rel, or "".
func (rs *RunSession) scopeOwnerLocked(rel, self string) string {
	ids := make([]string, 0, len(rs.run.Tasks))
	for id := range rs.run.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := rs.run.Tasks[id]
		if id == self || settled(t.Status) || len(t.Scope) == 0 {
			continue
		}
		if underScope(rel, t.Scope) {
			return id
		}
	}
	return ""
}

// workspaceRel maps an absolute path into the workspace: ("sub/path", true)
// when inside, ("", false) otherwise. Symlinks are resolved on both sides so
// /var vs /private/var does not defeat the check on macOS.
func (rs *RunSession) workspaceRel(abs string) (string, bool) {
	ws := rs.workspace
	if ws == "" {
		return "", false
	}
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	// The file itself may not exist yet; resolve its deepest existing parent.
	target := resolveExisting(abs)
	rel, err := filepath.Rel(ws, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	if rel == "." {
		rel = ""
	}
	return rel, true
}

// resolveExisting resolves symlinks on the longest existing prefix of p and
// re-attaches the rest unchanged.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	var tail []string
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				r = filepath.Join(r, tail[i])
			}
			return r
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// underScope reports whether a workspace-relative path is covered by a scope
// entry: an exact file, or a directory prefix.
func underScope(rel string, scope []string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	for _, s := range scope {
		s = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(s)), "/")
		if s == "." || s == "" {
			return true // whole workspace
		}
		if rel == s || strings.HasPrefix(rel, s+"/") {
			return true
		}
	}
	return false
}

// NormalizeScope cleans a scope list at delegation: slashes, no leading "./",
// no absolute paths (they are re-rooted at the workspace when possible),
// duplicates dropped. Empty in → nil out.
func NormalizeScope(scope []string, workspace string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range scope {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if filepath.IsAbs(s) && workspace != "" {
			if rel, err := filepath.Rel(workspace, s); err == nil && !strings.HasPrefix(rel, "..") {
				s = rel
			}
		}
		s = filepath.ToSlash(filepath.Clean(s))
		s = strings.TrimPrefix(s, "./")
		if s == "." || s == "" {
			return []string{"."} // whole workspace — subsumes everything else
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// protectedPath names the control surface a path belongs to, or "".
func protectedPath(abs, protectDir string) string {
	abs = filepath.ToSlash(resolveExisting(abs))
	if filepath.Base(abs) == "settings.local.json" && filepath.Base(filepath.Dir(abs)) == ".claude" {
		return "a session's tool jail"
	}
	if protectDir == "" {
		return ""
	}
	root := filepath.ToSlash(resolveExisting(protectDir))
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	parts := strings.Split(rel, "/")
	base := parts[len(parts)-1]
	switch {
	case parts[0] == "workflows":
		return "a workflow definition"
	case parts[0] == "agents" && len(parts) == 3 && base == "agent.md":
		return "an agent definition"
	case parts[0] == "agents" && len(parts) >= 4 && parts[2] == "home" && parts[3] == ".claude":
		return "an agent's private instructions"
	case parts[0] == "runs" && len(parts) == 3 && base == "run.json":
		return "a run ledger"
	case base == "AGENTS.md" || base == "CLAUDE.md":
		return "an instruction file under the data dir"
	}
	return ""
}

// bashWrites is the shell heuristic: does this command line obviously modify
// files? It errs toward "no" — a verification command must never be refused
// — and catches the ways a model reaches for when Write is refused:
// redirection into a file, tee, in-place sed/perl, rm/mv/cp/mkdir/touch/ln,
// mutating git subcommands, and package managers' install/add.
func bashWrites(cmd string) bool {
	return bashWriteRe.MatchString(cmd) || redirectsToFile(cmd)
}

var bashWriteRe = regexp.MustCompile(`(?:^|[;&|(]\s*|\bsudo\s+|\bxargs\s+(?:-\S+\s+)*)(?:` +
	`rm|mv|cp|mkdir|rmdir|touch|ln|tee|truncate|install|chmod|chown|` +
	`sed\s+(?:-\S*\s+)*-[a-zA-Z]*i|perl\s+(?:-\S*\s+)*-[a-zA-Z]*i|` +
	`git\s+(?:commit|checkout|switch|reset|restore|apply|stash|merge|rebase|cherry-pick|revert|clean|mv|rm|am|pull)|` +
	`(?:npm|pnpm|yarn|bun)\s+(?:install|i|add|remove|uninstall|rm|init|update|up)|` +
	`(?:pip|pip3|uv|poetry|cargo|go)\s+(?:install|add|remove|uninstall|init|get|mod\s+tidy)|` +
	`python3?\s+-c|patch|dd` +
	`)\b`)

// redirectsToFile detects `>` / `>>` into something other than /dev/null or
// a file descriptor, outside of quotes.
func redirectsToFile(cmd string) bool {
	inS, inD := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		case c == '>' && !inS && !inD:
			j := i + 1
			if j < len(cmd) && cmd[j] == '>' {
				j++
			}
			if j < len(cmd) && cmd[j] == '&' {
				continue // 2>&1, >&2
			}
			for j < len(cmd) && cmd[j] == ' ' {
				j++
			}
			rest := cmd[j:]
			if strings.HasPrefix(rest, "/dev/null") || rest == "" {
				i = j
				continue
			}
			return true
		}
	}
	return false
}

func grantedList(tools string) string {
	var out []string
	for name := range model.GrantedTools(tools) {
		out = append(out, name)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = append(f[:n], "…")
	}
	return strings.Join(f, " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- level ----

// Level is the run's current collaboration level; legacy runs without one
// report orchestrate (their coordinator had no hands).
func (rs *RunSession) Level() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.levelLocked()
}

func (rs *RunSession) levelLocked() string {
	if rs.run.Level == "" {
		return model.LevelOrchestrate
	}
	return rs.run.Level
}

// SetLevel records a level decision with its source. It is the engine's and
// the user's call; the main agent can only ask (see RequestLevel in a later
// phase). Same level again is a no-op.
func (rs *RunSession) SetLevel(level, source, reason string) error {
	switch level {
	case model.LevelSolo, model.LevelPair, model.LevelOrchestrate:
	default:
		return fmt.Errorf("unknown level %q (solo | pair | orchestrate)", level)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.run.Level == level && rs.run.LevelSource == source {
		return nil
	}
	rs.run.Level = level
	rs.run.LevelSource = source
	rs.run.LevelLog = append(rs.run.LevelLog, model.LevelChange{Ts: time.Now(), Level: level, Source: source, Reason: reason})
	rs.appendEventLocked("level", "", fmt.Sprintf("level → %s (%s)%s", level, source, optionalColon(reason)))
	return nil
}

func optionalColon(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}
