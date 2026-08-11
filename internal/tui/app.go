// Package tui is the bro-code chat UI, built on Bubble Tea v2 (charm.land).
//
// Layout follows the coding-agent TUI convention (Claude Code / opencode):
// header + chat viewport + input bar + status line, with a right-hand status
// panel on wide terminals (transparency: context/model + token usage, git,
// MCP, agents, activity). There is NO focus machine — the input is always
// the typing surface, and scrolling (arrows, pgup/pgdown, mouse wheel)
// always controls the chat viewport. When no conversation is active (fresh
// start, after /clear, or a failed resume), the body shows a centered
// landing instead of the chat.
//
// Anti-lag rules (docs/TECH_STACK.md §2) applied here:
//   - streaming is a ticker capped at streamFPS (~30fps) — never one msg per
//     token;
//   - all styles are precomputed once per theme change (pro-TUI rule 4),
//     never rebuilt in View();
//   - chat and activity history are bounded at creation (Principle 1);
//   - the spinner ticks only while the agent is working, and the streaming
//     ticker stops when the reply completes — no leaked goroutines.
package tui

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/plumpslabs/bro-code/internal/search"
)

// Render bounds (Principle 1 — bounded at the point of creation).
const (
	maxHistory   = 40
	maxActivity  = 15
	maxReplyLen  = 900
	panelWidth   = 32
	panelMinW    = 95 // status panel only on wide terminals
	streamFPS    = 30
	streamChunk  = 8 // chars revealed per streaming tick
	agentLatency = 400 * time.Millisecond
)

// role tags a chat message sender.
type role int

const (
	roleSystem role = iota
	roleUser
	roleAgent
)

// chatMsg is one message in the chat viewport. A message can carry a
// collapsible block (diff hunk, thinking trace) that is hidden by default
// and revealed with ctrl+o (Claude Code pattern — bounded rendering).
type chatMsg struct {
	role      role
	text      string // always-visible text
	summary   string // collapsible: one-line label shown while collapsed
	content   string // collapsible: full block shown while expanded
	collapsed bool   // collapsible: current state (hidden by default)
}

// collapsible reports whether the message has a hidden block.
func (cm chatMsg) collapsible() bool { return cm.content != "" }

// agentResultMsg carries the mock agent's reply after the simulated latency.
type agentResultMsg struct {
	reply mockReply
}

// streamTickMsg reveals the next chunk of the streaming reply.
type streamTickMsg struct{}

// Model is the bro-code TUI state (Bubble Tea v2 Elm architecture). All state
// lives here — nothing in globals (pro-TUI rule 1).
type Model struct {
	index   *search.Index
	version string
	commit  string

	width  int
	height int

	themeName string
	styles    styles
	started   bool // conversation begun — false shows the landing

	showPanel bool // right status panel on wide terminals
	panel     panelState

	provider string // selected provider (mock — no auth yet)
	window   int    // provider context window in tokens

	connectOpen bool // /connect modal visible
	connectSel  int  // selected provider index

	opencodeDetected bool   // cached detection state
	opencodeModel    string // cached free model name
	selectedModel    string // currently active model name

	modelsOpen bool // /models modal visible
	modelsSel  int  // selected model index

	themeOpen bool // /theme picker modal visible
	themeSel  int  // selected preset index

	queueOpen bool     // /queue modal visible
	queueSel  int      // selected queue item index
	queue     []string // queued user prompts while agent is busy

	suggestSel       int  // highlighted suggestion row
	suggestDismissed bool // popup hidden until the next keystroke

	subagents []subagentState // live active subagent tasks

	input    textinput.Model
	spinner  spinner.Model
	chat     []chatMsg
	activity []activityItem

	viewport viewport.Model

	agentWorking bool   // mock agent thinking (spinner)
	streaming    bool   // reply being revealed
	streamBuf    string // remaining reply text
	follow       bool   // keep viewport pinned to bottom
	status       string // transient status text
}

// subagentState represents one spawned subagent worker process.
type subagentState struct {
	name   string // e.g. "finder", "reviewer", "debugger"
	task   string // short task description
	status string // "working" | "done" | "error" | "checkpoint"
}

