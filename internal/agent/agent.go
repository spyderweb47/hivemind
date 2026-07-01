// Package agent ties the control-plane pieces together for a single agent: it
// derives an agent's live state from its transcript + lock (no LLM involved), and
// it dispatches prompts as detached, lock-serialized turns.
package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hivemind/internal/config"
	"hivemind/internal/events"
	"hivemind/internal/paths"
	"hivemind/internal/runner"
	"hivemind/internal/scaffold"
	"hivemind/internal/session"
	"hivemind/internal/transcript"
)

// States (derived deterministically — never via an LLM).
const (
	StateWorking = "WORKING"
	StateIdle    = "IDLE"
	StateError   = "ERROR"
	StateBlocked = "BLOCKED"
	StateNew     = "NEW" // session never started
)

// FreshWindow is how recently the transcript must have advanced to count as
// WORKING when the per-agent lock is NOT held. During a turn the lock is the
// authoritative "working" signal (held for the whole `claude -p` process); this
// window only governs the short "just finished / settling" tail after a turn and
// the fallback case where a session is driven outside hivemind's lock.
const FreshWindow = 10 * time.Second

// Target resolves where/how to run an agent (works for workers and the supervisor).
type Target struct {
	Name           string
	Workspace      string // absolute
	Model          string
	PermissionMode string
	SessionFile    string
	LockPath       string
	AddDirs        []string // absolute read-only grants (--add-dir)
}

// View is the derived dashboard/status row for an agent.
type View struct {
	Name         string
	State        string
	SessionID    string
	LastActivity time.Time
	CurrentTask  string
	Summary      string // last push event's turn summary (Stop-hook last message)
	FullMessage  string // last assistant text, untruncated (for the detail overlay)
	Model        string
	Tokens       int     // total (in+out+cache)
	InTokens     int     // input + cache (read/creation)
	OutTokens    int     // output
	CostUSD      float64 // estimate, kept for --json
	TranscriptOK bool
}

// ResolveTarget builds a Target for a worker agent or the fixed supervisor.
func ResolveTarget(p paths.Project, cfg *config.Project, name string) (Target, error) {
	t := Target{
		Name:           name,
		SessionFile:    p.AgentSessionFile(name),
		LockPath:       p.AgentLock(name),
		PermissionMode: cfg.EffectivePermissionModeFor(name),
	}
	if name == config.SupervisorName {
		t.Workspace = filepath.Join(p.HivemindDir(), "supervisor")
		t.Model = cfg.Supervisor.Model
		if t.Model == "" {
			t.Model = "haiku"
		}
		return t, nil
	}
	a := cfg.FindAgent(name)
	if a == nil {
		return t, fmt.Errorf("unknown agent %q", name)
	}
	t.Workspace = p.WorkspaceDir(a.Workspace)
	t.Model = cfg.EffectiveModel(a)
	for _, r := range a.Reads {
		t.AddDirs = append(t.AddDirs, p.WorkspaceDir(r))
	}
	return t, nil
}

// Observe derives the View for one agent from disk state.
func Observe(p paths.Project, cfg *config.Project, name string) View {
	v := View{Name: name, State: StateNew}
	tgt, err := ResolveTarget(p, cfg, name)
	if err != nil {
		v.State = "UNKNOWN"
		return v
	}
	v.Model = tgt.Model
	sid, err := session.ReadID(tgt.SessionFile)
	if err != nil {
		return v // NEW: no session id yet
	}
	v.SessionID = sid

	tpath, ok := transcript.Locate(paths.ClaudeProjectsDir(), sid)
	if !ok {
		// Session assigned but not yet started. A turn may already be dispatched
		// and racing to acquire the lock — count that as WORKING.
		if session.Held(tgt.LockPath) || dispatchPending(p, name) {
			v.State = StateWorking
		}
		return v
	}
	sum, _ := transcript.Parse(tpath)
	v.TranscriptOK = sum.Exists
	v.LastActivity = sum.LastEventTime
	v.Tokens = sum.TotalTokens()
	v.InTokens = sum.InputTokens + sum.CacheRead + sum.CacheCreate
	v.OutTokens = sum.OutputTokens
	v.CostUSD = EstimateCost(tgt.Model, sum)
	v.CurrentTask = describeActivity(sum)
	v.FullMessage = sum.LastTextFull
	// The Stop-hook push (real runner only) gives a human-readable turn summary.
	if ev, ok := events.LatestAny(p.AgentEvents(name)); ok {
		v.Summary = ev.Summary
	}

	locked := session.Held(tgt.LockPath)
	dispatched := dispatchPending(p, name)
	// Clamp recency to a non-negative window so a future-dated transcript
	// timestamp (clock skew) cannot pin the agent to WORKING forever.
	d := time.Since(sum.LastEventTime)
	fresh := !sum.LastEventTime.IsZero() && d >= 0 && d < FreshWindow
	switch {
	case sum.NeedsInput:
		v.State = StateBlocked // explicit needs-input beats everything
	case locked || dispatched:
		v.State = StateWorking // a turn is in flight (authoritative)
	case sum.Errored:
		v.State = StateError // a terminal error beats the freshness tail
	case fresh:
		v.State = StateWorking // recently active (settling tail)
	default:
		v.State = StateIdle
	}
	return v
}

// dispatchMarker is touched by Dispatch and removed once the turn takes the lock,
// closing the brief liveness gap between `send` returning and the child locking.
const dispatchMarker = "dispatched"

func dispatchPending(p paths.Project, name string) bool {
	fi, err := os.Stat(filepath.Join(p.AgentDir(name), dispatchMarker))
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) < FreshWindow
}

