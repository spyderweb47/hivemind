package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/session"
	"hivemind/internal/transcript"
)

func newTranscriptCmd() *cobra.Command {
	var tail int
	c := &cobra.Command{
		Use:   "transcript <agent>",
		Short: "Pretty-print the tail of an agent's JSONL transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			name := args[0]
			if name != config.SupervisorName && cfg.FindAgent(name) == nil {
				return fmt.Errorf("unknown agent %q", name)
			}
			tgt, err := agent.ResolveTarget(p, cfg, name)
			if err != nil {
				return err
			}
			sid, err := session.ReadID(tgt.SessionFile)
			if err != nil {
				return fmt.Errorf("agent %q has no session id", name)
			}
			path, ok := transcript.Locate(paths.ClaudeProjectsDir(), sid)
			if !ok {
				fmt.Printf("(no transcript yet for %s — session %s has not started)\n", name, sid)
				return nil
			}
			lines, err := lastLines(path, tail)
			if err != nil {
				return err
			}
			fmt.Printf("transcript: %s (last %d events)\n\n", path, len(lines))
			for _, ln := range lines {
				fmt.Println(formatEvent(ln))
			}
			return nil
		},
	}
	c.Flags().IntVar(&tail, "tail", 20, "number of trailing events to show")
	return c
}

func lastLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	var all []string
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			all = append(all, t)
		}
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, sc.Err()
}

// formatEvent renders one transcript JSONL record as a readable one/two-liner.
func formatEvent(raw string) string {
	var rec struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
	}
	if json.Unmarshal([]byte(raw), &rec) != nil {
		return dim("· " + trunc(raw, 100))
	}
	ts := rec.Timestamp
	if len(ts) > 19 {
		ts = ts[11:19]
	}
	switch rec.Type {
	case "user":
		return fmt.Sprintf("[%s] \033[36muser\033[0m      %s", ts, trunc(extractText(rec.Message), 100))
	case "assistant":
		return fmt.Sprintf("[%s] \033[32massistant\033[0m %s", ts, trunc(extractText(rec.Message), 100))
	case "result":
		return dim(fmt.Sprintf("[%s] result", ts))
	default:
		return dim(fmt.Sprintf("[%s] %s", ts, rec.Type))
	}
}

func extractText(raw json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	// content as string
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	// content as blocks
	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(m.Content, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, b.Text)
			case "tool_use":
				parts = append(parts, fmt.Sprintf("⚙ %s(%s)", b.Name, trunc(string(b.Input), 50)))
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func dim(s string) string { return "\033[90m" + s + "\033[0m" }
