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
	PopoverBG    color.Color // popover panel background (dim floating card)
}

// DefaultTheme is the modern minimalist palette: warm muted grays with a
// soft cyan accent and near-black popover panels. Low glare, high contrast,
// thin-bordered elegance (Warp/Kilo-style).
var DefaultTheme = Theme{
	Primary:      lipgloss.Color("#7dd3fc"), // soft cyan
	Secondary:    lipgloss.Color("#d6d3d1"), // warm off-white
	Muted:        lipgloss.Color("#a8a29e"), // warm muted gray
	Error:        lipgloss.Color("#fca5a5"), // soft red
	Success:      lipgloss.Color("#86efac"), // soft green
	Accent:       lipgloss.Color("#fcd34d"), // soft amber
	Border:       lipgloss.Color("#44403c"), // dark warm border
	BorderActive: lipgloss.Color("#7dd3fc"), // soft cyan active border
	PopoverBG:    lipgloss.Color("#1c1917"), // near-black stone (popover card)
}

// Themes are the minimal built-in presets — plain data maps, no runtime cost
// until one is selected. "mono" is the grayscale, distraction-free variant;
// "ocean" is a cool blue one. Add presets here, never inline colors.
var Themes = map[string]Theme{
	"default": DefaultTheme,
	"mono": {
		Primary:      lipgloss.Color("#e7e5e4"),
		Secondary:    lipgloss.Color("#d6d3d1"),
		Muted:        lipgloss.Color("#a8a29e"),
		Error:        lipgloss.Color("#fca5a5"),
		Success:      lipgloss.Color("#86efac"),
		Accent:       lipgloss.Color("#fafaf9"),
		Border:       lipgloss.Color("#44403c"),
		BorderActive: lipgloss.Color("#fafaf9"),
		PopoverBG:    lipgloss.Color("#1c1917"),
	},
	"ocean": {
		Primary:      lipgloss.Color("#67e8f9"),
		Secondary:    lipgloss.Color("#a5f3fc"),
		Muted:        lipgloss.Color("#7dd3fc"),
		Error:        lipgloss.Color("#fca5a5"),
		Success:      lipgloss.Color("#86efac"),
		Accent:       lipgloss.Color("#fde68a"),
		Border:       lipgloss.Color("#155e75"),
		BorderActive: lipgloss.Color("#67e8f9"),
		PopoverBG:    lipgloss.Color("#082f3b"),
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
	selReverse  lipgloss.Style // drag-select highlight (reverse video)
	builderMode lipgloss.Style
	plannerMode lipgloss.Style
	matchaMode  lipgloss.Style

	chatBoxOn  lipgloss.Style
	sideBoxIn  lipgloss.Style
	inputBoxOn lipgloss.Style

	// Popover chrome — thin-bordered floating card, bottom-anchored over the
	// input bar (command-palette style). The backdrop dims the base canvas.
	popoverBox    lipgloss.Style
	popoverTitle  lipgloss.Style
	popoverFooter lipgloss.Style
	backdrop      lipgloss.Style

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
		// Thin border (1px) — the "elegant" look. No thick rounded frames.
		return lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(c)
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
		selReverse:  lipgloss.NewStyle().Reverse(true),
		builderMode: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#fbbf24")),
		plannerMode: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#34d399")),
		matchaMode:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#2dd4bf")),

		chatBoxOn:  box(t.BorderActive),
		sideBoxIn:  box(t.Border),
		inputBoxOn: box(t.BorderActive),

		// Popover: thin muted border + dark card background so it reads as a
		// floating panel above the dimmed chat.
		popoverBox:    lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(t.BorderActive).Background(t.PopoverBG).Padding(0, 1),
		popoverTitle:  lipgloss.NewStyle().Bold(true).Foreground(t.Primary),
		popoverFooter: lipgloss.NewStyle().Foreground(t.Muted),
		backdrop:      lipgloss.NewStyle().Faint(true),

		logo:       lipgloss.NewStyle().Bold(true).Foreground(t.Primary),
		tagline:    lipgloss.NewStyle().Italic(true).Foreground(t.Secondary),
		hint:       lipgloss.NewStyle().Foreground(t.Muted),
		connectBox: box(t.BorderActive),
		inputBox:   box(t.BorderActive),
	}
}
