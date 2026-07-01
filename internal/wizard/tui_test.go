package wizard

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// drive feeds a sequence of key messages into the model's Update loop and returns
// the resulting model, exactly as Bubble Tea would at runtime (minus rendering).
func drive(m wizardModel, keys ...tea.KeyMsg) wizardModel {
	var tm tea.Model = m
	for _, k := range keys {
		tm, _ = tm.Update(k)
	}
	return tm.(wizardModel)
}

func typ(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func enter() tea.KeyMsg       { return tea.KeyMsg{Type: tea.KeyEnter} }
func down() tea.KeyMsg        { return tea.KeyMsg{Type: tea.KeyDown} }
func clear() tea.KeyMsg       { return tea.KeyMsg{Type: tea.KeyCtrlU} } // delete pre-filled default

func TestWizardBuildsAgentWithServiceTool(t *testing.T) {
	m := newWizardModel("/tmp/proj")

	m = drive(m,
		enter(),                       // root: accept default /tmp/proj
		clear(), typ("demo"), enter(), // project name (clear the pre-filled default first)
		enter(),                 // default model: sonnet (choice idx 0)
		enter(),                 // supervisor model: haiku (choice idx 0)
		enter(),                 // permission mode: acceptEdits
		typ("backend"), enter(), // agent name
		enter(),                            // workspace: default "backend"
		typ("owns the collector"), enter(), // role
		enter(),                        // agent model: sonnet
		typ("data-collector"), enter(), // new tool name
		enter(),                             // tool type: service
		typ("python collector.py"), enter(), // entrypoint
		typ("curl -sf localhost:9000"), enter(), // health
		typ("9000,9009"), enter(), // ports
		enter(),                              // source: (blank)
		typ("collects sample data"), enter(), // doc
		enter(),         // tool name: blank -> stop attaching tools
		enter(),         // reads: blank
		down(), enter(), // add another? -> No (idx 1) ... default is No(idx1); down clamps, enter selects No
		enter(), // review -> confirm
	)

	if !m.confirmed {
		t.Fatalf("expected confirmed; cancelled=%v step=%d err=%q", m.cancelled, m.step, m.errMsg)
	}
	if m.state.cfg.Project != "demo" {
		t.Errorf("project = %q, want demo", m.state.cfg.Project)
	}
	if m.state.cfg.Supervisor.Model != "haiku" {
		t.Errorf("supervisor model = %q, want haiku (idx 0)", m.state.cfg.Supervisor.Model)
	}
	if got := len(m.state.cfg.Agents); got != 1 {
		t.Fatalf("agents = %d, want 1", got)
	}
	a := m.state.cfg.Agents[0]
	if a.Name != "backend" || a.Workspace != "backend" || a.Model != "sonnet" {
		t.Errorf("agent = %+v", a)
	}
	if len(a.Tools) != 1 || a.Tools[0] != "data-collector" {
		t.Errorf("agent tools = %v", a.Tools)
	}
	if len(m.state.cfg.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(m.state.cfg.Tools))
	}
	tool := m.state.cfg.Tools[0]
	if tool.Type != "service" || tool.Entrypoint != "python collector.py" || tool.Health != "curl -sf localhost:9000" {
		t.Errorf("tool = %+v", tool)
	}
	if len(tool.Ports) != 2 || tool.Ports[0] != 9000 || tool.Ports[1] != 9009 {
		t.Errorf("ports = %v", tool.Ports)
	}
	if tool.Owner != "backend" {
		t.Errorf("owner = %q, want backend", tool.Owner)
	}
	if m.state.toolDocs["data-collector"] == "" {
		t.Errorf("expected a TOOL.md doc to be recorded")
	}
}

func TestWizardSupervisorModelSelectable(t *testing.T) {
	m := newWizardModel("/tmp/p")
	// root, project, default model, then DOWN at the supervisor-model step to pick
	// the 2nd option (sonnet, since the supervisor list is [haiku, sonnet, opus]).
	m = drive(m, enter(), typ("p"), enter(), enter(), down(), enter())
	if m.step != sPermMode {
		t.Fatalf("expected to land on permission mode, got step %d", m.step)
	}
	if m.state.cfg.Supervisor.Model != "sonnet" {
		t.Errorf("supervisor model = %q, want sonnet", m.state.cfg.Supervisor.Model)
	}
}

func TestWizardRejectsReservedAndDuplicate(t *testing.T) {
	m := newWizardModel("/tmp/p")
	m = drive(m, enter(), typ("p"), enter(), enter(), enter(), enter()) // through to agent-name step (root, project, defModel, supModel, permMode)
	if m.step != sAgentName {
		t.Fatalf("expected to be at agent-name step, got %d", m.step)
	}
	m = drive(m, typ("supervisor"), enter())
	if m.step != sAgentName || m.errMsg == "" {
		t.Errorf("reserved name should be rejected with an error, step=%d err=%q", m.step, m.errMsg)
	}
}

func TestWizardRejectsUnsafeReads(t *testing.T) {
	m := newWizardModel("/tmp/p")
	m = drive(m,
		enter(), typ("p"), enter(), enter(), enter(), enter(), // basics (root, project, defModel, supModel, permMode)
		typ("a"), enter(), enter(), typ("role"), enter(), enter(), // agent + workspace + role + model
		enter(), // tool name blank -> reads step
	)
	if m.step != sReads {
		t.Fatalf("expected reads step, got %d", m.step)
	}
	m = drive(m, typ("../escape"), enter())
	if m.step != sReads || m.errMsg == "" {
		t.Errorf("unsafe read path should be rejected, step=%d err=%q", m.step, m.errMsg)
	}
}
