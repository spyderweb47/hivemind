//go:build linux

package procscan

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// userHZ is the kernel's USER_HZ for the /proc interface. It is fixed at 100 for
// the procfs ABI on Linux regardless of the compiled CONFIG_HZ, so /proc/<pid>/stat
// starttime is always expressed in 1/100s ticks.
const userHZ = 100.0

// available reports whether /proc is mounted (the only requirement on Linux — no
// external tools needed, so this works on a minimal container).
func available() bool {
	fi, err := os.Stat("/proc/self")
	return err == nil && fi.IsDir()
}

// collect walks /proc to build the process table (ppid, comm, args, cwd, uptime)
// and maps listening-TCP ports to their owning PID via socket inodes. Pure /proc,
// no lsof/ss dependency.
func collect() (map[int]rawProc, map[int][]int) {
	raws := map[int]rawProc{}
	ports := map[int][]int{}

	bootUptime := readUptimeSecs() // seconds since boot, to convert starttime ticks
	now := time.Now()

	// inode→port for every listening TCP socket (v4 + v6).
	listen := map[uint64]int{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if b, err := os.ReadFile(f); err == nil {
			for ino, port := range parseProcNetTCP(string(b)) {
				listen[ino] = port
			}
		}
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return raws, ports
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid dir
		}
		statB, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		stat := string(statB)
		if procStateZombie(stat) {
			continue // a reaped/zombie process is dead — not a background service
		}
		ppid, startTicks, ok := parseProcStat(stat)
		if !ok {
			continue
		}
		cmdB, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		args := parseCmdline(string(cmdB))
		// Prefer basename(argv[0]) for the display name — identical to the macOS
		// collector — so a long executable name isn't shown as the kernel's
		// 15-char-truncated /proc/<pid>/stat comm (e.g. "jupyter-lab-ser"). Fall back
		// to stat comm for kernel threads (empty cmdline).
		comm := ""
		if a := strings.Fields(args); len(a) > 0 {
			comm = filepath.Base(a[0])
		}
		if comm == "" {
			comm = commFromStat(stat)
		}
		if args == "" {
			args = comm // kernel threads have empty cmdline
		}
		cwd, _ := os.Readlink("/proc/" + e.Name() + "/cwd") // EACCES for other users → ""

		var started time.Time
		var up time.Duration
		if bootUptime > 0 {
			procUp := bootUptime - float64(startTicks)/userHZ
			if procUp < 0 {
				procUp = 0
			}
			up = time.Duration(procUp * float64(time.Second))
			started = now.Add(-up)
		}

		raws[pid] = rawProc{PID: pid, PPID: ppid, Comm: comm, Args: args, CWD: cwd, StartedAt: started, Uptime: up}

		// Attribute listening ports by scanning this pid's socket fds (only worth it
		// when something is actually listening). fd readlink is owner/root-only, so
		// other users' processes simply contribute no ports.
		if len(listen) > 0 {
			fdDir := "/proc/" + e.Name() + "/fd"
			if fds, err := os.ReadDir(fdDir); err == nil {
				seen := map[int]bool{}
				for _, fd := range fds {
					tgt, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
					if err != nil {
						continue
					}
					if ino, ok := socketInode(tgt); ok {
						if port, ok := listen[ino]; ok && !seen[port] {
							seen[port] = true
							ports[pid] = append(ports[pid], port)
						}
					}
				}
			}
		}
	}
	return raws, ports
}

// readUptimeSecs reads the first field of /proc/uptime (seconds since boot).
func readUptimeSecs() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return v
}
