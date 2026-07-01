package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/procscan"
	"hivemind/internal/tools"
	"hivemind/internal/transcript"
)

func TestConversationViewAsyncLoadAndScroll(t *testing.T) {
	views := []agent.View{
		{Name: "supervisor", State: agent.StateIdle},
		{Name: "ron", State: agent.StateIdle, Model: "opus"},
		{Name: "bob", State: agent.StateIdle},
	}
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 80, Height: 20}, dataMsg{agents: views})
	// open detail on ron → loading state, conversation read off-thread
	m = feed(m, key(tea.KeyDown), runes("v"))
	if m.detailAgent != "ron" || !m.detailLoading {
		t.Fatalf("expected loading detail for ron; agent=%q loading=%v", m.detailAgent, m.detailLoading)
	}
	// the async read result arrives → cached lines, no longer loading
	var items []transcript.Item
	for i := 0; i < 20; i++ {
		items = append(items, transcript.Item{Role: "assistant", Kind: "text", Text: fmt.Sprintf("line %d of the reply", i)})
	}
	m = feed(m, convMsg{agent: "ron", items: items})
	if m.detailLoading || len(m.detailLines) == 0 {
		t.Fatalf("convMsg should cache lines and clear loading; loading=%v lines=%d", m.detailLoading, len(m.detailLines))
	}
	// a long conversation opens scrolled to the bottom (latest message)
	if m.detailScrollOffset == 0 {
		t.Errorf("long conversation should open at the bottom, got offset 0")
	}
	// g jumps to top, G back to the bottom
	m = feed(m, runes("g"))
	if m.detailScrollOffset != 0 {
		t.Errorf("g should jump to top, got %d", m.detailScrollOffset)
	}
	m = feed(m, runes("G"))
	if m.detailScrollOffset == 0 {
		t.Errorf("G should jump to the bottom")
	}
	// scrolling stays bounded
	for i := 0; i < 500; i++ {
		m = feed(m, runes("j"))
	}
	if m.detailScrollOffset > len(m.detailLines)-1 {
		t.Errorf("scroll offset %d exceeds %d cached lines", m.detailScrollOffset, len(m.detailLines))
	}
	// a convMsg for a different agent (user navigated away) is ignored
	m2 := newTestModel()
	m2 = feed(m2, dataMsg{agents: views}, key(tea.KeyDown), runes("v"))
	m2 = feed(m2, convMsg{agent: "bob", items: items})
	if len(m2.detailLines) != 0 {
		t.Errorf("a convMsg for a non-displayed agent must be ignored")
	}
}

func TestToolsPanelShowsAllTypes(t *testing.T) {
	cfg := &config.Project{Project: "d", Tools: []config.Tool{
		{Name: "csv_export", Type: "command", Owner: "alice"},
		{Name: "collector", Type: "service", Owner: "alice"},
		{Name: "tpl", Type: "library", Owner: "alice"},
	}}
	m := New(paths.NewProject("/tmp/does-not-exist-zz9"), cfg, true)
	m = feed(m, dataMsg{tools: []tools.Status{{Name: "collector", State: tools.StateRunning}}})
	out := m.renderToolsPanel()
	for _, want := range []string{"csv_export", "command", "collector", "RUNNING", "tpl", "library"} {
		if !strings.Contains(out, want) {
			t.Errorf("tools panel missing %q:\n%s", want, out)
		}
	}
}

