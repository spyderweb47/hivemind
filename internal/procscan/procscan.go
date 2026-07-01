// Package procscan auto-discovers the background processes that hivemind agents
// spin up — a Jupyter server, a database, a user's long-running binary — by
// inspecting live OS state and attributing each process to the agent whose
// workspace it runs in. It exists so the dashboard can show "what is actually
// running right now" beyond the registered service tools.
//
// It is strictly READ-ONLY: it never sends a signal, kills, or writes any file.
// The only files it reads are the OS process tables (via /proc on Linux, lsof+ps
// on macOS) and each service tool's recorded pane PID (to dedupe).
//
// Platform support is split by build tag (collect_linux.go / collect_darwin.go /
// collect_other.go); the discovery is dependency-free on Linux (pure /proc) so it
// works on a minimal cloud container with no lsof/ss installed. The attribution,
// dedupe, filtering, and parsing logic in this file is platform-neutral and unit
// tested on every platform.
package procscan

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/tools"
)

// Proc is one discovered background process attributed to the fleet.
type Proc struct {
	PID       int
	PPID      int
	Command   string        // short command name (e.g. "python3.11", "jupyter-lab")
	Args      string        // full argv (renderer truncates)
	CWD       string        // process working directory
	Agent     string        // owning agent name, or "fleet" (project-root but no specific workspace)
	Ports     []int         // listening TCP ports owned by this PID (sorted, deduped)
	StartedAt time.Time     // process start time
	Uptime    time.Duration // elapsed since StartedAt
	Tmux      bool          // belongs to a registered tmux service tool (deduped out by default)
}

// rawProc is the platform-neutral process record the per-OS collectors produce.
type rawProc struct {
	PID       int
	PPID      int
	Comm      string
	Args      string
	CWD       string
	StartedAt time.Time
	Uptime    time.Duration
}

// minUptime is the floor below which a process with no listening port is treated
// as a transient command (an agent ran something that's about to exit), not a
// background service worth surfacing.
const minUptime = 3 * time.Second

// denyComm drops shells, our own scan tooling, and infrastructure that is never an
// agent-spawned service. Matched against the basename of the command, lower-cased.
var denyComm = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "csh": true, "tcsh": true,
	"-bash": true, "-zsh": true, "-sh": true, "-fish": true, "login": true,
	"tmux": true, "ssh": true, "sshd": true, "ps": true, "lsof": true, "ss": true, "netstat": true,
	"grep": true, "awk": true, "sed": true, "claude": true, "hivemind": true,
}

// denyArgsSubstr drops hivemind's own detached helpers by argv substring (belt and
// suspenders alongside the comm=="hivemind" check, in case comm parsing differs).
var denyArgsSubstr = []string{"__turn", "__heartbeat", "__health"}

// Scan discovers background processes for a project. Returns nil when the platform
// has no process-inspection support (collect_other) or there is nothing to show.
// It never fails hard: partial OS data degrades to a partial (or empty) result.
func Scan(p paths.Project, cfg *config.Project) []Proc {
	if cfg == nil || !available() {
		return nil
	}
	raws, ports := collect()
	if len(raws) == 0 {
		return nil
	}
	return attribute(p, cfg, raws, ports, collectTmuxToolPIDs(p, cfg))
}

// collectTmuxToolPIDs reads each registered service tool's recorded pane PID so
// attribute can flag (and drop) auto-discovered processes that are already shown
// in the TOOLS panel — avoiding showing the same Jupyter twice.
func collectTmuxToolPIDs(p paths.Project, cfg *config.Project) map[int]bool {
	out := map[int]bool{}
	for _, t := range cfg.ServiceTools() {
		if pid := tools.ServicePID(p, t.Name); pid > 0 {
			out[pid] = true
		}
	}
	return out
}

type wsEntry struct {
	dir   string
	agent string
}

