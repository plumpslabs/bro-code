package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// logoBlock is the pixel-style "BRO CODE" wordmark. Drawn once at package
// load (cheap: a few dozen strings) and re-styled per render — no per-frame
// allocation beyond the final join. Pure box-drawing chars: renders on any
// terminal, zero images.
const logoBlock = `
██████╗ ██████╗  ██████╗     ██████╗  ██████╗ ██████╗ ███████╗
██╔══██╗██╔══██╗██╔═══██╗    ██╔════╝ ██╔═══██╗██╔══██╗██╔════╝
██████╔╝██████╔╝██║   ██║    ██║      ██║   ██║██║  ██║█████╗
██╔══██╗██╔══██╗██║   ██║    ██║      ██║   ██║██║  ██║██╔══╝
██████╔╝██║  ██║╚██████╔╝    ╚██████╗ ╚██████╔╝██████╔╝███████╗
╚══════╝╚═╝  ╚═╝ ╚═════╝      ╚═════╝  ╚═════╝ ╚═════╝ ╚══════╝`

// logoArt is the wordmark normalized into a perfect rectangle: every line
// trimmed of trailing spaces and right-padded to the widest line, so the
// whole block has ONE width. Official Lip Gloss guidance: trailing spaces
// and inconsistent line widths skew Width/Place centering — normalization
// makes the center exact. (font: figlet ansi_shadow, verified output)
var logoArt = padBlock(logoBlock)

// logoWidth is the display width of the normalized wordmark.
var logoWidth = lipgloss.Width(logoArt)

// themeSwatch precomputes a two-block color swatch per preset, once. It is
// immutable display data (like the Themes map itself) — the picker never
// rebuilds styles per frame (pro-TUI rule 4).
var themeSwatch = func() map[string]string {
	out := make(map[string]string, len(Themes))
	for n, t := range Themes {
		out[n] = lipgloss.NewStyle().Bold(true).Foreground(t.Primary).Render("■■")
	}
	return out
}()

// renderLanding builds the new-conversation screen: the pixel wordmark,
// tagline, hint, and the input form — all centered as one block so a fresh
// start reads as a form, not a bare prompt. Returns "" on tiny terminals,
// letting the caller fall back to the regular layout.
func (m Model) renderLanding(width, height int) string {
	// The wordmark is ~73 cols wide — the landing needs room to breathe.
	if width < logoWidth+6 || height < 14 {
		return ""
	}
	hint := "type a message or /help to begin"
	if width >= 62 {
		hint += " · brocode -c resumes your last session"
	}
	parts := []string{
		centerBlock(m.styles.logo.Render(logoArt), width),
		"",
		centerBlock(m.styles.tagline.Render("lean · efficient · transparent"), width),
		centerBlock(m.styles.hint.Render(hint), width),
		"",
		centerBlock(m.renderInputForm(width), width),
	}
	stack := strings.Join(parts, "\n")
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, stack)
}

// renderInputForm is the landing's input field framed as a form. The prompt
// glyph and the focused border make it obvious where to type — the input
// stays a real textinput, so typing works exactly like in the chat view.
func (m Model) renderInputForm(width int) string {
	typedView := m.input.View()
	promptStr := m.styles.prompt.Render("❯ ")

	if m.suggestVisible() {
		items := suggestFiltered(m.input.Value())
		if m.suggestSel >= 0 && m.suggestSel < len(items) {
			typed := m.input.Value()
			target := items[m.suggestSel].cmd
			if strings.HasPrefix(target, typed) && len(target) > len(typed) {
				ghostSuffix := target[len(typed):]
				trimmed := strings.TrimRight(typedView, " ")
				typedView = trimmed + m.styles.sys.Render(ghostSuffix)
			}
		}
	}

	// Re-pad typedView to m.input.Width() so the input box container never shrinks
	curW := lipgloss.Width(ansiStrip.ReplaceAllString(typedView, ""))
	if targetW := m.input.Width(); curW < targetW {
		typedView += strings.Repeat(" ", targetW-curW)
	}

	return m.styles.inputBox.Render(promptStr + typedView)
}

// renderThemeModalBox renders the framed modal box for /theme.
func (m Model) renderThemeModalBox() string {
	names := themeNames()
	w := min(50, m.width-4)
	if w < 30 {
		w = 30
	}

	var sb strings.Builder
	sb.WriteString(m.styles.title.Render(" select theme "))
	sb.WriteString("\n\n")
	for i, n := range names {
		marker := "  "
		if i == m.themeSel {
			marker = "❯ "
		}
		label := marker + n
		if i == m.themeSel {
			label = m.styles.sideSel.Render(" " + label + " ")
		} else {
			label = m.styles.statusLeft.Render(label)
		}
		sb.WriteString("  ")
		sb.WriteString(themeSwatch[n])
		sb.WriteString(" ")
		sb.WriteString(label)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("↑↓ / 1-%d select · enter apply · esc/q cancel", len(names))))

	return m.styles.connectBox.Width(w).Render(sb.String())
}

// renderTheme is the /theme picker modal helper.
func (m Model) renderTheme() string {
	bodyH := m.height - 5
	if bodyH < 8 {
		bodyH = 8
	}
	box := m.renderThemeModalBox()
	return lipgloss.Place(m.width, bodyH, lipgloss.Center, lipgloss.Center, box)
}

// padBlock normalizes a multi-line text block into a rectangle: trailing
// whitespace is trimmed per line, then every line is right-padded to the
// widest line. Used on the wordmark so its lines share one width and
// therefore center identically (Lip Gloss docs: trailing spaces skew
// Width/Place centering).
func padBlock(block string) string {
	// Trim leading AND trailing newlines — the const opens with a newline,
	// which would otherwise render a stray blank row above the wordmark and
	// shift the vertical center.
	lines := strings.Split(strings.Trim(block, "\n"), "\n")
	maxW := 0
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " ")
		if w := lipgloss.Width(lines[i]); w > maxW {
			maxW = w
		}
	}
	for i, ln := range lines {
		if pad := maxW - lipgloss.Width(ln); pad > 0 {
			lines[i] = ln + strings.Repeat(" ", pad)
		}
	}
	return strings.Join(lines, "\n")
}

// centerBlock centers every line of a pre-rendered multi-line string within
// width AND right-pads it to exactly width. The right-padding is the
// precision fix: without it the block's widest line stops short of the
// canvas, so lipgloss.Place centers the whole stack a second time and every
// line drifts right by half the leftover. With uniform-width lines Place
// adds nothing horizontally — single, exact centering.
func centerBlock(block string, width int) string {
	if width <= 0 {
		return block
	}
	lines := strings.Split(block, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		w := lipgloss.Width(ln)
		pad := (width - w) / 2
		right := width - w - pad
		if pad < 0 {
			pad = 0
		}
		if right < 0 {
			right = 0
		}
		out = append(out, strings.Repeat(" ", pad)+ln+strings.Repeat(" ", right))
	}
	return strings.Join(out, "\n")
}
