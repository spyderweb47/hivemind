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
	return &cobra.Command{
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
			fmt.Printf("attaching to %s (session %s)…\n", name, sid)
			// Replace this process with the interactive session.
			return syscall.Exec(bin, append([]string{bin}, argv...), os.Environ())
		},
	}
}
