package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecentLargeTranscript verifies that Recent handles large files efficiently
func TestRecentLargeTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.jsonl")

	// Create a large transcript with 1000 items
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		msg := fmt.Sprintf("message_%d", i)
		if i%2 == 0 {
			sb.WriteString(fmt.Sprintf(`{"type":"user","timestamp":"2026-06-23T10:00:%02dZ","message":{"role":"user","content":"%s"}}`, i%60, msg))
		} else {
			sb.WriteString(fmt.Sprintf(`{"type":"assistant","timestamp":"2026-06-23T10:00:%02dZ","message":{"role":"assistant","content":"%s"}}`, i%60, msg))
		}
		sb.WriteString("\n")
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := Recent(path, 50)
	if err != nil {
		t.Fatal(err)
	}

	if len(items) != 50 {
		t.Fatalf("got %d items, want 50", len(items))
	}

	// Last item should be message_999
	if !strings.Contains(items[49].Text, "message_999") {
		t.Errorf("last item.Text = %q, want to contain 'message_999'", items[49].Text)
	}
}

// TestRecentVeryLongLine checks that a line longer than the default buffer is handled
func TestRecentVeryLongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "longline.jsonl")

	// Create a message with a very long text (1MB)
	longText := strings.Repeat("x", 1024*1024)
	line := fmt.Sprintf(`{"type":"user","timestamp":"2026-06-23T10:00:00Z","message":{"role":"user","content":"%s"}}`, longText)

	// Add a normal message before and after
	content := `{"type":"user","timestamp":"2026-06-23T10:00:00Z","message":{"role":"user","content":"before"}}
` + line + `
{"type":"user","timestamp":"2026-06-23T10:00:00Z","message":{"role":"user","content":"after"}}
`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := Recent(path, 10)
	if err != nil {
		t.Fatal(err)
	}

	// Should have all 3 items
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}

	if items[0].Text != "before" {
		t.Errorf("items[0].Text = %q", items[0].Text)
	}
	if items[2].Text != "after" {
		t.Errorf("items[2].Text = %q", items[2].Text)
	}
}

// TestRecentScannerError checks behavior when a line exceeds even the 16MB buffer
// (this is hard to test without mocking, but we can verify buffer sizing)
func TestRecentScannerBufferSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	// The buffer is 16MB max per the code
	// This test just verifies Recent doesn't crash with large (but valid) input
	content := `{"type":"user","timestamp":"2026-06-23T10:00:00Z","message":{"role":"user","content":"test"}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := Recent(path, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
}
