// render.go — TUI rendering layer: builds the visual tree from Model state.
// Every render method is pure (no state mutation) and uses precomputed styles
// (pro-TUI rule 4: styles are rebuilt once per theme change, never in View()).
//
// Layout Pattern (Fixed-Bottom):
//
//	┌─────────────────────────────────┐
//	│ Header (1 line)                 │
//	├─────────────────────────────────┤
//	│                                 │
//	│ Viewport (chat area)            │
//	│ Fills remaining space           │
//	│                                 │
//	├─────────────────────────────────┤
//	│ ┌─────────────────────────────┐ │
//	│ │ Input Box (fixed 3 lines)   │ │  ← Fixed position
//	│ │ + scroll indicator           │ │  ← Grows UPWARD when text long
//	│ └─────────────────────────────┘ │
//	│ Status Bar (1 line)             │
//	└─────────────────────────────────┘
package tui

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ansiStrip removes ANSI escape sequences for plain-text width calculations.
var ansiStrip = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Fixed layout constants
const (
	headerHeight = 1                         // header lines
	statusHeight = 1                         // status bar lines
	inputHeight  = 3                         // input content lines (fixed)
	inputBorder  = 2                         // top + bottom border
	inputTotal   = inputHeight + inputBorder // total input box height = 5
	minViewport  = 5                         // minimum viewport height
)

// layout recomputes panel sizes from the current window size.
// Uses fixed-bottom layout: input is anchored at bottom, viewport fills rest.
func (m *Model) layout() {
	if m.height < 8 {
		m.height = 8
	}
	// A resize invalidates content coordinates — drop any in-progress drag.
	m.dragSel = dragSelection{}
	m.showPanel = false

	chatW := m.width
	if chatW < 12 {
		chatW = 12
	}

	// Fixed-bottom layout: header(1) + viewport + input(5) + status(1)
	viewportH := m.height - headerHeight - inputTotal - statusHeight
	if viewportH < minViewport {
		viewportH = minViewport
	}

	m.viewport.SetWidth(chatW)
	m.viewport.SetHeight(viewportH)

	// Size the input to the new terminal width here (not in View — purity).
	m.refreshInputWidth()

	// Window resize changes the wrap width — cached message views are stale.
	// Clear the cache so every message re-renders at the new width exactly
	// once (streaming afterwards stays incremental again).
	m.msgCache = m.msgCache[:0]
	m.streamCache = streamCache{}
	m.refreshChat()
}

// chatContentWidth is the wrap width for chat messages.
func (m Model) chatContentWidth() int {
	return m.width - 2
}

// refreshChat rebuilds the viewport content from the bounded chat history.
// Per-message render cache (pmCache): unchanged messages reuse their cached
// view — only messages whose fields changed (the streaming tail, new turns,
// toggled collapse) are re-rendered. The cache mirrors chat 1:1, so the join
// stays correct after /clear, compaction, or history resume (slots re-render
// on mismatch).
func (m *Model) refreshChat() {
	var sb strings.Builder

	// Brand block as the stable prefix of the viewport content (Phase 2):
	// top-aligned, pushed up and scrolled away as messages accumulate. Cached
	// gradient (m.logoView) + cheap centering → safe per frame. Empty chat
	// (fresh start / after /clear) = brand block alone.
	if brand := m.renderBrandBlock(m.width); brand != "" {
		sb.WriteString(brand)
		sb.WriteString("\n")
	}
	sb.WriteString("\n") // Breathing space below the brand / viewport top

	for i, cm := range m.chat {
		if i > 0 {
			sb.WriteString("\n")
		}

		// Cache hit → reuse the rendered view. Miss → render and store. The
		// streaming agent reply takes a dedicated INCREMENTAL path: its text
		// grows every stream tick, and re-rendering a long reply in full at
		// 20fps saturated the event loop (typing lag — keystrokes queued behind
		// full re-wraps). The incremental cache renders each completed line
		// once and only re-wraps the trailing partial line per tick.
		var msgView string
		// The incremental path renders the full body — a collapsed collapsible
		// streaming reply must fall back to renderChatMsg (summary only).
		if i == len(m.chat)-1 && cm.role == roleAgent && m.streaming && !(cm.collapsible() && cm.collapsed) {
			msgView = m.renderStreamingAgent(cm.text, m.chatContentWidth())
		} else if i < len(m.msgCache) && m.msgCache[i].matches(cm) {
			msgView = m.msgCache[i].view
		} else {
			msgView = m.renderChatMsg(cm)
			m.growCache(i)
			m.msgCache[i] = pmCache{
				role:      cm.role,
				text:      cm.text,
				summary:   cm.summary,
				content:   cm.content,
				collapsed: cm.collapsed,
				view:      msgView,
			}
		}
		sb.WriteString(msgView)

		// Render trace lines under user messages AND transient tool turns — the
		// agentic loop auto-sends tool results, so a dead-feeling gap between
		// "Tool Executed" and the next reply is exactly what users complained
		// about. The live trace (provider/model row + phase lines) shows in
		// both.
		if cm.role == roleUser || cm.role == roleTool {
			traceLines := cm.trace
			if (i == len(m.chat)-1 || (i == len(m.chat)-2 && m.chat[len(m.chat)-1].role == roleAgent)) && len(m.trace) > 0 {
				traceLines = m.trace
			}
			if len(traceLines) > 0 {
				sb.WriteString("\n")
				var allLines []string
				for _, rawLn := range traceLines {
					for _, subLn := range strings.Split(rawLn, "\n") {
						subLn = strings.TrimRight(subLn, " \r\n")
						if subLn != "" {
							if len(allLines) == 0 || allLines[len(allLines)-1] != subLn {
								allLines = append(allLines, subLn)
							}
						}
					}
				}
				maxVisible := 16
				if len(allLines) > maxVisible {
					for _, ln := range allLines[:maxVisible] {
						sb.WriteString(m.renderTraceLine(ln))
						sb.WriteString("\n")
					}
					sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("    … and %d more process steps", len(allLines)-maxVisible)))
					sb.WriteString("\n")
				} else {
					for _, ln := range allLines {
						sb.WriteString(m.renderTraceLine(ln))
						sb.WriteString("\n")
					}
				}
			}
			// Live progress row: shows while the agent works AND while
			// background tool commands run AND while /compact is processing,
			// so the UI never looks dead between phases (anti-lag rule: the
			// spinner only ticks while something is actually running).
			if (m.agentWorking || m.toolRunning || m.compacting) && !m.askOpen && i == len(m.chat)-1 {
				phaseLabel := ""
				if m.toolRunning {
					phaseLabel = "⚙ executing tool commands…"
				} else {
					phase := m.agentPhase
					if phase == "" {
						phase = "thinking…"
					}
					if m.agentStep > 0 {
						phaseLabel = fmt.Sprintf("Step %d: %s", m.agentStep, phase)
					} else {
						phaseLabel = phase
					}
				}
				sb.WriteString("\n")
				sb.WriteString(m.styles.thinking.Render(m.spinner.View() + " " + phaseLabel))
				sb.WriteString("\n")
			}
		}

		// Background tool execution: the last message is the finished agent
		// reply — keep a live "executing tool" spinner under it until the
		// commands return.
		if cm.role == roleAgent && m.toolRunning && !m.askOpen && i == len(m.chat)-1 {
			sb.WriteString("\n")
			sb.WriteString(m.styles.thinking.Render(m.spinner.View() + " ⚙ executing tool commands…"))
			sb.WriteString("\n")
		}
	}

	// Truncate the cache to the chat length (after /clear, compaction, resume).
	if len(m.msgCache) > len(m.chat) {
		m.msgCache = m.msgCache[:len(m.chat)]
	}

	if len(m.chat) == 0 && m.agentWorking && !m.askOpen {
		sb.WriteString("\n")
		sb.WriteString(m.styles.thinking.Render(m.spinner.View() + " " + m.agentPhase))
	}

	if m.askOpen {
		sb.WriteString(m.renderAskBox())
	}
	content := strings.TrimRight(sb.String(), "\n")
	if m.dragSel.active {
		content = m.highlightSelection(content)
	}
	m.viewport.SetContent(content)
	m.lastContent = content
	// While a drag-select is active the viewport must NOT auto-scroll (follow
	// or past-bottom pinning): the anchor line was captured against the current
	// offset, and a mid-drag jump would make the highlight/selection cover
	// DIFFERENT text than what the user is pointing at (the "selection jumps
	// and only part of a line is selected" bug on freshly-streamed replies).
	// handleMouseClick sets follow=false on press; the guard also covers the
	// past-bottom pin for long chats scrolled up.
	if m.follow && !m.dragSel.active {
		m.viewport.GotoBottom()
	} else if m.viewport.PastBottom() && !m.dragSel.active {
		m.viewport.GotoBottom()
	}
}

