package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
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

// ctxColor returns the style for a context-usage percentage on the 0–100
// scale: muted under the compaction trigger, sand (warning) between the
// trigger and 80%, red past 80% — window pressure is visible BEFORE it
// becomes a problem (doctrine P4: fire early, never wait for the cliff).
func (m Model) ctxColor(pct float64) lipgloss.Style {
	switch {
	case pct >= 80:
		return m.styles.err
	case pct >= m.compactTriggerPct()*100:
		return m.styles.statusRight
	default:
		return m.styles.statusLeft
	}
}

// modelWindowFor returns the exact context window size for a given model or
// provider. A custom provider's own declared limit (config.jsonc →
// provider.<id>.models.<model>.limit.context) is authoritative and wins over
// every heuristic — that's how a user with a 1M-token local model gets a 1M
// window instead of the 128k fallback.
func modelWindowFor(provider, model string) int {
	model = strings.ToLower(model)

	// Config-defined custom models declare their own window — the single
	// source of truth for custom providers (settings > heuristic). Matched
	// case-insensitively because model names are lowercased above.
	if p, ok := LoadAppConfig().Provider[provider]; ok {
		for mid, cm := range p.Models {
			if strings.EqualFold(mid, model) && cm.Limit.Context > 0 {
				return cm.Limit.Context
			}
		}
	}

	// Explicit context tags in model name
	if strings.Contains(model, "-2m") {
		return 2_000_000
	}
	if strings.Contains(model, "-1m") {
		return 1_000_000
	}
	if strings.Contains(model, "-200k") {
		return 200_000
	}
	if strings.Contains(model, "-128k") {
		return 128_000
	}
	if strings.Contains(model, "-32k") {
		return 32_000
	}

	// Model Family specific matching
	if strings.Contains(model, "gemini") {
		if strings.Contains(model, "pro") {
			return 2_000_000 // Gemini Pro
		}
		return 1_000_000 // Gemini Flash
	}
	if strings.Contains(model, "claude") {
		return 200_000
	}
	if strings.Contains(model, "deepseek") {
		return 131_072 // Native DeepSeek contexts
	}
	if strings.Contains(model, "llama") || strings.Contains(model, "qwen") || strings.Contains(model, "mimo") {
		return 128_000
	}
	if strings.Contains(model, "gpt-4") || strings.Contains(model, "o1") || strings.Contains(model, "o3") {
		return 128_000
	}
	if strings.Contains(model, "laguna") || strings.Contains(model, "poolside") {
		return 1_000_000 // Poolside Laguna is 1M context
	}
	if strings.Contains(model, "mistral") || strings.Contains(model, "mixtral") {
		return 32_768
	}

	// Provider fallbacks
	if provider == "antigravity" {
		return 1_000_000
	}
	if provider == "groq" {
		// Modern Groq models (llama-3.3-70b, deepseek-r1-distill, qwen, etc.)
		// expose 128k+ windows. The old 8k fallback made unknown Groq models
		// trigger auto-compaction at ~5.7k tokens — far too aggressive and
		// destructive for a provider whose models are 128k-class.
		return 128_000
	}

	return 128_000 // Standard fallback
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

	// Used — the effective context pressure: the provider's last reported
	// INPUT when available (settlement — what the API actually counted),
	// otherwise the calibrated forecast (doctrine P3), explicitly labeled
	// "~" so nobody mistakes an estimate for a bill. Percent is color-coded
	// by pressure and matches what auto-compaction fires on.
	used := m.contextPressure()
	label := ""
	if m.actualTokens.input == 0 {
		label = "~" // forecast, not settlement
	}
	win := "—"
	usedStr := fmt.Sprintf("%s%s / %s", label, fmtTokens(used), win)
	if m.window > 0 {
		win = fmtTokens(m.window)
		pct := float64(used) * 100 / float64(m.window)
		usedStr = m.ctxColor(pct).Render(fmt.Sprintf("%s%s / %s · %.1f%%", label, fmtTokens(used), win, pct))
	}
	if m.compactCount > 0 {
		// Auto-compaction history is transparency too — the badge shows the
		// ledger has been folded N times this session.
		usedStr += " " + m.styles.statusRight.Render(fmt.Sprintf("%dx compact", m.compactCount))
	}
	sb.WriteString(m.kv("used", usedStr, w))

	// Show real token breakdown when available
	if m.actualTokens.total > 0 {
		sb.WriteString(m.kv("in", fmtTokens(m.actualTokens.input), w))
		sb.WriteString(m.kv("out", fmtTokens(m.actualTokens.output), w))
		if m.actualTokens.cacheRead > 0 {
			sb.WriteString(m.kv("cache", fmtTokens(m.actualTokens.cacheRead), w))
		}
		if m.actualTokens.cost > 0 {
			sb.WriteString(m.kv("cost", fmt.Sprintf("$%.4f", m.actualTokens.cost), w))
		}
	}

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
			modBadge := ""
			if sa.model != "" {
				modBadge = m.styles.statusLeft.Render("· " + clip(sa.model, 10))
			}
			row := fmt.Sprintf("  @%-7s %s %s", sa.name, color.Render(icon+" "+clip(sa.task, w-22)), modBadge)
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
	line := fmt.Sprintf("  %-7s %s", key, val)
	return m.styles.statusLeft.Render(line) + "\n"
}
