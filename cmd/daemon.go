package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"hivemind/internal/paths"
)

// Generic detached-daemon plumbing shared by the heartbeat and health daemons.
// A daemon "<name>" runs `hivemind __<name> --root <root>` setsid'd, tracked by a
// pidfile, with a process-identity check so a reused PID is never mistaken for ours.

func daemonPidfile(p paths.Project, name string) string {
	return filepath.Join(p.HivemindDir(), name+".pid")
}

func daemonPid(p paths.Project, name string) int {
	b, err := os.ReadFile(daemonPidfile(p, name))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// daemonProcMatches verifies the pid's command line contains the daemon's hidden
// verb, guarding against PID reuse after a stale pidfile.
func daemonProcMatches(pid int, verb string) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), verb)
}

func daemonRunning(p paths.Project, name string) bool {
	pid := daemonPid(p, name)
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	return daemonProcMatches(pid, "__"+name)
}

// startDaemon launches `hivemind __<name> --root <root>` detached, if not already
// running. Returns true if a daemon is (now) running.
func startDaemon(p paths.Project, name string) bool {
	if daemonRunning(p, name) {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	cmd := exec.Command(exe, "__"+name, "--root", p.Root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	if logf, e := os.OpenFile(filepath.Join(p.HivemindDir(), name+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); e == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
		defer logf.Close()
	}
	if cmd.Start() != nil {
		return false
	}
	_ = os.WriteFile(daemonPidfile(p, name), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	_ = cmd.Process.Release()
	return true
}

// stopDaemon SIGTERMs the daemon (identity-checked) and removes its pidfile.
func stopDaemon(p paths.Project, name string) {
	if pid := daemonPid(p, name); pid > 0 && daemonProcMatches(pid, "__"+name) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	_ = os.Remove(daemonPidfile(p, name))
}
