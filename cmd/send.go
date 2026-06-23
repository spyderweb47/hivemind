package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
)

func newSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <agent> <prompt...>",
		Short: "Prompt an agent (headless resume; non-blocking; lock-serialized)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			name := args[0]
			prompt := strings.Join(args[1:], " ")
			if name != config.SupervisorName && cfg.FindAgent(name) == nil {
				return fmt.Errorf("unknown agent %q", name)
			}
			if !flagFake {
				if err := pickRunner().Available(); err != nil {
					return err
				}
			}
			if err := agent.Dispatch(p, name, prompt, flagFake, ""); err != nil {
				return err
			}
			fmt.Printf("→ dispatched to %s (running detached). Watch: hivemind status | hivemind transcript %s\n", name, name)
			return nil
		},
	}
}