// scrollToCompaction positions the viewport so the ✂ Compaction divider block
// (the L2 ledger) is visible right where the user is looking — instead of the
// fold happening out of view above the tail. Called once after a /compact
// resolves: the user asked to SEE the compaction, not read about it in a
// status line that scrolls past. The next user message re-pins the viewport
// (send() sets follow=true), so this never strands the chat off-screen.
func (m *Model) scrollToCompaction() {
	content := m.lastContent
	if content == "" {
		return
	}
	lines := strings.Split(content, "\n")
	target := -1
	for i, ln := range lines {
		if strings.Contains(ln, "✂ Compaction") {
			target = i
			break
		}
	}
	if target < 0 {
		return
	}
	h := m.viewport.Height()
	if h < 5 {
		h = 24
	}
	// Land the divider a few lines above the viewport bottom so the tail
	// below it is still peekable.
	y := target - (h - 4)
	if y < 0 {
		y = 0
	}
	m.viewport.SetYOffset(y)
	m.follow = false
}

// growCache extends msgCache to cover index i (bounded by maxHistory, which
// bounds chat itself). Missing slots default to the zero pmCache, which never
// matches a real message, forcing a re-render on the next refresh.
func (m *Model) growCache(i int) {
	for len(m.msgCache) <= i {
		m.msgCache = append(m.msgCache, pmCache{})
	}
}

// normalizeSel returns (yStart, xStart, yEnd, xEnd) in 2D document reading order,
// handling all 4 drag directions (down-right, up-left, down-left, up-right).
func normalizeSel(y0, x0, y1, x1 int) (int, int, int, int) {
	if y0 < y1 || (y0 == y1 && x0 <= x1) {
		return y0, x0, y1, x1
	}
	return y1, x1, y0, x0
}

// highlightSelection overlays reverse-video on the drag-selected rectangle of
// the viewport content (Phase 4). The selection is in content coordinates;
// only lines inside the visible window are styled (bounded work per frame).
func (m Model) highlightSelection(content string) string {
	sel := m.dragSel
	if !sel.active {
		return content
	}
	lines := strings.Split(content, "\n")
	top := m.viewport.YOffset()
	bottom := min(len(lines), top+m.viewport.Height())

	yStart, xStart, yEnd, xEnd := normalizeSel(sel.y0, sel.x0, sel.y1, sel.x1)

	for y := max(yStart, top); y <= yEnd && y < bottom; y++ {
		sx, ex := 0, 999999
		if y == yStart {
			sx = xStart
		}
		if y == yEnd {
			ex = xEnd
		}
		lines[y] = highlightRange(lines[y], sx, ex)
	}
	return strings.Join(lines, "\n")
}

// selectedText extracts the drag-selected rectangle from the viewport content
// as plain text (ANSI stripped, trailing whitespace trimmed per line). A
// zero-size selection (plain click) yields "".
func (m Model) selectedText() string {
	sel := m.dragSel
	if !sel.active {
		return ""
	}
	yStart, xStart, yEnd, xEnd := normalizeSel(sel.y0, sel.x0, sel.y1, sel.x1)

	lines := strings.Split(ansiStrip.ReplaceAllString(m.viewport.GetContent(), ""), "\n")
	var out []string
	for y := yStart; y <= yEnd && y < len(lines); y++ {
		ln := lines[y]
		w := lipgloss.Width(ln)
		if w == 0 {
			out = append(out, "")
			continue
		}

		sx, ex := 0, w-1
		if y == yStart {
			sx = min(xStart, w-1)
		}
		if y == yEnd {
			ex = min(xEnd, w-1)
		}
		if sx > ex {
			sx, ex = ex, sx
		}

		seg := sliceAnsiWidth(ln, sx, ex+1)
		out = append(out, strings.TrimRight(seg, " "))
	}
	return strings.Join(out, "\n")
}

// highlightRange wraps display columns [x0, x1] (inclusive) of an ANSI-styled
// line in reverse video — the drag-select highlight. Escape sequences are
// preserved in place (only visible cells advance the column counter), and a
// reverse-off is emitted instead of a full reset so surrounding colors are
// never clobbered.
func highlightRange(line string, x0, x1 int) string {
	if x1 < x0 {
		x0, x1 = x1, x0
	}
	const (
		reverseOn  = "\x1b[7m"
		reverseOff = "\x1b[27m"
	)

	var before, inside, after strings.Builder
	col := 0
	inEsc := false
	escBuf := strings.Builder{}
	target := &before
	flushEsc := func() {
		target.WriteString(escBuf.String())
		if target == &inside && escBuf.Len() > 0 {
			// Any ANSI style escape sequence inside the selection range might clear or change background — re-assert reverse video!
			target.WriteString(reverseOn)
		}
		escBuf.Reset()
	}

	for _, r := range line {
		if r == '\x1b' && !inEsc {
			inEsc = true
			escBuf.WriteRune(r)
			continue
		}
		if inEsc {
			escBuf.WriteRune(r)
			if r == 'm' {
				inEsc = false
				flushEsc()
			}
			continue
		}
		w := lipgloss.Width(string(r))
		if w == 0 {
			continue
		}
		if col < x0 {
			target = &before
		} else if col <= x1 {
			target = &inside
		} else {
			target = &after
		}
		target.WriteRune(r)
		col += w
		if col > x1 {
			// Escapes following the last selected cell belong to the tail — a
			// trailing reset must land after the reverse-off, never inside it.
			target = &after
		}
	}
	if inside.Len() == 0 {
		return line
	}
	return before.String() + reverseOn + inside.String() + reverseOff + after.String()
}

