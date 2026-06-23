// Package dashboard is the live TUI: a top agents table, a middle tools table,
// and a bottom prompt box bound to the selected agent. It is one Bubble Tea model
// driven by a ticker that re-derives all state from disk each interval (transcript
// liveness + tool health), so it live-refreshes without blocking the prompt box.
package dashboard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/events"
	"hivemind/internal/paths"
	"hivemind/internal/scaffold"
	"hivemind/internal/tools"
)

const refreshInterval = 1500 * time.Millisecond

// flashTTL is how long a status toast stays on screen before auto-dismissing.
// The refresh ticker re-renders every refreshInterval, so it clears on its own.
const flashTTL = 5 * time.Second

// Semantic palette — named roles resolved through one theme (the way Claude Code
// references colors by name, never inline ANSI). Truecolor with a graceful ANSI
// fallback on terminals without 24-bit color. Brand values are Claude Code's own.
var (
	colBrand   = lipgloss.AdaptiveColor{Light: "#D77757", Dark: "#D77757"} // terracotta (brand)
	colAccent  = lipgloss.AdaptiveColor{Light: "#5A6FD8", Dark: "#B1B9F9"} // periwinkle (selection/focus)
	colSuccess = lipgloss.AdaptiveColor{Light: "#2F9E44", Dark: "#4EBA65"}
	colError   = lipgloss.AdaptiveColor{Light: "#E03E52", Dark: "#FF6B80"}
	colWarn    = lipgloss.AdaptiveColor{Light: "#B8860B", Dark: "#FFC107"}
	colDim     = lipgloss.AdaptiveColor{Light: "#888888", Dark: "#999999"}
	colSubtle  = lipgloss.AdaptiveColor{Light: "#AAAAAA", Dark: "#666666"}

	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(colDim)
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colSubtle).Padding(0, 1)
	hintStyle   = lipgloss.NewStyle().Foreground(colSubtle)
	okStyle     = lipgloss.NewStyle().Foreground(colSuccess)
	warnStyle   = lipgloss.NewStyle().Foreground(colWarn)
	errStyle    = lipgloss.NewStyle().Foreground(colError)

	cAmber  = lipgloss.NewStyle().Foreground(colBrand).Bold(true)  // brand (banner, cursor)
	cGreen  = lipgloss.NewStyle().Foreground(colSuccess)           // working / running
	cAccent = lipgloss.NewStyle().Foreground(colAccent).Bold(true) // selection pointer
)

type focus int

const (
	focusAgents focus = iota
	focusInput
)

type model struct {
	p    paths.Project
	cfg  *config.Project
	fake bool

	input textinput.Model
	focus focus

	agentNames   []string
	agentViews   []agent.View   // latest derived agent rows
	toolStatuses []tools.Status // latest tool statuses
	selIdx       int            // selected agent (index into agentNames)
	recentEvents []events.Event
	watcher      *fsnotify.Watcher
	flash        string
	flashAt      time.Time // when flash was set (toast auto-dismiss)
	cmdOut       string    // last console command output
	showHelp     bool
	confirmMsg   string   // destructive-action confirmation overlay
	confirmCmd   tea.Cmd  // action to run on confirm
	palIdx       int      // selected item in the slash-command palette
	history      []string // submitted prompts/commands (↑/↓ recall)
	histIdx      int      // cursor into history (== len when on a fresh draft)

	frame        int                  // spinner animation frame
	spinning     bool                 // a fast spinner tick is scheduled
	workStart    map[string]time.Time // per-agent WORKING start (for elapsed timer)
	reduceMotion bool                 // HIVEMIND_NO_ANIM → static, no animation
	width        int
	height       int

	// tool-assign overlay (press 't' on a selected worker)
	assigning bool
	assignCh  []assignChoice
	assignIdx int

	// agent-detail overlay (press enter/v): full message + permission prompt when
	// blocked. detailAgent == "" means closed.
	detailAgent   string
	detailIdx     int
	detailChoices []permChoice
}

// permChoice is one option in a blocked agent's permission prompt.
type permChoice struct {
	label string
	kind  string   // "grant" | "reply" | "close"
	rules []string // permission rules to grant (kind == "grant")
}

// assignChoice is one option in the tool-assign picker: either attach an existing
// registered tool, or register a script dropped in the agent's workspace.
type assignChoice struct {
	label      string
	existing   string // tool name to attach (existing tool)
	scriptPath string // script to register+attach (new command tool)
	scriptName string
	entrypoint string
}

// palItem is one entry in the slash-command palette (shown while typing "/…").
type palItem struct {
	name    string
	desc    string
	args    bool     // true if the command needs arguments (complete, don't run on Enter)
	hint    string   // inline argument template shown after "/<name> "
	aliases []string // alternate verbs that resolve to this command (e.g. new→reset)
}

var paletteCommands = []palItem{
	{"help", "show all console commands", false, "", []string{"?"}},
	{"delegate", "route a high-level instruction across agents", true, "ask <agent> to <task> and <agent> to <task>", nil},
	{"task", "create one task for a specific agent", true, "<agent> <instruction>", nil},
	{"send", "prompt a specific agent", true, "<agent> <message>", []string{"msg"}},
	{"grant", "grant permission rule(s) to an agent + resume", true, "<agent> <rule...>", nil},
	{"attach", "open an agent's live interactive session", true, "<agent>", nil},
	{"tool", "start | stop | restart a service tool", true, "start|stop|restart <name>", nil},
	{"up", "start the fleet", false, "", nil},
	{"stop", "stop the fleet (keep sessions)", false, "", []string{"halt"}},
	{"report", "wake the supervisor to summarize → ledger", false, "", nil},
	{"reset", "force-stop + reset all sessions, restart fresh", false, "", []string{"new"}},
	{"destroy", "delete the whole project (back to setup)", false, "", []string{"nuke"}},
	{"refresh", "re-read fleet state now", false, "", nil},
	{"quit", "exit the console (fleet keeps running)", false, "", []string{"exit", "q"}},
}

// aliasToCanon maps every alias to its canonical command name (built once).
var aliasToCanon = func() map[string]string {
	m := map[string]string{}
	for _, c := range paletteCommands {
		for _, a := range c.aliases {
			m[a] = c.name
		}
	}
	return m
}()

// matchesPrefix / matchesSub test the typed filter against the command name and
// all its aliases, so "/new" surfaces (and selects) reset.
func (c palItem) matchesPrefix(f string) bool {
	if strings.HasPrefix(c.name, f) {
		return true
	}
	for _, a := range c.aliases {
		if strings.HasPrefix(a, f) {
			return true
		}
	}
	return false
}

func (c palItem) matchesSub(f string) bool {
	if strings.Contains(c.name, f) {
		return true
	}
	for _, a := range c.aliases {
		if strings.Contains(a, f) {
			return true
		}
	}
	return false
}

// aliasNote renders a command's aliases as a dim "· /new" suffix for the palette.
func aliasNote(c palItem) string {
	if len(c.aliases) == 0 {
		return ""
	}
	parts := make([]string, len(c.aliases))
	for i, a := range c.aliases {
		parts[i] = "/" + a
	}
	return "· " + strings.Join(parts, " ")
}

