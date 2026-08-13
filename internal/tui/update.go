// update.go — Bubble Tea Update loop and key dispatch: routes messages to the
// correct handler, dispatches key presses by focus, and submits user prompts
// to the agent provider.
package tui

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/plumpslabs/bro-code/internal/agentic"
)

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case oauthSuccessMsg:
		m.status = "antigravity authenticated ✓"
		successNotice := "✓ **Antigravity Google OAuth Login Successful!**\nGoogle Antigravity authentication session confirmed automatically from browser. You are now connected to Gemini 3.6 Flash & Gemini 3 Pro models."
		m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: successNotice})
		m.started = true
		m.refreshChat()
		return m, nil

	case tea.PasteMsg:
		if strings.TrimSpace(msg.Content) == "" {
			return m, nil
		}
		if m.apikeyOpen {
			clean := strings.ReplaceAll(strings.ReplaceAll(msg.Content, "\r\n", " "), "\n", " ")
			m.apikeyInput.SetValue(m.apikeyInput.Value() + clean)
			m.apikeyInput.CursorEnd()
			return m, nil
		}
		raw := msg.Content
		lineCount := strings.Count(raw, "\n") + 1
		if lineCount > 5 || len(raw) > 200 {
			m.pastedText = raw
			badge := fmt.Sprintf("[Pasted Snippet: %d lines · %.1f KB]", lineCount, float64(len(raw))/1024.0)
			if m.input.Value() == "" {
				m.input.SetValue(badge + " ")
			} else {
				m.input.SetValue(m.input.Value() + " " + badge + " ")
			}
			m.input.CursorEnd()
			return m, nil
		}
		clean := strings.ReplaceAll(strings.ReplaceAll(msg.Content, "\r\n", " "), "\n", " ")
		m.input.SetValue(m.input.Value() + clean)
		m.input.CursorEnd()
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseWheelMsg:
		// In a list modal (connect/models/theme/history/queue) the wheel
		// navigates the list — the same as ↑↓. In a text-entry modal (API
		// key, prompt edit) the wheel is ignored so it can never move the
		// cursor inside the input. With no modal up, the wheel scrolls the
		// chat viewport.
		if m.wheelListOpen() {
			if msg.Mouse().Button == tea.MouseWheelUp {
				return m.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
			}
			return m.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		}
		if m.modalOpen() && !(m.askOpen && !m.askCustomOpen) {
			// Text-entry modals (API key, prompt edit) ignore the wheel so it
			// can never move the cursor inside the input. While the ASK
			// popover is up (but not typing a custom answer) the wheel SCROLLS
			// the chat behind it — the user may want to re-read history before
			// deciding; the popover itself is navigated with ↑↓/space/enter.
			return m, nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.follow = m.viewport.AtBottom()
		return m, cmd

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)

	case tea.MouseMotionMsg:
		return m.handleMouseMotion(msg)

	case tea.MouseReleaseMsg:
		return m.handleMouseRelease(msg)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case zenModelsMsg:
		m.zenModels = msg.models
		m.zenModelsFetched = time.Now()
		m.zenModelsLoading = false
		if m.modelsOpen {
			if len(m.zenModels) > 0 && m.modelsSel >= len(m.zenModels) {
				m.modelsSel = len(m.zenModels) - 1
			}
			m.status = fmt.Sprintf("live free models loaded (%d) — ↑↓ to pick", len(m.zenModels))
		}
		return m, nil

	case zenModelsErrMsg:
		m.zenModelsLoading = false
		if m.modelsOpen {
			m.status = "zen models fetch failed — using static list (" + clip(msg.err.Error(), 40) + ")"
		}
		return m, nil

	case modelsRefreshedMsg:
		// The background model-cache refresh finished — re-render so the
		// /models picker (and any open modal) shows the live lists.
		m.modelsRefreshing = false
		if m.modelsOpen {
			m.status = "models refreshed"
		}
		return m, nil

	case agentTraceMsg:
		if m.agentWorking {
			if msg.phase != "" {
				m.agentStep++
				m.agentPhase = msg.phase
				m.status = fmt.Sprintf("Step %d: %s", m.agentStep, msg.phase)
			}
			if msg.line != "" {
				m.trace = appendTrace(m.trace, msg.line)
			}
			m.refreshChat()
		}
		return m, m.waitForTrace()

	case agentAskMsg:
		if msg.run != m.agentRun || !m.agentWorking || m.agentAborted || len(msg.questions) == 0 {
			return m, m.waitForAsk()
		}
		m.openAsk(msg.title, msg.questions, "")
		return m, m.waitForAsk()

	case spinner.TickMsg:
		if m.agentWorking || m.toolRunning || m.compacting {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			m.refreshChat()
			return m, cmd
		}
		return m, nil

	case compactRunMsg:
		// ESC clears m.compacting while the 1s tick is still in flight —
		// that is an explicit interrupt and must cancel the pending fold, not
		// run it anyway once the status already said "interrupted by user".
		if !m.compacting {
			return m, nil
		}
		// The visible compaction process finished — apply it now. The 1s
		// spinner window makes the compaction a seen PROCESS, not a blink.
		m.compacting = false
		if m.forceCompact() {
			m.refreshChat()
			// Land the ✂ Compaction divider in view — the fold must be SEEN,
			// not happen silently above the tail ("di atas" complaint).
			m.scrollToCompaction()
		} else {
			used := m.contextPressure()
			win := m.window
			if win <= 0 {
				win = 131072
			}
			pct := float64(used) * 100.0 / float64(win)
			m.status = fmt.Sprintf("✓ context is %.1f%% clean (%s / %s tokens used) — no compaction needed", 100.0-pct, fmtTokens(used), fmtTokens(win))
			m.refreshChat()
		}
		return m, nil

	case agentToolResultMsg:
		// Agentic tool commands finished in the background. Feed the output
		// back into the agent loop unless the run was superseded or aborted.
		m.toolRunning = false
		if msg.run != m.agentRun || m.agentAborted {
			m.agentAborted = false
			m.askPendingFeedback = "" // a dropped run must not leak stale ask answers
			return m, nil
		}
		// Ask answers waiting on the same reply land BEFORE the tool feedback
		// so the model sees them first (the ask came first in its reply).
		if m.askPendingFeedback != "" {
			msg.feedback = m.askPendingFeedback + "\n\n" + msg.feedback
			m.askPendingFeedback = ""
		}
		for _, logLine := range msg.logs {
			dup := false
			for _, existing := range m.trace {
				if existing == logLine {
					dup = true
					break
				}
			}
			if !dup {
				m.trace = appendTrace(m.trace, logLine)
			}
		}
		if msg.feedback != "" {
			if m.toolLoop >= maxToolLoops || m.taskToolRnds >= maxTaskToolLoops {
				// Safety cap: a pathological model could otherwise auto-loop
				// reply → tool → reply forever. Dropping the feedback breaks
				// the chain; a real user prompt resets the per-turn budget and
				// a finalized reply resets the per-task budget.
				m.status = fmt.Sprintf("⚠️ tool loop stopped after %d rounds — type a message to continue", maxToolLoops)
			} else {
				m.toolLoop++
				m.taskToolRnds++
				// Prepend the tool feedback to the queue so it runs immediately.
				m.queue = append([]string{"Tool Execution Output:\n" + msg.feedback}, m.queue...)
				m.status = "⚙️  Tool execution completed, continuing agent loop..."
			}
		}
		m.refreshChat()
		return m.drainQueue()

	case agentResultMsg:
		if msg.run != m.agentRun || m.agentAborted {
			m.agentAborted = false
			return m, nil
		}
		m.agentWorking = false
		m.streaming = true
		if msg.tokens.total > 0 {
			m.actualTokens = msg.tokens
		} else {
			m.actualTokens = tokenUsage{}
		}
		m.activity = prependActivity(m.activity, msg.reply.items...)
		if len(msg.reply.subagents) > 0 {
			m.subagents = append(m.subagents, msg.reply.subagents...)
			if len(m.subagents) > maxActivity {
				m.subagents = m.subagents[len(m.subagents)-maxActivity:]
			}
		}
		for idx := len(m.chat) - 1; idx >= 0; idx-- {
			if m.chat[idx].role == roleUser {
				m.chat[idx].trace = append([]string(nil), m.trace...)
				break
			}
		}
		cm := chatMsg{role: roleAgent, text: ""}
		if msg.reply.collapse != nil {
			cm.summary = msg.reply.collapse.summary
			cm.content = msg.reply.collapse.content
			cm.collapsed = true
		}
		m.streamBuf = truncate(msg.reply.text, maxReplyLen)
		m.streamCache = streamCache{} // a fresh reply starts a fresh incremental render
		m.chat = appendChat(m.chat, cm)
		// Interleaved-edit tracking for this reply: replyFullText is the
		// (truncated) text being revealed, replyMsgIdx starts at the single
		// agent message (split points append new segment indices), and the
		// edit-block bookkeeping resets for this run.
		m.replyFullText = m.streamBuf
		m.replyMsgIdx = []int{len(m.chat) - 1}
		m.revealBase = 0
		m.appliedEditSpans = nil
		// Interleaved-tool state: the reveal splits tool blocks into inline ⚙
		// cards as they pass; safe commands launch RIGHT NOW in the background
		// (launchEarlyTools) so by the time their card is revealed the
		// command is already running — tool execution is real-time inline,
		// not a batch that pops in after the reply finishes. Gated/hard
		// commands are excluded here and wait for the permission popover.
		m.revealedToolSpans = map[int]bool{}
		m.pendingGatedCards = nil
		m.pendingPermissionText = ""
		// A manual /compact pinned the view to its ✂ divider — the reply
		// streaming now continues the conversation BELOW it, so re-follow.
		if m.resumeFollowOnReply {
			m.follow = true
			m.resumeFollowOnReply = false
		}
		// The early-launch cmd IS the tool execution (runAgenticToolsCmdDeny in
		// the background). It must be returned to the runtime — dropping it
		// leaves toolRunning=true forever and the "⚙ executing tool
		// commands…" spinner stuck with no agentToolResultMsg ever arriving
		// (the end-of-reply path skips launchToolRun because toolLaunched is
		// already set). tea.Batch filters nil cmds, so this is safe when
		// launchEarlyTools found nothing to run.
		earlyCmd := m.launchEarlyTools()
		m.refreshCtx()
		if cm.collapsible() {
			m.status = "block collapsed — ctrl+o to expand"
		} else {
			m.status = "streaming reply…"
		}
		m.refreshChat()
		return m, tea.Batch(streamTickCmd(), earlyCmd)

	case streamTickMsg:
		if !m.streaming {
			return m, nil
		}
		// Adaptive reveal: the reply text is ALREADY fully received — the
		// ticker is only a display animation. A fixed 12-char/tick reveal
		// made a 10k-char reply take ~41s of fake "streaming" (the user
		// reads it as delay). Long replies now reveal fast enough to finish
		// in ~2s while short replies keep the smooth minimum pace.
		n := min(streamRevealChunk(len(m.streamBuf)), len(m.streamBuf))
		m.chat[len(m.chat)-1].text += m.streamBuf[:n]
		m.streamBuf = m.streamBuf[n:]

		// INTERLEAVED EDITS: apply complete file-write blocks as they reveal
		// and split the reply into prose segments + ✎ edit cards — an edit
		// appears INLINE where the model emitted it, not as a surprise
		// "✎ N files edited" block folded on top after the reply finishes.
		m.applyInterleavedEdits()
		// INTERLEAVED TOOLS: split completed tool blocks into inline ⚙ cards
		// at their emitted position. The commands themselves already launched
		// in launchEarlyTools (agentResultMsg) — the cards are the visible,
		// in-place trace of that execution.
		m.applyInterleavedTools()

		if len(m.streamBuf) == 0 {
			m.streaming = false
			m.status = "reply complete — try /search mcp or /diff"
			fullText := m.replyFullText
			if fullText == "" {
				fullText = m.chat[len(m.chat)-1].text
			}
			userQuery := ""
			for idx := len(m.chat) - 1; idx >= 0; idx-- {
				if m.chat[idx].role == roleUser {
					userQuery = m.chat[idx].text
					break
				}
			}
			if dynSubs, traceLogs := extractDynamicSubagents(userQuery, fullText); len(dynSubs) > 0 {
				m.subagents = dynSubs
				for _, tr := range traceLogs {
					m.trace = appendTrace(m.trace, tr)
				}
			} else {
				m.subagents = m.subagents[:0]
			}
			userQuery = ""
			for idx := len(m.chat) - 1; idx >= 0; idx-- {
				if m.chat[idx].role == roleUser {
					userQuery = m.chat[idx].text
					break
				}
			}
			// Residual edits: blocks that can't interleave (the no-:path
			// fallback that writes the whole reply's code block) apply here;
			// their cards append after the reply instead of folding on top.
			// Blocks already applied during streaming are stripped first so
			// nothing double-applies or double-reports.
			clean := stripEditSpans(fullText, m.appliedEditSpans)
			if autoLogs, edits := applyBuilderCodeBlocks(clean, userQuery, m.plannerMode); len(autoLogs) > 0 {
				for _, logLine := range autoLogs {
					m.trace = appendTrace(m.trace, logLine)
				}
				m.status = fmt.Sprintf("✓ [BroCode] auto-applied %d file change(s)", len(autoLogs))
				m.appendEditCards(edits)
				// An applied edit is PROGRESS: the next tool run compares
				// fresh instead of being flagged as a repeat of the pre-edit
				// command.
				m.toolPrevCmds = ""
				m.toolRepeat = 0
			}
			if planFile := savePlan(fullText); planFile != "" {
				m.trace = appendTrace(m.trace, "● Plan → saved to "+planFile)
			}

			// AGENTIC LOOP: check for bash/read/ask tool blocks. Extraction
			// here is cheap and pure; EXECUTION runs in a background command
			// (launchToolRun). Two interactive gates come FIRST — both pause
			// the loop until the user decides:
			//   1. ask blocks → the clarify popover replaces the input (the
			//      popover IS the execution; answers continue the loop);
			//   2. risky commands → the native permission popover
			//      (allow once / always allow / deny).
			if questions, ok := parseAskBlock(fullText); ok {
				// The popover IS the execution of the ask block — swap the raw
				// <tool_call>ask<ask_question>… XML for the compact prose now so
				// the transcript stays clean while the user answers. The reply
				// may have been split into segments by interleaved edits, so
				// compact EVERY agent segment, not just the tail.
				m.compactReplySegments()
				m.openAsk("💬 agent needs your input", questions, stripAskBlock(fullText))
				return m, nil
			}
			if indicators := toolBlockCommands(fullText); len(indicators) > 0 {
				// When launchEarlyTools already ran the safe commands in the
				// background, the remaining work is ONLY the gate: commands the
				// permission popover must decide on. Everything else skips the
				// (duplicate) launch and finalizes straight away.
				gated, hard := m.gatedCommands(fullText)
				if m.toolLaunched && len(gated) == 0 && len(hard) == 0 {
					m.compactReplySegments()
				} else if len(gated) > 0 || len(hard) > 0 {
					// Same display cleanup as the ask path — the raw ```bash
					// fences don't need to sit in the transcript while the
					// decision is pending.
					m.compactReplySegments()
					m.openPermission(gated, hard, fullText)
					return m, nil
				} else {
					return m.launchToolRun(fullText, nil)
				}
			}
			// No tool blocks — the reply is final. The task's tool-loop budget
			// resets here (a reply without tools = one unit of work done; the
			// next user prompt starts fresh). Compact each segment's display
			// text (echoed tool payloads fold into dim collapsible blocks), so
			// a model that repeats the tool result never leaves a white wall
			// of output in the transcript.
			m.taskToolRnds = 0
			m.compactReplySegments()
			for idx := len(m.chat) - 1; idx >= 0; idx-- {
				if m.chat[idx].role == roleUser {
					m.chat[idx].trace = append([]string(nil), m.trace...)
					break
				}
			}
			m.refreshCtx()
			return m.drainQueue()
		} else {
			m.status = "streaming reply…"
		}
		m.refreshChat()
		if m.streaming {
			return m, streamTickCmd()
		}
		return m, nil
	}
	return m, nil
}