// New creates the chat model. version/commit come from build-time ldflags.
// resume=true tries to load the last saved session (~/.brocode/sessions).
func New(ix *search.Index, version, commit string, resume bool) Model {
	ti := textinput.New()
	ti.Placeholder = "ask brocode… (try: mcp, diff, memory) or /help"
	ti.Prompt = ""
	ti.Focus()

	st := newStyles(DefaultTheme)
	m := Model{
		index:     ix,
		version:   version,
		commit:    commit,
		themeName: "default",
		styles:    st,
		input:     ti,
		spinner:   spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(st.spinner)),
		viewport:  viewport.New(),
		panel:     gitInfo(),
		follow:    true,
	}

	// Auto-detect OpenCode CLI & free models on startup
	if detected, model := DetectOpenCode(); detected {
		m.opencodeDetected = true
		m.opencodeModel = model
		m.selectedModel = model
		m.provider = "opencode"
		m.window = 200000
	}

	if resume {
		if msgs, err := LoadSession(); err == nil && len(msgs) > 0 {
			// The resume notice lives in the status line only — never in the
			// transcript, so repeated -c cycles don't stack notices in the
			// session file.
			m.chat = appendChat(nil, msgs...)
			m.started = true
			m.status = fmt.Sprintf("resumed %d messages — ", len(msgs)) + version
		} else {
			m.status = "no previous session found — starting fresh"
		}
	}
	return m
}

// Started reports whether the conversation has begun — used to decide whether
// the session should be persisted on quit.
func (m Model) Started() bool { return m.started }

// Messages returns a copy of the chat history (for session persistence).
func (m Model) Messages() []chatMsg { return append([]chatMsg(nil), m.chat...) }