func TestToolActionPicker(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 90, Height: 30}, dataMsg{tools: []tools.Status{
		{Name: "collector", State: tools.StateRunning},
		{Name: "jupyter", State: tools.StateStopped},
	}})
	// /tool stop (no name) → picker for the 'stop' action
	m = feed(m, key(tea.KeyTab), runes("/tool stop"), key(tea.KeyEnter))
	if !m.toolPicking || m.toolAction != "stop" {
		t.Fatalf("expected stop picker; picking=%v action=%q", m.toolPicking, m.toolAction)
	}
	if len(m.toolMatches()) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(m.toolMatches()))
	}
	// filter to 'jup' → jupyter
	m = feed(m, runes("jup"))
	if ms := m.toolMatches(); len(ms) != 1 || ms[0].Name != "jupyter" {
		t.Errorf("filter failed: %+v", ms)
	}
	// enter runs the action and closes
	tm, _ := m.Update(key(tea.KeyEnter))
	m = tm.(model)
	if m.toolPicking {
		t.Errorf("picker should close after enter")
	}
	if !strings.Contains(m.flash, "stop jupyter") {
		t.Errorf("expected a 'stop jupyter' flash, got %q", m.flash)
	}
	// esc cancels
	m2 := newTestModel()
	m2 = feed(m2, tea.WindowSizeMsg{Width: 90, Height: 30}, dataMsg{tools: []tools.Status{{Name: "x"}}})
	m2 = feed(m2, key(tea.KeyTab), runes("/tool start"), key(tea.KeyEnter), key(tea.KeyEsc))
	if m2.toolPicking {
		t.Errorf("esc should close the tool picker")
	}
}

func TestToolPickerFlow(t *testing.T) {
	dir := t.TempDir()
	p := paths.NewProject(dir)
	_ = os.MkdirAll(filepath.Join(dir, "alice"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "alice", "scraper.py"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "alice", ".hidden"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "alice", "bad name.py"), []byte("x"), 0o644) // unsafe name → excluded
	cfg := &config.Project{Project: "demo", Agents: []config.Agent{{Name: "alice", Workspace: "alice"}}}
	if err := config.Save(p.ConfigPath(), cfg); err != nil {
		t.Fatal(err)
	}
	m := New(p, cfg, true)
	m = feed(m, tea.WindowSizeMsg{Width: 80, Height: 26}, dataMsg{})
	// select alice (idx 1), press g → file picker opens with her workspace files (.hidden excluded)
	m = feed(m, key(tea.KeyDown), runes("g"))
	if !m.picking || m.pickAgent != "alice" {
		t.Fatalf("expected picker open for alice; picking=%v agent=%q", m.picking, m.pickAgent)
	}
	if len(m.pickFiles) != 1 || m.pickFiles[0] != "scraper.py" {
		t.Fatalf("pickFiles=%v, want [scraper.py] (.hidden excluded)", m.pickFiles)
	}
	// choose the file → command/service TYPE choice
	m = feed(m, key(tea.KeyEnter))
	if m.picking || !m.registerTypeChoice {
		t.Fatalf("expected type-choice step; picking=%v typeChoice=%v", m.picking, m.registerTypeChoice)
	}
	if m.registerAgent != "alice" || m.registerName != "scraper" {
		t.Errorf("registerAgent=%q name=%q, want alice/scraper", m.registerAgent, m.registerName)
	}
	// pick "command" (idx 0) → intent step
	m = feed(m, key(tea.KeyEnter))
	if m.registerTypeChoice || !m.awaitingIntent || m.registerType != "command" {
		t.Fatalf("expected intent step with command type; typeChoice=%v awaiting=%v type=%q", m.registerTypeChoice, m.awaitingIntent, m.registerType)
	}
	// tab away from the intent step abandons it cleanly (no stranded state)
	mt := feed(m, key(tea.KeyTab))
	if mt.awaitingIntent || mt.registerPath != "" || mt.registerAgent != "" {
		t.Errorf("tab should cancel the intent step; awaiting=%v path=%q agent=%q", mt.awaitingIntent, mt.registerPath, mt.registerAgent)
	}
	// type the intent + enter → generation starts
	m = feed(m, runes("fetches urls"), key(tea.KeyEnter))
	if !m.registerLoading || m.awaitingIntent || m.registerAbout != "fetches urls" {
		t.Errorf("expected generation to start; loading=%v awaiting=%v about=%q", m.registerLoading, m.awaitingIntent, m.registerAbout)
	}
	// esc on the picker cancels cleanly
	m2 := New(p, cfg, true)
	m2 = feed(m2, tea.WindowSizeMsg{Width: 80, Height: 26}, dataMsg{})
	m2 = feed(m2, key(tea.KeyDown), runes("g"), key(tea.KeyEsc))
	if m2.picking {
		t.Errorf("esc should close the picker")
	}
	// the supervisor has no workspace → picker refuses
	m3 := New(p, cfg, true)
	m3 = feed(m3, tea.WindowSizeMsg{Width: 80, Height: 26}, dataMsg{}, runes("g")) // selection is supervisor (idx 0)
	if m3.picking || !strings.Contains(m3.flash, "supervisor") {
		t.Errorf("supervisor picker should be refused, got picking=%v flash=%q", m3.picking, m3.flash)
	}
}

