package cmd

import (
	"errors"
	"io/fs"
	"time"

	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/paths"
)

// __heartbeat is the detached daemon started by `up`. Every heartbeat_minutes it
// wakes the supervisor to summarize the fleet. It re-reads config each cycle so a
// changed interval (or disabling it) takes effect, and tolerates transient errors.
func newHeartbeatCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:    "__heartbeat",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := paths.NewProject(root)
			for {
				cfg, err := config.Load(p.ConfigPath())
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil // config removed → stop cleanly
					}
					time.Sleep(30 * time.Second)
					continue
				}
				mins := cfg.Supervisor.Report.HeartbeatMinutes
				if mins <= 0 {
					return nil // heartbeat disabled
				}
				time.Sleep(time.Duration(mins) * time.Minute)
				cfg2, err := config.Load(p.ConfigPath())
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err == nil {
					if cfg2.Supervisor.Report.HeartbeatMinutes <= 0 {
						return nil
					}
					_ = supervisorReport(p, cfg2, "heartbeat")
				}
			}
		},
	}
	c.Flags().StringVar(&root, "root", "", "project root")
	return c
}

func startHeartbeat(p paths.Project, cfg *config.Project) bool {
	if cfg.Supervisor.Report.HeartbeatMinutes <= 0 {
		return false
	}
	return startDaemon(p, "heartbeat")
}

func stopHeartbeat(p paths.Project) { stopDaemon(p, "heartbeat") }