func describeActivity(s transcript.Summary) string {
	if s.LastTool != "" {
		if s.LastToolInput != "" {
			return s.LastTool + " " + s.LastToolInput
		}
		return s.LastTool
	}
	if s.LastText != "" {
		return s.LastText
	}
	return ""
}

// RunTurn executes a single prompt synchronously: acquire the per-agent lock,
// resume-or-create the session, run the turn to completion, release the lock.
// This is the body of the hidden `__turn` command (so the lock lives in one place).
func RunTurn(p paths.Project, cfg *config.Project, r runner.Runner, name, prompt, taskID string) error {
	tgt, err := ResolveTarget(p, cfg, name)
	if err != nil {
		return err
	}
	sid, err := session.ReadID(tgt.SessionFile)
	if err != nil {
		return fmt.Errorf("agent %q has no session id (run setup): %w", name, err)
	}
	if err := os.MkdirAll(tgt.Workspace, 0o755); err != nil {
		return err
	}
	// Self-heal: the permission mode + read-only deny rules live ONLY in the
	// workspace .claude/settings.json (we no longer pass --permission-mode, which
	// would bypass deny). If it's missing (deleted, or a project scaffolded by an
	// older build), regenerate it before the turn so edits aren't silently broken.
	if _, err := os.Stat(filepath.Join(tgt.Workspace, ".claude", "settings.json")); err != nil {
		_ = scaffold.Agent(p, cfg, name)
	}

	lock, err := session.NewLock(tgt.LockPath)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := lock.Acquire(0); err != nil { // block until free (queue behind in-flight)
		return err
	}
	// The turn now holds the lock; the dispatch marker has served its purpose.
	_ = os.Remove(filepath.Join(p.AgentDir(name), dispatchMarker))
	defer os.Remove(p.AgentTurnPid(name)) // clear the interrupt handle when done

	// Mark which task (if any) this specific turn is running, so the Stop hook
	// completes exactly that task. Written under the lock so each serialized turn
	// stamps its own id; cleared at the end of the turn.
	if taskID != "" {
		_ = os.WriteFile(p.AgentTurnTask(name), []byte(taskID), 0o644)
		defer os.Remove(p.AgentTurnTask(name))
	}

	_, started := transcript.Locate(paths.ClaudeProjectsDir(), sid)

	logf, err := os.OpenFile(p.AgentLog(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "\n=== %s | turn @ %s ===\n> %s\n", name, time.Now().Format(time.RFC3339), prompt)

	spec := runner.PromptSpec{
		Agent:          name,
		SessionID:      sid,
		Workspace:      tgt.Workspace,
		Model:          tgt.Model,
		PermissionMode: tgt.PermissionMode,
		AddDirs:        tgt.AddDirs,
		Prompt:         prompt,
		SessionStarted: started,
	}
	return r.Send(spec, logf)
}

// RecentConversation returns up to the last n conversation items from an agent's
// transcript (user prompts + every assistant text chunk + tool calls), for the
// dashboard's scrollable detail view. Empty if the session hasn't started.
func RecentConversation(p paths.Project, cfg *config.Project, name string, n int) []transcript.Item {
	tgt, err := ResolveTarget(p, cfg, name)
	if err != nil {
		return nil
	}
	sid, err := session.ReadID(tgt.SessionFile)
	if err != nil {
		return nil
	}
	tpath, ok := transcript.Locate(paths.ClaudeProjectsDir(), sid)
	if !ok {
		return nil
	}
	items, _ := transcript.Recent(tpath, n)
	return items
}

// Interrupt kills an agent's in-flight turn by signaling the detached __turn
// process group (Setsid made __turn the group leader, so claude is in it too) and
// clears the markers so the agent settles back to IDLE/ERROR. Returns true if a
// turn was actually running. The PID is verified against `ps` first so a stale
// pidfile whose PID has been recycled can't kill an unrelated process group.
func Interrupt(p paths.Project, name string) bool {
	b, err := os.ReadFile(p.AgentTurnPid(name))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	hit := false
	if err == nil && pid > 1 && turnAlive(pid) {
		// Negative PID targets the whole process group: __turn + its claude child.
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		hit = true
	}
	_ = os.Remove(p.AgentTurnPid(name))
	_ = os.Remove(filepath.Join(p.AgentDir(name), dispatchMarker))
	_ = os.Remove(p.AgentTurnTask(name))
	return hit
}

// turnAlive reports whether pid is still one of our __turn processes (guards
// against PID reuse before we signal a process group).
func turnAlive(pid int) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "__turn")
}

// HumanInt formats a token count compactly and within ~5 display chars (1234 →
// "1.2k", 120000 → "120k", 1.2e6 → "1.2m"). Boundaries are chosen post-rounding so
// a value never renders as e.g. "1000.0k" and overflows a fixed-width column.
func HumanInt(n int) string {
	switch {
	case n < 0:
		return "0"
	case n >= 999_500:
		return fmt.Sprintf("%.1fm", float64(n)/1e6)
	case n >= 99_950:
		return fmt.Sprintf("%dk", (n+500)/1000) // 100k..999k, no decimal
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return strconv.Itoa(n)
	}
}

// EstimateCost converts token usage into a rough USD figure using a small price
// table keyed by model family. Used only for display; the token totals are exact.
func EstimateCost(model string, s transcript.Summary) float64 {
	in, out, cache := pricePerMTok(model)
	return (float64(s.InputTokens)*in +
		float64(s.OutputTokens)*out +
		float64(s.CacheRead+s.CacheCreate)*cache) / 1_000_000
}

// pricePerMTok returns (input, output, cached) USD per million tokens.
func pricePerMTok(model string) (float64, float64, float64) {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "opus"):
		return 15, 75, 1.5
	case strings.Contains(m, "haiku"):
		return 0.80, 4, 0.08
	default: // sonnet / unknown
		return 3, 15, 0.30
	}
}
