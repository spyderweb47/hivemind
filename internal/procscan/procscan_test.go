package procscan

import (
	"testing"
	"time"

	"hivemind/internal/config"
	"hivemind/internal/paths"
)

// --- pure parser tests (run on every platform) ---

func TestParseDarwinPS(t *testing.T) {
	now := time.Date(2026, 6, 24, 15, 0, 0, 0, time.Local)
	out := "  481  1 Wed Jun 24 14:30:00 2026 /opt/py/bin/python3.11 -m jupyterlab --port 8888\n" +
		" 1002 481 Wed Jun  4 09:00:00 2026 /usr/bin/some prog with spaces\n" +
		"garbage line\n"
	got := parseDarwinPS(out, now)
	p, ok := got[481]
	if !ok {
		t.Fatalf("pid 481 missing")
	}
	if p.PPID != 1 || p.Comm != "python3.11" {
		t.Errorf("481: ppid=%d comm=%q", p.PPID, p.Comm)
	}
	if p.Args != "/opt/py/bin/python3.11 -m jupyterlab --port 8888" {
		t.Errorf("481 args=%q", p.Args)
	}
	if p.Uptime != 30*time.Minute { // 15:00 - 14:30
		t.Errorf("481 uptime=%v want 30m", p.Uptime)
	}
	// space-padded day "Jun  4" must still parse, and args with spaces preserved.
	if q := got[1002]; q.Args != "/usr/bin/some prog with spaces" {
		t.Errorf("1002 args=%q", q.Args)
	}
}

func TestParseDarwinCwds(t *testing.T) {
	// lsof emits an 'f' line we must ignore.
	out := "p481\nfcwd\nn/proj/research\np999\nfcwd\nn/elsewhere\n"
	got := parseDarwinCwds(out)
	if got[481] != "/proj/research" || got[999] != "/elsewhere" {
		t.Errorf("got %v", got)
	}
}

func TestParseDarwinPorts(t *testing.T) {
	out := "p481\nf11\nPTCP\nn*:8888\nf12\nPTCP\nn*:8888\n" + // dup port, one pid
		"p999\nf20\nPTCP\nn127.0.0.1:5432\n" +
		"p1001\nf3\nPTCP\nn[::1]:6000\n"
	got := parseDarwinPorts(out)
	if len(got[481]) != 2 { // parser keeps raw dups; attribute() dedupes
		t.Errorf("481 ports=%v", got[481])
	}
	if got[999][0] != 5432 {
		t.Errorf("999 ports=%v", got[999])
	}
	if got[1001][0] != 6000 { // IPv6: port after last ':'
		t.Errorf("1001 ports=%v", got[1001])
	}
}

func TestParseProcStat(t *testing.T) {
	// comm contains spaces and a ')' to exercise the last-')' anchor.
	// fields after comm: state ppid pgrp ... starttime(field22 => index19)
	stat := "1234 (weird )name) S 481 1234 1234 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 4242 0 0"
	ppid, ticks, ok := parseProcStat(stat)
	if !ok {
		t.Fatalf("parse failed")
	}
	if ppid != 481 {
		t.Errorf("ppid=%d want 481", ppid)
	}
	if ticks != 4242 {
		t.Errorf("starttime=%d want 4242", ticks)
	}
	if c := commFromStat(stat); c != "weird )name" {
		t.Errorf("comm=%q want %q", c, "weird )name")
	}
}

func TestParseProcNetTCP(t *testing.T) {
	s := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000  0 987654 1 0000 100 0\n" +
		"   1: 0100007F:C000 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1000  0 111 1 0000\n" // ESTABLISHED, skipped
	got := parseProcNetTCP(s)
	if got[987654] != 0x1F90 { // 8080
		t.Errorf("got %v want inode 987654 -> 8080", got)
	}
	if len(got) != 1 {
		t.Errorf("only the LISTEN row should be kept, got %v", got)
	}
}

func TestParseCmdlineAndSocketInode(t *testing.T) {
	if a := parseCmdline("python3\x00-m\x00http.server\x008000\x00"); a != "python3 -m http.server 8000" {
		t.Errorf("cmdline=%q", a)
	}
	if parseCmdline("\x00") != "" {
		t.Errorf("empty cmdline should be blank")
	}
	if ino, ok := socketInode("socket:[987654]"); !ok || ino != 987654 {
		t.Errorf("socketInode = %d,%v", ino, ok)
	}
	if _, ok := socketInode("/dev/null"); ok {
		t.Errorf("non-socket link should not parse")
	}
}

