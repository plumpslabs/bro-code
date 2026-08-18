package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/provider"

	tea "charm.land/bubbletea/v2"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// maxLineWidth returns the widest visible line (ANSI stripped) in s.
func maxLineWidth(s string) int {
	max := 0
	for _, line := range strings.Split(s, "\n") {
		w := len([]rune(ansiRe.ReplaceAllString(line, "")))
		if w > max {
			max = w
		}
	}
	return max
}

// TestConnectModalNoOverflow proves the connect wizard box never exceeds the
// terminal width on any step, even with a long (unwrappable) API key typed.
// Regression: the box auto-sized to content while the focused textinput
// renders Width+1 (cursor column), pushing the border past the terminal edge.
func TestConnectModalNoOverflow(t *testing.T) {
	longKey := "sk-" + strings.Repeat("abcdef123456", 6) // 84 chars, no spaces

	steps := []struct {
		step   int
		custom bool
	}{
		{0, false},
		{1, false}, // built-in API key
		{1, true},  // custom name
		{2, true},  // custom API key
		{3, true},  // base URL
		{4, true},  // models textarea
	}
	for _, st := range steps {
		m := newTestApp()
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		m.showConnect = true
		m.connectStep = st.step
		m.connectCustom = st.custom
		m.connectProviderSel = 0
		m.connectTextInput.Focus()
		m.connectTextInput.SetValue(longKey)
		out := m.renderConnectModal()
		if w := maxLineWidth(out); w > 100 {
			t.Errorf("step %d custom=%v: box width %d > 100", st.step, st.custom, w)
		}
	}

	// Narrow terminal too (60 cols) — inputs must shrink, not overflow.
	m := newTestApp()
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	m.showConnect = true
	m.connectStep = 1
	m.connectCustom = false
	m.connectTextInput.Focus()
	m.connectTextInput.SetValue(longKey)
	if w := maxLineWidth(m.renderConnectModal()); w > 60 {
		t.Errorf("narrow terminal: box width %d > 60", w)
	}
}

// TestAskModalNoOverflow proves the ask modal box fits the terminal too.
func TestAskModalNoOverflow(t *testing.T) {
	m := newTestApp()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.showAsk = true
	m.askQuestions = []provider.AskQuestion{
		{Question: "Where should we start? (this is a long question that wraps across multiple lines)", Options: []string{"Debug existing bug", "Add a new feature that has a very long description to test wrapping", "Review code"}, Multi: true},
		{Question: "Which area?", Options: []string{"Backend", "Frontend"}, Multi: false},
	}
	m.refreshAskModal()
	out := m.renderAskModal()
	if w := maxLineWidth(out); w > 100 {
		t.Errorf("ask modal: box width %d > 100\n%s", w, out)
	}

	// Custom answer input open.
	m.askCustomQ = 0
	m.askCustomInput.Focus()
	m.askCustomInput.SetValue("custom long answer here")
	m.refreshAskModal()
	out = m.renderAskModal()
	if w := maxLineWidth(out); w > 100 {
		t.Errorf("ask modal (custom): box width %d > 100\n%s", w, out)
	}
}
