package runner

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// ClaudeRunner drives the real `claude` CLI.
type ClaudeRunner struct {
	// Bin overrides the resolved binary path (from config or HIVEMIND_CLAUDE_BIN).
	Bin string
}

func (c *ClaudeRunner) Name() string { return "claude" }

// resolveBin finds the claude executable. Resolution order:
//  1. explicit c.Bin
//  2. HIVEMIND_CLAUDE_BIN
//  3. PATH lookup
//  4. a list of common install locations
func (c *ClaudeRunner) resolveBin() (string, error) {
	if c.Bin != "" {
		return c.Bin, nil
	}
	if v := os.Getenv("HIVEMIND_CLAUDE_BIN"); v != "" {
		return v, nil
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".claude", "local", "claude"),
		filepath.Join(home, ".local", "bin", "claude"),
		"/usr/local/bin/claude",
		"/opt/homebrew/bin/claude",
		filepath.Join(home, ".npm-global", "bin", "claude"),
	}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("`claude` CLI not found on PATH or in common locations; install it (https://docs.claude.com/claude-code) or set HIVEMIND_CLAUDE_BIN, or use --fake for a dry run")
}

func (c *ClaudeRunner) Available() error {
	_, err := c.resolveBin()
	return err
}

// buildArgs assembles the headless argv for one turn.
func (c *ClaudeRunner) buildArgs(spec PromptSpec) []string {
	args := []string{"-p"} // -p/--print is a bare flag; the prompt is positional
	if spec.SessionStarted {
		args = append(args, "--resume", spec.SessionID)
	} else {
		// Pre-assign the id so the session is addressable on subsequent turns.
		args = append(args, "--session-id", spec.SessionID)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	// NOTE: we deliberately do NOT pass --permission-mode here. A CLI
	// --permission-mode acceptEdits OVERRIDES (bypasses) the project's
	// permissions.deny rules, defeating read-only grant enforcement. Instead the
	// mode is carried by the generated .claude/settings.json `defaultMode`, which
	// honors deny rules. (Verified empirically against claude 2.1.x.)
	if spec.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", spec.AppendSystemPrompt)
	}
	for _, d := range spec.AddDirs {
		args = append(args, "--add-dir", d)
	}
	// JSON output gives a machine-readable final record (session id, cost, usage)
	// in the runner log; the transcript JSONL remains the source of truth.
	args = append(args, "--output-format", "json")
	// Terminate options with "--" so a prompt that begins with '-' or '--' is
	// taken as the positional prompt rather than parsed as a flag. This is also
	// what makes the preceding variadic --add-dir unambiguous.
	args = append(args, "--", spec.Prompt)
	return args
}

func (c *ClaudeRunner) Send(spec PromptSpec, logw io.Writer) error {
	bin, err := c.resolveBin()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, c.buildArgs(spec)...)
	cmd.Dir = spec.Workspace
	cmd.Stdout = logw
	cmd.Stderr = logw
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude turn failed: %w", err)
	}
	return nil
}

func (c *ClaudeRunner) AttachArgv(spec AttachSpec) (string, []string, error) {
	bin, err := c.resolveBin()
	if err != nil {
		return "", nil, err
	}
	args := []string{"--resume", spec.SessionID}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	return bin, args, nil
}
