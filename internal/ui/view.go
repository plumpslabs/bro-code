package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/plumpslabs/bro-code/internal/provider"
	"golang.org/x/term"
)

func (m *Model) View() tea.View {
	var content string
	if m.showAsk {
		content = m.renderAskModal()
	} else if m.showModels {
		content = m.renderModelsModal()
	} else if m.showSessions {
		content = m.renderSessionsModal()
	} else if m.showMCP {
		content = m.renderMCPModal()
	} else if m.showConnect {
		content = m.renderConnectModal()
	} else if m.showDebug {
		content = m.renderDebugModal()
	} else if m.pagerActive {
		content = m.renderPager()
	} else {
		var sb strings.Builder

		// Message Log — scrolls inside a viewport; the formatted log is cached
		// so markdown only re-renders when a message actually changes.
		contentWidth := m.width - 4
		if contentWidth < 0 {
			contentWidth = 0
		}
		log := m.buildLog(contentWidth)

		// Chrome below the log (activity slot + input + banner + help) is built
		// FIRST and measured, so the log viewport can be sized to exactly fill
		// the remaining terminal height. Hardcoding the chrome line count is
		// what made the view crop history and flicker — measuring the rendered
		// chrome can never drift.
		chrome, chromeLines := m.buildLogChrome()

		// buildLog always ends with "\n\n" (per-message separator), so the log
		// occupies count+1 rows. The viewport height hugs this: exactly as tall
		// as the content while it fits, capped at the screen once it overflows.
		logLines := 0
		if log != "" {
			logLines = lipgloss.Height(log)
		}

		if m.width > 0 {
			// The viewport must be sized before it can be rendered (it returns
			// "" when width/height are 0). Ensure it tracks the terminal even
			// before the first WindowSizeMsg lands.
			if m.logViewport.Width() != m.width {
				m.logViewport.SetWidth(m.width)
			}
			// Available height below the measured chrome (activity slot + input +
			// banner + help). The viewport NEVER exceeds it, so viewport+chrome
			// can never be taller than the terminal — nothing is ever cropped by
			// the renderer and nothing reflows (no flicker).
			avail := m.height - chromeLines
			if avail < 3 {
				avail = 3
			}
			// Content-hugging height: exactly as tall as the log while it fits,
			// capped at the screen once it overflows. This is the whole design —
			// one path, no natural↔viewport mode switch to jump or cut at the
			// boundary: a short `-c` session grows from the top like a normal
			// terminal (the viewport never pads short content, so there is no
			// blank gap before the input), and a long session locks into a
			// scrollable window with the newest content at the bottom.
			vpHeight := avail
			if logLines < vpHeight {
				vpHeight = logLines
			}
			if vpHeight != m.logViewport.Height() {
				m.logViewport.SetHeight(vpHeight)
			}
			if m.streaming {
				if log != m.renderedLog {
					wasAtBottom := m.logViewport.AtBottom()
					m.logViewport.SetContent(log)
					if wasAtBottom {
						m.logViewport.GotoBottom()
					}
					m.renderedLog = log
				}
			} else if key := m.logKey(); key != m.renderedKey || vpHeight != m.renderedH {
				wasAtBottom := m.logViewport.AtBottom()
				m.logViewport.SetContent(log)
				if key != m.renderedKey || m.renderedH == 0 {
					// Only auto-scroll when the user is already at the bottom.
					// Never yank the viewport away from a user who is reading history.
					if wasAtBottom {
						m.parkLogAfterNewContent(log, vpHeight, contentWidth)
					}
				}
				// Height-only change (the live activity slot grew/shrunk): content
				// is identical, so preserve the reading position — the viewport's
				// rendering clamps safely when it shrinks.
				m.renderedLog = log
				m.renderedKey = key
				m.renderedH = vpHeight
			}
			// Render the log through the viewport window. Because the viewport is
			// sized to its content (or capped at the screen), its output is
			// exactly the chat: newest at the bottom, older history reachable by
			// PgUp/PgDn/wheel, chrome below stays pinned.
			sb.WriteString(m.logViewport.View())
		} else {
			// Before the first WindowSizeMsg lands, width/height are 0 and the
			// viewport path is unavailable. Clip the raw log to the terminal
			// height so a resumed session's restored history never dumps its
			// whole length on the very first frame (a giant flash) before the
			// viewport takes over.
			sb.WriteString(clipToTerminalBounds(log, getTerminalHeight()-chromeLines))
		}

		sb.WriteString(chrome)
		content = sb.String()
	}

	v := tea.NewView(content)
	v.AltScreen = false
	// Mouse wheel scrolling only works when the terminal actually reports mouse
	// events. SELECT mode keeps the terminal's native text selection (no mouse
	// capture); SCROLL mode (ctrl+m) enables cell-motion events so the wheel
	// scrolls the log viewport.
	if m.mouseMode == "SCROLL" {
		v.MouseMode = tea.MouseModeCellMotion
	} else {
		v.MouseMode = tea.MouseModeNone
	}
	return v
}

