package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/tools"
)

func newLogsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs <agent|tool>",
		Short: "Tail logs for an agent (runner log) or a service tool (tmux pane)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			name := args[0]
			// Agent (or supervisor) → runner.log
			if name == config.SupervisorName || cfg.FindAgent(name) != nil {
				b, err := os.ReadFile(p.AgentLog(name))
				if err != nil {
					return fmt.Errorf("no logs for agent %q yet", name)
				}
				fmt.Print(string(b))
				return nil
			}
			// Service tool → capture the tmux pane
			if t := cfg.FindTool(name); t != nil && t.Type == config.ToolService {
				out, err := exec.Command("tmux", "capture-pane", "-p", "-t",
					tools.SessionName(p)+":tool-"+name).CombinedOutput()
				if err != nil {
					return fmt.Errorf("capture pane: %v (%s)", err, strings.TrimSpace(string(out)))
				}
				fmt.Print(string(out))
				return nil
			}
			return fmt.Errorf("%q is neither a known agent nor a service tool", name)
		},
	}
}
