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
	// The question text must appear next to each answer so the conversation
	// reads clearly — not just cryptic Q1:/Q2: labels.
	if !strings.Contains(entry, "Which filter?") || !strings.Contains(entry, "Pick extras?") {
		t.Errorf("entry should include the question text, got %q", entry)
	}
	if !strings.Contains(entry, "→") {
		t.Errorf("entry should use question → answer format, got %q", entry)
	}
	if m.showAsk {
		t.Error("modal should be closed after submit")
	}
}

func TestAskNavigationDoesNotChangeSelection(t *testing.T) {
	m := newTestApp()
	m.ask = newAskBroker()
	m.askQuestions = []tool.AskQuestion{
		{Question: "Q1", Options: []string{"a", "b", "c"}, Multi: false},
	}
	m.askSel = map[int]int{0: 0} // user selected "a"
	m.askCursorPos = map[int]int{}
	m.askCursor = 0
	m.askOptionIdx = 0

	// Navigate down twice: the cursor moves, but the selection dot must stay
	// on "a" — selection only changes on Space (askToggle).
	m.askMove(1)
	if m.askOptionIdx != 1 {
		t.Errorf("cursor should move to 1 after one move, got %d", m.askOptionIdx)
	}
	if m.askSel[0] != 0 {
		t.Errorf("selection must NOT change on navigation, got %d (want 0)", m.askSel[0])
	}
	m.askMove(1)
	if m.askOptionIdx != 2 {
		t.Errorf("cursor should move to 2 after two moves, got %d", m.askOptionIdx)
	}
	if m.askSel[0] != 0 {
		t.Errorf("selection must still be 0 after navigation, got %d", m.askSel[0])
	}

	// Selecting (Space) moves the dot to the cursor position.
	m.askToggle()
	if m.askSel[0] != 2 {
		t.Errorf("selection should follow cursor after toggle, got %d", m.askSel[0])
	}
}

func TestAskNextQuestionRemembersCursor(t *testing.T) {
	m := newTestApp()
	m.ask = newAskBroker()
	m.askQuestions = []tool.AskQuestion{
		{Question: "Q1", Options: []string{"a", "b", "c"}, Multi: false},
		{Question: "Q2", Options: []string{"x", "y"}, Multi: false},
	}
	m.askCursorPos = map[int]int{}
	m.askCursor = 0
	m.askOptionIdx = 2 // cursor on "c" in Q1

	// Tab to Q2: cursor resets to that question's remembered position (0).
	m.askNextQuestion(1)
	if m.askCursor != 1 || m.askOptionIdx != 0 {
		t.Errorf("after tab to Q2: cursor=%d opt=%d, want 1,0", m.askCursor, m.askOptionIdx)
	}
	m.askOptionIdx = 1
	// Tab back to Q1: cursor should return to its remembered position (2).
	m.askNextQuestion(-1)
	if m.askCursor != 0 || m.askOptionIdx != 2 {
		t.Errorf("after tab back to Q1: cursor=%d opt=%d, want 0,2", m.askCursor, m.askOptionIdx)
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
