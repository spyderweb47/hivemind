package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/scaffold"
)

// newRemoveCmd implements `hivemind remove agent <name>` — completely destroy an
// agent: its config entry, workspace, and control-plane state. The inverse of
// `hivemind add agent`.
func newRemoveCmd() *cobra.Command {
	c := &cobra.Command{Use: "remove", Short: "Remove an agent (destroys its config, workspace, and state)", Aliases: []string{"rm"}}
	c.AddCommand(newRemoveAgentCmd())
	return c
}

func newRemoveAgentCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "agent <name>",
		Short: "Completely remove an agent: config entry + workspace + session/state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			defer lockConfig(p)()
			if cfg, err = config.Load(p.ConfigPath()); err != nil { // re-read under the lock
				return err
			}
			name := args[0]
			if name == config.SupervisorName {
				return fmt.Errorf("the supervisor cannot be removed")
			}
			a := cfg.FindAgent(name)
			if a == nil {
				return fmt.Errorf("unknown agent %q", name)
			}
			ws := p.WorkspaceDir(a.Workspace)
			if !yes && !confirm(fmt.Sprintf("Permanently remove agent %q — config, workspace %s, and all state?", name, ws)) {
				fmt.Println("aborted.")
				return nil
			}

			// 1. Stop any in-flight turn (kills the detached __turn process group).
			if agent.Interrupt(p, name) {
				fmt.Printf("interrupted %s's in-flight turn\n", name)
			}
			// 2. Drop the agent from config and persist.
			var kept []config.Agent
			for _, x := range cfg.Agents {
				if x.Name != name {
					kept = append(kept, x)
				}
			}
			cfg.Agents = kept
			if err := config.Save(p.ConfigPath(), cfg); err != nil {
				return err
			}
			// 3. Delete the workspace (guarded so a hand-edited config can't escape root).
			if withinRoot(p.Root, ws) {
				if err := os.RemoveAll(ws); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not delete workspace %s: %v\n", ws, err)
				}
			} else {
				fmt.Fprintf(os.Stderr, "warning: workspace %s is outside the project root; left in place\n", ws)
			}
			// 4. Delete the control-plane dir (session id, lock, events, markers).
			_ = os.RemoveAll(p.AgentDir(name))
			// 5. Re-scaffold the supervisor so its CLAUDE.md no longer lists this agent.
			// Non-fatal: the agent is already gone from config + disk by this point,
			// so a re-scaffold hiccup must not abort and leave a half-done removal.
			if err := scaffold.Agent(p, cfg, config.SupervisorName); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not refresh supervisor CLAUDE.md: %v\n", err)
			}
			// 6. Warn about now-dangling references in other agents.
			for _, other := range cfg.Agents {
				for _, r := range other.Reads {
					if r == a.Workspace || strings.HasPrefix(r, a.Workspace+"/") {
						fmt.Fprintf(os.Stderr, "note: agent %q still has a read on %q (now gone) — edit it with `hivemind edit %s`\n", other.Name, r, other.Name)
					}
				}
			}
			fmt.Printf("removed agent %q\n", name)
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return c
}
