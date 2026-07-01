package dashboard

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// rawMarkers reports leftover markdown syntax that should have been consumed.
func hasRawMarkers(s string) bool {
	return strings.Contains(s, "**") || strings.Contains(s, "## ") ||
		strings.Contains(s, "`") || strings.HasPrefix(strings.TrimSpace(s), "- ")
}

func TestMarkdownStripsSyntaxAndKeepsStructure(t *testing.T) {
	src := strings.Join([]string{
		"## Fleet digest",
		"**Bold lead.** Body with `code` and *italic*.",
		"- first bullet",
		"- second `bullet`",
		"> a quoted note",
		"1. step one",
		"```",
		"x = 1",
		"```",
	}, "\n")
	lines := renderMarkdown(src, 70)
	out := strings.Join(lines, "\n")
	if hasRawMarkers(out) {
		t.Errorf("raw markdown syntax leaked into output:\n%s", out)
	}
	if !strings.Contains(out, "•") {
		t.Errorf("bullets should render with a • marker")
	}
	if !strings.Contains(out, "│") {
		t.Errorf("blockquote should render with a │ bar")
	}
	if !strings.Contains(out, "1.") {
		t.Errorf("numbered list marker should be kept")
	}
	if !strings.Contains(out, "x = 1") || strings.Contains(out, "```") {
		t.Errorf("code fence content should show without the ``` markers")
	}
}

// Underscores must stay literal so snake_case identifiers aren't italicized.
func TestMarkdownKeepsSnakeCase(t *testing.T) {
	out := strings.Join(renderMarkdown("ran data_sync.py and batch_export.py on session_id", 80), "\n")
	for _, id := range []string{"data_sync.py", "batch_export.py", "session_id"} {
		if !strings.Contains(out, id) {
			t.Errorf("snake_case identifier %q was corrupted: %q", id, out)
		}
	}
}

// A style boundary with no source space must not gain one ("**bold**:" → "bold:").
func TestMarkdownNoSpaceAtStyleBoundary(t *testing.T) {
	out := strings.Join(renderMarkdown("**Label**: value here", 80), "\n")
	if !strings.Contains(out, "Label: value") {
		t.Errorf("expected 'Label: value' with no inserted space, got %q", out)
	}
}

// Wrapping must be ANSI-aware: no rendered line exceeds the target width.
func TestMarkdownWrapWidth(t *testing.T) {
	long := "This is a reasonably long paragraph with **bold spans** and `code` that must wrap to the requested width without any single line exceeding it."
	width := 40
	for _, ln := range renderMarkdown(long, width) {
		if w := lipgloss.Width(ln); w > width {
			t.Errorf("line width %d exceeds %d: %q", w, width, ln)
		}
	}
}

// A single over-long token (URL/path) must hard-break so no line exceeds width.
func TestMarkdownHardBreaksLongWord(t *testing.T) {
	long := "see https://example.com/" + strings.Repeat("a", 90) + "/end and `also_a_very_long_inline_code_token_that_keeps_going_and_going`"
	width := 30
	for _, ln := range renderMarkdown(long, width) {
		if w := lipgloss.Width(ln); w > width {
			t.Errorf("line width %d exceeds %d after hard-break: %q", w, width, ln)
		}
	}
	// deeply-nested bullet at the minimum width must also stay bounded.
	for _, ln := range renderMarkdown("              - deeply nested item here", 24) {
		if w := lipgloss.Width(ln); w > 24 {
			t.Errorf("nested bullet line width %d exceeds 24: %q", w, ln)
		}
	}
}

func TestMarkdownAppliesStyling(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	out := strings.Join(renderMarkdown("## H\n**b** `c` *i*", 60), "\n")
	if !strings.Contains(out, "\x1b[1m") && !strings.Contains(out, "\x1b[1;") {
		t.Errorf("expected bold ANSI, got %q", out)
	}
	if !strings.Contains(out, "\x1b[3m") {
		t.Errorf("expected italic ANSI, got %q", out)
	}
	if !strings.Contains(out, "\x1b[38;2;") {
		t.Errorf("expected truecolor ANSI for headings/code, got %q", out)
	}
}