// attribute maps each PID to an owning agent by cwd-prefix (longest match wins),
// inherits attribution down the parent chain for daemons that chdir'd away,
// dedupes tmux-owned service processes, filters noise, and returns the visible
// background processes sorted by (agent, command, pid). Pure given its inputs.
func attribute(p paths.Project, cfg *config.Project, raws map[int]rawProc, ports map[int][]int, tmuxPIDs map[int]bool) []Proc {
	// Build the workspace→agent table. The OS reports a process cwd as a fully
	// symlink-resolved path (e.g. macOS /var → /private/var, or a project under any
	// symlinked dir), so we register BOTH the cleaned and the EvalSymlinks-resolved
	// form of each workspace and match against either — otherwise the prefix never
	// matches. The supervisor lives under .hivemind/supervisor; agents have their
	// configured dirs.
	var spaces []wsEntry
	for _, a := range cfg.Agents {
		for _, d := range resolveVariants(p.WorkspaceDir(a.Workspace)) {
			spaces = append(spaces, wsEntry{d, a.Name})
		}
	}
	for _, d := range resolveVariants(filepath.Join(p.HivemindDir(), "supervisor")) {
		spaces = append(spaces, wsEntry{d, config.SupervisorName})
	}
	roots := resolveVariants(p.Root)

	// Pass 1: attribute by the process's own cwd.
	agentOf := make(map[int]string, len(raws))
	for pid, r := range raws {
		agentOf[pid] = matchCwd(r.CWD, spaces, roots)
	}
	// Pass 2..N: a process with no cwd match inherits its parent's attribution.
	// This recovers a server that chdir'd to / but whose launching subtree is
	// still project-local. Iterate to a fixpoint (bounded).
	for pass := 0; pass < 8; pass++ {
		changed := false
		for pid, r := range raws {
			if agentOf[pid] != "" {
				continue
			}
			if pa := agentOf[r.PPID]; pa != "" {
				agentOf[pid] = pa
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	out := make([]Proc, 0, len(raws))
	for pid, r := range raws {
		ag := agentOf[pid]
		if ag == "" { // not under this project — not ours to show
			continue
		}
		if isNoise(r) {
			continue
		}
		pr := dedupSortPorts(ports[pid])
		// Keep only real background services: a listener, or a long-lived non-shell
		// program (the deny list already excludes shells/infra above via isNoise).
		// Fail OPEN when the start time is unknown (parse/read failed → StartedAt is
		// the zero time, Uptime 0): show the process rather than mistake it for
		// transient, since zero-uptime is otherwise indistinguishable from just-started.
		if len(pr) == 0 && !r.StartedAt.IsZero() && r.Uptime < minUptime {
			continue
		}
		if procIsTmux(pid, raws, tmuxPIDs) {
			continue // already represented in the TOOLS panel
		}
		out = append(out, Proc{
			PID: pid, PPID: r.PPID, Command: r.Comm, Args: r.Args, CWD: r.CWD,
			Agent: ag, Ports: pr, StartedAt: r.StartedAt, Uptime: r.Uptime,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		if out[i].Command != out[j].Command {
			return out[i].Command < out[j].Command
		}
		return out[i].PID < out[j].PID
	})
	return out
}

// matchCwd returns the agent owning cwd (longest workspace prefix wins), "fleet"
// if cwd is under the project root but no specific workspace, or "" if unrelated.
func matchCwd(cwd string, spaces []wsEntry, roots []string) string {
	if cwd == "" {
		return ""
	}
	cwd = filepath.Clean(cwd)
	best, bestLen := "", -1
	for _, s := range spaces {
		if underOrEqual(cwd, s.dir) && len(s.dir) > bestLen {
			best, bestLen = s.agent, len(s.dir)
		}
	}
	if best != "" {
		return best
	}
	for _, r := range roots {
		if underOrEqual(cwd, r) {
			return "fleet"
		}
	}
	return ""
}

// resolveVariants returns the cleaned path and, if different, its symlink-resolved
// form — so prefix matching works whether the OS reports a process cwd resolved or
// not. EvalSymlinks fails for a not-yet-created dir; we then use just the clean path.
func resolveVariants(in string) []string {
	c := filepath.Clean(in)
	out := []string{c}
	if r, err := filepath.EvalSymlinks(c); err == nil && r != c {
		out = append(out, r)
	}
	return out
}

// underOrEqual reports whether path is base or sits beneath it, with a path-
// boundary check so /proj/web does not match base /proj/we.
func underOrEqual(path, base string) bool {
	return path == base || strings.HasPrefix(path, base+string(os.PathSeparator))
}

// procIsTmux reports whether pid (or any ancestor) is a registered tmux service
// tool's process, so it can be deduped against the TOOLS panel.
func procIsTmux(pid int, raws map[int]rawProc, tmuxPIDs map[int]bool) bool {
	seen := map[int]bool{}
	for cur := pid; cur > 1 && !seen[cur]; {
		seen[cur] = true
		if tmuxPIDs[cur] {
			return true
		}
		r, ok := raws[cur]
		if !ok {
			break
		}
		cur = r.PPID
	}
	return false
}

// isNoise reports whether a process is hivemind's own machinery (its binary, the
// detached daemons), Claude Code, a shell, or scan infrastructure — never an
// agent-spawned background service.
func isNoise(r rawProc) bool {
	// Zombies / reaped processes (macOS ps reports their args as "<defunct>"; the
	// Linux collector drops Z-state before this). They are dead — never a service.
	if r.Comm == "<defunct>" || strings.Contains(r.Args, "<defunct>") {
		return true
	}
	for _, s := range denyArgsSubstr {
		if strings.Contains(r.Args, s) {
			return true
		}
	}
	comm := strings.ToLower(filepath.Base(r.Comm))
	if denyComm[comm] {
		return true
	}
	// Claude Code runs under node; drop node ONLY when it's the Claude process, so a
	// user's own node server is still surfaced.
	if comm == "node" && strings.Contains(r.Args, "claude") {
		return true
	}
	// tmux shows as "tmux: server"/"tmux: client" on some platforms.
	if strings.HasPrefix(comm, "tmux:") || strings.HasPrefix(r.Comm, "tmux:") {
		return true
	}
	return false
}

// scriptExts are interpreter script suffixes we surface as a friendlier name.
var scriptExts = []string{".py", ".js", ".mjs", ".ts", ".rb", ".pl", ".sh", ".jar"}

// Display returns a human-friendly service name. For a bare interpreter (python,
// node, …) several processes would all read as "python"/"node"; we instead show
// the `-m` module or the script filename pulled from Args, so the list is legible.
// Non-interpreter commands return their command name unchanged.
func (pr Proc) Display() string {
	c := strings.ToLower(pr.Command)
	interp := strings.HasPrefix(c, "python") ||
		c == "node" || c == "ruby" || c == "perl" || c == "deno" || c == "bun" || c == "java"
	if !interp {
		return pr.Command
	}
	toks := strings.Fields(pr.Args)
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		if t == "-m" && i+1 < len(toks) {
			return toks[i+1] // module: "python -m jupyterlab" → "jupyterlab"
		}
		if strings.HasPrefix(t, "-") {
			continue // a flag, not the script
		}
		low := strings.ToLower(t)
		for _, ext := range scriptExts {
			if strings.HasSuffix(low, ext) {
				return filepath.Base(t) // "node /srv/app.js" → "app.js"
			}
		}
	}
	return pr.Command
}

func dedupSortPorts(in []int) []int {
	if len(in) == 0 {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, p := range in {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}
