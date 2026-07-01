package dashboard

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// A small markdown→terminal renderer for agent/supervisor messages: it turns the
// markdown agents emit (bold, italics, inline code, headings, bullets, numbered
// lists, blockquotes, rules, code fences) into themed ANSI instead of showing the
// raw syntax. It is line-oriented with an ANSI-aware word wrap, so it fits the
// dashboard's narrow panels without a heavy markdown dependency.
//
// Styling mirrors the conventions of Claude Code's own terminal renderer: **bold**
// → bold, *italic* → italic, `code` → an accent color, # headings → bold (H1 also
// underlined), > quotes → a dim bar + italics, and - bullets. Underscores are left
// literal on purpose so snake_case identifiers (data_sync, batch_export)
// are never mistaken for emphasis.

var (
	mdItalicS = lipgloss.NewStyle().Italic(true)
	mdCodeS   = lipgloss.NewStyle().Foreground(colBrand)                             // inline / fenced code
	mdHead1S  = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(colAccent) // # H1
	mdHeadNS  = lipgloss.NewStyle().Bold(true).Foreground(colAccent)                 // ## H2+
	mdBulletS = lipgloss.NewStyle().Foreground(colAccent)                            // list markers
	mdQuoteS  = lipgloss.NewStyle().Foreground(colDim)                               // blockquote bar / rule
)

var (
	mdHeadingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdBulletRe  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdNumRe     = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+(.*)$`)
)

// mdRun is a styled span of text with no internal line breaks.
type mdRun struct {
	text  string
	style lipgloss.Style
}

// inlineRuns parses a single line's inline markdown into styled runs, inheriting
// base (so emphasis nests). Recognizes **bold**, *italic*, `code`, ~~strike~~.
func inlineRuns(s string, base lipgloss.Style) []mdRun {
	var runs []mdRun
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			runs = append(runs, mdRun{buf.String(), base})
			buf.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "**"):
			if j := strings.Index(s[i+2:], "**"); j >= 0 {
				flush()
				runs = append(runs, inlineRuns(s[i+2:i+2+j], base.Bold(true))...)
				i += 2 + j + 2
				continue
			}
			buf.WriteByte(s[i])
			i++
		case strings.HasPrefix(s[i:], "~~"):
			if j := strings.Index(s[i+2:], "~~"); j >= 0 {
				flush()
				runs = append(runs, inlineRuns(s[i+2:i+2+j], base.Strikethrough(true))...)
				i += 2 + j + 2
				continue
			}
			buf.WriteByte(s[i])
			i++
		case s[i] == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				flush()
				runs = append(runs, mdRun{s[i+1 : i+1+j], mdCodeS})
				i += 1 + j + 1
				continue
			}
			buf.WriteByte(s[i])
			i++
		case s[i] == '*': // single-asterisk italic (underscores left literal)
			if j := strings.IndexByte(s[i+1:], '*'); j > 0 {
				flush()
				runs = append(runs, inlineRuns(s[i+1:i+1+j], base.Italic(true))...)
				i += 1 + j + 1
				continue
			}
			buf.WriteByte(s[i])
			i++
		default:
			buf.WriteByte(s[i])
			i++
		}
	}
	flush()
	return runs
}

// mdWord is one whitespace-delimited word, possibly spanning several styled runs
// (e.g. a bold word immediately followed by a plain colon), with its display width.
type mdWord struct {
	segs []mdRun
	w    int
}

// runsToWords regroups styled runs into words so wrapping never inserts a space at
// a style boundary that had none in the source ("**bold**:" stays "bold:").
func runsToWords(runs []mdRun) []mdWord {
	var words []mdWord
	var cur mdWord
	end := func() {
		if len(cur.segs) > 0 {
			words = append(words, cur)
			cur = mdWord{}
		}
	}
	push := func(text string, st lipgloss.Style) {
		if text == "" {
			return
		}
		cur.segs = append(cur.segs, mdRun{text, st})
		cur.w += utf8.RuneCountInString(text)
	}
	for _, r := range runs {
		i := 0
		for i < len(r.text) {
			sp := strings.IndexByte(r.text[i:], ' ')
			if sp < 0 {
				push(r.text[i:], r.style)
				break
			}
			push(r.text[i:i+sp], r.style)
			end()
			i += sp + 1
		}
	}
	end()
	return words
}

// wrapRuns word-wraps styled runs to width, rendering each word with its styles.
// A single word wider than width is hard-broken at rune boundaries so no rendered
// line ever exceeds width (a long URL or path can't blow out a panel border).
func wrapRuns(runs []mdRun, width int) []string {
	if width < 4 {
		width = 4
	}
	words := runsToWords(runs)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	var cur strings.Builder
	curW := 0
	flush := func() {
		lines = append(lines, cur.String())
		cur.Reset()
		curW = 0
	}
	for _, wd := range words {
		if wd.w > width { // too long for any line — hard-break it
			if curW > 0 {
				flush()
			}
			lines = append(lines, breakWord(wd, width)...)
			continue
		}
		if curW > 0 && curW+1+wd.w > width {
			flush()
		}
		if curW > 0 {
			cur.WriteByte(' ')
			curW++
		}
		for _, sg := range wd.segs {
			cur.WriteString(sg.style.Render(sg.text))
		}
		curW += wd.w
	}
	lines = append(lines, cur.String())
	return lines
}

// breakWord splits one over-long word into lines of at most width runes, at rune
// boundaries, preserving each segment's style.
func breakWord(wd mdWord, width int) []string {
	if width < 1 {
		width = 1
	}
	var lines []string
	var cur strings.Builder
	curW := 0
	for _, sg := range wd.segs {
		runes := []rune(sg.text)
		for i := 0; i < len(runes); {
			if curW >= width {
				lines = append(lines, cur.String())
				cur.Reset()
				curW = 0
			}
			take := width - curW
			if take > len(runes)-i {
				take = len(runes) - i
			}
			cur.WriteString(sg.style.Render(string(runes[i : i+take])))
			curW += take
			i += take
		}
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

// renderMarkdown renders markdown source to themed, width-wrapped terminal lines.
func renderMarkdown(src string, width int) []string {
	if width < 4 {
		width = 4
	}
	var out []string
	inFence := false
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimRight(raw, " \t")
		ts := strings.TrimSpace(line)

		if strings.HasPrefix(ts, "```") || strings.HasPrefix(ts, "~~~") {
			inFence = !inFence
			continue // hide the fence markers themselves
		}
		if inFence {
			out = append(out, mdCodeS.Render(truncate(line, width)))
			continue
		}
		if ts == "" {
			out = append(out, "")
			continue
		}
		if isMdRule(ts) {
			n := width
			if n > 24 {
				n = 24
			}
			out = append(out, mdQuoteS.Render(strings.Repeat("─", n)))
			continue
		}
		if h := mdHeadingRe.FindStringSubmatch(ts); h != nil {
			st := mdHeadNS
			if len(h[1]) == 1 {
				st = mdHead1S
			}
			out = append(out, wrapRuns(inlineRuns(h[2], st), width)...)
			continue
		}
		if strings.HasPrefix(ts, ">") {
			inner := strings.TrimSpace(strings.TrimPrefix(ts, ">"))
			bar := mdQuoteS.Render("│") + " "
			for _, wl := range wrapRuns(inlineRuns(inner, mdItalicS), width-2) {
				out = append(out, bar+wl)
			}
			continue
		}
		if m := mdBulletRe.FindStringSubmatch(line); m != nil {
			out = append(out, mdListLines(len(m[1]), mdBulletS.Render("•"), m[2], width)...)
			continue
		}
		if m := mdNumRe.FindStringSubmatch(line); m != nil {
			out = append(out, mdListLines(len(m[1]), mdBulletS.Render(m[2]+"."), m[3], width)...)
			continue
		}
		out = append(out, wrapRuns(inlineRuns(line, lipgloss.NewStyle()), width)...)
	}
	return out
}

