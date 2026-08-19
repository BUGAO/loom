package model

import "strings"

// ToolGrants maps loom allowlist tokens (an agent's comma-separated Tools
// field) onto the Claude Code tools they legitimize. It is the single source
// for both enforcement layers: the static deny jail written into a session's
// settings (llm) and the live hook gate (hub).
var ToolGrants = map[string][]string{
	"read":      {"Read", "NotebookRead"},
	"grep":      {"Grep"},
	"glob":      {"Glob"},
	"write":     {"Write", "NotebookEdit"},
	"edit":      {"Edit", "MultiEdit", "NotebookEdit"},
	"bash":      {"Bash", "BashOutput", "KillShell"},
	"webfetch":  {"WebFetch"},
	"websearch": {"WebSearch"},
}

// CapabilityTools is every capability-granting Claude Code tool loom manages.
// Task is here unconditionally: subagents spawned outside the ledger are
// never a granted capability, whatever the allowlist says. Tools absent from
// this list (Skills, TodoWrite, MCP tools) act within the session and are
// never gated by allowlist.
var CapabilityTools = []string{
	"Task", "Bash", "BashOutput", "KillShell", "Read", "NotebookRead", "Write",
	"Edit", "MultiEdit", "NotebookEdit", "Grep", "Glob", "WebFetch", "WebSearch",
}

// WriteTools are the structured file-writing tools — the ones whose target
// path the hook gate can see and judge.
var WriteTools = []string{"Write", "Edit", "MultiEdit", "NotebookEdit"}

// IsCapabilityTool reports whether name is one of the managed tools.
func IsCapabilityTool(name string) bool {
	for _, t := range CapabilityTools {
		if t == name {
			return true
		}
	}
	return false
}

// IsWriteTool reports whether name is a structured file-writing tool.
func IsWriteTool(name string) bool {
	for _, t := range WriteTools {
		if t == name {
			return true
		}
	}
	return false
}

// GrantedTools expands an allowlist into the set of Claude Code tool names it
// legitimizes.
func GrantedTools(tools string) map[string]bool {
	keep := map[string]bool{}
	for _, t := range strings.Split(tools, ",") {
		for _, name := range ToolGrants[strings.ToLower(strings.TrimSpace(t))] {
			keep[name] = true
		}
	}
	return keep
}

// ToolGranted reports whether Claude Code tool name is legitimized by the
// allowlist. Unmanaged tools are always granted.
func ToolGranted(tools, name string) bool {
	if !IsCapabilityTool(name) {
		return true
	}
	return GrantedTools(tools)[name]
}