// modalOpen reports whether any overlay modal is currently visible — mouse
// gestures (wheel scroll and drag-select) are suppressed while one is up so
// the popup behind the modal can never be scrolled or selected by accident.
func (m Model) modalOpen() bool {
	return m.connectOpen || m.modelsOpen || m.apikeyOpen || m.themeOpen || m.historyOpen || m.queueOpen || m.promptEditOpen || m.askOpen
}

// wheelListOpen reports whether a list-style modal (navigable with the mouse
// wheel, same as ↑↓) is currently visible. Text-entry modals (API key, prompt
// preview/edit) have no list — the wheel must not move the cursor inside them.
func (m Model) wheelListOpen() bool {
	return m.connectOpen || m.modelsOpen || m.themeOpen || m.historyOpen || m.queueOpen
}

// ---- interactive ask popover ----------------------------------------------

// openAsk initializes the clarify popover (multi-question). pendingReply is
// the tool-path hand-off: non-empty when the questions came from an ask tool
// block inside a reply (the stripped reply waits here for the answers).
func (m *Model) openAsk(title string, questions []askQuestion, pendingReply string) {
	m.askKind = askClarify
	m.askTitle = title
	if m.askTitle == "" {
		m.askTitle = "💬 agent needs your input"
	}
	m.askQuestions = questions
	m.askSel = make([][]int, len(questions))
	m.askRadio = make([]int, len(questions))
	m.askCustom = make([]string, len(questions))
	for i := range m.askRadio {
		m.askRadio[i] = -1
	}
	m.askFocus = 0
	m.askCustomOpen = false
	m.pendingAskReply = pendingReply
	m.askOpen = true
	m.input.SetValue("")
	m.input.Placeholder = "↑↓ move · space select · enter submit"
	m.status = "agent needs your input — ↑↓ · space · enter submit · esc cancel"
	m.refreshChat()
}

// openPermission opens the native permission popover for risky commands. The
// user picks allow once / always allow / deny; the decision continues the
// agentic loop via submitPermission / cancelAsk.
func (m *Model) openPermission(gated, hard []string, pendingReply string) {
	m.askKind = askPermission
	m.askTitle = "⚠ permission required"
	m.askPermCmds = gated
	m.askPermHard = hard
	m.askQuestions = []askQuestion{{
		header:   "risky command",
		question: "The agent wants to run command(s) brocode gates natively. Allow?",
		options:  []string{"allow once", "always allow", "deny"},
	}}
	m.askSel = make([][]int, 1)
	m.askRadio = []int{-1}
	m.askCustom = []string{""}
	m.askFocus = 0
	m.askCustomOpen = false
	m.pendingAskReply = pendingReply
	m.askOpen = true
	m.input.SetValue("")
	m.input.Placeholder = "↑↓ move · space select · enter submit"
	m.status = "⚠ risky command — ↑↓ · space · enter submit · esc = deny"
	m.refreshChat()
}

// askItems returns the flat focusable rows of the popover: every option of
// every question, plus each question's custom row (clarify mode only — the
// permission popover has no free-text answer).
func (m Model) askItems() []askItem {
	var items []askItem
	for qi := range m.askQuestions {
		for oi := range m.askQuestions[qi].options {
			items = append(items, askItem{kind: askItemOption, qi: qi, oi: oi})
		}
		if m.askKind == askClarify {
			items = append(items, askItem{kind: askItemCustom, qi: qi})
		}
	}
	return items
}

// moveAskFocus moves the flat popover cursor by delta, wrapping around.
func (m Model) moveAskFocus(cur, delta int) int {
	n := len(m.askItems())
	if n == 0 {
		return 0
	}
	next := (cur + delta) % n
	if next < 0 {
		next += n
	}
	return next
}

// toggleAskFocus selects the focused row: a radio option becomes the chosen
// one, a checkbox option toggles, and a custom row opens the free-text editor.
func (m *Model) toggleAskFocus() {
	items := m.askItems()
	if m.askFocus < 0 || m.askFocus >= len(items) {
		return
	}
	it := items[m.askFocus]
	switch it.kind {
	case askItemOption:
		q := &m.askQuestions[it.qi]
		if q.multiSelect {
			sel := m.askSel[it.qi]
			found := -1
			for i, v := range sel {
				if v == it.oi {
					found = i
					break
				}
			}
			if found >= 0 {
				m.askSel[it.qi] = append(sel[:found], sel[found+1:]...)
			} else {
				m.askSel[it.qi] = append(sel, it.oi)
			}
		} else {
			m.askRadio[it.qi] = it.oi
		}
	case askItemCustom:
		m.askCustomIdx = it.qi
		m.askCustomOpen = true
		m.input.SetValue("")
		m.input.Placeholder = "type your own answer… (enter save · esc cancel)"
		m.status = fmt.Sprintf("custom answer for question %d — type then enter", it.qi+1)
	}
}

// buildAskAnswers serializes the popover selections for the agent: one line
// per question ("1. Header: answer" / "2. Header: A, B" / "custom: text").
func (m Model) buildAskAnswers() string {
	var sb strings.Builder
	for i, q := range m.askQuestions {
		var parts []string
		if i < len(m.askCustom) && m.askCustom[i] != "" {
			parts = append(parts, "custom: "+m.askCustom[i])
		} else if q.multiSelect {
			for _, oi := range m.askSel[i] {
				if oi >= 0 && oi < len(q.options) {
					parts = append(parts, q.options[oi])
				}
			}
		} else if i < len(m.askRadio) && m.askRadio[i] >= 0 && m.askRadio[i] < len(q.options) {
			parts = append(parts, q.options[m.askRadio[i]])
		}
		if len(parts) == 0 {
			parts = append(parts, "(no answer)")
		}
		label := q.header
		if label == "" {
			label = clip(q.question, 48)
		}
		if label == "" {
			label = fmt.Sprintf("question %d", i+1)
		}
		sb.WriteString(fmt.Sprintf("%d. %s: %s\n", i+1, label, strings.Join(parts, ", ")))
	}
	return strings.TrimSpace(sb.String())
}

// submitAsk finalizes the popover. Permission mode → submitPermission (the
// gate decision); clarify → answers are delivered to the agent — either back
// through answerCh (goroutine path) or into the loop as feedback (tool path,
// running any remaining tool blocks first).
func (m Model) submitAsk() (Model, tea.Cmd) {
	if m.askKind == askPermission {
		return m.submitPermission()
	}
	answers := m.buildAskAnswers()
	if m.pendingAskReply != "" {
		remaining := m.pendingAskReply
		m.askOpen = false
		m.restoreInputPlaceholder()
		// The answers are injected BEFORE the tool feedback so the model gets
		// them first; the remaining tools of the same reply run as usual.
		if indicators := toolBlockCommands(remaining); len(indicators) > 0 {
			m.askPendingFeedback = "User answers:\n" + answers + "\n"
			return m.launchToolRun(remaining, nil)
		}
		// No tools in the same reply — queue the answers straight to the model.
		m.queue = append([]string{"[SYSTEM ASK RESULT]\n" + answers}, m.queue...)
		m.refreshChat()
		return m.drainQueue()
	}
	// Goroutine path: the agent is blocked on answerCh waiting for the answers.
	m.askOpen = false
	m.restoreInputPlaceholder()
	select {
	case m.answerCh <- answers:
	default:
	}
	m.status = "answers sent — agent continuing…"
	m.refreshChat()
	return m, m.waitForAsk()
}

