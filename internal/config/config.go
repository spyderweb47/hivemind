// Package config defines the hivemind configuration schema (.hivemind/config.yaml)
// — the single source of truth produced by `hivemind setup` — plus load/save and
// small lookup helpers. The YAML shape matches the build spec exactly.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Reserved agent name. The supervisor is fixed and not user-configurable.
const SupervisorName = "supervisor"

// Tool types.
const (
	ToolService = "service"
	ToolCommand = "command"
	ToolLibrary = "library"
)

// Project is the root config document.
type Project struct {
	Project    string     `yaml:"project"`
	Supervisor Supervisor `yaml:"supervisor"`
	Defaults   Defaults   `yaml:"defaults"`
	Tools      []Tool     `yaml:"tools"`
	Agents     []Agent    `yaml:"agents"`
}

type Supervisor struct {
	Model  string `yaml:"model"`
	Report Report `yaml:"report"`
}

type Report struct {
	OnEvent          bool `yaml:"on_event"`
	HeartbeatMinutes int  `yaml:"heartbeat_minutes"`
}

type Defaults struct {
	Model          string `yaml:"model"`
	PermissionMode string `yaml:"permission_mode"`
}

// Tool is a first-class object: a dropped script/file + instructions, attached to
// one or more agents. `service` tools are health-supervised; `command` tools are
// ad-hoc CLIs; `library` tools are foundation files an agent reads.
type Tool struct {
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`
	Entrypoint string `yaml:"entrypoint,omitempty"`
	Path       string `yaml:"path,omitempty"`
	Health     string `yaml:"health,omitempty"`
	Ports      []int  `yaml:"ports,omitempty"`
	Restart    string `yaml:"restart,omitempty"`
	// Owner is the first agent that attached the tool; used for the dashboard's
	// "owner" column. Purely informational.
	Owner string `yaml:"owner,omitempty"`
}

// Agent is a named, persistent Claude Code session bound to one workspace + role.
type Agent struct {
	Name      string   `yaml:"name"`
	Workspace string   `yaml:"workspace"`
	Model     string   `yaml:"model"`
	Tools     []string `yaml:"tools,omitempty"`
	Reads     []string `yaml:"reads,omitempty"`
	Role      string   `yaml:"role"`
	SessionID string   `yaml:"session_id"`
	// Allow holds extra raw Claude `permissions.allow` rules granted to this agent
	// from the dashboard (e.g. "WebFetch", "WebSearch", "Bash(curl:*)"), appended
	// verbatim to the generated settings.json. This is how a blocked agent gets a
	// capability that isn't backed by a hivemind tool entrypoint.
	Allow []string `yaml:"allow,omitempty"`
}

// Load reads and parses a config file.
func Load(path string) (*Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Project
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &p, nil
}

// Save writes the config atomically (write temp, rename).
func Save(path string, p *Project) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// FindAgent returns a pointer into p.Agents, or nil. The supervisor is handled
// specially elsewhere and is not stored in Agents.
func (p *Project) FindAgent(name string) *Agent {
	for i := range p.Agents {
		if p.Agents[i].Name == name {
			return &p.Agents[i]
		}
	}
	return nil
}

// FindTool returns a pointer into p.Tools, or nil.
func (p *Project) FindTool(name string) *Tool {
	for i := range p.Tools {
		if p.Tools[i].Name == name {
			return &p.Tools[i]
		}
	}
	return nil
}

// ServiceTools returns just the service-type tools (the health-supervised ones).
func (p *Project) ServiceTools() []Tool {
	var out []Tool
	for _, t := range p.Tools {
		if t.Type == ToolService {
			out = append(out, t)
		}
	}
	return out
}

// EffectiveModel returns the agent's model or the project default.
func (p *Project) EffectiveModel(a *Agent) string {
	if a.Model != "" {
		return a.Model
	}
	if p.Defaults.Model != "" {
		return p.Defaults.Model
	}
	return "sonnet"
}

// EffectivePermissionMode returns the agent permission mode (defaults only for now).
func (p *Project) EffectivePermissionMode() string {
	if p.Defaults.PermissionMode != "" {
		return p.Defaults.PermissionMode
	}
	return "acceptEdits"
}

// AgentsUsingTool lists agent names that attach the given tool.
func (p *Project) AgentsUsingTool(tool string) []string {
	var out []string
	for _, a := range p.Agents {
		for _, t := range a.Tools {
			if t == tool {
				out = append(out, a.Name)
			}
		}
	}
	return out
}