type tickMsg struct{}
type fsMsg struct{}
type quitMsg struct{}            // request to quit (after an exec action)
type cmdMsg struct{ out string } // result of a console command
type dataMsg struct {
	agents []agent.View
	tools  []tools.Status
	events []events.Event
}

// New builds the dashboard model.
func New(p paths.Project, cfg *config.Project, fake bool) model {
	names := []string{config.SupervisorName}
	for _, a := range cfg.Agents {
		names = append(names, a.Name)
	}

	ti := textinput.New()
	ti.Placeholder = "prompt the selected agent, or /help for commands"
	ti.CharLimit = 4000
	ti.Prompt = "❯ "
	ti.PromptStyle = lipgloss.NewStyle().Foreground(colBrand).Bold(true)

	m := model{
		p: p, cfg: cfg, fake: fake,
		input: ti, focus: focusAgents, agentNames: names,
		workStart:    map[string]time.Time{},
		reduceMotion: os.Getenv("HIVEMIND_NO_ANIM") != "",
	}
	// Watch the control dir (events.log + ledger) AND the Claude transcripts dir so
	// the console refreshes the instant a turn writes anything. The ticker still
	// covers tool health, which is not file-change driven.
	if w, err := fsnotify.NewWatcher(); err == nil {
		ok := w.Add(p.HivemindDir()) == nil
		_ = w.Add(paths.ClaudeProjectsDir()) // transcripts (per spec); best-effort
		if ok {
			m.watcher = w
		} else {
			_ = w.Close()
		}
	}
	return m
}

// Close releases the fsnotify watcher; safe on a model that isn't watching.
func (m model) Close() error {
	if m.watcher != nil {
		return m.watcher.Close()
	}
	return nil
}

