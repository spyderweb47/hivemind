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
	"hivemind/internal/procscan"
	"hivemind/internal/scaffold"
	"hivemind/internal/tools"
	"hivemind/internal/transcript"
)

const refreshInterval = 1500 * time.Millisecond

// procScanInterval is the (slower) cadence for auto-discovering agent-spawned
// background processes. It is decoupled from refreshInterval because the scan
// shells out to lsof/ps (macOS) or walks /proc (Linux) — heavier than the
// transcript-liveness refresh, and listeners rarely change between ticks.
const procScanInterval = 4 * time.Second

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
	boxActive   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(0, 1) // focused panel
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
	agentViews   []agent.View    // latest derived agent rows
	toolStatuses []tools.Status  // latest tool statuses
	bgProcs      []procscan.Proc // auto-discovered agent-spawned background processes
	bgScanAt     time.Time       // when the last background scan completed

	// Cached supervisor-panel body (markdown is expensive to re-render every frame,
	// and the spinner re-renders at 130ms; recomputed only when the message/width
	// changes — see recacheSupervisor).
	supLines      []string
	supMsgCache   string
	supWidthCache int
	selIdx        int // selected agent (index into agentNames)
	recentEvents  []events.Event
	watcher       *fsnotify.Watcher
	flash         string
	flashAt       time.Time // when flash was set (toast auto-dismiss)
	cmdOut        string    // last console command output
	showHelp      bool
	confirmMsg    string   // destructive-action confirmation overlay
	confirmCmd    tea.Cmd  // action to run on confirm
	palIdx        int      // selected item in the slash-command palette
	history       []string // submitted prompts/commands (↑/↓ recall)
	histIdx       int      // cursor into history (== len when on a fresh draft)

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

	// agent-detail overlay (press enter/v): scrollable recent conversation + a
	// permission prompt when blocked. detailAgent == "" means closed.
	detailAgent        string
	detailIdx          int
	detailChoices      []permChoice
	detailItems        []transcript.Item // recent conversation (all chunks), snapshot on open
	detailLines        []string          // rendered+wrapped conversation lines (cached)
	detailLoading      bool              // conversation is being read off-thread
	detailScrollOffset int               // lines scrolled in the conversation view

	// smart-tool-register flow (/toolgen): pick a file from the selected agent's
	// workspace → describe its intent → Claude writes a TOOL.md → approve → register.
	picking         bool     // file-picker overlay open
	pickAgent       string   // whose workspace we're picking from
	pickFiles       []string // candidate files in that workspace
	pickIdx         int      // selected file (into the filtered list)
	pickFilter      string   // type-to-filter text (autocomplete)
	awaitingIntent  bool     // input bar is collecting the tool's intent
	registerLoading bool
	registerDoc     string
	registerPath    string
	registerAbout   string
	registerAgent   string
	registerName    string

	// service-tool action picker (/tool start|stop|restart with no name): pick a
	// tool from a filterable list showing each tool's live status.
	toolPicking bool
	toolAction  string // start | stop | restart
	toolIdx     int
	toolFilter  string

	// tool-type choice in the /toolgen flow (command vs service), shown after the
	// file is picked and before the intent is collected.
	registerTypeChoice bool
	registerType       string // "command" | "service"
	registerTypeIdx    int
}

const defaultPlaceholder = "prompt the selected agent, or /help for commands"

// permChoice is one option in a blocked agent's permission prompt.
type permChoice struct {
	label  string
	kind   string   // "grant" | "answer" | "reply" | "close"
	rules  []string // permission rules to grant (kind == "grant")
	answer string   // text dispatched to the agent (kind == "answer")
}

// optionRe matches a numbered option line like "1. Use Postgres" or "2) SQLite"
// (only the first line of a multi-line option is captured — keep options one-line).
var optionRe = regexp.MustCompile(`(?m)^\s*(\d{1,2})[.)]\s+(.+?)\s*$`)

// decisionRe gates option parsing to messages that actually pose a choice, so an
// incidental numbered list in a blocked message (steps already done, code) isn't
// mistaken for selectable answers.
var decisionRe = regexp.MustCompile(`(?i)(\bwhich\b|\bchoose\b|\bselect\b|\boption\b|\bprefer\b|\bapprove\b|\bpick\b|\bdecide\b|\?)`)

// parseOptions extracts the numbered options an agent offered for a decision. It
// only fires when the message poses a choice (decisionRe). Deduped, capped at 9.
func parseOptions(msg string) []string {
	if msg == "" || !decisionRe.MatchString(msg) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, mtch := range optionRe.FindAllStringSubmatch(msg, -1) {
		t := strings.TrimSpace(mtch[2])
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= 9 {
			break
		}
	}
	return out
}

