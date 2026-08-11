package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// provider is one connectable LLM provider. The /connect modal lists these;
// the actual authentication flows come later (this is UI only, per the mock
// stage of the project).
type provider struct {
	name   string
	method string
}

var providers = []provider{
	{name: "opencode", method: "url login (browser)"},
	{name: "antigravity", method: "url login (browser)"},
	{name: "claude", method: "api key"},
	{name: "deepseek", method: "api key"},
}

// renderConnectModalBox renders the framed modal box for /connect.
func (m Model) renderConnectModalBox() string {
	w := min(56, m.width-4)
	if w < 30 {
		w = 30
	}

	var sb strings.Builder
	sb.WriteString(m.styles.title.Render(" connect provider "))
	sb.WriteString("\n\n")
	for i, p := range providers {
		row := fmt.Sprintf("%d  %-12s %s", i+1, p.name, p.method)
		if i == m.connectSel {
			sb.WriteString("  ")
			sb.WriteString(m.styles.sideSel.Render(" " + row + " "))
			sb.WriteString("\n")
		} else {
			sb.WriteString("  ")
			sb.WriteString(m.styles.statusLeft.Render(row))
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")
	sb.WriteString(m.styles.statusLeft.Render("1-4 / ↑↓ select · enter choose · esc/q close"))
	sb.WriteString("\n")
	sb.WriteString(m.styles.statusLeft.Render("UI only for now — auth comes with the provider layer."))

	return m.styles.connectBox.Width(w).Render(sb.String())
}

// renderConnect is the full-viewport /connect view helper.
func (m Model) renderConnect() string {
	bodyH := m.height - 5
	if bodyH < 8 {
		bodyH = 8
	}
	box := m.renderConnectModalBox()
	return lipgloss.Place(m.width, bodyH, lipgloss.Center, lipgloss.Center, box)
}
