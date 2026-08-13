package tui

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// commandItem pairs a slash command name with a concise description.
type commandItem struct {
	cmd  string
	desc string
}

// commandList is the source for the "/" suggestion popup.
var commandList = []commandItem{
	{"/planner", "switch to PLANNER mode (strict no-edit plan/brainstorm)"},
	{"/builder", "switch to BUILDER mode (real-time execution & edits)"},
	{"/connect", "connect an LLM provider (UI)"},
	{"/models", "select active AI model"},
	{"/search", "search tools & skills (BM25)"},
	{"/diff", "Myers diff demo"},
	{"/agents", "primary agent + lazy subagents"},
	{"/info", "session info & status"},
	{"/mcp", "MCP server status"},
	{"/usage", "usage & context window"},
	{"/compact", "compact context window now"},
	{"/memory", "session memory plan"},
	{"/tools", "list indexed tools & skills"},
	{"/theme", "open theme picker"},
	{"/queue", "manage prompt queue (or ctrl+q)"},
	{"/history", "list saved sessions"},
	{"/clear", "start fresh conversation"},
	{"/help", "show help text"},
	{"/quit", "quit brocode"},
}

// subagentList is the source for "@" subagent mentions. It is EMPTY by
// design: brocode runs a single agent loop, so built-in "@matcha-*" roles
// would be pure decoration (they never spawned a real agent, and their
// "delegated to model X" traces were fictional). Only REAL custom agents
// discovered from .brocode/agents/<name>.md (project) and
// ~/.brocode/agents/<name>.md (global) appear here.
var subagentList = []commandItem{}

// suggestFiltered returns the items starting with prefix (slash commands or @ subagents).
func suggestFiltered(input string) []commandItem {
	var out []commandItem
	if strings.HasPrefix(input, "/") {
		for _, c := range commandList {
			if strings.HasPrefix(c.cmd, input) {
				out = append(out, c)
			}
		}
		return out
	}

	// Subagent suggestions ONLY trigger when the current word explicitly starts with '@'
	words := strings.Fields(input)
	lastWord := input
	if len(words) > 0 {
		lastWord = words[len(words)-1]
	}
	if !strings.HasPrefix(lastWord, "@") {
		return nil
	}

	cleanWord := strings.ToLower(strings.TrimPrefix(lastWord, "@"))
	activeSubagents := loadDiscoveredSubagents()
	for _, sa := range activeSubagents {
		saName := strings.ToLower(strings.TrimPrefix(sa.cmd, "@"))
		saBare := strings.TrimPrefix(saName, "matcha-")
		if cleanWord == "" || strings.HasPrefix(saName, cleanWord) || strings.HasPrefix(saBare, cleanWord) {
			out = append(out, sa)
		}
	}
	return out
}

// loadDiscoveredSubagents scans for custom agent Markdown files.
// Convention (matching Claude Code / OpenCode):
//   - Project-level: .brocode/agents/<name>.md
//   - Global-level:  ~/.brocode/agents/<name>.md
//
// NOTE: .agents/ is for plans/skills/reports — NOT subagents.
//
// The result is cached with a short TTL: this is called from
// suggestVisible/renderSuggest on EVERY keystroke while the popup is up, and a
// full disk walk + per-file read per keypress was pure churn. Agent definition
// files change rarely — a 10s TTL is invisible in practice (a newly added
// agent shows up within 10s of typing @) and turns per-keypress I/O into one
// walk per 10 seconds. The TUI is single-threaded (update loop + View), so no
// mutex is needed.
var (
	subagentCache   []commandItem
	subagentCacheAt time.Time
)

const subagentCacheTTL = 10 * time.Second

func loadDiscoveredSubagents() []commandItem {
	if subagentCache != nil && time.Since(subagentCacheAt) < subagentCacheTTL {
		return subagentCache
	}

	items := append([]commandItem(nil), subagentList...)
	seen := make(map[string]bool)
	for _, it := range items {
		seen[it.cmd] = true
	}

	// Scan ONLY agent-specific directories (not .agents/ which has plans/skills/reports)
	searchDirs := []string{
		".brocode/agents", // project-level subagents
		filepath.Join(os.Getenv("HOME"), ".brocode", "agents"), // global subagents
	}
	for _, dir := range searchDirs {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".md") {
				return nil
			}
			base := strings.TrimSuffix(info.Name(), ".md")
			if base == "current" || base == "README" || base == "SKILL" {
				return nil
			}
			cmdName := "@" + base
			if !seen[cmdName] {
				seen[cmdName] = true
				desc := "Custom agent from " + path
				if data, err := os.ReadFile(path); err == nil {
					for _, line := range strings.Split(string(data), "\n") {
						line = strings.TrimSpace(line)
						if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "Role:") || strings.HasPrefix(line, "description:") {
							desc = strings.TrimPrefix(line, "# ")
							desc = strings.TrimPrefix(desc, "Role: ")
							desc = strings.TrimPrefix(desc, "description: ")
							break
						}
					}
				}
				items = append(items, commandItem{cmd: cmdName, desc: desc})
			}
			return nil
		})
	}
	subagentCache = items
	subagentCacheAt = time.Now()
	// Defensive copy on the way out (same style as the built-in list copy
	// above): callers only read, but a future mutation must never corrupt
	// the shared cache.
	return append([]commandItem(nil), subagentCache...)
}

// suggestIndent indents the popup so it doesn't hug the left edge. A
// package-level style — never built per frame (anti-lag rule 5).
var suggestIndent = lipgloss.NewStyle().PaddingLeft(2)

// suggestVisible reports whether the suggestion popup should render: while
// the input starts with "/" or contains an "@" subagent query.
func (m Model) suggestVisible() bool {
	if m.suggestDismissed {
		return false
	}
	v := m.input.Value()
	return len(suggestFiltered(v)) > 0
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
	if maxW < 40 {
		maxW = 40
	}
	boxW := min(74, maxW)

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
