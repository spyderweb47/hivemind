package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/tools"
)

// healthInterval is how often the health daemon probes service tools.
const healthInterval = 15 * time.Second

// __health is the detached daemon started by `up`. On an interval it health-checks
// service tools and auto-restarts any with `restart: on-failure` that are
// STOPPED/UNHEALTHY (the spec's restart policy). Re-reads config each cycle.
func newHealthCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:    "__health",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := paths.NewProject(root)
			for {
				time.Sleep(healthInterval)
				cfg, err := config.Load(p.ConfigPath())
				if errors.Is(err, fs.ErrNotExist) {
					return nil // project gone → stop
				}
				if err != nil {
					continue // transient → retry next cycle
				}
				if r := tools.SuperviseOnce(p, cfg); len(r) > 0 {
					fmt.Printf("%s restarted %v\n", time.Now().Format(time.RFC3339), r)
				}
			}
		},
	}
	c.Flags().StringVar(&root, "root", "", "project root")
	return c
}

// hasOnFailureTool reports whether any service tool opts into auto-restart.
func hasOnFailureTool(cfg *config.Project) bool {
	for _, t := range cfg.ServiceTools() {
		if t.Restart == "on-failure" {
			return true
		}
	}
	return false
}

func startHealth(p paths.Project, cfg *config.Project) bool {
	if !hasOnFailureTool(cfg) {
		return false
	}
	return startDaemon(p, "health")
}

func stopHealth(p paths.Project) { stopDaemon(p, "health") }
