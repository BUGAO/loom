package store

import (
	"os"
	"path/filepath"
	"testing"
)

// The picker's folder operations are confined to $HOME (like BrowseDirs) and
// rename only ever moves an EMPTY folder — the kind the picker itself creates.
// t.Setenv(HOME) points confinement at a temp dir; macOS resolves /var → /private
// so the checks below go through EvalSymlinks like the code does.
func TestMkdirAndRenameWorkspaceDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, err := New(filepath.Join(home, ".loom"))
	if err != nil {
		t.Fatal(err)
	}
	real := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Create under home.
	got, err := s.MkdirWorkspace(home, "my project")
	if err != nil {
		t.Fatal(err)
	}
	if real(got) != real(filepath.Join(home, "my project")) {
		t.Fatalf("mkdir returned %s", got)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Fatalf("folder not created: %v", err)
	}
	// Twice is an error, not a silent reuse.
	if _, err := s.MkdirWorkspace(home, "my project"); err == nil {
		t.Fatal("creating an existing folder must fail")
	}
	// Bad names and escapes are refused.
	for _, bad := range []string{"", ".hidden", "a/b", "..", "x..", " "} {
		if _, err := s.MkdirWorkspace(home, bad); err == nil {
			t.Errorf("name %q should be refused", bad)
		}
	}
	// Outside home is refused (the temp dir's parent is outside the fake home).
	if _, err := s.MkdirWorkspace(filepath.Dir(home), "escape"); err == nil {
		t.Fatal("mkdir outside home must be refused")
	}

	// The listing marks it empty.
	listing, err := s.BrowseDirs(home)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, d := range listing.Dirs {
		if d.Name == "my project" {
			seen = true
			if !d.Empty {
				t.Fatal("a freshly created folder should be listed as empty")
			}
		}
	}
	if !seen {
		t.Fatal("created folder missing from listing")
	}

	// Rename follows the MRU history.
	if err := s.SaveRecentWorkspace(got); err != nil {
		t.Fatal(err)
	}
	renamed, err := s.RenameWorkspaceDir(got, "newspush")
	if err != nil {
		t.Fatal(err)
	}
	if real(renamed) != real(filepath.Join(home, "newspush")) {
		t.Fatalf("rename returned %s", renamed)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatal("old folder should be gone after rename")
	}
	recent, _ := s.ListRecentWorkspaces()
	if len(recent) != 1 || real(recent[0]) != real(renamed) {
		t.Fatalf("MRU should follow the rename, got %v", recent)
	}
	// Same name is a no-op.
	if again, err := s.RenameWorkspaceDir(renamed, "newspush"); err != nil || again != renamed {
		t.Fatalf("same-name rename should be a no-op: %s %v", again, err)
	}

	// A folder with content — even a dotfile — is never renamed here.
	if err := os.WriteFile(filepath.Join(renamed, ".keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameWorkspaceDir(renamed, "other"); err == nil {
		t.Fatal("renaming a non-empty folder must be refused")
	}
	// Home itself and anything outside are refused.
	if _, err := s.RenameWorkspaceDir(home, "x"); err == nil {
		t.Fatal("renaming home must be refused")
	}
	if _, err := s.RenameWorkspaceDir(filepath.Dir(home), "x"); err == nil {
		t.Fatal("renaming outside home must be refused")
	}
}
