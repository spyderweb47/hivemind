package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/session"
	"hivemind/internal/transcript"
)

// newCleanCmd stops everything AND clears the space: tears down the fleet, deletes
// the Claude session transcripts (freeing disk) and resets each agent to a fresh
// session, and wipes hivemind's runtime state. Config + workspaces are kept unless
// --purge. Confirms unless --yes.
func newCleanCmd() *cobra.Command {
	var purge, yes bool
	c := &cobra.Command{
		Use:   "clean",
		Short: "Stop everything and clear the space — reset sessions, wipe runtime state",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			n := len(cfg.Agents) + 1
			fmt.Println("This will:")
			fmt.Println("  • stop service tools, the heartbeat/health daemons, and the tmux session")
			fmt.Printf("  • delete %d agent session transcripts and assign fresh sessions\n", n)
			fmt.Println("  • wipe events.log, ledger.md, tasks.json, and per-agent logs")
			if purge {
				fmt.Println("  • --purge: ALSO delete every workspace, the tools dir, and .hivemind/ (the whole project)")
			}
			if !yes && !confirm("Proceed?") {
				fmt.Println("aborted.")
				return nil
			}
			freed, reset := cleanProject(p, cfg, purge)
			if purge {
				fmt.Printf("\npurged project at %s (freed ~%s). Run `hivemind` to start over.\n", p.Root, humanBytes(freed))
				return nil
			}
			fmt.Printf("\ncleaned: stopped fleet, reset %d sessions, freed ~%s of transcripts. Config + workspaces kept.\n", reset, humanBytes(freed))
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also delete workspaces, tools, and .hivemind/ (the whole project)")
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return c
}

// cleanProject stops the fleet and clears runtime state, resetting each agent to a
// fresh session (under its lock, so an in-flight turn is never reset mid-flight).
// With purge=true it additionally deletes workspaces, the tools dir, and .hivemind/.
// Returns (bytes freed, sessions reset).
func cleanProject(p paths.Project, cfg *config.Project, purge bool) (int64, int) {
	stopFleet(p, cfg, false)

	names := append([]string{config.SupervisorName}, agentNames(cfg)...)
	var freed int64
	reset := 0
	for _, name := range names {
		// Serialize against any in-flight turn by taking the agent's lock, so we
		// never reset a session mid-turn (and we leave the lock file in place —
		// removing it while a turn holds the flock would tear it).
		lock, lerr := session.NewLock(p.AgentLock(name))
		if lerr == nil {
			if err := lock.Acquire(8 * time.Second); err != nil {
				fmt.Printf("  ! %s has a turn in flight — skipping its reset\n", name)
				_ = lock.Release()
				continue
			}
		}
		if sid, err := session.ReadID(p.AgentSessionFile(name)); err == nil {
			if tp, ok := transcript.Locate(paths.ClaudeProjectsDir(), sid); ok {
				if fi, e := os.Stat(tp); e == nil {
					freed += fi.Size()
				}
				_ = os.Remove(tp)
			}
		}
		_ = session.WriteID(p.AgentSessionFile(name), session.NewID())
		reset++
		for _, f := range []string{"runner.log", "events.log", "status.json", "dispatched", "turn_task"} {
			_ = os.Remove(filepath.Join(p.AgentDir(name), f))
		}
		for _, m := range globMatch(filepath.Join(p.AgentDir(name), "prompt-*.txt")) {
			_ = os.Remove(m)
		}
		if lerr == nil {
			_ = lock.Release()
		}
	}

	for _, f := range []string{p.EventsLog(), p.Ledger(), p.TasksFile(),
		filepath.Join(p.HivemindDir(), "tasks.lock"),
		filepath.Join(p.HivemindDir(), "heartbeat.log"),
		filepath.Join(p.HivemindDir(), "health.log")} {
		_ = os.Remove(f)
	}
	_ = os.RemoveAll(p.ToolsStateDir())

	if purge {
		for _, a := range cfg.Agents {
			ws := p.WorkspaceDir(a.Workspace)
			if !withinRoot(p.Root, ws) {
				fmt.Printf("  ! refusing to delete workspace outside the project: %s\n", ws)
				continue
			}
			_ = os.RemoveAll(ws)
		}
		_ = os.RemoveAll(filepath.Join(p.HivemindDir(), "supervisor"))
		_ = os.RemoveAll(p.ToolsDir())
		_ = os.RemoveAll(p.HivemindDir())
		paths.UnregisterProject(p.Root)
	}
	return freed, reset
}

func agentNames(cfg *config.Project) []string {
	out := make([]string, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		out = append(out, a.Name)
	}
	return out
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}

func globMatch(pat string) []string {
	m, _ := filepath.Glob(pat)
	return m
}

// withinRoot reports whether path is strictly inside root (guards os.RemoveAll
// against an unsafe workspace in a hand-edited config escaping the project).
func withinRoot(root, path string) bool {
	r, err1 := filepath.Abs(root)
	pp, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil || r == pp {
		return false
	}
	return strings.HasPrefix(pp+string(filepath.Separator), r+string(filepath.Separator))
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
