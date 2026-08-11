package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// panelState is the static transparency data captured once at startup: the
// git branch and the working directory relative to home. Both are cheap to
// read (a HEAD file + cwd) and don't change mid-session.
type panelState struct {
	branch string
	path   string
}

// gitInfo reads the current git branch and a short working path. The repo
// is found by walking UP from the cwd (like git itself), so the info is
// correct no matter which subdirectory brocode is launched from. Never
// fails hard — unknown values become "—".
func gitInfo() panelState {
	var st panelState
	wd, err := os.Getwd()
	if err != nil {
		return panelState{branch: "—", path: "—"}
	}
	if root, ok := findRepoRoot(wd); ok {
		// Path shown relative to the repo root: "bro-code",
		// "bro-code/internal/tui".
		base := filepath.Base(root)
		if rel, err := filepath.Rel(root, wd); err == nil && rel != "." {
			st.path = base + "/" + rel
		} else {
			st.path = base
		}
		if head, ok := readHead(root); ok {
			switch {
			case strings.HasPrefix(head, "ref: refs/heads/"):
				st.branch = strings.TrimPrefix(head, "ref: refs/heads/")
			default:
				// Detached HEAD — short hash, sliced on runes not bytes.
				if r := []rune(head); len(r) >= 7 {
					st.branch = string(r[:7])
				}
			}
		}
	} else {
		st.path = wd
	}
	if st.branch == "" {
		st.branch = "—"
	}
	return st
}

// findRepoRoot walks up from wd looking for a .git entry (directory, or a
// "gitdir: …" worktree file). Returns the checkout root and true on success.
func findRepoRoot(wd string) (string, bool) {
	dir := wd
	for {
		gitEntry := filepath.Join(dir, ".git")
		if _, err := os.Stat(gitEntry); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// readHead returns the raw HEAD content of the repo whose checkout root is
// root (worktree-aware: a .git file points at the real gitdir).
func readHead(root string) (string, bool) {
	gitEntry := filepath.Join(root, ".git")
	gitDir := gitEntry
	if fi, err := os.Stat(gitEntry); err == nil && !fi.IsDir() {
		if data, err := os.ReadFile(gitEntry); err == nil {
			if s := strings.TrimSpace(string(data)); strings.HasPrefix(s, "gitdir:") {
				gitDir = strings.TrimSpace(strings.TrimPrefix(s, "gitdir:"))
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// tokenEstimate is a rough upper-ish bound of the transcript size — ~4 chars
// per token on English text. It is an ESTIMATE (Principle 3: real numbers
// come from the API); for now it keeps the context window display honest
// instead of showing a fake 0 forever.
func tokenEstimate(chat []chatMsg) int {
	n := 0
	for _, cm := range chat {
		n += utf8.RuneCountInString(cm.text) / 4
		if cm.content != "" {
			n += utf8.RuneCountInString(cm.content) / 4
		}
	}
	return n
}

// fmtTokens renders a token count compactly: 1.5k, 2.0M, 137.
func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// modelWindows is the mock context window per provider (UI stage). Real
// windows come with the provider layer; these only size the display.
var modelWindows = map[string]int{
	"opencode":    200_000,
	"antigravity": 200_000,
	"claude":      200_000,
	"deepseek":    131_072,
}

// activityPanelRows caps how many tool rows the panel shows — the rest is
// summarized as "+N more" (bounded rendering, Principle 1).
const activityPanelRows = 4

// renderPanel is the right-hand status panel — a transparency dashboard,
// not just activity: context (model + live token usage), git (branch +
// path), MCP (filter layer + connected servers), agents (primary + lazy),
// then tool activity. All values are truncated to the panel width.
func (m Model) renderPanel() string {
	w := panelWidth - 4 // inside the panel border
	var sb strings.Builder

	// Context — provider, model, window, live token usage (transparent, P3 spirit).
	sb.WriteString(m.section("context"))
	prov := "not connected"
	if m.provider != "" {
		prov = m.provider
	}
	sb.WriteString(m.kv("provider", prov, w))

	modName := "—"
	if m.selectedModel != "" {
		modName = m.selectedModel
	}
	sb.WriteString(m.kv("model", clip(modName, w-9), w))

	used := tokenEstimate(m.chat)
	win := "—"
	pct := ""
	if m.window > 0 {
		win = fmtTokens(m.window)
		pct = fmt.Sprintf(" · %.1f%%", float64(used)*100/float64(m.window))
	}
	sb.WriteString(m.kv("used", fmt.Sprintf("%s / %s%s", fmtTokens(used), win, pct), w))

	// Git — branch + cwd relative to home.
	sb.WriteString(m.section("git"))
	sb.WriteString(m.kv("branch", m.panel.branch, w))
	sb.WriteString(m.kv("path", clip(m.panel.path, w-9), w))

	// MCP — filter layer state (Principle 2) + connected servers.
	sb.WriteString(m.section("mcp"))
	sb.WriteString(m.kv("filter", "armed (lazy)", w))
	sb.WriteString(m.kv("servers", "0 connected", w))

	// Agents — primary + live spawned subagents dashboard.
	sb.WriteString(m.section("agents"))
	sb.WriteString(m.kv("primary", "brocode", w))
	if len(m.subagents) == 0 {
		sb.WriteString(m.kv("subagents", "idle (0 active)", w))
	} else {
		for _, sa := range m.subagents {
			icon, color := m.statusGlyph("spawn_" + sa.status)
			if sa.status == "working" {
				icon, color = "⚡", m.styles.statusRight
			}
			row := fmt.Sprintf("  @%-7s %s", sa.name, color.Render(icon+" "+clip(sa.task, w-14)))
			sb.WriteString(row)
			sb.WriteString("\n")
		}
	}

	// Activity — bounded at activityPanelRows, and each row is width-budgeted
	// (icon + separators + label + detail) so it can never exceed the panel
	// content width (bounded rendering, Principle 1).
	sb.WriteString(m.section("activity"))
	if len(m.activity) == 0 {
		sb.WriteString(m.styles.statusLeft.Render("  no tool calls yet"))
		sb.WriteString("\n")
	} else {
		// 4 fixed cols (spaces + glyph) + label 18 + detail 10 = 32 ≤ w.
		for i, a := range m.activity {
			if i >= activityPanelRows {
				sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("  +%d more", len(m.activity)-activityPanelRows)))
				sb.WriteString("\n")
				break
			}
			icon, color := m.statusGlyph(a.status)
			row := " " + color.Render(icon+" "+clip(a.label, 18))
			if a.detail != "" {
				row += " " + m.styles.statusLeft.Render(clip(a.detail, 10))
			}
			sb.WriteString(row)
			sb.WriteString("\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// section renders a panel section header in the accent color.
func (m Model) section(name string) string {
	return m.styles.title.Render(" "+name) + "\n"
}

// kv renders a "key  value" row: key padded to a fixed width, value clipped
// so a long branch/path never overflows the panel (Principle 1).
func (m Model) kv(key, val string, w int) string {
	line := fmt.Sprintf("  %-7s %s", key, clip(val, w-10))
	return m.styles.statusLeft.Render(line) + "\n"
}

// clip truncates a string to n runes with an ellipsis — single-line safe
// (unlike truncate, which is for chat blocks).
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
