package tui

import (
	"fmt"
	"strings"
)

// renderModelsModalBox renders the framed modal box for /models picker.
// Every provider's models are listed (model left, provider right) with a
// live search filter: typing filters by model name or provider, and the
// [active] badge marks the currently selected model.
// Height is capped; if more models exist, a scroll indicator shows.
func (m Model) renderModelsModalBox() string {
	w := min(72, m.width-4)
	if w < 40 {
		w = 40
	}

	all := m.allModelEntries()
	filtered := filterModelEntries(all, m.modelsQuery)
	noResults := len(filtered) == 0

	// Height budget: the popover floats ABOVE the fixed-bottom input, so the
	// 7 reserved rows (header 1 + input 5 + status 1) are never available.
	// Chrome overhead (title + rule + search + note + blank + footer + 2
	// borders) is ~10 lines. Anything beyond that is model rows.
	maxModels := m.height - headerHeight - inputTotal - statusHeight - 10
	if maxModels < 3 {
		maxModels = 3
	}
	if maxModels > 40 {
		maxModels = 40
	}

	var sb strings.Builder
	titleText := fmt.Sprintf("active AI model (%s)", m.provider)
	if m.provider == "" {
		titleText = "active AI model"
	}

	// Live search box — shows the query or a muted placeholder.
	queryView := m.modelsQuery
	if queryView == "" {
		queryView = m.styles.statusLeft.Render("type to search...")
	} else {
		queryView = m.styles.agent.Render(queryView + "▏")
	}
	sb.WriteString("  " + m.styles.title.Render("🔎") + " " + queryView)
	sb.WriteString("\n")
	if m.modelsQuery != "" {
		sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("  %d of %d models match", len(filtered), len(all))))
		sb.WriteString("\n")
	}
	// Live source note — transparency about where the list came from: the
	// gateway (live), a fetch in flight (⟳), or the static fallback when
	// offline (the fetch failed or never ran).
	switch {
	case m.zenModelsLoading:
		sb.WriteString(m.styles.thinking.Render("  ⟳ fetching live free models from zen gateway..."))
		sb.WriteString("\n")
	case len(m.zenModels) > 0:
		sb.WriteString(m.styles.ok.Render(fmt.Sprintf("  ✓ live: %d free models (zen gateway)", len(m.zenModels))))
		sb.WriteString("\n")
	default:
		sb.WriteString(m.styles.statusLeft.Render("  static list (offline — fetch failed)"))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	if noResults {
		sb.WriteString("  ")
		sb.WriteString(m.styles.err.Render("no models match \"" + m.modelsQuery + "\""))
		sb.WriteString("\n\n")
	} else {
		activeModel := m.selectedModel
		if activeModel == "" && len(all) > 0 {
			activeModel = all[0].model
		}

		// Scroll window: show models around the selected one
		start := 0
		if m.modelsSel >= maxModels {
			start = m.modelsSel - maxModels + 2
		}
		end := start + maxModels
		if end > len(filtered) {
			end = len(filtered)
		}

		if start > 0 {
			sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("    ... %d more above", start)))
			sb.WriteString("\n")
		}

		for i := start; i < end; i++ {
			e := filtered[i]
			marker := "  "
			if i == m.modelsSel {
				marker = "❯ "
			}
			activeBadge := ""
			if e.provider == m.provider && e.model == activeModel {
				activeBadge = " [active]"
			}
			// Model left, provider right — provider column is fixed so rows
			// line up and the provider is always visible. The whole row is
			// clipped to the box width so narrow terminals never overflow.
			row := fmt.Sprintf("%s%d  %-26s %s%s", marker, i+1, clip(e.model, 26), e.provider, activeBadge)
			row = clipLong(row, w-6)
			if i == m.modelsSel {
				sb.WriteString("  ")
				sb.WriteString(m.styles.sideSel.Render(" " + row + " "))
				sb.WriteString("\n")
			} else if e.provider == m.provider && e.model == activeModel {
				sb.WriteString("  ")
				sb.WriteString(m.styles.ok.Render(row))
				sb.WriteString("\n")
			} else {
				sb.WriteString("  ")
				sb.WriteString(m.styles.statusLeft.Render(row))
				sb.WriteString("\n")
			}
		}

		if end < len(filtered) {
			sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("    ... %d more below", len(filtered)-end)))
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n")
	if noResults {
		sb.WriteString(m.styles.popoverFooter.Render("esc clear search · esc/q close"))
	} else {
		sb.WriteString(m.styles.popoverFooter.Render("type to search · ↑↓/1-9 select · enter apply · esc/q close"))
	}

	return m.popoverFrame(titleText, sb.String(), w)
}
