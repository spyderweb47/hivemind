package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
)

// newInterruptCmd stops an agent's in-flight turn by killing its detached __turn
// process group (the console binds this to `esc`). Idempotent: a no-op if the
// agent is idle.
func newInterruptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "interrupt <agent>",
		Short: "Stop an agent's in-flight turn (kills the running claude process)",
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
			if agent.Interrupt(p, name) {
				fmt.Printf("✘ interrupted %s\n", name)
			} else {
				fmt.Printf("%s has no turn running (nothing to interrupt)\n", name)
			}
			return nil
		},
	}
}
