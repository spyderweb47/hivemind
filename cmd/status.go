package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/tools"
)

type statusReport struct {
	Project string         `json:"project"`
	Agents  []agent.View   `json:"agents"`
	Tools   []tools.Status `json:"tools"`
}

func gatherStatus() (statusReport, error) {
	p, cfg, err := openProject()
	if err != nil {
		return statusReport{}, err
	}
	return gatherStatusFor(p, cfg), nil
}

// gatherStatusFor builds a report from an already-resolved project (used by the
// hook and supervisor, which carry an explicit root rather than CLI flags).
func gatherStatusFor(p paths.Project, cfg *config.Project) statusReport {
	rep := statusReport{Project: cfg.Project}
	rep.Agents = append(rep.Agents, agent.Observe(p, cfg, config.SupervisorName))
	for _, a := range cfg.Agents {
		rep.Agents = append(rep.Agents, agent.Observe(p, cfg, a.Name))
	}
	for _, t := range cfg.ServiceTools() {
		rep.Tools = append(rep.Tools, tools.Observe(p, t))
	}
	return rep
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show agents (state, activity, task, cost) and tools (status, port)",
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := gatherStatus()
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			printStatus(rep)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

func printStatus(rep statusReport) {
	fmt.Printf("project: %s\n\n", rep.Project)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATE\tLAST\tCURRENT/LAST TASK\tMODEL\tIN\tOUT")
	for _, v := range rep.Agents {
		last := "—"
		if !v.LastActivity.IsZero() {
			last = ago(time.Since(v.LastActivity))
		}
		in, out := "—", "—"
		if v.InTokens > 0 {
			in = agent.HumanInt(v.InTokens)
		}
		if v.OutTokens > 0 {
			out = agent.HumanInt(v.OutTokens)
		}
		task := v.CurrentTask
		if v.Summary != "" { // prefer the human-readable push summary when present
			task = v.Summary
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.State, last, trunc(task, 40), v.Model, in, out)
	}
	tw.Flush()

	fmt.Println()
	if len(rep.Tools) == 0 {
		fmt.Println("tools: (no service tools)")
		return
	}
	tw = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tOWNER\tSTATUS\tUPTIME\tPORT")
	for _, t := range rep.Tools {
		up := "—"
		if t.State != tools.StateStopped {
			up = ago(t.Uptime)
		}
		port := "—"
		if len(t.Ports) > 0 {
			port = fmt.Sprint(t.Ports[0])
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Owner, t.State, up, port)
	}
	tw.Flush()
}

func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n { // rune boundary; never split a UTF-8 sequence
		return string(r[:n-1]) + "…"
	}
	if s == "" {
		return "—"
	}
	return s
}
