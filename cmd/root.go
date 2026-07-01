// Package cmd wires the hivemind CLI surface (cobra). Every command resolves a
// project root (--root, else the nearest ancestor containing .hivemind, else cwd),
// loads config, and picks a runner (real claude or --fake).
package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/runner"
	"hivemind/internal/scaffold"
	"hivemind/internal/wizard"
)

// Version is reported by `hivemind --version`.
const Version = "0.2.0-m2"

var (
	flagRoot string
	flagFake bool
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "hivemind",
		Version: Version,
		Short:   "Deploy and manage a fleet of Claude Code agents",
		Long: "hivemind deploys and supervises a fleet of Claude Code agents — each a\n" +
			"persistent, role-bound session with its own workspace and tools — from a\n" +
			"single static binary. Run `hivemind setup` once, then bare `hivemind` opens\n" +
			"the interactive console to manage everything.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Bare `hivemind` does everything: set up if needed, auto-start, open console.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return cmd.Help()
			}
			return runUnified()
		},
	}
	root.PersistentFlags().StringVar(&flagRoot, "root", "", "project root (default: nearest .hivemind or cwd)")
	root.PersistentFlags().BoolVar(&flagFake, "fake", false, "use the fake runner (no real claude; for testing/demo)")

	root.AddCommand(
		newSetupCmd(),
		newStatusCmd(),
		newSendCmd(),
		newInterruptCmd(),
		newGrantCmd(),
		newAttachCmd(),
		newTranscriptCmd(),
		newToolCmd(),
		newUpCmd(),
		newDownCmd(),
		newDashboardCmd(),
		newLogsCmd(),
		newReportCmd(),
		newEventsCmd(),
		newDelegateCmd(),
		newTaskCmd(),
		newTasksCmd(),
		newAddCmd(),
		newRemoveCmd(),
		newEditCmd(),
		newCleanCmd(),
		newResetCmd(),
		newTurnCmd(),      // hidden
		newHookStopCmd(),  // hidden
		newHeartbeatCmd(), // hidden
		newHealthCmd(),    // hidden
	)
	return root
}

// Execute is the program entry point.
func Execute() {
	if exe, err := os.Executable(); err == nil {
		scaffold.HivemindBin = exe
	}
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// runUnified is the bare `hivemind` flow: auto-detect the project (run setup if
// absent), auto-start the fleet (idempotent), then open the console. The single
// command — no separate setup/up/down needed.
func runUnified() error {
	root := resolveRoot()
	p := paths.NewProject(root)
	if _, err := os.Stat(p.ConfigPath()); err != nil {
		// No fleet configured here → onboarding wizard, then continue.
		_, finalRoot, err := wizard.RunInteractive(root)
		if err != nil {
			if errors.Is(err, wizard.ErrCancelled) {
				fmt.Println("setup cancelled.")
				return nil
			}
			return err
		}
		p = paths.NewProject(finalRoot)
	}
	cfg, err := config.Load(p.ConfigPath())
	if err != nil {
		return err
	}
	bringUp(p, cfg, false) // ensure the fleet is running (idempotent)
	return launchConsoleFor(p, cfg)
}

// resolveRoot returns the effective project root.
func resolveRoot() string {
	if flagRoot != "" {
		abs, _ := filepath.Abs(flagRoot)
		return abs
	}
	cwd, _ := os.Getwd()
	if r, ok := paths.FindRoot(cwd); ok {
		return r
	}
	return cwd
}

// openProject loads the project (root + parsed config), erroring if not set up.
func openProject() (paths.Project, *config.Project, error) {
	root := resolveRoot()
	p := paths.NewProject(root)
	if _, err := os.Stat(p.ConfigPath()); err != nil {
		return p, nil, fmt.Errorf("no hivemind project at %s — run `hivemind setup` first", root)
	}
	cfg, err := config.Load(p.ConfigPath())
	if err != nil {
		return p, nil, err
	}
	return p, cfg, nil
}

// pickRunner selects a runner, honoring --fake / HIVEMIND_FAKE_RUNNER.
func pickRunner() runner.Runner { return runner.Select(flagFake) }