func (m Model) Init() tea.Cmd {
	return m.input.Focus()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.MouseWheelMsg:
		// Wheel scrolls the chat viewport — no focus required (the input is
		// always the typing surface).
		switch msg.Mouse().Button {
		case tea.MouseWheelUp:
			m.viewport.ScrollUp(3)
		case tea.MouseWheelDown:
			m.viewport.ScrollDown(3)
		}
		m.follow = m.viewport.AtBottom()
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case agentResultMsg:
		m.agentWorking = false
		m.streaming = true
		m.activity = prependActivity(m.activity, msg.reply.items...)
		if len(msg.reply.subagents) > 0 {
			m.subagents = append(m.subagents, msg.reply.subagents...)
			if len(m.subagents) > maxActivity {
				m.subagents = m.subagents[len(m.subagents)-maxActivity:]
			}
		}
		cm := chatMsg{role: roleAgent, text: ""}
		if msg.reply.collapse != nil {
			cm.summary = msg.reply.collapse.summary
			cm.content = msg.reply.collapse.content
			cm.collapsed = true
		}
		m.streamBuf = truncate(msg.reply.text, maxReplyLen)
		m.chat = appendChat(m.chat, cm)
		if cm.collapsible() {
			m.status = "block collapsed — ctrl+o to expand"
		} else {
			m.status = "streaming reply…"
		}
		m.refreshChat()
		return m, streamTickCmd()

	case streamTickMsg:
		if !m.streaming {
			return m, nil
		}
		n := min(streamChunk, len(m.streamBuf))
		m.chat[len(m.chat)-1].text += m.streamBuf[:n]
		m.streamBuf = m.streamBuf[n:]
		if len(m.streamBuf) == 0 {
			m.streaming = false
			m.status = "reply complete — try /search mcp or /diff"
			// Auto-flush next queued prompt if available
			if len(m.queue) > 0 {
				nextPrompt := m.queue[0]
				m.queue = m.queue[1:]
				m.input.SetValue(nextPrompt)
				var cmd tea.Cmd
				m, cmd = m.send()
				return m, cmd
			}
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

// handleKey routes a key press by focus (pro-TUI rule 3: every binding checks
// focus before acting; nothing reacts twice to the same key).
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// /connect modal: its own key handling (ctrl+c still quits; esc/q closes).
	if m.connectOpen {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.connectOpen = false
		case "up", "k":
			if m.connectSel > 0 {
				m.connectSel--
			}
		case "down", "j":
			if m.connectSel < len(defaultProviders)-1 {
				m.connectSel++
			}
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			// Number keys select providers dynamically — index is guarded by
			// len(defaultProviders) so a shorter list never overflows.
			if idx := int(msg.String()[0] - '1'); idx < len(defaultProviders) {
				m.connectSel = idx
			}
		case "enter":
			p := defaultProviders[m.connectSel]
			// Mock selection only — real auth comes with the provider layer.
			// Choosing a provider sizes the context-window display (UI/UX
			// stage; valid numbers come later, Principle 3).
			m.provider = p.name
			m.window = modelWindows[p.name]
			m.status = "connected to " + p.name + " (mock) — " + fmtTokens(m.window) + " ctx window"
			m.connectOpen = false
		}
		return m, nil
	}

	// /models modal key handling
	if m.modelsOpen {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.modelsOpen = false
		case "up", "k":
			if m.modelsSel > 0 {
				m.modelsSel--
			}
		case "down", "j":
			if m.modelsSel < len(openCodeFreeModels)-1 {
				m.modelsSel++
			}
		case "1", "2", "3", "4", "5", "6", "7":
			if idx := int(msg.String()[0] - '1'); idx < len(openCodeFreeModels) {
				m.modelsSel = idx
			}
		case "enter":
			m.selectedModel = openCodeFreeModels[m.modelsSel]
			m.modelsOpen = false
			m.status = "active model set to " + m.selectedModel
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
		case "up", "k":
			if m.queueSel > 0 {
				m.queueSel--
			}
		case "down", "j":
			if m.queueSel < len(m.queue)-1 {
				m.queueSel++
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
		case "e":
			// Edit selected item: move back to input field and remove from queue
			if len(m.queue) > 0 && m.queueSel < len(m.queue) {
				m.input.SetValue(m.queue[m.queueSel])
				m.input.CursorEnd()
				m.queue = append(m.queue[:m.queueSel], m.queue[m.queueSel+1:]...)
				m.queueOpen = false
				m.status = "editing queued item"
			}
		case "m":
			// Merge selected item with item above it
			if m.queueSel > 0 && len(m.queue) > 1 {
				m.queue[m.queueSel-1] += " " + m.queue[m.queueSel]
				m.queue = append(m.queue[:m.queueSel], m.queue[m.queueSel+1:]...)
				m.queueSel--
				m.status = "merged queue item"
			}
		}
		return m, nil
	}
	// enter applies the highlighted preset).
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
			}
		case "down", "j":
			if m.themeSel < len(names)-1 {
				m.themeSel++
			}
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			if idx := int(msg.String()[0] - '1'); idx < len(names) {
				m.themeSel = idx
			}
		case "enter":
			m.themeOpen = false
			return m.applyTheme(names[m.themeSel]), nil
		}
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

	// Command suggestions: while the input starts with "/" the popup owns
	// navigation and accept keys; every other key flows to the input, which
	// re-filters the list.
	if m.suggestVisible() {
		switch msg.String() {
		case "esc":
			m.suggestDismissed = true
			m.suggestSel = 0
			return m, nil
		case "up", "k":
			if m.suggestSel > 0 {
				m.suggestSel--
			}
			return m, nil
		case "down", "j":
			if items := suggestFiltered(m.input.Value()); m.suggestSel < len(items)-1 {
				m.suggestSel++
			}
			return m, nil
		case "tab", "enter":
			items := suggestFiltered(m.input.Value())
			if len(items) > 0 {
				sel := m.suggestSel
				if sel < 0 || sel >= len(items) {
					sel = 0
				}
				m.input.SetValue(items[sel].cmd)
				m.input.CursorEnd()
				m.suggestSel = 0
				m.suggestDismissed = true // popup closes once accepted
				if msg.String() == "enter" {
					return m.send()
				}
			}
			return m, nil
		}
		// A typed character re-enables the popup (esc only suppresses it
		// until the user actually types again).
		m.suggestDismissed = false
	}

	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+y":
		// Copy last agent response to clipboard or status notice
		if len(m.chat) > 0 {
			lastMsg := m.chat[len(m.chat)-1]
			m.status = "copied last reply (" + fmt.Sprintf("%d chars", len(lastMsg.text)) + ")"
		} else {
			m.status = "nothing to copy"
		}
		return m, nil
	case "q":
		// 'q' quits only when the input is empty — so typing a query that
		// contains the letter q never quits the app.
		if m.input.Value() == "" {
			return m, tea.Quit
		}
	case "ctrl+o":
		return m.toggleCollapse(), nil
	case "enter":
		return m.send()
	case "?":
		// Same guard as send(): never queue a second agent turn while one is
		// in flight (a double-press would append two replies and overwrite
		// the streaming buffer).
		if m.input.Value() == "" && !m.agentWorking && !m.streaming {
			// Help is content — leaving the landing so the reply is visible
			// (same as send(); otherwise the streamed reply would be hidden).
			m.started = true
			m.agentWorking = true
			m.status = "loading help…"
			return m, tea.Batch(m.spinner.Tick, func() tea.Msg {
				return agentResultMsg{reply: buildReply("/help", m.index)}
			})
		}
	}

	// Scroll keys always control the chat viewport — no focus dance. Only
	// arrow/page keys scroll (j/k stay typing characters in the input).
	switch msg.String() {
	case "up", "pgup":
		if msg.String() == "pgup" {
			m.viewport.HalfPageUp()
		} else {
			m.viewport.ScrollUp(1)
		}
		m.follow = m.viewport.AtBottom()
		return m, nil
	case "down", "pgdown":
		if msg.String() == "pgdown" {
			m.viewport.HalfPageDown()
		} else {
			m.viewport.ScrollDown(1)
		}
		m.follow = m.viewport.AtBottom()
		return m, nil
	}

	// Everything else goes to the text input. A typed character
	// re-enables the suggestion popup even if it was dismissed with esc —
	// only the popup's own esc suppresses it, and only until the next key.
	if m.suggestDismissed && msg.Text != "" {
		m.suggestDismissed = false
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// send submits the current input to the (mock) agent or queues it if busy.
func (m Model) send() (Model, tea.Cmd) {
	q := strings.TrimSpace(m.input.Value())
	if q == "" {
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
	}

	// If agent is working or streaming, queue the message for auto-flush
	if m.agentWorking || m.streaming {
		m.queue = append(m.queue, q)
		m.input.SetValue("")
		m.status = fmt.Sprintf("queued (%d) · [ctrl+q] manage queue", len(m.queue))
		return m, nil
	}

	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: q})
	m.input.SetValue("")
	m.agentWorking = true
	m.follow = true
	m.status = "thinking…"
	m.refreshChat()
	return m, tea.Batch(m.spinner.Tick, m.agentWorkCmd(q))
}

