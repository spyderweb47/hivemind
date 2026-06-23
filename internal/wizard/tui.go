package wizard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"hivemind/internal/config"
	"hivemind/internal/paths"
)

// ErrCancelled is returned when the user aborts the setup TUI.
var ErrCancelled = errors.New("setup cancelled")

// RunInteractive drives the Bubble Tea setup wizard, then scaffolds the project.
func RunInteractive(defaultRoot string) (*config.Project, string, error) {
	fm, err := tea.NewProgram(newWizardModel(defaultRoot), tea.WithAltScreen()).Run()
	if err != nil {
		return nil, defaultRoot, err
	}
	wm := fm.(wizardModel)
	if wm.cancelled || !wm.confirmed {
		return nil, wm.state.root, ErrCancelled
	}
	cfg := &wm.state.cfg
	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return nil, wm.state.root, err
	}
	p := paths.NewProject(wm.state.root)
	if err := materialize(p, cfg, wm.state.toolSources, wm.state.toolDocs, os.Stdout); err != nil {
		return nil, wm.state.root, err
	}
	return cfg, wm.state.root, nil
}

// wizard steps (a linear state machine with nested agent/tool loops).
const (
	sRoot = iota
	sProject
	sDefModel
	sPermMode
	sAgentName
	sWorkspace
	sRole
	sAgentModel
	sToolName
	sToolType
	sToolEntry // entrypoint (service/command) or foundation path (library)
	sToolHealth
	sToolPorts
	sToolSource
	sToolDoc
	sReads
	sAddAnother
	sReview
	sDone
)

const (
	modeText = iota
	modeChoice
)

type buildState struct {
	root        string
	cfg         config.Project
	toolSources map[string]string
	toolDocs    map[string]string
	registered  map[string]bool

	cur        config.Agent // agent under construction
	curTool    config.Tool  // tool under construction
	curToolSrc string
	curToolDoc string
}

type wizardModel struct {
	step      int
	mode      int
	prompt    string
	hint      string
	input     textinput.Model
	choices   []string
	choiceIdx int
	state     buildState
	errMsg    string
	confirmed bool
	cancelled bool
	width     int
}

var (
	wTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("63")).Padding(0, 1)
	wStep   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
	wHint   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	wErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	wCursor = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	wSel    = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	wDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	wPanel  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("60")).Padding(0, 1)
	wOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("70"))
)

func newWizardModel(defaultRoot string) wizardModel {
	ti := textinput.New()
	ti.CharLimit = 4000
	ti.Width = 52
	m := wizardModel{
		input: ti,
		state: buildState{
			root:        defaultRoot,
			toolSources: map[string]string{},
			toolDocs:    map[string]string{},
			registered:  map[string]bool{},
		},
	}
	// Sensible supervisor defaults (the TUI doesn't ask about these).
	m.state.cfg.Supervisor.Model = "haiku"
	m.state.cfg.Supervisor.Report = config.Report{OnEvent: true, HeartbeatMinutes: 30}
	return m.goTo(sRoot)
}

func (m wizardModel) Init() tea.Cmd { return textinput.Blink }

