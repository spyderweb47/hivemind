package cmd

import (
	"path/filepath"
	"time"

	"hivemind/internal/paths"
	"hivemind/internal/session"
)

// lockConfig takes an exclusive lock around a project's config so a
// load→modify→save sequence can't interleave with another hivemind process (the
// console and the supervisor both shell out to config-mutating commands, so two
// could otherwise read the same config and clobber each other's write).
//
// It is best-effort: if the lock can't be acquired within the timeout it returns a
// no-op release and the caller proceeds unlocked rather than hanging. The flock is
// freed automatically if the process dies. Callers MUST (re)load config AFTER
// calling this — typically `defer lockConfig(p)()` then `config.Load(...)` — so the
// read happens inside the locked window.
func lockConfig(p paths.Project) func() {
	lk, err := session.NewLock(filepath.Join(p.HivemindDir(), "config.lock"))
	if err != nil {
		return func() {}
	}
	if err := lk.Acquire(10 * time.Second); err != nil {
		return func() {}
	}
	return func() { _ = lk.Release() }
}
