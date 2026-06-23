// Package session handles agent session identity and the per-agent prompt lock.
//
// Each agent owns a stable UUID (its Claude Code --session-id), assigned once at
// setup and persisted under .hivemind/agents/<name>/session_id. A flock-based lock
// serializes prompts so a user-initiated send and a supervisor-initiated send can
// never hit the same on-disk session concurrently (the second blocks/queues).
package session

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// NewID returns a random RFC-4122 v4 UUID string suitable for --session-id.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is fatal-ish; fall back to time-seeded bytes.
		t := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(t >> (i % 8 * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ReadID returns the persisted session id for an agent given its session file path.
func ReadID(sessionFile string) (string, error) {
	b, err := os.ReadFile(sessionFile)
	if err != nil {
		return "", err
	}
	id := string(b)
	// trim trailing whitespace/newline
	for len(id) > 0 && (id[len(id)-1] == '\n' || id[len(id)-1] == '\r' || id[len(id)-1] == ' ') {
		id = id[:len(id)-1]
	}
	return id, nil
}

// WriteID persists a session id, creating the parent dir as needed.
func WriteID(sessionFile, id string) error {
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(sessionFile, []byte(id+"\n"), 0o644)
}

// Lock is an advisory inter-process lock backed by flock(2).
type Lock struct {
	path string
	fd   int
}

// ErrLocked is returned by TryAcquire when the lock is already held.
var ErrLocked = errors.New("agent is busy: another prompt is in flight")

// NewLock prepares (does not acquire) a lock at the given path.
func NewLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	return &Lock{path: path, fd: fd}, nil
}

// TryAcquire takes the lock without blocking; returns ErrLocked if held elsewhere.
func (l *Lock) TryAcquire() error {
	if err := syscall.Flock(l.fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLocked
		}
		return err
	}
	return nil
}

// Acquire blocks until the lock is held or ctx-equivalent timeout elapses. A zero
// timeout blocks indefinitely (the "queue behind the in-flight prompt" behavior).
func (l *Lock) Acquire(timeout time.Duration) error {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		err := l.TryAcquire()
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrLocked) {
			return err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return ErrLocked
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// Release unlocks and closes the lock file descriptor.
func (l *Lock) Release() error {
	if l.fd == 0 {
		return nil
	}
	_ = syscall.Flock(l.fd, syscall.LOCK_UN)
	err := syscall.Close(l.fd)
	l.fd = 0
	return err
}

// Held reports whether some process currently holds the lock (best-effort probe).
func Held(path string) bool {
	fd, err := syscall.Open(path, syscall.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(fd, syscall.LOCK_UN)
	return false
}
