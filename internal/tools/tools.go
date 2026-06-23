// Package tools manages service-type tools: long-running processes that hivemind
// starts in dedicated tmux windows, health-probes on an interval, and surfaces on
// the dashboard as RUNNING / UNHEALTHY / STOPPED with uptime and ports. Library
// and command tools have no process and are not managed here.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"hivemind/internal/config"
	"hivemind/internal/paths"
)

// State names shown on the dashboard.
const (
	StateRunning   = "RUNNING"
	StateUnhealthy = "UNHEALTHY"
	StateStopped   = "STOPPED"
)

// Status is the observed runtime status of a service tool.
type Status struct {
	Name   string
	Owner  string
	State  string
	Uptime time.Duration
	PID    int
	Ports  []int
}

// persisted runtime state for a service tool.
type state struct {
	Window     string    `json:"window"`
	Entrypoint string    `json:"entrypoint"`
	StartedAt  time.Time `json:"started_at"`
	PID        int       `json:"pid"`
}

// SessionName returns a stable, project-scoped tmux session name so multiple
// hivemind projects on one host do not collide.
func SessionName(p paths.Project) string {
	h := fnv.New32a()
	h.Write([]byte(p.Root))
	return fmt.Sprintf("hivemind_%08x", h.Sum32())
}

func windowName(tool string) string { return "tool-" + tool }

func tmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// HaveTmux reports whether tmux is on PATH.
func HaveTmux() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// KillSession tears down the project's tmux session (used by `hivemind down`).
func KillSession(p paths.Project) error {
	_, err := tmux("kill-session", "-t", SessionName(p))
	return err
}

// EnsureSession creates the detached tmux session if it does not exist.
func EnsureSession(p paths.Project) error {
	sess := SessionName(p)
	if _, err := tmux("has-session", "-t", sess); err == nil {
		return nil
	}
	_, err := tmux("new-session", "-d", "-s", sess, "-n", "hivemind")
	return err
}

func loadState(p paths.Project, tool string) (state, bool) {
	var st state
	b, err := os.ReadFile(filepath.Join(p.ToolStateDir(tool), "state.json"))
	if err != nil {
		return st, false
	}
	if json.Unmarshal(b, &st) != nil {
		return st, false
	}
	return st, true
}

func saveState(p paths.Project, tool string, st state) error {
	dir := p.ToolStateDir(tool)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(filepath.Join(dir, "state.json"), b, 0o644)
}

// WindowExists reports whether the tool's tmux window is present.
func WindowExists(p paths.Project, tool string) bool {
	out, err := tmux("list-windows", "-t", SessionName(p), "-F", "#{window_name}")
	if err != nil {
		return false
	}
	for _, w := range strings.Split(out, "\n") {
		if w == windowName(tool) {
			return true
		}
	}
	return false
}

// Start launches a service tool in its own tmux window, recording start time/PID.
func Start(p paths.Project, t config.Tool) error {
	if t.Type != config.ToolService {
		return fmt.Errorf("tool %q is not a service", t.Name)
	}
	if t.Entrypoint == "" {
		return fmt.Errorf("service tool %q has no entrypoint", t.Name)
	}
	if err := EnsureSession(p); err != nil {
		return fmt.Errorf("tmux session: %w", err)
	}
	if WindowExists(p, t.Name) {
		return nil // already running
	}
	dir := p.ToolDir(t.Name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("tool dir missing: %s", dir)
	}
	sess := SessionName(p)
	win := windowName(t.Name)
	// We deliberately do NOT set remain-on-exit: when the service process exits or
	// is killed, the window closes and the tool reads as STOPPED. UNHEALTHY is
	// reserved for "process alive but health probe failing".
	if _, err := tmux("new-window", "-d", "-t", sess, "-n", win, "-c", dir, t.Entrypoint); err != nil {
		return fmt.Errorf("start window: %w", err)
	}
	pid := 0
	if out, err := tmux("display-message", "-p", "-t", sess+":"+win, "#{pane_pid}"); err == nil {
		pid, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	return saveState(p, t.Name, state{Window: win, Entrypoint: t.Entrypoint, StartedAt: time.Now(), PID: pid})
}

// Stop kills the tool's tmux window.
func Stop(p paths.Project, t config.Tool) error {
	if WindowExists(p, t.Name) {
		if _, err := tmux("kill-window", "-t", SessionName(p)+":"+windowName(t.Name)); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Join(p.ToolStateDir(t.Name), "state.json"))
	return nil
}

// Restart is Stop followed by Start.
func Restart(p paths.Project, t config.Tool) error {
	_ = Stop(p, t)
	time.Sleep(300 * time.Millisecond)
	return Start(p, t)
}

// Probe runs the tool's health command; nil means healthy (exit 0).
func Probe(t config.Tool) error {
	if t.Health == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", t.Health)
	return cmd.Run()
}

// SuperviseOnce restarts any service tool whose policy is `restart: on-failure`
// and which is currently STOPPED or UNHEALTHY. Returns the names it restarted.
// This is the loop body of the `__health` daemon (started by `up`).
func SuperviseOnce(p paths.Project, cfg *config.Project) []string {
	var restarted []string
	for _, t := range cfg.ServiceTools() {
		if t.Restart != "on-failure" {
			continue
		}
		switch Observe(p, t).State {
		case StateStopped, StateUnhealthy:
			if Restart(p, t) == nil {
				restarted = append(restarted, t.Name)
			}
		}
	}
	return restarted
}

// Observe returns the live status of a service tool.
func Observe(p paths.Project, t config.Tool) Status {
	s := Status{Name: t.Name, Owner: t.Owner, Ports: t.Ports, State: StateStopped}
	if !WindowExists(p, t.Name) {
		return s
	}
	if st, ok := loadState(p, t.Name); ok {
		s.PID = st.PID
		if !st.StartedAt.IsZero() {
			s.Uptime = time.Since(st.StartedAt)
		}
	}
	if err := Probe(t); err != nil {
		s.State = StateUnhealthy
		return s
	}
	s.State = StateRunning
	return s
}
