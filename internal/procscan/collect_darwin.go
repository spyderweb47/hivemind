//go:build darwin

package procscan

import (
	"context"
	"os/exec"
	"time"
)

// available reports whether the macOS process-inspection tools are present.
func available() bool {
	if _, err := exec.LookPath("lsof"); err != nil {
		return false
	}
	if _, err := exec.LookPath("ps"); err != nil {
		return false
	}
	return true
}

// runCmd runs an external tool with a hard timeout and returns its stdout. lsof
// exits non-zero on permission-denied sockets or empty results; we keep whatever
// stdout it produced and ignore the exit status (partial data is still useful).
// Every call uses an explicit arg slice — never a shell string — so there is no
// shell-injection surface.
func runCmd(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, name, args...).Output()
	return string(out)
}

// collect gathers all user processes (pid/ppid/start/args via ps, cwd via lsof)
// plus the listening-TCP port map (via lsof). macOS has no /proc, so this is the
// lsof+ps path; the heavy calls are throttled by the dashboard (every few seconds,
// off the UI thread).
func collect() (map[int]rawProc, map[int][]int) {
	raws := parseDarwinPS(runCmd("ps", "-axo", "pid=,ppid=,lstart=,args="), time.Now())
	for pid, cwd := range parseDarwinCwds(runCmd("lsof", "-nP", "-d", "cwd", "-Fpn")) {
		if r, ok := raws[pid]; ok {
			r.CWD = cwd
			raws[pid] = r
		}
	}
	ports := parseDarwinPorts(runCmd("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-FpPn"))
	return raws, ports
}
