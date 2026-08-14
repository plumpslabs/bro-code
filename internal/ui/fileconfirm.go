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

// fileConfirmMsg opens the input-bar confirm for a critical file action
// (create/delete). The tool layer blocks until the user answers via Answer.
type fileConfirmMsg struct {
	id   string
	kind string // "create_file" | "delete_file"
	path string
}

// fileConfirmBroker bridges the tool layer (a blocked goroutine waiting for
// approval) and the Bubble Tea UI — the same pattern as askBroker, but the
// prompt renders as a compact bar REPLACING the chat input instead of a modal.
type fileConfirmBroker struct {
	prog    *tea.Program
	mu      sync.Mutex
	pending map[string]chan tool.FileActionDecision
	seq     int64
}

func newFileConfirmBroker() *fileConfirmBroker {
	return &fileConfirmBroker{pending: make(map[string]chan tool.FileActionDecision)}
}

// Confirm presents the file action in the input bar and blocks until the user
// answers (or the context is cancelled).
func (b *fileConfirmBroker) Confirm(ctx context.Context, req tool.FileActionRequest) (tool.FileActionDecision, error) {
	id := fmt.Sprintf("fconf_%d", atomic.AddInt64(&b.seq, 1))
	ch := make(chan tool.FileActionDecision, 1)

	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	if b.prog != nil {
		b.prog.Send(fileConfirmMsg{id: id, kind: req.Kind, path: req.Path})
	}

	select {
	case dec := <-ch:
		return dec, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return tool.FileActionDecision{}, ctx.Err()
	}
}

// Answer delivers the user's decision back to the waiting tool call.
func (b *fileConfirmBroker) Answer(id string, dec tool.FileActionDecision) {
	b.mu.Lock()
	ch := b.pending[id]
	delete(b.pending, id)
	b.mu.Unlock()
	if ch != nil {
		ch <- dec
	}
}

// openFileConfirm initializes the input-bar confirm state.
func (m *Model) openFileConfirm(msg fileConfirmMsg) {
	m.showFileConfirm = true
	m.fileConfirmID = msg.id
	m.fileConfirmKind = msg.kind
	m.fileConfirmPath = msg.path
	m.fileConfirmSel = 0
	m.status = "Awaiting your confirmation..."
}

// moveFileConfirm moves the cursor within the confirm options (wraps).
func (m *Model) moveFileConfirm(delta int) {
	m.fileConfirmSel = (m.fileConfirmSel + delta + 3) % 3
}

// submitFileConfirm resolves the pending file action with the current choice:
// 0 = Allow once, 1 = Always allow (session), 2 = Discard (deny).
func (m *Model) submitFileConfirm() {
	dec := tool.FileActionDecision{}
	switch m.fileConfirmSel {
	case 1:
		dec = tool.FileActionDecision{Allow: true, Always: true}
	case 2:
		dec = tool.FileActionDecision{Allow: false}
	default:
		dec = tool.FileActionDecision{Allow: true}
	}
	id := m.fileConfirmID
	kind := m.fileConfirmKind
	path := m.fileConfirmPath
	m.showFileConfirm = false
	m.status = "Resuming..."

	// Record the decision in the history so the interaction stays visible
	// (compact one-liner, not a modal-only choice).
	verb := "✅ Allowed"
	if dec.Always {
		verb = "🔁 Always allowed for this session"
	} else if !dec.Allow {
		verb = "🚫 Discarded"
	}
	label := strings.ReplaceAll(kind, "_file", "")
	m.appendMessages(fmt.Sprintf("🗂️ %s %s %s", verb, label, path))

	if m.fileConfirm != nil {
		m.fileConfirm.Answer(id, dec)
	}
}

// discardFileConfirm cancels the confirm without answering — the pending tool
// call is denied (treated as discard).
func (m *Model) discardFileConfirm() {
	id := m.fileConfirmID
	kind := m.fileConfirmKind
	path := m.fileConfirmPath
	m.showFileConfirm = false
	m.status = "Resuming..."
	m.appendMessages(fmt.Sprintf("🚫 Discarded %s %s", strings.ReplaceAll(kind, "_file", ""), path))
	if m.fileConfirm != nil {
		m.fileConfirm.Answer(id, tool.FileActionDecision{Allow: false})
	}
}

// renderFileConfirmBar renders the compact confirmation bar that replaces the
// chat input while a critical file action awaits approval.
func (m *Model) renderFileConfirmBar() string {
	barStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("214")).Padding(0, 1)
	if m.width > 0 {
		barStyle = barStyle.Width(m.width - 2)
	}

	kindLabel := strings.ReplaceAll(m.fileConfirmKind, "_file", "")
	title := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render("⚠️ BroCode wants to " + kindLabel + " a file")
	pathStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)

	var opts strings.Builder
	labels := []string{"Allow once", "Always allow", "Discard"}
	for i, l := range labels {
		marker := "  "
		if i == m.fileConfirmSel {
			marker = "❯ "
		}
		dot := "( )"
		if i == m.fileConfirmSel {
			dot = "(●)"
		}
		opts.WriteString(fmt.Sprintf("%s%s %s  ", marker, dot, l))
	}

	return barStyle.Render(title + "\n" +
		pathStyle.Render(m.fileConfirmPath) + "\n" +
		opts.String() + "\n" +
		lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("[←/→ or 1/2/3 choose · ENTER confirm · ESC discard]"))
}
