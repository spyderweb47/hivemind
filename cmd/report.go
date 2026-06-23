package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/runner"
	"hivemind/internal/tools"
)

// report wakes the haiku supervisor to summarize the fleet (its final message is
// recorded to the ledger by its Stop hook). It also prints an immediate
// deterministic digest so the human gets instant value, and falls back to writing
// that digest to the ledger when no real claude is available.
func newReportCmd() *cobra.Command {
	var noSupervisor bool
	c := &cobra.Command{
		Use:   "report",
		Short: "Summarize the fleet now (wakes the haiku supervisor; prints a digest)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			rep := gatherStatusFor(p, cfg)
			fmt.Print(buildDigest(rep))

			if noSupervisor {
				return nil
			}
			// Under --fake (or when no real claude exists) a supervisor LLM turn
			// can't run/record, so write the deterministic digest to the ledger.
			if flagFake {
				fmt.Println("\n(--fake: wrote deterministic digest to ledger; supervisor LLM not invoked)")
				return appendLedger(p, buildDigest(rep))
			}
			if runner.Select(false).Available() != nil {
				fmt.Println("\n(supervisor LLM unavailable; wrote deterministic digest to ledger)")
				return appendLedger(p, buildDigest(rep))
			}
			if err := supervisorReport(p, cfg, "manual"); err != nil {
				return err
			}
			fmt.Printf("\n→ supervisor (haiku) summarizing; its digest will land in %s\n", p.Ledger())
			return nil
		},
	}
	c.Flags().BoolVar(&noSupervisor, "no-supervisor", false, "skip waking the supervisor; print the deterministic digest only (does not write the ledger)")
	return c
}

// buildDigest renders a deterministic, control-plane-derived fleet digest.
func buildDigest(rep statusReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Fleet digest — %s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Project: %s\n\n", rep.Project)
	b.WriteString("Agents:\n")
	for _, v := range rep.Agents {
		task := v.Summary
		if task == "" {
			task = v.CurrentTask
		}
		if task == "" {
			task = "—"
		}
		fmt.Fprintf(&b, "- %s: %s | %s | tokens in %s / out %s\n", v.Name, v.State, task, agent.HumanInt(v.InTokens), agent.HumanInt(v.OutTokens))
	}
	if len(rep.Tools) > 0 {
		b.WriteString("\nTools:\n")
		for _, t := range rep.Tools {
			up := "—"
			if t.State != tools.StateStopped {
				up = ago(t.Uptime)
			}
			fmt.Fprintf(&b, "- %s: %s (uptime %s)\n", t.Name, t.State, up)
		}
	}
	return b.String()
}