func clampSel(i, n int) int {
	if n <= 0 || i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{tick(), m.refresh()}
	if m.watcher != nil {
		cmds = append(cmds, watchCmd(m.watcher))
	}
	return tea.Batch(cmds...)
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// The "breathing star" spinner (Claude Code's pulse: a dot grows into a star)
// and a pool of whimsical present-participle verbs, animated on a fast tick that
// runs ONLY while an agent is working (decoupled from the slow data refresh).
var spinnerFrames = []string{"·", "✢", "✦", "✶", "✻", "✽"}
var workingVerbs = []string{"Brewing", "Pondering", "Cooking", "Crunching", "Noodling", "Synthesizing", "Percolating", "Spelunking", "Conjuring", "Ruminating"}

const spinInterval = 130 * time.Millisecond

type spinMsg struct{}

func spinTick() tea.Cmd { return tea.Tick(spinInterval, func(time.Time) tea.Msg { return spinMsg{} }) }

func (m model) anyWorking() bool {
	for _, v := range m.agentViews {
		if v.State == agent.StateWorking {
			return true
		}
	}
	return false
}

func (m model) spin() string { return spinnerFrames[m.frame%len(spinnerFrames)] }

// mmss formats an elapsed duration as "12s" or "1m04s".
func mmss(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// watchCmd blocks until the control dir changes, then asks for a refresh.
func watchCmd(w *fsnotify.Watcher) tea.Cmd {
	return func() tea.Msg {
		select {
		case _, ok := <-w.Events:
			if !ok {
				return nil
			}
			return fsMsg{}
		case _, ok := <-w.Errors:
			if !ok {
				return nil
			}
			return fsMsg{}
		}
	}
}

// refresh gathers fresh state off the UI thread.
func (m model) refresh() tea.Cmd {
	p, cfg := m.p, m.cfg
	names := m.agentNames
	return func() tea.Msg {
		var avs []agent.View
		for _, n := range names {
			avs = append(avs, agent.Observe(p, cfg, n))
		}
		var tss []tools.Status
		for _, t := range cfg.ServiceTools() {
			tss = append(tss, tools.Observe(p, t))
		}
		sort.Slice(tss, func(i, j int) bool { return tss[i].Name < tss[j].Name })
		return dataMsg{agents: avs, tools: tss, events: events.Tail(p.EventsLog(), 6)}
	}
}

func (m model) selectedAgent() string {
	if m.selIdx >= 0 && m.selIdx < len(m.agentNames) {
		return m.agentNames[m.selIdx]
	}
	return config.SupervisorName
}

// setFlash shows a transient status toast and stamps it so View can auto-dismiss
// it after flashTTL. Always go through this (never assign m.flash directly) so the
// timestamp stays in sync — a stale timestamp would hide a fresh message.
func (m *model) setFlash(s string) {
	m.flash = s
	m.flashAt = time.Now()
}

// notice builds a styled toast string with a leading status glyph. kinds:
// "success" ✔, "error" ✘, "warn" ⚠, anything else → info ℹ.
func notice(kind, msg string) string {
	icon, st := "ℹ", lipgloss.NewStyle().Foreground(colAccent)
	switch kind {
	case "success":
		icon, st = "✔", okStyle
	case "error":
		icon, st = "✘", errStyle
	case "warn":
		icon, st = "⚠", warnStyle
	}
	return st.Render(icon + "  " + msg)
}

// isWorking reports whether a named agent is currently mid-turn.
func (m model) isWorking(name string) bool {
	for _, v := range m.agentViews {
		if v.Name == name {
			return v.State == agent.StateWorking
		}
	}
	return false
}

// interruptTurns kills in-flight turns: the selected agent if it's working,
// otherwise every working agent (Claude-Code's "esc to interrupt").
func (m model) interruptTurns() (tea.Model, tea.Cmd) {
	n := 0
	if ag := m.selectedAgent(); m.isWorking(ag) {
		if agent.Interrupt(m.p, ag) {
			n++
		}
	} else {
		for _, v := range m.agentViews {
			if v.State == agent.StateWorking && agent.Interrupt(m.p, v.Name) {
				n++
			}
		}
	}
	if n == 0 {
		m.setFlash(notice("info", "nothing to interrupt"))
	} else {
		m.setFlash(notice("warn", fmt.Sprintf("interrupted %d turn(s)", n)))
	}
	return m, m.refresh()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(tick(), m.refresh())

	case fsMsg:
		return m, tea.Batch(m.refresh(), watchCmd(m.watcher))

	case cmdMsg:
		m.cmdOut = msg.out
		return m, m.refresh()

	case quitMsg:
		return m, tea.Quit

	case dataMsg:
		m.agentViews = msg.agents
		m.toolStatuses = msg.tools
		m.recentEvents = msg.events
		m.selIdx = clampSel(m.selIdx, len(m.agentNames))
		// Track per-agent work-start for the elapsed timer.
		live := map[string]bool{}
		for _, v := range msg.agents {
			if v.State == agent.StateWorking {
				live[v.Name] = true
				if _, ok := m.workStart[v.Name]; !ok {
					m.workStart[v.Name] = time.Now()
				}
			}
		}
		for name := range m.workStart {
			if !live[name] {
				delete(m.workStart, name)
			}
		}
		if m.anyWorking() && !m.spinning && !m.reduceMotion {
			m.spinning = true
			return m, spinTick()
		}
		return m, nil

	case spinMsg:
		m.frame++
		if m.anyWorking() && !m.reduceMotion {
			return m, spinTick() // keep animating while work is in flight
		}
		m.spinning = false
		return m, nil

	case tea.KeyMsg:
		// Agent-detail overlay intercepts keys while open.
		if m.detailAgent != "" {
			if len(m.detailChoices) > 0 { // blocked agent: navigable permission prompt
				switch msg.String() {
				case "ctrl+c":
					return m, tea.Quit
				case "up", "k":
					if m.detailIdx > 0 {
						m.detailIdx--
					}
					return m, nil
				case "down", "j":
					if m.detailIdx < len(m.detailChoices)-1 {
						m.detailIdx++
					}
					return m, nil
				case "esc", "q":
					m.detailAgent = ""
					return m, nil
				case "enter":
					return m.activateDetailChoice()
				}
				if d := digit(msg.String()); d >= 1 && d <= len(m.detailChoices) {
					m.detailIdx = d - 1
					return m.activateDetailChoice()
				}
				return m, nil
			}
			// read-only message view: any key closes (ctrl+c still quits)
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			m.detailAgent = ""
			return m, nil
		}
		// Tool-assign overlay intercepts keys while open.
		if m.assigning {
			switch msg.String() {
			case "up", "k":
				if m.assignIdx > 0 {
					m.assignIdx--
				}
			case "down", "j":
				if m.assignIdx < len(m.assignCh)-1 {
					m.assignIdx++
				}
			case "esc":
				m.assigning = false
			case "enter":
				m = m.performAssign()
			}
			return m, nil
		}
		// Confirmation overlay for destructive actions.
		if m.confirmMsg != "" {
			switch msg.String() {
			case "y", "Y", "enter":
				act := m.confirmCmd
				m.confirmMsg, m.confirmCmd = "", nil
				m.setFlash(notice("info", "working…"))
				return m, act
			default:
				m.confirmMsg, m.confirmCmd = "", nil
				return m, nil
			}
		}
		// Help overlay: any key closes it (ctrl+c still quits).
		if m.showHelp {
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			m.showHelp = false
			return m, nil
		}
		// Slash-command palette: while typing "/<verb>" (no space yet), keys drive
		// the menu — ↑/↓ select, tab completes, enter completes-or-runs, typing filters.
		if m.paletteOpen() {
			matches := m.paletteMatches()
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "up", "ctrl+p":
				if n := len(matches); n > 0 {
					m.palIdx = (m.palIdx - 1 + n) % n
				}
				return m, nil
			case "down", "ctrl+n":
				if n := len(matches); n > 0 {
					m.palIdx = (m.palIdx + 1) % n
				}
				return m, nil
			case "tab":
				if len(matches) > 0 {
					m.completePalette(matches[clampSel(m.palIdx, len(matches))])
				}
				return m, nil
			case "enter":
				if len(matches) > 0 {
					sel := matches[clampSel(m.palIdx, len(matches))]
					m.palIdx = 0
					if sel.args {
						m.completePalette(sel)
						return m, nil
					}
					return m.runSlash("/" + sel.name) // arg-less → run it
				}
				return m.runSlash(m.input.Value()) // no match → surface "unknown command"
			case "esc":
				m.input.SetValue("")
				m.focus = focusAgents
				m.input.Blur()
				m.palIdx = 0
				return m, nil
			default:
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				m.palIdx = 0 // filter changed → reset selection
				return m, cmd
			}
		}
		// Esc interrupts in-flight turns whenever work is running (Claude-Code's
		// "esc to interrupt"), ahead of any focus-specific esc handling.
		if msg.String() == "esc" && m.anyWorking() {
			return m.interruptTurns()
		}
		switch msg.String() {
		case "ctrl+c":
			// Claude Code: first clears a non-empty input, otherwise exits.
			if m.focus == focusInput && m.input.Value() != "" {
				m.input.SetValue("")
				m.histIdx = len(m.history)
				return m, nil
			}
			return m, tea.Quit
		case "ctrl+l":
			return m, tea.ClearScreen // redraw
		case "tab":
			if m.focus == focusAgents {
				m.focus = focusInput
				m.input.Focus()
			} else {
				m.focus = focusAgents
				m.input.Blur()
			}
			return m, nil
		}

		if m.focus == focusInput {
			switch msg.String() {
			case "up", "ctrl+p":
				return m.historyPrev(), nil
			case "down", "ctrl+n":
				return m.historyNext(), nil
			case "enter":
				v := strings.TrimSpace(m.input.Value())
				if v == "" {
					return m, nil
				}
				m.pushHistory(v)
				if strings.HasPrefix(v, "/") {
					return m.runSlash(v) // a console command
				}
				target := m.selectedAgent()
				if err := agent.Dispatch(m.p, target, v, m.fake, ""); err != nil {
					m.setFlash(notice("error", "send failed: "+err.Error()))
				} else {
					m.setFlash(notice("success", "dispatched to "+target))
				}
				m.input.SetValue("")
				return m, m.refresh()
			case "esc":
				m.focus = focusAgents
				m.input.Blur()
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}

		// agents focus (navigation)
		switch msg.String() {
		case "q":
			return m, tea.Quit
		case "up", "k":
			m.selIdx = clampSel(m.selIdx-1, len(m.agentNames))
			return m, nil
		case "down", "j":
			m.selIdx = clampSel(m.selIdx+1, len(m.agentNames))
			return m, nil
		case "enter", "v":
			m.openDetail(m.selectedAgent())
			return m, nil
		case "?":
			m.showHelp = true
			return m, nil
		case "r":
			return m, m.refresh()
		case "X":
			m.confirmMsg = "Force-stop the fleet and reset ALL sessions (config kept)?"
			m.confirmCmd = m.runSelf("reset", "--yes")
			return m, nil
		case "t":
			ag := m.selectedAgent()
			if ag == config.SupervisorName {
				m.setFlash(notice("warn", "select a worker agent to attach a tool"))
				return m, nil
			}
			m.assignCh = m.buildAssignChoices(ag)
			if len(m.assignCh) == 0 {
				m.setFlash(notice("info", "nothing to attach — drop a script in the workspace, or register tools at setup"))
				return m, nil
			}
			m.assigning, m.assignIdx = true, 0
			return m, nil
		}
		return m, nil
	}
	return m, nil
}

func (m model) View() string {
	switch {
	case m.confirmMsg != "":
		return m.clampTop(m.withFlash("\n\n  " + warnStyle.Render("⚠  "+m.confirmMsg) +
			"\n\n  " + hintStyle.Render("y = yes  ·  any other key = cancel")))
	case m.assigning:
		return m.clampTop(m.withFlash(m.assignView()))
	case m.detailAgent != "":
		return m.clampTop(m.withFlash(m.detailView()))
	case m.showHelp:
		return m.clampTop(m.withFlash(m.helpView()))
	default:
		return m.clampBottom(m.mainView()) // mainView renders the flash in its footer
	}
}

// withFlash appends the transient toast beneath overlay content so user feedback
// (e.g. a grant confirmation) survives while an overlay is open — mainView renders
// the flash in its own footer, so this is only for the overlay code paths.
func (m model) withFlash(s string) string {
	if m.flash != "" && time.Since(m.flashAt) < flashTTL {
		return s + "\n\n  " + m.flash
	}
	return s
}

// viewFor returns the latest derived view for a named agent (zero value if absent).
func (m model) viewFor(name string) agent.View {
	for _, v := range m.agentViews {
		if v.Name == name {
			return v
		}
	}
	return agent.View{Name: name}
}

// openDetail opens the agent-detail overlay for name. If that agent is BLOCKED it
// builds a Claude-Code-style permission prompt (grant the requested rules / reply /
// close), best-effort parsing the requested capability from the blocked message.
func (m *model) openDetail(name string) {
	m.detailAgent = name
	m.detailIdx = 0
	m.detailChoices = nil
	v := m.viewFor(name)
	if v.State == agent.StateBlocked && name != config.SupervisorName {
		if rules := parsePermissionRequest(v.FullMessage); len(rules) > 0 {
			m.detailChoices = append(m.detailChoices, permChoice{
				label: "Grant " + strings.Join(rules, ", ") + " and resume " + name,
				kind:  "grant", rules: rules,
			})
		}
		m.detailChoices = append(m.detailChoices,
			permChoice{label: "Reply with guidance instead", kind: "reply"},
			permChoice{label: "Close", kind: "close"},
		)
	}
}

// activateDetailChoice runs the selected permission choice and closes the overlay.
func (m model) activateDetailChoice() (tea.Model, tea.Cmd) {
	ag := m.detailAgent
	m.detailAgent = "" // close regardless of choice
	if m.detailIdx < 0 || m.detailIdx >= len(m.detailChoices) {
		return m, nil
	}
	switch c := m.detailChoices[m.detailIdx]; c.kind {
	case "grant":
		m.setFlash(notice("info", "granting "+strings.Join(c.rules, ", ")+" to "+ag+"…"))
		return m, m.runSelf(append([]string{"grant", ag}, c.rules...)...)
	case "reply":
		m.focus = focusInput
		m.input.Focus()
		m.input.SetValue("")
		m.setFlash(notice("info", "type guidance for "+ag+", then enter"))
		return m, nil
	default: // close
		return m, nil
	}
}

// detailView renders the agent-detail overlay: header + meta, the FULL last message
// word-wrapped (no table truncation), and — when blocked — the permission prompt.
// When there's an actionable prompt, the message body is truncated (not the prompt)
// so the grant choices stay visible even on a short terminal (clampTop keeps the
// TOP, so an un-budgeted long message would otherwise push the choices off-screen).
func (m model) detailView() string {
	v := m.viewFor(m.detailAgent)

	var head strings.Builder
	head.WriteString(titleStyle.Render("  "+v.Name) + "  " + stateColored(v.State) + "\n")
	meta := fmt.Sprintf("model %s · tokens %s↓ %s↑", v.Model, agent.HumanInt(v.InTokens), agent.HumanInt(v.OutTokens))
	if !v.LastActivity.IsZero() {
		meta += " · " + humanAgo(time.Since(v.LastActivity)) + " ago"
	}
	head.WriteString("  " + wDim2(meta) + "\n\n")
	head.WriteString(headerStyle.Render("  MESSAGE") + "\n")

	msg := strings.TrimSpace(v.FullMessage)
	if msg == "" {
		msg = strings.TrimSpace(v.CurrentTask)
	}
	var body []string
	if msg == "" {
		body = []string{"  " + wDim2("(no message yet — the agent hasn't produced text)")}
	} else {
		body = strings.Split(indentLines(wrap(msg, m.msgWidth()), "  "), "\n")
	}

	var foot strings.Builder
	if len(m.detailChoices) > 0 {
		foot.WriteString("\n" + warnStyle.Render("  ⚠ PERMISSION REQUEST") + "\n")
		if req := parsePermissionRequest(v.FullMessage); len(req) > 0 {
			foot.WriteString("  " + wDim2("requested: ") + cAccent.Render(strings.Join(req, ", ")) + "\n")
		}
		foot.WriteString("\n")
		for i, c := range m.detailChoices {
			num := fmt.Sprintf("%d. ", i+1)
			if i == m.detailIdx {
				foot.WriteString(cAccent.Render("  ❯ "+num) + lipgloss.NewStyle().Bold(true).Render(c.label) + "\n")
			} else {
				foot.WriteString("    " + wDim2(num) + c.label + "\n")
			}
		}
		foot.WriteString("\n  " + hintStyle.Render(fmt.Sprintf("↑↓ choose · 1–%d / enter select · esc close", len(m.detailChoices))))
	} else {
		foot.WriteString("\n  " + hintStyle.Render("any key to close"))
	}

	// Budget the message to the terminal height so the footer (the actionable grant
	// prompt, or the close hint) always stays visible and a too-long message
	// truncates with a visible marker instead of being silently top-clipped.
	if m.height > 0 {
		headN := strings.Count(head.String(), "\n")
		footN := strings.Count(foot.String(), "\n") + 1
		// reserve 3: 2 for a possible withFlash toast + 1 for the truncation marker.
		budget := m.height - headN - footN - 3
		if budget < 1 {
			budget = 1
		}
		if len(body) > budget {
			body = append(body[:budget], "  "+wDim2("… (message truncated — widen the terminal to read all)"))
		}
	}
	return head.String() + strings.Join(body, "\n") + "\n" + foot.String()
}

// msgWidth is the wrap width for the detail message (terminal width minus margins).
func (m model) msgWidth() int {
	w := m.width
	if w <= 0 {
		w = 88
	}
	if w -= 4; w < 24 {
		w = 24
	}
	return w
}

// wrap word-wraps s to width w (lipgloss breaks on spaces, hard-breaks long words).
func wrap(s string, w int) string {
	if w < 24 {
		w = 24
	}
	return lipgloss.NewStyle().Width(w).Render(s)
}

// indentLines prefixes every line of s with pad.
func indentLines(s, pad string) string {
	ls := strings.Split(s, "\n")
	for i := range ls {
		ls[i] = pad + ls[i]
	}
	return strings.Join(ls, "\n")
}

// digit returns 1..9 for a single-digit key string, else 0.
func digit(s string) int {
	if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		return int(s[0] - '0')
	}
	return 0
}