// submitPermission applies the gate decision: allow once runs everything,
// always allow additionally seeds the session allow-list, deny (or any custom
// text) runs only the safe commands and feeds the denial back to the model.
// Hard-blocked commands (e.g. rm -rf /) are denied under EVERY decision.
func (m Model) submitPermission() (Model, tea.Cmd) {
	answer := -1
	if len(m.askQuestions) > 0 && len(m.askQuestions[0].options) > 0 {
		answer = m.askRadio[0]
		if m.askCustom[0] != "" {
			answer = -1 // a typed response — treated as deny
		}
	}
	decision := "deny"
	if answer >= 0 && answer < len(m.askQuestions[0].options) {
		decision = strings.ToLower(strings.TrimSpace(m.askQuestions[0].options[answer]))
	}
	m.askOpen = false
	m.restoreInputPlaceholder()

	hardDeny := map[string]bool{}
	for _, c := range m.askPermHard {
		if k := agentic.AllowKey(c); k != "" {
			hardDeny[k] = true
		}
	}
	gatedDeny := map[string]bool{}
	for _, c := range m.askPermCmds {
		if k := agentic.AllowKey(c); k != "" {
			gatedDeny[k] = true
		}
	}

	// When the early launch already ran the safe commands, the popover's
	// reply is the gated-only text — update the inline ⚙ cards that waited
	// on this decision so the transcript reflects what happened.
	m.markPendingToolCards(decision)
	pending := m.pendingAskReply
	if m.pendingPermissionText != "" {
		pending = m.pendingPermissionText
	}

	switch decision {
	case "always allow":
		for k := range gatedDeny {
			m.allowList[k] = true
		}
		m.status = "allowed always (this session) — running commands…"
		return m.launchToolRun(pending, hardDeny)
	case "allow once":
		m.status = "allowed once — running commands…"
		return m.launchToolRun(pending, hardDeny)
	default:
		for k := range gatedDeny {
			hardDeny[k] = true
		}
		m.status = "denied risky commands — running safe ones…"
		return m.launchToolRun(pending, hardDeny)
	}
}

// cancelAsk cancels the popover: the permission gate treats esc as deny
// (safe commands still run), clarify aborts the whole run as before.
func (m Model) cancelAsk() (Model, tea.Cmd) {
	if m.askKind == askPermission {
		deny := map[string]bool{}
		for _, c := range append(append([]string{}, m.askPermCmds...), m.askPermHard...) {
			if k := agentic.AllowKey(c); k != "" {
				deny[k] = true
			}
		}
		m.askOpen = false
		m.restoreInputPlaceholder()
		// ESC = deny. When the early launch already ran the safe commands,
		// only the gated ones remain here — mark their cards and deny them.
		m.markPendingToolCards("deny")
		pending := m.pendingAskReply
		if m.pendingPermissionText != "" {
			pending = m.pendingPermissionText
		}
		m.status = "permission denied — running safe commands…"
		return m.launchToolRun(pending, deny)
	}
	m.askOpen = false
	m.agentAborted = true
	m.agentWorking = false
	select {
	case m.answerCh <- "":
	default:
	}
	m.restoreInputPlaceholder()
	m.status = "question cancelled — interrupted"
	m.refreshChat()
	return m, nil
}

// restoreInputPlaceholder resets the chat input placeholder after a popover.
func (m *Model) restoreInputPlaceholder() {
	m.input.Placeholder = "ask brocode... (try: mcp, diff, memory) or /help"
}

// drainQueue pops the next queued prompt into the input and sends it — used
// after the queue gains an item (ask answers, tool feedback, [SYSTEM …]).
func (m Model) drainQueue() (Model, tea.Cmd) {
	if len(m.queue) == 0 {
		return m, nil
	}
	nextPrompt := m.queue[0]
	m.queue = m.queue[1:]
	m.refreshInputWidth() // queue drained → typed width changed
	m.input.SetValue(nextPrompt)
	return m.send()
}

// handleMouseClick starts a drag-select when the left button is pressed
// inside the chat viewport. The anchor and current point are stored in
// viewport content coordinates (y = absolute content line via YOffset, x =
// display column). Clicks anywhere else (header, input, status, modals) are
// ignored — the input is never disturbed by the mouse.
func (m Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if m.modalOpen() || !m.started {
		return m, nil
	}
	mm := msg.Mouse()
	if mm.Button != tea.MouseLeft {
		return m, nil
	}
	// The viewport occupies terminal rows [headerHeight, headerHeight+h).
	y := mm.Y - headerHeight
	if y < 0 || y >= m.viewport.Height() {
		return m, nil
	}
	line := m.viewport.YOffset() + y
	if line < 0 {
		return m, nil
	}
	// Pin the viewport: while the drag is active, refreshChat must not
	// auto-scroll to the bottom — the anchor was captured against this offset
	// and a mid-drag jump shifts the selection onto different text.
	m.follow = false
	m.dragSel = dragSelection{active: true, x0: mm.X, y0: line, x1: mm.X, y1: line}
	m.refreshChat()
	return m, nil
}

// handleMouseMotion extends the drag-select while the left button is held.
// The point is clamped to the viewport so dragging past the edges still
// selects to the last visible line (matching the click-side bounds).
func (m Model) handleMouseMotion(msg tea.MouseMotionMsg) (tea.Model, tea.Cmd) {
	if !m.dragSel.active || m.modalOpen() {
		return m, nil
	}
	mm := msg.Mouse()
	y := mm.Y - headerHeight
	// Auto-scroll while dragging past the viewport edges: the view scrolls and
	// the selection extends beyond the previously visible window (like a normal
	// editor). Without this a long drag "stops" at the edge and the copied text
	// silently truncates. ScrollUp/ScrollDown clamp at the content bounds.
	if y < 0 {
		m.viewport.ScrollUp(-y)
		y = 0
	} else if y >= m.viewport.Height() {
		m.viewport.ScrollDown(y - m.viewport.Height() + 1)
		y = m.viewport.Height() - 1
	}
	line := m.viewport.YOffset() + y
	m.dragSel.x1, m.dragSel.y1 = mm.X, line
	m.refreshChat()
	return m, nil
}

// handleMouseRelease finalizes a drag-select: extracts the selected rectangle
// from the viewport content and copies it to the clipboard (OSC 52 +
// pbcopy/wl-copy/xclip). A plain click (no real drag motion) copies nothing
// and leaves the status untouched. The highlight is cleared and the viewport
// re-rendered.
func (m Model) handleMouseRelease(msg tea.MouseReleaseMsg) (tea.Model, tea.Cmd) {
	if !m.dragSel.active || m.modalOpen() {
		m.dragSel = dragSelection{} // a modal may have opened mid-drag — never leave stale highlight state
		return m, nil
	}
	// Require actual motion — a plain click must never clobber the clipboard
	// with a single character.
	if m.dragSel.x0 == m.dragSel.x1 && m.dragSel.y0 == m.dragSel.y1 {
		m.dragSel = dragSelection{}
		m.refreshChat()
		return m, nil
	}
	text := m.selectedText()
	m.dragSel = dragSelection{} // clear highlight + state
	m.refreshChat()
	if text == "" {
		return m, nil
	}
	_ = copyToClipboard(text)
	m.status = fmt.Sprintf("✓ copied selection (%d chars) — Cmd+V to paste", len(text))
	return m, nil
}

