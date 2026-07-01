// Package wizard implements `hivemind setup`. The interactive path is a Bubble Tea
// TUI (see tui.go); a non-interactive --preset path (here) loads the same answers
// from a YAML file for repeatable setups and automated tests. Both converge on
// validate() + materialize().
package wizard

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/scaffold"

	"gopkg.in/yaml.v3"
)

// Preset is the non-interactive form: a full config plus the source files/docs
// needed to materialize each tool.
type Preset struct {
	Project     config.Project    `yaml:",inline"`
	ToolSources map[string]string `yaml:"tool_sources,omitempty"` // tool -> file to drop
	ToolDocs    map[string]string `yaml:"tool_docs,omitempty"`    // tool -> TOOL.md content
}

// RunPreset scaffolds a project non-interactively from a preset file.
func RunPreset(root, presetPath string, out io.Writer) (*config.Project, error) {
	b, err := os.ReadFile(presetPath)
	if err != nil {
		return nil, err
	}
	var pre Preset
	if err := yaml.Unmarshal(b, &pre); err != nil {
		return nil, fmt.Errorf("parse preset: %w", err)
	}
	cfg := &pre.Project
	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	p := paths.NewProject(root)
	if err := materialize(p, cfg, pre.ToolSources, pre.ToolDocs, out); err != nil {
		return nil, err
	}
	return cfg, nil
}

// materialize writes config, drops tool files, and scaffolds the fleet.
func materialize(p paths.Project, cfg *config.Project, toolSrc, toolDoc map[string]string, out io.Writer) error {
	if err := os.MkdirAll(p.HivemindDir(), 0o755); err != nil {
		return err
	}
	if err := config.Save(p.ConfigPath(), cfg); err != nil {
		return err
	}
	for _, t := range cfg.Tools {
		if err := scaffold.WriteToolFiles(p, t, toolDoc[t.Name], toolSrc[t.Name]); err != nil {
			return fmt.Errorf("tool %s: %w", t.Name, err)
		}
	}
	if err := scaffold.Project(p, cfg); err != nil {
		return err
	}
	paths.RegisterProject(p.Root) // add to the global ~/.hivemind/ registry
	fmt.Fprintf(out, "\n  Registered fleet for %q:\n", cfg.Project)
	fmt.Fprintf(out, "    supervisor (haiku) — fixed orchestrator\n")
	for _, a := range cfg.Agents {
		fmt.Fprintf(out, "    %-12s (%s) ws=%s tools=%v\n", a.Name, cfg.EffectiveModel(&a), a.Workspace, a.Tools)
	}
	for _, t := range cfg.Tools {
		fmt.Fprintf(out, "    tool %-14s type=%s owner=%s\n", t.Name, t.Type, t.Owner)
	}
	return nil
}

func applyDefaults(cfg *config.Project) {
	if cfg.Supervisor.Model == "" {
		cfg.Supervisor.Model = "haiku"
	}
	if cfg.Defaults.Model == "" {
		cfg.Defaults.Model = "sonnet"
	}
	if cfg.Defaults.PermissionMode == "" {
		cfg.Defaults.PermissionMode = "acceptEdits"
	}
	if cfg.Project == "" {
		cfg.Project = "hivemind"
	}
	// Derive each tool's owner (first agent that attaches it) if unset.
	for i := range cfg.Tools {
		if cfg.Tools[i].Owner == "" {
			if owners := cfg.AgentsUsingTool(cfg.Tools[i].Name); len(owners) > 0 {
				cfg.Tools[i].Owner = owners[0]
			}
		}
	}
}

func validate(cfg *config.Project) error {
	if !config.ValidModel(cfg.Defaults.Model) {
		return fmt.Errorf("invalid default model %q (use %s, or a claude-… id)", cfg.Defaults.Model, strings.Join(config.KnownModels, "/"))
	}
	if !config.ValidModel(cfg.Supervisor.Model) {
		return fmt.Errorf("invalid supervisor model %q (use %s, or a claude-… id)", cfg.Supervisor.Model, strings.Join(config.KnownModels, "/"))
	}
	if cfg.Defaults.PermissionMode != "" && !config.ValidPermissionMode(cfg.Defaults.PermissionMode) {
		return fmt.Errorf("invalid permission mode %q (use %s)", cfg.Defaults.PermissionMode, strings.Join(config.PermissionModes, "/"))
	}
	seen := map[string]bool{}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Name == "" {
			return fmt.Errorf("agent #%d has no name", i+1)
		}
		if a.Name == config.SupervisorName {
			return fmt.Errorf("'supervisor' is reserved")
		}
		if seen[a.Name] {
			return fmt.Errorf("duplicate agent %q", a.Name)
		}
		seen[a.Name] = true
		if a.Model != "" && !config.ValidModel(a.Model) {
			return fmt.Errorf("agent %q: invalid model %q (use %s, or a claude-… id)", a.Name, a.Model, strings.Join(config.KnownModels, "/"))
		}
		if a.Workspace == "" {
			a.Workspace = a.Name
		}
		if filepath.IsAbs(a.Workspace) || strings.Contains(a.Workspace, "..") {
			return fmt.Errorf("agent %q: unsafe workspace %q", a.Name, a.Workspace)
		}
		// Read grants must stay inside the project and be free of characters that
		// would escape the project root or corrupt a generated deny-rule pattern.
		for _, r := range a.Reads {
			if filepath.IsAbs(r) || strings.Contains(r, "..") || strings.ContainsAny(r, "()* \t") {
				return fmt.Errorf("agent %q: unsafe read path %q (no absolute, .., or ()* whitespace)", a.Name, r)
			}
		}
		for _, tn := range a.Tools {
			if cfg.FindTool(tn) == nil {
				return fmt.Errorf("agent %q references unknown tool %q", a.Name, tn)
			}
		}
	}
	for _, t := range cfg.Tools {
		switch t.Type {
		case config.ToolService:
			if t.Entrypoint == "" {
				return fmt.Errorf("service tool %q needs an entrypoint", t.Name)
			}
		case config.ToolLibrary:
			if t.Path == "" {
				return fmt.Errorf("library tool %q needs a path", t.Name)
			}
		case config.ToolCommand:
			if t.Entrypoint == "" {
				return fmt.Errorf("command tool %q needs an entrypoint", t.Name)
			}
		default:
			return fmt.Errorf("tool %q has invalid type %q", t.Name, t.Type)
		}
	}
	return nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
