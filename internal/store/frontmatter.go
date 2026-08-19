package store

import (
	"fmt"
	"strconv"
	"strings"

	"loom/internal/model"
)

// Agents are stored as markdown with a minimal `key: value` frontmatter block,
// structurally compatible with .claude/agents definitions so the same pool can
// be reused there. Only flat string/int keys are supported — by design.

func marshalAgent(a *model.Agent) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", a.Name)
	fmt.Fprintf(&b, "description: %s\n", strings.ReplaceAll(a.Description, "\n", " "))
	if a.Runtime != "" {
		fmt.Fprintf(&b, "runtime: %s\n", a.Runtime)
	}
	if a.Model != "" {
		fmt.Fprintf(&b, "model: %s\n", a.Model)
	}
	if a.Tools != "" {
		fmt.Fprintf(&b, "tools: %s\n", a.Tools)
	}
	if a.MaxTurns > 0 {
		fmt.Fprintf(&b, "max_turns: %d\n", a.MaxTurns)
	}
	if a.Independent {
		b.WriteString("independent: true\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(a.SystemPrompt))
	b.WriteString("\n")
	return b.String()
}

// agentsMD renders the agent's home AGENTS.md: its role (the system prompt)
// plus the standing loom execution contract. Regenerated on every save — the
// definition is the source of truth, not this file.
func agentsMD(a *model.Agent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", a.Name)
	b.WriteString(strings.TrimSpace(a.SystemPrompt))
	b.WriteString(`

---

## Loom execution contract (auto-generated; do not edit — edit the agent definition instead)

- This directory is YOUR private workspace. It persists across runs: keep notes,
  drafts and reusable material here freely.`)
	if agentCanWriteFiles(a) {
		b.WriteString(`
- MEMORY.md in this directory is your durable CRAFT memory: loom reads it into
  every task prompt you receive. Append short lessons about your craft as you
  learn them (techniques, pitfalls, checklist items) — not project facts, not
  task logs. Keep it small; its head and tail survive truncation, its middle
  may not.`)
	}
	b.WriteString(`
- Each task names the run's workspace (an absolute path): the project directory and the deliverable folder in one.
  Upstream artifacts are there; every deliverable MUST be written there too.
- Each task lists the exact tools granted to you. Calls to any other tool
  (including shell/terminal, if not granted) are rejected — never let a
  rejection end your work: continue with the tools you do have, using
  absolute paths with your file tools.
- Your private skills live under .claude/skills/ in this directory.
- End every task reply with the result envelope:
  ` + "```json" + `
  {"status": "ok", "summary": "...", "artifacts": ["paths relative to the workspace"]}
  ` + "```" + `
`)
	return b.String()
}

// agentCanWriteFiles reports whether the agent can write files in its own home
// with its own tools — the precondition for maintaining MEMORY.md (the hub's
// write_artifact only reaches the workspace, never the home).
func agentCanWriteFiles(a *model.Agent) bool {
	for _, t := range strings.Split(a.Tools, ",") {
		switch strings.TrimSpace(t) {
		case "Write", "Edit", "MultiEdit", "Bash":
			return true
		}
	}
	return false
}

func unmarshalAgent(name, raw string) *model.Agent {
	a := &model.Agent{Name: name}
	rest := raw
	if after, ok := strings.CutPrefix(raw, "---\n"); ok {
		if fm, body, ok := strings.Cut(after, "\n---"); ok {
			for _, line := range strings.Split(fm, "\n") {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				v = strings.TrimSpace(v)
				switch strings.TrimSpace(k) {
				case "name":
					a.Name = v
				case "description":
					a.Description = v
				case "runtime":
					a.Runtime = v
				case "model":
					a.Model = v
				case "tools":
					a.Tools = v
				case "max_turns":
					a.MaxTurns, _ = strconv.Atoi(v)
				case "independent":
					a.Independent = v == "true"
				}
			}
			rest = body
		}
	}
	a.SystemPrompt = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "---"))
	return a
}
