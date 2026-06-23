package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/scaffold"
)

// TestAssignDroppedScript exercises the dashboard's 't' tool-assign flow without a
// TTY: a script dropped in an agent's workspace is discovered and, on assign,
// registered as a command tool + attached (regenerating the agent's CLAUDE.md).
func TestAssignDroppedScript(t *testing.T) {
	dir := t.TempDir()
	p := paths.NewProject(dir)
	cfg := &config.Project{
		Project:    "t",
		Defaults:   config.Defaults{Model: "haiku", PermissionMode: "acceptEdits"},
		Supervisor: config.Supervisor{Model: "haiku"},
		Agents:     []config.Agent{{Name: "w", Workspace: "w", Role: "worker"}},
	}
	if err := os.MkdirAll(p.HivemindDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(p.ConfigPath(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := scaffold.Project(p, cfg); err != nil {
		t.Fatal(err)
	}
	// drop a script in w's workspace
	if err := os.WriteFile(filepath.Join(p.WorkspaceDir("w"), "collector.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(p, cfg, false)
	m.selIdx = 1 // [supervisor, w] → select w
	if m.selectedAgent() != "w" {
		t.Fatalf("selectedAgent = %q, want w", m.selectedAgent())
	}

	choices := m.buildAssignChoices("w")
	if len(choices) != 1 || choices[0].scriptName != "collector" {
		t.Fatalf("expected one script choice 'collector', got %+v", choices)
	}
	m.assignCh = choices
	m.assignIdx = 0
	m = m.performAssign()

	if cfg.FindTool("collector") == nil {
		t.Fatal("collector tool was not registered")
	}
	a := cfg.FindAgent("w")
	if a == nil || len(a.Tools) != 1 || a.Tools[0] != "collector" {
		t.Fatalf("collector not attached to w: %+v", a)
	}
	md, _ := os.ReadFile(filepath.Join(p.WorkspaceDir("w"), ".claude", "CLAUDE.md"))
	if !strings.Contains(string(md), "Tool: collector") {
		t.Fatal("CLAUDE.md was not regenerated to teach the collector tool")
	}
	if _, err := os.Stat(filepath.Join(p.ToolDir("collector"), "collector.py")); err != nil {
		t.Fatal("dropped script was not copied into the tool dir")
	}
}