var (
	backtickRe = regexp.MustCompile("`([^`]+)`")
	// Only unambiguous compound tool names are matched OUTSIDE backticks — bare
	// "Read"/"Write"/"Edit"/"Task" are everyday English words and would false-
	// positive on prose, so those are only honored inside backticks.
	fallbackToolRe = regexp.MustCompile(`(?i)\b(WebFetch|WebSearch|MultiEdit|NotebookEdit|TodoWrite)\b`)
	// safeGrantTools is the allowlist for ONE-KEYSTROKE auto-grant: Claude built-in
	// tool names that grant a capability but NOT arbitrary shell. Bash(...) and any
	// other raw rule are deliberately EXCLUDED — otherwise an agent's own BLOCKED
	// text could trick the user into one-click granting e.g. `Bash(rm:*)`. Such
	// rules must be granted explicitly with `/grant <agent> <rule>` (user-authored,
	// not parsed from the agent's message).
	safeGrantTools = map[string]string{
		"webfetch": "WebFetch", "websearch": "WebSearch", "read": "Read", "write": "Write",
		"edit": "Edit", "multiedit": "MultiEdit", "glob": "Glob", "grep": "Grep",
		"notebookedit": "NotebookEdit", "task": "Task", "todowrite": "TodoWrite", "ls": "LS",
	}
)

