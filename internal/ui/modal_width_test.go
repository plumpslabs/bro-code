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

// TestModelsModalSlidingWindowAndWrapAround verifies that the models selector
// stays within bounded vertical height via sliding window and wraps around.
func TestModelsModalSlidingWindowAndWrapAround(t *testing.T) {
	m := newTestApp()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.showModels = true

	items := m.getModelList()
	if len(items) == 0 {
		t.Skip("no models available in test app")
	}

	// 1. Up from 0 should wrap around to bottom
	m.modelsSel = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.modelsSel != len(items)-1 {
		t.Errorf("expected wrap-around to bottom (%d), got %d", len(items)-1, m.modelsSel)
	}

	// 2. Down from bottom should wrap around to top
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.modelsSel != 0 {
		t.Errorf("expected wrap-around to top (0), got %d", m.modelsSel)
	}

	// 3. Sliding window renders bounded lines (not overflowing 24 terminal height)
	rendered := m.renderModelsModal()
	lineCount := len(strings.Split(rendered, "\n"))
	if lineCount > 24 {
		t.Errorf("models modal height %d lines > 24 terminal height", lineCount)
	}
}

// TestModelsModalLiveFilter verifies typing filters model list in real time.
func TestModelsModalLiveFilter(t *testing.T) {
	m := newTestApp()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.showModels = true

	// Type 'g', 'e', 'm'
	m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
	m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	m.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})

	if m.modelsQuery != "gem" {
		t.Errorf("expected modelsQuery 'gem', got %q", m.modelsQuery)
	}

	filtered := m.getModelList()
	for _, item := range filtered {
		if !strings.Contains(strings.ToLower(item.ModelName), "gem") && !strings.Contains(strings.ToLower(item.ProviderID), "gem") {
			t.Errorf("unexpected model %q under filter 'gem'", item.ModelName)
		}
	}

	// Backspace removes last character
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.modelsQuery != "ge" {
		t.Errorf("expected modelsQuery 'ge', got %q", m.modelsQuery)
	}
}

