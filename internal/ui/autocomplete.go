package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
)

// AutocompleteKind distinguishes between slash commands and file mentions.
type AutocompleteKind int

const (
	AutoKindNone AutocompleteKind = iota
	AutoKindSlash
	AutoKindFile
)

// AutocompleteItem represents a single selectable completion candidate.
type AutocompleteItem struct {
	Value string // text to insert (e.g. "/memory" or "internal/ui/app.go")
	Label string // display name
	Desc  string // short explanation
}

// AutocompleteState manages the active suggestion popup.
type AutocompleteState struct {
	Active   bool
	Kind     AutocompleteKind
	Items    []AutocompleteItem
	Selected int
	Query    string
}

// BuiltinSlashCommands lists all known BroCode slash commands with descriptions.
var BuiltinSlashCommands = []AutocompleteItem{
	{Value: "/ask", Label: "/ask", Desc: "Isolated QA: Ask codebase questions without context pollution"},
	{Value: "/spec", Label: "/spec", Desc: "Spec-First Gate: Draft an architectural blueprint contract"},
	{Value: "/tournament", Label: "/tournament", Desc: "Multi-Candidate: Run 2 parallel candidate agents to solve tasks"},
	{Value: "/help", Label: "/help", Desc: "Help commands & keyboard shortcuts"},
	{Value: "/plan", Label: "/plan", Desc: "View or archive active plan (/plan, /plan archive)"},
	{Value: "/undo", Label: "/undo", Desc: "Time-Travel Rollback: Revert file changes from last turn"},
	{Value: "/report", Label: "/report", Desc: "View or export privacy-safe benchmark/activity report"},
	{Value: "/memory", Label: "/memory", Desc: "View learned project knowledge & gotchas"},
	{Value: "/sessions", Label: "/sessions", Desc: "Switch / manage session history"},
	{Value: "/models", Label: "/models", Desc: "Select active AI model"},
	{Value: "/connect", Label: "/connect", Desc: "Configure providers & API keys"},
	{Value: "/cost", Label: "/cost", Desc: "Token statistics & estimated spend (USD & IDR)"},
	{Value: "/lsp", Label: "/lsp", Desc: "Language Server Protocol status"},
	{Value: "/diagnose", Label: "/diagnose", Desc: "Run autonomous codebase diagnostics (/diagnose, /diagnose fix)"},
	{Value: "/lsp-install", Label: "/lsp-install", Desc: "LSP binary installation guide"},
	{Value: "/builder", Label: "/builder", Desc: "BUILDER mode: autonomous coding & file editing"},
	{Value: "/planner", Label: "/planner", Desc: "PLANNER mode: read-only architecture analysis"},
	{Value: "/miner", Label: "/miner", Desc: "MINER mode: deep codebase exploration & memory persistence"},
	{Value: "/mode", Label: "/mode", Desc: "Switch engine mode (/mode builder|planner|miner)"},
	{Value: "/workspace", Label: "/workspace", Desc: "Manage multi-repo workspace & repos"},
	{Value: "/clear", Label: "/clear", Desc: "Clear chat history screen"},
	{Value: "/new", Label: "/new", Desc: "Start a new conversation session"},
	{Value: "/search-key", Label: "/search-key", Desc: "Configure web search API key (Tavily/Exa) for documentation research"},
	{Value: "/context7-key", Label: "/context7-key", Desc: "Configure Context7 API key for native official library documentation"},
	{Value: "/diff", Label: "/diff", Desc: "Side-by-side visual diff viewer for modified files"},
	{Value: "/repair", Label: "/repair", Desc: "Autonomous pipeline repair doctor for build/test failures"},
	{Value: "/worktree", Label: "/worktree", Desc: "Git worktree sandboxing for isolated agent experiments"},
	{Value: "/update", Label: "/update", Desc: "Check and perform autonomous in-place self-update"},
	{Value: "/debug-context", Label: "/debug-context", Desc: "Dump active context tokens"},
}