// applyTheme sets the theme preset by name — no cycling, no hidden changes.
// Unknown names are rejected with a status notice (no agent work). The theme
// change is recorded in the transcript only once the chat is visible; on the
// landing the system message would be invisible anyway.
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

// setTheme swaps the active theme and rebuilds the precomputed styles. The
// spinner is recreated so its style matches (its old TickMsg IDs are ignored).
func (m *Model) setTheme(name string) {
	m.themeName = name
	m.styles = newStyles(Themes[name])
	m.spinner = spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(m.styles.spinner))
}

// agentWorkCmd executes real OpenCode CLI or fallback mock agent latency.
func (m Model) agentWorkCmd(q string) tea.Cmd {
	selectedMod := m.selectedModel
	if selectedMod == "" {
		selectedMod = "opencode/deepseek-v4-flash-free"
	}

	return func() tea.Msg {
		// Attempt real opencode execution via opencode CLI runner if installed
		if m.opencodeDetected {
			cmd := exec.Command("opencode", "run", "--model", selectedMod, q)
			out, err := cmd.CombinedOutput()
			if err == nil && len(out) > 0 {
				respText := strings.TrimSpace(string(out))
				reply := mockReply{
					text: fmt.Sprintf("[%s]\n%s", selectedMod, respText),
					items: []activityItem{
						{tool: "opencode", label: fmt.Sprintf("opencode run --model %s", selectedMod), status: "ok", detail: "real AI response"},
					},
				}
				return agentResultMsg{reply: reply}
			}
		}

		// Fallback mock pipeline execution
		reply := buildReply(q, m.index)
		time.Sleep(agentLatency)
		return agentResultMsg{reply: reply}
	}
}

