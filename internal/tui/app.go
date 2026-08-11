// Package tui is the demo UI (Bubble Tea) for the skeleton.
//
// Anti-lag rules (docs/TECH_STACK.md §2) applied here:
//   - No per-token rendering (no streaming yet — results are set once per
//     Enter, so there is no reason for a refresh loop).
//   - The diff pane is capped to a fixed number of rows (bounded rendering,
//     Principle 1).
//   - Search results are capped at 10 (bounded).
//
// When LLM token streaming arrives: buffer into a channel + ticker capped at
// ~30–60fps, NEVER send one tea.Msg per token.
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/plumpslabs/bro-code/internal/search"
)

// Render bounds — bounded at the point of creation (Principle 1).
const (
	maxResults  = 10
	maxDiffRows = 8
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212")).
			Padding(0, 1)
	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)
	diffStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	addStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	delStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	scoreStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

// Model is the TUI state (Elm architecture: Init/Update/View).
type Model struct {
	index    *search.Index
	input    textinput.Model
	results  []search.Result
	diffText string
	width    int
	status   string
}

// New creates a model with a prebuilt index and precomputed diff text.
func New(index *search.Index, diffText string) Model {
	ti := textinput.New()
	ti.Placeholder = "search tools/skills… (try: mcp, diff, memory)"
	ti.Focus()
	return Model{
		index:    index,
		input:    ti,
		diffText: diffText,
		status:   "type a query and press Enter • q / ctrl+c to quit",
	}
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			// 'q' only quits when the input is empty — so typing a query
			// that contains the letter q does not quit the app.
			if m.input.Value() == "" {
				return m, tea.Quit
			}
		case "enter":
			// One search per Enter — results capped at maxResults.
			m.results = m.index.Search(strings.TrimSpace(m.input.Value()), maxResults)
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) View() string {
	if m.width == 0 {
		m.width = 80
	}

	var sb strings.Builder
	sb.WriteString(titleStyle.Render("brocode — skeleton demo"))
	sb.WriteString("\n\n")

	// Content width — guard against negative values in small windows.
	contentW := m.width - 4
	if contentW < 0 {
		contentW = 0
	}

	// Diff pane — bounded: only the first maxDiffRows lines are rendered.
	sb.WriteString(boxStyle.Render("Myers diff (hunk)"))
	sb.WriteString("\n")
	sb.WriteString(boxStyle.Width(contentW).Render(renderDiff(m.diffText, maxDiffRows)))
	sb.WriteString("\n\n")

	// Search results pane — bounded: maxResults.
	sb.WriteString(boxStyle.Render("bm25 search results"))
	sb.WriteString("\n")
	sb.WriteString(boxStyle.Width(contentW).Render(renderResults(m.results)))
	sb.WriteString("\n\n")

	sb.WriteString(m.input.View())
	sb.WriteString("\n")
	sb.WriteString(statusStyle.Render(m.status))

	return sb.String()
}

// renderDiff colors +/- lines and truncates to maxRows. Rendering is
// precomputed (not per-frame) — anti-lag rule: never re-style in View().
func renderDiff(text string, maxRows int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > maxRows {
		lines = lines[:maxRows]
		lines = append(lines, statusStyle.Render("… (truncated)"))
	}
	var sb strings.Builder
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "+"):
			sb.WriteString(addStyle.Render(line))
		case strings.HasPrefix(line, "-"):
			sb.WriteString(delStyle.Render(line))
		case strings.HasPrefix(line, "@@"):
			sb.WriteString(titleStyle.Render(line))
		default:
			sb.WriteString(diffStyle.Render(line))
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func renderResults(results []search.Result) string {
	if len(results) == 0 {
		return "no results — type a query below and press Enter."
	}
	var sb strings.Builder
	for _, r := range results {
		sb.WriteString(scoreStyle.Render(trimScore(r.Score)))
		sb.WriteString("  ")
		sb.WriteString(r.Title)
		sb.WriteString(" (")
		sb.WriteString(diffStyle.Render(r.ID))
		sb.WriteString(")\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func trimScore(s float64) string {
	return fmt.Sprintf("%.2f", s)
}