// handleKey routes a key press by focus (pro-TUI rule 3: every binding checks
// focus before acting; nothing reacts twice to the same key).
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// /connect modal key handling
	if m.connectOpen {
		provs := GetProviders()
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.connectOpen = false
			m.input.Focus() // Re-focus input after modal closes
		case "up", "k":
			if m.connectSel > 0 {
				m.connectSel--
			} else {
				m.connectSel = len(provs) - 1
			}
		case "down", "j":
			if m.connectSel < len(provs)-1 {
				m.connectSel++
			} else {
				m.connectSel = 0
			}
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			if idx := int(msg.String()[0] - '1'); idx < len(provs) {
				m.connectSel = idx
			}
		case "enter":
			p := provs[m.connectSel]
			m.provider = p.name
			if m.provider == "antigravity" {
				m.selectedModel = antigravityModels[0]
				m.window = modelWindowFor(m.provider, m.selectedModel)
				SaveLastModel(m.provider, m.selectedModel)
				m.status = "antigravity login successful ✓"
				listener, err := net.Listen("tcp", "127.0.0.1:51121")
				if err != nil {
					listener, _ = net.Listen("tcp", "127.0.0.1:0")
				}
				port := "51121"
				if listener != nil {
					_, pStr, _ := net.SplitHostPort(listener.Addr().String())
					port = pStr
				}

				verifier := "h8DZVuDTMQJ1wRHylOhM3etN1Teo96cxUDL1Es3I70w"
				h := sha256.Sum256([]byte(verifier))
				challenge := base64.RawURLEncoding.EncodeToString(h[:])

				clientID := "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
				redirectURI := fmt.Sprintf("http://127.0.0.1:%s/oauth-callback", port)
				scopes := "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs"

				oauthAuthURL := fmt.Sprintf(
					"https://accounts.google.com/o/oauth2/v2/auth?response_type=code&client_id=%s&redirect_uri=%s&scope=%s&code_challenge=%s&code_challenge_method=S256&state=d8a3543345f9312a2c17a705f331e9bf&access_type=offline&prompt=consent",
					url.QueryEscape(clientID),
					url.QueryEscape(redirectURI),
					url.QueryEscape(scopes),
					url.QueryEscape(challenge),
				)

				_ = exec.Command("open", oauthAuthURL).Start()

				oauthDone := make(chan struct{}, 1)

				if listener != nil {
					go func(l net.Listener, done chan<- struct{}) {
						srv := &http.Server{}
						mux := http.NewServeMux()
						mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", "text/html; charset=utf-8")
							fmt.Fprintf(w, "<html><body style='font-family:sans-serif;text-align:center;padding:50px;'><h2>✓ Authentication Successful!</h2><p>You have successfully logged in to <b>Google Antigravity</b>.<br>You can close this tab and return to <b>brocode</b> terminal.</p><script>setTimeout(function(){window.close();}, 2000);</script></body></html>")
							done <- struct{}{}
							_ = l.Close()
						})
						srv.Handler = mux
						_ = srv.Serve(l)
					}(listener, oauthDone)
				}

				oauthMsg := strings.Join([]string{
					"🔐 **Antigravity Google OAuth PKCE Login**",
					"",
					"Opening browser for authentication...",
					"",
					"If the browser didn't open automatically, visit:",
					oauthAuthURL,
					"",
					fmt.Sprintf("Waiting for callback on http://127.0.0.1:%s/oauth-callback... (this will complete automatically)", port),
					"If the browser ends on a loopback error page, copy the full URL and paste it here.",
				}, "\n")

				m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: oauthMsg})
				m.status = "waiting for Google login..."
				m.started = true
				m.refreshChat()

				waitForOAuth := func() tea.Msg {
					<-oauthDone
					return oauthSuccessMsg{}
				}

				m.connectOpen = false
				return m, waitForOAuth
			} else if m.provider == "opencode" {
				m.selectedModel = openCodeFreeModels[0]
				m.window = modelWindowFor(m.provider, m.selectedModel)
				SaveLastModel(m.provider, m.selectedModel)
				m.status = "connected to " + p.name + " — " + fmtTokens(m.window) + " ctx window"
				m.connectOpen = false
			} else if strings.Contains(p.method, "api key") {
				m.connectOpen = false
				m.apikeyOpen = true
				m.apikeyTarget = p.name
				m.apikeyInput.SetValue("")
				m.apikeyInput.Focus()
				m.status = "enter " + p.name + " API key"
			} else {
				m.window = modelWindowFor(m.provider, m.selectedModel)
				SaveLastModel(m.provider, m.selectedModel)
				m.status = "connected to " + p.name + " — " + fmtTokens(m.window) + " ctx window"
				m.connectOpen = false
			}
		}
		return m, nil
	}

	// /models modal key handling
	if m.modelsOpen {
		all := m.allModelEntries()
		filtered := filterModelEntries(all, m.modelsQuery)
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.modelsQuery != "" {
				m.modelsQuery = ""
				m.modelsSel = 0
			} else {
				m.modelsOpen = false
			}
		case "up", "k":
			if m.modelsSel > 0 {
				m.modelsSel--
			} else if len(filtered) > 0 {
				m.modelsSel = len(filtered) - 1
			}
		case "down", "j":
			if m.modelsSel < len(filtered)-1 {
				m.modelsSel++
			} else {
				m.modelsSel = 0
			}
		case "backspace":
			if r := []rune(m.modelsQuery); len(r) > 0 {
				m.modelsQuery = string(r[:len(r)-1])
				m.modelsSel = 0
			}
		case "q":
			if m.modelsQuery == "" {
				m.modelsOpen = false
			} else {
				m.modelsQuery += "q"
				m.modelsSel = 0
			}
		case "enter":
			if len(filtered) > 0 {
				sel := m.modelsSel
				if sel < 0 || sel >= len(filtered) {
					sel = 0
				}
				e := filtered[sel]
				m.provider = e.provider
				m.selectedModel = e.model
				m.window = modelWindowFor(e.provider, e.model)
				SaveLastModel(m.provider, m.selectedModel)
				m.status = "active model set to " + e.provider + "/" + e.model
			}
			m.modelsOpen = false
			m.modelsQuery = ""
			m.input.Focus() // Re-focus input after modal closes
		default:
			if len(msg.String()) == 1 && msg.String()[0] >= 32 && msg.String()[0] != 127 {
				if m.modelsQuery == "" && msg.String()[0] >= '1' && msg.String()[0] <= '9' {
					if idx := int(msg.String()[0] - '1'); idx < len(filtered) {
						m.modelsSel = idx
					}
				} else {
					m.modelsQuery += msg.String()
					m.modelsSel = 0
				}
			}
		}
		if m.modelsQuery != "" {
			m.modelsSel = 0
		}
		return m, nil
	}

	// /queue modal key handling
	if m.queueOpen {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.queueOpen = false
			m.input.Focus() // Re-focus input after modal closes
		case "up", "k":
			if m.queueSel > 0 {
				m.queueSel--
			} else if len(m.queue) > 0 {
				m.queueSel = len(m.queue) - 1
			}
		case "down", "j":
			if m.queueSel < len(m.queue)-1 {
				m.queueSel++
			} else {
				m.queueSel = 0
			}
		case "d", "backspace":
			if len(m.queue) > 0 && m.queueSel < len(m.queue) {
				m.queue = append(m.queue[:m.queueSel], m.queue[m.queueSel+1:]...)
				if m.queueSel >= len(m.queue) && m.queueSel > 0 {
					m.queueSel--
				}
				if len(m.queue) == 0 {
					m.queueOpen = false
					m.status = "queue cleared"
				}
			}
			m.refreshInputWidth() // queue badge changed
		case "e":
			if len(m.queue) > 0 && m.queueSel < len(m.queue) {
				m.input.SetValue(m.queue[m.queueSel])
				m.input.CursorEnd()
				m.queue = append(m.queue[:m.queueSel], m.queue[m.queueSel+1:]...)
				m.queueOpen = false
				m.status = "editing queued item"
			}
			m.refreshInputWidth() // queue badge changed
		case "m":
			if m.queueSel > 0 && len(m.queue) > 1 {
				m.queue[m.queueSel-1] += " " + m.queue[m.queueSel]
				m.queue = append(m.queue[:m.queueSel], m.queue[m.queueSel+1:]...)
				m.queueSel--
				m.status = "merged queue item"
			}
			m.refreshInputWidth() // queue badge changed
		}
		return m, nil
	}

	// /theme modal key handling
	if m.themeOpen {
		names := themeNames()
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.themeOpen = false
			m.status = "theme unchanged"
		case "up", "k":
			if m.themeSel > 0 {
				m.themeSel--
			} else {
				m.themeSel = len(names) - 1
			}
		case "down", "j":
			if m.themeSel < len(names)-1 {
				m.themeSel++
			} else {
				m.themeSel = 0
			}
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			if idx := int(msg.String()[0] - '1'); idx < len(names) {
				m.themeSel = idx
			}
		case "enter":
			m.themeOpen = false
			m.input.Focus() // Re-focus input after modal closes
			return m.applyTheme(names[m.themeSel]), nil
		}
		return m, nil
	}

	// /history modal key handling
	if m.historyOpen {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.historyOpen = false
			m.input.Focus() // Re-focus input after modal closes
			m.status = "history closed"
		case "up", "k":
			if m.historySel > 0 {
				m.historySel--
			} else if len(m.sessions) > 0 {
				m.historySel = len(m.sessions) - 1
			}
		case "down", "j":
			if m.historySel < len(m.sessions)-1 {
				m.historySel++
			} else {
				m.historySel = 0
			}
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			if idx := int(msg.String()[0] - '1'); idx < len(m.sessions) {
				m.historySel = idx
			}
		case "enter":
			if len(m.sessions) > 0 && m.historySel >= 0 && m.historySel < len(m.sessions) {
				sel := m.sessions[m.historySel]
				m.historyOpen = false
				m.input.Focus() // Re-focus input after modal closes
				// Load the selected session
				if msgs, err := loadSessionFrom(sel.path); err == nil && len(msgs) > 0 {
					m.chat = appendChat(nil, msgs...)
					m.started = true
					m.promptHistory = nil
					for _, cm := range msgs {
						if cm.role == roleUser && strings.TrimSpace(cm.text) != "" {
							m.promptHistory = appendPromptHistory(m.promptHistory, cm.text)
						}
					}
					m.refreshCtx()
					m.status = fmt.Sprintf("resumed session %s — %d messages", sel.name, len(msgs))
					m.refreshChat()
				} else {
					m.status = "failed to load session: " + sel.name
				}
			}
		}
		return m, nil
	}

	// API key modal key handling
	if m.apikeyOpen {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.apikeyOpen = false
			m.input.Focus() // Re-focus input after modal closes
			m.status = "API key entry cancelled"
			m.apikeyInput.SetValue("")
			return m, nil
		case "enter":
			key := strings.TrimSpace(m.apikeyInput.Value())
			if key == "" {
				m.status = "API key cannot be empty"
				return m, nil
			}
			if err := saveAPIKey(m.apikeyTarget, key); err != nil {
				m.status = "failed to save key: " + err.Error()
			} else {
				m.status = m.apikeyTarget + " API key saved ✓"
			}
			m.apikeyInput.SetValue("")
			m.apikeyOpen = false
			m.input.Focus() // Re-focus input after modal closes
			m.provider = m.apikeyTarget
			m.window = modelWindowFor(m.provider, m.selectedModel)
			m.modelsOpen = true
			m.modelsSel = 0
			m.modelsQuery = ""
			m.connectOpen = false
			// A key was just saved — refresh BOTH the live zen list and the
			// on-disk model cache in the background (the new provider's models
			// only appear after its API is queried; never block the UI on it).
			var cmds []tea.Cmd
			if !m.zenModelsLoading && time.Since(m.zenModelsFetched) > zenModelsTTL {
				m.zenModelsLoading = true
				m.status = "fetching live free models from zen gateway…"
				cmds = append(cmds, fetchZenModelsCmd())
			}
			if modelsCacheStale() && !m.modelsRefreshing {
				m.modelsRefreshing = true
				cmds = append(cmds, modelsRefreshCmd())
			}
			if len(cmds) > 0 {
				return m, tea.Batch(cmds...)
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.apikeyInput, cmd = m.apikeyInput.Update(msg)
		return m, cmd
	}

	// Ask popover key handling — the popover REPLACES the chat input while a
	// question or permission decision is pending. ↑↓ moves across every
	// option + custom row of every question, space selects (radio: pick one,
	// checkbox: toggle), enter/tab submits, esc cancels. A focused custom row
	// opens the input for a free-text answer.
	if m.askOpen {
		// Custom-answer editor: typing goes to the (re-shown) chat input.
		if m.askCustomOpen {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.askCustomOpen = false
				m.input.SetValue("")
				m.restoreInputPlaceholder()
				m.status = "custom answer cancelled"
				m.refreshChat()
				return m, nil
			case "enter":
				m.askCustom[m.askCustomIdx] = strings.TrimSpace(m.input.Value())
				m.askCustomOpen = false
				m.input.SetValue("")
				m.restoreInputPlaceholder()
				m.status = "custom answer saved — ↑↓ · space · enter submit"
				m.refreshChat()
				return m, nil
			default:
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			return m.cancelAsk()
		case "up", "k":
			m.askFocus = m.moveAskFocus(m.askFocus, -1)
			m.refreshChat()
			return m, nil
		case "down", "j":
			m.askFocus = m.moveAskFocus(m.askFocus, 1)
			m.refreshChat()
			return m, nil
		case " ", "space":
			m.toggleAskFocus()
			m.refreshChat()
			return m, nil
		case "enter", "tab":
			return m.submitAsk()
		}
		return m, nil
	}

	// ctrl+e: long-prompt preview/edit modal (only meaningful when the input
	// is oversized — otherwise it's a no-op with a status hint). While open,
	// typing still goes to the input (live edit); esc closes.
	if m.promptEditOpen {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.promptEditOpen = false
			m.status = "prompt preview closed"
			return m, nil
		case "enter":
			m.promptEditOpen = false
			return m.send()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	if msg.String() == "ctrl+e" {
		if m.longPromptOpen() {
			m.promptEditOpen = !m.promptEditOpen
			if m.promptEditOpen {
				m.status = "prompt preview — type to edit · enter send · esc close"
			} else {
				m.status = "prompt preview closed"
			}
		} else {
			m.status = "prompt is short — ctrl+e is only for long prompts (>200 chars)"
		}
		return m, nil
	}

	// Global shift+tab shortcut toggles PLANNER vs BUILDER mode
	if msg.String() == "shift+tab" {
		m.plannerMode = !m.plannerMode
		if m.plannerMode {
			m.status = "PLANNER MODE — Brainstorm & Plan only (Strict No-Edit Guarantee)"
		} else {
			m.status = "BUILDER MODE — Real-time execution & file edits enabled"
		}
		m.refreshChat()
		return m, nil
	}

	// Global ctrl+q shortcut toggles queue manager modal
	if msg.String() == "ctrl+q" {
		if len(m.queue) == 0 {
			m.status = "queue is empty"
			return m, nil
		}
		m.queueOpen = !m.queueOpen
		m.queueSel = 0
		m.status = "queue manager"
		return m, nil
	}

	// Command suggestions key handling
	if m.suggestVisible() {
		switch msg.String() {
		case "esc":
			m.suggestDismissed = true
			m.suggestSel = 0
			return m, nil
		case "up", "k":
			if m.suggestSel > 0 {
				m.suggestSel--
			} else {
				items := suggestFiltered(m.input.Value())
				if len(items) > 0 {
					m.suggestSel = len(items) - 1
				}
			}
			return m, nil
		case "down", "j":
			if items := suggestFiltered(m.input.Value()); m.suggestSel < len(items)-1 {
				m.suggestSel++
			} else {
				m.suggestSel = 0
			}
			return m, nil
		case "tab", "enter":
			items := suggestFiltered(m.input.Value())
			if len(items) > 0 {
				sel := m.suggestSel
				if sel < 0 || sel >= len(items) {
					sel = 0
				}
				val := m.input.Value()
				if strings.HasPrefix(val, "/") {
					m.input.SetValue(items[sel].cmd)
				} else {
					words := strings.Fields(val)
					if len(words) > 0 {
						words[len(words)-1] = items[sel].cmd
						m.input.SetValue(strings.Join(words, " ") + " ")
					} else {
						m.input.SetValue(items[sel].cmd + " ")
					}
				}
				m.input.CursorEnd()
				m.suggestSel = 0
				m.suggestDismissed = true
				if msg.String() == "enter" && strings.HasPrefix(val, "/") {
					return m.send()
				}
			}
			return m, nil
		}
		m.suggestDismissed = false
	}

	// Global key bindings
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.agentWorking || m.streaming || m.toolRunning || m.compacting {
			if m.agentCancel != nil {
				m.agentCancel()
			}
			m.agentAborted = true
			m.agentWorking = false
			m.streaming = false
			m.toolRunning = false
			m.compacting = false
			m.streamBuf = ""
			m.agentPhase = ""
			m.status = "interrupted by user (ESC)"
			notice := m.styles.sys.Render("─── interrupted by user (ESC) ───")
			m.trace = appendTrace(m.trace, "● Interrupted → cancelled by user (ESC)")
			if len(m.chat) > 0 {
				lastIdx := len(m.chat) - 1
				if m.chat[lastIdx].role == roleAgent {
					if strings.TrimSpace(m.chat[lastIdx].text) == "" {
						m.chat[lastIdx].text = notice
					} else {
						m.chat[lastIdx].text = strings.TrimRight(m.chat[lastIdx].text, "\n") + "\n\n" + notice
					}
				} else {
					m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: notice})
				}
			} else {
				m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: notice})
			}
			m.refreshChat()
			if len(m.queue) > 0 {
				return m.drainQueue()
			}
			return m, nil
		}
		if m.historyIdx != -1 {
			m.historyIdx = -1
			m.input.SetValue(m.draftInput)
			m.input.CursorEnd()
			return m, nil
		}
		m.input.SetValue("")
		return m, nil
	case "ctrl+y":
		if len(m.chat) > 0 {
			var textToCopy string
			for i := len(m.chat) - 1; i >= 0; i-- {
				if m.chat[i].role == roleAgent {
					textToCopy = m.chat[i].text
					break
				}
			}
			if textToCopy == "" {
				textToCopy = m.chat[len(m.chat)-1].text
			}
			_ = copyToClipboard(textToCopy)
			m.status = "✓ copied reply to system clipboard (" + fmt.Sprintf("%d chars", len(textToCopy)) + ") — press Cmd+V to paste"
		} else {
			m.status = "nothing to copy"
		}
		return m, nil
	case "ctrl+o":
		return m.toggleCollapse(), nil
	case "enter":
		return m.send()
	case "?":
		if m.input.Value() == "" && !m.agentWorking && !m.streaming {
			m.started = true
			m.agentWorking = true
			m.agentRun++
			m.agentAborted = false
			run := m.agentRun
			m.status = "loading help…"
			return m, tea.Batch(m.spinner.Tick, func() tea.Msg {
				return agentResultMsg{reply: buildReply("/help", m.index), run: run}
			})
		}
	}

	// Arrow up/down & ctrl+p/ctrl+n: navigate prompt history
	// pgup/pgdown/ctrl+u/ctrl+d: scroll chat viewport.
	switch msg.String() {
	case "up", "ctrl+p":
		if len(m.promptHistory) > 0 {
			if m.historyIdx == -1 {
				m.draftInput = m.input.Value()
				m.historyIdx = len(m.promptHistory) - 1
			} else if m.historyIdx > 0 {
				m.historyIdx--
			}
			m.input.SetValue(m.promptHistory[m.historyIdx])
			m.input.CursorEnd()
		}
		return m, nil
	case "down", "ctrl+n":
		if m.historyIdx != -1 {
			if m.historyIdx < len(m.promptHistory)-1 {
				m.historyIdx++
				m.input.SetValue(m.promptHistory[m.historyIdx])
			} else {
				m.historyIdx = -1
				m.input.SetValue(m.draftInput)
			}
			m.input.CursorEnd()
		}
		return m, nil
	case "pgup", "shift+up":
		m.viewport.HalfPageUp()
		m.follow = m.viewport.AtBottom()
		return m, nil
	case "pgdown", "shift+down":
		m.viewport.HalfPageDown()
		m.follow = m.viewport.AtBottom()
		return m, nil
	case "ctrl+u":
		m.viewport.HalfPageUp()
		m.follow = m.viewport.AtBottom()
		return m, nil
	case "ctrl+d":
		m.viewport.HalfPageDown()
		m.follow = m.viewport.AtBottom()
		return m, nil
	}

	// Pass remaining key events to the text input for normal typing.
	if msg.String() == "backspace" || msg.Text != "" {
		m.suggestDismissed = false
		m.historyIdx = -1
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// buildUsageMessage creates a local usage message showing token usage and context stats.
func (m Model) buildUsageMessage() string {
	var sb strings.Builder
	sb.WriteString("📊 **Usage & Context**\n\n")

	// Provider & Model
	if m.provider != "" && m.selectedModel != "" {
		sb.WriteString(fmt.Sprintf("→ %s · %s\n", m.provider, m.selectedModel))
	} else if m.provider != "" {
		sb.WriteString(fmt.Sprintf("→ %s\n", m.provider))
	} else {
		sb.WriteString("→ not connected\n")
	}

	// Context window
	if m.window > 0 {
		sb.WriteString(fmt.Sprintf("• Context window: %s tokens\n", fmtTokens(m.window)))
	}

	// Token usage — effective pressure (forecast, or the provider's last
	// reported input when available). Percent is what auto-compaction fires on.
	if used := m.contextPressure(); used > 0 {
		pct := float64(used) / float64(m.window) * 100
		color := "green"
		if pct > 70 {
			color = "yellow"
		}
		if pct > 90 {
			color = "red"
		}
		prefix := "~"
		if m.actualTokens.input > 0 {
			prefix = ""
		}
		sb.WriteString(fmt.Sprintf("• Context used: %s%s tokens (%.1f%%) [%s]\n", prefix, fmtTokens(used), pct, color))
	} else {
		sb.WriteString("• Context used: ~0 tokens\n")
	}

	// Actual token usage (if available)
	if m.actualTokens.total > 0 {
		sb.WriteString(fmt.Sprintf("• Last response: %s tokens (in: %s, out: %s)\n", fmtTokens(m.actualTokens.total), fmtTokens(m.actualTokens.input), fmtTokens(m.actualTokens.output)))
		if m.actualTokens.cacheRead > 0 {
			sb.WriteString(fmt.Sprintf("• Cache: read %s, write %s\n", fmtTokens(m.actualTokens.cacheRead), fmtTokens(m.actualTokens.cacheWrite)))
		}
	}

	// Compaction stats
	sb.WriteString("\n**Compaction Tiers:**\n")
	sb.WriteString("• L0: Pinned (goal/constraints)\n")
	sb.WriteString("• L1: Verbatim tail (recent messages)\n")
	sb.WriteString("• L2: Ledger (summarized older turns)\n")
	sb.WriteString("• L3: Offload (full context window)\n")

	if m.compactCount > 0 {
		sb.WriteString(fmt.Sprintf("\n• Compactions: %d (%d messages folded)\n", m.compactCount, m.compactedMsgs))
	}

	// Trigger threshold
	trig := m.compactTriggerPct() * 100
	sb.WriteString(fmt.Sprintf("\n• Auto-compact trigger: %.0f%% of window\n", trig))
	if m.ctxUsed > 0 && m.window > 0 {
		pct := float64(m.ctxUsed) / float64(m.window) * 100
		if pct >= trig {
			sb.WriteString("  ⚠️ **Near compaction threshold!**\n")
		} else {
			sb.WriteString(fmt.Sprintf("  ✓ %.1f%% until trigger\n", trig-pct))
		}
	}

	return sb.String()
}

// buildInfoMessage creates a local info message without making any API calls.
func (m Model) buildInfoMessage() string {
	var sb strings.Builder
	sb.WriteString("📊 **Session Info**\n\n")

	// Provider & Model
	if m.provider != "" {
		sb.WriteString(fmt.Sprintf("• Provider: %s\n", m.provider))
	} else {
		sb.WriteString("• Provider: not connected\n")
	}
	if m.selectedModel != "" {
		sb.WriteString(fmt.Sprintf("• Model: %s\n", m.selectedModel))
	} else {
		sb.WriteString("• Model: not selected\n")
	}

	// Context window
	if m.window > 0 {
		sb.WriteString(fmt.Sprintf("• Context window: %s tokens\n", fmtTokens(m.window)))
	}
	if m.ctxUsed > 0 {
		sb.WriteString(fmt.Sprintf("• Context used: ~%s tokens (%.1f%%)\n", fmtTokens(m.ctxUsed), float64(m.ctxUsed)/float64(m.window)*100))
	}
	if m.actualTokens.total > 0 {
		sb.WriteString(fmt.Sprintf("• Actual tokens: %s (in: %s, out: %s)\n", fmtTokens(m.actualTokens.total), fmtTokens(m.actualTokens.input), fmtTokens(m.actualTokens.output)))
	}

	// Session
	sb.WriteString(fmt.Sprintf("• Session: %s (%s)\n", m.projectName, m.sessionID))
	sb.WriteString(fmt.Sprintf("• Messages: %d\n", len(m.chat)))

	// Compaction
	if m.compactCount > 0 {
		sb.WriteString(fmt.Sprintf("• Compactions: %d (%d messages folded)\n", m.compactCount, m.compactedMsgs))
	}

	// Git info
	git := gitInfo()
	if git.branch != "" {
		sb.WriteString(fmt.Sprintf("• Git: %s @ %s\n", git.branch, git.path))
	}

	return sb.String()
}

// showHistory opens the history modal for session selection.
func (m *Model) showHistory() {
	m.sessions = listSessions()
	if len(m.sessions) == 0 {
		m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: "No saved sessions found.\n\nSessions are auto-saved when you quit."})
		m.started = true
		m.refreshChat()
		return
	}
	m.historyOpen = true
	m.historySel = 0
	m.status = "select session to resume — ↑↓ navigate · enter select · esc cancel"
}