// renderAskBox renders the interactive popover inside the chat body — either
// the agent's multi-question clarify form or the native permission gate. The
// chat input is hidden while it shows (see renderBottom), so the popover reads
// as a focused form. Radio questions show ○/◉, checkbox questions ☐/☑, and
// each question gets a ✎ custom-answer row; ▸ marks the focused row.
func (m Model) renderAskBox() string {
	// Bounded: at most ~46 wide on big terminals, never wider than the chat
	// (narrow terminals clamp instead of overflowing).
	w := m.chatContentWidth() - 4
	if w > 46 {
		w = 46
	}
	if w < 20 {
		w = 20
	}
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(m.styles.title.Render("  " + m.askTitle))
	sb.WriteString("\n")

	// Permission mode: show exactly which commands are being decided.
	if m.askKind == askPermission {
		cmds := append(append([]string{}, m.askPermCmds...), m.askPermHard...)
		if len(cmds) > 0 {
			sb.WriteString("\n")
			for _, c := range cmds {
				sb.WriteString(m.styles.statusLeft.Render("    ⚙ " + clip(c, w-6)))
				sb.WriteString("\n")
			}
		}
	}

	items := m.askItems()
	lastQI := -1
	for flat, it := range items {
		if it.qi != lastQI {
			lastQI = it.qi
			q := m.askQuestions[it.qi]
			label := fmt.Sprintf("%d. %s", it.qi+1, q.header)
			if q.header == "" {
				label = fmt.Sprintf("%d.", it.qi+1)
			}
			if q.multiSelect {
				label += " [multiple]"
			}
			sb.WriteString("\n")
			sb.WriteString(m.styles.hint.Render("  " + label))
			sb.WriteString("\n")
			if q.question != "" {
				sb.WriteString(m.renderPlain(clip(q.question, w-2), w-2))
				sb.WriteString("\n")
			}
		}
		if it.kind == askItemOption {
			q := m.askQuestions[it.qi]
			selected := false
			if q.multiSelect {
				for _, oi := range m.askSel[it.qi] {
					if oi == it.oi {
						selected = true
						break
					}
				}
			} else {
				selected = m.askRadio[it.qi] == it.oi
			}
			marker := "  "
			if q.multiSelect {
				if selected {
					marker = "  ☑ "
				} else {
					marker = "  ☐ "
				}
			} else {
				if selected {
					marker = "  ◉ "
				} else {
					marker = "  ○ "
				}
			}
			row := marker + q.options[it.oi]
			if flat == m.askFocus {
				sb.WriteString(m.styles.sideSel.Render("  ▸ " + clip(row, w-4)))
			} else {
				sb.WriteString(m.styles.statusLeft.Render("    " + clip(row, w-6)))
			}
			sb.WriteString("\n")
		} else {
			row := "✎ type your own answer"
			if m.askCustomOpen && it.qi == m.askCustomIdx {
				row = "✎ " + m.input.Value() + " █"
			} else if m.askCustom[it.qi] != "" {
				row = "✎ " + clip(m.askCustom[it.qi], w-8)
			}
			if flat == m.askFocus {
				sb.WriteString(m.styles.sideSel.Render("  ▸ " + clip(row, w-4)))
			} else {
				sb.WriteString(m.styles.statusLeft.Render("    " + clip(row, w-6)))
			}
			sb.WriteString("\n")
		}
	}

	hint := "↑↓ move · space select · enter submit · esc cancel"
	if m.askKind == askPermission {
		hint = "↑↓ · space choose · enter submit · esc = deny"
	}
	sb.WriteString("\n")
	sb.WriteString(m.styles.hint.Render("  " + clip(hint, w-6)))
	// Thin-bordered floating card — the same popover chrome as every other
	// modal (popoverBox: 1px border + dark background). A bare text blob was
	// easy to miss against the chat; the bordered card makes the pending
	// decision unmistakable. Width = inner w + border 2 + padding 2.
	return m.styles.popoverBox.Width(w + 4).Render(sb.String())
}

// askHint is the bottom-bar hint shown in place of the hidden chat input
// while the ask popover is up.
func (m Model) askHint() string {
	if m.askKind == askPermission {
		return "⚠ answering in the popover above · ↑↓ · space · enter · esc = deny"
	}
	return "agent needs your input — answer in the popover above · ↑↓ · space · enter submit · esc cancel"
}

// ---- rendering -------------------------------------------------------------

func (m Model) View() tea.View {
	if m.width == 0 || m.height == 0 {
		m.width, m.height = 80, 24
		m.layout()
	}
	// Single view (Phase 2): header + body + fixed bottom input + status,
	// always. There is no separate landing layout anymore — on a fresh start
	// the brand block is the top element of the viewport content (see
	// refreshChat), and the input is always the same fixed-bottom one.
	baseCanvas := strings.Join([]string{m.renderHeader(), m.renderBody(), m.renderBottom(), m.renderStatus()}, "\n")

	content := baseCanvas
	switch {
	case m.connectOpen:
		content = m.overlayPopover(baseCanvas, m.renderConnectModalBox())
	case m.modelsOpen:
		content = m.overlayPopover(baseCanvas, m.renderModelsModalBox())
	case m.themeOpen:
		content = m.overlayPopover(baseCanvas, m.renderThemeModalBox())
	case m.historyOpen:
		content = m.overlayPopover(baseCanvas, m.renderHistoryModalBox())
	case m.queueOpen:
		content = m.overlayPopover(baseCanvas, m.renderQueueModalBox())
	case m.apikeyOpen:
		content = m.overlayPopover(baseCanvas, m.renderAPIKeyModalBox())
	case m.promptEditOpen:
		content = m.overlayPopover(baseCanvas, m.renderPromptEditModalBox())
	case m.suggestVisible():
		content = compositeOverlay(baseCanvas, m.renderSuggest(), m.width, m.height, overlayChatSuggest)
	}

	return m.makeView(content)
}

func (m Model) makeView(content string) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	// Mouse capture is always on so the wheel always scrolls the chat and
	// click-drag selects text to copy. There is deliberately no toggle — a
	// toggle let the terminal swallow wheel events and made scroll feel
	// broken (see removed /mouse + ctrl+m).
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "brocode " + m.version
	return v
}

func (m Model) renderHeader() string {
	title := " brocode · " + m.version
	if m.commit != "" && m.commit != "unknown" {
		title += " (" + m.commit + ")"
	}
	if m.plannerMode {
		title += "  PLANNER"
	} else {
		title += "  BUILDER"
	}
	left := m.styles.title.Render(title)

	win := "—"
	if m.window > 0 {
		win = fmtTokens(m.window)
	}
	// Context pressure = the worse of the forecast and the provider's last
	// reported input. The API number is settlement — it reflects what the
	// provider ACTUALLY counted, which is what compaction fires on.
	used := m.contextPressure()
	label := ""
	if m.actualTokens.input == 0 {
		label = "~" // pure forecast — no settlement yet
	}
	var rightText string
	if win == "—" {
		rightText = fmt.Sprintf("ctx %s%s", label, fmtTokens(used))
	} else {
		rightText = fmt.Sprintf("ctx %s%s / %s", label, fmtTokens(used), win)
	}
	right := m.styles.title.Render(rightText)

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return lipgloss.JoinHorizontal(lipgloss.Center,
		left, m.styles.statusLeft.Render(strings.Repeat(" ", gap)), right)
}

func (m Model) toggleCollapse() Model {
	anyFound := false
	anyCollapsed := false
	for _, cm := range m.chat {
		if cm.collapsible() {
			anyFound = true
			if cm.collapsed {
				anyCollapsed = true
				break
			}
		}
	}

	if !anyFound {
		m.status = "nothing to expand/collapse"
		return m
	}

	targetState := !anyCollapsed
	for i := range m.chat {
		if m.chat[i].collapsible() {
			m.chat[i].collapsed = targetState
		}
	}

	if targetState {
		m.status = "collapsed all blocks — ctrl+o to expand"
	} else {
		m.status = "expanded all blocks — ctrl+o to collapse"
	}
	m.refreshChat()
	return m
}

func (m Model) renderBody() string {
	chatPanel := m.viewport.View()
	if !m.showPanel {
		return chatPanel
	}
	sideTitle := m.styles.title.Render(" status ")
	sidePanel := m.styles.sideBoxIn.Render(sideTitle + "\n" + m.renderPanel())
	return lipgloss.JoinHorizontal(lipgloss.Top, chatPanel, " ", sidePanel)
}

// compositeOverlay overlays fg on top of bg canvas.
func compositeOverlay(bg, fg string, width, height int, mode overlayMode) string {
	if bg == "" {
		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, fg)
	}

	bgLines := strings.Split(bg, "\n")
	fgLines := strings.Split(fg, "\n")

	for len(bgLines) < height {
		bgLines = append(bgLines, strings.Repeat(" ", width))
	}
	if len(bgLines) > height {
		bgLines = bgLines[:height]
	}

	fgH := len(fgLines)
	fgW := 0
	for _, l := range fgLines {
		if w := lipgloss.Width(l); w > fgW {
			fgW = w
		}
	}

	var startX, startY int
	switch mode {
	case overlayChatSuggest:
		// Position popup directly above input box (inputTotal lines) + status (1)
		startY = height - fgH - inputTotal - statusHeight
		startX = 2
	case overlayPopoverPos:
		// Bottom-anchored above the input bar, centered — command-palette
		// style. startY is clamped so a tall popover grows upward, never over
		// the input or the status bar.
		startY = max(height-fgH-inputTotal-statusHeight, 0)
		startX = (width - fgW) / 2
	}
	if startX < 0 {
		startX = 0
	}

	for i, fgLine := range fgLines {
		targetY := startY + i
		if targetY < 0 {
			// Overlay taller than the canvas — drop the overflow from the top
			// so the visible part stays anchored (never a negative index).
			continue
		}
		if targetY >= len(bgLines) {
			break
		}
		bgLines[targetY] = compositeLine(bgLines[targetY], fgLine, startX, width)
	}

	return strings.Join(bgLines, "\n")
}

