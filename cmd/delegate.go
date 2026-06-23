package cmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/runner"
	"hivemind/internal/session"
	"hivemind/internal/tasks"
)

// planItem is one routed assignment in a delegation plan.
type planItem struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

// delegate routes a high-level instruction to the fleet: the supervisor decomposes
// it into per-agent prompts, and each is dispatched to the named agent as a tracked
// task. Falls back to a deterministic "ask <agent> to <task>" parser when no real
// claude is available.
func newDelegateCmd() *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "delegate <instruction...>",
		Short: "Route a high-level instruction to the right agents (supervisor decomposes it)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			instruction := strings.Join(args, " ")
			items, source, err := planTasks(p, cfg, instruction, flagFake)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				return fmt.Errorf("nothing to route — name agents explicitly (e.g. \"ask %s to …\")", firstAgentName(cfg))
			}
			fmt.Printf("plan (%s):\n", source)
			for _, it := range items {
				a := cfg.FindAgent(it.Agent)
				if a == nil {
					fmt.Printf("  ! skipping unknown agent %q for: %s\n", it.Agent, trunc(it.Task, 50))
					continue
				}
				if dryRun {
					fmt.Printf("  · %-12s %s\n", it.Agent, trunc(it.Task, 60))
					continue
				}
				t, err := tasks.Add(p, tasks.Task{Agent: a.Name, Prompt: it.Task, Status: tasks.Dispatched, Source: tasks.SourceDelegate, Origin: instruction})
				if err != nil {
					return err
				}
				if err := agent.Dispatch(p, a.Name, it.Task, flagFake, t.ID); err != nil {
					_ = tasks.SetStatus(p, t.ID, tasks.Failed)
					fmt.Printf("  ✗ [%s] %-12s dispatch failed: %v\n", t.ID, a.Name, err)
					continue
				}
				fmt.Printf("  → [%s] %-12s %s\n", t.ID, a.Name, trunc(it.Task, 60))
			}
			if !dryRun {
				fmt.Println("\ntrack with: hivemind tasks")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show the routing plan without dispatching")
	return c
}

// planTasks produces a routing plan: the LLM supervisor when real claude is
// available, else a deterministic parser of explicit "ask <agent> to <task>" form.
func planTasks(p paths.Project, cfg *config.Project, instruction string, fake bool) ([]planItem, string, error) {
	if !fake && runner.Select(false).Available() == nil {
		if items, err := llmPlan(p, cfg, instruction); err == nil && len(items) > 0 {
			return items, "supervisor", nil
		}
		// fall through to the deterministic parser on any planning failure
	}
	return regexPlan(cfg, instruction), "rule-based", nil
}

func llmPlan(p paths.Project, cfg *config.Project, instruction string) ([]planItem, error) {
	tgt, err := agent.ResolveTarget(p, cfg, config.SupervisorName)
	if err != nil {
		return nil, err
	}
	spec := runner.PromptSpec{
		Agent:     config.SupervisorName,
		SessionID: session.NewID(), // ephemeral planning session
		Workspace: tgt.Workspace,
		Model:     tgt.Model,
		Prompt:    planningPrompt(cfg, instruction),
	}
	out, err := runner.CaptureResult(spec)
	if err != nil {
		return nil, err
	}
	return parsePlan(out)
}

func planningPrompt(cfg *config.Project, instruction string) string {
	var b strings.Builder
	b.WriteString("You are the hivemind supervisor routing work to your fleet.\n\nAgents you can assign work to:\n")
	for _, a := range cfg.Agents {
		fmt.Fprintf(&b, "- %s: %s\n", a.Name, firstLine(a.Role))
	}
	fmt.Fprintf(&b, "\nOperator instruction:\n\"\"\"\n%s\n\"\"\"\n\n", instruction)
	b.WriteString("Decompose this into concrete per-agent tasks. Rules:\n")
	b.WriteString("- If the instruction names an agent for a piece of work, assign it to THAT agent (match by name).\n")
	b.WriteString("- Otherwise choose the best-fit agent by role.\n")
	b.WriteString("- Each \"task\" is the exact, self-contained prompt to hand that agent.\n")
	b.WriteString("Respond with ONLY a JSON array, no prose and no code fences:\n")
	b.WriteString(`[{"agent":"<name>","task":"<prompt>"}]`)
	return b.String()
}

// parsePlan extracts the JSON array from the supervisor's reply.
func parsePlan(text string) ([]planItem, error) {
	s := strings.TrimSpace(text)
	if i := strings.IndexByte(s, '['); i >= 0 {
		if j := strings.LastIndexByte(s, ']'); j > i {
			s = s[i : j+1]
		}
	}
	var items []planItem
	if err := json.Unmarshal([]byte(s), &items); err != nil {
		return nil, fmt.Errorf("could not parse routing plan from supervisor: %w", err)
	}
	return items, nil
}

var segSplit = regexp.MustCompile(`(?i)\s+and\s+|,\s*|;\s*`)
var segMatch = regexp.MustCompile(`(?i)^\s*(?:please\s+|also\s+)?(?:ask|tell|have|get)?\s*([A-Za-z0-9_-]+)\s+to\s+(.+?)\s*$`)

// regexPlan parses explicit "ask <agent> to <task>" style instructions without an
// LLM. Used under --fake and as the fallback when claude is unavailable.
func regexPlan(cfg *config.Project, instruction string) []planItem {
	canon := map[string]string{}
	for _, a := range cfg.Agents {
		canon[strings.ToLower(a.Name)] = a.Name
	}
	var items []planItem
	for _, seg := range segSplit.Split(instruction, -1) {
		m := segMatch.FindStringSubmatch(seg)
		if m == nil {
			continue
		}
		if name, ok := canon[strings.ToLower(m[1])]; ok {
			items = append(items, planItem{Agent: name, Task: strings.TrimSpace(m[2])})
		}
	}
	return items
}

func firstAgentName(cfg *config.Project) string {
	if len(cfg.Agents) > 0 {
		return cfg.Agents[0].Name
	}
	return "<agent>"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