func TestParseOptions(t *testing.T) {
	opts := parseOptions("BLOCKED: Which database?\n1. Postgres (durable)\n2. SQLite (simple)\n3) MySQL")
	if len(opts) != 3 || opts[0] != "Postgres (durable)" || opts[2] != "MySQL" {
		t.Fatalf("parseOptions = %+v", opts)
	}
	if got := parseOptions("BLOCKED: I need WebFetch access to the docs"); len(got) != 0 {
		t.Errorf("plain blocked message should yield no options, got %+v", got)
	}
	// a numbered list that is NOT a decision (no question/choice words) → no options
	if got := parseOptions("BLOCKED: I tried these and they failed:\n1. curl the API\n2. wget the file"); len(got) != 0 {
		t.Errorf("non-decision numbered list must not yield options, got %+v", got)
	}
	// duplicates are deduped
	if got := parseOptions("Which? please choose:\n1. A\n2. A\n3. B"); len(got) != 2 {
		t.Errorf("duplicate options should dedup to 2, got %+v", got)
	}
}

func TestDetailChoicesRebuiltOnRefresh(t *testing.T) {
	m := newTestModel()
	blocked := agent.View{Name: "ron", State: agent.StateBlocked, FullMessage: "BLOCKED: Which? choose:\n1. A\n2. B"}
	m = feed(m, tea.WindowSizeMsg{Width: 90, Height: 30}, dataMsg{agents: []agent.View{{Name: "supervisor"}, blocked, {Name: "bob"}}})
	m = feed(m, key(tea.KeyDown), runes("v"))
	if len(m.detailChoices) == 0 || m.detailChoices[0].kind != "answer" {
		t.Fatalf("expected answer choices for blocked ron, got %+v", m.detailChoices)
	}
	// the agent resumes (no longer blocked) → a refresh clears the stale choices
	resumed := agent.View{Name: "ron", State: agent.StateIdle, FullMessage: "done"}
	m = feed(m, dataMsg{agents: []agent.View{{Name: "supervisor"}, resumed, {Name: "bob"}}})
	if len(m.detailChoices) != 0 {
		t.Errorf("choices should clear once the agent is no longer blocked, got %+v", m.detailChoices)
	}
}

func TestBlockedAnswerOptions(t *testing.T) {
	m := newTestModel()
	views := []agent.View{
		{Name: "supervisor", State: agent.StateIdle},
		{Name: "ron", State: agent.StateBlocked, FullMessage: "BLOCKED: Which approach should I take?\n1. Fast path\n2. Robust path"},
		{Name: "bob", State: agent.StateIdle},
	}
	m = feed(m, tea.WindowSizeMsg{Width: 90, Height: 30}, dataMsg{agents: views})
	m = feed(m, key(tea.KeyDown), runes("v"))
	if m.detailAgent != "ron" {
		t.Fatalf("expected detail for ron, got %q", m.detailAgent)
	}
	// the parsed answers come first, before grant/reply/close
	if len(m.detailChoices) < 4 || m.detailChoices[0].kind != "answer" || m.detailChoices[0].answer != "Fast path" {
		t.Fatalf("expected answer options first, got %+v", m.detailChoices)
	}
	// selecting the first answer closes the overlay and sends the choice
	tm, _ := m.Update(key(tea.KeyEnter))
	m = tm.(model)
	if m.detailAgent != "" {
		t.Errorf("overlay should close after answering")
	}
	if !strings.Contains(m.flash, "Fast path") {
		t.Errorf("expected a sent-choice flash, got %q", m.flash)
	}
}

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

