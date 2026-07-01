// Package transcript locates and parses Claude Code session transcripts.
//
// Claude Code writes each session as an append-only JSONL file under
// ~/.claude/projects/<mangled-cwd>/<session-id>.jsonl. We never hardcode the
// mangled-cwd hash: an agent's transcript is found by globbing for its pre-assigned
// <session-id>.jsonl. The parser is deliberately tolerant of schema drift across
// Claude Code versions — it reads the fields it understands and ignores the rest —
// because the control plane must keep working as the transcript format evolves.
package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Locate returns the path to the transcript for a session id, or ("", false).
// Claude Code writes transcripts one level deep (projects/<mangled-cwd>/<sid>.jsonl);
// we also check the flat case. We match by the pre-assigned session id, never by a
// hardcoded directory hash. (Go's filepath.Glob is not recursive, so we enumerate
// the layouts we expect rather than relying on a "**" wildcard.)
func Locate(projectsDir, sessionID string) (string, bool) {
	patterns := []string{
		filepath.Join(projectsDir, "*", sessionID+".jsonl"),
		filepath.Join(projectsDir, sessionID+".jsonl"),
	}
	var newest string
	var newestMod time.Time
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			if fi.ModTime().After(newestMod) {
				newest, newestMod = m, fi.ModTime()
			}
		}
	}
	return newest, newest != ""
}

// Summary is the derived, LLM-free view of a transcript used for liveness, current
// activity, and cost.
type Summary struct {
	Path          string
	Exists        bool
	LastEventTime time.Time // wall-clock of the most recent event
	LastTool      string    // name of the most recent tool_use, if any
	LastToolInput string    // short human summary of that tool's input
	LastText      string    // last assistant text snippet (truncated for tables)
	LastTextFull  string    // last assistant text, untruncated (for the detail view)
	Turns         int       // number of assistant turns observed
	Errored       bool      // last result/turn ended in error
	NeedsInput    bool      // a turn flagged it is blocked waiting on input

	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheCreate  int
}

// TotalTokens is a simple roll-up used for cost display.
func (s Summary) TotalTokens() int {
	return s.InputTokens + s.OutputTokens + s.CacheRead + s.CacheCreate
}

// line is the subset of a transcript record we care about. Unknown fields are
// ignored by encoding/json.
type line struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	Timestamp string          `json:"timestamp"`
	IsError   bool            `json:"is_error"`
	Message   json.RawMessage `json:"message"`
	// Some Claude Code versions place usage at the top level of a result record.
	Usage *usage `json:"usage"`
}

