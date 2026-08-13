package ui

import (
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/tool"
)

func TestSubmitAskAppendsToHistory(t *testing.T) {
	m := newTestApp()
	m.ask = newAskBroker()
	m.askQuestions = []tool.AskQuestion{
		{Question: "Which filter?", Options: []string{"Semua", "Sedang ditangani"}, Multi: false},
		{Question: "Pick extras?", Options: []string{"A", "B"}, Multi: true},
	}
	m.askSel = map[int]int{0: 1}
	m.askChecked = map[int]map[int]bool{1: {0: true}}
	m.askID = "test-1"

	before := len(m.messages)
	m.submitAsk()

	if len(m.messages) != before+1 {
		t.Fatalf("history not appended: %d -> %d messages", before, len(m.messages))
	}
	entry := m.messages[len(m.messages)-1]
	if !strings.HasPrefix(entry, "YOU:") {
		t.Errorf("entry should be a YOU message, got %q", entry)
	}
	if !strings.Contains(entry, "Sedang ditangani") || !strings.Contains(entry, "A") {
		t.Errorf("entry should contain the selected answers, got %q", entry)
	}
	if m.showAsk {
		t.Error("modal should be closed after submit")
	}
}

func TestAskAnsweredCount(t *testing.T) {
	m := newTestApp()
	m.askQuestions = []tool.AskQuestion{
		{Question: "Q1", Options: []string{"a", "b"}, Multi: false},
		{Question: "Q2", Options: []string{"c", "d"}, Multi: true},
		{Question: "Q3", Options: []string{"e"}, Multi: false},
	}
	m.askSel = map[int]int{0: 0}
	m.askChecked = map[int]map[int]bool{1: {1: true}}

	got := m.askAnsweredCount()
	if got != 2 {
		t.Errorf("askAnsweredCount = %d, want 2 (Q1 radio, Q2 checkbox, Q3 empty)", got)
	}
}

func TestStripTrailingWS(t *testing.T) {
	in := "line one   \nline two\t\n\nlast   "
	want := "line one\nline two\n\nlast"
	if got := stripTrailingWS(in); got != want {
		t.Errorf("stripTrailingWS = %q, want %q", got, want)
	}
}
