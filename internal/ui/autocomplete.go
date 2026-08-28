package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/plumpslabs/bro-code/internal/agent"
	"github.com/plumpslabs/bro-code/internal/skill"
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
	{Value: "/repair", Label: "/repair", Desc: "Autonomous pipeline repair doctor for build/test failures"},
	{Value: "/help", Label: "/help", Desc: "Help commands & keyboard shortcuts"},
	{Value: "/plan", Label: "/plan", Desc: "View or archive active plan (/plan, /plan archive)"},
	{Value: "/undo", Label: "/undo", Desc: "Time-Travel Rollback: Revert file changes from last turn"},
	{Value: "/diff", Label: "/diff", Desc: "Side-by-side visual diff viewer for modified files"},
	{Value: "/diagnose", Label: "/diagnose", Desc: "Run autonomous codebase diagnostics (/diagnose, /diagnose fix)"},
	{Value: "/trace", Label: "/trace", Desc: "Show provenance trace for last turn's output"},
	{Value: "/cost", Label: "/cost", Desc: "Token statistics & estimated spend (USD & IDR)"},
	{Value: "/report", Label: "/report", Desc: "View or export privacy-safe benchmark/activity report"},
	{Value: "/memory", Label: "/memory", Desc: "View learned project knowledge & gotchas"},
	{Value: "/sessions", Label: "/sessions", Desc: "Switch / manage session history (/sessions, /history)"},
	{Value: "/models", Label: "/models", Desc: "Select active AI model interactively"},
	{Value: "/model", Label: "/model", Desc: "Switch model directly (/model <name>)"},
	{Value: "/connect", Label: "/connect", Desc: "Configure providers & API keys"},
	{Value: "/copy", Label: "/copy", Desc: "Copy last assistant response directly to OS clipboard"},
	{Value: "/mouse", Label: "/mouse", Desc: "Toggle mouse mode: SELECT (drag copy) ↔ SCROLL (wheel scrolling)"},
	{Value: "/builder", Label: "/builder", Desc: "BUILDER mode: autonomous coding & file editing"},
	{Value: "/planner", Label: "/planner", Desc: "PLANNER mode: read-only architecture analysis"},
	{Value: "/miner", Label: "/miner", Desc: "MINER mode: deep codebase exploration & memory persistence"},
	{Value: "/mode", Label: "/mode", Desc: "Switch engine mode (/mode builder|planner|miner)"},
	{Value: "/lsp", Label: "/lsp", Desc: "Language Server Protocol status"},
	{Value: "/lsp-install", Label: "/lsp-install", Desc: "LSP binary installation guide"},
	{Value: "/mcp", Label: "/mcp", Desc: "Show MCP server status & registered tools"},
	{Value: "/mcp-reload", Label: "/mcp-reload", Desc: "Hot-reload all MCP server configurations"},
	{Value: "/workspace", Label: "/workspace", Desc: "Manage multi-repo workspace & repos (/workspace, /repos)"},
	{Value: "/worktree", Label: "/worktree", Desc: "Git worktree sandboxing for isolated agent experiments"},
	{Value: "/search-key", Label: "/search-key", Desc: "Configure web search API key (Tavily/Exa)"},
	{Value: "/context7-key", Label: "/context7-key", Desc: "Configure Context7 API key for library docs"},
	{Value: "/clear", Label: "/clear", Desc: "Clear chat history screen"},
	{Value: "/new", Label: "/new", Desc: "Start a new conversation session"},
	{Value: "/update", Label: "/update", Desc: "Check and perform autonomous in-place self-update"},
	{Value: "/debug-context", Label: "/debug-context", Desc: "Dump active context tokens"},
	{Value: "/agents", Label: "/agents", Desc: "List all detected custom subagents"},
}

// DetectAutocomplete inspects the current prompt input text and cursor position
// to determine if slash command, custom skill, agent mention, or file mention completion should activate.
// It preserves previous selection index if the query has not changed.
func DetectAutocomplete(input string, allFiles []string, customAgents []agent.CustomAgent, customSkills []skill.Skill, prev AutocompleteState) AutocompleteState {
	text := input
	if text == "" {
		return AutocompleteState{}
	}

	// 1. Slash command & Custom Skill detection (input starts with '/' and has no whitespace yet)
	if strings.HasPrefix(text, "/") && !strings.ContainsAny(text, " \t\n") {
		query := strings.ToLower(text)
		var matches []AutocompleteItem

		// Priority A: Custom Skills (/skill-name)
		for _, sk := range customSkills {
			val := "/" + sk.Name
			if strings.HasPrefix(strings.ToLower(val), query) || strings.Contains(strings.ToLower(sk.Name), query[1:]) || strings.Contains(strings.ToLower(sk.Description), query[1:]) {
				desc := sk.Description
				if desc == "" {
					desc = "Custom Workflow Skill"
				}
				matches = append(matches, AutocompleteItem{
					Value: val,
					Label: val,
					Desc:  "✨ " + desc,
				})
			}
		}

		// Priority B: Builtin Slash Commands (exact prefix matches first, then substring matches)
		var prefixMatches []AutocompleteItem
		var subMatches []AutocompleteItem
		for _, cmd := range BuiltinSlashCommands {
			cmdVal := strings.ToLower(cmd.Value)
			if strings.HasPrefix(cmdVal, query) {
				prefixMatches = append(prefixMatches, cmd)
			} else if strings.Contains(cmdVal, query[1:]) || strings.Contains(strings.ToLower(cmd.Desc), query[1:]) {
				subMatches = append(subMatches, cmd)
			}
		}
		matches = append(matches, prefixMatches...)
		matches = append(matches, subMatches...)

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

	// 2. Mention detection (agents and files via '@')
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

			// Priority A: Custom Subagents (@agentName)
			for _, ag := range customAgents {
				agName := ag.Name
				if agName == "" {
					continue
				}
				if lQuery == "" || strings.Contains(strings.ToLower(agName), lQuery) || strings.Contains(strings.ToLower(ag.Description), lQuery) {
					desc := ag.Description
					if desc == "" {
						desc = fmt.Sprintf("Custom %s Agent", ag.Mode)
					}
					matches = append(matches, AutocompleteItem{
						Value: agName,
						Label: "@" + agName,
						Desc:  desc,
					})
				}
			}

			// Priority B: Project Files (@filename)
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
		header = fmt.Sprintf("📂 Mentions — Files & Agents (%d/%d):", state.Selected+1, len(state.Items))
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
