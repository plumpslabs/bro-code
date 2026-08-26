package ui

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/plumpslabs/bro-code/internal/tool"
	"golang.org/x/term"
)

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
var multiNewlineRegex = regexp.MustCompile(`\n{3,}`)

// isStatusLine reports whether a trimmed line looks like an OpenCode CLI
// status header (spinner frames, "build · model" banners, prompt prefixes)
// that must be stripped from the answer.
func isStatusLine(trimmed string) bool {
	if trimmed == "" || trimmed == "[0m" || trimmed == "[?25l" || trimmed == "[?25h" {
		return true
	}
	if strings.Contains(trimmed, "build ·") || strings.Contains(trimmed, "build •") || strings.Contains(trimmed, "build·") {
		return true
	}
	for _, p := range []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "❯", "→", "├", "│", "┃", "⬢"} {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

func sanitizeLLMOutput(content string) string {
	content = ansiRegex.ReplaceAllString(content, "")

	lines := strings.Split(content, "\n")
	var cleanLines []string
	skippingHeader := true

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if skippingHeader {
			if isStatusLine(trimmed) {
				continue
			}
			skippingHeader = false
		}
		cleanLines = append(cleanLines, line)
	}

	res := strings.TrimSpace(strings.Join(cleanLines, "\n"))
	res = strings.TrimPrefix(res, "[0m")
	res = strings.TrimSuffix(res, "[0m")
	res = multiNewlineRegex.ReplaceAllString(res, "\n\n")
	return strings.TrimSpace(res)
}

func getTerminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 20 {
		return w
	}
	return 80
}

// FormatMessageForTerminal renders a formatted message string for stdout stream printing.
func FormatMessageForTerminal(msg string, width int) string {
	return formatMessage(msg, width, false)
}

