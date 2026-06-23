package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"hivemind/internal/wizard"
)

func newSetupCmd() *cobra.Command {
	var preset string
	c := &cobra.Command{
		Use:   "setup",
		Short: "Interactive onboarding wizard — the only thing needed to bootstrap",
		RunE: func(cmd *cobra.Command, args []string) error {
			root := resolveRoot()
			if preset != "" {
				if _, err := wizard.RunPreset(root, preset, os.Stdout); err != nil {
					return err
				}
				fmt.Printf("\n  Project ready at %s.\n  Next: hivemind up   then   hivemind dashboard\n", root)
				return nil
			}
			_, finalRoot, err := wizard.RunInteractive(root)
			if err != nil {
				if errors.Is(err, wizard.ErrCancelled) {
					fmt.Println("setup cancelled.")
					return nil
				}
				return err
			}
			fmt.Printf("\n  Project ready at %s.\n  Next: hivemind up   then   hivemind dashboard\n", finalRoot)
			return nil
		},
	}
	c.Flags().StringVar(&preset, "preset", "", "non-interactive: load answers from a preset YAML file")
	return c
}