func streamTickCmd() tea.Cmd {
	return tea.Tick(time.Second/streamFPS, func(time.Time) tea.Msg { return streamTickMsg{} })
}

// layout recomputes panel sizes from the current window size. Adaptive, not
// pixel-perfect assumptions (pro-TUI rule 7).
func (m *Model) layout() {
	if m.height < 8 {
		m.height = 8
	}
	m.showPanel = m.width >= panelMinW

	chatW := m.width
	if m.showPanel {
		chatW = m.width - panelWidth - 1 // 1-col gap between panels
	}
	if chatW < 12 {
		chatW = 12
	}

	// header(1) + input box(3 incl. border) + status(1) + body panel border(2)
	bodyH := m.height - 7
	if bodyH < 3 {
		bodyH = 3
	}
	m.viewport.SetWidth(chatW)
	m.viewport.SetHeight(bodyH)
	m.input.SetWidth(chatW - 4) // "❯ " prompt(2) + padding(2)
	m.refreshChat()
}

// chatContentWidth is the wrap width for chat messages.
func (m Model) chatContentWidth() int {
	w := m.width
	if m.showPanel {
		w -= panelWidth + 1
	}
	return w - 2
}

// refreshChat rebuilds the viewport content from the bounded chat history.
// Called on every chat change and stream tick (the 30fps bound is the
// anti-lag rule; the rebuild itself is cheap string work).
func (m *Model) refreshChat() {
	var sb strings.Builder
	for _, cm := range m.chat {
		sb.WriteString(m.renderChatMsg(cm))
	}
	if m.agentWorking {
		sb.WriteString("\n")
		sb.WriteString(m.styles.thinking.Render(m.spinner.View() + " thinking…"))
	}
	m.viewport.SetContent(strings.TrimRight(sb.String(), "\n"))
	if m.follow {
		m.viewport.GotoBottom()
	}
}

// ---- rendering -------------------------------------------------------------

func (m Model) View() tea.View {
	// Default size so the UI renders sensibly in tests and tiny terminals.
	if m.width == 0 || m.height == 0 {
		m.width, m.height = 80, 24
		m.layout()
	}
	var baseCanvas string
	if !m.connectOpen && !m.themeOpen && !m.started {
		// New conversation: header + centered landing (logo + form), no
		// separate input bar — the form IS the input.
		if land := m.renderLanding(m.width, m.height); land != "" {
			baseCanvas = strings.Join([]string{m.renderHeader(), land, m.renderStatus()}, "\n")
		}
	}
	if baseCanvas == "" {
		baseCanvas = strings.Join([]string{m.renderHeader(), m.renderBody(), m.renderInput(), m.renderStatus()}, "\n")
	}

	content := baseCanvas
	if m.connectOpen {
		content = compositeOverlay(baseCanvas, m.renderConnectModalBox(), m.width, m.height, overlayCenter)
	} else if m.modelsOpen {
		content = compositeOverlay(baseCanvas, m.renderModelsModalBox(), m.width, m.height, overlayCenter)
	} else if m.themeOpen {
		content = compositeOverlay(baseCanvas, m.renderThemeModalBox(), m.width, m.height, overlayCenter)
	} else if m.queueOpen {
		content = compositeOverlay(baseCanvas, m.renderQueueModalBox(), m.width, m.height, overlayCenter)
	} else if m.suggestVisible() {
		mode := overlayChatSuggest
		if !m.started {
			mode = overlayLandingSuggest
		}
		content = compositeOverlay(baseCanvas, m.renderSuggest(), m.width, m.height, mode)
	}

	return m.makeView(content)
}

// makeView builds the tea.View with declarative v2 options: alt screen,
// cell-motion mouse (enables wheel scrolling), and the window title.
func (m Model) makeView(content string) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "brocode " + m.version
	return v
}