func formatMessage(msg string, width int, filesExpanded bool) string {
	if width <= 0 {
		width = getTerminalWidth() - 2
	}

	userLabelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
	userBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("86")).Padding(1, 2)

	botBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("205")).Padding(1, 2)

	thinkingStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Italic(true)

	processLabelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	processBarStyle := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color("240")).Padding(0, 1)

	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	if width > 0 {
		userBarStyle = userBarStyle.Width(width)
		botBarStyle = botBarStyle.Width(width)
		processBarStyle = processBarStyle.Width(width)
		errStyle = errStyle.Width(width)
	}

	if strings.HasPrefix(msg, "YOU:\n") || strings.HasPrefix(msg, "👤 ") {
		content := strings.TrimPrefix(strings.TrimPrefix(msg, "YOU:\n"), "👤 ")
		return userBarStyle.Render(userLabelStyle.Render("YOU") + "\n" + content)
	}

	if strings.HasPrefix(msg, "CMD:") {
		line, content, _ := strings.Cut(msg, "\n")
		cmdName := strings.TrimPrefix(line, "CMD:")

		color := "86"
		switch cmdName {
		case "/spec":
			color = "141"
		case "/tournament":
			color = "220"
		case "/ask":
			color = "39"
		}

		cmdLabelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true)
		cmdBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color(color)).Padding(1, 2)
		if width > 0 {
			cmdBarStyle = cmdBarStyle.Width(width)
		}
		return cmdBarStyle.Render(cmdLabelStyle.Render("YOU ("+cmdName+")") + "\n" + content)
	}

	if strings.HasPrefix(msg, "FILES:\n") && strings.Contains(msg, tool.FileChangesSep) {
		compact, diff, _ := strings.Cut(msg, tool.FileChangesSep)
		compact = strings.TrimPrefix(compact, "FILES:\n")
		fileBarStyle := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
		if width > 0 {
			fileBarStyle = fileBarStyle.Width(width)
		}
		labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("178")).Bold(true)
		var body string
		if filesExpanded {
			body = compact + "\n\n" + formatDiffLines(diff)
		} else {
			body = compact
		}
		return fileBarStyle.Render(labelStyle.Render("FILES") + "\n" + body)
	}

	if strings.HasPrefix(msg, "DIFF:\n") {
		body := strings.TrimPrefix(msg, "DIFF:\n")
		path, diff := body, ""
		if nl := strings.Index(body, "\n"); nl >= 0 {
			path, diff = body[:nl], body[nl+1:]
		}
		// Strip optional "#idx" sequence suffix (used internally as unique key)
		// so the rendered card always shows the clean file path.
		if hi := strings.LastIndex(path, "#"); hi >= 0 {
			if _, err := fmt.Sscanf(path[hi+1:], "%d", new(int)); err == nil {
				path = path[:hi]
			}
		}
		diffBarStyle := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
		if width > 0 {
			diffBarStyle = diffBarStyle.Width(width)
		}
		add, del := diffStat(diff)
		actionLabel := "DIFF"
		labelColor := "178"
		if del == 0 && add > 0 && (!strings.Contains(diff, "@@ -") || strings.Contains(diff, "@@ -0,0 +")) {
			actionLabel = "CREATE"
			labelColor = "42"
		} else if add == 0 && del > 0 {
			actionLabel = "DELETE"
			labelColor = "196"
		}
		labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(labelColor)).Bold(true)
		if filesExpanded {
			return diffBarStyle.Render(labelStyle.Render(actionLabel) + "  " + path + "\n" + formatDiffLines(diff))
		}
		statStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
		return diffBarStyle.Render(labelStyle.Render(actionLabel) + "  " + path + "  " + statStyle.Render(fmt.Sprintf("(+%d −%d) · [press Ctrl+F for diff]", add, del)))
	}

	if strings.HasPrefix(msg, "ASK:\n") {
		body := strings.TrimPrefix(msg, "ASK:\n")
		query, answer, _ := strings.Cut(body, "\n---\n")
		askCardStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("86")).Padding(0, 1)
		if width > 0 {
			askCardStyle = askCardStyle.Width(width)
		}
		labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
		qStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Bold(true)
		dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
		wrap := width - 6
		if wrap < 30 {
			wrap = 30
		}
		renderedAnswer := renderMarkdown(strings.TrimSpace(answer), wrap)
		header := labelStyle.Render("💬 CODEBASE QA") + "  " + dimStyle.Render("(Ephemeral · Zero Context Pollution)")
		qLine := qStyle.Render("❓ \"" + query + "\"")
		return askCardStyle.Render(header + "\n" + qLine + "\n\n" + renderedAnswer)
	}

	if strings.HasPrefix(msg, "PROVENANCE:\n") {
		content := strings.TrimPrefix(msg, "PROVENANCE:\n")
		return renderBorderedCard("🔐 CRYPTOGRAPHIC AI PROVENANCE & ATTESTATION", "(Merkle Proof & Git Notes)", content, "💡 Verify any git commit: `/trace <commit-hash>`", "86", width)
	}

	if strings.HasPrefix(msg, "SPEC:\n") {
		body := strings.TrimPrefix(msg, "SPEC:\n")
		specPath, specContent, _ := strings.Cut(body, "\n---\n")
		return renderBorderedCard("📋 ARCHITECTURAL BLUEPRINT CONTRACT", "("+specPath+")", specContent, "💡 Next: Switch to BUILDER (Shift+Tab) and say 'Implement spec in "+specPath+"'", "141", width)
	}

	if strings.HasPrefix(msg, "TOURNAMENT:\n") {
		body := strings.TrimPrefix(msg, "TOURNAMENT:\n")
		task, content, _ := strings.Cut(body, "\n---\n")
		return renderBorderedCard("🏆 MULTI-CANDIDATE TOURNAMENT", "(\""+truncatePrompt(task)+"\")", content, "", "220", width)
	}

	if strings.HasPrefix(msg, "PLAN:\n") {
		content := strings.TrimPrefix(msg, "PLAN:\n")
		footer := "💡 Next: Switch to BUILDER (Shift+Tab) to execute or type `/plan archive` to clear."
		if strings.Contains(content, "Plan archived successfully!") || strings.Contains(content, "No active plan found") {
			footer = ""
		}
		return renderBorderedCard("📋 EXECUTION PLAN & ROADMAP", "(.brocode/current_plan.md)", content, footer, "81", width)
	}

	if strings.HasPrefix(msg, "HELP:\n") {
		content := strings.TrimPrefix(msg, "HELP:\n")
		return renderBorderedCard("📖 BROCODE CLI CHEATSHEET & SHORTCUTS", "(Commands & Keybindings)", content, "", "214", width)
	}

	if strings.HasPrefix(msg, "MEMORY:\n") {
		content := strings.TrimPrefix(msg, "MEMORY:\n")
		return renderBorderedCard("🧠 PROJECT MEMORY", "(.brocode/memory.md)", content, "", "177", width)
	}

	if strings.HasPrefix(msg, "COST:\n") {
		content := strings.TrimPrefix(msg, "COST:\n")
		return renderBorderedCard("📊 TOKEN ECONOMY & COST RADAR", "(Spend Telemetry)", content, "", "42", width)
	}

	if strings.HasPrefix(msg, "WORKSPACE:\n") {
		content := strings.TrimPrefix(msg, "WORKSPACE:\n")
		return renderBorderedCard("📦 MULTI-REPO WORKSPACE", "(Discovered Repos)", content, "", "208", width)
	}

	if strings.HasPrefix(msg, "LSP:\n") {
		content := strings.TrimPrefix(msg, "LSP:\n")
		return renderBorderedCard("⚡ LANGUAGE SERVER PROTOCOL (LSP)", "(Code Intelligence)", content, "", "39", width)
	}

	if strings.HasPrefix(msg, "DIAGNOSE:\n") {
		content := strings.TrimPrefix(msg, "DIAGNOSE:\n")
		return renderBorderedCard("🩺 CODEBASE DIAGNOSTICS", "(Diagnostics & Warnings)", content, "", "226", width)
	}

	if strings.HasPrefix(msg, "REPORT:\n") {
		content := strings.TrimPrefix(msg, "REPORT:\n")
		return renderBorderedCard("📊 SESSION ACTIVITY & BENCHMARK REPORT", "(/report --json to export)", content, "", "37", width)
	}

	if strings.HasPrefix(msg, "MCP:\n") {
		content := strings.TrimPrefix(msg, "MCP:\n")
		return renderBorderedCard("🔌 MCP SERVERS & TOOLS", "(Model Context Protocol)", content, "", "33", width)
	}

	if strings.HasPrefix(msg, "UNDO:\n") {
		content := strings.TrimPrefix(msg, "UNDO:\n")
		return renderBorderedCard("↩️ TIME-TRAVEL SHADOW ROLLBACK", "(Reverted File Edits)", content, "", "208", width)
	}

	if strings.HasPrefix(msg, "SEARCH:\n") {
		content := strings.TrimPrefix(msg, "SEARCH:\n")
		return renderBorderedCard("🌐 WEB SEARCH ENGINE", "(Research & Documentation)", content, "", "33", width)
	}

	if strings.HasPrefix(msg, "CONTEXT7:\n") {
		content := strings.TrimPrefix(msg, "CONTEXT7:\n")
		return renderBorderedCard("📚 CONTEXT7 & DOCS RESOLVER", "(Native REST API)", content, "", "141", width)
	}

	if strings.HasPrefix(msg, "WORKTREE:\n") {
		content := strings.TrimPrefix(msg, "WORKTREE:\n")
		return renderBorderedCard("🌿 GIT WORKTREE SANDBOX", "(Isolated Agent Workspaces)", content, "", "70", width)
	}

	if strings.HasPrefix(msg, "AGENTS:\n") {
		content := strings.TrimPrefix(msg, "AGENTS:\n")
		return renderBorderedCard("🤖 CUSTOM AGENTS & MODES", "(.brocode/agents/*.md)", content, "", "99", width)
	}

	if strings.HasPrefix(msg, "REPAIR:\n") {
		content := strings.TrimPrefix(msg, "REPAIR:\n")
		return renderBorderedCard("🩺 PIPELINE DOCTOR & SELF-REPAIR", "(Build & Test Fixer)", content, "", "196", width)
	}

	if strings.HasPrefix(msg, "UPDATE:\n") {
		content := strings.TrimPrefix(msg, "UPDATE:\n")
		return renderBorderedCard("🚀 BROCODE AUTO-UPDATER", "(Release Channel)", content, "", "86", width)
	}

	if strings.HasPrefix(msg, "MODE:") {
		line, content, _ := strings.Cut(msg, "\n")
		targetMode := strings.TrimPrefix(line, "MODE:")
		return renderBorderedCard("🔀 MODE ACTIVATED: "+targetMode, "(Shift+Tab to toggle)", content, "", "86", width)
	}

	if strings.HasPrefix(msg, "PROCESS:\n") {
		content := strings.TrimPrefix(msg, "PROCESS:\n")
		formatted := formatDiffLines(content)
		return processBarStyle.Render(processLabelStyle.Render(formatted))
	}

	mode := ""
	model := ""
	var content string
	if strings.HasPrefix(msg, "BROCODE:") {
		rest := strings.TrimPrefix(msg, "BROCODE:")
		if i := strings.Index(rest, "\n"); i >= 0 {
			stamp := rest[:i]
			content = rest[i+1:]
			if s, m, ok := strings.Cut(stamp, ":"); ok {
				mode = s
				model = m
			} else {
				mode = stamp
			}
		} else {
			content = rest
		}
	} else if strings.HasPrefix(msg, "🤖 ") {
		content = strings.TrimPrefix(msg, "🤖 ")
	} else {
		content = ""
	}

	if strings.HasPrefix(msg, "BROCODE:") || strings.HasPrefix(msg, "🤖 ") {
		content = sanitizeLLMOutput(content)

		body := content
		var thinking string
		if strings.HasPrefix(body, "💭 ") {
			if idx := strings.Index(body, "\n\n"); idx >= 0 {
				thinking = body[:idx]
				body = body[idx+2:]
			}
		}

		wrap := width - 6
		if wrap < 30 {
			wrap = 30
		}
		formattedBody := renderMarkdown(body, wrap)
		if strings.Contains(formattedBody, "--- ") || strings.Contains(formattedBody, "+++ ") || strings.Contains(formattedBody, "@@ ") {
			formattedBody = formatDiffLines(formattedBody)
		}

		if mode == "" {
			mode = "BUILDER"
		}
		badgeColor := "42"
		switch mode {
		case "BUILDER":
			badgeColor = "42"
		case "PLANNER":
			badgeColor = "141"
		case "MINER":
			badgeColor = "214"
		default:
			badgeColor = "42"
		}

		label := lipgloss.NewStyle().Foreground(lipgloss.Color(badgeColor)).Bold(true).Render("BROCODE")
		badge := lipgloss.NewStyle().Bold(true).Padding(0, 1).Foreground(lipgloss.Color("0")).Background(lipgloss.Color(badgeColor))
		label += " " + badge.Render(mode)
		if model != "" {
			modelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
			label += " " + modelStyle.Render(model)
		}

		modeBarStyle := botBarStyle.BorderForeground(lipgloss.Color(badgeColor))
		if thinking != "" {
			return modeBarStyle.Render(label + "\n\n" +
				thinkingStyle.Render(thinking) + "\n\n" + formattedBody)
		}
		return modeBarStyle.Render(label + "\n\n" + formattedBody)
	}

	if strings.HasPrefix(msg, "ERROR: ") || strings.HasPrefix(msg, "❌ ") {
		return errStyle.Render(msg)
	}

	if width > 0 {
		return lipgloss.NewStyle().Width(width).Render(msg)
	}
	return msg
}

