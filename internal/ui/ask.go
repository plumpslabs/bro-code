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
	m.askFlat = 0
	m.askChecked = map[int]map[int]bool{}
	m.askSel = map[int]int{}       // real selections only (set on Space)
	m.askCursorPos = map[int]int{} // per-question cursor memory (navigation)
	m.askCustom = map[int]string{}
	m.askCustomQ = -1
	m.askCustomInput.SetValue("")
	m.status = "Waiting for input..."
	m.askApplyFlat()
	m.refreshAskModal()
}

// askSubmitRowIndex is the flat index of the trailing Submit row (one past the
// last question's options). It is the final row in the tabbable list.
func (m *Model) askSubmitRowIndex() int {
	n := 0
	for _, q := range m.askQuestions {
		n += askRowCount(q)
	}
	return n
}

// askTotalRows is the number of tabbable rows: every option of every question
// plus the Submit row.
func (m *Model) askTotalRows() int {
	return m.askSubmitRowIndex() + 1
}

// askOnSubmit reports whether the flat cursor is on the Submit row.
func (m *Model) askOnSubmit() bool {
	return m.askFlat == m.askSubmitRowIndex()
}

// askApplyFlat maps the flat cursor onto (question, option) coordinates so the
// rest of the renderer/collector keeps working unchanged. When the cursor sits
// on the Submit row, no option is highlighted.
func (m *Model) askApplyFlat() {
	flat := m.askFlat
	for qi := range m.askQuestions {
		rc := askRowCount(m.askQuestions[qi])
		if flat < rc {
			m.askCursor = qi
			m.askOptionIdx = flat
			if m.askCursorPos != nil {
				m.askCursorPos[qi] = flat
			}
			return
		}
		flat -= rc
	}
	// On the Submit row: keep a valid question context, no option highlighted.
	if len(m.askQuestions) > 0 {
		m.askCursor = len(m.askQuestions) - 1
		m.askOptionIdx = -1
	}
}

// askMoveRow moves the flat cursor by delta rows, wrapping around the whole
// list (all options of all questions + Submit). Tab / ↓ go forward, Shift+Tab
// / ↑ go back — so the user can tab straight through every choice and also go
// backwards, exactly like a native selector.
func (m *Model) askMoveRow(delta int) {
	if len(m.askQuestions) == 0 {
		return
	}
	total := m.askTotalRows()
	m.askFlat = (m.askFlat + delta + total) % total
	m.askApplyFlat()
	m.refreshAskModal()
}

// askSelectQuickOption handles pressing 1-9 to select that option on the current question.
func (m *Model) askSelectQuickOption(num int) {
	if m.askCursor < 0 || m.askCursor >= len(m.askQuestions) {
		return
	}
	q := m.askQuestions[m.askCursor]
	targetIdx := num - 1
	if targetIdx < 0 || targetIdx > len(q.Options) {
		return
	}

	// Calculate target flat index
	flat := 0
	for i := 0; i < m.askCursor; i++ {
		flat += askRowCount(m.askQuestions[i])
	}
	flat += targetIdx

	m.askFlat = flat
	m.askApplyFlat()
	m.askToggle()
}

// refreshAskModal rebuilds the modal's scrollable content after any state
// change so it never overflows the terminal.
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
	cw := w - 30
	if cw < 10 {
		cw = 10
	}
	m.askCustomInput.SetWidth(cw)
	if m.askCustomQ >= 0 {
		m.askViewport.GotoBottom()
	} else {
		m.askViewport.GotoTop()
	}
}

// askMove moves the option cursor (↑/↓) — flat row move.
func (m *Model) askMove(delta int) {
	m.askMoveRow(delta)
}

// askNextQuestion (Tab / Shift+Tab) moves focus to the next/previous row.
func (m *Model) askNextQuestion(delta int) {
	m.askMoveRow(delta)
}

// askToggle selects/toggles the option under the flat cursor.
func (m *Model) askToggle() {
	if m.askOnSubmit() {
		return
	}
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
	if m.turnRunning {
		m.status = "Thinking..."
	} else {
		m.status = "Ready"
	}

	// Persist the answers into conversation history as a YOU entry.
	m.appendAskToHistory(results)

	if m.ask != nil {
		m.ask.Answer(id, results)
	}
}

