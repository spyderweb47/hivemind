package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/scaffold"
)

// newAddCmd implements the spec's `hivemind add agent|tool` — add an agent or a
// tool to a project after setup.
func newAddCmd() *cobra.Command {
	c := &cobra.Command{Use: "add", Short: "Add an agent or tool after setup"}
	c.AddCommand(newAddAgentCmd(), toolAddCommand("tool"))
	return c
}

func newAddAgentCmd() *cobra.Command {
	var workspace, model, role, toolsCSV, readsCSV string
	c := &cobra.Command{
		Use:   "agent <name>",
		Short: "Add a new agent (scaffolds its workspace + CLAUDE.md)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			name := args[0]
			if name == config.SupervisorName {
				return fmt.Errorf("'supervisor' is reserved")
			}
			if cfg.FindAgent(name) != nil {
				return fmt.Errorf("agent %q already exists", name)
			}
			a := config.Agent{Name: name, Workspace: name, Model: model, Role: role}
			if workspace != "" {
				a.Workspace = workspace
			}
			if filepath.IsAbs(a.Workspace) || strings.Contains(a.Workspace, "..") {
				return fmt.Errorf("unsafe workspace %q", a.Workspace)
			}
			a.Tools = csv(toolsCSV)
			for _, tn := range a.Tools {
				if cfg.FindTool(tn) == nil {
					return fmt.Errorf("unknown tool %q (register it first with `hivemind add tool`)", tn)
				}
			}
			a.Reads = csv(readsCSV)
			for _, r := range a.Reads {
				if filepath.IsAbs(r) || strings.Contains(r, "..") || strings.ContainsAny(r, "()* \t") {
					return fmt.Errorf("unsafe read path %q", r)
				}
			}
			cfg.Agents = append(cfg.Agents, a)
			if err := config.Save(p.ConfigPath(), cfg); err != nil {
				return err
			}
			if err := scaffold.Agent(p, cfg, name); err != nil {
				return err
			}
			fmt.Printf("added agent %q (workspace=%s model=%s tools=%v)\n", name, a.Workspace, cfg.EffectiveModel(&a), a.Tools)
			fmt.Println("the console will show it next launch; prompt it with: hivemind send " + name + " \"…\"")
			return nil
		},
	}
	c.Flags().StringVar(&workspace, "workspace", "", "workspace dir (default: agent name)")
	c.Flags().StringVar(&model, "model", "", "model tier (default: project default)")
	c.Flags().StringVar(&role, "role", "", "role & responsibilities")
	c.Flags().StringVar(&toolsCSV, "tools", "", "comma-separated tools to attach")
	c.Flags().StringVar(&readsCSV, "reads", "", "comma-separated read-only paths")
	return c
}

// csv splits a comma list, trimming blanks.
func csv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
