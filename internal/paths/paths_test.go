package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindRootRequiresConfig guards the bug where the global ~/.hivemind state dir
// (which has no config.yaml) made $HOME look like a project root: FindRoot must only
// match a directory whose .hivemind contains config.yaml.
func TestFindRootRequiresConfig(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// A bare .hivemind dir (no config.yaml), like the global state dir, must NOT match.
	if err := os.MkdirAll(filepath.Join(root, ".hivemind"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r, ok := FindRoot(sub); ok {
		t.Fatalf("FindRoot matched a .hivemind dir without config.yaml: %q", r)
	}
	// With config.yaml it's a real project — found from a nested subdir.
	if err := os.WriteFile(filepath.Join(root, ".hivemind", "config.yaml"), []byte("project: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, ok := FindRoot(sub); !ok || r != root {
		t.Errorf("FindRoot(%q) = (%q, %v), want (%q, true)", sub, r, ok, root)
	}
}
