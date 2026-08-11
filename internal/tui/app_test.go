package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/plumpslabs/bro-code/internal/search"
)

func newTestModel() Model {
	m := New(search.New(search.SampleCorpus()), "--- a.txt\n+++ b.txt\n@@ -1 +1 @@\n-old line\n+new line\n")
	m.width = 80
	return m
}

func TestViewRendersAllPanes(t *testing.T) {
	m := newTestModel()
	v := m.View()
	for _, want := range []string{
		"brocode — skeleton demo",
		"Myers diff",
		"bm25 search results",
		"no results",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("View() missing %q\n---\n%s", want, v)
		}
	}
}

func TestEnterRunsSearch(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("mcp")
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	if len(m2.results) == 0 {
		t.Fatal("expected results after Enter")
	}
	if m2.results[0].ID != "skill-mcp" {
		t.Fatalf("expected skill-mcp first, got %q (score %.3f)", m2.results[0].ID, m2.results[0].Score)
	}
	if v := m2.View(); !strings.Contains(v, "MCP client") {
		t.Fatalf("View() missing search result\n---\n%s", v)
	}
}

func TestResultsBounded(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("tool") // matches many docs — must be capped at maxResults
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(Model)
	if len(m2.results) > maxResults {
		t.Fatalf("results not bounded: %d > %d", len(m2.results), maxResults)
	}
}

// expectsQuit reports whether running cmd produces a tea.QuitMsg.
func expectsQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestQuitKeys(t *testing.T) {
	m := newTestModel()

	// ctrl+c must quit.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !expectsQuit(cmd) {
		t.Fatal("expected Quit for ctrl+c")
	}

	// 'q' with empty input must quit.
	q := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}
	_, cmd = m.Update(q)
	if !expectsQuit(cmd) {
		t.Fatal("expected Quit for 'q' with empty input")
	}

	// 'q' with non-empty input is a query character, NOT quit.
	m.input.SetValue("query")
	_, cmd = m.Update(q)
	if expectsQuit(cmd) {
		t.Fatal("'q' must not quit while typing a query")
	}

	// Any other key must not quit (input is forwarded to textinput).
	other := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}}
	_, cmd = m.Update(other)
	if expectsQuit(cmd) {
		t.Fatal("unexpected Quit for normal key")
	}
}