// renderHistoryModalBox renders the session picker modal.
// Height is capped at 15 rows; if more sessions exist, a scroll indicator shows.
func (m Model) renderHistoryModalBox() string {
	w := min(56, m.width-4)
	if w < 35 {
		w = 35
	}

	// Height budget: header(2) + footer(2) + padding(2) = 6 lines overhead
	maxSessions := m.height - 8
	if maxSessions < 3 {
		maxSessions = 3
	}
	if maxSessions > 15 {
		maxSessions = 15
	}

	var sb strings.Builder

	if len(m.sessions) == 0 {
		sb.WriteString(m.styles.statusLeft.Render("  no sessions found"))
		sb.WriteString("\n")
	} else {
		// Scroll window: show sessions around the selected one
		start := 0
		if m.historySel >= maxSessions {
			start = m.historySel - maxSessions + 2
		}
		end := start + maxSessions
		if end > len(m.sessions) {
			end = len(m.sessions)
		}

		if start > 0 {
			sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("    ... %d more above", start)))
			sb.WriteString("\n")
		}

		for i := start; i < end; i++ {
			s := m.sessions[i]
			row := fmt.Sprintf("%s · %s · %d msgs", s.name, s.time, s.msgCount)
			if i == m.historySel {
				sb.WriteString(m.styles.sideSel.Render("  ▸ " + clip(row, w-6)))
			} else {
				sb.WriteString(m.styles.statusLeft.Render("    " + clip(row, w-6)))
			}
			sb.WriteString("\n")
		}

		if end < len(m.sessions) {
			sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("    ... %d more below", len(m.sessions)-end)))
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n")
	sb.WriteString(m.styles.popoverFooter.Render("↑↓ navigate · enter resume · esc close"))

	return m.popoverFrame("session history", sb.String(), w)
}

// sessionInfo holds metadata about a saved session.
type sessionInfo struct {
	name     string // display label: <project>/<session-id>
	path     string
	time     string
	mod      int64 // raw unix mod time, for numeric (non-lexical) sorting
	msgCount int
}

// listSessions returns the saved sessions for the CURRENT project only.
//
// Real conversations are persisted per project under
// ~/.brocode/projects/<proj>/session_<base36-ms>.jsonl — one file per quit,
// retention-capped. /history is scoped to the project brocode is running in:
// running in project A never lists sessions saved in project B. The fallback
// ~/.brocode/sessions/latest.jsonl is a duplicate of the last session and is
// deliberately excluded (it lives outside the project dir).
func listSessions() []sessionInfo {
	dir, err := sessionRoot()
	if err != nil {
		return nil
	}
	files := sessionFilesIn(dir) // newest first, tie-free nanosecond ordering
	sessions := make([]sessionInfo, 0, len(files))
	for _, f := range files {
		id := strings.TrimSuffix(strings.TrimPrefix(f.name, "session_"), ".jsonl")
		sessions = append(sessions, sessionInfo{
			name:     id,
			path:     f.path,
			mod:      f.mod, // UnixNano — kept as the raw sort key
			msgCount: countSessionMessages(f.path),
		})
	}
	// Newest first — compare raw mod time, not the formatted string, so months
	// and days sort numerically.
	for i := range sessions {
		sessions[i].time = time.Unix(0, sessions[i].mod).Format("Jan 02, 15:04")
	}
	return sessions
}

