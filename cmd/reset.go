package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newResetCmd force-stops the fleet, resets all sessions (clean), and restarts it
// fresh in the same directory — the "start over here" button. With --purge it
// deletes the project entirely (next `hivemind` re-runs setup).
func newResetCmd() *cobra.Command {
	var purge, yes bool
	c := &cobra.Command{
		Use:   "reset",
		Short: "Force-stop + reset all sessions and restart the fleet fresh (same directory)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			if !yes {
				if purge {
					fmt.Println("This DELETES the project (workspaces + config).")
				} else {
					fmt.Println("This force-stops the fleet and resets all agent sessions (config kept).")
				}
				if !confirm("Proceed?") {
					fmt.Println("aborted.")
					return nil
				}
			}
			freed, n := cleanProject(p, cfg, purge)
			if purge {
				fmt.Printf("project deleted (freed ~%s). Run `hivemind` to set up again.\n", humanBytes(freed))
				return nil
			}
			fmt.Printf("reset %d sessions, freed ~%s — restarting fleet…\n", n, humanBytes(freed))
			bringUp(p, cfg, true)
			fmt.Println("fresh fleet is up. Open the console with: hivemind")
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "delete the whole project instead of just resetting")
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return c
}
