package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// renderModelsModalBox renders the framed modal box for /models picker.
func (m Model) renderModelsModalBox() string {
	w := min(62, m.width-4)
	if w < 32 {
		w = 32
	}

	activeModel := m.selectedModel
	if activeModel == "" {
		activeModel = openCodeFreeModels[0]
	}

	var sb strings.Builder
	sb.WriteString(m.styles.title.Render(" select active AI model "))
	sb.WriteString("\n\n")

	for i, modName := range openCodeFreeModels {
		marker := "  "
		if i == m.modelsSel {
			marker = "❯ "
		}
		activeBadge := ""
		if modName == activeModel {
			activeBadge = " [active]"
		}
		row := fmt.Sprintf("%s%d  %-25s%s", marker, i+1, modName, activeBadge)
		if i == m.modelsSel {
			sb.WriteString("  ")
			sb.WriteString(m.styles.sideSel.Render(" " + row + " "))
			sb.WriteString("\n")
		} else if modName == activeModel {
			sb.WriteString("  ")
			sb.WriteString(m.styles.ok.Render(row))
			sb.WriteString("\n")
		} else {
			sb.WriteString("  ")
			sb.WriteString(m.styles.statusLeft.Render(row))
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n")
	sb.WriteString(m.styles.statusLeft.Render("1-7 / ↑↓ select · enter apply · esc/q close"))

	return m.styles.connectBox.Width(w).Render(sb.String())
}

// renderModels is the full-viewport /models view helper.
func (m Model) renderModels() string {
	bodyH := m.height - 5
	if bodyH < 8 {
		bodyH = 8
	}
	box := m.renderModelsModalBox()
	return lipgloss.Place(m.width, bodyH, lipgloss.Center, lipgloss.Center, box)
}
