package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
)

// keymap centralizes every binding in the app (pro-TUI rule: consistency is
// a system, not a memory exercise). Key names are canonical Bubble Tea v2
// strings — "enter", "up", "pgup", "ctrl+c", "q", etc.
type keymap struct {
	Quit     key.Binding
	Send     key.Binding
	Scroll   key.Binding
	Help     key.Binding
	Collapse key.Binding
	Copy     key.Binding
}

var keys = keymap{
	Quit: key.NewBinding(
		key.WithKeys("ctrl+c", "q"),
		key.WithHelp("ctrl+c / q", "quit"),
	),
	Send: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "send"),
	),
	Scroll: key.NewBinding(
		key.WithKeys("up", "down", "pgup", "pgdown"),
		key.WithHelp("↑↓ history / wheel scroll", ""),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "help"),
	),
	Collapse: key.NewBinding(
		key.WithKeys("ctrl+o"),
		key.WithHelp("ctrl+o", "expand/collapse"),
	),
	Copy: key.NewBinding(
		key.WithKeys("ctrl+y"),
		key.WithHelp("ctrl+y", "copy reply"),
	),
}

// shortHelp renders the one-line hint shown in the status bar, built from the
// bindings themselves so it can never drift from the actual keymap.
func (k keymap) shortHelp() string {
	bindings := []key.Binding{k.Send, k.Scroll, k.Collapse, k.Copy, k.Help, k.Quit}
	parts := make([]string, 0, len(bindings))
	for _, b := range bindings {
		h := b.Help()
		parts = append(parts, h.Key+" "+h.Desc)
	}
	return strings.Join(parts, " · ")
}
