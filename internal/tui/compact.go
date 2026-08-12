// compact.go — Tiered context compaction (doctrine P3/P4): preventive,
// well-before-the-hard-limit folding of the transcript into structured
// L2 state ledgers so the model never hits the window ceiling.
package tui

import (
	"fmt"
	"strings"

	"github.com/plumpslabs/bro-code/internal/tokens"
)

// chatTokens forecasts the token count of the whole transcript: each message
// text via tokens.Estimate plus ~4 tokens of role overhead per message
// (OpenAI cookbook). A FORECAST, not settlement — label it in the UI.
func chatTokens(chat []chatMsg) int {
	n := 0
	for _, cm := range chat {
		n += tokens.Estimate(cm.text) + 4
	}
	return n
}

// refreshCtx recomputes the forecast context usage after any chat mutation.
func (m *Model) refreshCtx() {
	m.ctxUsed = chatTokens(m.chat)
}

// maybeCompact applies tiered auto-compaction (doctrine P4) when the forecast
// transcript exceeds compactTriggerPct of the window — preventive, well
// before the hard limit. L0 (the goal: everything up to the first user
// message) and an L1 verbatim tail (a window share, at least compactMinTail
// messages) are kept word-for-word; the middle becomes one visible, persisted
// L2 ledger message, so nothing is compacted silently and the result survives
// a -c resume. Returns true when a compaction ran.
func (m *Model) maybeCompact() bool {
	return m.compactInternal(false)
}

// forceCompact triggers compaction manually regardless of current threshold.
func (m *Model) forceCompact() bool {
	return m.compactInternal(true)
}

func (m *Model) compactInternal(force bool) bool {
	if m.window <= 0 {
		return false
	}
	minMsgs := compactMinMsgs
	if force {
		minMsgs = 3
	}
	if len(m.chat) < minMsgs {
		return false
	}
	used := chatTokens(m.chat)
	if !force && used <= int(float64(m.window)*compactTriggerPct) {
		return false
	}

	// L0 — pinned head: everything up to and including the first user
	// message (the task/goal) stays verbatim.
	headEnd := 0
	for i, cm := range m.chat {
		if cm.role == roleUser {
			headEnd = i + 1
			break
		}
	}

	// L1 — verbatim tail within a token-budget share of the window.
	tailStart := len(m.chat)
	if !force {
		tailBudget := int(float64(m.window) * compactTailPct)
		tailTokens := 0
		for i := len(m.chat) - 1; i >= headEnd; i-- {
			t := tokens.Estimate(m.chat[i].text) + 4
			if len(m.chat)-i > compactMinTail && tailTokens+t > tailBudget {
				break
			}
			tailTokens += t
			tailStart = i
		}
	} else {
		// Forced manual compaction: keep only the last turn verbatim in tail
		if len(m.chat) > headEnd+1 {
			tailStart = len(m.chat) - 1
		}
	}

	if tailStart <= headEnd {
		return false // nothing worth folding
	}

	middle := m.chat[headEnd:tailStart]
	ledger := buildLedger(middle)
	saved := chatTokens(middle) - (tokens.Estimate(ledger.text) + 4)
	if saved <= 0 && !force {
		return false
	}
	if saved < 0 {
		saved = 0
	}

	m.compactCount++
	m.compactedMsgs += len(middle)
	notice := fmt.Sprintf("🔄 context compact — %d messages → L2 state ledger · saved %s tokens", len(middle), fmtTokens(saved))
	m.trace = appendTrace(m.trace, fmt.Sprintf("● Compaction(L2 Ledger) → %d messages folded into state ledger\n  ⎿  Reclaimed %s tokens (%s free)", len(middle), fmtTokens(saved), fmtTokens(m.window-chatTokens(m.chat))))
	ledger.text = notice + "\n" + ledger.text

	newChat := make([]chatMsg, 0, headEnd+1+len(m.chat)-tailStart)
	newChat = append(newChat, m.chat[:headEnd]...)
	newChat = append(newChat, ledger)
	newChat = append(newChat, m.chat[tailStart:]...)
	m.chat = newChat
	m.status = notice
	return true
}

// buildLedger folds a run of older messages into a structured L2 ledger
// (doctrine P4: a state ledger, not lossy prose). Bounded to ledgerMaxRunes —
// summarizers self-bound their output.
func buildLedger(middle []chatMsg) chatMsg {
	var sb strings.Builder
	sb.WriteString("📋 context summary (L2 ledger)\n")
	sb.WriteString("goal: ")
	for _, cm := range middle {
		if cm.role == roleUser {
			sb.WriteString(clip(cm.text, 120))
			break
		}
	}
	sb.WriteString("\n\nuser requests:\n")
	shown := 0
	for _, cm := range middle {
		if cm.role == roleUser {
			if shown > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString("  • " + clip(cm.text, 120))
			shown++
		}
	}
	sb.WriteString("\n\nagent responses (summary):\n")
	shown = 0
	for _, cm := range middle {
		if cm.role == roleAgent && strings.TrimSpace(cm.text) != "" {
			if shown > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString("  • " + clip(firstLine(cm.text), 160))
			shown++
		}
	}
	text := sb.String()
	if r := []rune(text); len(r) > ledgerMaxRunes {
		text = string(r[:ledgerMaxRunes]) + "…"
	}
	return chatMsg{role: roleSystem, text: strings.TrimRight(text, "\n")}
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}