type overlayMode int

const (
	overlayChatSuggest overlayMode = iota
	overlayPopoverPos
)

// overlayPopover renders a modal as a floating popover: the base canvas is
// dimmed (backdrop), then the popover is composited bottom-anchored above
// the input bar — the VS Code command-palette feel. The popover never
// overlaps the input/status rows, so nothing is ever hidden by accident.
// On tiny terminals the popover is trimmed from the TOP (keeping its title
// row 0 visible) so it always fits in the space above the input.
func (m Model) overlayPopover(baseCanvas, popover string) string {
	dimmed := m.dimBaseCanvas(baseCanvas)
	if lines := strings.Split(popover, "\n"); len(lines) > m.height-headerHeight-inputTotal-statusHeight {
		// Popover taller than the available space — drop its trailing rows so
		// the title + first rows stay visible and the input bar is never
		// covered. Trimming the tail is safe: the footer hint is the least
		// critical content (scroll/list still works).
		keep := m.height - headerHeight - inputTotal - statusHeight
		if keep > 1 {
			popover = strings.Join(lines[:keep], "\n")
		}
	}
	return compositeOverlay(dimmed, popover, m.width, m.height, overlayPopoverPos)
}

// popoverFrame wraps every modal in the SAME chrome so all popovers share
// one design language: a thin-bordered floating card with a dark background,
// a small bold title, and a hairline rule. width includes border+padding.
func (m Model) popoverFrame(title, body string, width int) string {
	var sb strings.Builder
	sb.WriteString(m.styles.popoverTitle.Render(title))
	sb.WriteString("\n")
	// Hairline rule spans the full content width (box width minus border 2 +
	// padding 2) — a rule that stops short looks broken against the border.
	sb.WriteString(m.styles.popoverFooter.Render(strings.Repeat("─", max(width-4, 8))))
	sb.WriteString("\n")
	sb.WriteString(body)
	return m.styles.popoverBox.Width(width).Render(sb.String())
}

// dimBaseCanvas renders the whole base canvas with the faint backdrop style
// so the popover reads as a floating panel above a dimmed app.
func (m Model) dimBaseCanvas(baseCanvas string) string {
	lines := strings.Split(baseCanvas, "\n")
	for i, ln := range lines {
		if ln != "" {
			lines[i] = m.styles.backdrop.Render(ln)
		}
	}
	return strings.Join(lines, "\n")
}

func compositeLine(bgLine, fgLine string, startX, width int) string {
	plainBg := ansiStrip.ReplaceAllString(bgLine, "")
	bgW := lipgloss.Width(plainBg)
	if bgW < width {
		bgLine += strings.Repeat(" ", width-bgW)
	}

	fgW := lipgloss.Width(ansiStrip.ReplaceAllString(fgLine, ""))
	if fgW <= 0 {
		return bgLine
	}

	left := sliceAnsiWidth(bgLine, 0, startX)
	right := sliceAnsiWidth(bgLine, startX+fgW, width)

	return left + fgLine + right
}

func sliceAnsiWidth(s string, start, end int) string {
	if start >= end {
		return ""
	}
	var sb strings.Builder
	curCol := 0
	inEsc := false
	escBuf := strings.Builder{}

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\x1b' && i+1 < len(runes) && runes[i+1] == '[' {
			inEsc = true
			escBuf.WriteRune(r)
			continue
		}
		if inEsc {
			escBuf.WriteRune(r)
			if r == 'm' {
				inEsc = false
				sb.WriteString(escBuf.String())
				escBuf.Reset()
			}
			continue
		}

		w := lipgloss.Width(string(r))
		if w == 0 {
			continue
		}

		if curCol >= start && curCol < end {
			sb.WriteRune(r)
		}
		curCol += w
		if curCol >= end {
			break
		}
	}
	return sb.String()
}

// inputPrompt builds the prompt prefix shown inside the input box: the mode
// badge (Builder/Planner/Matcha), the queue indicator when items are queued,
// and the ❯ glyph. Shared by renderInput, renderInputForm, and refreshInputWidth
// so the typed-area math can never drift between them.
func (m Model) inputPrompt() string {
	promptStr := m.styles.prompt.Render("❯ ")

	// Queue indicator
	if len(m.queue) > 0 {
		qBadge := m.styles.statusRight.Render(fmt.Sprintf("📥 queued (%d)", len(m.queue)))
		promptStr = qBadge + " " + promptStr
	}
	return promptStr
}

// promptWidth returns the display width of the current input prompt prefix.
func (m Model) promptWidth() int {
	return lipgloss.Width(ansiStrip.ReplaceAllString(m.inputPrompt(), ""))
}

// inputBoxW is the available input box width (terminal width minus borders
// and padding).
func (m Model) inputBoxW() int {
	boxW := m.width - 4
	if boxW < 20 {
		boxW = 20
	}
	return boxW
}

// refreshInputWidth sizes the textinput to the available typed width. Called
// from layout() and from every state change that alters the prompt prefix
// (mode switch, queue mutations) — NEVER from View(), so the render path
// stays pure (pro-TUI rule: no side effects in View).
func (m *Model) refreshInputWidth() {
	availW := m.inputBoxW() - m.promptWidth()
	if availW < 10 {
		availW = 10
	}
	m.input.SetWidth(availW)
}

// longPromptThresholds — past this size the input collapses into a badge so
// the box can never grow out of its frame (doctrine P1: bounded at creation).
const (
	longPromptLines = 5
	longPromptChars = 200
)

// longPromptBadge renders the collapsed badge for an oversized prompt:
// "[Long prompt: 12 lines · 1.2 KB] · ctrl+e to view/edit". The full text
// stays in m.input and is sent unchanged — only the display collapses.
func (m Model) longPromptBadge() string {
	val := m.input.Value()
	lines := strings.Count(val, "\n") + 1
	kb := float64(len(val)) / 1024.0
	badge := fmt.Sprintf("[Long prompt: %d lines · %.1f KB]  · ctrl+e to view/edit", lines, kb)
	return m.styles.statusRight.Render(badge)
}

// longPromptOpen reports whether the input is oversized and should render as
// a collapsed badge (lines > 5 OR chars > 200 — the same spirit as the paste
// compaction pattern).
func (m Model) longPromptOpen() bool {
	val := m.input.Value()
	return strings.Count(val, "\n") >= longPromptLines || len(val) > longPromptChars
}

// renderBottom is the fixed bottom element above the status bar. While the
// ask popover is up the chat input is REPLACED by a thin hint — the popover
// IS the form (the user asked for the input to hide, not compete with the
// questions). The custom-answer editor re-shows the real input for typing.
func (m Model) renderBottom() string {
	if m.askOpen && !m.askCustomOpen {
		return m.styles.hint.Render("  " + m.askHint())
	}
	return m.renderInput()
}

