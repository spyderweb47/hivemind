package cmd

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/config"
	"hivemind/internal/scaffold"
	"hivemind/internal/tools"
)

func newToolCmd() *cobra.Command {
	c := &cobra.Command{Use: "tool", Short: "Manage service tools (start|stop|restart|status)"}
	c.AddCommand(
		toolActionCmd("start", "Start a service tool"),
		toolActionCmd("stop", "Stop a service tool"),
		toolActionCmd("restart", "Restart a service tool"),
		newToolStatusCmd(),
		toolAddCommand("add"),
		newToolAttachCmd(),
	)
	return c
}

// toolAddCommand registers a new tool after setup (optionally attaching it). The
// `use` parameter lets it serve as both `tool add` and the spec's `add tool`.
func toolAddCommand(use string) *cobra.Command {
	var typ, entrypoint, health, ports, file, path, doc, attachTo string
	c := &cobra.Command{
		Use:   use + " <name>",
		Short: "Register a new tool after setup (drop a script + optionally attach it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			t := config.Tool{Name: args[0], Type: typ, Entrypoint: entrypoint, Health: health, Path: path}
			for _, ps := range strings.Split(ports, ",") {
				if ps = strings.TrimSpace(ps); ps != "" {
					if n, err := strconv.Atoi(ps); err == nil {
						t.Ports = append(t.Ports, n)
					}
				}
			}
			if t.Type == config.ToolLibrary && t.Path == "" && file != "" {
				t.Path = filepath.Base(file)
			}
			docContent := doc
			if docContent != "" {
				docContent = fmt.Sprintf("# %s\n\n%s\n", t.Name, doc)
			}
			if err := scaffold.RegisterTool(p, cfg, t, file, docContent); err != nil {
				return err
			}
			fmt.Printf("registered tool %q (%s)\n", t.Name, t.Type)
			if attachTo != "" {
				if err := scaffold.AttachTool(p, cfg, t.Name, attachTo); err != nil {
					return err
				}
				fmt.Printf("attached to %s (CLAUDE.md regenerated)\n", attachTo)
			}
			return nil
		},
	}
	c.Flags().StringVar(&typ, "type", "command", "service|command|library")
	c.Flags().StringVar(&entrypoint, "entrypoint", "", "how to run it (service/command)")
	c.Flags().StringVar(&health, "health", "", "health probe (service)")
	c.Flags().StringVar(&ports, "ports", "", "comma-separated ports (service)")
	c.Flags().StringVar(&file, "file", "", "path to a script/file to drop into the tool dir")
	c.Flags().StringVar(&path, "path", "", "foundation file name (library)")
	c.Flags().StringVar(&doc, "doc", "", "one-line usage description (TOOL.md)")
	c.Flags().StringVar(&attachTo, "agent", "", "attach the tool to this agent")
	return c
}

// newToolAttachCmd attaches an already-registered tool to an agent.
func newToolAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <tool> <agent>",
		Short: "Attach an existing tool to an agent (regenerates its CLAUDE.md)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			if err := scaffold.AttachTool(p, cfg, args[0], args[1]); err != nil {
				return err
			}
			fmt.Printf("attached %q to %s (CLAUDE.md regenerated)\n", args[0], args[1])
			return nil
		},
	}
}

func toolActionCmd(verb, short string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <tool>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			t := cfg.FindTool(args[0])
			if t == nil {
				return fmt.Errorf("unknown tool %q", args[0])
			}
			if t.Type != config.ToolService {
				return fmt.Errorf("tool %q is type %q, not a managed service", t.Name, t.Type)
			}
			if !tools.HaveTmux() {
				return fmt.Errorf("tmux not found on PATH; service tools require tmux")
			}
			var actErr error
			switch verb {
			case "start":
				actErr = tools.Start(p, *t)
			case "stop":
				actErr = tools.Stop(p, *t)
			case "restart":
				actErr = tools.Restart(p, *t)
			}
			if actErr != nil {
				return actErr
			}
			st := tools.Observe(p, *t)
			fmt.Printf("%s: %s\n", t.Name, st.State)
			return nil
		},
	}
}

func newToolStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [tool]",
		Short: "Show service-tool status (all, or one)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			list := cfg.ServiceTools()
			if len(args) == 1 {
				t := cfg.FindTool(args[0])
				if t == nil || t.Type != config.ToolService {
					return fmt.Errorf("unknown service tool %q", args[0])
				}
				list = []config.Tool{*t}
			}
			for _, t := range list {
				st := tools.Observe(p, t)
				port := "—"
				if len(st.Ports) > 0 {
					port = fmt.Sprint(st.Ports[0])
				}
				fmt.Printf("%-16s %-10s uptime=%-8s port=%s pid=%d\n",
					st.Name, st.State, ago(st.Uptime), port, st.PID)
			}
			return nil
		},
	}
}