// toolTypeChoices are the options in the /toolgen command-vs-service step.
var toolTypeChoices = []struct{ kind, label, hint string }{
	{"command", "command", "the agent invokes it per task (no persistent process)"},
	{"service", "service", "long-running — hivemind starts & health-checks it; shows RUNNING"},
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
	{"agent", "add or remove an agent (deploy / destroy)", true, "add <name> [model] [role] | remove <name>", nil},
	{"supervisor", "set the supervisor (orchestrator) model", true, "<model>", []string{"super"}},
	{"permission", "set an agent's permission mode (e.g. bypassPermissions)", true, "<agent> <mode>", []string{"perm"}},
	{"toolgen", "register a tool — pick a workspace file, Claude writes its docs", false, "", []string{"learn"}},
	{"attach", "open an agent's live interactive session", true, "<agent>", nil},
	{"tool", "start | stop | restart a service tool (pick from a list)", true, "start|stop|restart [name]", nil},
	{"up", "start the fleet", false, "", nil},
	{"stop", "stop the fleet (keep sessions)", false, "", []string{"halt"}},
	{"report", "wake the supervisor to summarize → ledger", false, "", nil},
	{"usage", "token usage per agent + total (cost estimate)", false, "", nil},
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
type quitMsg struct{}                                 // request to quit (after an exec action)
type cmdMsg struct{ out string }                      // result of a console command
type toolDocMsg struct{ name, path, doc, err string } // result of async TOOL.md generation
type convMsg struct {                                 // result of an async conversation read
	agent string
	items []transcript.Item
}
type dataMsg struct {
	agents []agent.View
	tools  []tools.Status
	events []events.Event
}

// bgTickMsg fires on the slow procScanInterval; bgMsg carries a completed scan.
type bgTickMsg struct{}
type bgMsg struct{ procs []procscan.Proc }

// New builds the dashboard model.
func New(p paths.Project, cfg *config.Project, fake bool) model {
	names := []string{config.SupervisorName}
	for _, a := range cfg.Agents {
		names = append(names, a.Name)
	}

	ti := textinput.New()
	ti.Placeholder = defaultPlaceholder
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
	cmds := []tea.Cmd{tick(), m.refresh(), bgTick(), m.scanBg()}
	if m.watcher != nil {
		cmds = append(cmds, watchCmd(m.watcher))
	}
	return tea.Batch(cmds...)
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func bgTick() tea.Cmd {
	return tea.Tick(procScanInterval, func(time.Time) tea.Msg { return bgTickMsg{} })
}

// scanBg discovers agent-spawned background processes off the UI thread. Bubble
// Tea runs Cmds in goroutines, so the lsof/ps (or /proc walk) cost never blocks
// rendering; the result is cached in m.bgProcs.
func (m model) scanBg() tea.Cmd {
	p, cfg := m.p, m.cfg
	return func() tea.Msg { return bgMsg{procs: procscan.Scan(p, cfg)} }
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
		widthChanged := msg.Width != m.width
		m.width = msg.Width
		m.height = msg.Height
		// The conversation lines are wrapped to the width; re-wrap on a width change
		// and keep the scroll offset in bounds.
		if widthChanged && m.detailAgent != "" && len(m.detailItems) > 0 {
			m.detailLines = m.renderConversation(m.detailItems)
			if m.detailScrollOffset > len(m.detailLines)-1 {
				m.detailScrollOffset = len(m.detailLines) - 1
			}
			if m.detailScrollOffset < 0 {
				m.detailScrollOffset = 0
			}
		}
		if widthChanged {
			m.recacheSupervisor() // wrap width changed → re-render the cached digest
		}
		return m, nil

	case tickMsg:
		return m, tea.Batch(tick(), m.refresh())

	case bgTickMsg:
		return m, tea.Batch(bgTick(), m.scanBg())

	case bgMsg:
		m.bgProcs = msg.procs
		m.bgScanAt = time.Now()
		return m, nil

	case fsMsg:
		return m, tea.Batch(m.refresh(), watchCmd(m.watcher))

	case cmdMsg:
		m.cmdOut = msg.out
		return m, m.refresh()

	case toolDocMsg:
		if !m.registerLoading || msg.path != m.registerPath {
			return m, nil // stale or superseded generation — ignore
		}
		m.registerLoading = false
		if msg.err != "" {
			m.setFlash(notice("error", "doc generation failed: "+truncate(msg.err, 80)))
			m.registerPath, m.registerAgent, m.registerName, m.registerAbout = "", "", "", ""
			return m, nil
		}
		m.registerName, m.registerDoc = msg.name, msg.doc
		return m, nil

	case convMsg:
		if msg.agent != m.detailAgent { // user navigated away before it loaded
			return m, nil
		}
		m.detailLoading = false
		m.detailItems = msg.items
		m.detailLines = m.renderConversation(msg.items)     // cache once (not per keypress)
		m.detailScrollOffset = m.conversationBottomOffset() // open at the latest message
		return m, nil

	case quitMsg:
		return m, tea.Quit

	case dataMsg:
		m.agentViews = msg.agents
		m.toolStatuses = msg.tools
		m.recentEvents = msg.events
		m.recacheSupervisor() // refresh the cached digest if the supervisor's message changed
		// Re-read config from disk so the fleet reflects out-of-band changes —
		// add/remove agent (CLI or another process) rewrites config.yaml, and the
		// console must not keep rendering a stale roster.
		if c, err := config.Load(m.p.ConfigPath()); err == nil {
			m.cfg = c
			names := []string{config.SupervisorName}
			for _, a := range c.Agents {
				names = append(names, a.Name)
			}
			m.agentNames = names
		}
		m.selIdx = clampSel(m.selIdx, len(m.agentNames))
		// If the detail overlay is open, rebuild its choices from the fresh view so
		// they never go stale (e.g. the agent resumed and is no longer blocked).
		if m.detailAgent != "" {
			m.detailChoices = m.buildDetailChoices(m.detailAgent)
			m.detailIdx = clampSel(m.detailIdx, len(m.detailChoices))
		}
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
		// Service-tool action picker intercepts keys while open (type to filter).
		if m.toolPicking {
			matches := m.toolMatches()
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.toolPicking, m.toolFilter = false, ""
				return m, nil
			case "up", "ctrl+p":
				if n := len(matches); n > 0 {
					m.toolIdx = (m.toolIdx - 1 + n) % n
				}
				return m, nil
			case "down", "ctrl+n":
				if n := len(matches); n > 0 {
					m.toolIdx = (m.toolIdx + 1) % n
				}
				return m, nil
			case "enter":
				if len(matches) == 0 {
					return m, nil
				}
				name := matches[clampSel(m.toolIdx, len(matches))].Name
				action := m.toolAction
				m.toolPicking, m.toolFilter = false, ""
				m.setFlash(notice("info", action+" "+name+"…"))
				return m, m.runSelf("tool", action, name)
			case "backspace":
				if m.toolFilter != "" {
					m.toolFilter = m.toolFilter[:len(m.toolFilter)-1]
				}
				m.toolIdx = 0
				return m, nil
			default:
				if len(msg.Runes) > 0 {
					m.toolFilter += string(msg.Runes)
					m.toolIdx = 0
				}
				return m, nil
			}
		}
		// Tool-file picker overlay intercepts keys while open (type to filter).
		if m.picking {
			matches := m.pickMatches()
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.picking, m.pickAgent, m.pickFilter = false, "", ""
				return m, nil
			case "up", "ctrl+p":
				if n := len(matches); n > 0 {
					m.pickIdx = (m.pickIdx - 1 + n) % n
				}
				return m, nil
			case "down", "ctrl+n":
				if n := len(matches); n > 0 {
					m.pickIdx = (m.pickIdx + 1) % n
				}
				return m, nil
			case "enter":
				if len(matches) == 0 {
					return m, nil
				}
				return m.pickFile(matches[clampSel(m.pickIdx, len(matches))])
			case "backspace":
				if m.pickFilter != "" {
					m.pickFilter = m.pickFilter[:len(m.pickFilter)-1]
				}
				m.pickIdx = 0
				return m, nil
			default:
				if len(msg.Runes) > 0 {
					m.pickFilter += string(msg.Runes)
					m.pickIdx = 0
				}
				return m, nil
			}
		}
		// Tool-type choice overlay (/toolgen: command vs service).
		if m.registerTypeChoice {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "up", "k":
				if m.registerTypeIdx > 0 {
					m.registerTypeIdx--
				}
				return m, nil
			case "down", "j":
				if m.registerTypeIdx < len(toolTypeChoices)-1 {
					m.registerTypeIdx++
				}
				return m, nil
			case "esc":
				m.cancelIntent()
				m.setFlash(notice("info", "tool registration cancelled"))
				return m, nil
			case "enter":
				m.registerType = toolTypeChoices[m.registerTypeIdx].kind
				m.registerTypeChoice = false
				return m.askToolIntent(filepath.Base(m.registerPath)), nil
			}
			if d := digit(msg.String()); d >= 1 && d <= len(toolTypeChoices) {
				m.registerType = toolTypeChoices[d-1].kind
				m.registerTypeChoice = false
				return m.askToolIntent(filepath.Base(m.registerPath)), nil
			}
			return m, nil
		}
		// Tool-doc approval overlay intercepts keys while open.
		if m.registerDoc != "" {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "enter", "y":
				return m.approveToolDoc()
			case "esc", "q":
				m.registerDoc = ""
				m.setFlash(notice("info", "tool registration cancelled"))
				return m, nil
			}
			return m, nil
		}
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
			// read-only conversation view: scrollable; esc/q closes.
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc", "q":
				m.detailAgent = ""
				return m, nil
			case "down", "j", "ctrl+n":
				if m.detailScrollOffset < len(m.detailLines)-1 {
					m.detailScrollOffset++
				}
				return m, nil
			case "up", "k", "ctrl+p":
				if m.detailScrollOffset > 0 {
					m.detailScrollOffset--
				}
				return m, nil
			case "g", "home":
				m.detailScrollOffset = 0
				return m, nil
			case "G", "end":
				m.detailScrollOffset = m.conversationBottomOffset()
				return m, nil
			}
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
			if m.awaitingIntent { // tabbing away abandons the intent step
				m.cancelIntent()
			}
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
				if m.awaitingIntent { // collecting the tool's intent for /toolgen
					return m.startToolGen(v)
				}
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
				if m.awaitingIntent { // cancel the tool-gen intent step
					m.cancelIntent()
					m.setFlash(notice("info", "tool registration cancelled"))
					return m, nil
				}
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
			return m, m.openDetail(m.selectedAgent())
		case "?":
			m.showHelp = true
			return m, nil
		case "r":
			return m, m.refresh()
		case "X":
			m.confirmMsg = "Force-stop the fleet and reset ALL sessions (config kept)?"
			m.confirmCmd = m.runSelf("reset", "--yes")
			return m, nil
		case "D":
			ag := m.selectedAgent()
			if ag == config.SupervisorName {
				m.setFlash(notice("warn", "the supervisor cannot be removed"))
				return m, nil
			}
			m.confirmMsg = fmt.Sprintf("Permanently remove agent %q — workspace, session, and all state?", ag)
			m.confirmCmd = m.runSelf("remove", "agent", ag, "--yes")
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
		case "g":
			m.openToolPicker(m.selectedAgent())
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
	case m.toolPicking:
		return m.clampTop(m.withFlash(m.toolPickerView()))
	case m.picking:
		return m.clampTop(m.withFlash(m.pickerView()))
	case m.registerTypeChoice:
		return m.clampTop(m.withFlash(m.typeChoiceView()))
	case m.registerDoc != "":
		return m.clampTop(m.withFlash(m.registerView()))
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
func (m *model) openDetail(name string) tea.Cmd {
	m.detailAgent = name
	m.detailIdx = 0
	m.detailChoices = nil
	m.detailScrollOffset = 0
	m.detailItems, m.detailLines = nil, nil
	m.detailLoading = true
	m.detailChoices = m.buildDetailChoices(name)
	// Read the recent conversation OFF the UI thread — a long transcript would
	// otherwise block rendering on the keypress that opened the overlay.
	p, cfg := m.p, m.cfg
	return func() tea.Msg {
		return convMsg{agent: name, items: agent.RecentConversation(p, cfg, name, 80)}
	}
}