// clipToTerminalBounds keeps only the NEWEST maxH lines of content (the tail),
// so a first frame rendered before WindowSizeMsg lands lands on the newest
// content instead of dumping the whole (restored) history.
func clipToTerminalBounds(content string, maxH int) string {
	if maxH <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) > maxH {
		lines = lines[len(lines)-maxH:]
	}
	return strings.Join(lines, "\n")
}

// truncatePrompt flattens a queued prompt onto one line (newlines and runs of
// whitespace collapse to single spaces) and cuts it to a readable preview width
// for the queue rows rendered above the input.
func truncatePrompt(s string) string {
	one := strings.Join(strings.Fields(s), " ")
	if len(one) > 70 {
		one = one[:70] + "…"
	}
	return one
}

// buildLog renders the message history + live streaming block. The history
// part is cached (renderedHistory) and rebuilt only when the messages change,
// so every streamed chunk re-renders just the cheap streaming box instead of
// the whole log — this keeps unbounded history responsive.
func (m *Model) buildLog(contentWidth int) string {
	key := fmt.Sprintf("%s|%d|%v", m.logKey(), contentWidth, m.filesExpanded)
	if key != m.historyKey {
		var sb strings.Builder
		for _, msg := range m.messages {
			sb.WriteString(formatMessage(msg, contentWidth, m.filesExpanded) + "\n\n")
		}
		m.renderedHistory = sb.String()
		m.historyKey = key
	}
	var out strings.Builder
	out.WriteString(m.renderedHistory)
	if m.streaming && m.pendingStream != "" {
		streamMode := m.turnMode
		if streamMode == "" {
			streamMode = m.mode
		}
		if streamMode == "" {
			streamMode = "BUILDER"
		}
		badgeColor := "42"
		switch streamMode {
		case "BUILDER":
			badgeColor = "42"
		case "PLANNER":
			badgeColor = "141"
		case "MINER":
			badgeColor = "214"
		default:
			badgeColor = "42"
		}
		label := lipgloss.NewStyle().Foreground(lipgloss.Color(badgeColor)).Bold(true).Render("BROCODE")
		badgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Bold(true).Padding(0, 1).Background(lipgloss.Color(badgeColor))
		label += "  " + badgeStyle.Render(streamMode)
		if m.activeModel != "" {
			modelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
			label += "  " + modelStyle.Render(m.activeModel)
		}
		botBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color(badgeColor)).Padding(1, 2)
		w := contentWidth
		if w <= 0 {
			w = getTerminalWidth() - 2
		}
		botBarStyle = botBarStyle.Width(w)
		wrap := w - 6
		if wrap < 30 {
			wrap = 30
		}
		formattedStream := m.getFormattedStream(wrap)
		if strings.Contains(formattedStream, "--- ") || strings.Contains(formattedStream, "+++ ") || strings.Contains(formattedStream, "@@ ") {
			formattedStream = formatDiffLines(formattedStream)
		}
		out.WriteString(botBarStyle.Render(label+"\n\n"+formattedStream) + "\n\n")
	}
	return out.String()
}