// goTo configures the input/choices/prompt for a step.
func (m wizardModel) goTo(step int) wizardModel {
	m.step = step
	m.errMsg = ""
	m.input.Blur()
	textVal := ""
	switch step {
	case sRoot:
		m.prompt, m.hint = "Project root directory", "where .hivemind/ and workspaces are created"
		textVal = m.state.root
	case sProject:
		m.prompt, m.hint = "Project name", ""
		textVal = m.state.cfg.Project
		if textVal == "" {
			textVal = filepath.Base(m.state.root)
		}
	case sDefModel:
		m.choices, m.prompt, m.hint = []string{"sonnet", "opus", "haiku"}, "Default model for agents", ""
		m.choiceIdx = 0
	case sPermMode:
		m.choices, m.prompt, m.hint = []string{"acceptEdits", "plan", "default"}, "Default permission mode", "acceptEdits auto-applies edits in each workspace"
		m.choiceIdx = 0
	case sAgentName:
		m.prompt, m.hint = "Agent name", "leave blank when you're done adding agents"
	case sWorkspace:
		m.prompt, m.hint = "Workspace directory", "created under the project root"
		textVal = m.state.cur.Name
	case sRole:
		m.prompt, m.hint = "Role & responsibilities", "free text — becomes the agent's CLAUDE.md role"
	case sAgentModel:
		m.choices, m.prompt, m.hint = []string{"sonnet", "opus", "haiku"}, "Model tier for "+m.state.cur.Name, ""
		m.choiceIdx = indexOf(m.choices, m.state.cfg.Defaults.Model)
	case sToolName:
		m.prompt, m.hint = "Attach a tool", "existing name to reuse, new name to register, or blank to stop"
	case sToolType:
		m.choices, m.prompt, m.hint = []string{"service", "command", "library"}, "Type of tool "+m.state.curTool.Name, "service=long-running, command=CLI, library=file"
		m.choiceIdx = 0
	case sToolEntry:
		if m.state.curTool.Type == config.ToolLibrary {
			m.prompt, m.hint = "Foundation file name", "relative to the tool dir (e.g. template.cpp)"
		} else {
			m.prompt, m.hint = "Entrypoint", "how to run it (e.g. python collector.py)"
		}
	case sToolHealth:
		m.prompt, m.hint = "Health probe", "shell command; exit 0 = healthy (e.g. curl -sf localhost:9000)"
	case sToolPorts:
		m.prompt, m.hint = "Ports", "comma-separated, optional (e.g. 9000,9009)"
	case sToolSource:
		m.prompt, m.hint = "Path to a script/file to drop in", "optional; copied into the tool dir"
	case sToolDoc:
		m.prompt, m.hint = "One-line usage description", "optional; becomes the tool's TOOL.md"
	case sReads:
		m.prompt, m.hint = "Read-only paths for "+m.state.cur.Name, "comma-separated, optional (relative to root)"
	case sAddAnother:
		m.choices, m.prompt, m.hint = []string{"Yes", "No"}, "Add another agent?", ""
		m.choiceIdx = 1
	case sReview:
		m.prompt, m.hint = "Review", "enter to create the fleet · esc to cancel"
	}
	if isChoiceStep(step) {
		m.mode = modeChoice
	} else {
		m.mode = modeText
		m.input.SetValue(textVal)
		m.input.CursorEnd()
		m.input.Focus()
	}
	return m
}

func isChoiceStep(step int) bool {
	switch step {
	case sDefModel, sPermMode, sAgentModel, sToolType, sAddAnother:
		return true
	}
	return false
}

