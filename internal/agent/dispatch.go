package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"hivemind/internal/paths"
)

// Dispatch launches a prompt as a detached child process (`hivemind __turn …`) so
// the caller — a CLI invocation or the dashboard's prompt box — returns
// immediately while the agent works. The child runs the lock-serialized turn and
// outlives the parent (Setsid). The prompt is passed via a temp file to avoid
// argv quoting issues with multi-line prompts.
func Dispatch(p paths.Project, name, prompt string, fake bool, taskID string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.AgentDir(name), 0o755); err != nil {
		return err
	}
	// Touch the dispatch marker so Observe reports WORKING immediately, closing
	// the gap until the detached child actually acquires the lock.
	_ = os.WriteFile(filepath.Join(p.AgentDir(name), dispatchMarker), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o644)
	tmp := filepath.Join(p.AgentDir(name), fmt.Sprintf("prompt-%d.txt", time.Now().UnixNano()))
	if err := os.WriteFile(tmp, []byte(prompt), 0o644); err != nil {
		return err
	}
	args := []string{"__turn", "--agent", name, "--root", p.Root, "--prompt-file", tmp}
	if fake {
		args = append(args, "--fake")
	}
	if taskID != "" {
		args = append(args, "--task-id", taskID)
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logf, err := os.OpenFile(p.AgentLog(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	// Record the PID so the turn can be interrupted (its process group is killed).
	_ = os.WriteFile(p.AgentTurnPid(name), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	// The child holds its own copy of the fd; close ours. Do not Wait — detach.
	logf.Close()
	_ = cmd.Process.Release()
	return nil
}
