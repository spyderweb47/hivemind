package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/tasks"
)

// task creates and dispatches one explicit task to a named agent (no LLM routing).
func newTaskCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "task <agent> <prompt...>",
		Short: "Create and dispatch one task to a specific agent (explicit routing)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			name := args[0]
			if name != config.SupervisorName && cfg.FindAgent(name) == nil {
				return fmt.Errorf("unknown agent %q", name)
			}
			prompt := strings.Join(args[1:], " ")
			if !flagFake {
				if err := pickRunner().Available(); err != nil {
					return err
				}
			}
			t, err := tasks.Add(p, tasks.Task{Agent: name, Prompt: prompt, Status: tasks.Dispatched, Source: tasks.SourceManual})
			if err != nil {
				return err
			}
			if err := agent.Dispatch(p, name, prompt, flagFake, t.ID); err != nil {
				_ = tasks.SetStatus(p, t.ID, tasks.Failed)
				return err
			}
			fmt.Printf("→ [%s] dispatched to %s\n", t.ID, name)
			return nil
		},
	}
}

// tasks lists the task board.
func newTasksCmd() *cobra.Command {
	var asJSON, openOnly bool
	c := &cobra.Command{
		Use:   "tasks",
		Short: "Show the task board (delegated + manual tasks and their status)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, err := openProject()
			if err != nil {
				return err
			}
			list := tasks.List(p)
			if openOnly {
				list = tasks.Open(p)
			}
			if asJSON {
				b, _ := json.MarshalIndent(list, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(list) == 0 {
				fmt.Println("(no tasks — create with `hivemind delegate` or `hivemind task`)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tAGENT\tSTATUS\tAGE\tSRC\tPROMPT")
			for _, t := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					t.ID, t.Agent, t.Status, ago(time.Since(t.Created)), t.Source, trunc(t.Prompt, 50))
			}
			tw.Flush()
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	c.Flags().BoolVar(&openOnly, "open", false, "only pending/dispatched tasks")
	return c
}