// mdListLines renders one list item: a colored marker with a hanging indent so
// wrapped continuation lines align under the text, not the marker.
func mdListLines(indent int, marker, content string, width int) []string {
	mw := lipgloss.Width(marker)
	// Cap the indent so the marker + a minimum content area always fit the width
	// (prevents deeply-nested items from overflowing at the 24-col minimum).
	if max := width - mw - 5; indent > max {
		if max < 0 {
			max = 0
		}
		indent = max
	}
	pad := strings.Repeat(" ", indent)
	avail := width - indent - mw - 1
	wls := wrapRuns(inlineRuns(content, lipgloss.NewStyle()), avail)
	hang := strings.Repeat(" ", mw+1)
	out := make([]string, 0, len(wls))
	for i, wl := range wls {
		if i == 0 {
			out = append(out, pad+marker+" "+wl)
		} else {
			out = append(out, pad+hang+wl)
		}
	}
	return out
}

// isMdRule reports whether a trimmed line is a thematic break (---, ***, ___).
func isMdRule(ts string) bool {
	if len(ts) < 3 {
		return false
	}
	c := ts[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(ts); i++ {
		if ts[i] != c {
			return false
		}
	}
	return true
}

// mdRenderIndented renders markdown and prefixes every line with pad (for panels).
func mdRenderIndented(src string, width int, pad string) []string {
	lines := renderMarkdown(src, width)
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return lines
}
