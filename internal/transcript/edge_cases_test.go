package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecentRingBufferOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	// Create 10 user messages in order
	content := ""
	for i := 1; i <= 10; i++ {
		c := "0" + string(rune(48+i))
		if i > 9 {
			c = "1" + string(rune(48+i-10))
		}
		content += `{"type":"user","timestamp":"2026-06-23T10:00:00Z","message":{"role":"user","content":"msg` + c + `"}}` + "\n"
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Get last 3 with ring buffer
	items, err := Recent(path, 3)
	if err != nil {
		t.Fatal(err)
	}

	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}

	// Should be items 8, 9, 10 in order (chronological)
	expectedMsgs := []string{"msg08", "msg09", "msg10"}
	for i, it := range items {
		if it.Text != expectedMsgs[i] {
			t.Errorf("item[%d].Text = %q, want %q", i, it.Text, expectedMsgs[i])
		}
	}
}

func TestRecentZeroOrNegativeN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	if err := os.WriteFile(path, []byte(`{"type":"user","timestamp":"2026-06-23T10:00:00Z","message":{"role":"user","content":"test"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// n <= 0 should default to 1
	items, err := Recent(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("n=0: got %d items, want 1", len(items))
	}

	items, err = Recent(path, -5)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("n=-5: got %d items, want 1", len(items))
	}
}

func TestRecentMissingFile(t *testing.T) {
	items, err := Recent("/nonexistent/path/file.jsonl", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if items != nil {
		t.Fatalf("got %v, want nil", items)
	}
}