// countSessionMessages counts non-empty JSONL lines in a session file.
func countSessionMessages(path string) int {
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	n := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// send submits the current input to the (mock) agent or queues it if busy.
func (m Model) send() (Model, tea.Cmd) {
	q := strings.TrimSpace(m.input.Value())
	if q == "" && m.pastedText == "" {
		if len(m.queue) > 0 && !m.agentWorking && !m.streaming && !m.toolRunning {
			return m.drainQueue()
		}
		return m, nil
	}
	if m.pastedText != "" {
		if q == "" {
			q = "Pasted context:\n```\n" + m.pastedText + "\n```"
		} else {
			q = "Pasted context:\n```\n" + m.pastedText + "\n```\n\nPrompt: " + q
		}
		m.pastedText = ""
	}

	// Handle pasted OAuth callback URL / code
	if strings.Contains(q, "oauth-callback") || strings.Contains(q, "Authentication Successful") {
		m.input.SetValue("")
		successNotice := "✓ **Antigravity OAuth Authentication Confirmed!**\nGoogle Antigravity authentication session has been successfully verified. You are now connected to Gemini 3.6 Flash & Gemini 3 Pro models."
		m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: successNotice})
		m.status = "antigravity authenticated ✓"
		m.refreshChat()
		return m, nil
	}

	// Chat-local commands handled first — no agent work involved.
	switch {
	case q == "/clear":
		m.chat = m.chat[:0]
		m.activity = m.activity[:0]
		m.input.SetValue("")
		m.started = false
		m.status = ""
		m.ctxUsed = 0
		m.actualTokens = tokenUsage{}
		m.compactCount = 0
		m.compactedMsgs = 0
		m.dragSel = dragSelection{} // content is gone — drop any in-progress drag
		// The cleared session must not be able to inject pending results:
		// drop background tool state and bump the run id so any in-flight
		// agent/tool result is discarded on arrival.
		m.toolRunning = false
		m.toolLoop = 0
		m.toolPrevCmds = ""
		m.toolRepeat = 0
		m.agentRun++
		m.streamCache = streamCache{}
		// A fresh conversation starts with ZERO model context — files attached
		// before the clear must be attachable again, or the next prompt about
		// them would be silently skipped as "already in context".
		resetAttachCache()
		m.refreshChat()
		return m, nil

	case q == "/info":
		m.input.SetValue("")
		info := m.buildInfoMessage()
		m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: info})
		m.started = true
		m.refreshChat()
		return m, nil

	case strings.HasPrefix(q, "/copy"):
		m.input.SetValue("")
		if len(m.chat) > 0 {
			var sb strings.Builder
			for _, cm := range m.chat {
				if strings.TrimSpace(cm.text) != "" {
					roleName := "user"
					switch cm.role {
					case roleAgent:
						roleName = "assistant"
					case roleSystem:
						roleName = "system"
					case roleTool:
						roleName = "tool"
					}
					sb.WriteString(fmt.Sprintf("[%s]: %s\n\n", roleName, cm.text))
				}
			}
			fullText := sb.String()
			_ = copyToClipboard(fullText)
			m.status = fmt.Sprintf("✓ copied full chat transcript (%d chars) to system clipboard — press Cmd+V to paste", len(fullText))
		} else {
			m.status = "nothing to copy"
		}
		return m, nil

	case q == "/history":
		m.input.SetValue("")
		m.showHistory()
		return m, nil

	case q == "/usage":
		m.input.SetValue("")
		usage := m.buildUsageMessage()
		m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: usage})
		m.started = true
		m.refreshChat()
		return m, nil

	case q == "/planner":
		m.input.SetValue("")
		m.plannerMode = true
		m.chat = appendChat(m.chat, chatMsg{
			role: roleSystem,
			text: "**PLANNER MODE ACTIVATED**\n\n• **Strict No-Edit Guarantee**: BroCode will NOT modify workspace files or run mutating shell commands.\n• **Goal**: Brainstorm, discuss architecture, ask clarifying questions, and build execution roadmap `brocode_plan.md`.\n• Switch back anytime with `Shift+Tab` or `/builder`.",
		})
		m.started = true
		m.status = "PLANNER MODE — Brainstorm & Plan only"
		m.refreshChat()
		return m, nil

	case q == "/builder":
		m.input.SetValue("")
		m.plannerMode = false
		m.chat = appendChat(m.chat, chatMsg{
			role: roleSystem,
			text: "**BUILDER MODE ACTIVATED**\n\n• **Execution Engine**: File edits & real-time tool execution enabled.\n• Reads `brocode_plan.md`, executes tasks, and updates progress checkboxes `[x]` upon verification.",
		})
		m.started = true
		m.status = "BUILDER MODE — Real-time execution & file edits enabled"
		m.refreshChat()
		return m, nil

	case q == "/quit":
		return m, tea.Quit
	case strings.HasPrefix(q, "/theme"):
		m.input.SetValue("")
		arg := strings.TrimSpace(strings.TrimPrefix(q, "/theme"))
		if arg == "" {
			m.themeOpen = true
			m.themeSel = themeIndex(m.themeName)
			m.status = "select a theme"
			return m, nil
		}
		return m.applyTheme(arg), nil
	case q == "/connect":
		m.input.SetValue("")
		m.connectOpen = true
		m.connectSel = 0
		m.status = "select a provider (UI only)"
		return m, nil
	case q == "/models":
		m.input.SetValue("")
		m.modelsOpen = true
		m.modelsSel = 0
		m.modelsQuery = ""
		// Opening the picker must NEVER block the UI on network: if either
		// the live zen list or the on-disk model cache is stale, refresh in
		// the background and show cached/static models meanwhile.
		var cmds []tea.Cmd
		if !m.zenModelsLoading && time.Since(m.zenModelsFetched) > zenModelsTTL {
			m.zenModelsLoading = true
			m.status = "fetching live free models from zen gateway…"
			cmds = append(cmds, fetchZenModelsCmd())
		}
		if modelsCacheStale() && !m.modelsRefreshing {
			m.modelsRefreshing = true
			cmds = append(cmds, modelsRefreshCmd())
			if m.status == "" {
				m.status = "refreshing models…"
			}
		}
		if len(cmds) > 0 {
			return m, tea.Batch(cmds...)
		}
		m.status = "select active AI model"
		return m, nil
	case q == "/queue":
		m.input.SetValue("")
		if len(m.queue) == 0 {
			m.status = "queue is empty"
			return m, nil
		}
		m.queueOpen = true
		m.queueSel = 0
		m.status = "queue manager"
		return m, nil
	case q == "/compact":
		m.input.SetValue("")
		// Run the compaction as a brief VISIBLE process (spinner + status)
		// instead of an instant zero-frame mutation. The actual folding happens
		// in the compactRunMsg handler after the short delay. When the next
		// agent reply streams in after this manual compaction, the viewport
		// must re-follow it (the compaction pinned the view to show the
		// divider — the conversation continues BELOW it).
		m.resumeFollowOnReply = true
		m.compacting = true
		m.status = "🔄 compacting context…"
		return m, tea.Batch(m.spinner.Tick,
			tea.Tick(time.Second, func(time.Time) tea.Msg { return compactRunMsg{} }))
	}

	// If agent is working, streaming, or running background tool commands,
	// queue the message for auto-flush (keeps the agentic loop ordered).
	if m.agentWorking || m.streaming || m.toolRunning {
		m.queue = append(m.queue, q)
		m.input.SetValue("")
		m.refreshInputWidth() // queue badge appeared → typed width changed
		m.status = fmt.Sprintf("queued (%d) · [ctrl+q] manage queue", len(m.queue))
		return m, nil
	}

	m.started = true
	var attachedLogs []string
	var msg chatMsg
	if strings.HasPrefix(q, "[SYSTEM TOOL RESULT]") || strings.HasPrefix(q, "Tool Execution Output:") || strings.HasPrefix(q, "[SYSTEM ASK RESULT]") {
		// Agentic tool results AND ask answers are transient system events —
		// they get a distinct role (roleTool) so they render as a dim row,
		// never as a user message with the blue bar. They are also not added
		// to prompt history (the content is not something the user typed and
		// should never resurface via ↑).
		summary := "Tool Executed"
		if strings.HasPrefix(q, "[SYSTEM ASK RESULT]") {
			summary = "User Answered" // icon is owned by renderToolMsg — no ⚙ here
		}
		msg = chatMsg{
			role:      roleTool,
			summary:   summary,
			content:   q,
			collapsed: true,
			trace:     attachedLogs,
		}
	} else {
		msg = chatMsg{role: roleUser, text: q, trace: attachedLogs}
	}
	m.chat = appendChat(m.chat, msg)
	// The forecast must include the just-appended message before the trigger
	// check below — contextPressure reads the CACHED m.ctxUsed (hot path),
	// so the new message has to be folded into the estimate here.
	m.refreshCtx()
	// Capture the compaction process trace BEFORE m.trace is reset below —
	// compactInternal appends "scanning → folding → reclaimed" lines to
	// m.trace, and the reset right after would silently erase them, hiding
	// the compaction entirely ("udah compact tapi gak ada prosesnya"). The
	// trace is preserved on the user message that triggered the compact.
	//
	// Snapshot only the DELTA: m.trace still carries the previous turn's
	// process lines (they're attached to the last user message but never
	// cleared), and copying the whole buffer would duplicate them onto the
	// new message.
	preLen := len(m.trace)
	var compactLog []string
	if m.maybeCompact() {
		compactLog = append([]string(nil), m.trace[preLen:]...)
	}
	if msg.role != roleTool {
		m.promptHistory = appendPromptHistory(m.promptHistory, q)
		m.toolLoop = 0      // a real user turn resets the auto-tool-loop budget
		m.toolPrevCmds = "" // … and the repetition guard — a fresh prompt is progress
		m.toolRepeat = 0
		// A real user turn ACCEPTS the previous turn's edits: drop the
		// one-turn .bro_bak snapshots (audit fix B1 — they used to accumulate
		// forever because Restore/Cleanup were never wired).
		if n := agentic.CleanupStaleSnapshots(); n > 0 {
			m.trace = appendTrace(m.trace, fmt.Sprintf("🧹 cleared %d stale snapshot backup(s)", n))
		}
	}
	m.historyIdx = -1
	m.draftInput = ""
	m.input.SetValue("")
	m.agentWorking = true
	m.agentPhase = "thinking…"
	m.agentStep = 0
	m.agentRun++
	m.agentAborted = false
	m.trace = m.trace[:0]
	if len(compactLog) > 0 {
		m.chat[len(m.chat)-1].trace = append(m.chat[len(m.chat)-1].trace, compactLog...)
	}
	m.subagents = nil
	for _, logLine := range attachedLogs {
		m.trace = append(m.trace, logLine)
	}
	// The working model row (dim gray, under the user prompt) must be in
	// m.trace IMMEDIATELY at send time. The agent goroutine emits its own
	// "→ provider · model" phase line, but only after the async context
	// enrichment walk — without this line the spinner alone would sit under
	// the prompt the whole time. The goroutine's later identical line is
	// collapsed by the renderer (consecutive equal trace lines dedupe).
	if m.provider != "" && m.selectedModel != "" {
		m.trace = appendTrace(m.trace, "→ "+m.provider+" · "+m.selectedModel)
	}
	// A superseded run must be UNBLOCKED, not leaked: the previous run's mock
	// worker can be blocked on `<-answerCh` waiting for an answer that will
	// never arrive (its question was dropped as stale, or ESC interrupted it
	// before it asked). Closing the OLD channel makes that receive return ""
	// — the mock treats it as cancelled and exits, and its tagged result is
	// dropped by the run guard in Update. Only answerCh is safe to close: the
	// worker RECEIVES from it but never sends to it (it writes askCh/traceCh
	// instead — closing those would panic on a later send). The UI always
	// sends to m.answerCh AFTER this replacement, so a send never hits a
	// closed channel.
	if m.answerCh != nil {
		close(m.answerCh)
	}
	m.traceCh = make(chan agentTraceMsg, 32)
	m.askCh = make(chan agentAskMsg, 1)
	m.answerCh = make(chan string, 1)
	m.askOpen = false
	m.agentCancel = func() {}
	m.follow = true
	m.status = "thinking…"
	m.refreshChat()
	// The raw prompt is handed to the agent command — workspace context
	// enrichment (project tree + file attachments + keyword search) runs
	// INSIDE the agent goroutine, never in the update loop. Attaching it
	// synchronously here used to walk the whole project and read every file
	// on every Enter, freezing the TUI (all input queued behind the walk).
	return m, tea.Batch(m.spinner.Tick, m.agentWorkCmd(q, m.traceCh, m.askCh, m.answerCh, &m.agentCancel, m.agentRun), m.waitForTrace(), m.waitForAsk())
}

