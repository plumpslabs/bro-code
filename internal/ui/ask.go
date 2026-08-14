package ui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// askUserMsg is sent by the ask broker to open the interactive question modal.
type askUserMsg struct {
	id        string
	questions []tool.AskQuestion
}

// askBroker bridges the tool layer (a blocked goroutine waiting for input)
// and the Bubble Tea UI. Ask() presents the questions and blocks until the
// user answers; the UI calls Answer() to deliver the results back.
type askBroker struct {
	prog    *tea.Program
	mu      sync.Mutex
	pending map[string]chan []tool.AskResult
	seq     int64
}

func newAskBroker() *askBroker {
	return &askBroker{pending: make(map[string]chan []tool.AskResult)}
}

// Ask presents the questions in the TUI and blocks until the user answers
// (or the context is cancelled).
func (b *askBroker) Ask(ctx context.Context, questions []tool.AskQuestion) ([]tool.AskResult, error) {
	id := fmt.Sprintf("ask_%d", atomic.AddInt64(&b.seq, 1))
	ch := make(chan []tool.AskResult, 1)

	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	if b.prog != nil {
		b.prog.Send(askUserMsg{id: id, questions: questions})
	}

	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Answer delivers the collected answers back to the waiting tool call.
func (b *askBroker) Answer(id string, results []tool.AskResult) {
	b.mu.Lock()
	ch := b.pending[id]
	delete(b.pending, id)
	b.mu.Unlock()
	if ch != nil {
		ch <- results
	}
}

// askRowCount returns the number of interactive rows for a question
// (its options plus the trailing custom-answer row).
func askRowCount(q tool.AskQuestion) int {
	return len(q.Options) + 1
}

// openAsk initializes the modal state for a new ask_user call.
func (m *Model) openAsk(msg askUserMsg) {
	m.showAsk = true
	m.askID = msg.id
	m.askQuestions = msg.questions
	m.askCursor = 0
	m.askOptionIdx = 0
	m.askChecked = map[int]map[int]bool{}
	m.askSel = map[int]int{}       // real selections only (set on Space)
	m.askCursorPos = map[int]int{} // per-question cursor memory (navigation)
	m.askCustom = map[int]string{}
	m.askCustomQ = -1
	m.askCustomInput.SetValue("")
	m.status = "Waiting for your input..."
	m.refreshAskModal()
}

// refreshAskModal rebuilds the modal's scrollable content after any state
// change so it never overflows the terminal (which caused the glitchy
// flicker when a modal had more options than lines on screen).
func (m *Model) refreshAskModal() {
	body := m.buildAskBody()
	m.askViewport.SetContent(body)
	h := m.height - 4
	if h < 6 {
		h = 6
	}
	m.askViewport.SetHeight(h)
	w := m.width - 8
	if w < 10 {
		w = 10
	}
	m.askViewport.SetWidth(w)
	// The custom answer input must be wide enough to actually show what the
	// user types (a zero-width textinput renders nothing visible).
	cw := w - 30
	if cw < 10 {
		cw = 10
	}
	m.askCustomInput.SetWidth(cw)
	// When the custom answer input is open it lives at the bottom of the
	// body, so anchor there; otherwise show the questions from the top.
	if m.askCustomQ >= 0 {
		m.askViewport.GotoBottom()
	} else {
		m.askViewport.GotoTop()
	}
}

// askMove moves the option cursor within the current question (wraps around).
// Navigation only moves the cursor — the selection dot (●) stays put until the
// user actually selects with Space.
func (m *Model) askMove(delta int) {
	if m.askCursor < 0 || m.askCursor >= len(m.askQuestions) {
		return
	}
	q := m.askQuestions[m.askCursor]
	m.askCursorPos[m.askCursor] = m.askOptionIdx
	n := askRowCount(q)
	m.askOptionIdx = (m.askOptionIdx + delta + n) % n
	m.refreshAskModal()
}

// askNextQuestion moves focus to the previous/next question.
func (m *Model) askNextQuestion(delta int) {
	if len(m.askQuestions) == 0 {
		return
	}
	m.askCursorPos[m.askCursor] = m.askOptionIdx
	n := len(m.askQuestions)
	m.askCursor = (m.askCursor + delta + n) % n
	m.askOptionIdx = m.askCursorPos[m.askCursor]
	m.refreshAskModal()
}

// askToggle toggles the option under the cursor (checkbox for multi questions,
// radio selection for single questions). Selecting the custom row opens the
// custom answer input. Only here (and askSaveCustom) does the selection state
// change — navigation never touches it.
func (m *Model) askToggle() {
	if m.askCursor < 0 || m.askCursor >= len(m.askQuestions) {
		return
	}
	q := m.askQuestions[m.askCursor]
	customIdx := len(q.Options)
	if m.askOptionIdx == customIdx {
		m.askCustomQ = m.askCursor
		m.askCustomInput.SetValue("")
		m.askCustomInput.Focus()
		m.askCursorPos[m.askCursor] = m.askOptionIdx
		m.refreshAskModal()
		return
	}
	if q.Multi {
		if m.askChecked[m.askCursor] == nil {
			m.askChecked[m.askCursor] = map[int]bool{}
		}
		m.askChecked[m.askCursor][m.askOptionIdx] = !m.askChecked[m.askCursor][m.askOptionIdx]
	} else {
		m.askSel[m.askCursor] = m.askOptionIdx
	}
	m.refreshAskModal()
}

// askSaveCustom stores the custom answer and returns to the option list.
func (m *Model) askSaveCustom() {
	qi := m.askCustomQ
	if qi < 0 || qi >= len(m.askQuestions) {
		m.askCustomQ = -1
		return
	}
	q := m.askQuestions[qi]
	val := strings.TrimSpace(m.askCustomInput.Value())
	m.askCustom[qi] = val
	if q.Multi {
		if m.askChecked[qi] == nil {
			m.askChecked[qi] = map[int]bool{}
		}
		m.askChecked[qi][len(q.Options)] = val != ""
	} else {
		m.askSel[qi] = len(q.Options)
	}
	m.askCustomQ = -1
	m.askCustomInput.Blur()
	m.askCustomInput.SetValue("")
	m.refreshAskModal()
}

// submitAsk collects all answers and delivers them to the waiting tool call.
// The answers are also appended to the chat history as a YOU entry so the
// interaction stays visible in the conversation, not just in a transient modal.
func (m *Model) submitAsk() {
	results := make([]tool.AskResult, 0, len(m.askQuestions))
	for qi, q := range m.askQuestions {
		r := tool.AskResult{Question: q.Question}
		if q.Multi {
			for oi := range q.Options {
				if m.askChecked[qi][oi] {
					r.Answers = append(r.Answers, q.Options[oi])
				}
			}
			if m.askChecked[qi][len(q.Options)] && m.askCustom[qi] != "" {
				r.Answers = append(r.Answers, m.askCustom[qi])
			}
		} else {
			sel := m.askSel[qi]
			switch {
			case sel == len(q.Options):
				if m.askCustom[qi] != "" {
					r.Answers = []string{m.askCustom[qi]}
					r.Custom = m.askCustom[qi]
				}
			case sel >= 0 && sel < len(q.Options):
				r.Answers = []string{q.Options[sel]}
			}
		}
		results = append(results, r)
	}

	id := m.askID
	m.showAsk = false
	m.status = "Resuming..."

	// Persist the answers into the conversation history as a YOU entry so the
	// Q&A is recorded like any other chat turn.
	m.appendAskToHistory(results)

	if m.ask != nil {
		m.ask.Answer(id, results)
	}
}

// appendAskToHistory renders the submitted answers as a chat entry, showing
// the question text alongside each answer so the conversation reads clearly
// (Q1 · <question> → <answer>), not just cryptic Q1:/Q2: labels.
func (m *Model) appendAskToHistory(results []tool.AskResult) {
	if len(results) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString("YOU:\n")
	for i, r := range results {
		qText := r.Question
		if qText == "" && i < len(m.askQuestions) {
			qText = m.askQuestions[i].Question
		}
		label := ""
		if len(m.askQuestions) > 1 {
			label = fmt.Sprintf("Q%d · ", i+1)
		}
		ans := strings.Join(r.Answers, ", ")
		if ans == "" {
			ans = "(no selection)"
		}
		sb.WriteString(label + qText + " → " + ans)
		if i < len(results)-1 {
			sb.WriteString("\n")
		}
	}
	m.appendMessages(sb.String())
}

// skipAsk cancels the interaction without answers (the model proceeds with
// the most reasonable default).
func (m *Model) skipAsk() {
	id := m.askID
	m.showAsk = false
	m.status = "Resuming..."
	if m.ask != nil {
		m.ask.Answer(id, nil)
	}
}

// buildAskBody renders the scrollable content of the question modal.
func (m *Model) buildAskBody() string {
	var sb strings.Builder

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
	checkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	sb.WriteString(titleStyle.Render("🎯 BroCode Needs Your Input") + "\n\n")

	for qi, q := range m.askQuestions {
		tag := "single-select"
		if q.Multi {
			tag = "multi-select"
		}
		header := fmt.Sprintf("Q%d/%d · %s", qi+1, len(m.askQuestions), q.Question)
		if qi == m.askCursor {
			sb.WriteString(cursorStyle.Render("❯ "+header) + dimStyle.Render("  ["+tag+"]") + "\n")
		} else {
			sb.WriteString("  " + header + dimStyle.Render("  ["+tag+"]") + "\n")
		}

		for oi, opt := range q.Options {
			cursor := "  "
			if qi == m.askCursor && oi == m.askOptionIdx {
				cursor = cursorStyle.Render("❯ ")
			}
			if q.Multi {
				mark := dimStyle.Render("[ ]")
				if m.askChecked[qi][oi] {
					mark = checkStyle.Render("[✓]")
				}
				sb.WriteString(fmt.Sprintf("%s%s %s\n", cursor, mark, opt))
			} else {
				mark := dimStyle.Render("( )")
				if m.askSel[qi] == oi {
					mark = checkStyle.Render("(●)")
				}
				sb.WriteString(fmt.Sprintf("%s%s %s\n", cursor, mark, opt))
			}
		}

		// Custom answer row
		customIdx := len(q.Options)
		cursor := "  "
		if qi == m.askCursor && m.askOptionIdx == customIdx {
			cursor = cursorStyle.Render("❯ ")
		}
		if q.Multi {
			mark := dimStyle.Render("[ ]")
			if m.askChecked[qi][customIdx] {
				mark = checkStyle.Render("[✓]")
			}
			sb.WriteString(fmt.Sprintf("%s%s ✍️ Custom answer...\n", cursor, mark))
		} else {
			mark := dimStyle.Render("( )")
			if m.askSel[qi] == customIdx {
				mark = checkStyle.Render("(●)")
			}
			sb.WriteString(fmt.Sprintf("%s%s ✍️ Custom answer...\n", cursor, mark))
		}
		sb.WriteString("\n")
	}

	if m.askCustomQ >= 0 {
		sb.WriteString(fmt.Sprintf("✍️ Custom answer for Q%d: %s\n", m.askCustomQ+1, m.askCustomInput.View()))
		sb.WriteString(dimStyle.Render("[Type answer · ENTER save · ESC back]"))
	} else {
		sb.WriteString("\n")
		// Submit bar: a clear action button showing how many questions are
		// answered, so the user knows the interaction ends with an explicit
		// Send — not an accidental Enter.
		answered := m.askAnsweredCount()
		btn := " ⏎ Submit answers (" + fmt.Sprintf("%d/%d", answered, len(m.askQuestions)) + ") "
		btnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("86")).Padding(0, 1)
		dimBtnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Padding(0, 1)
		if answered == len(m.askQuestions) && len(m.askQuestions) > 0 {
			sb.WriteString(btnStyle.Render(btn) + "\n")
		} else {
			sb.WriteString(dimBtnStyle.Render(btn) + "\n")
		}
		sb.WriteString(dimStyle.Render("[↑/↓ move · Space select · Tab next question · Enter submit · ESC skip]"))
	}

	return sb.String()
}

// askAnsweredCount returns how many questions have at least one selection
// (radio choice, checked box, or custom answer) — used by the submit button.
func (m *Model) askAnsweredCount() int {
	n := 0
	for qi, q := range m.askQuestions {
		if q.Multi {
			answered := false
			for oi := range q.Options {
				if m.askChecked[qi][oi] {
					answered = true
					break
				}
			}
			if m.askChecked[qi][len(q.Options)] && m.askCustom[qi] != "" {
				answered = true
			}
			if answered {
				n++
			}
		} else {
			sel, answered := m.askSel[qi]
			if answered && ((sel >= 0 && sel < len(q.Options)) || (sel == len(q.Options) && m.askCustom[qi] != "")) {
				n++
			}
		}
	}
	return n
}

// renderAskModal renders the interactive question modal inside a scrollable
// viewport so long question sets never overflow the terminal.
func (m Model) renderAskModal() string {
	style := lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2)
	if m.width > 0 {
		style = style.Width(m.width - 4)
	}
	return style.Render(m.askViewport.View())
}
