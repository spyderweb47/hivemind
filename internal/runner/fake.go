package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FakeRunner simulates a Claude Code session for local testing. It writes a
// transcript in the same JSONL shape Claude Code uses, advancing it incrementally
// so liveness/state transitions and token-cost parsing can be observed end-to-end
// without the real `claude` binary. It also honors trivial "create <file>"
// instructions so attach/inspection shows real side effects in the workspace.
type FakeRunner struct {
	ProjectsDir string
}

func (f *FakeRunner) Name() string     { return "fake" }
func (f *FakeRunner) Available() error { return nil }

var createRe = regexp.MustCompile(`(?i)create\s+([A-Za-z0-9._/-]+\.[A-Za-z0-9]+)`)

func (f *FakeRunner) transcriptPath(spec PromptSpec) string {
	dir := filepath.Join(f.ProjectsDir, MangleCWD(spec.Workspace))
	return filepath.Join(dir, spec.SessionID+".jsonl")
}

func (f *FakeRunner) Send(spec PromptSpec, logw io.Writer) error {
	tp := f.transcriptPath(spec)
	if err := os.MkdirAll(filepath.Dir(tp), 0o755); err != nil {
		return err
	}
	tf, err := os.OpenFile(tp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer tf.Close()

	emit := func(v map[string]any) {
		v["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
		v["sessionId"] = spec.SessionID
		v["cwd"] = spec.Workspace
		b, _ := json.Marshal(v)
		tf.Write(append(b, '\n'))
		tf.Sync()
	}

	fmt.Fprintf(logw, "[fake] turn for %s (session %s)\n", spec.Agent, spec.SessionID)

	// Test/demo hook: HIVEMIND_FAKE_SLEEP=<seconds> stretches the turn so the
	// interrupt path (esc / `hivemind interrupt`) can be exercised without claude.
	if s := os.Getenv("HIVEMIND_FAKE_SLEEP"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			time.Sleep(time.Duration(secs) * time.Second)
		}
	}

	// 1) user message
	emit(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": spec.Prompt,
		},
	})
	time.Sleep(250 * time.Millisecond)

	// 2) honor a trivial "create <file>" instruction, recorded as a tool_use.
	if m := createRe.FindStringSubmatch(spec.Prompt); m != nil {
		target := filepath.Join(spec.Workspace, m[1])
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		_ = os.WriteFile(target, []byte("created by hivemind fake runner\n"), 0o644)
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":  "assistant",
				"model": spec.Model,
				"content": []map[string]any{
					{"type": "tool_use", "name": "Write", "input": map[string]any{"file_path": target}},
				},
				"usage": map[string]any{"input_tokens": 1800, "output_tokens": 90, "cache_read_input_tokens": 12000},
			},
		})
		fmt.Fprintf(logw, "[fake] wrote %s\n", target)
		time.Sleep(400 * time.Millisecond)
	}

	// 3) final assistant text + result. Prompt sentinels let the fake deterministically
	// drive the ERROR and BLOCKED states for the acceptance harness.
	lp := strings.ToLower(spec.Prompt)
	errored := strings.Contains(lp, "fail") || strings.Contains(lp, "simulate error")
	blocked := strings.Contains(lp, "blocked") || strings.Contains(lp, "need input")

	reply := f.compose(spec.Prompt)
	if blocked {
		reply = "BLOCKED: I need clarification before continuing — " + reply
	}
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"model":   spec.Model,
			"content": []map[string]any{{"type": "text", "text": reply}},
			"usage":   map[string]any{"input_tokens": 2200, "output_tokens": 320, "cache_read_input_tokens": 14500},
		},
	})
	if errored {
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true})
		fmt.Fprintf(logw, "[fake] turn ended with simulated error\n")
		return nil
	}
	emit(map[string]any{
		"type":     "result",
		"subtype":  "success",
		"is_error": false,
		"usage":    map[string]any{"input_tokens": 0, "output_tokens": 0},
	})
	fmt.Fprintf(logw, "[fake] turn complete\n")
	return nil
}

func (f *FakeRunner) compose(prompt string) string {
	p := strings.ToLower(prompt)
	switch {
	case strings.Contains(p, "describe") && strings.Contains(p, "tool"):
		return "Done. I created the requested file and reviewed my attached tool's TOOL.md: it is a service I invoke via its documented entrypoint and rely on for my role."
	case strings.Contains(p, "status"):
		return "Status: workspace is healthy, no blockers. Standing by for the next instruction."
	default:
		return "Acknowledged — completed the requested step within my workspace."
	}
}

func (f *FakeRunner) AttachArgv(spec AttachSpec) (string, []string, error) {
	// There is no interactive process to attach to in fake mode; surface a clear
	// message via a portable `sh -c` so `hivemind attach` degrades gracefully.
	msg := fmt.Sprintf("fake runner: no live session to attach for %s. Install `claude` to use real attach.", spec.SessionID)
	return "/bin/sh", []string{"-c", "echo " + shellQuote(msg)}, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