// DetectAutocomplete inspects the current prompt input text and cursor position
// to determine if slash command or file mention completion should activate.
// It preserves previous selection index if the query has not changed.
func DetectAutocomplete(input string, allFiles []string, prev AutocompleteState) AutocompleteState {
	text := input
	if text == "" {
		return AutocompleteState{}
	}

	// 1. Slash command detection (input starts with '/' and has no whitespace yet)
	if strings.HasPrefix(text, "/") && !strings.ContainsAny(text, " \t\n") {
		query := strings.ToLower(text)
		var matches []AutocompleteItem
		for _, cmd := range BuiltinSlashCommands {
			if strings.HasPrefix(strings.ToLower(cmd.Value), query) || strings.Contains(strings.ToLower(cmd.Value), query[1:]) {
				matches = append(matches, cmd)
			}
		}
		if len(matches) > 0 {
			sel := 0
			if prev.Active && prev.Kind == AutoKindSlash && prev.Query == text {
				sel = prev.Selected
				if sel >= len(matches) {
					sel = len(matches) - 1
				}
				if sel < 0 {
					sel = 0
				}
			}
			return AutocompleteState{
				Active:   true,
				Kind:     AutoKindSlash,
				Items:    matches,
				Selected: sel,
				Query:    text,
			}
		}
		return AutocompleteState{}
	}

	// 2. File mention detection (find last '@' token in current line)
	lastLine := text
	if idx := strings.LastIndex(text, "\n"); idx >= 0 {
		lastLine = text[idx+1:]
	}

	if atIdx := strings.LastIndex(lastLine, "@"); atIdx >= 0 {
		// Ensure there is no space between @ and cursor
		query := lastLine[atIdx+1:]
		if !strings.ContainsAny(query, " \t") {
			lQuery := strings.ToLower(query)
			var matches []AutocompleteItem
			for _, f := range allFiles {
				base := filepath.Base(f)
				if lQuery == "" || strings.Contains(strings.ToLower(f), lQuery) || strings.Contains(strings.ToLower(base), lQuery) {
					matches = append(matches, AutocompleteItem{
						Value: f,
						Label: "@" + base,
						Desc:  f,
					})
					if len(matches) >= 30 {
						break
					}
				}
			}
			if len(matches) > 0 {
				sel := 0
				if prev.Active && prev.Kind == AutoKindFile && prev.Query == query {
					sel = prev.Selected
					if sel >= len(matches) {
						sel = len(matches) - 1
					}
					if sel < 0 {
						sel = 0
					}
				}
				return AutocompleteState{
					Active:   true,
					Kind:     AutoKindFile,
					Items:    matches,
					Selected: sel,
					Query:    query,
				}
			}
		}
	}

	return AutocompleteState{}
}

// ApplyAutocomplete updates input text by replacing the active query with the selected item.
func ApplyAutocomplete(input string, state AutocompleteState) string {
	if !state.Active || state.Selected < 0 || state.Selected >= len(state.Items) {
		return input
	}
	item := state.Items[state.Selected]

	if state.Kind == AutoKindSlash {
		return item.Value + " "
	}

	if state.Kind == AutoKindFile {
		atIdx := strings.LastIndex(input, "@")
		if atIdx >= 0 {
			return input[:atIdx] + "@" + item.Value + " "
		}
		return input + " @" + item.Value + " "
	}

	return input
}

// RenderAutocomplete renders a compact floating suggestion box with sliding window scrolling.
func RenderAutocomplete(state AutocompleteState, width int) string {
	if !state.Active || len(state.Items) == 0 {
		return ""
	}

	boxWidth := width - 4
	if boxWidth > 68 {
		boxWidth = 68
	}
	if boxWidth < 30 {
		boxWidth = 30
	}

	var rows []string
	header := "Suggestions (Tab / Enter to select, Esc to close):"
	if state.Kind == AutoKindSlash {
		header = fmt.Sprintf("⚡ Slash Commands (%d/%d):", state.Selected+1, len(state.Items))
	} else if state.Kind == AutoKindFile {
		header = fmt.Sprintf("📂 File Mention (%d/%d):", state.Selected+1, len(state.Items))
	}
	rows = append(rows, lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Italic(true).Render(header))

	const maxVisible = 6
	startIdx := 0
	endIdx := len(state.Items)

	if len(state.Items) > maxVisible {
		startIdx = state.Selected - (maxVisible / 2)
		if startIdx < 0 {
			startIdx = 0
		}
		endIdx = startIdx + maxVisible
		if endIdx > len(state.Items) {
			endIdx = len(state.Items)
			startIdx = endIdx - maxVisible
			if startIdx < 0 {
				startIdx = 0
			}
		}
	}

	if startIdx > 0 {
		rows = append(rows, lipgloss.NewStyle().Foreground(lipgloss.Color("#555555")).Render(fmt.Sprintf("  ▲ ... (%d above)", startIdx)))
	}

	for i := startIdx; i < endIdx; i++ {
		item := state.Items[i]
		isSel := i == state.Selected

		prefix := "  "
		labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#CCCCCC"))
		descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#777777"))

		if isSel {
			prefix = "▸ "
			labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FFFF")).Bold(true)
			descStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#EEEEEE")).Bold(true)
		}

		label := item.Label
		if len(label) > 24 {
			label = label[:21] + "..."
		}
		desc := item.Desc
		if len(desc) > 34 {
			desc = desc[:31] + "..."
		}

		line := fmt.Sprintf("%s%-24s %s", prefix, labelStyle.Render(label), descStyle.Render(desc))
		rows = append(rows, line)
	}

	if endIdx < len(state.Items) {
		rows = append(rows, lipgloss.NewStyle().Foreground(lipgloss.Color("#555555")).Render(fmt.Sprintf("  ▼ ... (%d below)", len(state.Items)-endIdx)))
	}

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#00AAAA")).
		Padding(0, 1).
		Width(boxWidth)

	return boxStyle.Render(strings.Join(rows, "\n"))
}