// renderInput renders the input box with FIXED height at bottom.
// The box is always 3 lines content + 2 border = 5 total lines.
// Text scrolls horizontally within the box — it never grows vertically.
// An oversized prompt collapses into a badge (Phase 3) so the box can never
// grow out of its frame.
func (m Model) renderInput() string {
	boxW := m.inputBoxW()

	// Build prompt string
	promptStr := m.inputPrompt()

	// Calculate prompt width
	promptW := lipgloss.Width(ansiStrip.ReplaceAllString(promptStr, ""))

	// Available width for typed content
	availW := boxW - promptW
	if availW < 10 {
		availW = 10
	}

	// The input width was set by layout()/refreshInputWidth — View is pure.
	typedView := m.input.View()

	// Long prompt → collapse into a badge; the box never grows.
	if m.longPromptOpen() {
		content := promptStr + m.longPromptBadge()
		return m.styles.inputBoxOn.Width(boxW + 2).Height(inputHeight).Render(content)
	}

	// Ghost text for suggestions
	if m.suggestVisible() {
		items := suggestFiltered(m.input.Value())
		if m.suggestSel >= 0 && m.suggestSel < len(items) {
			typed := m.input.Value()
			target := items[m.suggestSel].cmd
			if strings.HasPrefix(target, typed) && len(target) > len(typed) {
				ghostSuffix := target[len(typed):]
				trimmed := strings.TrimRight(typedView, " ")
				typedView = trimmed + m.styles.sys.Render(ghostSuffix)
			}
		}
	}

	// Re-pad typedView to available width
	curW := lipgloss.Width(ansiStrip.ReplaceAllString(typedView, ""))
	if curW < availW {
		typedView += strings.Repeat(" ", availW-curW)
	}

	// Line count indicator (right-aligned) when input has multiple lines
	lineIndicator := ""
	inputVal := m.input.Value()
	lineCount := strings.Count(inputVal, "\n") + 1
	if lineCount > 1 {
		lineIndicator = m.styles.statusLeft.Render(fmt.Sprintf("  L%d", lineCount))
	}

	// FIXED height: always inputHeight (3) content lines.
	// The textinput is single-line (horizontal scroll), so content is always
	// 1 visual line. Height() pads to inputHeight to keep the box stable —
	// lipgloss Height is a minimum, never a maximum, so the content must
	// already fit. Using the constant guarantees the box never grows.
	content := promptStr + typedView + lineIndicator
	return m.styles.inputBoxOn.Width(boxW + 2).Height(inputHeight).Render(content)
}

// renderPromptEditModalBox renders the ctrl+e modal: a scroll-safe preview of
// the full prompt text with the line count, so an oversized prompt can be
// reviewed and edited without the input box ever growing. Content is clipped
// to the box width (Principle 1) and the box height is bounded.
func (m Model) renderPromptEditModalBox() string {
	w := min(72, m.width-4)
	if w < 40 {
		w = 40
	}
	h := min(20, m.height-8)
	if h < 8 {
		h = 8
	}

	var sb strings.Builder
	val := m.input.Value()
	lines := strings.Count(val, "\n") + 1
	sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("  %d lines · %d chars — typing below edits this prompt", lines, len(val))))
	sb.WriteString("\n\n")

	// Wrap the preview to the inner width and cap visible lines.
	preview := lipgloss.Wrap(val, w-6, "")
	previewLines := strings.Split(preview, "\n")
	maxVisible := h - 6
	for i, ln := range previewLines {
		if i >= maxVisible {
			sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("  … %d more lines", len(previewLines)-maxVisible)))
			sb.WriteString("\n")
			break
		}
		sb.WriteString("  " + m.styles.agent.Render(clipLong(ln, w-6)))
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
	sb.WriteString(m.styles.hint.Render("  type to edit · esc close"))
	return m.popoverFrame("prompt preview", sb.String(), w)
}

func (m Model) renderQueueModalBox() string {
	w := min(62, m.width-4)
	if w < 35 {
		w = 35
	}

	var sb strings.Builder

	if len(m.queue) == 0 {
		sb.WriteString(m.styles.statusLeft.Render("  queue is empty"))
		sb.WriteString("\n")
	} else {
		for i, qItem := range m.queue {
			marker := "  "
			if i == m.queueSel {
				marker = "❯ "
			}
			row := fmt.Sprintf("[%d] %s", i+1, clip(qItem, w-10))
			if i == m.queueSel {
				sb.WriteString("  ")
				sb.WriteString(m.styles.sideSel.Render(marker + row))
				sb.WriteString("\n")
			} else {
				sb.WriteString("  ")
				sb.WriteString(m.styles.statusLeft.Render(marker + row))
				sb.WriteString("\n")
			}
		}
	}

	sb.WriteString("\n")
	sb.WriteString(m.styles.popoverFooter.Render("↑↓ navigate · e edit · d delete · m merge · esc close"))

	return m.popoverFrame("prompt queue", sb.String(), w)
}

func (m Model) renderStatus() string {
	sessBadge := fmt.Sprintf("%s (%s)", m.projectName, m.sessionID)
	left := m.styles.statusLeft.Render(fmt.Sprintf("%s · %s", keys.shortHelp(), sessBadge))
	right := m.status
	if right == "" {
		if m.provider != "" && m.selectedModel != "" {
			right = fmt.Sprintf("%s · %s", m.provider, m.selectedModel)
		} else if m.provider != "" {
			right = fmt.Sprintf("%s · %d msgs", m.provider, len(m.chat))
		} else {
			right = fmt.Sprintf("%d msgs", len(m.chat))
		}
		right = m.styles.statusRight.Render(right)
	} else {
		right = m.styles.statusRight.Render(right)
	}
	return lipgloss.JoinHorizontal(lipgloss.Center,
		left, m.styles.statusLeft.Render("   │   "), right)
}

func (m Model) renderChatMsg(cm chatMsg) string {
	w := m.chatContentWidth()
	if w < 10 {
		w = 10
	}
	var sb strings.Builder
	switch cm.role {
	case roleUser:
		sb.WriteString(m.renderUserMsg(cm.text, w))
	case roleAgent:
		sb.WriteString(m.renderPlain(cm.text, w))
		divLen := w
		if divLen > 60 {
			divLen = 60
		}
		divider := m.styles.sys.Render(strings.Repeat("·", divLen))
		sb.WriteString("\n" + divider + "\n")
	case roleTool:
		sb.WriteString(m.renderToolMsg(cm, w))
	default:
		if strings.Contains(cm.text, "context compact") {
			// The L2 compaction ledger renders as a centered ✂ divider block,
			// not a plain "system:" line — the same visual language the top
			// agentic CLIs use (Claude Code's "────  ✂  Compaction  ────").
			sb.WriteString(m.renderCompactBlock(cm, w))
		} else {
			sb.WriteString(renderLabeled(m.styles.sys, "system", cm.text, w))
		}
	}
	if cm.role != roleTool && cm.collapsible() {
		if cm.collapsed {
			sb.WriteString("  ")
			sb.WriteString(m.styles.statusLeft.Render("▸ " + cm.summary))
			sb.WriteString("\n")
		} else if isEchoContent(cm.content) {
			// An echoed tool-result block expanded in place renders like a tool
			// row — dim, labeled, never white prose — so the transcript stays
			// readable even when the user opens it with ctrl+o.
			sb.WriteString(renderLabeled(m.styles.tool, "tool output", stripToolResultPrefix(cm.content), w))
		} else if isDiffContent(cm.content) {
			// An auto-applied file edit expanded in place renders as a real
			// unified diff — additions green, deletions red, context dim — so
			// the user sees exactly what changed (Ctrl+O to expand/collapse).
			sb.WriteString(m.renderDiffContent(cm.content, w))
		} else {
			sb.WriteString(renderLabeled(m.styles.agent, "", cm.content, w))
		}
	}
	return sb.String()
}