// parsePermissionRequest best-effort extracts the SAFE permission rules an agent
// asked for in a free-form BLOCKED message, for the one-keystroke grant prompt:
// backtick-quoted built-in tool names first (e.g. `WebFetch`), else unambiguous
// compound names in prose. Only names on the safeGrantTools allowlist are returned
// — Bash and arbitrary rules are intentionally NOT auto-grantable (use /grant).
// De-duplicated; may be empty.
func parsePermissionRequest(msg string) []string {
	if msg == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(canon string) {
		if canon != "" && !seen[canon] {
			seen[canon] = true
			out = append(out, canon)
		}
	}
	for _, mtch := range backtickRe.FindAllStringSubmatch(msg, -1) {
		if c, ok := safeGrantTools[strings.ToLower(strings.TrimSpace(mtch[1]))]; ok {
			add(c)
		}
	}
	if len(out) == 0 {
		for _, mtch := range fallbackToolRe.FindAllString(msg, -1) {
			add(safeGrantTools[strings.ToLower(mtch)])
		}
	}
	return out
}

// splitLines splits a rendered frame into lines, ignoring a trailing newline so a
// re-join doesn't grow the frame by one blank row each pass.
func splitLines(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }

// clampBottom keeps the LAST `height` lines of a frame. The interactive footer
// (input, slash palette, status bar) lives at the bottom and so always stays
// visible; the welcome banner up top is dropped first when the terminal is short.
// Clamping every frame to the window height is the fix for alt-screen ghosting:
// Bubble Tea cannot clear rows that scrolled off the top when a frame is taller
// than the terminal, so a long slash palette that then filtered down left its old
// rows stranded on screen. A frame that never exceeds the height never ghosts.
func (m model) clampBottom(s string) string {
	if m.height <= 0 {
		return s // size not yet known (no WindowSizeMsg); render unclamped
	}
	ls := splitLines(s)
	if len(ls) > m.height {
		ls = ls[len(ls)-m.height:]
	}
	return strings.Join(ls, "\n")
}

// clampTop keeps the FIRST `height` lines — used for overlays (confirm/assign/
// help), which read top-down so their title must stay visible.
func (m model) clampTop(s string) string {
	if m.height <= 0 {
		return s
	}
	ls := splitLines(s)
	if len(ls) > m.height {
		ls = ls[:m.height]
	}
	return strings.Join(ls, "\n")
}

// mainView renders the full console frame: header panels (welcome banner, agents,
// tools, recent events) stacked above the interactive footer (rule, input, slash
// palette, status bar). View height-clamps the result via clampBottom.
func (m model) mainView() string {
	var b strings.Builder
	b.WriteString(m.welcomeBox())
	b.WriteString("\n\n")

	b.WriteString(headerStyle.Render("  AGENTS"))
	b.WriteString("\n")
	b.WriteString(boxStyle.Render(m.renderAgentsPanel()))
	b.WriteString("\n\n")

	b.WriteString(headerStyle.Render("  TOOLS"))
	b.WriteString("\n")
	if len(m.cfg.ServiceTools()) == 0 {
		b.WriteString(hintStyle.Render("  (no service tools)"))
	} else {
		b.WriteString(boxStyle.Render(m.renderToolsPanel()))
	}
	b.WriteString("\n\n")

	// Hide the events/output panes while the command palette is open, so it has
	// room near the input (like Claude Code's slash menu).
	if !m.paletteOpen() {
		b.WriteString(headerStyle.Render("  RECENT EVENTS"))
		b.WriteString("\n")
		if len(m.recentEvents) == 0 {
			b.WriteString(hintStyle.Render("  (none yet — emitted by the Stop hook on real claude turns)"))
		} else {
			var lines []string
			for _, e := range m.recentEvents {
				flag := " "
				if e.Blocked {
					flag = warnStyle.Render("⚠")
				} else if e.Errored {
					flag = errStyle.Render("✗")
				}
				lines = append(lines, fmt.Sprintf("%s %s %-11s i:%-5s o:%-5s %s",
					e.TS.Local().Format("15:04:05"), flag, e.Agent,
					agent.HumanInt(e.InTokens), agent.HumanInt(e.OutTokens), truncate(e.Summary, 40)))
			}
			b.WriteString(boxStyle.Render(strings.Join(lines, "\n")))
		}
		b.WriteString("\n\n")

		if m.cmdOut != "" {
			b.WriteString(headerStyle.Render("  CONSOLE OUTPUT"))
			b.WriteString("\n")
			b.WriteString(boxStyle.Render(lastLines(m.cmdOut, 6)))
			b.WriteString("\n\n")
		}
	}

	b.WriteString(m.rule() + "\n")
	inputLine := "  " + hintStyle.Render(m.selectedAgent()) + " " + m.input.View()
	if h := m.argHint(); h != "" {
		inputLine += "  " + wDim2(h) // dim argument template, inline
	}
	b.WriteString(inputLine + "\n")
	if m.paletteOpen() {
		b.WriteString(m.paletteView())
	}
	if m.flash != "" && time.Since(m.flashAt) < flashTTL {
		b.WriteString("  " + m.flash + "\n")
	}
	b.WriteString(m.statusBar() + "\n")
	return b.String()
}

// --- welcome box + colored panels (custom-rendered for full color control;
// the bubbles table cannot safely color cells without breaking width math) ---

// welcomeBox renders the Claude-Code-style header: a rounded box with a left
// identity column and a right quick-start/fleet column, split by a divider.
func (m model) welcomeBox() string {
	working, inTot, outTot := 0, 0, 0
	for _, v := range m.agentViews {
		if v.State == agent.StateWorking {
			working++
		}
		inTot += v.InTokens
		outTot += v.OutTokens
	}
	up := 0
	for _, s := range m.toolStatuses {
		if s.State == tools.StateRunning {
			up++
		}
	}
	fakeTag := ""
	if m.fake {
		fakeTag = "  " + warnStyle.Render("[fake]")
	}
	// Keep every left-column line ≤ 31 cells so the box never wraps.
	workLine := fmt.Sprintf("%d agents · %s", len(m.agentNames), cGreen.Render(fmt.Sprintf("%d working", working)))
	if working > 0 && !m.reduceMotion {
		verb := workingVerbs[(m.frame/8)%len(workingVerbs)]
		workLine = cGreen.Render(fmt.Sprintf("%s %d/%d · %s…", m.spin(), working, len(m.agentNames), verb))
	}
	left := strings.Join([]string{
		cAmber.Render("█░█ █ █░█ █▀▀ █▀▄▀█ █ █▄░█ █▀▄"),
		cAmber.Render("█▀█ █ ▀▄▀ ██▄ █░▀░█ █ █░▀█ █▄▀"),
		"",
		cAmber.Render(truncate(m.cfg.Project, 14)) + wDim2("  ·  "+truncate(m.cfg.Defaults.Model, 8)+fakeTag),
		workLine,
		wDim2(shortenPath(m.p.Root)),
	}, "\n")
	right := strings.Join([]string{
		headerStyle.Render("Getting started"),
		wDim2("type → prompt the selected agent"),
		wDim2("enter → read full msg · grant perms"),
		wDim2("/help  ·  X reset  ·  q quit"),
		"",
		wDim2(fmt.Sprintf("fleet: %d/%d tools up · tok %s↓ %s↑", up, len(m.cfg.ServiceTools()), agent.HumanInt(inTot), agent.HumanInt(outTot))),
	}, "\n")
	leftCol := lipgloss.NewStyle().Width(31).Render(left)
	rightCol := lipgloss.NewStyle().Width(38).Render(right)
	body := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "  ", dividerCol(6), "  ", rightCol)
	return titledBox(" "+cAmber.Render("✻ hivemind")+" ", body, colBrand)
}