func TestDetailOverlayNonBlockedScrollsAndCloses(t *testing.T) {
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
	// non-close keys (e.g. 'x') no longer close — it's a scrollable view now
	m = feed(m, runes("x"))
	if m.detailAgent != "ron" {
		t.Errorf("a non-scroll/non-close key must NOT close the conversation view")
	}
	// esc closes
	m = feed(m, key(tea.KeyEsc))
	if m.detailAgent != "" {
		t.Errorf("esc should close the conversation view")
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

func TestSupervisorMarkdownCached(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 100, Height: 44}, dataMsg{agents: []agent.View{
		{Name: "supervisor", State: agent.StateIdle, Model: "opus",
			FullMessage: "## Digest\n**All good** — `data_sync.py` is up."},
	}})
	if len(m.supLines) == 0 {
		t.Fatalf("supervisor body should be cached after a dataMsg")
	}
	if m.supMsgCache == "" {
		t.Errorf("supMsgCache should record the rendered message")
	}
	panel := m.renderSupervisorPanel()
	// markdown markers should be gone, snake_case preserved.
	if strings.Contains(panel, "##") || strings.Contains(panel, "**") || strings.Contains(panel, "`") {
		t.Errorf("raw markdown leaked into supervisor panel:\n%s", panel)
	}
	if !strings.Contains(panel, "Digest") || !strings.Contains(panel, "data_sync.py") {
		t.Errorf("supervisor panel missing rendered content:\n%s", panel)
	}
}

func TestBackgroundPanelRenders(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 96, Height: 44})
	m.bgProcs = []procscan.Proc{
		{PID: 48213, Command: "jupyter-lab", Agent: "research", Ports: []int{8888}, Uptime: 12 * time.Minute},
		{PID: 49101, Command: "media_encoder", Agent: "worker", Uptime: time.Hour}, // no port
	}
	panel := m.renderBackgroundPanel()
	for _, want := range []string{"SERVICE", "jupyter-lab", ":8888", "research", "media_encoder", "worker", "48213", "up12m"} {
		if !strings.Contains(panel, want) {
			t.Errorf("background panel missing %q\n%s", want, panel)
		}
	}
	// The panel must appear in the main view when there are processes.
	if !strings.Contains(m.mainView(), "BACKGROUND") {
		t.Errorf("mainView should show the BACKGROUND panel when bgProcs is non-empty")
	}
}

// On a wide terminal the BACKGROUND list is a right sidebar spanning the
// banner+agents block, so the AGENTS panel sits right under the banner with no
// gap. Proof: the AGENTS header line is joined beside the background box (it
// contains a box border char), and the agents task column is narrowed to fit.
func TestBackgroundSidebarNoGap(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 150, Height: 60}, dataMsg{agents: []agent.View{
		{Name: "ron", State: agent.StateIdle, Model: "opus", Summary: "did a thing"},
	}})
	var procs []procscan.Proc
	for i := 0; i < 10; i++ {
		procs = append(procs, procscan.Proc{PID: 2000 + i, Command: "python3",
			Args: fmt.Sprintf("python3 svc%d.py", i), Agent: "ron", Uptime: time.Hour})
	}
	m.bgProcs = procs
	out := m.mainView()
	var agentsLine string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "AGENTS") {
			agentsLine = ln
			break
		}
	}
	if agentsLine == "" || !strings.Contains(agentsLine, "│") {
		t.Errorf("expected the AGENTS header to render beside the background sidebar (same line), got:\n%q", agentsLine)
	}
}

// The right-column background list should hold a long run of processes (≥10) and
// only collapse the overflow past the 15-row cap.
func TestBackgroundPanelLongList(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 180, Height: 50})
	var procs []procscan.Proc
	for i := 0; i < 12; i++ {
		procs = append(procs, procscan.Proc{PID: 1000 + i, Command: "python3",
			Args: fmt.Sprintf("python3 worker%d.py", i), Agent: "worker", Uptime: time.Minute})
	}
	m.bgProcs = procs
	panel := m.renderBackgroundPanel()
	if strings.Contains(panel, "more background process") {
		t.Errorf("12 procs (≤15 cap) should not collapse into '+N more'")
	}
	for _, n := range []string{"worker0.py", "worker11.py"} { // friendly name via Display()
		if !strings.Contains(panel, n) {
			t.Errorf("panel missing %q (Display should de-genericize python rows)\n%s", n, panel)
		}
	}
	// 20 procs → cap at 15, remainder collapses.
	m.bgProcs = append(procs, procs...)[:20]
	if !strings.Contains(m.renderBackgroundPanel(), "+5 more") {
		t.Errorf("20 procs should show '+5 more' past the 15 cap")
	}
}

