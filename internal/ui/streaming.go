package ui

import (
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
)

// mdRenderers caches a glamour renderer per wrap width so the markdown line
// length follows the terminal width instead of a fixed 90 columns (which left
// most of a wide terminal unused and broke lines too early).
var mdRenderers = struct {
	sync.Mutex
	m map[int]*glamour.TermRenderer
}{m: map[int]*glamour.TermRenderer{}}

func renderMarkdown(text string, wrap int) string {
	if wrap < 30 {
		wrap = 30
	}

	mdRenderers.Lock()
	r, ok := mdRenderers.m[wrap]
	if !ok {
		// Cap cached renderers to 4 recent widths to prevent memory leaks on continuous terminal resizes
		if len(mdRenderers.m) >= 4 {
			for k := range mdRenderers.m {
				delete(mdRenderers.m, k)
				break
			}
		}
		r, _ = glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(wrap),
			glamour.WithPreservedNewLines(),
		)
		if r != nil {
			mdRenderers.m[wrap] = r
		}
	}
	mdRenderers.Unlock()

	if r == nil {
		return text
	}
	out, err := r.Render(text)
	if err != nil || strings.TrimSpace(out) == "" {
		return text
	}
	res := strings.TrimSpace(out)

	// Glamour pads every rendered line with trailing spaces so its word wrap
	// is stable — but inside the border box those pad the lines to full width
	// and make tables/paragraphs look ragged ("acak-acakan"). Strip per-line
	// trailing whitespace; the box border is the right edge.
	res = stripTrailingWS(res)
	res = formatTableOutput(res, wrap)

	// Clean up any remaining unparsed **text** into bold lipgloss styling
	if strings.Contains(res, "**") {
		boldStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
		parts := strings.Split(res, "**")
		var sb strings.Builder
		for i, p := range parts {
			if i%2 == 1 && p != "" {
				sb.WriteString(boldStyle.Render(p))
			} else {
				sb.WriteString(p)
			}
		}
		res = sb.String()
	}

	return res
}

// completeStreamingMarkdown scans raw streaming markdown and appends virtual
// closing delimiters (code blocks, bold, inline code) so terminal markdown
// renderers (Glamour/Goldmark/Chroma) can parse and syntax-highlight in-flight
// responses in real-time without broken ASTs or styling bleed.
func completeStreamingMarkdown(text string) string {
	if text == "" {
		return ""
	}

	lines := strings.Split(text, "\n")
	inCodeBlock := false
	var fence string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock && strings.HasPrefix(trimmed, fence) {
				inCodeBlock = false
				fence = ""
			} else if !inCodeBlock {
				inCodeBlock = true
				fence = "```"
			}
		} else if strings.HasPrefix(trimmed, "~~~") {
			if inCodeBlock && strings.HasPrefix(trimmed, fence) {
				inCodeBlock = false
				fence = ""
			} else if !inCodeBlock {
				inCodeBlock = true
				fence = "~~~"
			}
		}
	}

	var virtualSuffix strings.Builder
	if inCodeBlock {
		if !strings.HasSuffix(text, "\n") {
			virtualSuffix.WriteString("\n")
		}
		virtualSuffix.WriteString("```\n")
		return text + virtualSuffix.String()
	}

	backtickCount := 0
	starCount := 0
	inInlineCode := false

	for i := 0; i < len(text); i++ {
		if text[i] == '`' {
			backtickCount++
			inInlineCode = !inInlineCode
		} else if !inInlineCode && text[i] == '*' {
			if i+1 < len(text) && text[i+1] == '*' {
				starCount += 2
				i++
			}
		}
	}

	if backtickCount%2 != 0 {
		virtualSuffix.WriteString("`")
	} else if starCount%4 != 0 {
		virtualSuffix.WriteString("**")
	}

	if virtualSuffix.Len() > 0 {
		return text + virtualSuffix.String()
	}
	return text
}

// renderStreamingMarkdown formats in-flight markdown streams with virtual AST
// completion so headings, bolding, and code syntax highlighting are visible live.
func renderStreamingMarkdown(text string, wrap int) string {
	if text == "" {
		return ""
	}
	completed := completeStreamingMarkdown(text)
	return renderMarkdown(completed, wrap)
}

// getFormattedStream returns memoized, live-formatted streaming markdown
// with virtual AST completion. Cached per string content and wrap width
// to guarantee 0% idle CPU overhead.
func (m *Model) getFormattedStream(wrap int) string {
	if m.pendingStream == "" {
		return ""
	}
	if m.streamRenderRaw == m.pendingStream && m.streamRenderWrap == wrap && m.streamRenderCached != "" {
		return m.streamRenderCached
	}
	m.streamRenderRaw = m.pendingStream
	m.streamRenderWrap = wrap
	m.streamRenderCached = renderStreamingMarkdown(m.pendingStream, wrap)
	return m.streamRenderCached
}

// stripTrailingWS removes trailing spaces/tabs from each line (glamour pads
// lines; the border box must be the visual right edge, not invisible spaces).
func stripTrailingWS(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// formatTableOutput cleans up Glamour table rendering by clamping horizontal
// divider lines to the terminal width and merging orphaned table cell wraps.
func formatTableOutput(text string, wrap int) string {
	if !strings.Contains(text, "│") && !strings.Contains(text, "┼") && !strings.Contains(text, "─") {
		return text
	}
	lines := strings.Split(text, "\n")
	var cleaned []string
	inTable := false

	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		cleanText := ansiRegex.ReplaceAllString(trimmed, "")
		cleanTrimmed := strings.TrimSpace(cleanText)

		// Detect table state
		if strings.Contains(trimmed, "│") {
			inTable = true
		} else if cleanTrimmed == "" {
			inTable = false
		}

		// Clamp horizontal divider lines to terminal wrap width
		if strings.Contains(trimmed, "─") && (strings.Contains(trimmed, "┼") || strings.Contains(trimmed, "│")) {
			runes := []rune(trimmed)
			if wrap > 10 && len(runes) > wrap {
				trimmed = string(runes[:wrap])
			}
		}

		// Merge orphaned table cell wrap lines (lines inside a table that lack column separators)
		if inTable && !strings.Contains(trimmed, "│") && !strings.Contains(trimmed, "─") && cleanTrimmed != "" {
			if len(cleaned) > 0 {
				cleaned[len(cleaned)-1] = cleaned[len(cleaned)-1] + " " + cleanTrimmed
				continue
			}
		}

		cleaned = append(cleaned, trimmed)
	}
	return strings.Join(cleaned, "\n")
}

func formatDiffLines(text string) string {
	lines := strings.Split(text, "\n")
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	var sb strings.Builder
	for i, line := range lines {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			sb.WriteString(greenStyle.Render(line))
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			sb.WriteString(redStyle.Render(line))
		} else if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			sb.WriteString(dimStyle.Render(line))
		} else {
			sb.WriteString(line)
		}
		if i < len(lines)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// diffStat counts added/removed lines in a unified diff (ignoring the
// +++/--- file headers and @@ hunk markers) so a collapsed DIFF entry can
// show a compact (+N −M) summary.
func diffStat(diff string) (add, del int) {
	for _, line := range strings.Split(diff, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case '+':
			if strings.HasPrefix(line, "+++") {
				continue
			}
			add++
		case '-':
			if strings.HasPrefix(line, "---") {
				continue
			}
			del++
		}
	}
	return add, del
}
