package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/plumpslabs/bro-code/internal/version"
)

// bannerGradient is the per-row blue shade (ANSI 256): dark indigo → bright
// sky blue top to bottom, applied to the shared version.Logo rows.
var bannerGradient = []string{"24", "33", "39"}

// welcomeBanner renders the fresh-session hero: the compact blue-gradient logo
// (shared with `brocode -v`), the tagline, the version line, and the one-line
// hint. It lives in the message log (the first entry), so once the user starts
// typing it simply scrolls up with the conversation and never overlaps the
// input. Only shown when the session has no history — a resumed conversation
// starts from its own messages.
func welcomeBanner() string {
	var sb strings.Builder
	rows := strings.Split(version.Logo, "\n")
	for i, row := range rows {
		color := "24"
		if i < len(bannerGradient) {
			color = bannerGradient[i]
		}
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Render(row))
		sb.WriteString("\n")
	}
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	sb.WriteString(dim.Render(version.Tagline))
	sb.WriteString("\n")
	sb.WriteString(dim.Render("BroCode " + version.Version))
	sb.WriteString("\n\n⚡ Type a prompt or /help for commands.")
	return sb.String()
}
