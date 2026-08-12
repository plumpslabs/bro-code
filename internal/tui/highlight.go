package tui

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
)

var (
	kwColor     = lipgloss.Color("#FF79C6") // Pink
	strColor    = lipgloss.Color("#F1FA8C") // Yellow
	commentColor= lipgloss.Color("#6272A4") // Grey/Blue
	typeColor   = lipgloss.Color("#8BE9FD") // Cyan
	funcColor   = lipgloss.Color("#50FA7B") // Green

	rxKeyword = regexp.MustCompile(`\b(func|package|import|type|struct|interface|return|if|else|for|range|switch|case|default|break|continue|go|defer|var|const|map|chan|nil|true|false)\b`)
	rxType    = regexp.MustCompile(`\b(string|int|int64|bool|byte|rune|error)\b`)
	rxFunc    = regexp.MustCompile(`\b([a-zA-Z_][a-zA-Z0-9_]*)\s*\(`)
)

func highlightCodeLine(line string) string {
	// Simple string parsing (rudimentary)
	if strings.HasPrefix(strings.TrimSpace(line), "//") {
		return lipgloss.NewStyle().Foreground(commentColor).Render(line)
	}

	// This is very rudimentary and will break on complex lines, but it's zero-bloat!
	out := line
	out = rxKeyword.ReplaceAllStringFunc(out, func(m string) string {
		return lipgloss.NewStyle().Foreground(kwColor).Render(m)
	})
	out = rxType.ReplaceAllStringFunc(out, func(m string) string {
		return lipgloss.NewStyle().Foreground(typeColor).Render(m)
	})
	// Fix overlapping ansi with func
	out = rxFunc.ReplaceAllStringFunc(out, func(m string) string {
		name := strings.TrimRight(m, " (")
		return lipgloss.NewStyle().Foreground(funcColor).Render(name) + "("
	})

	return out
}