func dividerCol(n int) string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = wDim2("│")
	}
	return strings.Join(rows, "\n")
}

// statusBar is the Claude-Code-style bottom line: shortcuts left, fleet indicator
// right. While any turn is running it leads with a highlighted "esc interrupt".
func (m model) statusBar() string {
	w := m.width
	if w < 40 {
		w = 96
	}
	hint := "? shortcuts · tab focus · ↑↓ history/select · ^L redraw · X reset · q quit"
	leftPlain, leftStyled := hint, wDim2(hint)
	if m.anyWorking() {
		leftPlain = "esc interrupt · " + hint
		leftStyled = warnStyle.Render("esc interrupt") + wDim2(" · "+hint)
	}
	right := "● " + m.selectedAgent()
	if m.fake {
		right += " · fake"
	}
	gap := w - lipgloss.Width(leftPlain) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return "  " + leftStyled + strings.Repeat(" ", gap) + wDim2(right)
}

func (m model) rule() string {
	w := m.width
	if w < 10 {
		w = 96
	}
	return wDim2(strings.Repeat("─", w))
}

// shortenPath collapses a path to ~ and middle-truncates long ones (keeping the
// first + last segment), like Claude Code's ADe() cwd renderer.
func shortenPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		p = "~" + p[len(home):]
	}
	if lipgloss.Width(p) <= 28 {
		return p
	}
	if parts := strings.Split(p, "/"); len(parts) > 3 {
		first := parts[0]
		if first == "" {
			first = "/" + parts[1]
		}
		p = first + "/…/" + parts[len(parts)-1]
	}
	return p
}

// titledBox draws a rounded box whose title is embedded in the top border (like
// Claude Code's borderText), since lipgloss has no native titled border.
func titledBox(title, body string, border lipgloss.TerminalColor) string {
	bs := lipgloss.NewStyle().Foreground(border)
	lines := strings.Split(body, "\n")
	w := 0
	for _, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			w = lw
		}
	}
	inner := w + 2 // 1 space padding each side
	tW := lipgloss.Width(title)
	right := inner - 2 - tW
	if right < 1 {
		right = 1
	}
	var b strings.Builder
	b.WriteString(bs.Render("╭──") + title + bs.Render(strings.Repeat("─", right)+"╮") + "\n")
	for _, l := range lines {
		b.WriteString(bs.Render("│") + " " + l + strings.Repeat(" ", w-lipgloss.Width(l)) + " " + bs.Render("│") + "\n")
	}
	b.WriteString(bs.Render("╰" + strings.Repeat("─", inner) + "╯"))
	return b.String()
}

func (m model) renderAgentsPanel() string {
	var b strings.Builder
	b.WriteString(wDim2(fmt.Sprintf("  %-12s%-10s%-7s%-34s%-8s%-12s",
		"AGENT", "STATE", "LAST", "CURRENT / LAST TASK", "MODEL", "TOKENS i/o")))
	if len(m.agentViews) == 0 {
		b.WriteString("\n" + hintStyle.Render("  loading…"))
		return b.String()
	}
	for i, v := range m.agentViews {
		cur := "  "
		name := padCell(truncate(v.Name, 12), 12)
		if i == m.selIdx {
			cur = cAccent.Render("❯ ") // periwinkle selection pointer (Claude-style)
			name = lipgloss.NewStyle().Bold(true).Render(name)
		}
		// STATE cell: animate WORKING with the breathing-star spinner.
		stateCell := stateColored(padCell(v.State, 10))
		last := "—"
		if v.State == agent.StateWorking {
			if !m.reduceMotion {
				stateCell = cGreen.Render(m.spin() + " " + padCell("WORKING", 8))
			}
			if st, ok := m.workStart[v.Name]; ok {
				last = mmss(time.Since(st)) // elapsed since work began
			}
		} else if !v.LastActivity.IsZero() {
			last = humanAgo(time.Since(v.LastActivity))
		}
		io := "—"
		if v.InTokens > 0 || v.OutTokens > 0 {
			io = agent.HumanInt(v.InTokens) + "/" + agent.HumanInt(v.OutTokens)
		}
		task := v.CurrentTask
		if v.Summary != "" {
			task = v.Summary
		}
		b.WriteString("\n" + cur + name + stateCell +
			padCell(last, 7) + padCell(truncate(task, 33), 34) +
			padCell(truncate(v.Model, 7), 8) + io)
	}
	return b.String()
}

func (m model) renderToolsPanel() string {
	var b strings.Builder
	b.WriteString(wDim2(fmt.Sprintf("  %-16s%-12s%-11s%-10s%-8s",
		"TOOL", "OWNER", "STATUS", "UPTIME", "PORT")))
	for _, s := range m.toolStatuses {
		up := "—"
		if s.State != tools.StateStopped {
			up = humanAgo(s.Uptime)
		}
		port := "—"
		if len(s.Ports) > 0 {
			ps := make([]string, len(s.Ports))
			for i, p := range s.Ports {
				ps[i] = fmt.Sprintf("%d", p)
			}
			port = strings.Join(ps, ",")
		}
		b.WriteString("\n  " + padCell(truncate(s.Name, 16), 16) + padCell(truncate(s.Owner, 12), 12) +
			statusColored(padCell(s.State, 11)) + padCell(up, 10) + port)
	}
	return b.String()
}

func stateColored(s string) string {
	switch strings.TrimSpace(s) {
	case agent.StateWorking:
		return cGreen.Render(s)
	case agent.StateError:
		return errStyle.Render(s)
	case agent.StateBlocked:
		return warnStyle.Render(s)
	case agent.StateIdle:
		return wDim2(s)
	default:
		return hintStyle.Render(s) // NEW / UNKNOWN
	}
}

func statusColored(s string) string {
	switch strings.TrimSpace(s) {
	case tools.StateRunning:
		return cGreen.Render(s)
	case tools.StateUnhealthy:
		return warnStyle.Render(s)
	case tools.StateStopped:
		return errStyle.Render(s)
	default:
		return s
	}
}