// --- attribution / dedupe / noise-filter tests ---

func proj() (paths.Project, *config.Project) {
	p := paths.NewProject("/proj")
	cfg := &config.Project{
		Project: "t",
		Agents: []config.Agent{
			{Name: "research", Workspace: "research"},
			{Name: "nested", Workspace: "research/inner"},
		},
	}
	return p, cfg
}

// raw builds a record with a KNOWN (non-zero) start time, as a real collector
// produces. The transient gate only applies when the start time is known.
func raw(pid, ppid int, comm, args, cwd string, up time.Duration) rawProc {
	return rawProc{PID: pid, PPID: ppid, Comm: comm, Args: args, CWD: cwd,
		StartedAt: time.Unix(1_700_000_000, 0), Uptime: up}
}

func find(ps []Proc, pid int) *Proc {
	for i := range ps {
		if ps[i].PID == pid {
			return &ps[i]
		}
	}
	return nil
}

func TestAttributeCwdAndFleet(t *testing.T) {
	p, cfg := proj()
	raws := map[int]rawProc{
		10: raw(10, 1, "python3", "python3 -m jupyterlab", "/proj/research", time.Minute),
		11: raw(11, 1, "media_encoder", "/proj/research/media_encoder --live", "/proj/research/sub", time.Hour), // no port, long-lived
		12: raw(12, 1, "redis-server", "redis-server", "/proj", time.Minute),                                    // fleet (root, no workspace)
		13: raw(13, 1, "python3", "python3 x", "/somewhere/else", time.Minute),                                  // unrelated → dropped
	}
	ports := map[int][]int{10: {8888}}
	got := attribute(p, cfg, raws, ports, nil)

	if pr := find(got, 10); pr == nil || pr.Agent != "research" || len(pr.Ports) != 1 || pr.Ports[0] != 8888 {
		t.Errorf("pid10: %+v", pr)
	}
	if pr := find(got, 11); pr == nil || pr.Agent != "research" { // long-lived non-listener kept
		t.Errorf("pid11 (long-running binary, no port) should be kept under research: %+v", pr)
	}
	if pr := find(got, 12); pr == nil || pr.Agent != "fleet" {
		t.Errorf("pid12 should be 'fleet': %+v", pr)
	}
	if find(got, 13) != nil {
		t.Errorf("pid13 (outside project) must be dropped")
	}
}

func TestAttributeLongestPrefixWins(t *testing.T) {
	p, cfg := proj()
	raws := map[int]rawProc{
		20: raw(20, 1, "node", "node server.js", "/proj/research/inner/app", time.Minute),
	}
	got := attribute(p, cfg, raws, map[int][]int{20: {3000}}, nil)
	if pr := find(got, 20); pr == nil || pr.Agent != "nested" {
		t.Errorf("nested workspace should win over parent: %+v", pr)
	}
}

func TestAttributePPIDWalkForChdir(t *testing.T) {
	p, cfg := proj()
	raws := map[int]rawProc{
		30: raw(30, 1, "python3", "python3 parent", "/proj/research", time.Minute), // attributed by cwd
		31: raw(31, 30, "postgres", "postgres -D /data", "/", time.Hour),           // chdir'd to /, parent is 30
	}
	got := attribute(p, cfg, raws, map[int][]int{31: {5432}}, nil)
	if pr := find(got, 31); pr == nil || pr.Agent != "research" {
		t.Errorf("chdir'd daemon should inherit agent via PPID walk: %+v", pr)
	}
}

func TestAttributeDedupeTmux(t *testing.T) {
	p, cfg := proj()
	raws := map[int]rawProc{
		40: raw(40, 1, "python3", "python3 -m jupyterlab", "/proj/research", time.Minute), // the tmux pane PID
		41: raw(41, 40, "python3", "python3 child", "/proj/research", time.Minute),        // child of the tmux service
	}
	tmux := map[int]bool{40: true}
	got := attribute(p, cfg, raws, map[int][]int{40: {8888}}, tmux)
	if find(got, 40) != nil {
		t.Errorf("registered tmux service pane should be deduped out")
	}
	if find(got, 41) != nil {
		t.Errorf("child of a tmux service should also be deduped out")
	}
}