// renderBorderedCard formats structured cards (plans, specs, help, diagnostics, reports, etc.)
// with a consistent thick left border, title header, optional badge/subtitle, and optional footer.
func renderBorderedCard(title, subtitle, body, footer, colorCode string, width int) string {
	cardStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color(colorCode)).Padding(0, 1)
	if width > 0 {
		cardStyle = cardStyle.Width(width)
	}
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colorCode)).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	wrap := width - 6
	if wrap < 30 {
		wrap = 30
	}
	renderedBody := renderMarkdown(strings.TrimSpace(body), wrap)

	header := labelStyle.Render(title)
	if subtitle != "" {
		header += "  " + dimStyle.Render(subtitle)
	}

	res := header + "\n\n" + renderedBody
	if footer != "" {
		res += "\n\n" + dimStyle.Render(footer)
	}
	return cardStyle.Render(res)
}

// resolveTournamentSelection detects if a prompt is applying a candidate from a
// recent tournament (e.g. "Apply Beta") and enriches the execution prompt with
// that candidate's exact root cause analysis, target files, and proposed patch.
func resolveTournamentSelection(query string, messages []string) string {
	q := strings.TrimSpace(strings.ToLower(query))
	target := ""
	if strings.Contains(q, "apply alpha") || strings.Contains(q, "pilih alpha") || strings.Contains(q, "terapkan alpha") {
		target = "Candidate-Alpha"
	} else if strings.Contains(q, "apply beta") || strings.Contains(q, "pilih beta") || strings.Contains(q, "terapkan beta") {
		target = "Candidate-Beta"
	}
	if target == "" {
		return ""
	}

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if strings.HasPrefix(msg, "TOURNAMENT:\n") {
			body := strings.TrimPrefix(msg, "TOURNAMENT:\n")
			startMarker := "### 🥊 " + target
			startIdx := strings.Index(body, startMarker)
			if startIdx >= 0 {
				candidateSection := body[startIdx:]
				if endIdx := strings.Index(candidateSection, "### ⚖️"); endIdx >= 0 {
					candidateSection = candidateSection[:endIdx]
				} else if nextCand := strings.Index(candidateSection, "### 🥊"); nextCand > 0 {
					candidateSection = candidateSection[:nextCand]
				}
				candidateSection = strings.TrimSpace(candidateSection)
				return fmt.Sprintf(
					"Goal: Execute %s's verified patch and fix from the tournament.\n\n"+
						"Language: Automatically detect and mirror the user's conversation language in your explanations and final summary (e.g. Bahasa Indonesia, English).\n\n"+
						"Verified Analysis & Proposal from %s:\n"+
						"%s\n\n"+
						"Action Required:\n"+
						"1. Locate and inspect the target file(s) and specific line(s) specified in the proposal above.\n"+
						"2. Apply the fix using edit_file (or write_file).\n"+
						"3. Summarize the changes made clearly in the user's language.",
					target, target, candidateSection,
				)
			}
		}
	}
	return ""
}