// renderCompactBlock renders the L2 compaction ledger as a divider-centered
// block: a full-width ─── ✂ Compaction ─── hairline, the process summary on
// the next line, then the ledger body. This makes a compaction read as a
// deliberate, visible milestone in the transcript instead of another "system:"
// line that scrolls past unnoticed.
func (m Model) renderCompactBlock(cm chatMsg, w int) string {
	var sb strings.Builder

	// Hairline divider with the ✂ marker centered. The marker " ✂ Compaction "
	// is 14 display cells; the two ── runs share the remaining width exactly,
	// so the line NEVER overflows the content width — even on a narrow
	// terminal (a marker wider than the width is clipped, never ragged).
	marker := " ✂ Compaction "
	divLen := w - 2
	if divLen > 44 {
		divLen = 44
	}
	mLen := lipgloss.Width(marker)
	var divider string
	if divLen <= mLen {
		divider = m.styles.sys.Render(clip(marker, divLen))
	} else {
		side := (divLen - mLen) / 2
		divider = m.styles.sys.Render(strings.Repeat("─", side) + marker + strings.Repeat("─", divLen-mLen-side))
	}
	sb.WriteString(divider)
	sb.WriteString("\n")

	// The notice line ("🔄 context compact — …") becomes the summary header.
	notice := cm.text
	if idx := strings.Index(cm.text, "\n"); idx >= 0 {
		notice = cm.text[:idx]
		cm.text = cm.text[idx+1:]
	}
	notice = strings.Replace(notice, "🔄 context compact — ", "", 1)
	// The summary line wraps like the body — a long "saved … (was 150.0k /
	// 135.0k)" notice must never overflow a narrow terminal.
	for _, ln := range strings.Split(lipgloss.Wrap("  ✓ "+notice, w-2, ""), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		sb.WriteString(m.styles.ok.Render(ln))
		sb.WriteString("\n")
	}

	// Ledger body below the header, indented like a summary.
	body := lipgloss.Wrap(cm.text, w-4, "")
	for _, ln := range strings.Split(body, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		sb.WriteString(m.styles.statusLeft.Render("    " + ln))
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderToolMsg renders an agentic tool execution row. It is deliberately NOT
// styled like a user message: no blue ▌ bar, no sender label — just a dim,
// italic ⚙ line that reads as a system event. Collapsed by default with a
// clean gap between the icon and the label (the old "▸ ⚙️Tool Executed"
// cramped look is gone); ctrl+o reveals the full command output.
func (m Model) renderToolMsg(cm chatMsg, w int) string {
	if cm.collapsible() {
		if cm.collapsed {
			// The renderer owns the icon — cm.summary must not carry its own
			// (that would render a double glyph). Edit cards (summary starts
			// with "Edit(") use ✎ instead of the ⚙ tool gear so the interleaved
			// file changes read as edits, not commands.
			icon := "⚙"
			if strings.HasPrefix(cm.summary, "Edit(") {
				icon = "✎"
			}
			return "  " + m.styles.tool.Render(icon+"  "+cm.summary) + "  " + m.styles.statusLeft.Render("ctrl+o to view")
		}
		if isDiffContent(cm.content) {
			// An expanded edit card renders as a real unified diff — additions
			// green, deletions red (same as the old folded edit block).
			return m.renderDiffContent(cm.content, w)
		}
		outStr := strings.TrimSpace(stripToolResultPrefix(cm.content))
		if outStr == "" {
			outStr = "(command completed with no stdout/stderr output)"
		}
		return renderLabeled(m.styles.tool, "tool output", outStr, w)
	}
	return renderLabeled(m.styles.tool, "tool", stripToolResultPrefix(cm.text), w)
}

// isDiffContent reports whether a collapsible block's content is an
// auto-applied file-edit diff (✎ file · N lines header) rather than an echo,
// thinking trace, or diff demo.
func isDiffContent(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "✎ ")
}

// renderDiffContent renders a folded file-edit diff with real diff coloring:
// ✎ file headers dim-cyan, + additions green, - deletions red, @@@ hunk marks
// and context lines dim. Kept line-wise so a huge diff can never overflow the
// wrap width (clipLong) — the whole block is already bounded by the collapsible.
func (m Model) renderDiffContent(content string, w int) string {
	wrapW := w - 2
	if wrapW < 10 {
		wrapW = 10
	}
	var sb strings.Builder
	for _, rawLn := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(rawLn)
		switch {
		case strings.HasPrefix(trimmed, "✎ "):
			sb.WriteString(m.styles.title.Render(clip(rawLn, wrapW)))
			sb.WriteString("\n")
		case strings.HasPrefix(rawLn, "+"):
			sb.WriteString(m.styles.ok.Render("  " + clip(rawLn, wrapW-2)))
			sb.WriteString("\n")
		case strings.HasPrefix(rawLn, "-"):
			sb.WriteString(m.styles.err.Render("  " + clip(rawLn, wrapW-2)))
			sb.WriteString("\n")
		case strings.HasPrefix(rawLn, "@@"):
			sb.WriteString(m.styles.statusLeft.Render("  " + clip(rawLn, wrapW-2)))
			sb.WriteString("\n")
		case trimmed == "":
			sb.WriteString("\n")
		default:
			sb.WriteString(m.styles.statusLeft.Render("  " + clip(rawLn, wrapW-2)))
			sb.WriteString("\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// isEchoContent reports whether a collapsible block's content is an echoed
// tool-result payload (model repeated the [SYSTEM TOOL RESULT]/[SYSTEM ASK
// RESULT] block verbatim) rather than a thinking trace or diff hunk.
func isEchoContent(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "[SYSTEM TOOL RESULT]") || strings.HasPrefix(s, "[SYSTEM ASK RESULT]")
}

// stripToolResultPrefix removes the internal "[SYSTEM TOOL RESULT]\n" or
// "[SYSTEM ASK RESULT]\n" plumbing header before rendering tool output — users
// should see the command output, never the transport prefix the loop uses
// internally.
func stripToolResultPrefix(s string) string {
	for _, p := range []string{"[SYSTEM TOOL RESULT]", "[SYSTEM ASK RESULT]"} {
		if strings.HasPrefix(s, p) {
			if idx := strings.Index(s, "\n"); idx >= 0 {
				return s[idx+1:]
			}
			return ""
		}
	}
	return s
}

func (m Model) renderUserMsg(text string, w int) string {
	body := lipgloss.Wrap(text, w-4, "")
	lines := strings.Split(body, "\n")
	bar := m.styles.userBar.Render("▌ ")
	emptyBar := m.styles.userBar.Render("▌")

	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln == "" {
			out = append(out, emptyBar)
		} else {
			out = append(out, bar+ln)
		}
	}
	return strings.Join(out, "\n")
}

func (m Model) renderMarkdown(line string) string {
	plain := ansiStrip.ReplaceAllString(line, "")
	trimmed := strings.TrimSpace(plain)

	if strings.HasPrefix(trimmed, "### ") {
		text := strings.TrimPrefix(trimmed, "### ")
		return m.styles.title.Render("   ") + m.styles.agent.Render(m.styles.title.Render(" "+m.renderInlineMarkdown(text)))
	}
	if strings.HasPrefix(trimmed, "## ") {
		text := strings.TrimPrefix(trimmed, "## ")
		return m.styles.title.Render("  ") + m.styles.agent.Render(m.styles.title.Render(" "+m.renderInlineMarkdown(text)))
	}
	if strings.HasPrefix(trimmed, "# ") {
		text := strings.TrimPrefix(trimmed, "# ")
		return m.styles.title.Render(" ") + m.styles.agent.Render(m.styles.title.Render(" "+m.renderInlineMarkdown(text)))
	}

	if strings.HasPrefix(trimmed, "```") {
		return m.styles.statusRight.Render("  " + trimmed)
	}

	if strings.HasPrefix(trimmed, "> ") {
		text := strings.TrimPrefix(trimmed, "> ")
		return m.styles.statusLeft.Render("  │ ") + m.renderInlineMarkdown(text)
	}

	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		bullet := "  •"
		rest := trimmed[2:]
		return m.styles.agent.Render(bullet) + " " + m.renderInlineMarkdown(rest)
	}

	if len(trimmed) > 2 && trimmed[0] >= '0' && trimmed[0] <= '9' {
		for i := 1; i < len(trimmed); i++ {
			if trimmed[i] == '.' && i+2 < len(trimmed) && trimmed[i+1] == ' ' {
				num := trimmed[:i+1]
				rest := trimmed[i+2:]
				return m.styles.statusRight.Render("  "+num) + " " + m.renderInlineMarkdown(rest)
			}
			if trimmed[i] < '0' || trimmed[i] > '9' {
				break
			}
		}
	}

	return m.renderInlineMarkdown(plain)
}

func (m Model) renderInlineMarkdown(text string) string {
	var sb strings.Builder
	i := 0
	for i < len(text) {
		if text[i] == '`' {
			end := strings.Index(text[i+1:], "`")
			if end >= 0 {
				inner := text[i+1 : i+1+end]
				sb.WriteString(m.styles.statusRight.Render(inner))
				i += end + 2
				continue
			}
		}
		if i+1 < len(text) && text[i] == '*' && text[i+1] == '*' {
			end := strings.Index(text[i+2:], "**")
			if end >= 0 {
				inner := text[i+2 : i+2+end]
				sb.WriteString(m.styles.title.Render(m.renderInlineMarkdown(inner)))
				i += end + 4
				continue
			}
		}
		sb.WriteByte(text[i])
		i++
	}
	return sb.String()
}

func (m Model) renderPlain(text string, w int) string {
	wrapW := w - 2
	if wrapW < 10 {
		wrapW = 10
	}

	st := plainState{}
	for _, line := range strings.Split(text, "\n") {
		m.renderPlainLine(line, wrapW, &st)
	}
	st.flushTable(m, wrapW)
	return strings.Join(st.out, "\n") + "\n"
}

// plainState carries the per-line render state of an agent reply across line
// runs: the fence state (``` inside/outside) and the pending markdown-table
// rows. Both the full renderer and the incremental streaming renderer feed
// lines through renderPlainLine so a table is rendered ONCE — column-aligned
// as a block — instead of one ragged row at a time.
type plainState struct {
	inCode       bool
	pendingTable []string // consecutive table rows awaiting alignment
	out          []string // rendered lines
}

// flushTable renders any buffered table rows as one aligned block and clears
// the buffer. No-op when the buffer is empty.
func (st *plainState) flushTable(m Model, wrapW int) {
	if len(st.pendingTable) == 0 {
		return
	}
	st.out = append(st.out, m.renderTableBlock(st.pendingTable, wrapW))
	st.pendingTable = nil
}

// renderPlainLine renders one text line of an agent reply into st.out. The
// code-block state (st.inCode) carries across calls so line runs render with
// the correct fence state (full render, or the incremental streaming renderer
// which continues the state across ticks). Consecutive markdown-table rows are
// buffered in st.pendingTable and flushed as one aligned block the moment a
// non-table line arrives (or via flushTable at end of input) — so a table
// never renders as ragged per-row text.
func (m Model) renderPlainLine(line string, wrapW int, st *plainState) {
	trimmed := strings.TrimSpace(line)

	// Leaving a table (code fence, prose, etc.) — flush the aligned block.
	if len(st.pendingTable) > 0 && !isTableRow(trimmed) {
		st.flushTable(m, wrapW)
	}

	if strings.HasPrefix(trimmed, "```") {
		st.inCode = !st.inCode
		st.out = append(st.out, m.styles.statusRight.Render("  "+trimmed))
		return
	}
	if st.inCode {
		clipped := clipLong(line, wrapW-2)
		highlighted := highlightCodeLine(clipped)
		st.out = append(st.out, m.styles.statusLeft.Render("  "+highlighted))
		return
	}

	if isTableRow(trimmed) {
		st.pendingTable = append(st.pendingTable, line)
		return
	}

	switch {
	case strings.HasPrefix(trimmed, "+") && !strings.HasPrefix(trimmed, "+++"):
		st.out = append(st.out, wrapStyled(line, wrapW, m.styles.ok))
	case strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "---"):
		st.out = append(st.out, wrapStyled(line, wrapW, m.styles.err))
	case strings.HasPrefix(trimmed, "⚡"):
		st.out = append(st.out, wrapStyled(line, wrapW, m.styles.statusLeft))
	default:
		st.out = append(st.out, lipgloss.Wrap(m.renderMarkdown(line), wrapW, ""))
	}
}