// buildDetailChoices derives the actionable choices for a blocked agent from its
// CURRENT view: answer-options (a decision), a permission grant, then reply/close.
// Returns nil if the agent isn't blocked (the overlay then shows the read-only
// conversation). Recomputed on every refresh so the choices never go stale.
func (m model) buildDetailChoices(name string) []permChoice {
	v := m.viewFor(name)
	if v.State != agent.StateBlocked || name == config.SupervisorName {
		return nil
	}
	var choices []permChoice
	for _, opt := range parseOptions(v.FullMessage) {
		choices = append(choices, permChoice{label: "Answer: " + opt, kind: "answer", answer: opt})
	}
	if rules := parsePermissionRequest(v.FullMessage); len(rules) > 0 {
		choices = append(choices, permChoice{
			label: "Grant " + strings.Join(rules, ", ") + " and resume " + name, kind: "grant", rules: rules,
		})
	}
	return append(choices,
		permChoice{label: "Reply with guidance instead", kind: "reply"},
		permChoice{label: "Close", kind: "close"},
	)
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
	case "answer":
		// Send the chosen option back to the agent (resumes its blocked turn).
		m.setFlash(notice("info", "sending your choice to "+ag+": "+truncate(c.answer, 36)))
		return m, m.runSelf("send", ag, "The human chose: "+c.answer)
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
	blocked := len(m.detailChoices) > 0

	var head strings.Builder
	head.WriteString(titleStyle.Render("  "+v.Name) + "  " + stateColored(v.State) + "\n")
	meta := fmt.Sprintf("model %s · tokens %s↓ %s↑", v.Model, agent.HumanInt(v.InTokens), agent.HumanInt(v.OutTokens))
	if !v.LastActivity.IsZero() {
		meta += " · " + humanAgo(time.Since(v.LastActivity)) + " ago"
	}
	head.WriteString("  " + wDim2(meta) + "\n\n")

	var body []string
	off, total := 0, 0
	if blocked {
		// Blocked: show the agent's ask (last message); the grant prompt is below.
		head.WriteString(headerStyle.Render("  MESSAGE") + "\n")
		msg := strings.TrimSpace(v.FullMessage)
		if msg == "" {
			msg = strings.TrimSpace(v.CurrentTask)
		}
		if msg == "" {
			body = []string{"  " + wDim2("(no message)")}
		} else {
			body = strings.Split(indentLines(wrap(msg, m.msgWidth()), "  "), "\n")
		}
	} else {
		// Read-only: the full recent conversation (every chunk), scrollable. Lines
		// are cached (rendered once on load); we never re-wrap per keypress.
		full := m.detailLines
		if m.detailLoading {
			full = []string{"  " + wDim2("loading recent conversation…")}
		} else if len(full) == 0 {
			if msg := strings.TrimSpace(v.FullMessage); msg != "" {
				full = strings.Split(indentLines(wrap(msg, m.msgWidth()), "  "), "\n")
			} else {
				full = []string{"  " + wDim2("(no conversation yet — the agent hasn't produced output)")}
			}
		}
		total = len(full)
		off = m.detailScrollOffset
		if off > total-1 {
			off = total - 1
		}
		if off < 0 {
			off = 0
		}
		head.WriteString(headerStyle.Render("  CONVERSATION") + "  " + wDim2(fmt.Sprintf("%d msgs", len(m.detailItems))) + "\n")
		body = append([]string{}, full[off:]...)
		if off > 0 {
			body = append([]string{"  " + wDim2(fmt.Sprintf("↑ %d more above (k to scroll up)", off))}, body...)
		}
	}

	var foot strings.Builder
	if blocked {
		hasGrant, hasAnswer := false, false
		for _, c := range m.detailChoices {
			switch c.kind {
			case "grant":
				hasGrant = true
			case "answer":
				hasAnswer = true
			}
		}
		label := "NEEDS YOUR INPUT"
		if hasGrant {
			label = "PERMISSION REQUEST"
		} else if hasAnswer {
			label = "DECISION NEEDED"
		}
		foot.WriteString("\n" + warnStyle.Render("  ⚠ "+label) + "\n")
		if req := parsePermissionRequest(v.FullMessage); hasGrant && len(req) > 0 {
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
		pos := ""
		if total > 0 {
			pos = fmt.Sprintf("   %d/%d", off+1, total)
		}
		foot.WriteString("\n  " + hintStyle.Render("j/k ↑↓ scroll · g top · G bottom · /attach live · esc close"+pos))
	}

	// Budget the body to the terminal height so the footer (grant prompt / scroll
	// hint) always stays visible; the overflow truncates with a marker.
	if m.height > 0 {
		headN := strings.Count(head.String(), "\n")
		footN := strings.Count(foot.String(), "\n") + 1
		budget := m.height - headN - footN - 3 // 2 for a withFlash toast + 1 for the marker
		if budget < 1 {
			budget = 1
		}
		if len(body) > budget {
			marker := "  " + wDim2("↓ j to scroll for more")
			if blocked {
				marker = "  " + wDim2("… (widen the terminal to read all)")
			}
			body = append(body[:budget], marker)
		}
	}
	return head.String() + strings.Join(body, "\n") + "\n" + foot.String()
}

// renderConversation renders conversation items into display lines: dim "▸ you:"
// prefixes for prompts, plain text for assistant chunks, and a dim "⎿ tool(input)"
// line for tool calls — one blank line between turns. Called once per load (cached),
// not per keypress. Capped to avoid pathological bloat on extremely verbose agents.
func (m model) renderConversation(items []transcript.Item) []string {
	const maxLines = 6000
	w := m.msgWidth()
	var lines []string
	for _, it := range items {
		switch {
		case it.Role == "user":
			lines = append(lines, wDim2("  ▸ you:"))
			lines = append(lines, wrapLines(truncate(it.Text, 200), w, "    ")...)
		case it.Kind == "tool_use":
			lines = append(lines, "  "+wDim2("⎿ "+it.Tool+"("+truncate(it.Text, 46)+")"))
		default: // assistant text chunk — render its markdown as themed terminal text
			lines = append(lines, mdRenderIndented(it.Text, w, "  ")...)
		}
		lines = append(lines, "")
		if len(lines) > maxLines {
			lines = append(lines, "  "+wDim2("… (older output trimmed — use /attach for the full session)"))
			break
		}
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// wrapLines word-wraps s to width w, indents every line with pad, and returns the
// resulting display lines.
func wrapLines(s string, w int, pad string) []string {
	return strings.Split(indentLines(wrap(s, w), pad), "\n")
}

// conversationBodyRows estimates how many conversation lines are visible in the
// read-only detail view (chrome ≈ header 4 + footer 2 + reserve 3 = 9 lines).
func (m model) conversationBodyRows() int {
	r := m.height - 9
	if r < 1 {
		r = 1
	}
	return r
}

// conversationBottomOffset is the scroll offset that pins the LATEST message to the
// bottom of the view (used to open at the bottom and for the G / End shortcut).
func (m model) conversationBottomOffset() int {
	if off := len(m.detailLines) - m.conversationBodyRows() + 1; off > 0 {
		return off
	}
	return 0
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

	// The auto-discovered BACKGROUND processes render as a right-hand SIDEBAR that
	// spans the banner+agents+tools block, so a long list never pushes the agents
	// panel down or leaves a gap. To make room, the agents TASK column is narrowed
	// to fit beside the sidebar. When the terminal is too narrow for that, the
	// background stacks below instead (and agents keep full width).
	var bgBlock string
	bgW := 0
	if len(m.bgProcs) > 0 {
		bgBlock = headerStyle.Render("  BACKGROUND") + "  " + wDim2("auto-discovered · agent-spawned") +
			"\n" + boxStyle.Render(m.renderBackgroundPanel())
		bgW = lipgloss.Width(bgBlock)
	}
	bannerW := lipgloss.Width(m.welcomeBox())
	availLeft := m.width - bgW - 3
	sidebar := bgW > 0 && m.width > 0 && availLeft >= bannerW+2
	taskW := 34
	if sidebar {
		// agents box width ≈ 55 + taskW; size taskW so the box fits availLeft.
		if t := availLeft - 55; t < taskW {
			taskW = t
		}
		if taskW < 18 {
			taskW = 18
		}
	}

	// Left column: banner, agents, tools.
	var left strings.Builder
	left.WriteString(m.welcomeBox())
	left.WriteString("\n\n")
	if m.focus == focusAgents {
		left.WriteString(titleStyle.Render("  ❯ AGENTS") + "  " + wDim2("selecting · tab to type") + "\n")
		left.WriteString(boxActive.Render(m.renderAgentsPanel(taskW)))
	} else {
		left.WriteString(headerStyle.Render("  AGENTS") + "  " + wDim2("tab to select an agent") + "\n")
		left.WriteString(boxStyle.Render(m.renderAgentsPanel(taskW)))
	}
	left.WriteString("\n\n")
	left.WriteString(headerStyle.Render("  TOOLS") + "\n")
	if len(m.cfg.Tools) == 0 {
		left.WriteString(hintStyle.Render("  (no tools registered — press g on an agent to add one)"))
	} else {
		left.WriteString(boxStyle.Render(m.renderToolsPanel()))
	}

	if sidebar {
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, left.String(), "   ", bgBlock))
	} else {
		b.WriteString(left.String())
		if bgW > 0 { // too narrow for a sidebar — stack below, still no gap
			b.WriteString("\n\n" + bgBlock)
		}
	}
	b.WriteString("\n\n")

	// Hide the supervisor/output panes while the command palette is open, so it has
	// room near the input (like Claude Code's slash menu).
	if !m.paletteOpen() {
		b.WriteString(headerStyle.Render("  SUPERVISOR") + "  " + wDim2("orchestrator · select it + enter for the full chat"))
		b.WriteString("\n")
		b.WriteString(boxStyle.Render(m.renderSupervisorPanel()))
		b.WriteString("\n\n")

		if m.cmdOut != "" {
			b.WriteString(headerStyle.Render("  CONSOLE OUTPUT"))
			b.WriteString("\n")
			b.WriteString(boxStyle.Render(lastLines(m.cmdOut, 6)))
			b.WriteString("\n\n")
		}
	}

	b.WriteString(m.rule() + "\n")
	// The prompt bar is bright when it has focus (prompt mode), dim otherwise — so
	// together with the AGENTS highlight the active mode is always clear.
	if m.focus == focusInput {
		m.input.PromptStyle = lipgloss.NewStyle().Foreground(colBrand).Bold(true)
	} else {
		m.input.PromptStyle = lipgloss.NewStyle().Foreground(colSubtle)
	}
	agentLabel := wDim2(m.selectedAgent())
	if m.focus == focusInput {
		agentLabel = cAccent.Render(m.selectedAgent())
	}
	inputLine := "  " + agentLabel + " " + m.input.View()
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
	costTot := 0.0
	for _, v := range m.agentViews {
		if v.State == agent.StateWorking {
			working++
		}
		inTot += v.InTokens
		outTot += v.OutTokens
		costTot += v.CostUSD
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
		wDim2(fmt.Sprintf("fleet: %d/%d tools up", up, len(m.cfg.ServiceTools()))),
		wDim2(fmt.Sprintf("usage ~$%.2f · %s↓ %s↑ tok", costTot, agent.HumanInt(inTot), agent.HumanInt(outTot))),
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

// renderAgentsPanel renders the agents table. taskW is the width of the
// CURRENT/LAST TASK column — narrowed when the BACKGROUND sidebar is shown so the
// whole row still fits beside it; full width (34) otherwise.
func (m model) renderAgentsPanel(taskW int) string {
	var b strings.Builder
	b.WriteString(wDim2(fmt.Sprintf("  %-12s%-10s%-7s%-*s%-8s%-12s",
		"AGENT", "STATE", "LAST", taskW, truncate("CURRENT / LAST TASK", taskW-1), "MODEL", "TOKENS i/o")))
	if len(m.agentViews) == 0 {
		b.WriteString("\n" + hintStyle.Render("  loading…"))
		return b.String()
	}
	for i, v := range m.agentViews {
		cur := "  "
		name := padCell(truncate(v.Name, 12), 12)
		if i == m.selIdx {
			// bright pointer when the list is the active focus; dim when typing.
			if m.focus == focusAgents {
				cur = cAccent.Render("❯ ")
			} else {
				cur = wDim2("❯ ")
			}
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
			padCell(last, 7) + padCell(truncate(task, taskW-1), taskW) +
			padCell(truncate(v.Model, 7), 8) + io)
	}
	return b.String()
}

func (m model) renderToolsPanel() string {
	var b strings.Builder
	b.WriteString(wDim2(fmt.Sprintf("  %-16s%-12s%-11s%-10s%-8s",
		"TOOL", "OWNER", "STATUS", "UPTIME", "PORT")))
	// Index live service status by tool name; service tools have a running process,
	// command/library tools are on-demand/files (shown for visibility, no process).
	st := map[string]tools.Status{}
	for _, s := range m.toolStatuses {
		st[s.Name] = s
	}
	for _, t := range m.cfg.Tools {
		owner := t.Owner
		if owner == "" {
			owner = "—"
		}
		var statusCell string
		up, port := "—", "—"
		if t.Type == config.ToolService {
			if s, ok := st[t.Name]; ok {
				statusCell = statusColored(padCell(s.State, 11))
				if s.State != tools.StateStopped {
					up = humanAgo(s.Uptime)
				}
				if len(s.Ports) > 0 {
					ps := make([]string, len(s.Ports))
					for i, p := range s.Ports {
						ps[i] = fmt.Sprintf("%d", p)
					}
					port = strings.Join(ps, ",")
				}
			} else {
				statusCell = wDim2(padCell("—", 11))
			}
		} else {
			// command / library: registered + ready, invoked by the agent on demand
			statusCell = wDim2(padCell(t.Type, 11))
		}
		b.WriteString("\n  " + padCell(truncate(t.Name, 16), 16) + padCell(truncate(owner, 12), 12) +
			statusCell + padCell(up, 10) + port)
	}
	return b.String()
}

// renderBackgroundPanel lists processes hivemind agents spun up (a jupyter server,
// a database, a user's long-running binary), each attributed to the owning agent
// with its listening port and live uptime. Read-only — discovered, never managed.
// bgMaxRows is how many background processes the panel lists before collapsing the
// rest into a "+N more" line. It sits in the wide right column, so it can run long.
const bgMaxRows = 15

func (m model) renderBackgroundPanel() string {
	var b strings.Builder
	b.WriteString(wDim2(fmt.Sprintf("  %-18s%-16s%-14s%-7s%s",
		"SERVICE", "AGENT", "PORT", "UPTIME", "PID")))
	show := m.bgProcs
	if len(show) > bgMaxRows {
		show = show[:bgMaxRows]
	}
	for _, pr := range show {
		name := pr.Display()
		if name == "" {
			name = "—"
		}
		agentCell := padCell(truncate(pr.Agent, 15), 16)
		if pr.Agent == "fleet" {
			agentCell = wDim2(agentCell)
		} else {
			agentCell = cAccent.Render(agentCell)
		}
		port := "—"
		if len(pr.Ports) > 0 {
			ps := make([]string, len(pr.Ports))
			for i, p := range pr.Ports {
				ps[i] = fmt.Sprintf("%d", p)
			}
			port = ":" + strings.Join(ps, ",")
		}
		b.WriteString("\n  " + padCell(truncate(name, 17), 18) + agentCell +
			padCell(truncate(port, 13), 14) + padCell("up"+humanAgo(pr.Uptime), 7) +
			fmt.Sprintf("%d", pr.PID))
	}
	if n := len(m.bgProcs); n > bgMaxRows {
		b.WriteString("\n  " + wDim2(fmt.Sprintf("+%d more background process(es)", n-bgMaxRows)))
	}
	return b.String()
}

// renderSupervisorPanel shows the supervisor's latest message (its orchestration
// digest / decision), wrapped to a few lines. The full chat is via enter/v on it.
func (m model) renderSupervisorPanel() string {
	v := m.viewFor(config.SupervisorName)
	msg := strings.TrimSpace(v.FullMessage)
	if msg == "" {
		msg = strings.TrimSpace(v.Summary)
	}
	if msg == "" {
		msg = strings.TrimSpace(v.CurrentTask)
	}
	head := "  " + wDim2(fmt.Sprintf("%s · %s", stateColored(v.State), v.Model))
	if len(m.supLines) == 0 { // no message yet (or recache hasn't run)
		if msg == "" {
			return head + "\n  " + wDim2("(no summary yet — run /report to wake the orchestrator)")
		}
		// Fallback path (e.g. first paint before a dataMsg) — render directly.
		return head + "\n" + strings.Join(m.supervisorBody(msg, m.msgWidth()), "\n")
	}
	return head + "\n" + strings.Join(m.supLines, "\n")
}

// supervisorBody renders + caps the supervisor's message to the panel's line budget.
func (m model) supervisorBody(msg string, width int) []string {
	lines := mdRenderIndented(msg, width, "  ")
	const maxLines = 6
	if len(lines) > maxLines {
		lines = append(lines[:maxLines:maxLines], "  "+wDim2("… select the supervisor + enter for the full chat"))
	}
	return lines
}

// recacheSupervisor recomputes the cached supervisor body, but only when the
// message text or panel width actually changed — so the per-frame render (and the
// 130ms spinner) reuses the rendered markdown instead of re-parsing it each time.
func (m *model) recacheSupervisor() {
	v := m.viewFor(config.SupervisorName)
	msg := strings.TrimSpace(v.FullMessage)
	if msg == "" {
		msg = strings.TrimSpace(v.Summary)
	}
	if msg == "" {
		msg = strings.TrimSpace(v.CurrentTask)
	}
	w := m.msgWidth()
	if msg == m.supMsgCache && w == m.supWidthCache && (msg == "") == (len(m.supLines) == 0) {
		return // unchanged
	}
	m.supMsgCache, m.supWidthCache = msg, w
	if msg == "" {
		m.supLines = nil
		return
	}
	m.supLines = m.supervisorBody(msg, w)
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
	if m.focus != focusInput || m.awaitingIntent {
		return false // don't hijack the input while collecting a tool's intent
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
	case "usage":
		m.setFlash(notice("info", "token usage below · subscription rate limit isn't exposed to tools — see the Claude Console"))
		return m, m.runSelf("status")
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
		action := "start"
		if len(fields) >= 2 {
			action = fields[1]
		}
		if action != "start" && action != "stop" && action != "restart" {
			m.setFlash(notice("warn", "usage: /tool start|stop|restart [name]"))
			return m, nil
		}
		if len(fields) >= 3 { // direct: /tool <action> <name>
			return m, m.runSelf("tool", action, fields[2])
		}
		m.openToolActionPicker(action) // no name → pick a tool from a filterable list
		return m, nil
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
	case "agent":
		if len(fields) < 2 {
			m.setFlash(notice("warn", "usage: /agent add <name> [model] [role…]  ·  /agent remove <name>"))
			return m, nil
		}
		switch fields[1] {
		case "add":
			if len(fields) < 3 {
				m.setFlash(notice("warn", "usage: /agent add <name> [model] [role…]"))
				return m, nil
			}
			argv := []string{"add", "agent", fields[2]}
			if len(fields) > 3 {
				argv = append(argv, "--model", fields[3])
			}
			if len(fields) > 4 {
				argv = append(argv, "--role", strings.Join(fields[4:], " "))
			}
			m.setFlash(notice("info", "deploying agent "+fields[2]+"…"))
			return m, m.runSelf(argv...)
		case "remove", "rm":
			if len(fields) < 3 {
				m.setFlash(notice("warn", "usage: /agent remove <name>"))
				return m, nil
			}
			if fields[2] == config.SupervisorName {
				m.setFlash(notice("warn", "the supervisor cannot be removed"))
				return m, nil
			}
			m.confirmMsg = fmt.Sprintf("Permanently remove agent %q — workspace, session, and all state?", fields[2])
			m.confirmCmd = m.runSelf("remove", "agent", fields[2], "--yes")
			return m, nil
		default:
			m.setFlash(notice("warn", "usage: /agent add … · /agent remove …"))
			return m, nil
		}
	case "supervisor", "super":
		if len(fields) < 2 {
			m.setFlash(notice("warn", "usage: /supervisor <model>  (e.g. haiku, sonnet, opus)"))
			return m, nil
		}
		if !config.ValidModel(fields[1]) {
			m.setFlash(notice("warn", "invalid model "+fields[1]+" — use haiku, sonnet, or opus"))
			return m, nil
		}
		m.setFlash(notice("info", "setting supervisor model → "+fields[1]+"…"))
		return m, m.runSelf("edit", "supervisor", "--model", fields[1])
	case "permission", "perm":
		if len(fields) < 3 {
			m.setFlash(notice("warn", "usage: /permission <agent> <mode>  ("+strings.Join(config.PermissionModes, "|")+")"))
			return m, nil
		}
		if !config.ValidPermissionMode(fields[2]) {
			m.setFlash(notice("warn", "invalid mode "+fields[2]+" — use "+strings.Join(config.PermissionModes, "|")))
			return m, nil
		}
		m.setFlash(notice("info", "setting "+fields[1]+"'s permission mode → "+fields[2]+"…"))
		return m, m.runSelf("edit", fields[1], "--permission-mode", fields[2])
	case "toolgen", "learn":
		if m.registerLoading || m.registerDoc != "" || m.picking || m.awaitingIntent || m.registerTypeChoice {
			m.setFlash(notice("info", "a tool registration is already in progress — finish that first"))
			return m, nil
		}
		ag := m.selectedAgent()
		if len(fields) < 2 {
			m.openToolPicker(ag) // no path → pick a file from the agent's workspace
			return m, nil
		}
		if ag == config.SupervisorName {
			m.setFlash(notice("warn", "select a worker agent to attach the tool to"))
			return m, nil
		}
		m.registerPath, m.registerAgent, m.registerName = fields[1], ag, toolNameFromFile(fields[1])
		if len(fields) > 2 { // path + intent inline → generate now (defaults to command)
			m.registerType = "command"
			m.registerAbout = strings.Join(fields[2:], " ")
			m.registerLoading = true
			m.setFlash(notice("info", "reading "+filepath.Base(fields[1])+" and writing its docs…"))
			return m, m.genToolDocCmd(fields[1], m.registerAbout)
		}
		m.registerTypeChoice, m.registerTypeIdx = true, 0 // path only → choose type, then intent
		return m, nil
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

// genToolDocCmd runs `hivemind tool gendoc <path> --about …` off the UI thread and
// returns a toolDocMsg with the generated TOOL.md (or an error). LLM generation
// takes seconds, so this is async; registerLoading shows a flash meanwhile.
func (m model) genToolDocCmd(path, about string) tea.Cmd {
	root, fake, name := m.p.Root, m.fake, toolNameFromFile(path)
	return func() tea.Msg {
		exe, err := os.Executable()
		if err != nil {
			return toolDocMsg{err: err.Error(), path: path, name: name}
		}
		g := []string{"--root", root}
		if fake {
			g = append(g, "--fake")
		}
		out, e := exec.Command(exe, append(g, "tool", "gendoc", path, "--about", about)...).CombinedOutput()
		if e != nil {
			return toolDocMsg{err: strings.TrimSpace(string(out)), path: path, name: name}
		}
		return toolDocMsg{name: name, path: path, doc: strings.TrimSpace(string(out))}
	}
}

// approveToolDoc registers the tool with the approved (generated) doc and attaches
// it to the agent, then closes the overlay. The doc is passed via a temp file so the
// exact approved text is what gets registered.
func (m model) approveToolDoc() (tea.Model, tea.Cmd) {
	root, fake := m.p.Root, m.fake
	name, path, ag, doc := m.registerName, m.registerPath, m.registerAgent, m.registerDoc
	toolType := m.registerType
	if toolType == "" {
		toolType = "command"
	}
	m.registerDoc = "" // close overlay
	if ag != config.SupervisorName && m.cfg.FindAgent(ag) == nil {
		m.setFlash(notice("error", "agent "+ag+" no longer exists — not registering"))
		return m, nil
	}
	m.setFlash(notice("info", "registering "+name+" → "+ag+"…"))
	return m, func() tea.Msg {
		f, err := os.CreateTemp("", "hm-tooldoc-*.md")
		if err != nil {
			return cmdMsg{out: "register failed: " + err.Error()}
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(doc); err != nil {
			_ = f.Close()
			return cmdMsg{out: "register failed writing doc: " + err.Error()}
		}
		if err := f.Close(); err != nil {
			return cmdMsg{out: "register failed closing doc: " + err.Error()}
		}
		exe, err := os.Executable()
		if err != nil {
			return cmdMsg{out: err.Error()}
		}
		g := []string{"--root", root}
		if fake {
			g = append(g, "--fake")
		}
		args := append(g, "tool", "register", name, "--file", path, "--doc-file", f.Name(), "--agent", ag, "--type", toolType, "--yes", "--force")
		out, _ := exec.Command(exe, args...).CombinedOutput()
		return cmdMsg{out: strings.TrimSpace(string(out))}
	}
}

// registerView is the approval overlay: the generated TOOL.md, budgeted so the
// approve/cancel hint stays visible even when the doc is long.
func (m model) registerView() string {
	var head strings.Builder
	verb := "register tool:"
	if m.cfg.FindTool(m.registerName) != nil {
		verb = "update tool:" // already exists — approving overwrites its docs
	}
	head.WriteString(titleStyle.Render("  "+verb+" "+m.registerName) + "  " + wDim2("→ "+m.registerAgent) + "\n")
	head.WriteString("  " + wDim2("entrypoint  "+inferEntrypoint(m.registerPath)) + "\n\n")
	head.WriteString(headerStyle.Render("  GENERATED TOOL.md") + "  " + wDim2("(written by Claude from the source)") + "\n")
	body := strings.Split(indentLines(wrap(strings.TrimSpace(m.registerDoc), m.msgWidth()), "  "), "\n")
	foot := "\n  " + okStyle.Render("enter") + wDim2(" approve & register  ·  ") + warnStyle.Render("esc") + wDim2(" cancel")
	if m.height > 0 {
		// reserve: hint(1) + truncation marker(1) + withFlash toast(2).
		budget := m.height - strings.Count(head.String(), "\n") - 4
		if budget < 1 {
			budget = 1
		}
		if len(body) > budget {
			body = append(body[:budget], "  "+wDim2("… (doc truncated for display — review the full text before approving)"))
		}
	}
	return head.String() + strings.Join(body, "\n") + foot
}

// openToolPicker starts the /toolgen flow: list files in the selected agent's
// workspace so the user picks one (no path typing), then describe its intent.
func (m *model) openToolPicker(name string) {
	if name == config.SupervisorName {
		m.setFlash(notice("warn", "select a worker agent — the supervisor has no workspace"))
		return
	}
	a := m.cfg.FindAgent(name)
	if a == nil {
		m.setFlash(notice("error", "unknown agent "+name))
		return
	}
	files := workspaceFiles(m.p.WorkspaceDir(a.Workspace))
	if len(files) == 0 {
		m.setFlash(notice("info", "no files in "+name+"'s workspace — drop a script there, then press g"))
		return
	}
	m.picking, m.pickAgent, m.pickFiles, m.pickIdx, m.pickFilter = true, name, files, 0, ""
}

// pickMatches filters the workspace files by the typed filter (autocomplete).
func (m model) pickMatches() []string {
	if m.pickFilter == "" {
		return m.pickFiles
	}
	f := strings.ToLower(m.pickFilter)
	var out []string
	for _, n := range m.pickFiles {
		if strings.Contains(strings.ToLower(n), f) {
			out = append(out, n)
		}
	}
	return out
}

// pickFile records the chosen file and advances to the intent step.
func (m model) pickFile(file string) (tea.Model, tea.Cmd) {
	ag := m.pickAgent
	a := m.cfg.FindAgent(ag)
	m.picking, m.pickAgent, m.pickFilter = false, "", ""
	if a == nil {
		m.setFlash(notice("error", "agent "+ag+" no longer exists"))
		return m, nil
	}
	path := filepath.Join(m.p.WorkspaceDir(a.Workspace), file)
	if fi, err := os.Stat(path); err != nil || fi.IsDir() { // fail fast before any async work
		m.setFlash(notice("error", file+" is no longer available"))
		return m, nil
	}
	m.registerPath = path
	m.registerAgent = ag
	m.registerName = toolNameFromFile(file)
	m.registerTypeChoice, m.registerTypeIdx = true, 0 // ask command vs service next
	return m, nil
}

// typeChoiceView renders the command-vs-service choice in the /toolgen flow.
func (m model) typeChoiceView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("  register "+m.registerName+" as…") + "  " + wDim2("→ "+m.registerAgent) + "\n\n")
	for i, c := range toolTypeChoices {
		num := fmt.Sprintf("%d. ", i+1)
		if i == m.registerTypeIdx {
			b.WriteString(cAccent.Render("  ❯ "+num) + lipgloss.NewStyle().Bold(true).Render(c.label) + "\n")
		} else {
			b.WriteString("    " + wDim2(num) + c.label + "\n")
		}
		b.WriteString("       " + wDim2(c.hint) + "\n")
	}
	b.WriteString("\n  " + hintStyle.Render("↑↓ choose · 1–2 / enter select · esc cancel"))
	return b.String()
}

// askToolIntent focuses the input bar to collect the tool's English description.
func (m model) askToolIntent(file string) model {
	m.awaitingIntent = true
	m.focus = focusInput
	m.input.Focus()
	m.input.SetValue("")
	m.input.Placeholder = "what does " + file + " do? (enter to generate · esc to cancel)"
	m.setFlash(notice("info", "describe what "+file+" does, then enter — Claude reads the code"))
	return m
}

// cancelIntent abandons the in-progress /toolgen flow and clears its state.
func (m *model) cancelIntent() {
	m.awaitingIntent = false
	m.registerTypeChoice = false
	m.registerType, m.registerTypeIdx = "", 0
	m.registerPath, m.registerAgent, m.registerName, m.registerAbout = "", "", "", ""
	m.input.SetValue("")
	m.input.Placeholder = defaultPlaceholder
}

// startToolGen kicks off async doc generation once the intent is provided.
func (m model) startToolGen(intent string) (tea.Model, tea.Cmd) {
	m.awaitingIntent = false
	m.registerAbout = intent
	m.input.SetValue("")
	m.input.Placeholder = defaultPlaceholder
	m.registerLoading = true
	m.setFlash(notice("info", "reading "+filepath.Base(m.registerPath)+" and writing its docs…"))
	return m, m.genToolDocCmd(m.registerPath, intent)
}

// unsafeFileRe matches filenames unsafe to register (they'd become part of an
// inferred Bash allow-rule); such files are kept out of the picker entirely.
var unsafeFileRe = regexp.MustCompile("[;|&$`<>(){}\\[\\]!*?~\\\\\"' \t]")

// workspaceFiles lists top-level, non-hidden, REGULAR (no dirs/symlinks/devices)
// files with registration-safe names, sorted — the candidates for /toolgen.
func workspaceFiles(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") || unsafeFileRe.MatchString(e.Name()) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// openToolActionPicker opens the service-tool picker for an action (start/stop/restart).
func (m *model) openToolActionPicker(action string) {
	if len(m.toolStatuses) == 0 {
		m.setFlash(notice("info", "no service tools configured for this project"))
		return
	}
	m.toolPicking, m.toolAction, m.toolIdx, m.toolFilter = true, action, 0, ""
}

// toolMatches filters the service tools by the typed filter (autocomplete).
func (m model) toolMatches() []tools.Status {
	if m.toolFilter == "" {
		return m.toolStatuses
	}
	f := strings.ToLower(m.toolFilter)
	var out []tools.Status
	for _, s := range m.toolStatuses {
		if strings.Contains(strings.ToLower(s.Name), f) {
			out = append(out, s)
		}
	}
	return out
}

// toolPickerView renders the service-tool picker (filter line + status list).
func (m model) toolPickerView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("  "+m.toolAction+" a service tool") + "\n")
	b.WriteString("  " + wDim2("pick a tool — enter to "+m.toolAction+" it") + "\n\n")
	filt := m.toolFilter
	if filt == "" {
		filt = wDim2("(type to filter)")
	}
	b.WriteString("  " + cAmber.Render("filter ") + filt + "\n\n")
	matches := m.toolMatches()
	if len(matches) == 0 {
		b.WriteString("  " + wDim2("no matching tools") + "\n")
	} else {
		sel := clampSel(m.toolIdx, len(matches))
		rows := paletteRows(m.height, len(matches))
		start := sel - rows/2
		if mx := len(matches) - rows; start > mx {
			start = mx
		}
		if start < 0 {
			start = 0
		}
		end := start + rows
		if start > 0 {
			b.WriteString("    " + wDim2(fmt.Sprintf("↑ %d more", start)) + "\n")
		}
		for i := start; i < end; i++ {
			s := matches[i]
			row := padCell(truncate(s.Name, 20), 20) + statusColored(s.State)
			if s.State != tools.StateStopped && s.Uptime > 0 {
				row += wDim2("  up " + humanAgo(s.Uptime))
			}
			if i == sel {
				b.WriteString(cAccent.Render("  ❯ ") + lipgloss.NewStyle().Bold(true).Render(padCell(truncate(s.Name, 20), 20)) + statusColored(s.State) + "\n")
			} else {
				b.WriteString("    " + row + "\n")
			}
		}
		if end < len(matches) {
			b.WriteString("    " + wDim2(fmt.Sprintf("↓ %d more", len(matches)-end)) + "\n")
		}
	}
	b.WriteString("\n  " + hintStyle.Render("type to filter · ↑↓ select · enter "+m.toolAction+" · esc cancel"))
	return b.String()
}

// pickerView renders the workspace-file picker (filter line + scrollable list).
func (m model) pickerView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("  register a tool for "+m.pickAgent) + "\n")
	b.WriteString("  " + wDim2("pick a file from its workspace — Claude reads the code and writes the docs") + "\n\n")
	filt := m.pickFilter
	if filt == "" {
		filt = wDim2("(type to filter)")
	}
	b.WriteString("  " + cAmber.Render("filter ") + filt + "\n\n")
	matches := m.pickMatches()
	if len(matches) == 0 {
		b.WriteString("  " + wDim2("no matching files") + "\n")
	} else {
		sel := clampSel(m.pickIdx, len(matches))
		rows := paletteRows(m.height, len(matches))
		start := sel - rows/2
		if mx := len(matches) - rows; start > mx {
			start = mx
		}
		if start < 0 {
			start = 0
		}
		end := start + rows
		if start > 0 {
			b.WriteString("    " + wDim2(fmt.Sprintf("↑ %d more", start)) + "\n")
		}
		for i := start; i < end; i++ {
			if i == sel {
				b.WriteString(cAccent.Render("  ❯ ") + lipgloss.NewStyle().Bold(true).Render(matches[i]) + "\n")
			} else {
				b.WriteString("    " + matches[i] + "\n")
			}
		}
		if end < len(matches) {
			b.WriteString("    " + wDim2(fmt.Sprintf("↓ %d more", len(matches)-end)) + "\n")
		}
	}
	b.WriteString("\n  " + hintStyle.Render("type to filter · ↑↓ select · enter choose · esc cancel"))
	return b.String()
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
		{"/agent add|remove <name>", "deploy a new agent or destroy one (key: D)"},
		{"/supervisor <model>", "change the supervisor (orchestrator) model"},
		{"/permission <agent> <mode>", "set an agent's permission mode (incl. bypassPermissions)"},
		{"/toolgen", "pick a workspace file → Claude writes its docs → register (key: g)"},
		{"/delegate <instruction>", "route work across agents (supervisor decomposes)"},
		{"/task <agent> <prompt>", "create one task for an agent"},
		{"/attach <agent>", "open the live interactive session"},
		{"/tool start|stop|restart", "pick a service tool to start/stop/restart"},
		{"/up  /stop  /report", "start / stop the fleet · supervisor digest"},
		{"/usage", "token usage + cost estimate (limits → Claude Console)"},
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
		{"g", "register a tool from the selected agent's workspace"},
		{"D", "remove the selected agent (with confirm)"},
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
