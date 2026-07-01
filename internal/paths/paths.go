// Package paths centralizes every on-disk location hivemind uses, so the rest
// of the codebase never hardcodes a directory layout. Two roots matter:
//
//   - the *project root*: a hivemind-managed directory that contains .hivemind/,
//     the per-agent workspaces, and tools/. Chosen by the user at `setup`.
//   - the *claude home*: ~/.claude, where Claude Code writes session transcripts.
//
// Per the build spec we never hardcode the transcript directory hash; transcripts
// are located by globbing for a pre-assigned <session-id>.jsonl (see LocateTranscript).
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Project bundles the derived locations for one hivemind project root.
type Project struct {
	Root string
}

// NewProject returns a Project rooted at an absolute path.
func NewProject(root string) Project {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return Project{Root: abs}
}

func (p Project) HivemindDir() string   { return filepath.Join(p.Root, ".hivemind") }
func (p Project) ConfigPath() string    { return filepath.Join(p.HivemindDir(), "config.yaml") }
func (p Project) AgentsDir() string     { return filepath.Join(p.HivemindDir(), "agents") }
func (p Project) EventsLog() string     { return filepath.Join(p.HivemindDir(), "events.log") }
func (p Project) Ledger() string        { return filepath.Join(p.HivemindDir(), "ledger.md") }
func (p Project) ToolsStateDir() string { return filepath.Join(p.HivemindDir(), "tools") }
func (p Project) ToolsDir() string      { return filepath.Join(p.Root, "tools") }

// AgentDir is the control-plane state directory for one agent (session id, lock,
// events, last-status cache) — distinct from its workspace.
func (p Project) AgentDir(name string) string { return filepath.Join(p.AgentsDir(), name) }

func (p Project) AgentSessionFile(name string) string {
	return filepath.Join(p.AgentDir(name), "session_id")
}
func (p Project) AgentLock(name string) string { return filepath.Join(p.AgentDir(name), "lock") }
func (p Project) AgentEvents(name string) string {
	return filepath.Join(p.AgentDir(name), "events.log")
}
func (p Project) AgentStatusCache(name string) string {
	return filepath.Join(p.AgentDir(name), "status.json")
}
func (p Project) AgentLog(name string) string { return filepath.Join(p.AgentDir(name), "runner.log") }

// AgentTurnTask marks the task id of the turn currently running for an agent, so
// the Stop hook completes exactly that task (and a plain `send` — no marker —
// completes nothing). Written by RunTurn after it takes the lock.
func (p Project) AgentTurnTask(name string) string {
	return filepath.Join(p.AgentDir(name), "turn_task")
}

// AgentTurnPid records the PID of the detached __turn process so an in-flight turn
// can be interrupted (the process group is killed).
func (p Project) AgentTurnPid(name string) string {
	return filepath.Join(p.AgentDir(name), "turn.pid")
}

// TasksFile is the task board (delegated/manual tasks + their status).
func (p Project) TasksFile() string { return filepath.Join(p.HivemindDir(), "tasks.json") }

// WorkspaceDir is the agent's cwd. `workspace` is a relative dir name from config.
func (p Project) WorkspaceDir(workspace string) string { return filepath.Join(p.Root, workspace) }

// ToolDir is where a registered tool's files live.
func (p Project) ToolDir(name string) string { return filepath.Join(p.ToolsDir(), name) }

// ToolStateDir holds runtime state (pid, started-at) for a service tool.
func (p Project) ToolStateDir(name string) string { return filepath.Join(p.ToolsStateDir(), name) }

// FindRoot walks up from start looking for a hivemind *project* and returns the
// directory that contains it. A project is identified by .hivemind/config.yaml —
// NOT just a .hivemind directory, because the global state dir ~/.hivemind shares
// that name and would otherwise make $HOME look like a project root (so running in
// ~/anything/here would resolve to $HOME). Returns ("", false) if none is found.
func FindRoot(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".hivemind", "config.yaml")); err == nil && !fi.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// ClaudeHome returns ~/.claude (honoring CLAUDE_CONFIG_DIR if Claude Code respects it).
func ClaudeHome() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".claude")
	}
	return filepath.Join(home, ".claude")
}

// ClaudeProjectsDir is ~/.claude/projects, the parent of all per-cwd transcript dirs.
func ClaudeProjectsDir() string { return filepath.Join(ClaudeHome(), "projects") }

// GlobalDir is ~/.hivemind — global state: installed defaults + the project registry.
func GlobalDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".hivemind")
	}
	return filepath.Join(home, ".hivemind")
}

// GlobalRegistry is the file listing every project root hivemind has scaffolded.
func GlobalRegistry() string { return filepath.Join(GlobalDir(), "projects.txt") }

// RegisterProject records a project root in the global registry (idempotent).
func RegisterProject(root string) {
	abs, _ := filepath.Abs(root)
	_ = os.MkdirAll(GlobalDir(), 0o755)
	if b, err := os.ReadFile(GlobalRegistry()); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == abs {
				return // already registered
			}
		}
	}
	f, err := os.OpenFile(GlobalRegistry(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(abs + "\n")
}

// UnregisterProject removes a project root from the global registry (for `clean --purge`).
func UnregisterProject(root string) {
	abs, _ := filepath.Abs(root)
	b, err := os.ReadFile(GlobalRegistry())
	if err != nil {
		return
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if t := strings.TrimSpace(line); t != "" && t != abs {
			kept = append(kept, t)
		}
	}
	out := ""
	if len(kept) > 0 {
		out = strings.Join(kept, "\n") + "\n"
	}
	_ = os.WriteFile(GlobalRegistry(), []byte(out), 0o644)
}