func TestAttributeNoiseFilter(t *testing.T) {
	p, cfg := proj()
	raws := map[int]rawProc{
		50: raw(50, 1, "claude", "claude -p --resume", "/proj/research", time.Hour),
		51: raw(51, 1, "node", "node /x/cli.js claude", "/proj/research", time.Hour),         // Claude under node
		52: raw(52, 1, "node", "node /proj/research/server.js", "/proj/research", time.Hour), // user's node → kept
		53: raw(53, 1, "hivemind", "hivemind __turn --agent research", "/proj/research", time.Hour),
		54: raw(54, 1, "zsh", "-zsh", "/proj/research", time.Hour),
		55: raw(55, 1, "python3", "python3 quickjob", "/proj/research", time.Second), // transient, no port → dropped
		56: raw(56, 1, "lsof", "lsof -nP", "/proj/research", time.Hour),
	}
	got := attribute(p, cfg, raws, map[int][]int{52: {4000}}, nil)
	for _, pid := range []int{50, 51, 53, 54, 55, 56} {
		if find(got, pid) != nil {
			t.Errorf("pid %d should have been filtered as noise/transient", pid)
		}
	}
	if pr := find(got, 52); pr == nil || pr.Agent != "research" {
		t.Errorf("user's own node server should be kept: %+v", pr)
	}
}

// A long-lived port-less process whose start time could not be determined
// (StartedAt zero, Uptime 0 — e.g. a non-English ps locale on macOS or an
// unreadable /proc/uptime on Linux) must be KEPT, not mistaken for transient.
func TestAttributeUnknownStartFailsOpen(t *testing.T) {
	p, cfg := proj()
	raws := map[int]rawProc{
		70: {PID: 70, PPID: 1, Comm: "media_encoder", Args: "/proj/research/media_encoder", CWD: "/proj/research"}, // StartedAt zero, Uptime 0
	}
	got := attribute(p, cfg, raws, nil, nil)
	if pr := find(got, 70); pr == nil || pr.Agent != "research" {
		t.Errorf("process with unknown start time should be kept (fail-open): %+v", pr)
	}
}

func TestProcStateZombie(t *testing.T) {
	if !procStateZombie("123 (python3) Z 1 123 123 0 -1 0 0 0") {
		t.Errorf("Z state should be detected as zombie")
	}
	if procStateZombie("123 (python3) S 1 123 123 0 -1 0 0 0") {
		t.Errorf("S state is not a zombie")
	}
}

func TestAttributeDropsDefunct(t *testing.T) {
	p, cfg := proj()
	raws := map[int]rawProc{
		60: raw(60, 1, "<defunct>", "<defunct>", "/proj/research", time.Hour), // macOS zombie
		61: raw(61, 1, "python3", "python3 /proj/research/app.py", "/proj/research", time.Hour),
	}
	got := attribute(p, cfg, raws, nil, nil)
	if find(got, 60) != nil {
		t.Errorf("<defunct> zombie must be filtered out")
	}
	if find(got, 61) == nil {
		t.Errorf("the real process should remain")
	}
}

func TestProcDisplay(t *testing.T) {
	cases := []struct {
		comm, args, want string
	}{
		{"Python", "/Library/Frameworks/Python -m jupyterlab --port 8888", "jupyterlab"},
		{"python3", "python3 /srv/app_server.py --live", "app_server.py"},
		{"node", "node /app/server.js", "server.js"},
		{"mytool", "/proj/mytool --live", "mytool"}, // non-interpreter: unchanged
		{"python3", "python3", "python3"},           // interpreter, no script → unchanged
		{"redis-server", "redis-server *:6379", "redis-server"},
	}
	for _, c := range cases {
		pr := Proc{Command: c.comm, Args: c.args}
		if got := pr.Display(); got != c.want {
			t.Errorf("Display(%q,%q)=%q want %q", c.comm, c.args, got, c.want)
		}
	}
}

func TestScanNoConfigOrUnavailable(t *testing.T) {
	// nil config → nil result, regardless of platform availability.
	if got := Scan(paths.NewProject("/proj"), nil); got != nil {
		t.Errorf("Scan(nil cfg) = %v, want nil", got)
	}
}
