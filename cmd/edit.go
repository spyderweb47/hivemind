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
	var model, role, addTool, rmTool, readsCSV, permMode string
	c := &cobra.Command{
		Use:   "edit <agent>",
		Short: "Modify an agent after setup (model/role/tools/reads) and re-scaffold it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			defer lockConfig(p)()
			if cfg, err = config.Load(p.ConfigPath()); err != nil { // re-read under the lock
				return err
			}
			name := args[0]
			if name == config.SupervisorName {
				// The supervisor has no workspace/tools/reads; only its model is tunable.
				if role != "" || addTool != "" || rmTool != "" || cmd.Flags().Changed("reads") {
					return fmt.Errorf("for the supervisor, only --model is editable")
				}
				if model == "" {
					return fmt.Errorf("for the supervisor, pass --model (e.g. --model sonnet)")
				}
				if !config.ValidModel(model) {
					return fmt.Errorf("invalid model %q (use %s, or a claude-… id)", model, strings.Join(config.KnownModels, "/"))
				}
				cfg.Supervisor.Model = model
				if err := config.Save(p.ConfigPath(), cfg); err != nil {
					return err
				}
				if err := scaffold.Agent(p, cfg, config.SupervisorName); err != nil {
					return err
				}
				fmt.Printf("supervisor model set to %s\n", model)
				return nil
			}
			a := cfg.FindAgent(name)
			if a == nil {
				return fmt.Errorf("unknown agent %q", name)
			}
			changed := false
			if model != "" {
				if !config.ValidModel(model) {
					return fmt.Errorf("invalid model %q (use %s, or a claude-… id)", model, strings.Join(config.KnownModels, "/"))
				}
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
			if permMode != "" {
				if !config.ValidPermissionMode(permMode) {
					return fmt.Errorf("invalid permission mode %q (use %s)", permMode, strings.Join(config.PermissionModes, "/"))
				}
				a.PermissionMode = permMode
				changed = true
			}
			if !changed {
				return fmt.Errorf("nothing to change — pass --model/--role/--reads/--add-tool/--remove-tool/--permission-mode")
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
	c.Flags().StringVar(&permMode, "permission-mode", "", "override this agent's permission mode (acceptEdits|plan|default|bypassPermissions)")
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