// extractRecentSessionContext extracts concise context from recent turns so isolated subagents
// (like /ask) understand contextual references (e.g. "masalah tadi", "before after fix ini").
func extractRecentSessionContext(messages []string, maxCount int) string {
	if len(messages) == 0 {
		return ""
	}
	var snippets []string
	count := 0
	for i := len(messages) - 1; i >= 0 && count < maxCount; i-- {
		msg := messages[i]
		if strings.HasPrefix(msg, "YOU:\n") {
			body := strings.TrimPrefix(msg, "YOU:\n")
			snippets = append([]string{"- User: " + truncatePrompt(body)}, snippets...)
			count++
		} else if strings.HasPrefix(msg, "BROCODE:") {
			parts := strings.SplitN(msg, "\n", 2)
			if len(parts) > 1 {
				snippets = append([]string{"- Assistant: " + truncatePrompt(parts[1])}, snippets...)
				count++
			}
		} else if strings.HasPrefix(msg, "TOURNAMENT:\n") {
			parts := strings.SplitN(msg, "\n", 2)
			if len(parts) > 1 {
				snippets = append([]string{"- Tournament Task: " + truncatePrompt(parts[1])}, snippets...)
				count++
			}
		} else if strings.HasPrefix(msg, "CMD:/") {
			snippets = append([]string{"- Command: " + truncatePrompt(msg)}, snippets...)
			count++
		}
	}
	if len(snippets) == 0 {
		return ""
	}
	return "Active Conversation Context (for resolving references like 'itu', 'masalah tadi', or recent fixes):\n" + strings.Join(snippets, "\n") + "\n\n"
}
