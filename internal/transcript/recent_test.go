package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecentCapturesAllChunks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	// a user prompt, then an assistant turn with TWO text blocks + a tool_use
	lines := `{"type":"user","timestamp":"2026-06-23T10:00:00Z","message":{"role":"user","content":"do the thing"}}
{"type":"assistant","timestamp":"2026-06-23T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"chunk one"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}},{"type":"text","text":"chunk two"}]}}
{"type":"user","timestamp":"2026-06-23T10:00:06Z","message":{"role":"user","content":[{"type":"tool_result","content":"output"}]}}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := Recent(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	// expect: user "do the thing", assistant "chunk one", tool Bash, assistant "chunk two"
	// (the tool_result user message is skipped)
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4: %+v", len(items), items)
	}
	if items[0].Role != "user" || items[0].Text != "do the thing" {
		t.Errorf("item0 = %+v", items[0])
	}
	if items[1].Text != "chunk one" || items[3].Text != "chunk two" {
		t.Errorf("multi-chunk reply not preserved: %+v", items)
	}
	if items[2].Kind != "tool_use" || items[2].Tool != "Bash" {
		t.Errorf("tool_use not captured: %+v", items[2])
	}
}
