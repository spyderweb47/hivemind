// Package events is the push side of progress tracking. Each agent's Stop hook
// fires after every turn (confirmed to run under headless `claude -p`) and calls
// `hivemind __hook-stop`, which appends a structured Event here. The control plane
// and the supervisor read this feed to summarize "what happened on the last turn"
// without re-deriving everything from transcripts.
//
// events.log is an append-only JSONL file (one Event per line). Reads tolerate
// unknown/missing fields so the format can evolve.
package events

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kind discriminates event types; today the only producer is the Stop hook.
const (
	KindStop = "stop"
)

// Event is one push record appended after a turn.
type Event struct {
	Kind      string    `json:"kind"`
	Agent     string    `json:"agent"`
	TS        time.Time `json:"ts"`
	SessionID string    `json:"session_id,omitempty"`
	Summary   string    `json:"summary,omitempty"` // last assistant message (truncated)
	LastTool  string    `json:"last_tool,omitempty"`
	Tokens    int       `json:"tokens,omitempty"`       // per-turn delta (total)
	InTokens  int       `json:"in_tokens,omitempty"`    // per-turn delta (input+cache)
	OutTokens int       `json:"out_tokens,omitempty"`   // per-turn delta (output)
	CostUSD   float64   `json:"cost_usd,omitempty"`     // per-turn delta
	CumTokens int       `json:"cum_tokens,omitempty"`   // cumulative session total
	CumIn     int       `json:"cum_in,omitempty"`       // cumulative input+cache
	CumOut    int       `json:"cum_out,omitempty"`      // cumulative output
	CumCost   float64   `json:"cum_cost_usd,omitempty"` // cumulative session total
	Blocked   bool      `json:"blocked,omitempty"`
	Errored   bool      `json:"errored,omitempty"`
}

// Append writes one event as a JSON line, creating parent dirs as needed. Append
// to a single file from short-lived processes is atomic enough for our line sizes
// on local filesystems; we never rewrite existing lines.
func Append(path string, e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// ReadAll parses every event in a log (missing file → empty, no error).
func ReadAll(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// Tail returns the last n events (all if n<=0).
func Tail(path string, n int) []Event {
	all, _ := ReadAll(path)
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// LatestAny returns the most recent event in a (per-agent) log.
func LatestAny(path string) (Event, bool) {
	all, _ := ReadAll(path)
	if len(all) == 0 {
		return Event{}, false
	}
	return all[len(all)-1], true
}