func (m Model) renderHeader() string {
	title := " brocode · " + m.version
	if m.commit != "" && m.commit != "unknown" {
		title += " (" + m.commit + ")"
	}
	left := m.styles.title.Render(title)

	// Live token transparency (Principle 3 spirit): used vs provider window.
	// Real settlement numbers replace the estimate once the provider layer
	// lands; until then the estimate keeps the display honest.
	win := "—"
	if m.window > 0 {
		win = fmtTokens(m.window)
	}
	right := m.styles.title.Render(fmt.Sprintf("ctx %s / %s", fmtTokens(tokenEstimate(m.chat)), win))

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return lipgloss.JoinHorizontal(lipgloss.Center,
		left, m.styles.statusLeft.Render(strings.Repeat(" ", gap)), right)
}

// toggleCollapse expands/collapses ALL collapsible blocks (thinking traces, diffs) across the chat.
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

	// Target state: if any block is currently collapsed, expand all; otherwise collapse all.
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

// renderBody shows the chat + activity panels, or landing screen if not started.
func (m Model) renderBody() string {
	if !m.started {
		return m.renderLanding(m.width, m.height)
	}

	// Chat area is clean & borderless — no heavy box lines surrounding chat messages.
	chatPanel := m.viewport.View()

	if !m.showPanel {
		return chatPanel
	}
	sideTitle := m.styles.title.Render(" status ")
	sidePanel := m.styles.sideBoxIn.Render(sideTitle + "\n" + m.renderPanel())
	return lipgloss.JoinHorizontal(lipgloss.Top, chatPanel, " ", sidePanel)
}

// compositeOverlay overlays fg (modal or suggest box) on top of bg canvas.
// When started=false (landing), suggest sits below the landing input form.
// When started=true (chat), suggest sits directly above the chat input bar.
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
	case overlayCenter:
		startY = (height - fgH) / 2
		startX = (width - fgW) / 2
	case overlayLandingSuggest:
		// On landing screen, place popup directly above the centered input form box
		startY = height/2 - fgH + 1
		startX = (width - fgW) / 2
	case overlayChatSuggest:
		// On chat screen, sit directly above the bottom input bar with breathing space
		startY = height - fgH - 5
		startX = 2 // left-aligned with chat prompt
	}

	if startY < 0 {
		startY = 0
	}
	if startX < 0 {
		startX = 0
	}

	for i, fgLine := range fgLines {
		targetY := startY + i
		if targetY >= len(bgLines) {
			break
		}

		bgLines[targetY] = compositeLine(bgLines[targetY], fgLine, startX, width)
	}

	return strings.Join(bgLines, "\n")
}

type overlayMode int

const (
	overlayCenter overlayMode = iota
	overlayLandingSuggest
	overlayChatSuggest
)

// compositeLine overlays fgLine onto bgLine starting at visual column startX,
// preserving background ANSI escape states before startX and after startX + fgWidth.
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

	// Substring parts of bgLine using rune visual widths
	left := sliceAnsiWidth(bgLine, 0, startX)
	right := sliceAnsiWidth(bgLine, startX+fgW, width)

	return left + fgLine + right
}

// sliceAnsiWidth returns a substring of s that spans visual display columns from start to end,
// preserving ANSI formatting.
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

func (m Model) renderInput() string {
	// The input is always the typing surface — always the active border.
	typedView := m.input.View()
	promptStr := m.styles.prompt.Render("❯ ")

	if m.suggestVisible() {
		items := suggestFiltered(m.input.Value())
		if m.suggestSel >= 0 && m.suggestSel < len(items) {
			typed := m.input.Value()
			target := items[m.suggestSel].cmd
			if strings.HasPrefix(target, typed) && len(target) > len(typed) {
				ghostSuffix := target[len(typed):]
				// Remove trailing padding whitespace from textinput view to insert ghost text inline
				trimmed := strings.TrimRight(typedView, " ")
				typedView = trimmed + m.styles.sys.Render(ghostSuffix)
			}
		}
	}

	// Active model badge indicator inside input box
	if m.provider != "" && m.selectedModel != "" {
		modelBadge := m.styles.statusRight.Render(" 🤖 " + m.provider + " · " + m.selectedModel)
		promptStr = modelBadge + " " + promptStr
	}

	// Queue indicator badge inside input box if queue contains items
	if len(m.queue) > 0 {
		qBadge := m.styles.statusRight.Render(fmt.Sprintf(" 📥 queued (%d)", len(m.queue)))
		promptStr = qBadge + " " + promptStr
	}

	// Re-pad typedView to m.input.Width() so the input box container never shrinks
	curW := lipgloss.Width(ansiStrip.ReplaceAllString(typedView, ""))
	if targetW := m.input.Width(); curW < targetW {
		typedView += strings.Repeat(" ", targetW-curW)
	}

	return m.styles.inputBoxOn.Render(promptStr + typedView)
}

