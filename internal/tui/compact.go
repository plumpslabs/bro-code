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
// Messages that carry their payload in content (roleTool rows with empty
// text) count content, so tool output is never invisible to the forecast —
// otherwise compaction would trigger late and the window could overflow.
func chatTokens(chat []chatMsg) int {
	n := 0
	for _, cm := range chat {
		t := cm.text
		if t == "" {
			t = cm.content
		}
		n += tokens.Estimate(t) + 4
	}
	return n
}

// refreshCtx recomputes the forecast context usage after any chat mutation.
func (m *Model) refreshCtx() {
	m.ctxUsed = chatTokens(m.chat)
}

// contextPressure returns the token usage to base compaction decisions on:
// the WORSE of the local forecast (m.ctxUsed — the cached chatTokens) and
// the provider's last reported input count (actualTokens.input). The
// 4-char/token forecast systematically underestimates dense code and tool
// output — a transcript can sit at 150k real input in a 135k window while
// the forecast still reads under the 70% trigger. The API-reported number
// is settlement, not an estimate, and matches what the provider will
// actually count on the next request.
//
// HOT PATH: this runs in renderHeader/renderPanel on every frame — it must
// NOT rescan the transcript (chatTokens over up to maxHistory messages with
// tokens.Estimate each). m.ctxUsed is kept fresh by refreshCtx() after
// every chat mutation, so the cached forecast is always current.
func (m Model) contextPressure() int {
	used := m.ctxUsed
	if m.actualTokens.input > used {
		used = m.actualTokens.input
	}
	return used
}

// maybeCompact applies tiered auto-compaction (doctrine P4) when the effective
// context pressure (the worse of the local forecast and the provider's last
// reported input — see contextPressure) exceeds compactTriggerPct of the
// window — preventive, well before the hard limit. L0 (the goal: everything
// up to the first user message) and an L1 verbatim tail (a window share, at
// least compactMinTail messages) are kept word-for-word; the middle becomes
// one visible, persisted L2 ledger message, so nothing is compacted silently
// and the result survives a -c resume. Returns true when a compaction ran.
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
	// The forecast must include every message in m.chat as it stands NOW —
	// contextPressure reads the cached m.ctxUsed (per-frame hot path), so a
	// caller that appended a message without refreshing (tests, /compact on
	// a resumed session, future call sites) would otherwise see a stale,
	// too-low pressure and skip the fold. Compaction is not a hot path — a
	// full rescan here is free and makes the trigger authoritative.
	m.refreshCtx()
	// Effective pressure: the worse of the local forecast and the provider's
	// last reported input. The forecast alone let sessions drift 20k+ past
	// the window (dense code/tool output reads at ~2 chars/token, not 4).
	used := m.contextPressure()
	if !force && used <= int(float64(m.window)*m.compactTriggerPct()) {
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

	// Only fold turns that carry real conversation content. A middle of
	// transient tool/system rows would collapse into a blank ledger
	// ("goal: \n\nuser requests:\n\n\nagent responses:\n") — worse than no
	// compaction. Bail instead.
	hasContent := false
	for _, cm := range middle {
		if (cm.role == roleUser || cm.role == roleAgent) && strings.TrimSpace(cm.text) != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return false
	}

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
	// The notice is a PROCESS line, not just a result: it carries the trigger
	// pressure (the API-reported input that actually crossed the threshold)
	// and doubles as the ledger's divider header when rendered.
	notice := fmt.Sprintf("🔄 context compact — %d messages → L2 state ledger · saved %s tokens (was %s / %s)", len(middle), fmtTokens(saved), fmtTokens(used), fmtTokens(m.window))
	ledger.text = notice + "\n" + ledger.text

	newChat := make([]chatMsg, 0, headEnd+1+len(m.chat)-tailStart)
	newChat = append(newChat, m.chat[:headEnd]...)
	newChat = append(newChat, ledger)
	newChat = append(newChat, m.chat[tailStart:]...)
	m.chat = newChat
	m.status = notice

	// Keep the forecast and the header in sync with the FOLDED transcript:
	// (1) refreshCtx recomputes ctxUsed from the new (smaller) chat — the old
	// pre-compact value would otherwise linger in the top-right header;
	// (2) actualTokens was the previous response's settlement, which no longer
	// matches the shrunken transcript — drop it so the header falls back to
	// the fresh forecast instead of pinning a stale number until the next
	// reply. Both make the compact visibly reduce the ctx readout.
	m.refreshCtx()
	m.actualTokens = tokenUsage{}
	// A PROCESS trace, not a single result line: the user asked to SEE the
	// compaction happen (scanning → folding → reclaimed), not just read a
	// one-line outcome. Rendered as the "entry" under the prompt that
	// triggered it (send() preserves it on the user message).
	m.trace = appendTrace(m.trace, fmt.Sprintf(
		"● Compaction → transcript at %s / %s (%d%%)\n  ⎿  Folding %d middle messages into L2 state ledger…\n  ⎿  Reclaimed %s tokens · %s free now",
		fmtTokens(used), fmtTokens(m.window), int(float64(used)*100/float64(m.window)),
		len(middle), fmtTokens(saved), fmtTokens(m.window-m.contextPressure())))
	return true
}

// compactTriggerPct returns the compaction trigger threshold — the
// user-configurable value from config.jsonc (compact_trigger_pct) when set,
// otherwise the tuned default (70%). Kept out of the hot path: read once per
// compaction check, never per frame.
func (m Model) compactTriggerPct() float64 {
	if pct := LoadAppConfig().CompactTriggerPct; pct > 0 && pct <= 1 {
		return pct
	}
	return compactTriggerPct
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
