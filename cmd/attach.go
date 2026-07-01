package cmd

import (
	"fmt"
	"os"
	"syscall"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/runner"
	"hivemind/internal/session"
)

func newAttachCmd() *cobra.Command {
	var noHistory bool
	c := &cobra.Command{
		Use:   "attach <agent>",
		Short: "Drop into the agent's live session interactively (claude --resume)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			name := args[0]
			if name != config.SupervisorName && cfg.FindAgent(name) == nil {
				return fmt.Errorf("unknown agent %q", name)
			}
			tgt, err := agent.ResolveTarget(p, cfg, name)
			if err != nil {
				return err
			}
			sid, err := session.ReadID(tgt.SessionFile)
			if err != nil {
				return fmt.Errorf("agent %q has no session id (run setup)", name)
			}
			r := pickRunner()
			bin, argv, err := r.AttachArgv(runner.AttachSpec{SessionID: sid, Workspace: tgt.Workspace, Model: tgt.Model})
			if err != nil {
				return err
			}
			if err := os.Chdir(tgt.Workspace); err != nil {
				return err
			}
			// Print the recent conversation first so the scrollback holds the prior
			// exchange (including earlier reply chunks) when you land in the session.
			if !noHistory {
				if items := agent.RecentConversation(p, cfg, name, 40); len(items) > 0 {
					fmt.Printf("──── recent conversation with %s (scroll up for more) ────\n", name)
					for _, it := range items {
						switch {
						case it.Role == "user":
							fmt.Printf("\n› you: %s\n", it.Text)
						case it.Kind == "tool_use":
							fmt.Printf("  ⎿ %s(%s)\n", it.Tool, it.Text)
						default:
							fmt.Printf("\n%s\n", it.Text)
						}
					}
					fmt.Println("\n──────────────────────────────────────────────────────────")
				}
			}
			fmt.Printf("\nattaching to %s (session %s)…\n", name, sid)
			fmt.Println("  ↩ to leave this session and return to hivemind: press Ctrl-D (or type /exit)")
			fmt.Println()
			// Replace this process with the interactive session.
			return syscall.Exec(bin, append([]string{bin}, argv...), os.Environ())
		},
	}
	c.Flags().BoolVar(&noHistory, "no-history", false, "skip printing the recent conversation before resuming")
	return c
}
