package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/scaffold"
	"hivemind/internal/tools"
)

// stopFleet stops service tools + the heartbeat/health daemons + the tmux session.
// Shared by `down` and `clean`.
func stopFleet(p paths.Project, cfg *config.Project, verbose bool) {
	for _, t := range cfg.ServiceTools() {
		if err := tools.Stop(p, t); err != nil {
			if verbose {
				fmt.Printf("  %-16s stop error: %v\n", t.Name, err)
			}
		} else if verbose {
			fmt.Printf("  %-16s STOPPED\n", t.Name)
		}
	}
	stopHeartbeat(p)
	stopHealth(p)
	_ = tools.KillSession(p)
}

// bringUp starts the fleet (service tools in tmux + health/heartbeat daemons).
// Idempotent — safe to call on every launch. Shared by `up` and the auto-start in
// bare `hivemind`.
func bringUp(p paths.Project, cfg *config.Project, verbose bool) {
	// Keep each agent's .claude files (permission rules, Stop hook, CLAUDE.md) in
	// sync with the current config + binary path on every launch — this also
	// upgrades projects scaffolded by an older build (e.g. picking up the
	// supervisor's hivemind allow rule). Generated content only; idempotent.
	_ = scaffold.Project(p, cfg)
	if tools.HaveTmux() {
		_ = tools.EnsureSession(p)
		for _, t := range cfg.ServiceTools() {
			if err := tools.Start(p, t); err != nil {
				if verbose {
					fmt.Printf("  %-16s FAILED: %v\n", t.Name, err)
				}
			} else if verbose {
				fmt.Printf("  %-16s %s\n", t.Name, tools.Observe(p, t).State)
			}
		}
	} else if verbose && len(cfg.ServiceTools()) > 0 {
		fmt.Println("  tmux not found — skipping service tools")
	}
	if startHealth(p, cfg) && verbose {
		fmt.Println("  health-monitor   on (auto-restart for restart:on-failure tools)")
	}
	if startHeartbeat(p, cfg) && verbose {
		fmt.Printf("  heartbeat        every %dm (supervisor digest)\n", cfg.Supervisor.Report.HeartbeatMinutes)
	}
}

func newUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Start the project: service tools (in tmux) + health/heartbeat daemons",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			bringUp(p, cfg, true)
			fmt.Println("\nfleet is up. Open the console with: hivemind")
			return nil
		},
	}
}

func newDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop the project: stop service tools and tear down the tmux session",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			stopFleet(p, cfg, true)
			fmt.Println("fleet is down.")
			return nil
		},
	}
}
