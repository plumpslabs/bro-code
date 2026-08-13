package tui

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
)

var (
	kwColor      = lipgloss.Color("#FF79C6") // Pink
	strColor     = lipgloss.Color("#F1FA8C") // Yellow
	commentColor = lipgloss.Color("#6272A4") // Grey/Blue
	typeColor    = lipgloss.Color("#8BE9FD") // Cyan
	funcColor    = lipgloss.Color("#50FA7B") // Green

	// Precompiled styles — highlightCodeLine runs on every code line of every
	// render, and lipgloss.NewStyle() per match per line (thousands of
	// allocations per streaming tick) measurably dragged the event loop.
	styleComment = lipgloss.NewStyle().Foreground(commentColor)
	styleKw      = lipgloss.NewStyle().Foreground(kwColor)
	styleType    = lipgloss.NewStyle().Foreground(typeColor)
	styleFunc    = lipgloss.NewStyle().Foreground(funcColor)

	rxKeyword = regexp.MustCompile(`\b(func|package|import|type|struct|interface|return|if|else|for|range|switch|case|default|break|continue|go|defer|var|const|map|chan|nil|true|false)\b`)
	rxType    = regexp.MustCompile(`\b(string|int|int64|bool|byte|rune|error)\b`)
	rxFunc    = regexp.MustCompile(`\b([a-zA-Z_][a-zA-Z0-9_]*)\s*\(`)
)

func highlightCodeLine(line string) string {
	// Simple string parsing (rudimentary)
	if strings.HasPrefix(strings.TrimSpace(line), "//") {
		return styleComment.Render(line)
	}

	// This is very rudimentary and will break on complex lines, but it's zero-bloat!
	out := line
	out = rxKeyword.ReplaceAllStringFunc(out, func(m string) string {
		return styleKw.Render(m)
	})
	out = rxType.ReplaceAllStringFunc(out, func(m string) string {
		return styleType.Render(m)
	})
	// Fix overlapping ansi with func
	out = rxFunc.ReplaceAllStringFunc(out, func(m string) string {
		name := strings.TrimRight(m, " (")
		return styleFunc.Render(name) + "("
	})

	return out
}
