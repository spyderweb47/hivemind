// Package runner is the one seam through which hivemind drives Claude Code. The
// rest of the program never shells out to `claude` directly — it goes through a
// Runner. Two implementations exist:
//
//   - ClaudeRunner: the real thing. Spawns `claude -p --resume <sid> …` to run a
//     single prompt to completion over a persistent on-disk session, then exits.
//   - FakeRunner: simulates a Claude session by writing a claude-format JSONL
//     transcript. It lets the entire control plane (liveness, cost, dashboard,
//     the M1 acceptance test) be exercised on a machine where `claude` is not
//     installed. Selected via HIVEMIND_FAKE_RUNNER=1 or `--fake`.
package runner

import (
	"io"
	"os"
	"strings"

	"hivemind/internal/paths"
)

// PromptSpec describes one headless turn.
type PromptSpec struct {
	Agent              string
	SessionID          string
	Workspace          string // absolute cwd for the agent
	Model              string
	PermissionMode     string
	AppendSystemPrompt string
	AddDirs            []string
	Prompt             string
	// SessionStarted controls whether we create the session (--session-id) or
	// resume it (--resume). Derived from transcript existence by the caller.
	SessionStarted bool
}

// AttachSpec describes an interactive resume.
type AttachSpec struct {
	SessionID string
	Workspace string
	Model     string
}

// Runner abstracts the Claude Code runtime.
type Runner interface {
	// Name identifies the runner ("claude" or "fake") for diagnostics.
	Name() string
	// Available reports whether the runtime can be used; nil means yes.
	Available() error
	// Send runs one prompt to completion, streaming process output to logw.
	Send(spec PromptSpec, logw io.Writer) error
	// AttachArgv returns the binary + args to exec for an interactive session.
	AttachArgv(spec AttachSpec) (bin string, args []string, err error)
}

// Select picks a runner. If HIVEMIND_FAKE_RUNNER=1 (or forceFake) it returns the
// fake; otherwise the real claude runner. The caller may still inspect
// Available() to present a helpful error.
func Select(forceFake bool) Runner {
	if forceFake || os.Getenv("HIVEMIND_FAKE_RUNNER") == "1" {
		return &FakeRunner{ProjectsDir: paths.ClaudeProjectsDir()}
	}
	return &ClaudeRunner{}
}

// MangleCWD reproduces Claude Code's project-directory naming: every byte that is
// not an ASCII letter or digit becomes '-'. e.g. /Users/me/My Proj -> -Users-me-My-Proj.
// Only used by the fake runner to place transcripts in a realistic location; the
// real locator finds transcripts by globbing for the session id regardless.
func MangleCWD(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