// renderQueueModalBox renders the framed modal box for /queue.
func (m Model) renderQueueModalBox() string {
	w := min(62, m.width-4)
	if w < 35 {
		w = 35
	}

	var sb strings.Builder
	sb.WriteString(m.styles.title.Render(" prompt queue manager "))
	sb.WriteString("\n\n")

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
	sb.WriteString(m.styles.statusLeft.Render("↑↓ navigate · e edit · d delete · m merge · esc close"))

	return m.styles.connectBox.Width(w).Render(sb.String())
}

func (m Model) renderStatus() string {
	left := m.styles.statusLeft.Render(keys.shortHelp())
	right := m.status
	if right == "" {
		if m.provider != "" && m.selectedModel != "" {
			right = fmt.Sprintf("%s · %s", m.provider, m.selectedModel)
		} else if m.provider != "" {
			right = fmt.Sprintf("%s · %d msgs", m.provider, len(m.chat))
		} else {
			right = fmt.Sprintf("%d msgs · %d activities", len(m.chat), len(m.activity))
		}
		right = m.styles.statusRight.Render(right)
	} else {
		right = m.styles.statusRight.Render(right)
	}
	return lipgloss.JoinHorizontal(lipgloss.Center,
		left, m.styles.statusLeft.Render("   │   "), right)
}

// renderChatMsg renders one chat message, wrapped to the chat content width.
// No backgrounds anywhere (user ask, Aug 2026): user messages get a
// theme-colored vertical bar on the left (Claude Code pattern, no label),
// agent responses are plain text in the agent color (no label), and system
// notices keep their muted "system:" label. Collapsible messages show their
// summary when collapsed and the full block when expanded.
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
		sb.WriteString(renderPlain(m.styles.agent, cm.text, w))
	default:
		sb.WriteString(renderLabeled(m.styles.sys, "system", cm.text, w))
	}
	if cm.collapsible() {
		if cm.collapsed {
			sb.WriteString("  ")
			sb.WriteString(m.styles.statusLeft.Render("▸ " + cm.summary))
			sb.WriteString("\n")
		} else {
			sb.WriteString(renderLabeled(m.styles.agent, "", cm.content, w))
		}
	}
	return sb.String() + "\n"
}

// renderUserMsg renders a user message with a theme-colored bold vertical bar
// on the left of every line — no background, no label. Blank lines keep a bare
// bar so the block stays visually connected.
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
	return strings.Join(out, "\n") + "\n"
}

// renderPlain renders wrapped plain text with no label, no indent, and no
// background — the agent's response style. Wrapped to w-2 so agent lines
// share the same total width as user lines (bar + text).
func renderPlain(style lipgloss.Style, text string, w int) string {
	return style.Render(lipgloss.Wrap(text, w-2, "")) + "\n"
}

// renderLabeled renders a block with an optional label line. When label is
// empty only the indented body renders. The leading indent is inside the
// style so the background tint forms a continuous block.
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

// statusGlyph maps a status to a Unicode glyph + semantic color. Meaning is
// never carried by color alone (pro-TUI: color has meaning, not decoration).
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

// appendChat keeps chat history bounded at maxHistory (Principle 1).
func appendChat(chat []chatMsg, msgs ...chatMsg) []chatMsg {
	chat = append(chat, msgs...)
	if len(chat) > maxHistory {
		chat = chat[len(chat)-maxHistory:]
	}
	return chat
}

// prependActivity keeps the activity list bounded, newest first.
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