func (m wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.cancelled = true
			return m, tea.Quit
		case tea.KeyEsc:
			if m.step == sReview {
				m.cancelled = true
				return m, tea.Quit
			}
			m.cancelled = true
			return m, tea.Quit
		}
		if m.step == sDone {
			return m, tea.Quit
		}
		if m.mode == modeChoice {
			switch msg.String() {
			case "up", "k":
				if m.choiceIdx > 0 {
					m.choiceIdx--
				}
			case "down", "j":
				if m.choiceIdx < len(m.choices)-1 {
					m.choiceIdx++
				}
			case "enter":
				return m.submit(m.choices[m.choiceIdx])
			}
			return m, nil
		}
		if msg.Type == tea.KeyEnter {
			return m.submit(strings.TrimSpace(m.input.Value()))
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// submit applies the value for the current step and advances.
func (m wizardModel) submit(val string) (tea.Model, tea.Cmd) {
	switch m.step {
	case sRoot:
		if val == "" {
			m.errMsg = "a project root is required"
			return m, nil
		}
		abs, _ := filepath.Abs(val)
		m.state.root = abs
		return m.goTo(sProject), nil
	case sProject:
		m.state.cfg.Project = orDefault(val, filepath.Base(m.state.root))
		return m.goTo(sDefModel), nil
	case sDefModel:
		m.state.cfg.Defaults.Model = val
		return m.goTo(sPermMode), nil
	case sPermMode:
		m.state.cfg.Defaults.PermissionMode = val
		m.state.cur = config.Agent{}
		return m.goTo(sAgentName), nil

	case sAgentName:
		if val == "" {
			if len(m.state.cfg.Agents) == 0 {
				m.errMsg = "add at least one agent before finishing"
				return m, nil
			}
			return m.goTo(sReview), nil
		}
		if val == config.SupervisorName {
			m.errMsg = "'supervisor' is reserved"
			return m, nil
		}
		if m.state.cfg.FindAgent(val) != nil {
			m.errMsg = fmt.Sprintf("agent %q already exists", val)
			return m, nil
		}
		m.state.cur = config.Agent{Name: val}
		return m.goTo(sWorkspace), nil
	case sWorkspace:
		ws := orDefault(val, m.state.cur.Name)
		if filepath.IsAbs(ws) || strings.Contains(ws, "..") {
			m.errMsg = "workspace must be a safe relative name"
			return m, nil
		}
		m.state.cur.Workspace = ws
		return m.goTo(sRole), nil
	case sRole:
		m.state.cur.Role = val
		return m.goTo(sAgentModel), nil
	case sAgentModel:
		m.state.cur.Model = val
		return m.goTo(sToolName), nil

	case sToolName:
		if val == "" {
			return m.goTo(sReads), nil
		}
		if m.state.registered[val] {
			m.state.cur.Tools = append(m.state.cur.Tools, val)
			return m.goTo(sToolName), nil // attach more
		}
		m.state.curTool = config.Tool{Name: val}
		m.state.curToolSrc, m.state.curToolDoc = "", ""
		return m.goTo(sToolType), nil
	case sToolType:
		m.state.curTool.Type = val
		return m.goTo(sToolEntry), nil
	case sToolEntry:
		if m.state.curTool.Type == config.ToolLibrary {
			m.state.curTool.Path = val
			return m.goTo(sToolSource), nil
		}
		if val == "" {
			m.errMsg = "an entrypoint is required"
			return m, nil
		}
		m.state.curTool.Entrypoint = val
		if m.state.curTool.Type == config.ToolService {
			return m.goTo(sToolHealth), nil
		}
		return m.goTo(sToolSource), nil
	case sToolHealth:
		m.state.curTool.Health = val
		return m.goTo(sToolPorts), nil
	case sToolPorts:
		for _, ps := range splitCSV(val) {
			if n, err := strconv.Atoi(ps); err == nil {
				m.state.curTool.Ports = append(m.state.curTool.Ports, n)
			}
		}
		return m.goTo(sToolSource), nil
	case sToolSource:
		m.state.curToolSrc = val
		if m.state.curTool.Type == config.ToolLibrary && m.state.curTool.Path == "" && val != "" {
			m.state.curTool.Path = filepath.Base(val)
		}
		return m.goTo(sToolDoc), nil
	case sToolDoc:
		doc := val
		if doc != "" {
			doc = fmt.Sprintf("# %s\n\n%s\n", m.state.curTool.Name, doc)
		}
		t := m.state.curTool
		t.Owner = m.state.cur.Name
		m.state.cfg.Tools = append(m.state.cfg.Tools, t)
		m.state.registered[t.Name] = true
		if m.state.curToolSrc != "" {
			m.state.toolSources[t.Name] = m.state.curToolSrc
		}
		if doc != "" {
			m.state.toolDocs[t.Name] = doc
		}
		m.state.cur.Tools = append(m.state.cur.Tools, t.Name)
		return m.goTo(sToolName), nil // attach more

	case sReads:
		for _, r := range splitCSV(val) {
			if filepath.IsAbs(r) || strings.Contains(r, "..") || strings.ContainsAny(r, "()* \t") {
				m.errMsg = fmt.Sprintf("unsafe read path %q (no absolute, .., or ()* whitespace)", r)
				return m, nil
			}
			m.state.cur.Reads = append(m.state.cur.Reads, r)
		}
		m.state.cfg.Agents = append(m.state.cfg.Agents, m.state.cur)
		return m.goTo(sAddAnother), nil
	case sAddAnother:
		if val == "Yes" {
			m.state.cur = config.Agent{}
			return m.goTo(sAgentName), nil
		}
		return m.goTo(sReview), nil
	case sReview:
		m.confirmed = true
		m.step = sDone
		return m, tea.Quit
	}
	return m, nil
}

func (m wizardModel) View() string {
	if m.cancelled {
		return ""
	}
	var b strings.Builder
	b.WriteString(wTitle.Render("hivemind setup"))
	b.WriteString("  " + wDim.Render(m.state.root))
	b.WriteString("\n\n")

	// breadcrumb
	b.WriteString(wStep.Render("▸ " + m.prompt))
	b.WriteString("\n")
	if m.hint != "" {
		b.WriteString(wHint.Render("  " + m.hint))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if m.step == sReview {
		b.WriteString(m.reviewBody())
	} else if m.mode == modeChoice {
		for i, c := range m.choices {
			if i == m.choiceIdx {
				b.WriteString(wCursor.Render("  › ") + wSel.Render(c) + "\n")
			} else {
				b.WriteString("    " + c + "\n")
			}
		}
	} else {
		b.WriteString("  " + m.input.View() + "\n")
	}

	if m.errMsg != "" {
		b.WriteString("\n" + wErr.Render("  ✗ "+m.errMsg) + "\n")
	}

	// live fleet panel
	b.WriteString("\n")
	b.WriteString(wPanel.Render(m.fleetSummary()))
	b.WriteString("\n")

	if m.mode == modeChoice {
		b.WriteString(wHint.Render("  ↑/↓ choose · enter select · esc cancel"))
	} else if m.step == sReview {
		b.WriteString(wHint.Render("  enter create fleet · esc cancel"))
	} else {
		b.WriteString(wHint.Render("  enter continue · esc cancel"))
	}
	return b.String()
}

func (m wizardModel) fleetSummary() string {
	var b strings.Builder
	b.WriteString(wDim.Render("fleet so far") + "\n")
	b.WriteString("supervisor " + wDim.Render("(haiku, fixed)") + "\n")
	for _, a := range m.state.cfg.Agents {
		line := fmt.Sprintf("%s (%s)", a.Name, orDefault(a.Model, m.state.cfg.Defaults.Model))
		if len(a.Tools) > 0 {
			line += wDim.Render(" tools=" + strings.Join(a.Tools, ","))
		}
		if len(a.Reads) > 0 {
			line += wDim.Render(" reads=" + strings.Join(a.Reads, ","))
		}
		b.WriteString(wOK.Render("✓ ") + line + "\n")
	}
	// agent under construction
	if m.state.cur.Name != "" && m.step >= sWorkspace && m.step <= sReads {
		line := fmt.Sprintf("%s …", m.state.cur.Name)
		if len(m.state.cur.Tools) > 0 {
			line += wDim.Render(" tools=" + strings.Join(m.state.cur.Tools, ","))
		}
		b.WriteString(wCursor.Render("◆ ") + wDim.Render(line) + "\n")
	}
	if len(m.state.cfg.Tools) > 0 {
		var names []string
		for _, t := range m.state.cfg.Tools {
			names = append(names, fmt.Sprintf("%s(%s)", t.Name, t.Type))
		}
		b.WriteString(wDim.Render("tools: "+strings.Join(names, " ")) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m wizardModel) reviewBody() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  project   %s\n", m.state.cfg.Project)
	fmt.Fprintf(&b, "  defaults  model=%s perms=%s\n", m.state.cfg.Defaults.Model, m.state.cfg.Defaults.PermissionMode)
	fmt.Fprintf(&b, "  agents    %d  (+ supervisor)\n", len(m.state.cfg.Agents))
	fmt.Fprintf(&b, "  tools     %d\n", len(m.state.cfg.Tools))
	return b.String()
}

func indexOf(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return 0
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
