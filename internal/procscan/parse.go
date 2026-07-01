package procscan

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// --- macOS: parse lsof / ps -F field output (consumed by collect_darwin.go) ---

// parseDarwinPS parses `ps -axo pid=,ppid=,lstart=,args=` (no header). Each line
// is: <pid> <ppid> <Www Mmm DD HH:MM:SS YYYY> <args…>. lstart is a fixed 5 tokens.
func parseDarwinPS(out string, now time.Time) map[int]rawProc {
	res := map[int]rawProc{}
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) < 8 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		lstart := strings.Join(f[2:7], " ") // "Wed Jun 24 14:30:22 2026"
		args := strings.Join(f[7:], " ")
		var started time.Time
		var up time.Duration
		if t, e := time.ParseInLocation("Mon Jan 2 15:04:05 2006", lstart, now.Location()); e == nil {
			started = t
			if d := now.Sub(started); d > 0 {
				up = d
			}
		}
		comm := ""
		if a := strings.Fields(args); len(a) > 0 {
			comm = filepath.Base(a[0])
		}
		res[pid] = rawProc{PID: pid, PPID: ppid, Comm: comm, Args: args, StartedAt: started, Uptime: up}
	}
	return res
}

// parseDarwinCwds parses `lsof -nP -d cwd -Fpn` into pid→cwd. lsof emits p/f/n
// records; we key only on 'p' (process) and 'n' (the cwd path), ignoring 'f'.
func parseDarwinCwds(out string) map[int]string {
	res := map[int]string{}
	cur := 0
	for _, ln := range strings.Split(out, "\n") {
		if ln == "" {
			continue
		}
		switch ln[0] {
		case 'p':
			cur, _ = strconv.Atoi(ln[1:])
		case 'n':
			if cur != 0 {
				res[cur] = ln[1:]
			}
		}
	}
	return res
}

// parseDarwinPorts parses `lsof -nP -iTCP -sTCP:LISTEN -FpPn` into pid→ports. The
// 'n' field holds a name like "*:8888", "127.0.0.1:5432", or "[::1]:8888"; the
// port is whatever follows the last ':'.
func parseDarwinPorts(out string) map[int][]int {
	res := map[int][]int{}
	cur := 0
	for _, ln := range strings.Split(out, "\n") {
		if ln == "" {
			continue
		}
		switch ln[0] {
		case 'p':
			cur, _ = strconv.Atoi(ln[1:])
		case 'n':
			if cur == 0 {
				continue
			}
			name := ln[1:]
			i := strings.LastIndex(name, ":")
			if i < 0 || i == len(name)-1 {
				continue
			}
			if port, err := strconv.Atoi(name[i+1:]); err == nil {
				res[cur] = append(res[cur], port)
			}
		}
	}
	return res
}

// --- Linux: parse /proc contents (consumed by collect_linux.go) ---

// parseProcStat extracts ppid and starttime (in clock ticks) from /proc/<pid>/stat.
// comm (field 2) can contain spaces and parentheses, so we anchor on the LAST ')'
// and count the space-separated fields after it: state(0) ppid(1) … starttime(19).
func parseProcStat(s string) (ppid int, startTicks int64, ok bool) {
	r := strings.LastIndexByte(s, ')')
	if r < 0 {
		return 0, 0, false
	}
	rest := strings.Fields(s[r+1:])
	if len(rest) < 20 {
		return 0, 0, false
	}
	pp, err := strconv.Atoi(rest[1])
	if err != nil {
		return 0, 0, false
	}
	st, err := strconv.ParseInt(rest[19], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return pp, st, true
}

// procStateZombie reports whether /proc/<pid>/stat shows a zombie (state field,
// the first token after the last ')', is "Z").
func procStateZombie(stat string) bool {
	r := strings.LastIndexByte(stat, ')')
	if r < 0 {
		return false
	}
	rest := strings.Fields(stat[r+1:])
	return len(rest) > 0 && rest[0] == "Z"
}

// commFromStat returns the comm field of /proc/<pid>/stat (between first '(' and
// last ')').
func commFromStat(s string) string {
	l := strings.IndexByte(s, '(')
	r := strings.LastIndexByte(s, ')')
	if l < 0 || r < 0 || r <= l {
		return ""
	}
	return s[l+1 : r]
}

// parseCmdline turns the NUL-separated /proc/<pid>/cmdline into a space-joined
// argv string. Empty (kernel threads) returns "".
func parseCmdline(s string) string {
	s = strings.TrimRight(s, "\x00")
	if s == "" {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(s, "\x00", " "))
}

// parseProcNetTCP parses /proc/net/tcp{,6} into socket-inode→port for sockets in
// the LISTEN state (st == "0A"). Columns: sl local_address rem_address st … inode.
func parseProcNetTCP(s string) map[uint64]int {
	res := map[uint64]int{}
	for _, ln := range strings.Split(s, "\n") {
		f := strings.Fields(ln)
		if len(f) < 10 || f[3] != "0A" { // header line and non-LISTEN rows fall out here
			continue
		}
		local := f[1]
		c := strings.LastIndex(local, ":")
		if c < 0 {
			continue
		}
		port, err := strconv.ParseUint(local[c+1:], 16, 32)
		if err != nil {
			continue
		}
		ino, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil || ino == 0 {
			continue
		}
		res[ino] = int(port)
	}
	return res
}

// socketInode parses a /proc/<pid>/fd symlink target of the form "socket:[12345]".
func socketInode(link string) (uint64, bool) {
	if !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
		return 0, false
	}
	n, err := strconv.ParseUint(link[len("socket:["):len(link)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
