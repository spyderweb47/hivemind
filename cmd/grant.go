package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/scaffold"
)

// newGrantCmd grants raw Claude permission rules to one agent and resumes it.
// This is the control-plane action behind the dashboard's permission prompt:
// when an agent reports BLOCKED needing a capability (e.g. WebFetch/WebSearch),
// the user grants it here and the agent is re-dispatched to continue. Rules are
// added verbatim to the agent's permissions.allow and persisted to config.
func newGrantCmd() *cobra.Command {
	var noResume bool
	c := &cobra.Command{
		Use:   "grant <agent> <rule...>",
		Short: "Grant permission rule(s) to an agent and resume it (e.g. grant romeo WebFetch WebSearch)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			name := args[0]
			if name == config.SupervisorName || cfg.FindAgent(name) == nil {
				return fmt.Errorf("unknown worker agent %q", name)
			}
			rules := args[1:]
			added, err := scaffold.GrantPermissions(p, cfg, name, rules)
			if err != nil {
				return err
			}
			if len(added) > 0 {
				fmt.Printf("✔ granted to %s: %s\n", name, strings.Join(added, ", "))
			} else {
				fmt.Printf("%s already had: %s (no change)\n", name, strings.Join(rules, ", "))
			}
			if noResume {
				return nil
			}
			// Resume: nudge the agent to continue. Tell it the truth — only what was
			// newly granted, or that it already had the requested capability (so it
			// can clarify what it actually needs instead of re-blocking on the same).
			var prompt string
			if len(added) > 0 {
				prompt = fmt.Sprintf("Permissions updated — you now have access to: %s. "+
					"Please retry and continue your task.", strings.Join(added, ", "))
			} else {
				prompt = fmt.Sprintf("You already have access to: %s. If you are still blocked, "+
					"state precisely what additional capability you need.", strings.Join(rules, ", "))
			}
			if !flagFake {
				if err := pickRunner().Available(); err != nil {
					fmt.Printf("(granted, but not resuming: %v)\n", err)
					return nil
				}
			}
			if err := agent.Dispatch(p, name, prompt, flagFake, ""); err != nil {
				return fmt.Errorf("granted, but resume dispatch failed: %w", err)
			}
			fmt.Printf("→ resumed %s\n", name)
			return nil
		},
	}
	c.Flags().BoolVar(&noResume, "no-resume", false, "grant the permission(s) but do not re-dispatch the agent")
	return c
}