// padCell right-pads to a display width (ansi-aware) so colored/multibyte cells
// stay aligned.
func padCell(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// pushHistory records a submitted command/prompt (skips consecutive dupes).
func (m *model) pushHistory(v string) {
	if v == "" {
		return
	}
	if n := len(m.history); n == 0 || m.history[n-1] != v {
		m.history = append(m.history, v)
	}
	m.histIdx = len(m.history)
}

// historyPrev / historyNext recall earlier / later inputs (Claude-Code ↑/↓).
func (m model) historyPrev() model {
	if len(m.history) == 0 {
		return m
	}
	if m.histIdx > 0 {
		m.histIdx--
	}
	m.input.SetValue(m.history[m.histIdx])
	m.input.CursorEnd()
	return m
}

func (m model) historyNext() model {
	if len(m.history) == 0 {
		return m
	}
	if m.histIdx < len(m.history)-1 {
		m.histIdx++
		m.input.SetValue(m.history[m.histIdx])
	} else {
		m.histIdx = len(m.history)
		m.input.SetValue("")
	}
	m.input.CursorEnd()
	return m
}

// argHint returns the inline argument template to show after "/<verb> " once a
// command is chosen but no args are typed yet (Claude Code's dim argumentHint).
func (m model) argHint() string {
	if m.focus != focusInput {
		return ""
	}
	v := m.input.Value()
	rest, ok := strings.CutPrefix(v, "/")
	if !ok {
		return ""
	}
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return "" // still choosing the verb (the palette handles this)
	}
	if strings.TrimSpace(rest[sp+1:]) != "" {
		return "" // user is already typing args
	}
	verb := rest[:sp]
	for _, c := range paletteCommands {
		if c.name == verb {
			return c.hint
		}
	}
	return ""
}

// paletteOpen reports whether the slash-command menu should be active: the input
// is focused and holds "/<verb>" with no space yet (still choosing a command).
func (m model) paletteOpen() bool {
	if m.focus != focusInput {
		return false
	}
	v := m.input.Value()
	return strings.HasPrefix(v, "/") && !strings.Contains(v, " ")
}

// paletteMatches returns commands matching the typed filter: prefix matches first,
// then substring matches.
func (m model) paletteMatches() []palItem {
	f := strings.ToLower(strings.TrimPrefix(m.input.Value(), "/"))
	var pre, sub []palItem
	for _, c := range paletteCommands {
		switch {
		case f == "" || c.matchesPrefix(f):
			pre = append(pre, c)
		case c.matchesSub(f):
			sub = append(sub, c)
		}
	}
	return append(pre, sub...)
}

func (m *model) completePalette(c palItem) {
	m.input.SetValue("/" + c.name + " ")
	m.input.CursorEnd()
	m.palIdx = 0
}

// paletteRows derives how many command rows fit, scaling with the terminal height
// and clamped to a sane [4,12] band (10 if the height is unknown).
func paletteRows(height, n int) int {
	c := 10
	if height > 0 {
		c = height / 3
	}
	if c > 12 {
		c = 12
	}
	if c < 4 {
		c = 4
	}
	if c > n {
		c = n
	}
	return c
}

// paletteView renders the command dropdown under the input, scrolling a window
// that keeps the selected row near the middle so long, filtered lists stay usable.
func (m model) paletteView() string {
	matches := m.paletteMatches()
	if len(matches) == 0 {
		return "  " + wDim2("no matching command") + "\n"
	}
	sel := clampSel(m.palIdx, len(matches))
	rows := paletteRows(m.height, len(matches))
	// Center the window on the selection, then clamp to the list bounds.
	start := sel - rows/2
	if max := len(matches) - rows; start > max {
		start = max
	}
	if start < 0 {
		start = 0
	}
	end := start + rows

	var b strings.Builder
	if start > 0 {
		b.WriteString("    " + wDim2(fmt.Sprintf("↑ %d more", start)) + "\n")
	}
	for i := start; i < end; i++ {
		c := matches[i]
		name := padCell("/"+c.name, 14)
		desc := wDim2(truncate(c.desc, 48))
		if note := aliasNote(c); note != "" {
			desc += " " + wDim2(note)
		}
		if i == sel {
			b.WriteString(cAmber.Render("  ❯ ") + lipgloss.NewStyle().Bold(true).Render(name) + " " + desc + "\n")
		} else {
			b.WriteString("    " + name + " " + desc + "\n")
		}
	}
	if end < len(matches) {
		b.WriteString("    " + wDim2(fmt.Sprintf("↓ %d more", len(matches)-end)) + "\n")
	}
	b.WriteString("  " + wDim2("↑↓ select · tab complete · enter run/complete · esc cancel"))
	return b.String() + "\n"
}

// runSlash parses and executes a console command typed in the prompt box.
// Plain text (no leading '/') is sent to the selected agent; '/'-commands manage
// the fleet. Long-running commands run as a subprocess of this binary and their
// output is shown in the console pane, so the UI stays responsive.
func (m model) runSlash(line string) (tea.Model, tea.Cmd) {
	m.input.SetValue("")
	body := strings.TrimSpace(strings.TrimPrefix(line, "/"))
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return m, nil
	}
	verb := fields[0]
	if canon, ok := aliasToCanon[verb]; ok {
		verb = canon // resolve /new → reset, /q → quit, /? → help, …
	}
	switch verb {
	case "help":
		m.showHelp = true
		return m, nil
	case "quit":
		return m, tea.Quit
	case "refresh":
		return m, m.refresh()
	case "up", "down", "report":
		return m, m.runSelf(verb)
	case "stop":
		return m, m.runSelf("down")
	case "reset":
		m.confirmMsg = "Force-stop the fleet and reset ALL sessions (config kept)?"
		m.confirmCmd = m.runSelf("reset", "--yes")
		return m, nil
	case "destroy":
		m.confirmMsg = "DELETE this project entirely (workspaces + config)? This cannot be undone."
		m.confirmCmd = m.execAndQuit("clean", "--purge", "--yes")
		return m, nil
	case "attach":
		if len(fields) < 2 {
			m.setFlash(notice("warn", "usage: /attach <agent>"))
			return m, nil
		}
		return m, m.execAttach(fields[1])
	case "tool":
		if len(fields) < 3 {
			m.setFlash(notice("warn", "usage: /tool start|stop|restart <name>"))
			return m, nil
		}
		return m, m.runSelf("tool", fields[1], fields[2])
	case "task":
		if len(fields) < 3 {
			m.setFlash(notice("warn", "usage: /task <agent> <prompt>"))
			return m, nil
		}
		return m, m.runSelf("task", fields[1], strings.Join(fields[2:], " "))
	case "send":
		if len(fields) < 3 {
			m.setFlash(notice("warn", "usage: /send <agent> <prompt>"))
			return m, nil
		}
		target := fields[1]
		if err := agent.Dispatch(m.p, target, strings.Join(fields[2:], " "), m.fake, ""); err != nil {
			m.setFlash(notice("error", "send failed: "+err.Error()))
		} else {
			m.setFlash(notice("success", "dispatched to "+target))
		}
		return m, m.refresh()
	case "grant":
		if len(fields) < 3 {
			m.setFlash(notice("warn", "usage: /grant <agent> <rule...>  (e.g. /grant romeo WebFetch WebSearch)"))
			return m, nil
		}
		m.setFlash(notice("info", "granting "+strings.Join(fields[2:], ", ")+" to "+fields[1]+"…"))
		return m, m.runSelf(append([]string{"grant"}, fields[1:]...)...)
	case "delegate":
		instr := strings.TrimSpace(strings.TrimPrefix(body, "delegate"))
		if instr == "" {
			m.setFlash(notice("warn", "usage: /delegate <instruction>"))
			return m, nil
		}
		m.setFlash(notice("info", "delegating…"))
		return m, m.runSelf("delegate", instr)
	default:
		m.setFlash(notice("warn", "unknown command /"+verb+" — type /help"))
		return m, nil
	}
}