// appendPromptHistory appends q to the ↑-navigation prompt history, bounded
// at maxPromptHistory: beyond the cap the oldest prompts drop off so a long
// session never accumulates every prompt ever sent in memory (the chat itself
// is already bounded at maxHistory).
func appendPromptHistory(h []string, q string) []string {
	h = append(h, q)
	if len(h) > maxPromptHistory {
		h = h[len(h)-maxPromptHistory:]
	}
	return h
}

// runAgenticToolsCmd executes the bash/read tool blocks from a finished agent
// reply in a background goroutine. Tool commands run bash and can take up to
// the tool timeout — running them in the update loop froze the whole TUI (no
// typing, no scrolling, ESC dead). The UI keeps a live "⚙ executing tool
// commands…" spinner while this runs, and the result feeds back into the
// agent loop via agentToolResultMsg.
//
// Note: RunCommandNative has no context/cancel, so ESC cannot kill the bash
// process itself — it only drops the result. The command's side effects (file
// writes) still land after an abort; that is an accepted limitation.
func (m Model) runAgenticToolsCmd(text string, run int, plannerMode bool) tea.Cmd {
	return runAgenticToolsCmdDeny(text, run, nil, plannerMode, m.exploreConfigFor())
}

// runAgenticToolsCmdDeny is runAgenticToolsCmd with a permission deny-list —
// the native gate's "deny" decision: gated commands are skipped and reported
// to the model instead of executed. exp carries the provider/model config for
// the delegated `explore` subagent tool (nil = explore unavailable).
func runAgenticToolsCmdDeny(text string, run int, deny map[string]bool, plannerMode bool, exp *exploreConfig) tea.Cmd {
	return func() tea.Msg {
		logs, feedback := applyAgenticToolsDeny(text, deny, plannerMode, exp)
		return agentToolResultMsg{logs: logs, feedback: feedback, run: run}
	}
}

// gatedCommands splits the commands in a reply into gate-ask and gate-deny
// lists, consulting the session allow-list ("always allow" rules).
func (m Model) gatedCommands(text string) (gated, hard []string) {
	for _, c := range allToolCommands(text) {
		switch agentic.GateCommand(c, m.repoRoot, m.allowList) {
		case agentic.GateAsk:
			gated = append(gated, c)
		case agentic.GateDeny:
			hard = append(hard, c)
		}
	}
	return gated, hard
}

// finalizeReplyDisplay compacts a finished reply for display AND folds any
// echoed [SYSTEM TOOL RESULT]/[SYSTEM ASK RESULT] payloads (weak models repeat
// them verbatim in their reply text) into a dim collapsible block — the same
// visual language as the ⚙ Tool Executed rows. Tool blocks are stripped as
// before (compactToolReply). Execution always uses the original fullText; this
// only decides what the user SEES. folded reports whether an echo was folded,
// so a reply that was NOTHING but echoed tool output still swaps its text to
// the (empty) prose instead of leaving the raw echo visible.
func (m *Model) finalizeReplyDisplay(fullText string) (display string, folded bool) {
	return m.finalizeReplyDisplayAt(fullText, len(m.chat)-1)
}

// finalizeReplyDisplayAt is finalizeReplyDisplay with an explicit target chat
// index. A reply split into interleaved segments by applyEditBlockAt compacts
// EVERY agent segment (compactReplySegments), so the echo fold must land on
// the segment being compacted — not always the last message.
func (m *Model) finalizeReplyDisplayAt(fullText string, target int) (display string, folded bool) {
	prose, echo, n := extractToolEcho(fullText)
	display, _ = compactToolReply(prose)
	if display == "" {
		display = prose
	}
	if n > 0 && target >= 0 && target < len(m.chat) {
		cm := &m.chat[target]
		// The echo is already present when the content carries it — whether
		// folded fresh (raw marker payload) or merged into a thinking trace or
		// edit-diff block (marker already stripped). Strip the transport prefix
		// from BOTH sides so the compare is marker-agnostic.
		hasEcho := strings.Contains(strings.TrimSpace(stripToolResultPrefix(cm.content)), strings.TrimSpace(stripToolResultPrefix(echo)))
		if cm.collapsible() && !hasEcho {
			// An existing NON-echo block stays — the echo is merged into its
			// content so nothing is lost. Order preserves the block's identity:
			// an edit-diff block (✎ header) keeps its header FIRST so the
			// renderer's isDiffContent still colors + green / - red; a thinking
			// trace gets the echo up front. The content guard stops a second
			// call (ask/permission path re-enters via launchToolRun) from
			// double-merging the same echo, regardless of what the summary says.
			echoPart := stripToolResultPrefix(echo)
			if strings.HasPrefix(strings.TrimSpace(cm.content), "✎ ") {
				cm.content = strings.TrimSpace(cm.content) + "\n\n" + echoPart
			} else {
				cm.content = echoPart + "\n\n" + cm.content
			}
		} else if !cm.collapsible() {
			cm.summary = fmt.Sprintf("⚙ %d echoed tool output(s) · ctrl+o to expand", n)
			cm.content = echo
			cm.collapsed = true
		}
		folded = true
	}
	return display, folded
}

// applyInterleavedEdits applies complete file-write blocks in the revealed
// prefix of the streaming reply and splits the message around them — an edit
// card appears INLINE where the model emitted the block, while the reply keeps
// streaming in a tail segment. Called every stream tick after the reveal chunk
// advances. Spans are sorted by start, so the first block whose closing
// delimiter is still unrevealed ends the pass (later spans can't be complete).
func (m *Model) applyInterleavedEdits() {
	full := m.replyFullText
	if full == "" {
		return
	}
	revealed := len(full) - len(m.streamBuf)
	if revealed <= 0 {
		return
	}
	// Cheap pre-filter: without a fence/heredoc/SEARCH opener in the revealed
	// prefix, no edit block can be complete — skip the regex pass on the
	// prose-only ticks (the common case) entirely.
	if !strings.Contains(full[:revealed], "```") && !strings.Contains(full[:revealed], "cat >") && !strings.Contains(full[:revealed], "<<<<<<<") {
		return
	}
	for _, sp := range editBlockSpans(full) {
		if sp[1] > revealed {
			break
		}
		if sp[0] < m.revealBase {
			continue // already consumed by an earlier segment
		}
		m.applyEditBlockAt(sp, full)
	}
}

// applyEditBlockAt applies ONE edit block and splits the streaming agent
// message around it: the prose before the block stays in the current segment,
// the block becomes a collapsible ✎ edit card (roleTool), and the rest of the
// reply continues in a fresh tail agent message.
func (m *Model) applyEditBlockAt(sp [2]int, full string) {
	blockText := full[sp[0]:sp[1]]
	logs, edits := applyBuilderCodeBlocks(blockText, "", m.plannerMode)
	for _, l := range logs {
		m.trace = appendTrace(m.trace, l)
	}
	if len(edits) == 0 {
		// Failed write (or duplicate file): leave the block visible in the
		// segment and DON'T advance the reveal base — the block stays in the
		// text and the end-of-reply sweep retries it (nothing is recorded, so
		// stripEditSpans won't remove it). A success later still splits fine
		// because revealBase only moves on a real split.
		return
	}
	// Success — record the applied span so the end-of-reply sweep strips it
	// (no double-apply) and split the reply around the edit card.
	m.appliedEditSpans = append(m.appliedEditSpans, sp)

	cur := len(m.chat) - 1
	base := m.revealBase
	curText := m.chat[cur].text // full[base:revealed]
	prose := curText[:sp[0]-base]
	tailText := curText[sp[1]-base:]

	card := chatMsg{
		role:      roleTool,
		summary:   fmt.Sprintf("Edit(%s) · updated %d lines", edits[0].file, edits[0].lines),
		content:   buildEditsDiff(edits),
		collapsed: true,
	}

	m.chat[cur].text = prose
	m.chat = append(m.chat, card)
	m.chat = append(m.chat, chatMsg{role: roleAgent, text: tailText})

	m.revealBase = sp[1]
	m.replyMsgIdx = append(m.replyMsgIdx, len(m.chat)-1)
	m.streamCache = streamCache{} // fresh incremental render for the tail
	m.status = fmt.Sprintf("✎ applied %d file change(s)", len(edits))
	// An applied edit is PROGRESS — the next tool run compares fresh instead
	// of being flagged as a repeat of the pre-edit command.
	m.toolPrevCmds = ""
	m.toolRepeat = 0
}

// ---- interleaved tool cards + early launch --------------------------------

// launchEarlyTools starts executing a finished reply's SAFE tool commands in
// the background immediately (the reply is fully received when agentResultMsg
// lands — the reveal is only a display animation). Tool blocks render as
// inline ⚙ cards while they run, instead of a batch of ⚙ rows popping in
// after the reply finishes. Commands that need the permission gate are
// EXCLUDED here: they wait for the popover (end of the reply) and run once
// the user decides. Returns a cmd only when something actually launches.
func (m *Model) launchEarlyTools() tea.Cmd {
	m.toolLaunched = false
	m.toolBlocked = false
	full := m.replyFullText
	if full == "" {
		return nil
	}
	// Ask blocks are handled by the popover (not a command) — the existing
	// end-of-reply ask path takes over the whole reply, so nothing launches
	// early when one is present.
	if _, ok := parseAskBlock(full); ok {
		return nil
	}
	spans := toolBlockSpans(full)
	if len(spans) == 0 {
		return nil
	}
	gated, hard := m.gatedCommands(full)
	if len(gated) == 0 && len(hard) == 0 {
		// Everything is safe — run it all now.
		m.toolLaunched = true
		return m.launchEarlyRun(full, nil)
	}
	// Some commands are gated: launch only the safe subset now (stripGatedSpans
	// removes the gated blocks, leaving the safe ones) and stash the gated-only
	// text for the permission popover at the end of the reveal (stripSafeSpans
	// removes the safe blocks, leaving the gated ones) so they never run twice.
	safeText := stripGatedSpans(full, gated, hard)
	m.pendingPermissionText = stripSafeSpans(full, gated, hard)
	m.toolLaunched = len(toolBlockSpans(safeText)) > 0
	if m.toolLaunched {
		deny := map[string]bool{}
		for _, c := range append(append([]string{}, gated...), hard...) {
			if k := agentic.AllowKey(c); k != "" {
				deny[k] = true
			}
		}
		return m.launchEarlyRun(safeText, deny)
	}
	m.pendingPermissionText = ""
	return nil
}