func TestBackgroundPanelHiddenWhenEmpty(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 96, Height: 44})
	if m.bgProcs != nil {
		t.Fatalf("expected no background procs by default")
	}
	if strings.Contains(m.mainView(), "BACKGROUND") {
		t.Errorf("BACKGROUND panel must be hidden when there are no discovered processes")
	}
}

// TestViewClampsToHeight guards the alt-screen ghosting fix: a tall frame (open
// slash palette) on a short terminal must never render more lines than the window
// height, and the interactive footer (status bar) must stay on the last line.
func TestSupervisorPanelAndUsage(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 92, Height: 44}, dataMsg{agents: []agent.View{
		{Name: "supervisor", State: agent.StateIdle, Model: "haiku", FullMessage: "Fleet digest: all good", CostUSD: 0.5, InTokens: 1000, OutTokens: 100},
		{Name: "ron", State: agent.StateIdle, Model: "opus", CostUSD: 1.25},
	}})
	out := m.View()
	if !strings.Contains(out, "SUPERVISOR") {
		t.Errorf("expected a SUPERVISOR panel")
	}
	if strings.Contains(out, "RECENT EVENTS") {
		t.Errorf("RECENT EVENTS panel should be removed")
	}
	if !strings.Contains(out, "Fleet digest: all good") {
		t.Errorf("supervisor panel should show the orchestrator's latest message")
	}
	if !strings.Contains(out, "usage ~$1.75") {
		t.Errorf("welcome box should show the aggregate usage/cost (0.5+1.25), got:\n%s", out)
	}
}

func TestFocusModeIndicator(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 92, Height: 40}, dataMsg{agents: []agent.View{{Name: "supervisor", State: agent.StateIdle}}})
	// default is agents focus → "selecting" hint visible
	if out := m.View(); !strings.Contains(out, "selecting") {
		t.Errorf("agents-focus view should show the 'selecting' indicator")
	}
	// tab → input focus → "tab to select" hint instead
	m = feed(m, key(tea.KeyTab))
	out := m.View()
	if !strings.Contains(out, "tab to select") {
		t.Errorf("input-focus view should prompt 'tab to select'")
	}
	if strings.Contains(out, "selecting · tab to type") {
		t.Errorf("input-focus view should NOT show the agents 'selecting' indicator")
	}
}

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

func TestAgentRemoveAndDKey(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 90, Height: 30})
	// D on the supervisor (selected by default) must refuse, not confirm.
	m = feed(m, runes("D"))
	if m.confirmMsg != "" {
		t.Fatalf("supervisor must not be removable; got confirm %q", m.confirmMsg)
	}
	if !strings.Contains(m.flash, "supervisor") {
		t.Errorf("expected a supervisor-protect warning, got %q", m.flash)
	}
	// Move to ron and press D → a remove confirmation.
	m = feed(m, key(tea.KeyDown), runes("D"))
	if !strings.Contains(m.confirmMsg, "ron") {
		t.Errorf("expected a remove confirm for ron, got %q", m.confirmMsg)
	}
	// /agent remove ron also confirms.
	m = newTestModel()
	m = feed(m, key(tea.KeyTab), runes("/agent remove ron"), key(tea.KeyEnter))
	if !strings.Contains(m.confirmMsg, "ron") {
		t.Errorf("/agent remove should confirm, got %q", m.confirmMsg)
	}
	// /agent add flashes a deploy notice.
	m = newTestModel()
	m = feed(m, key(tea.KeyTab), runes("/agent add carol haiku helper"), key(tea.KeyEnter))
	if !strings.Contains(m.flash, "deploying") {
		t.Errorf("/agent add should flash deploying, got %q", m.flash)
	}
}

