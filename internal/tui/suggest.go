package tui

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
)

// ansiStrip removes SGR escape sequences so background text can be measured as plain text.
var ansiStrip = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// commandItem pairs a slash command name with a concise description.
type commandItem struct {
	cmd  string
	desc string
}

// commandList is the source for the "/" suggestion popup.
var commandList = []commandItem{
	{"/connect", "connect an LLM provider (UI)"},
	{"/search", "search tools & skills (BM25)"},
	{"/diff", "Myers diff demo"},
	{"/agents", "primary agent + lazy subagents"},
	{"/mcp", "MCP server status"},
	{"/usage", "usage & context window"},
	{"/memory", "session memory plan"},
	{"/tools", "list indexed tools & skills"},
	{"/theme", "open theme picker"},
	{"/queue", "manage prompt queue (or ctrl+q)"},
	{"/clear", "start fresh conversation"},
	{"/help", "show help text"},
	{"/quit", "quit brocode"},
}

// suggestFiltered returns the commands starting with prefix. A prefix of "/"
// lists everything; a partial command narrows the list.
func suggestFiltered(prefix string) []commandItem {
	var out []commandItem
	for _, c := range commandList {
		if strings.HasPrefix(c.cmd, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// suggestIndent indents the popup so it doesn't hug the left edge. A
// package-level style — never built per frame (anti-lag rule 5).
var suggestIndent = lipgloss.NewStyle().PaddingLeft(2)

// suggestVisible reports whether the suggestion popup should render: only
// while the input starts with "/", not while the agent is busy, and not
// after the user dismissed it with esc (until they type again).
func (m Model) suggestVisible() bool {
	if m.suggestDismissed || m.agentWorking || m.streaming {
		return false
	}
	v := m.input.Value()
	return strings.HasPrefix(v, "/") && len(suggestFiltered(v)) > 0
}

// renderSuggest draws the command popup as a clean box above the input bar.
// Each row shows the command name plus a dimmed, truncated description on the right.
func (m Model) renderSuggest() string {
	items := suggestFiltered(m.input.Value())
	if len(items) == 0 {
		return ""
	}

	// Calculate maximum width for layout alignment
	maxW := m.width - 8
	if maxW < 35 {
		maxW = 35
	}
	boxW := min(52, maxW)

	lines := make([]string, 0, len(items))
	for i, it := range items {
		// Available width for description inside popup box
		cmdStr := it.cmd
		descStr := clip(it.desc, boxW-len(cmdStr)-6)

		if i == m.suggestSel {
			left := m.styles.sideSel.Render("❯ " + cmdStr)
			right := m.styles.sideSel.Render(" " + descStr)
			gap := boxW - lipgloss.Width(left) - lipgloss.Width(right) - 4
			if gap < 1 {
				gap = 1
			}
			row := left + strings.Repeat(" ", gap) + right
			lines = append(lines, m.styles.sideSel.Render(" "+row+" "))
		} else {
			left := m.styles.prompt.Render("  " + cmdStr)
			right := m.styles.sys.Render(descStr)
			gap := boxW - lipgloss.Width(left) - lipgloss.Width(right) - 4
			if gap < 1 {
				gap = 1
			}
			lines = append(lines, left+strings.Repeat(" ", gap)+right)
		}
	}
	box := m.styles.connectBox.Width(boxW).Render(strings.Join(lines, "\n"))
	return suggestIndent.Render(box)
}
