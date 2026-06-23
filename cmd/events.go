package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/events"
)

// events shows the Stop-hook push feed — one line per completed turn. This is the
// "report after each prompt" stream the supervisor also consumes.
func newEventsCmd() *cobra.Command {
	var tail int
	c := &cobra.Command{
		Use:   "events [agent]",
		Short: "Show the per-turn push feed (Stop-hook events)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			logPath := p.EventsLog()
			if len(args) == 1 {
				name := args[0]
				if name != config.SupervisorName && cfg.FindAgent(name) == nil {
					return fmt.Errorf("unknown agent %q", name)
				}
				logPath = p.AgentEvents(name)
			}
			evs := events.Tail(logPath, tail)
			if len(evs) == 0 {
				fmt.Println("(no push events yet — these are emitted by the Stop hook on real `claude` turns)")
				return nil
			}
			for _, e := range evs {
				flag := "   "
				if e.Blocked {
					flag = "BLK"
				} else if e.Errored {
					flag = "ERR"
				}
				fmt.Printf("%s  %-3s  %-12s  in:%-6s out:%-6s  %s\n",
					e.TS.Local().Format("15:04:05"), flag, e.Agent,
					agent.HumanInt(e.InTokens), agent.HumanInt(e.OutTokens), oneLine(trunc(e.Summary, 70)))
			}
			return nil
		},
	}
	c.Flags().IntVar(&tail, "tail", 20, "number of events to show")
	return c
}
