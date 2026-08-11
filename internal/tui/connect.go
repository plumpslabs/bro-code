package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
)

// provider is one connectable LLM provider.
type provider struct {
	name      string
	method    string
	detected  bool
	freeModel string
}

var defaultProviders = []provider{
	{name: "opencode", method: "auto-detect (free models)", detected: false},
	{name: "antigravity", method: "url login (browser)", detected: false},
	{name: "claude", method: "api key", detected: false},
	{name: "deepseek", method: "api key", detected: false},
}

// openCodeFreeModels lists default free models supported by OpenCode.
var openCodeFreeModels = []string{
	"deepseek-v4-flash-free",
	"mimo-v2.5-free",
	"laguna-s-2.1-free",
	"ling-3.0-tiny-free",
	"longcat-2.0-free",
	"nemotron-3-ultra-free",
	"big-pickle",
}

// DetectOpenCode checks if OpenCode CLI or config exists locally on the machine.
func DetectOpenCode() (bool, string) {
	// matcha:explain check standard PATH or ~/.opencode/bin/opencode location
	if _, err := exec.LookPath("opencode"); err == nil {
		return true, openCodeFreeModels[0]
	}

	home, err := os.UserHomeDir()
	if err == nil {
		paths := []string{
			filepath.Join(home, ".opencode", "bin", "opencode"),
			filepath.Join(home, ".config", "opencode"),
			filepath.Join(home, ".local", "share", "opencode"),
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				return true, openCodeFreeModels[0]
			}
		}
	}
	return false, ""
}

// renderConnectModalBox renders the framed modal box for /connect with auto-detection badges.
func (m Model) renderConnectModalBox() string {
	w := min(62, m.width-4)
	if w < 32 {
		w = 32
	}

	// matcha:explain reuse cached opencodeDetected state from Model to prevent disk I/O in render loop
	detected := m.opencodeDetected
	activeModel := m.selectedModel
	if activeModel == "" {
		activeModel = openCodeFreeModels[0]
	}

	var sb strings.Builder
	sb.WriteString(m.styles.title.Render(" connect provider "))
	sb.WriteString("\n\n")

	for i, p := range defaultProviders {
		statusStr := p.method
		if p.name == "opencode" && detected {
			statusStr = m.styles.ok.Render("✓ configured")
		}

		row := fmt.Sprintf("%d  %-12s %s", i+1, p.name, statusStr)
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

	if detected {
		sb.WriteString("\n" + m.styles.title.Render(" ── select model for opencode ──") + "\n")
		for i, modName := range openCodeFreeModels {
			marker := "  "
			if modName == activeModel {
				marker = "❯ "
			}
			activeBadge := ""
			if modName == activeModel {
				activeBadge = " [active]"
			}
			line := fmt.Sprintf("  %s%d  %s%s", marker, i+1, modName, activeBadge)
			if modName == activeModel {
				sb.WriteString(m.styles.ok.Render(line) + "\n")
			} else {
				sb.WriteString(m.styles.statusLeft.Render(line) + "\n")
			}
		}
	}

	sb.WriteString("\n")
	sb.WriteString(m.styles.statusLeft.Render("1-4 select provider · enter choose · esc/q close"))

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
