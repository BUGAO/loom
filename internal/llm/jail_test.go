package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The jail must carry both layers: tool-level rules from the allowlist and
// path-level rules shielding loom's own state from the granted tools.
func TestToolJailPathDeny(t *testing.T) {
	work := t.TempDir()
	data := "/Users/tester/.loom/data"
	if err := writeToolJail(work, "Read,Write,Edit,Bash", data, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(work, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	deny := strings.Join(cfg.Permissions.Deny, "\n")

	// Granted tools are not denied at tool level…
	for _, granted := range []string{"Write", "Edit", "Read", "Bash"} {
		for _, rule := range cfg.Permissions.Deny {
			if rule == granted {
				t.Errorf("granted tool %s is denied outright", granted)
			}
		}
	}
	// …but their write forms are path-denied on loom's control surfaces.
	for _, want := range []string{
		"Write(//Users/tester/.loom/data/workflows/**)",
		"Write(//Users/tester/.loom/data/agents/*/agent.md)",
		"Write(//Users/tester/.loom/data/agents/*/home/.claude/**)",
		"Write(//Users/tester/.loom/data/runs/*/run.json)",
		"Write(//Users/tester/.loom/data/**/AGENTS.md)",
		"Edit(//Users/tester/.loom/data/**/CLAUDE.md)",
		"Write(**/.claude/settings.local.json)",
	} {
		if !strings.Contains(deny, want) {
			t.Errorf("missing path rule %q", want)
		}
	}
	// Task stays denied outright regardless of allowlist.
	if !strings.Contains(deny, "Task") {
		t.Error("Task must remain denied")
	}
}

// Without a protect dir (tests, ad-hoc), only the session's own jail file is
// path-protected.
func TestToolJailNoProtectDir(t *testing.T) {
	work := t.TempDir()
	if err := writeToolJail(work, "Read", "", false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(work, ".claude", "settings.local.json"))
	s := string(raw)
	if strings.Contains(s, ".loom") {
		t.Error("unexpected data-dir rules without a protect dir")
	}
	if !strings.Contains(s, "Write(**/.claude/settings.local.json)") {
		t.Error("own-jail protection missing")
	}
}

// A USER workspace is not loom's file: the jail merges into what is there,
// adds only path rules and the gate hooks when the session is gated, and the
// last session out removes exactly what loom added.
func TestToolJailMergesIntoUserWorkspace(t *testing.T) {
	work := t.TempDir()
	data := filepath.Join(t.TempDir(), "data") // elsewhere: work is a user workspace
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	user := `{"permissions":{"allow":["Bash(npm test)"],"deny":["WebFetch"]},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo user"}]}]},"theme":"dark"}`
	path := filepath.Join(work, ".claude", "settings.local.json")
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two gated sessions with DIFFERENT allowlists share the workspace.
	if err := writeToolJail(work, "read,edit,bash", data, true); err != nil {
		t.Fatal(err)
	}
	if err := writeToolJail(work, "read", data, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	perms := cfg["permissions"].(map[string]any)
	deny := fmtList(perms["deny"])
	// User entries survive; loom adds no tool-level deny in a workspace (the
	// gate judges per identity — the second session must not jail the first).
	if !strings.Contains(deny, "WebFetch") {
		t.Errorf("user's own deny entry lost: %s", deny)
	}
	for _, tool := range []string{"Edit", "Bash", "Task"} {
		for _, d := range perms["deny"].([]any) {
			if d == tool {
				t.Errorf("workspace jail must not carry tool-level deny %q (gate's job): %s", tool, deny)
			}
		}
	}
	if !strings.Contains(deny, "Write(**/.claude/settings.local.json)") {
		t.Errorf("own-jail path rule missing: %s", deny)
	}
	if fmtList(perms["allow"]) != "Bash(npm test)" {
		t.Errorf("user's allow list altered: %v", perms["allow"])
	}
	if cfg["theme"] != "dark" {
		t.Error("unrelated user setting altered")
	}
	hooks := cfg["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("expected the user's hook plus one loom gate hook, got %d: %v", len(pre), pre)
	}
	if !isGateHookEntry(pre[1]) || isGateHookEntry(pre[0]) {
		t.Errorf("loom's hook should be appended after the user's: %v", pre)
	}
	if post, _ := hooks["PostToolUse"].([]any); len(post) != 1 || !isGateHookEntry(post[0]) {
		t.Errorf("PostToolUse attribution hook missing: %v", hooks["PostToolUse"])
	}

	// First session closes: the other is still live, nothing changes.
	releaseToolJail(work, data)
	raw2, _ := os.ReadFile(path)
	if string(raw2) != string(raw) {
		t.Error("jail changed while another session in the cwd is live")
	}
	// Last session closes: loom's entries go, the user's stay.
	releaseToolJail(work, data)
	raw3, _ := os.ReadFile(path)
	var after map[string]any
	if err := json.Unmarshal(raw3, &after); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw3), "settings.local.json)") || strings.Contains(string(raw3), "' gate") {
		t.Errorf("loom entries not removed on last close:\n%s", raw3)
	}
	if fmtList(after["permissions"].(map[string]any)["deny"]) != "WebFetch" || after["theme"] != "dark" {
		t.Errorf("user's settings damaged by cleanup:\n%s", raw3)
	}
	if pre, _ := after["hooks"].(map[string]any)["PreToolUse"].([]any); len(pre) != 1 {
		t.Errorf("user's hook lost: %v", after["hooks"])
	}
}

// A workspace with no settings at all gets a file only while loom is there.
func TestToolJailCreatesAndRemovesInEmptyWorkspace(t *testing.T) {
	work := t.TempDir()
	data := filepath.Join(t.TempDir(), "data")
	if err := writeToolJail(work, "read,edit", data, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(work, ".claude", "settings.local.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("jail not written")
	}
	releaseToolJail(work, data)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("jail file should be removed when loom created it and nothing else is in it")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Error(".claude dir loom created should be removed when empty")
	}
}

// An ungated session in a workspace keeps the full tool-level list: the gate
// is the only thing that may relax it, and it is not there.
func TestToolJailUngatedWorkspaceKeepsDenyList(t *testing.T) {
	work := t.TempDir()
	data := filepath.Join(t.TempDir(), "data")
	if err := writeToolJail(work, "read", data, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(work, ".claude", "settings.local.json"))
	if !strings.Contains(string(raw), `"Edit"`) || !strings.Contains(string(raw), `"Task"`) {
		t.Errorf("ungated workspace session must keep the deny list:\n%s", raw)
	}
	if strings.Contains(string(raw), "' gate") {
		t.Error("no hook without a gate credential")
	}
	releaseToolJail(work, data)
}

// An agent home is loom's own: rewritten wholesale, never cleaned up.
func TestToolJailAgentHomeRewritten(t *testing.T) {
	data := t.TempDir()
	home := filepath.Join(data, "agents", "x", "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.local.json")
	os.WriteFile(path, []byte(`{"garbage": true}`), 0o644)
	if err := writeToolJail(home, "read", data, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "garbage") {
		t.Error("agent home jail must be rewritten wholesale")
	}
	if !strings.Contains(string(raw), `"Edit"`) || !strings.Contains(string(raw), "' gate") {
		t.Errorf("agent home jail needs the deny list AND the hooks:\n%s", raw)
	}
	releaseToolJail(home, data)
	if _, err := os.Stat(path); err != nil {
		t.Error("agent home jail must survive session close")
	}
}

func fmtList(v any) string {
	arr, _ := v.([]any)
	var out []string
	for _, x := range arr {
		out = append(out, x.(string))
	}
	return strings.Join(out, ",")
}