// isTableRow reports whether a line is a markdown table row (a '|' pipe
// delimited line with at least one cell on each side).
func isTableRow(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "|") && strings.Count(t, "|") >= 2
}

// isTableDivider reports whether a row is a markdown table divider (only
// '|', '-', ':' and spaces — e.g. "|:---|---:|").
func isTableDivider(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "|") {
		return false
	}
	for _, ch := range t {
		if ch != '|' && ch != '-' && ch != ':' && ch != ' ' && ch != '\t' {
			return false
		}
	}
	return true
}

// renderTableBlock renders buffered markdown table rows as ONE aligned grid:
// column widths come from the widest cell across all rows (bounded to the wrap
// width), alignment (:--- left, ---: right, :--: center) comes from the first
// divider row, and the divider renders as a ──┼── hairline. The header row and
// body rows share the same pipes, so the columns line up vertically — the
// ragged "table" look (each row independently wrapped) is gone.
func (m Model) renderTableBlock(rows []string, wrapW int) string {
	type trow struct {
		cells []string
		div   bool
	}
	var parsed []trow
	ncols := 0
	for _, raw := range rows {
		t := strings.TrimSpace(raw)
		parts := strings.Split(t, "|")
		// Strip the empty cells produced by the leading/trailing pipes.
		if len(parts) > 0 && strings.TrimSpace(parts[0]) == "" {
			parts = parts[1:]
		}
		if len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
			parts = parts[:len(parts)-1]
		}
		cells := make([]string, 0, len(parts))
		for _, p := range parts {
			cells = append(cells, strings.TrimSpace(p))
		}
		if len(cells) > ncols {
			ncols = len(cells)
		}
		parsed = append(parsed, trow{cells: cells, div: isTableDivider(t)})
	}
	if ncols == 0 {
		return ""
	}

	// Per-column alignment from the first divider row (:--- left, ---: right,
	// :--: center); plain rows default to left.
	align := make([]string, ncols)
	for i := range align {
		align[i] = "left"
	}
	for _, r := range parsed {
		if !r.div {
			continue
		}
		for i, c := range r.cells {
			if i >= ncols {
				break
			}
			switch {
			case strings.HasPrefix(c, ":") && strings.HasSuffix(c, ":"):
				align[i] = "center"
			case strings.HasSuffix(c, ":"):
				align[i] = "right"
			}
		}
		break
	}

	// Column widths = widest non-divider cell, shrunk to fit wrapW.
	widths := make([]int, ncols)
	for _, r := range parsed {
		if r.div {
			continue
		}
		for i, c := range r.cells {
			if i >= ncols {
				break
			}
			if w := lipgloss.Width(ansiStrip.ReplaceAllString(c, "")); w > widths[i] {
				widths[i] = w
			}
		}
	}
	maxW := wrapW - 4 // "  " prefix + trailing "│" + gaps
	if maxW < ncols {
		maxW = ncols
	}
	total := 0
	for _, w := range widths {
		total += w
	}
	for total > maxW {
		mi := 0
		for i, w := range widths {
			if w > widths[mi] {
				mi = i
			}
		}
		if widths[mi] <= 1 {
			break
		}
		widths[mi]--
		total--
	}

	pipe := m.styles.statusLeft.Render("│")
	var sb strings.Builder
	for _, r := range parsed {
		if r.div {
			var d strings.Builder
			d.WriteString("  ")
			for i := 0; i < ncols; i++ {
				if i > 0 {
					d.WriteString("┼")
				}
				seg := strings.Repeat("─", widths[i])
				switch align[i] {
				case "right":
					seg = seg + ":"
				case "center":
					if len(seg) > 0 {
						seg = ":" + seg[1:]
					}
					seg = seg + ":"
				default:
					seg = ":" + seg
				}
				d.WriteString(seg)
			}
			sb.WriteString(m.styles.sys.Render(d.String()))
			sb.WriteString("\n")
			continue
		}
		sb.WriteString("  ")
		for i := 0; i < ncols; i++ {
			if i > 0 {
				sb.WriteString(pipe)
			}
			cell := ""
			if i < len(r.cells) {
				cell = m.renderInlineMarkdown(r.cells[i])
			}
			plain := ansiStrip.ReplaceAllString(cell, "")
			cw := lipgloss.Width(plain)
			if cw > widths[i] {
				cell = clipLong(cell, widths[i])
				cw = lipgloss.Width(ansiStrip.ReplaceAllString(cell, ""))
			}
			pad := widths[i] - cw
			switch align[i] {
			case "right":
				sb.WriteString(strings.Repeat(" ", pad))
				sb.WriteString(cell)
			case "center":
				l := pad / 2
				sb.WriteString(strings.Repeat(" ", l))
				sb.WriteString(cell)
				sb.WriteString(strings.Repeat(" ", pad-l))
			default:
				sb.WriteString(cell)
				sb.WriteString(strings.Repeat(" ", pad))
			}
		}
		sb.WriteString("│")
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderStreamingAgent renders the streaming agent reply INCREMENTALLY. Each
// completed text line (terminated by '\n') is rendered exactly once and never
// re-wrapped; every stream tick re-renders only the trailing partial line.
// Rendering cost per tick is O(new suffix) instead of O(whole reply) — a
// multi-KB reply used to be fully re-parsed, re-wrapped and re-highlighted
// 30×/sec, which saturated the event loop and made typing lag. The rendered
// output is byte-identical to a fresh full render (covered by tests).
func (m *Model) renderStreamingAgent(text string, w int) string {
	wrapW := w - 2
	if wrapW < 10 {
		wrapW = 10
	}

	c := &m.streamCache
	nl := strings.LastIndexByte(text, '\n')
	complete, partial := "", text
	if nl >= 0 {
		complete, partial = text[:nl+1], text[nl+1:]
	}

	if complete != c.text {
		// Render only the newly completed lines. c.text always ends with '\n'
		// (or is empty), so the suffix starts at a fresh line boundary and the
		// running code-block / pending-table state stays correct across ticks.
		if !strings.HasPrefix(complete, c.text) {
			// The text was replaced or shrunk (not during normal streaming,
			// but be robust) — reset and re-render from scratch instead of
			// slicing a shorter prefix out of a longer cache.
			c.text, c.view, c.inCode = "", "", false
			c.pendingTable = nil
		}
		suffix := strings.TrimSuffix(complete[len(c.text):], "\n")
		st := plainState{inCode: c.inCode, pendingTable: append([]string(nil), c.pendingTable...)}
		for _, ln := range strings.Split(suffix, "\n") {
			m.renderPlainLine(ln, wrapW, &st)
		}
		var sb strings.Builder
		if len(st.out) > 0 {
			sb.WriteString(strings.Join(st.out, "\n"))
			sb.WriteString("\n")
		}
		c.view += sb.String()
		// Table rows still buffered (the suffix ended inside a table) stay
		// pending; they render as one aligned block when the table closes or in
		// the final full render once streaming ends.
		c.pendingTable = st.pendingTable
		c.inCode = st.inCode
		c.text = complete
	}

	// The partial line changes every tick — re-render it with a copy of the
	// block state so an unterminated ``` fence never toggles state early.
	// A partial TABLE row can't be aligned yet (columns unknown) — show it
	// inline; it is buffered properly once the line completes.
	pst := plainState{inCode: c.inCode}
	c.partial = partial
	if isTableRow(strings.TrimSpace(partial)) {
		c.partialV = m.renderInlineMarkdown(strings.TrimSpace(partial))
	} else {
		m.renderPlainLine(partial, wrapW, &pst)
		c.partialV = strings.Join(pst.out, "\n")
	}

	body := c.view + c.partialV
	// renderPlain renders one "\n" after every split element (including the
	// phantom empty element for a trailing '\n') plus the separator blank
	// line before the divider — together always "\n\n" after the last
	// visible content. Matching that keeps the incremental view byte-identical
	// to a full render.
	divLen := w
	if divLen > 60 {
		divLen = 60
	}
	divider := m.styles.sys.Render(strings.Repeat("·", divLen))
	return body + "\n\n" + divider + "\n"
}

func wrapStyled(line string, w int, style lipgloss.Style) string {
	segs := strings.Split(lipgloss.Wrap(line, w, ""), "\n")
	for i, s := range segs {
		segs[i] = style.Render(s)
	}
	return strings.Join(segs, "\n")
}

func (m Model) renderTraceLine(ln string) string {
	trimmed := strings.TrimSpace(ln)
	if strings.Contains(ln, " +  ") || strings.Contains(ln, " + ") || (strings.HasPrefix(trimmed, "+") && !strings.HasPrefix(trimmed, "+++")) {
		return m.styles.ok.Render("  " + ln)
	}
	if strings.Contains(ln, " -  ") || strings.Contains(ln, " - ") || (strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "---")) {
		return m.styles.err.Render("  " + ln)
	}
	if strings.HasPrefix(trimmed, "●") {
		return m.styles.thinking.Render("  " + ln)
	}
	if strings.HasPrefix(trimmed, "⎿") || strings.HasPrefix(trimmed, "@@") {
		return m.styles.statusLeft.Render("  " + ln)
	}
	return m.styles.statusLeft.Render("  " + ln)
}