// parkLogAfterNewContent repositions the viewport after new content was
// appended to the log. A short newest content (or a short whole log) lands at
// the bottom so everything is visible. When a long assistant answer — taller
// than the viewport — is the newest content (trailing FILES/PROCESS summaries
// allowed), it parks at the START of that answer instead, so the reader begins
// at its top and pages down. Previously the answer's beginning hid above the
// fold and looked "cut off" until Ctrl+P opened a pager.
func (m *Model) parkLogAfterNewContent(log string, vpHeight, contentWidth int) {
	if len(m.messages) == 0 || strings.Count(log, "\n")+1 <= vpHeight {
		m.logViewport.GotoBottom()
		return
	}
	// Walk back from the newest message, skipping trailing FILES/PROCESS
	// summaries, to find the assistant answer this turn produced. If anything
	// else (a user prompt) is the newest content, land at the bottom.
	ansIdx := -1
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if strings.HasPrefix(msg, "BROCODE") || strings.HasPrefix(msg, "🤖 ") {
			ansIdx = i
			break
		}
		if strings.HasPrefix(msg, "FILES:") || strings.HasPrefix(msg, "PROCESS:") {
			continue
		}
		break
	}
	if ansIdx < 0 {
		m.logViewport.GotoBottom()
		return
	}
	tail := formatMessage(m.messages[ansIdx], contentWidth, m.filesExpanded)
	if lipgloss.Height(tail) <= vpHeight {
		m.logViewport.GotoBottom()
		return
	}
	// Offset the viewport so the answer's first line sits at the top: the
	// y-offset is the number of lines that precede it in the rendered log
	// (count newlines before the tail block's last occurrence). Clamp so the
	// viewport never scrolls past the end of the content.
	idx := strings.LastIndex(log, tail)
	if idx == -1 {
		m.logViewport.GotoBottom()
		return
	}
	offset := strings.Count(log[:idx], "\n")
	if max := strings.Count(log, "\n") + 1 - vpHeight; offset > max {
		offset = max
	}
	if offset < 0 {
		offset = 0
	}
	m.logViewport.SetYOffset(offset)
}

// buildPagerContent renders the in-TUI pager view: a header bar naming the
// navigation keys above the last assistant answer.
func (m *Model) buildPagerContent(contentWidth int) string {
	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).
		Render("── LATEST ANSWER ── PgUp/PgDn/Home/End/↑/↓ scroll · q/Esc/Ctrl+P exit") + "\n\n"
	ans := m.lastAssistantAnswer()
	if strings.TrimSpace(ans) == "" {
		return header + lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("(no assistant response to display)")
	}
	return header + formatMessage("BROCODE:\n"+ans, contentWidth, false)
}

// exitPager closes the in-TUI pager and forces the next frame to re-render the
// normal chat log, landing back on the newest content.
func (m *Model) exitPager() {
	m.pagerActive = false
	m.pagerContent = ""
	m.renderedLog = ""
	m.renderedKey = ""
	m.renderedH = 0
}

// lastAssistantAnswer returns the text of the most recent assistant answer.
func (m *Model) lastAssistantAnswer() string {
	msgs := m.context.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	for i := len(m.messages) - 1; i >= 0; i-- {
		if strings.HasPrefix(m.messages[i], "BROCODE") {
			parts := strings.SplitN(m.messages[i], "\n", 2)
			if len(parts) > 1 {
				return parts[1]
			}
		}
	}
	return ""
}

// logKey is a cheap fingerprint of the message list so
// the cache only invalidates when the history actually changes.
func (m *Model) logKey() string {
	if len(m.messages) == 0 {
		return "0|"
	}
	return fmt.Sprintf("v%d|%d|%s", m.historyVersion, len(m.messages), m.messages[len(m.messages)-1])
}

