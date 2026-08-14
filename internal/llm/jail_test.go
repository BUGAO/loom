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
	if err := writeToolJail(work, "Read,Write,Edit,Bash", data); err != nil {
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
	if err := writeToolJail(work, "Read", ""); err != nil {
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
