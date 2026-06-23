package cmd

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/dashboard"
	"hivemind/internal/paths"
)

func newDashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "dashboard",
		Aliases: []string{"console"},
		Short:   "Launch the live console — manage agents & tools interactively",
		RunE: func(cmd *cobra.Command, args []string) error {
			return launchConsole()
		},
	}
}

// launchConsole opens the interactive console for the resolved project.
func launchConsole() error {
	p, cfg, err := openProject()
	if err != nil {
		return err
	}
	return launchConsoleFor(p, cfg)
}

// launchConsoleFor runs the Bubble Tea console for an already-resolved project.
func launchConsoleFor(p paths.Project, cfg *config.Project) error {
	m := dashboard.New(p, cfg, flagFake)
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if c, ok := final.(interface{ Close() error }); ok {
		_ = c.Close() // release the fsnotify watcher
	}
	return err
}