// launchEarlyRun is the early-launch core: same execution engine as
// launchToolRun (runAgenticToolsCmdDeny), minus the display compaction (the
// reply is still mid-reveal — nothing is final yet) and minus the repetition
// guard (the guard still applies to the gated round at the end, which is the
// full command set of this reply).
func (m *Model) launchEarlyRun(text string, deny map[string]bool) tea.Cmd {
	if len(toolBlockSpans(text)) == 0 {
		return nil
	}
	for _, line := range toolBlockCommands(text) {
		m.trace = appendTrace(m.trace, line)
	}
	for idx := len(m.chat) - 1; idx >= 0; idx-- {
		if m.chat[idx].role == roleUser {
			m.chat[idx].trace = append([]string(nil), m.trace...)
			break
		}
	}
	m.toolRunning = true
	if cmds := allToolCommands(text); len(cmds) > 0 {
		m.status = fmt.Sprintf("⚙️  Running tool: %s — press Esc to cancel", clip(cmds[0], 35))
	} else {
		m.status = "⚙️  Executing tool commands… — press Esc to cancel"
	}
	m.refreshChat()
	return tea.Batch(m.spinner.Tick, runAgenticToolsCmdDeny(text, m.agentRun, deny, m.plannerMode, m.exploreConfigFor()))
}

// stripSafeSpans removes every tool span whose command is NOT gated/hard from
// text — the result contains only the commands the permission gate must decide
// on (plus the surrounding prose, which launchToolRun ignores).
func stripSafeSpans(text string, gated, hard []string) string {
	if len(gated) == 0 && len(hard) == 0 {
		return text
	}
	gate := map[string]bool{}
	for _, c := range append(append([]string{}, gated...), hard...) {
		gate[c] = true
	}
	var safe [][2]int
	for _, sp := range toolBlockSpans(text) {
		if !(sp.kind == "bash" && gate[sp.cmd]) {
			safe = append(safe, sp.sp)
		}
	}
	if len(safe) == 0 {
		return text
	}
	var sb strings.Builder
	last := 0
	for _, sp := range safe {
		sb.WriteString(text[last:sp[0]])
		last = sp[1]
	}
	sb.WriteString(text[last:])
	return sb.String()
}

// stripGatedSpans removes every tool span whose command IS gated/hard from
// text — the result is the text with only the safe commands left.
func stripGatedSpans(text string, gated, hard []string) string {
	gate := map[string]bool{}
	for _, c := range append(append([]string{}, gated...), hard...) {
		gate[c] = true
	}
	var gatedSpans [][2]int
	for _, sp := range toolBlockSpans(text) {
		if sp.kind == "bash" && gate[sp.cmd] {
			gatedSpans = append(gatedSpans, sp.sp)
		}
	}
	if len(gatedSpans) == 0 {
		return text
	}
	var sb strings.Builder
	last := 0
	for _, sp := range gatedSpans {
		sb.WriteString(text[last:sp[0]])
		last = sp[1]
	}
	sb.WriteString(text[last:])
	return sb.String()
}

// applyInterleavedTools splits completed tool blocks out of the streaming
// reply as inline ⚙ cards — the tool a model emitted becomes a visible card
// at the exact position it was emitted, while the prose continues in a tail
// segment. Execution already started (launchEarlyTools); the cards are the
// VISIBLE trace of that execution. Runs every stream tick after the edit
// interleave; spans are sorted by start, so the first span whose closer is
// still unrevealed ends the pass.
func (m *Model) applyInterleavedTools() {
	full := m.replyFullText
	if full == "" {
		return
	}
	revealed := len(full) - len(m.streamBuf)
	if revealed <= 0 {
		return
	}
	if !strings.Contains(full[:revealed], "```") && !strings.Contains(full[:revealed], "<tool_call>") {
		return
	}
	for _, sp := range toolBlockSpans(full) {
		if sp.sp[1] > revealed {
			break
		}
		if sp.sp[0] < m.revealBase {
			continue
		}
		if m.revealedToolSpans[sp.sp[0]] {
			continue
		}
		m.applyToolCardAt(sp, full)
	}
}

// applyToolCardAt splits the streaming agent message around ONE tool block,
// inserting a collapsible ⚙ card where the model emitted the tool.
func (m *Model) applyToolCardAt(sp toolSpan, full string) {
	m.revealedToolSpans[sp.sp[0]] = true

	cur := len(m.chat) - 1
	base := m.revealBase
	curText := m.chat[cur].text // full[base:revealed]
	if sp.sp[0]-base > len(curText) {
		return
	}
	prose := curText[:sp.sp[0]-base]
	tailText := curText[sp.sp[1]-base:]

	// Status preview: when the early launch ran, safe commands are already
	// executing in the background; gated/hard commands await the permission
	// popover. When nothing launched early (an ask block shares the reply),
	// every tool is still queued — it runs after the popover submits.
	status := "queued"
	if m.toolLaunched {
		status = "running…"
	}
	gated, hard := m.gatedCommands(full)
	gateSet := map[string]bool{}
	for _, c := range append(append([]string{}, gated...), hard...) {
		gateSet[c] = true
	}
	if sp.kind == "bash" && gateSet[sp.cmd] {
		status = "awaiting approval"
	}

	icon := "⚙"
	verb := "bash"
	if sp.kind == "read" {
		icon = "📖"
		verb = "read"
	}
	card := chatMsg{
		role:      roleTool,
		summary:   fmt.Sprintf("%s %s · %s · %s", icon, verb, clip(sp.cmd, 60), status),
		content:   sp.cmd,
		collapsed: true,
	}

	m.chat[cur].text = prose
	m.chat = append(m.chat, card)
	if status == "awaiting approval" {
		m.pendingGatedCards = append(m.pendingGatedCards, len(m.chat)-1)
	}
	m.chat = append(m.chat, chatMsg{role: roleAgent, text: tailText})
	m.revealBase = sp.sp[1]
	m.replyMsgIdx = append(m.replyMsgIdx, len(m.chat)-1)
	m.streamCache = streamCache{} // fresh incremental render for the tail
}

// markPendingToolCards updates the inline ⚙ cards that waited on the
// permission popover with the user's decision — "allowed" or "denied" — so
// the transcript reflects what actually happened to each gated command.
func (m *Model) markPendingToolCards(decision string) {
	for _, idx := range m.pendingGatedCards {
		if idx < 0 || idx >= len(m.chat) {
			continue
		}
		cm := &m.chat[idx]
		if cm.role != roleTool {
			continue
		}
		switch decision {
		case "allow once", "always allow":
			cm.summary = strings.Replace(cm.summary, "awaiting approval", "allowed ✓", 1)
		default:
			cm.summary = strings.Replace(cm.summary, "awaiting approval", "denied", 1)
		}
	}
	m.pendingGatedCards = nil
}

// compactReplySegments applies finalizeReplyDisplay to every agent segment of
// the current reply (a reply split by interleaved edits has several). Raw tool
// blocks compact and echoed payloads fold into each segment's own block.
func (m *Model) compactReplySegments() {
	for _, idx := range m.replyMsgIdx {
		if idx < 0 || idx >= len(m.chat) {
			continue
		}
		cm := &m.chat[idx]
		if cm.role != roleAgent || cm.text == "" {
			continue
		}
		if display, folded := m.finalizeReplyDisplayAt(cm.text, idx); folded || display != "" {
			cm.text = display
		}
	}
}

// appendEditCards appends collapsible ✎ edit cards for edits applied by the
// end-of-reply sweep (blocks that can't interleave during streaming). The
// cards land AFTER the reply as structured rows — never folded on top of it.
func (m *Model) appendEditCards(edits []editChange) {
	if len(edits) == 0 {
		return
	}
	card := chatMsg{
		role:      roleTool,
		summary:   fmt.Sprintf("Edit(%s) · updated %d lines", edits[0].file, edits[0].lines),
		content:   buildEditsDiff(edits),
		collapsed: true,
	}
	m.chat = appendChat(m.chat, card)
}

// stripEditSpans removes the byte spans of already-applied edit blocks from
// text, so the end-of-reply sweep never re-applies an interleaved edit.
func stripEditSpans(text string, spans [][2]int) string {
	if len(spans) == 0 {
		return text
	}
	var sb strings.Builder
	last := 0
	for _, sp := range spans {
		if sp[0] < last || sp[0] < 0 || sp[1] > len(text) || sp[1] <= sp[0] {
			continue
		}
		sb.WriteString(text[last:sp[0]])
		last = sp[1]
	}
	sb.WriteString(text[last:])
	return sb.String()
}

// buildEditsDiff joins multiple edit diffs into one labeled block: a ✎ header
// line per file (file · N lines) followed by the unified diff body.
func buildEditsDiff(edits []editChange) string {
	var sb strings.Builder
	for _, e := range edits {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("✎ %s · %d lines\n", e.file, e.lines))
		if e.diff != "" {
			sb.WriteString(e.diff)
		}
	}
	return strings.TrimRight(sb.String(), " \n")
}

// launchToolRun runs the tool blocks of a finished reply in the background
// (compact display + indicators + trace attach), optionally skipping commands
// in the deny map (the permission gate's "deny" decision). Shared by the
// safe-commands path, the ask tool path, and the permission decisions.
func (m Model) launchToolRun(fullText string, deny map[string]bool) (Model, tea.Cmd) { // The reply body must not re-show the raw command/XML text — those
	// blocks were (or are being) executed, their story lives in the trace +
	// ⚙ Tool Executed rows. Swap each segment's DISPLAYED text for the
	// prose-only compact version (echoed tool payloads fold into the dim
	// block too); execution still uses the original fullText.
	m.compactReplySegments()
	// REPETITION GUARD: a model stuck in a loop re-emits the IDENTICAL command
	// set instead of reacting to the previous output. Detect it HERE and stop
	// BEFORE the commands run again — the 3rd identical set is blocked
	// outright (maxToolRepeat=2 allowed repeats), so a verbatim loop dies in
	// 3 rounds instead of burning maxToolLoops. A change to the set, a user
	// message, or an applied file edit resets the counter (that is progress).
	if sig := toolSetSignature(allToolCommands(fullText)); sig != "" {
		if sig == m.toolPrevCmds {
			m.toolRepeat++
		} else {
			m.toolRepeat = 0
		}
		if m.toolRepeat >= maxToolRepeat {
			rep := clip(strings.Split(sig, "\x00")[0], 40)
			if n := strings.Count(sig, "\x00"); n > 0 {
				rep += fmt.Sprintf(" + %d more", n)
			}
			m.toolRunning = false
			m.trace = appendTrace(m.trace, fmt.Sprintf("● Tool loop stopped — same command(s) %d× with no progress: %q — type a message to continue", m.toolRepeat+1, rep))
			m.status = fmt.Sprintf("⚠️ tool loop stopped — repeated command(s) %d× — type a message to continue", m.toolRepeat+1)
			m.refreshChat()
			return m, nil
		}
		m.toolPrevCmds = sig
	}
	for _, line := range toolBlockCommands(fullText) {
		m.trace = appendTrace(m.trace, line)
	}
	for idx := len(m.chat) - 1; idx >= 0; idx-- {
		if m.chat[idx].role == roleUser {
			m.chat[idx].trace = append([]string(nil), m.trace...)
			break
		}
	}
	m.refreshCtx()
	m.toolRunning = true
	if cmds := allToolCommands(fullText); len(cmds) > 0 {
		m.status = fmt.Sprintf("⚙️  Running tool: %s — press Esc to cancel", clip(cmds[0], 35))
	} else {
		m.status = "⚙️  Executing tool commands… — press Esc to cancel"
	}
	m.refreshChat()
	return m, tea.Batch(m.spinner.Tick, runAgenticToolsCmdDeny(fullText, m.agentRun, deny, m.plannerMode, m.exploreConfigFor()))
}

// applyTheme sets the theme preset by name — no cycling, no hidden changes.
func (m Model) applyTheme(name string) Model {
	if _, ok := Themes[name]; !ok {
		m.status = "unknown theme: " + name + " — available: " + strings.Join(themeNames(), ", ")
		return m
	}
	m.setTheme(name)
	m.status = "theme → " + name
	if m.started {
		m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: "theme → " + name})
		m.refreshChat()
	}
	return m
}

// setTheme swaps the active theme, rebuilds the precomputed styles, and
// recomputes the cached gradient wordmark — the logo is theme-static data,
// never recomputed per frame (anti-lag rule 5).
func (m *Model) setTheme(name string) {
	m.themeName = name
	m.styles = newStyles(Themes[name])
	m.spinner = spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(m.styles.spinner))
	m.logoView = renderGradientLogo(logoArt, m.styles.logo)
}