// skipAsk dismisses the question modal without selecting answers.
func (m *Model) skipAsk() {
	id := m.askID
	m.showAsk = false
	if m.turnRunning {
		m.status = "Thinking..."
	} else {
		m.status = "Ready"
	}
	if m.ask != nil {
		m.ask.Answer(id, nil)
	}
}

// extractCommandFromQuestion extracts the inner code command from a gated question if present.
func extractCommandFromQuestion(q string) (string, bool) {
	if !strings.Contains(q, "```") {
		return "", false
	}
	parts := strings.Split(q, "```")
	if len(parts) >= 3 {
		cmd := strings.TrimSpace(parts[1])
		lines := strings.Split(cmd, "\n")
		if len(lines) > 0 {
			first := strings.TrimSpace(lines[0])
			if first == "bash" || first == "sh" || first == "zsh" {
				lines = lines[1:]
			}
		}
		cleanCmd := strings.TrimSpace(strings.Join(lines, "\n"))
		if cleanCmd != "" {
			return cleanCmd, true
		}
	}
	return "", false
}

// appendAskToHistory records the user's answers into the chat transcript with clean readable formatting.
func (m *Model) appendAskToHistory(results []tool.AskResult) {
	var entries []string
	for _, r := range results {
		ans := "(none)"
		if len(r.Answers) > 0 {
			ans = strings.Join(r.Answers, ", ")
		} else if r.Custom != "" {
			ans = r.Custom
		}
		q := strings.TrimSpace(r.Question)
		if cmd, ok := extractCommandFromQuestion(q); ok {
			// Clean terminal command card without raw backticks
			entries = append(entries, fmt.Sprintf("• Command Approval: %s\n  $ %s", ans, cmd))
		} else if strings.Contains(q, "\n") {
			lines := strings.Split(q, "\n")
			firstLine := strings.TrimSpace(lines[0])
			entries = append(entries, fmt.Sprintf("• %s\n  ↳ Answer: %s", firstLine, ans))
		} else {
			entries = append(entries, fmt.Sprintf("• %s → %s", q, ans))
		}
	}
	if len(entries) > 0 {
		m.appendMessages("YOU:\n" + strings.Join(entries, "\n\n"))
	}
}

// formatQuestionBlock renders question text, markdown code blocks, and hints cleanly.
func formatQuestionBlock(text string, maxWidth int) string {
	text = strings.TrimSpace(text)
	if !strings.Contains(text, "```") {
		lines := strings.Split(text, "\n")
		var out []string
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l != "" {
				out = append(out, "  "+l)
			}
		}
		if len(out) == 0 {
			return "  " + text + "\n"
		}
		return strings.Join(out, "\n") + "\n"
	}

	var sb strings.Builder
	parts := strings.Split(text, "```")
	for i, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		if i%2 == 1 {
			// Code block: render in a clean styled box
			codeLines := strings.Split(trimmed, "\n")
			if len(codeLines) > 0 {
				first := strings.TrimSpace(codeLines[0])
				if first == "bash" || first == "sh" || first == "json" || first == "js" || first == "ts" {
					codeLines = codeLines[1:]
				}
			}
			cleanCode := strings.Join(codeLines, "\n")
			codeBox := lipgloss.NewStyle().
				Foreground(lipgloss.Color("#38bdf8")).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#475569")).
				Padding(0, 1).
				MarginLeft(2)
			if maxWidth > 12 {
				codeBox = codeBox.Width(maxWidth - 8)
			}
			sb.WriteString(codeBox.Render(cleanCode) + "\n")
		} else {
			// Plain text or hint outside code block
			lines := strings.Split(trimmed, "\n")
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if l == "" {
					continue
				}
				if strings.HasPrefix(l, "💡") {
					hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#94a3b8")).Italic(true)
					sb.WriteString("  " + hintStyle.Render(l) + "\n")
				} else {
					qStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#f8fafc"))
					sb.WriteString("  " + qStyle.Render(l) + "\n")
				}
			}
		}
	}
	return sb.String()
}