// runSelf runs `hivemind [--root R] [--fake] <args...>` as a subprocess and routes
// its output to the console pane (off the UI thread).
func (m model) runSelf(args ...string) tea.Cmd {
	root, fake := m.p.Root, m.fake
	return func() tea.Msg {
		exe, err := os.Executable()
		if err != nil {
			return cmdMsg{out: err.Error()}
		}
		g := []string{"--root", root}
		if fake {
			g = append(g, "--fake")
		}
		out, _ := exec.Command(exe, append(g, args...)...).CombinedOutput()
		return cmdMsg{out: strings.TrimSpace(string(out))}
	}
}

// execAndQuit runs a hivemind subcommand to completion (suspending the TUI), then
// quits — used for /destroy, after which the project no longer exists.
func (m model) execAndQuit(args ...string) tea.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return cmdMsg{out: err.Error()} }
	}
	c := exec.Command(exe, append([]string{"--root", m.p.Root}, args...)...)
	return tea.ExecProcess(c, func(error) tea.Msg { return quitMsg{} })
}

// execAttach suspends the TUI, runs an interactive `hivemind attach <agent>`
// (which execs claude --resume), and restores the TUI when it exits.
func (m model) execAttach(ag string) tea.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return cmdMsg{out: err.Error()} }
	}
	c := exec.Command(exe, "--root", m.p.Root, "attach", ag)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return cmdMsg{out: "attach " + ag + " ended: " + err.Error()}
		}
		return cmdMsg{out: "returned from " + ag}
	})
}

// helpView lists the console commands.
func (m model) helpView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("  hivemind console — commands"))
	b.WriteString("\n\n")
	rows := [][2]string{
		{"<text>", "send to the selected agent"},
		{"/send <agent> <prompt>", "prompt a specific agent"},
		{"/grant <agent> <rule>", "grant a permission (e.g. WebFetch) + resume"},
		{"/delegate <instruction>", "route work across agents (supervisor decomposes)"},
		{"/task <agent> <prompt>", "create one task for an agent"},
		{"/attach <agent>", "open the live interactive session"},
		{"/tool start|stop|restart <n>", "manage a service tool"},
		{"/up  /stop  /report", "start / stop the fleet · supervisor digest"},
		{"/reset", "force-stop + reset all sessions, restart fresh (key: X)"},
		{"/destroy", "delete the whole project (back to setup)"},
		{"/refresh  /help  /quit", "console controls (quit leaves the fleet running)"},
	}
	for _, r := range rows {
		b.WriteString("  " + wKey(r[0]) + "  " + wDim2(r[1]) + "\n")
	}
	b.WriteString("\n  " + headerStyle.Render("Keyboard") + "\n")
	for _, r := range [][2]string{
		{"tab", "switch focus: agents ↔ command bar"},
		{"enter / v", "view an agent's full message · grant if it's BLOCKED"},
		{"↑ / ↓", "command history (in bar) · select agent (in list)"},
		{"^P / ^N", "command history"},
		{"esc", "interrupt running turn(s) · else unfocus the bar"},
		{"t", "attach a tool to the selected agent"},
		{"X", "reset (force-stop + fresh sessions)"},
		{"^L", "redraw  ·  ^C clear input / exit  ·  q quit"},
	} {
		b.WriteString("  " + wKey(r[0]) + "  " + wDim2(r[1]) + "\n")
	}
	b.WriteString("\n  " + hintStyle.Render("(any key to close)"))
	return b.String()
}

func wKey(s string) string {
	return lipgloss.NewStyle().Foreground(colAccent).Render(fmt.Sprintf("%-30s", s))
}
func wDim2(s string) string { return lipgloss.NewStyle().Foreground(colDim).Render(s) }

// assignView renders the tool-assign picker overlay.
func (m model) assignView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("  attach a tool"))
	b.WriteString("  " + hintStyle.Render("→ "+m.selectedAgent()))
	b.WriteString("\n\n")
	for i, c := range m.assignCh {
		if i == m.assignIdx {
			b.WriteString(okStyle.Render("  › "+c.label) + "\n")
		} else {
			b.WriteString("    " + c.label + "\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("  ↑/↓ choose · enter attach (regenerates CLAUDE.md) · esc cancel\n"))
	return b.String()
}

// buildAssignChoices lists attachable existing tools + scripts found in the
// agent's workspace (registered as command tools on selection).
func (m model) buildAssignChoices(ag string) []assignChoice {
	var out []assignChoice
	attached := map[string]bool{}
	a := m.cfg.FindAgent(ag)
	if a != nil {
		for _, t := range a.Tools {
			attached[t] = true
		}
	}
	for _, t := range m.cfg.Tools {
		if !attached[t.Name] {
			out = append(out, assignChoice{label: fmt.Sprintf("attach tool: %s (%s)", t.Name, t.Type), existing: t.Name})
		}
	}
	if a != nil {
		for _, f := range candidateScripts(m.p.WorkspaceDir(a.Workspace)) {
			ep := inferEntrypoint(f)
			name := toolNameFromFile(f)
			if m.cfg.FindTool(name) != nil {
				continue // already a tool of that name
			}
			out = append(out, assignChoice{
				label:      fmt.Sprintf("register script: %s → %s", filepath.Base(f), ep),
				scriptPath: f, scriptName: name, entrypoint: ep,
			})
		}
	}
	return out
}

// performAssign attaches the selected choice (existing tool or new script-tool).
func (m model) performAssign() model {
	ag := m.selectedAgent()
	c := m.assignCh[m.assignIdx]
	var err error
	name := c.existing
	if c.existing != "" {
		err = scaffold.AttachTool(m.p, m.cfg, c.existing, ag)
	} else {
		name = c.scriptName
		t := config.Tool{Name: c.scriptName, Type: config.ToolCommand, Entrypoint: c.entrypoint}
		doc := fmt.Sprintf("# %s\n\nRun via `%s`.\n", c.scriptName, c.entrypoint)
		if err = scaffold.RegisterTool(m.p, m.cfg, t, c.scriptPath, doc); err == nil {
			err = scaffold.AttachTool(m.p, m.cfg, c.scriptName, ag)
		}
	}
	m.assigning = false
	if err != nil {
		m.setFlash(notice("error", "attach failed: "+err.Error()))
	} else {
		m.setFlash(notice("success", fmt.Sprintf("attached %s to %s (CLAUDE.md regenerated)", name, ag)))
	}
	return m
}

var scriptExt = map[string]bool{".py": true, ".sh": true, ".js": true, ".ts": true, ".rb": true, ".go": true, ".pl": true}

func candidateScripts(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if scriptExt[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func inferEntrypoint(f string) string {
	b := filepath.Base(f)
	switch strings.ToLower(filepath.Ext(f)) {
	case ".py":
		return "python3 " + b
	case ".sh":
		return "bash " + b
	case ".js":
		return "node " + b
	case ".ts":
		return "npx ts-node " + b
	case ".rb":
		return "ruby " + b
	case ".pl":
		return "perl " + b
	default:
		return "./" + b
	}
}

// lastLines returns the last n non-empty lines of s (for the console pane).
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func toolNameFromFile(f string) string {
	n := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
	var b strings.Builder
	for _, r := range n { // sanitize → a safe tool/dir name
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

func humanAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > n { // rune boundary; the table also rune-truncates
		return string(r[:n-1]) + "…"
	}
	return s
}