type message struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Usage   *usage          `json:"usage"`
	Content json.RawMessage `json:"content"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Parse reads a transcript file and derives a Summary. A missing file yields
// Exists=false with no error (the session simply hasn't started yet).
func Parse(path string) (Summary, error) {
	s := Summary{Path: path}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	defer f.Close()
	s.Exists = true

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024) // tolerate large lines
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var ln line
		if err := json.Unmarshal([]byte(raw), &ln); err != nil {
			continue // skip malformed lines rather than failing the whole read
		}
		if ts := parseTime(ln.Timestamp); !ts.IsZero() {
			s.LastEventTime = ts
		}

		// Top-level usage (result records on some versions).
		if ln.Usage != nil {
			s.addUsage(ln.Usage)
		}

		// Error detection.
		if ln.IsError || (ln.Type == "result" && strings.Contains(ln.Subtype, "error")) {
			s.Errored = true
		} else if ln.Type == "result" {
			s.Errored = false // a clean result clears a prior error
		}

		if len(ln.Message) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(ln.Message, &m); err != nil {
			continue
		}
		if m.Usage != nil {
			s.addUsage(m.Usage)
		}
		switch m.Role {
		case "assistant":
			s.Turns++
			s.NeedsInput = false // recomputed from this turn's content below
			s.consumeContent(m.Content)
		case "user":
			s.NeedsInput = false // a new user turn clears a prior block
		}
	}
	return s, sc.Err()
}

// Item is one element of the recent conversation: a user prompt, an assistant text
// block, or an assistant tool_use. A multi-chunk assistant turn yields ONE Item per
// text block (unlike Summary, which keeps only the last) so the full reply is shown.
type Item struct {
	Role string // "user" | "assistant"
	Kind string // "text" | "tool_use"
	Text string // text content, or a short tool-input summary
	Tool string // tool name (Kind == "tool_use")
	TS   time.Time
}

// Recent streams a transcript and returns up to the last n conversation Items in
// chronological order. It keeps every assistant text block (so chunked replies are
// preserved) and skips tool_result user records (machine output, not human turns).
// A missing file yields (nil, nil). Memory is bounded to n via a small ring.
func Recent(path string, n int) ([]Item, error) {
	if n <= 0 {
		n = 1
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	ring := make([]Item, 0, n)
	push := func(it Item) {
		if len(ring) < n {
			ring = append(ring, it)
			return
		}
		copy(ring, ring[1:])
		ring[n-1] = it
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var ln line
		if json.Unmarshal([]byte(raw), &ln) != nil || len(ln.Message) == 0 {
			continue
		}
		ts := parseTime(ln.Timestamp)
		var m message
		if json.Unmarshal(ln.Message, &m) != nil {
			continue
		}
		switch m.Role {
		case "user":
			for _, t := range userTexts(m.Content) {
				push(Item{Role: "user", Kind: "text", Text: t, TS: ts})
			}
		case "assistant":
			var blocks []contentBlock
			if json.Unmarshal(m.Content, &blocks) == nil {
				for _, b := range blocks {
					switch b.Type {
					case "text":
						if t := strings.TrimSpace(b.Text); t != "" {
							push(Item{Role: "assistant", Kind: "text", Text: t, TS: ts})
						}
					case "tool_use":
						push(Item{Role: "assistant", Kind: "tool_use", Tool: b.Name, Text: snippet(string(b.Input), 80), TS: ts})
					}
				}
				continue
			}
			var str string
			if json.Unmarshal(m.Content, &str) == nil {
				if t := strings.TrimSpace(str); t != "" {
					push(Item{Role: "assistant", Kind: "text", Text: t, TS: ts})
				}
			}
		}
	}
	return ring, sc.Err()
}

// userTexts extracts the human-authored text from a user message, skipping
// tool_result blocks (which are tool output fed back to the model, not a prompt).
func userTexts(raw json.RawMessage) []string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		if t := strings.TrimSpace(str); t != "" {
			return []string{t}
		}
		return nil
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var out []string
		for _, b := range blocks {
			if b.Type == "text" {
				if t := strings.TrimSpace(b.Text); t != "" {
					out = append(out, t)
				}
			}
		}
		return out
	}
	return nil
}

func (s *Summary) addUsage(u *usage) {
	s.InputTokens += u.InputTokens
	s.OutputTokens += u.OutputTokens
	s.CacheRead += u.CacheReadInputTokens
	s.CacheCreate += u.CacheCreationInputTokens
}

// consumeContent records the last text/tool_use seen in an assistant message.
// Content may be a string or an array of blocks depending on version.
func (s *Summary) consumeContent(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	// Try array-of-blocks first.
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					s.LastText = snippet(t, 200)
					s.LastTextFull = t
					if isBlocked(t) {
						s.NeedsInput = true
					}
				}
			case "tool_use":
				s.LastTool = b.Name
				s.LastToolInput = snippet(string(b.Input), 120)
			}
		}
		return
	}
	// Fall back to a plain string.
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if t := strings.TrimSpace(str); t != "" {
			s.LastText = snippet(t, 200)
			s.LastTextFull = t
			if isBlocked(t) {
				s.NeedsInput = true
			}
		}
	}
}

// isBlocked detects the sentinel the scaffolded CLAUDE.md asks agents to prefix
// their final message with when they are waiting on input.
func isBlocked(text string) bool {
	lt := strings.ToLower(strings.TrimSpace(text))
	return strings.HasPrefix(lt, "blocked:") || strings.HasPrefix(lt, "[blocked]")
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n { // slice on rune boundary, never mid-UTF-8
		return string(r[:n]) + "…"
	}
	return s
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