func clipLong(line string, maxW int) string {
	plain := ansiStrip.ReplaceAllString(line, "")
	if lipgloss.Width(plain) <= maxW {
		return line
	}
	runes := []rune(plain)
	for lipgloss.Width(string(runes)) > maxW-1 && len(runes) > 0 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func renderLabeled(style lipgloss.Style, label, text string, w int) string {
	body := lipgloss.Wrap(text, w-4, "")
	lines := strings.Split(body, "\n")
	var sb strings.Builder
	if label == "" {
		for _, ln := range lines {
			if ln != "" {
				sb.WriteString(style.Render("  " + ln))
				sb.WriteString("\n")
			}
		}
		return sb.String()
	}
	if lines[0] == "" {
		sb.WriteString(style.Render(label + ":"))
		sb.WriteString("\n")
	} else {
		sb.WriteString(style.Render(label + ": " + lines[0]))
		sb.WriteString("\n")
	}
	for _, ln := range lines[1:] {
		sb.WriteString(style.Render("  " + ln))
		sb.WriteString("\n")
	}
	return sb.String()
}

func (m Model) statusGlyph(status string) (string, lipgloss.Style) {
	switch status {
	case "done", "ok", "spawn_ok", "bash_ok", "read_ok", "write_ok":
		return "✓", m.styles.ok
	case "error", "err", "spawn_err", "bash_err", "read_err", "write_err":
		return "✗", m.styles.err
	case "spawn", "subagent":
		return "⚡", m.styles.statusRight
	case "bash", "exec":
		return "❯", m.styles.prompt
	case "read", "view":
		return "📖", m.styles.statusLeft
	default:
		return "…", m.styles.thinking
	}
}

func appendChat(chat []chatMsg, msgs ...chatMsg) []chatMsg {
	chat = append(chat, msgs...)
	if len(chat) > maxHistory {
		chat = chat[len(chat)-maxHistory:]
	}
	return chat
}

func prependActivity(items []activityItem, news ...activityItem) []activityItem {
	all := make([]activityItem, 0, len(news)+len(items))
	all = append(all, news...)
	all = append(all, items...)
	if len(all) > maxActivity {
		all = all[:maxActivity]
	}
	return all
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…(truncated)"
}

func streamTickCmd() tea.Cmd {
	return tea.Tick(time.Second/streamFPS, func(time.Time) tea.Msg { return streamTickMsg{} })
}

func copyToClipboard(text string) error {
	if text == "" {
		return nil
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	fmt.Printf("\x1b]52;c;%s\x07", b64)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		}
	case "windows":
		cmd = exec.Command("clip")
	}

	if cmd != nil {
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return nil
}
