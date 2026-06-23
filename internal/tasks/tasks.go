// Package tasks is the delegation layer: a task is one unit of work routed to one
// agent (its prompt). Tasks are created by `hivemind delegate` (the supervisor
// decomposes a high-level instruction into per-agent tasks) or `hivemind task`
// (an explicit one). They live on a small JSON "board" and are completed
// automatically when the assigned agent's turn finishes (correlated via a
// per-agent active-task marker — safe because the per-agent lock serializes turns).
package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"hivemind/internal/paths"
	"hivemind/internal/session"
)

// Status values.
const (
	Pending    = "pending"
	Dispatched = "dispatched"
	Done       = "done"
	Blocked    = "blocked"
	Failed     = "failed"
)

// Source values.
const (
	SourceDelegate = "delegate"
	SourceManual   = "manual"
)

// Task is one routed unit of work.
type Task struct {
	ID      string    `json:"id"`
	Agent   string    `json:"agent"`
	Prompt  string    `json:"prompt"`
	Status  string    `json:"status"`
	Source  string    `json:"source"`
	Origin  string    `json:"origin,omitempty"` // the high-level instruction, if delegated
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// List returns all tasks (missing board → empty).
func List(p paths.Project) []Task {
	b, err := os.ReadFile(p.TasksFile())
	if err != nil {
		return nil
	}
	var ts []Task
	_ = json.Unmarshal(b, &ts)
	return ts
}

func save(p paths.Project, ts []Task) error {
	if err := os.MkdirAll(p.HivemindDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(ts, "", "  ")
	tmp := p.TasksFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.TasksFile())
}

// mutate runs fn over the board under a lock so concurrent create/complete from
// short-lived processes (a delegate command + a Stop hook) don't clobber. If the
// lock can't be acquired we return the error WITHOUT saving — dropping one update
// is far better than a lost-update clobber of the whole board.
func mutate(p paths.Project, fn func([]Task) []Task) error {
	lock, err := session.NewLock(filepath.Join(p.HivemindDir(), "tasks.lock"))
	if err != nil {
		return err
	}
	if err := lock.Acquire(10 * time.Second); err != nil {
		return err
	}
	defer lock.Release()
	return save(p, fn(List(p)))
}

// Add creates a task (status pending) and returns it.
func Add(p paths.Project, t Task) (Task, error) {
	now := time.Now().UTC()
	t.Created, t.Updated = now, now
	if t.Status == "" {
		t.Status = Pending
	}
	err := mutate(p, func(ts []Task) []Task {
		t.ID = nextID(ts)
		return append(ts, t)
	})
	return t, err
}

// SetStatus updates one task's status by id.
func SetStatus(p paths.Project, id, status string) error {
	return mutate(p, func(ts []Task) []Task {
		for i := range ts {
			if ts[i].ID == id {
				ts[i].Status = status
				ts[i].Updated = time.Now().UTC()
			}
		}
		return ts
	})
}

// Open returns tasks not yet in a terminal state.
func Open(p paths.Project) []Task {
	var out []Task
	for _, t := range List(p) {
		if t.Status == Pending || t.Status == Dispatched {
			out = append(out, t)
		}
	}
	return out
}

func nextID(ts []Task) string {
	max := 0
	for _, t := range ts {
		if n, err := strconv.Atoi(strings.TrimPrefix(t.ID, "t")); err == nil && n > max {
			max = n
		}
	}
	return "t" + strconv.Itoa(max+1)
}
