package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/scaffold"
)

// newEditCmd implements the spec's `hivemind edit <agent>` — modify an agent after
// setup and regenerate its CLAUDE.md / settings.json.
func newEditCmd() *cobra.Command {
	var model, role, addTool, rmTool, readsCSV string
	c := &cobra.Command{
		Use:   "edit <agent>",
		Short: "Modify an agent after setup (model/role/tools/reads) and re-scaffold it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			name := args[0]
			if name == config.SupervisorName {
				return fmt.Errorf("'supervisor' is fixed and cannot be edited")
			}
			a := cfg.FindAgent(name)
			if a == nil {
				return fmt.Errorf("unknown agent %q", name)
			}
			changed := false
			if model != "" {
				a.Model = model
				changed = true
			}
			if role != "" {
				a.Role = role
				changed = true
			}
			if cmd.Flags().Changed("reads") {
				reads := csv(readsCSV)
				for _, r := range reads {
					if filepath.IsAbs(r) || strings.Contains(r, "..") || strings.ContainsAny(r, "()* \t") {
						return fmt.Errorf("unsafe read path %q", r)
					}
				}
				a.Reads = reads
				changed = true
			}
			if addTool != "" {
				if cfg.FindTool(addTool) == nil {
					return fmt.Errorf("unknown tool %q", addTool)
				}
				if !contains(a.Tools, addTool) {
					a.Tools = append(a.Tools, addTool)
					changed = true
				}
			}
			if rmTool != "" {
				a.Tools = remove(a.Tools, rmTool)
				changed = true
			}
			if !changed {
				return fmt.Errorf("nothing to change — pass --model/--role/--reads/--add-tool/--remove-tool")
			}
			if err := config.Save(p.ConfigPath(), cfg); err != nil {
				return err
			}
			if err := scaffold.Agent(p, cfg, name); err != nil {
				return err
			}
			fmt.Printf("updated agent %q (model=%s tools=%v reads=%v) — CLAUDE.md regenerated\n",
				name, cfg.EffectiveModel(a), a.Tools, a.Reads)
			return nil
		},
	}
	c.Flags().StringVar(&model, "model", "", "set model tier")
	c.Flags().StringVar(&role, "role", "", "set role text")
	c.Flags().StringVar(&addTool, "add-tool", "", "attach a registered tool")
	c.Flags().StringVar(&rmTool, "remove-tool", "", "detach a tool")
	c.Flags().StringVar(&readsCSV, "reads", "", "replace read-only paths (comma-separated)")
	return c
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func remove(xs []string, v string) []string {
	out := xs[:0]
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