// buildLogChrome renders everything below the log viewport — the live
// activity slot (spinner + steps, or a blank gap when idle), the multi-line
// input (or the file-action confirm bar), the sticky footer banner and the
// help hint — and returns the rendered string plus how many terminal lines it
// occupies. The log viewport is then sized to terminal height minus these
// lines, so the full view is EXACTLY one terminal tall: nothing is cropped by
// the renderer and nothing reflows, which is what eliminated the flicker and
// the history cutting. Measuring the rendered chrome instead of hardcoding a
// count keeps this correct as the input grows/shrinks and as the activity
// slot appears/disappears.
func (m *Model) buildLogChrome() (string, int) {
	var sb strings.Builder

	// Live agent activity slot: spinner + current step + the last few tool
	// calls, rendered ABOVE the input. Activity is transient — it never
	// enters the conversation history (that's what made process rows pile
	// up above the answer and hide the user's prompt).
	if (m.turnRunning || (m.status != "Ready" && m.status != "Failed")) && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug && !m.showMCP {
		frame := spinnerFrames[m.spinnerIdx%len(spinnerFrames)]
		elapsed := ""
		if !m.turnStart.IsZero() {
			d := time.Since(m.turnStart)
			elapsed = fmt.Sprintf("  ⏱ %d:%02d", int(d.Minutes()), int(d.Seconds())%60)
		}
		if m.status == "Initializing..." {
			sb.WriteString(normalizeEmojiSpacing(m.status) + "\n")
		} else {
			spinnerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
			sb.WriteString(spinnerStyle.Render(frame) + "  " + normalizeEmojiSpacing(m.status) + elapsed + "\n")
		}
		actStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
		for _, act := range m.activity {
			sb.WriteString(actStyle.Render("  · "+normalizeEmojiSpacing(act)) + "\n")
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n")
	}

	// Queued prompts: shown live above the input, never as history rows. In
	// queue mode (Ctrl+K / Alt+K) the selected row is highlighted and a hint names the
	// management keys (e edit · d delete · m mode · ↑/↓ select · Shift+↑/↓ reorder · Esc exit).
	if len(m.pendingQueue) > 0 {
		qHead := lipgloss.NewStyle().Foreground(lipgloss.Color("178")).Bold(true).Render(fmt.Sprintf("⏳ PROMPT QUEUE (%d)", len(m.pendingQueue)))
		if m.queueMode {
			qHead += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("  · e edit · d delete · m mode · ↑/↓ select · Shift+↑/↓ reorder · Esc exit")
		} else {
			qHead += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("  · Ctrl+K / Alt+K manage")
		}
		sb.WriteString(qHead + "\n")
		for i, q := range m.pendingQueue {
			badge := modeBadgeMini(q.Mode)
			row := fmt.Sprintf("  %d · %s %s", i+1, badge, truncatePrompt(q.Text))
			if m.queueMode && i == m.queueSel {
				sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true).Render("▸ "+row) + "\n")
			} else {
				sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(row) + "\n")
			}
		}
		sb.WriteString("\n")
	}

	// Interactive Senior Recommendations:
	if len(m.activeRecommendations) > 0 && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug && !m.showMCP && !m.showAsk && !m.showFileConfirm {
		if recBar := RenderRecommendationsBar(m.activeRecommendations, m.width); recBar != "" {
			sb.WriteString(recBar + "\n\n")
		}
	}

	// Input area: while a critical file action (create/delete) awaits
	// approval, the chat input is temporarily replaced by the confirm bar. In
	// pager mode the input gives way to a hint bar naming the pager keys.
	if m.pagerActive {
		pagerBar := lipgloss.NewStyle().Foreground(lipgloss.Color("241")).
			Render("── PAGER: LATEST ANSWER · PgUp/PgDn/Home/End/↑/↓ · q/Esc/Ctrl+P exit ──")
		sb.WriteString(pagerBar + "\n\n")
	} else if m.showFileConfirm {
		sb.WriteString(m.renderFileConfirmBar() + "\n\n")
	} else {
		// Input Box (Borderless & Minimalist, multi-line textarea that grows)
		modeColor := "42"
		promptStr := "BUILD ❯ "
		switch m.mode {
		case "PLANNER":
			modeColor = "141"
			promptStr = "PLAN ❯ "
			m.promptInput.Placeholder = "Planner Mode: Ask for architecture plans, code analysis, or roadmaps..."
		case "MINER":
			modeColor = "214"
			promptStr = "MINE ❯ "
			m.promptInput.Placeholder = "Miner Mode: Explore the codebase and persist verified knowledge to project memory..."
		default:
			modeColor = "42"
			promptStr = "BUILD ❯ "
			m.promptInput.Placeholder = "Type a prompt or command (/help, /sessions, /new)..."
		}
		promptStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(modeColor)).Bold(true)
		if m.autocomplete.Active {
			if acBox := RenderAutocomplete(m.autocomplete, m.width); acBox != "" {
				sb.WriteString(acBox + "\n")
			}
		}
		sb.WriteString(promptStyle.Render(promptStr) + m.promptInput.View() + "\n\n")
	}

	// STICKY BOTTOM FOOTER BANNER (Never disappears when history grows long)
	bannerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Padding(0, 1)
	if m.width > 0 {
		bannerStyle = bannerStyle.MaxWidth(m.width)
	}
	tokenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
	modeColor := "42"
	switch m.mode {
	case "PLANNER":
		modeColor = "141"
	case "MINER":
		modeColor = "214"
	default:
		modeColor = "42"
	}
	modeBadgeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(modeColor))

	sessID := m.context.SessionID()
	if len(sessID) > 12 {
		sessID = sessID[:12] + "…"
	}
	// Compact token/$ HUD: window usage, per-turn tokens+$ and per-session $.
	// Short labels keep the footer on one line even on narrow terminals.
	tokensStr := fmt.Sprintf("%s/%s", provider.FormatTokens(m.context.TotalTokens()), provider.FormatTokens(m.context.MaxWindow()))
	if m.engine != nil {
		if tk, c := m.engine.TurnTokens(), m.engine.CostUSD(); tk > 0 || c > 0 {
			tokensStr += fmt.Sprintf(" · T:%s $%.4f", provider.FormatTokens(tk), c)
		}
		if s := m.engine.SessionCostUSD(); s > 0 {
			tokensStr += fmt.Sprintf(" · Sess:$%.4f", s)
		}
	}

	// Live LSP indicator: count of available language servers (binary on
	// PATH). Compact "· LSP:N" form so the footer never overflows.
	lspBadge := ""
	if m.lspMgr != nil {
		if n := len(m.lspMgr.AvailableServers()); n > 0 {
			lspBadge = fmt.Sprintf(" · LSP:%d", n)
		}
	}

	// Live Web Search provider badge (supports single and multi-tier)
	searchBadge := provider.GetSearchProviderStatus().Badge

	// Lead icon: dynamic animated loader while busy/running, fire emoji when ready
	leadIcon := "🔥"
	if m.turnRunning {
		leadIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true).Render(spinnerFrames[m.spinnerIdx%len(spinnerFrames)])
	}

	footerBanner := fmt.Sprintf("%s %s · P:%s · M:%s · S:%s · %s%s%s",
		leadIcon, modeBadgeStyle.Render(m.mode), m.activeProvider.Info.Name, m.activeModel, sessID, tokenStyle.Render(tokensStr), lspBadge, searchBadge)

	helpStr := " ENTER send · Alt+Enter newline · Tab mode · ↑/↓ history · PgUp/PgDn scroll · Ctrl+P pager · Ctrl+Y copy · Ctrl+M mouse · /help "
	if m.width >= 120 {
		helpStr = " ENTER send · Alt+Enter newline · Tab/Shift+Tab mode · ↑/↓ history · PgUp/PgDn scroll · Ctrl+P pager · Ctrl+Y copy · Ctrl+M mouse mode · /sessions · /models · /lsp · /help "
	} else if m.width >= 90 {
		helpStr = " ENTER send · Alt+Enter newline · Tab mode · ↑/↓ history · Ctrl+P pager · Ctrl+Y copy · Ctrl+M mouse · /sessions · /models · /help "
	}
	// When prompts are queued, advertise the queue-management key.
	if len(m.pendingQueue) > 0 && m.width >= 120 {
		helpStr += " · Ctrl+K / Alt+K queue "
	}
	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	if m.width > 0 {
		helpStyle = helpStyle.MaxWidth(m.width)
	}

	sb.WriteString(bannerStyle.Render(footerBanner) + "\n")
	sb.WriteString(helpStyle.Render(helpStr))

	s := sb.String()
	return s, strings.Count(s, "\n")
}

func modeBadgeMini(mode string) string {
	switch mode {
	case "PLANNER":
		return lipgloss.NewStyle().Background(lipgloss.Color("141")).Foreground(lipgloss.Color("0")).Bold(true).Render(" PLAN ")
	case "MINER":
		return lipgloss.NewStyle().Background(lipgloss.Color("214")).Foreground(lipgloss.Color("0")).Bold(true).Render(" MINE ")
	default:
		return lipgloss.NewStyle().Background(lipgloss.Color("42")).Foreground(lipgloss.Color("0")).Bold(true).Render(" BUILD ")
	}
}

// updateLogHeight sizes the log viewport to the terminal height below the
// chrome (activity slot + input + banner + help). This is the resize-time
// default; View() then refines it each frame to the content-hugging height
// (min(avail, logLines)) so short sessions have no blank gap and long ones
// lock into a scrollable window. Returns the computed height.
func (m *Model) updateLogHeight() int {
	_, chromeLines := m.buildLogChrome()
	h := m.height - chromeLines
	if h < 3 {
		h = 3
	}
	if h != m.logViewport.Height() {
		m.logViewport.SetHeight(h)
	}
	return h
}

func getTerminalHeight() int {
	if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && h > 10 {
		return h
	}
	return 40
}
