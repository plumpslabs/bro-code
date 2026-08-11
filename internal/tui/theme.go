package tui

import (
	"image/color"
	"sort"

	"charm.land/lipgloss/v2"
)

// Theme holds the semantic color roles for the whole UI. Every style in the
// app references these roles — never raw ANSI colors scattered inline
// (pro-TUI rule 8). This is what makes re-theming and dark/light adaptation
// possible without a search-and-replace.
type Theme struct {
	Primary      color.Color // brand accent — active focus, primary action
	Secondary    color.Color // secondary accent — agent identity
	Muted        color.Color // de-emphasized text, hints, inactive borders
	Error        color.Color // errors, deletions
	Success      color.Color // success, additions, done
	Accent       color.Color // scores, warnings, in-flight work
	Border       color.Color // inactive panel border
	BorderActive color.Color // focused panel border
}

// DefaultTheme is the modern slate & soft cyan palette (Nord/Tokyo-Night inspired).
// Values use ANSI 256 indexes for terminal compatibility while feeling clean and low-glare.
var DefaultTheme = Theme{
	Primary:      lipgloss.Color("117"), // soft cyan
	Secondary:    lipgloss.Color("253"), // soft off-white/grey for agent text
	Muted:        lipgloss.Color("244"), // low-contrast muted grey
	Error:        lipgloss.Color("203"), // soft red
	Success:      lipgloss.Color("114"), // soft green
	Accent:       lipgloss.Color("180"), // subtle warm grey/sand
	Border:       lipgloss.Color("237"), // dark slate border
	BorderActive: lipgloss.Color("117"), // soft cyan active border
}

// Themes are the minimal built-in presets — plain data maps, no runtime cost
// until one is selected. "mono" is the grayscale, distraction-free variant;
// "ocean" is a cool blue one. Add presets here, never inline colors.
var Themes = map[string]Theme{
	"default": DefaultTheme,
	"mono": {
		Primary:      lipgloss.Color("255"),
		Secondary:    lipgloss.Color("250"),
		Muted:        lipgloss.Color("244"),
		Error:        lipgloss.Color("203"),
		Success:      lipgloss.Color("114"),
		Accent:       lipgloss.Color("252"),
		Border:       lipgloss.Color("238"),
		BorderActive: lipgloss.Color("255"),
	},
	"ocean": {
		Primary:      lipgloss.Color("81"),
		Secondary:    lipgloss.Color("75"),
		Muted:        lipgloss.Color("110"),
		Error:        lipgloss.Color("203"),
		Success:      lipgloss.Color("114"),
		Accent:       lipgloss.Color("229"),
		Border:       lipgloss.Color("239"),
		BorderActive: lipgloss.Color("81"),
	},
}

// themeNames returns the preset names in stable (sorted) order.
func themeNames() []string {
	names := make([]string, 0, len(Themes))
	for n := range Themes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// themeIndex returns the picker index of a preset name (0 for unknown — the
// picker always has a valid selection).
func themeIndex(name string) int {
	for i, n := range themeNames() {
		if n == name {
			return i
		}
	}
	return 0
}

// styles is the complete precomputed style set for one theme. Built once per
// theme change (pro-TUI rule 4: never rebuilt per frame).
type styles struct {
	title       lipgloss.Style
	userBar     lipgloss.Style // vertical bar on the left of user messages
	agent       lipgloss.Style
	sys         lipgloss.Style
	prompt      lipgloss.Style
	statusLeft  lipgloss.Style
	statusRight lipgloss.Style
	ok          lipgloss.Style
	err         lipgloss.Style
	thinking    lipgloss.Style
	sideSel     lipgloss.Style
	spinner     lipgloss.Style

	chatBoxOn  lipgloss.Style
	sideBoxIn  lipgloss.Style
	inputBoxOn lipgloss.Style

	logo       lipgloss.Style // landing wordmark (pixel block)
	tagline    lipgloss.Style // landing one-liner
	hint       lipgloss.Style // landing secondary hint
	connectBox lipgloss.Style
	inputBox   lipgloss.Style
}

// newStyles precomputes every style from a theme. Call only on theme changes
// (once at startup + once per /theme), never inside View().
func newStyles(t Theme) styles {
	box := func(c color.Color) lipgloss.Style {
		return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c)
	}
	return styles{
		title:       lipgloss.NewStyle().Bold(true).Foreground(t.Primary),
		userBar:     lipgloss.NewStyle().Foreground(t.Primary),
		agent:       lipgloss.NewStyle().Foreground(t.Secondary),
		sys:         lipgloss.NewStyle().Italic(true).Foreground(t.Muted),
		prompt:      lipgloss.NewStyle().Bold(true).Foreground(t.Primary),
		statusLeft:  lipgloss.NewStyle().Foreground(t.Muted),
		statusRight: lipgloss.NewStyle().Foreground(t.Accent),
		ok:          lipgloss.NewStyle().Foreground(t.Success),
		err:         lipgloss.NewStyle().Foreground(t.Error),
		thinking:    lipgloss.NewStyle().Italic(true).Foreground(t.Accent),
		sideSel:     lipgloss.NewStyle().Background(t.Primary).Foreground(lipgloss.Color("0")),
		spinner:     lipgloss.NewStyle().Foreground(t.Accent),

		chatBoxOn:  box(t.BorderActive),
		sideBoxIn:  box(t.Border),
		inputBoxOn: box(t.BorderActive),

		logo:       lipgloss.NewStyle().Bold(true).Foreground(t.Primary),
		tagline:    lipgloss.NewStyle().Italic(true).Foreground(t.Secondary),
		hint:       lipgloss.NewStyle().Foreground(t.Muted),
		connectBox: box(t.BorderActive),
		inputBox:   box(t.BorderActive),
	}
}