// buildAskBody renders the scrollable content of the question modal cleanly without noisy emojis.
func (m *Model) buildAskBody() string {
	var sb strings.Builder

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#38bdf8"))
	cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#38bdf8")).Bold(true)
	checkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#34d399")).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b"))
	shortcutStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#94a3b8"))
	badgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#94a3b8")).Background(lipgloss.Color("#1e293b")).Padding(0, 1)

	sb.WriteString(titleStyle.Render("Clarification Required") + " " + dimStyle.Render(fmt.Sprintf("(%d questions)", len(m.askQuestions))) + "\n\n")

	row := 0 // running flat index across all option rows
	onSubmit := m.askOnSubmit()
	for qi, q := range m.askQuestions {
		tag := "single-select"
		if q.Multi {
			tag = "multi-select"
		}
		qTag := badgeStyle.Render(tag)
		qNum := fmt.Sprintf("Question %d/%d", qi+1, len(m.askQuestions))

		if !onSubmit && qi == m.askCursor {
			sb.WriteString(cursorStyle.Render("▸ "+qNum) + "  " + qTag + "\n")
		} else {
			sb.WriteString("  " + titleStyle.Render(qNum) + "  " + qTag + "\n")
		}
		sb.WriteString(formatQuestionBlock(q.Question, m.width-6) + "\n")

		for oi, opt := range q.Options {
			flatHere := row
			row++
			cursor := "  "
			if !onSubmit && flatHere == m.askFlat {
				cursor = cursorStyle.Render("▸ ")
			}
			sc := ""
			if qi == m.askCursor && oi < 9 {
				sc = shortcutStyle.Render(fmt.Sprintf("[%d] ", oi+1))
			}
			if q.Multi {
				mark := dimStyle.Render("[ ]")
				if m.askChecked[qi][oi] {
					mark = checkStyle.Render("[✓]")
				}
				sb.WriteString(fmt.Sprintf("%s%s%s %s\n", cursor, sc, mark, opt))
			} else {
				mark := dimStyle.Render("( )")
				if m.askSel[qi] == oi {
					mark = checkStyle.Render("(●)")
				}
				sb.WriteString(fmt.Sprintf("%s%s%s %s\n", cursor, sc, mark, opt))
			}
		}

		// Custom answer row
		customIdx := len(q.Options)
		flatHere := row
		row++
		cursor := "  "
		if !onSubmit && flatHere == m.askFlat {
			cursor = cursorStyle.Render("▸ ")
		}
		sc := ""
		if qi == m.askCursor && customIdx < 9 {
			sc = shortcutStyle.Render(fmt.Sprintf("[%d] ", customIdx+1))
		}
		if q.Multi {
			mark := dimStyle.Render("[ ]")
			if m.askChecked[qi][customIdx] {
				mark = checkStyle.Render("[✓]")
			}
			sb.WriteString(fmt.Sprintf("%s%s%s ✏️  Custom answer...\n", cursor, sc, mark))
		} else {
			mark := dimStyle.Render("( )")
			if m.askSel[qi] == customIdx {
				mark = checkStyle.Render("(●)")
			}
			sb.WriteString(fmt.Sprintf("%s%s%s ✏️  Custom answer...\n", cursor, sc, mark))
		}
		sb.WriteString("\n")
	}

	if m.askCustomQ >= 0 {
		sb.WriteString(fmt.Sprintf("Custom answer for Q%d: %s\n", m.askCustomQ+1, m.askCustomInput.View()))
		sb.WriteString(dimStyle.Render("[Type answer · Enter save · Esc back]"))
	} else {
		sb.WriteString("\n")
		answered := m.askAnsweredCount()
		btn := fmt.Sprintf(" [ Submit Answers (%d/%d) ] ", answered, len(m.askQuestions))
		btnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#090d13")).Background(lipgloss.Color("#38bdf8")).Padding(0, 1)
		dimBtnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b")).Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("#334155")).Padding(0, 1)
		if onSubmit {
			sb.WriteString(cursorStyle.Render("▸ ") + btnStyle.Render(btn) + "\n")
		} else if answered == len(m.askQuestions) && len(m.askQuestions) > 0 {
			sb.WriteString("  " + btnStyle.Render(btn) + "\n")
		} else {
			sb.WriteString("  " + dimBtnStyle.Render(btn) + "\n")
		}
		sb.WriteString("\n" + dimStyle.Render("[1-9 Quick pick · Space Toggle · Tab/↑/↓ Navigate · Enter Submit · Esc Skip]"))
	}

	return sb.String()
}

// askAnsweredCount returns how many questions have at least one selection.
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

// renderAskModal renders the interactive question modal cleanly with rounded borders.
func (m Model) renderAskModal() string {
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#38bdf8")).
		Padding(1, 2)
	if m.width > 0 {
		style = style.Width(m.width - 4)
	}
	return style.Render(m.askViewport.View())
}
