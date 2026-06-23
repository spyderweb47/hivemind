package dashboard

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/paths"
)

func TestParsePermissionRequest(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"BLOCKED: Please grant `WebFetch`/`WebSearch` to fetch docs", "WebFetch,WebSearch"},
		{"BLOCKED: I need WebFetch access to the internet", "WebFetch"},
		{"BLOCKED: grant `Read` access to the shared dir", "Read"}, // safe built-in, in backticks
		// SECURITY: agent-authored Bash rules are NOT auto-grantable (no one-keystroke
		// approval of shell access); they must be granted explicitly via /grant.
		{"BLOCKED: I need `Bash(rm:*)` to clean up temp files", ""},
		{"BLOCKED: grant `Bash(curl:*)` so I can call the API", ""},
		{"BLOCKED: I am stuck on the task and need to read the file", ""}, // no false positive on task/read
		{"BLOCKED: nothing structured here", ""},
	}
	for _, c := range cases {
		if got := strings.Join(parsePermissionRequest(c.in), ","); got != c.want {
			t.Errorf("parsePermissionRequest(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDetailOverlayShowsFullMessageAndGrants(t *testing.T) {
	m := newTestModel()
	full := "BLOCKED: Please grant `WebFetch` and `WebSearch` so I can fetch the external documentation the task references."
	views := []agent.View{
		{Name: "supervisor", State: agent.StateIdle},
		{Name: "ron", State: agent.StateBlocked, Model: "opus", FullMessage: full},
		{Name: "bob", State: agent.StateIdle},
	}
	m = feed(m, tea.WindowSizeMsg{Width: 90, Height: 30}, dataMsg{agents: views})
	// select ron, open the detail overlay with 'v'
	m = feed(m, key(tea.KeyDown), runes("v"))
	if m.detailAgent != "ron" {
		t.Fatalf("expected detail overlay for ron, got %q", m.detailAgent)
	}
	// full (untruncated) message is rendered — "documentation" appears only near
	// the end, well past the ~33-char table truncation point (word survives wrap).
	if !strings.Contains(m.detailView(), "documentation") {
		t.Errorf("detail view should render the full untruncated message")
	}
	// blocked → a grant choice is offered with the parsed rules
	if len(m.detailChoices) == 0 || m.detailChoices[0].kind != "grant" {
		t.Fatalf("expected a grant choice first, got %+v", m.detailChoices)
	}
	if got := strings.Join(m.detailChoices[0].rules, ","); got != "WebFetch,WebSearch" {
		t.Errorf("grant rules = %q, want WebFetch,WebSearch", got)
	}
	// activating the grant closes the overlay and flashes (the runSelf cmd is async)
	tm, _ := m.Update(key(tea.KeyEnter))
	m = tm.(model)
	if m.detailAgent != "" {
		t.Errorf("overlay should close after activating a choice")
	}
	if !strings.Contains(m.flash, "granting") {
		t.Errorf("expected a granting flash, got %q", m.flash)
	}
}

func TestBlockedBashNotAutoGrantable(t *testing.T) {
	m := newTestModel()
	views := []agent.View{
		{Name: "supervisor", State: agent.StateIdle},
		{Name: "ron", State: agent.StateBlocked, FullMessage: "BLOCKED: I need `Bash(rm -rf /:*)` to clean up temp files."},
		{Name: "bob", State: agent.StateIdle},
	}
	m = feed(m, tea.WindowSizeMsg{Width: 80, Height: 30}, dataMsg{agents: views})
	m = feed(m, key(tea.KeyDown), runes("v"))
	for _, c := range m.detailChoices {
		if c.kind == "grant" {
			t.Fatalf("a Bash rule from agent text must NOT become a one-keystroke grant, got %+v", c)
		}
	}
	// reply + close should still be offered so the user can respond
	if len(m.detailChoices) != 2 {
		t.Errorf("expected reply+close choices for an unparseable/unsafe request, got %d", len(m.detailChoices))
	}
}

func TestDetailPermissionPromptSurvivesShortTerminal(t *testing.T) {
	m := newTestModel()
	long := "BLOCKED: Please grant `WebFetch`. " + strings.Repeat("This is a very long explanation that wraps across many lines. ", 20)
	views := []agent.View{
		{Name: "supervisor", State: agent.StateIdle},
		{Name: "ron", State: agent.StateBlocked, Model: "opus", FullMessage: long},
		{Name: "bob", State: agent.StateIdle},
	}
	m = feed(m, tea.WindowSizeMsg{Width: 70, Height: 16}, dataMsg{agents: views})
	m = feed(m, key(tea.KeyDown), runes("v"))
	out := m.View()
	if got := strings.Count(out, "\n") + 1; got > 16 {
		t.Fatalf("detail overlay rendered %d lines, exceeds height 16", got)
	}
	// the actionable grant choice + hint must remain visible despite the long message
	if !strings.Contains(out, "Grant WebFetch") {
		t.Errorf("grant choice was clipped off the short terminal:\n%s", out)
	}
	if !strings.Contains(out, "enter select") {
		t.Errorf("choice hint was clipped off the short terminal")
	}
}

func TestDetailOverlayNonBlockedClosesOnAnyKey(t *testing.T) {
	m := newTestModel()
	views := []agent.View{
		{Name: "supervisor", State: agent.StateIdle},
		{Name: "ron", State: agent.StateIdle, FullMessage: "all good, finished the report"},
		{Name: "bob", State: agent.StateIdle},
	}
	m = feed(m, tea.WindowSizeMsg{Width: 80, Height: 30}, dataMsg{agents: views})
	m = feed(m, key(tea.KeyDown), runes("v"))
	if m.detailAgent != "ron" || len(m.detailChoices) != 0 {
		t.Fatalf("idle agent should open a read-only detail (no choices), got agent=%q choices=%d", m.detailAgent, len(m.detailChoices))
	}
	m = feed(m, runes("x")) // any key closes
	if m.detailAgent != "" {
		t.Errorf("read-only detail should close on any key")
	}
}

func feed(m model, msgs ...tea.Msg) model {
	var tm tea.Model = m
	for _, msg := range msgs {
		tm, _ = tm.Update(msg)
	}
	return tm.(model)
}

func runes(s string) tea.KeyMsg    { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func newTestModel() model {
	cfg := &config.Project{Project: "t", Agents: []config.Agent{{Name: "ron"}, {Name: "bob"}}}
	return New(paths.NewProject("/tmp/does-not-exist-xyz"), cfg, false)
}

func TestConsoleHelpCommand(t *testing.T) {
	m := newTestModel()
	m = feed(m, key(tea.KeyTab), runes("/help"), key(tea.KeyEnter))
	if !m.showHelp {
		t.Fatalf("expected /help to open the help overlay")
	}
	if !strings.Contains(m.helpView(), "/delegate") {
		t.Errorf("help overlay missing /delegate")
	}
	// any key closes it
	m = feed(m, key(tea.KeyEsc))
	if m.showHelp {
		t.Errorf("expected help to close on a key")
	}
}

func TestConsoleUnknownCommand(t *testing.T) {
	m := newTestModel()
	m = feed(m, key(tea.KeyTab), runes("/nope"), key(tea.KeyEnter))
	if !strings.Contains(m.flash, "unknown command") {
		t.Errorf("expected unknown-command flash, got %q", m.flash)
	}
}

// TestViewClampsToHeight guards the alt-screen ghosting fix: a tall frame (open
// slash palette) on a short terminal must never render more lines than the window
// height, and the interactive footer (status bar) must stay on the last line.
func TestViewClampsToHeight(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 100, Height: 12}, key(tea.KeyTab), runes("/"))
	if !m.paletteOpen() {
		t.Fatalf("expected the slash palette to be open")
	}
	out := m.View()
	if got := strings.Count(out, "\n") + 1; got > 12 {
		t.Fatalf("View rendered %d lines, exceeds window height 12:\n%s", got, out)
	}
	lines := strings.Split(out, "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "shortcuts") {
		t.Errorf("status bar should stay pinned to the last line, got %q", last)
	}
}

func TestConsoleSlashUsageGuard(t *testing.T) {
	m := newTestModel()
	// '/tool' without enough args should flash usage, not crash
	m = feed(m, key(tea.KeyTab), runes("/tool start"), key(tea.KeyEnter))
	if !strings.Contains(m.flash, "usage:") {
		t.Errorf("expected usage flash, got %q", m.flash)
	}
}
