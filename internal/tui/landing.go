package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// logoBlock is the pixel-style "BRO CODE" wordmark — resized to the compact
// figlet "standard" glyphs (5 rows × 44 cols instead of 6 × 73) so the brand
// block breathes instead of dominating the viewport. Drawn once at package
// load (cheap: a few dozen strings) and re-styled per render — no per-frame
// allocation beyond the final join. Pure ASCII: renders on any terminal.
// (glyphs verified via pyfiglet font="standard", not hand-drawn)
const logoBlock = `
 ____  ____   ___     ____ ___  ____  _____
| __ )|  _ \ / _ \   / ___/ _ \|  _ \| ____|
|  _ \| |_) | | | | | |  | | | | | | |  _|
| |_) |  _ <| |_| | | |__| |_| | |_| | |___
|____/|_| \_\\___/   \____\___/|____/|_____|
`

// logoArt is the wordmark normalized into a perfect rectangle: every line
// trimmed of trailing spaces and right-padded to the widest line, so the
// whole block has ONE width. Official Lip Gloss guidance: trailing spaces
// and inconsistent line widths skew Width/Place centering — normalization
// makes the center exact. (font: figlet standard, verified via pyfiglet)
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

// renderBrandBlock builds the compact brand element that lives at the top of
// the chat viewport (Phase 2 single-view design): the pixel wordmark, the
// tagline, and — only while no conversation has started — a hint. It is
// TOP-aligned, never a giant centered screen, and acts as a stable prefix:
// on a fresh start the empty chat shows it alone; as messages accumulate it
// is pushed up and scrolls away naturally.
//
// Perf: the expensive gradient coloring is cached per theme (m.logoView);
// this function only centers precomputed strings, so it is safe per frame.
// Returns "" on tiny terminals, leaving just the viewport breathing space.
func (m Model) renderBrandBlock(width int) string {
	// The wordmark is 44 cols wide (figlet "standard") — the brand needs room
	// to breathe, so it hides on very narrow terminals (below ~50 cols).
	if width < logoWidth+6 {
		return ""
	}
	hint := "type a message or /help to begin"
	if width >= 62 {
		hint += " · brocode -c resumes your last session"
	}

	var sb strings.Builder
	sb.WriteString(centerBlock(m.logoView, width))
	sb.WriteString("\n")
	sb.WriteString(centerBlock(m.styles.tagline.Render("lean · efficient · transparent"), width))
	if !m.started {
		sb.WriteString("\n")
		sb.WriteString(centerBlock(m.styles.hint.Render(hint), width))
	}
	return sb.String()
}

// renderThemeModalBox renders the popover for /theme (thin border, muted
// swatches, compact rows).
func (m Model) renderThemeModalBox() string {
	names := themeNames()
	w := min(46, m.width-4)
	if w < 30 {
		w = 30
	}

	var sb strings.Builder
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
	sb.WriteString(m.styles.popoverFooter.Render(fmt.Sprintf("↑↓ / 1-%d select · enter apply · esc/q cancel", len(names))))

	return m.popoverFrame("theme", sb.String(), w)
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

// renderGradientLogo renders the wordmark with a two-tone vertical gradient:
// the top rows lighten toward white and the bottom rows darken toward
// black, meeting at the theme's true logo color in the middle. Blending
// both directions (instead of only lightening) guarantees visible contrast
// no matter how light or dark the active theme's base color already is —
// a one-directional blend toward white, for example, does almost nothing
// if the base color is already near-white. No new theme fields needed —
// the base color is read straight off the existing m.styles.logo style.
func renderGradientLogo(art string, base lipgloss.Style) string {
	lines := strings.Split(art, "\n")
	n := len(lines)
	if n <= 1 {
		return base.Render(art)
	}

	r, g, b, _ := base.GetForeground().RGBA()
	// RGBA() returns 16-bit channels (0-65535); scale down to 8-bit.
	baseR, baseG, baseB := uint8(r>>8), uint8(g>>8), uint8(b>>8)

	// How far the extremes are allowed to travel toward white/black.
	// 0.65 is aggressive enough to read clearly as a gradient at a glance.
	const maxShift = 0.65

	out := make([]string, n)
	for i, ln := range lines {
		// frac: 0 at the top row, 1 at the bottom row.
		frac := float64(i) / float64(n-1)

		var lr, lg, lb uint8
		if frac < 0.5 {
			// Top half: blend toward white. Strength peaks at the very
			// top row (frac=0) and fades to 0 at the midpoint.
			strength := (0.5 - frac) * 2 * maxShift
			lr = blendToward(baseR, 255, strength)
			lg = blendToward(baseG, 255, strength)
			lb = blendToward(baseB, 255, strength)
		} else {
			// Bottom half: blend toward black. Strength peaks at the very
			// bottom row (frac=1) and fades to 0 at the midpoint.
			strength := (frac - 0.5) * 2 * maxShift
			lr = blendToward(baseR, 0, strength)
			lg = blendToward(baseG, 0, strength)
			lb = blendToward(baseB, 0, strength)
		}

		c := lipgloss.Color(hexColor(lr, lg, lb))
		rendered := lipgloss.NewStyle().Bold(true).Foreground(c).Render(ln)
		// lipgloss Render trims leading/trailing whitespace — re-pad so every
		// row keeps the exact original width (lipgloss.Width strips ANSI).
		// Without this, rows that start or end with a space (figlet "standard"
		// row 0) render 1-2 cols narrower and centerBlock then mis-centers them.
		if cur := lipgloss.Width(rendered); cur < lipgloss.Width(ln) {
			rendered += strings.Repeat(" ", lipgloss.Width(ln)-cur)
		}
		out[i] = rendered
	}
	return strings.Join(out, "\n")
}

// blendToward mixes a toward b by fraction t (0 = stay at a, 1 = fully b).
func blendToward(a, b uint8, t float64) uint8 {
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return uint8(float64(a) + (float64(b)-float64(a))*t)
}

func hexColor(r, g, b uint8) string {
	return "#" + hexByte(r) + hexByte(g) + hexByte(b)
}

func hexByte(v uint8) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[v>>4], hex[v&0xf]})
}