func TestToolgenApprovalFlow(t *testing.T) {
	m := newTestModel()
	m = feed(m, tea.WindowSizeMsg{Width: 90, Height: 30})
	m = feed(m, key(tea.KeyDown)) // select ron (worker)
	m = feed(m, key(tea.KeyTab), runes("/toolgen /tmp/scraper.py fetches a url"), key(tea.KeyEnter))
	if !m.registerLoading {
		t.Fatalf("expected registerLoading after /toolgen")
	}
	if m.registerAgent != "ron" || m.registerName != "scraper" {
		t.Fatalf("registerAgent=%q name=%q; want ron/scraper", m.registerAgent, m.registerName)
	}
	// Simulate the async generation result → approval overlay opens.
	m = feed(m, toolDocMsg{name: "scraper", path: "/tmp/scraper.py", doc: "# scraper\n\nUsage: run it to fetch.\n"})
	if m.registerLoading {
		t.Errorf("registerLoading should clear on toolDocMsg")
	}
	if m.registerDoc == "" {
		t.Fatalf("expected the approval overlay to open (registerDoc set)")
	}
	if !strings.Contains(m.registerView(), "Usage: run it to fetch") {
		t.Errorf("approval overlay should show the generated doc")
	}
	// Approving (enter) closes the overlay and kicks off registration.
	tm, _ := m.Update(key(tea.KeyEnter))
	m = tm.(model)
	if m.registerDoc != "" {
		t.Errorf("overlay should close after approval")
	}
	// A generation error surfaces as a flash, not a crash / overlay.
	m2 := newTestModel()
	m2 = feed(m2, tea.WindowSizeMsg{Width: 90, Height: 30}, key(tea.KeyDown))
	m2 = feed(m2, key(tea.KeyTab), runes("/toolgen /tmp/x.py does x"), key(tea.KeyEnter))
	m2 = feed(m2, toolDocMsg{name: "x", path: "/tmp/x.py", err: "boom"})
	if m2.registerDoc != "" || !strings.Contains(m2.flash, "generation failed") {
		t.Errorf("expected a generation-failed flash and no overlay, got flash=%q", m2.flash)
	}
	// A stale toolDocMsg (no generation in flight / wrong path) is ignored.
	m3 := newTestModel()
	m3 = feed(m3, toolDocMsg{name: "z", path: "/tmp/z.py", doc: "# z"})
	if m3.registerDoc != "" {
		t.Errorf("a stale toolDocMsg must not open the overlay")
	}
}

func TestDashboardReloadsConfigOnData(t *testing.T) {
	dir := t.TempDir()
	p := paths.NewProject(dir)
	_ = os.MkdirAll(p.HivemindDir(), 0o755)
	if err := config.Save(p.ConfigPath(), &config.Project{Project: "t", Agents: []config.Agent{{Name: "a"}, {Name: "b"}}}); err != nil {
		t.Fatal(err)
	}
	m := New(p, &config.Project{Project: "t"}, false)
	m = feed(m, dataMsg{})
	if got := strings.Join(m.agentNames, ","); got != "supervisor,a,b" {
		t.Fatalf("agentNames=%q want supervisor,a,b", got)
	}
	// Remove b on disk (as `remove agent` would) and tick again.
	if err := config.Save(p.ConfigPath(), &config.Project{Project: "t", Agents: []config.Agent{{Name: "a"}}}); err != nil {
		t.Fatal(err)
	}
	m = feed(m, dataMsg{})
	if got := strings.Join(m.agentNames, ","); got != "supervisor,a" {
		t.Errorf("after remove, agentNames=%q want supervisor,a", got)
	}
}

func TestConsoleSlashUsageGuard(t *testing.T) {
	m := newTestModel()
	// an invalid /tool action should flash usage, not crash or open a picker
	m = feed(m, key(tea.KeyTab), runes("/tool bogus"), key(tea.KeyEnter))
	if !strings.Contains(m.flash, "usage:") {
		t.Errorf("expected usage flash, got %q", m.flash)
	}
	if m.toolPicking {
		t.Errorf("an invalid action must not open the tool picker")
	}
	// '/tool start' with no service tools configured flashes a clear notice (not usage)
	m = newTestModel()
	m = feed(m, key(tea.KeyTab), runes("/tool start"), key(tea.KeyEnter))
	if m.toolPicking || !strings.Contains(m.flash, "no service tools") {
		t.Errorf("expected a no-service-tools notice, got picking=%v flash=%q", m.toolPicking, m.flash)
	}
}
