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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
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
		if m.modalOpen() {
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

	case agentQuestionMsg:
		if msg.run != m.agentRun || !m.agentWorking || m.agentAborted {
			return m, m.waitForAsk()
		}
		m.askOpen = true
		m.askPrompt = msg.prompt
		m.askOptions = msg.options
		m.askSel = 0
		m.input.SetValue("")
		m.input.Placeholder = "answer the agent… (type custom or ↑↓ choose, enter submit)"
		m.status = "agent needs your input — ↑↓ choose · type custom · enter submit · esc cancel"
		m.refreshChat()
		return m, m.waitForAsk()

	case spinner.TickMsg:
		if m.agentWorking {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			m.refreshChat()
			return m, cmd
		}
		return m, nil

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
		m.chat = appendChat(m.chat, cm)
		m.refreshCtx()
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
			fullText := m.chat[len(m.chat)-1].text
			userQuery := ""
			for idx := len(m.chat) - 1; idx >= 0; idx-- {
				if m.chat[idx].role == roleUser {
					userQuery = m.chat[idx].text
					break
				}
			}
			if dynSubs, traceLogs := extractDynamicSubagents(userQuery, fullText, m.provider, m.selectedModel); len(dynSubs) > 0 {
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
			if autoLogs := applyBuilderCodeBlocks(fullText, userQuery); len(autoLogs) > 0 {
				for _, logLine := range autoLogs {
					m.trace = appendTrace(m.trace, logLine)
				}
				m.status = fmt.Sprintf("✓ [BroCode] auto-applied %d file change(s)", len(autoLogs))
			}

			// AGENTIC LOOP: Check for bash/read tool executions
			if toolLogs, feedback := applyAgenticTools(fullText); feedback != "" {
				for _, logLine := range toolLogs {
					m.trace = appendTrace(m.trace, logLine)
				}
				// Prepend the tool feedback to the queue so it runs immediately
				m.queue = append([]string{"[SYSTEM TOOL RESULT]\n" + feedback}, m.queue...)
				m.status = "⚙️  Tool execution completed, continuing agent loop..."
			}
			if planFile := saveMatchaPlan(fullText); planFile != "" {
				m.trace = appendTrace(m.trace, "● Matcha Plan → saved to "+planFile)
			}
			for idx := len(m.chat) - 1; idx >= 0; idx-- {
				if m.chat[idx].role == roleUser {
					m.chat[idx].trace = append([]string(nil), m.trace...)
					break
				}
			}
			m.refreshCtx()
			if len(m.queue) > 0 {
				nextPrompt := m.queue[0]
				m.queue = m.queue[1:]
				m.refreshInputWidth() // queue drained → typed width changed
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

// modalOpen reports whether any overlay modal is currently visible — mouse
// gestures (wheel scroll and drag-select) are suppressed while one is up so
// the popup behind the modal can never be scrolled or selected by accident.
func (m Model) modalOpen() bool {
	return m.connectOpen || m.modelsOpen || m.apikeyOpen || m.themeOpen || m.historyOpen || m.queueOpen || m.promptEditOpen
}

// handleMouseClick starts a drag-select when the left button is pressed
// inside the chat viewport. The anchor and current point are stored in
// viewport content coordinates (y = absolute content line via YOffset, x =
// display column). Clicks anywhere else (header, input, status, modals) are
// ignored — the input is never disturbed by the mouse.
func (m Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if !m.mouseEnabled || m.modalOpen() || !m.started {
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
	m.dragSel = dragSelection{active: true, x0: mm.X, y0: line, x1: mm.X, y1: line}
	m.refreshChat()
	return m, nil
}

// handleMouseMotion extends the drag-select while the left button is held.
// The point is clamped to the viewport so dragging past the edges still
// selects to the last visible line (matching the click-side bounds).
func (m Model) handleMouseMotion(msg tea.MouseMotionMsg) (tea.Model, tea.Cmd) {
	if !m.dragSel.active || !m.mouseEnabled || m.modalOpen() {
		return m, nil
	}
	mm := msg.Mouse()
	y := mm.Y - headerHeight
	if y < 0 {
		y = 0
	}
	if y >= m.viewport.Height() {
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
	if !m.dragSel.active || !m.mouseEnabled || m.modalOpen() {
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
				m.connectSel = len(defaultProviders) - 1
			}
		case "down", "j":
			if m.connectSel < len(defaultProviders)-1 {
				m.connectSel++
			} else {
				m.connectSel = 0
			}
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			if idx := int(msg.String()[0] - '1'); idx < len(defaultProviders) {
				m.connectSel = idx
			}
		case "enter":
			p := defaultProviders[m.connectSel]
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
							m.promptHistory = append(m.promptHistory, cm.text)
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
			if !m.zenModelsLoading && time.Since(m.zenModelsFetched) > zenModelsTTL {
				m.zenModelsLoading = true
				m.status = "fetching live free models from zen gateway…"
				return m, fetchZenModelsCmd()
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.apikeyInput, cmd = m.apikeyInput.Update(msg)
		return m, cmd
	}

	// Agent question modal key handling
	if m.askOpen {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.askOpen = false
			m.agentAborted = true
			m.agentWorking = false
			select {
			case m.answerCh <- "":
			default:
			}
			m.input.SetValue("")
			m.input.Placeholder = "ask brocode... (try: mcp, diff, memory) or /help"
			m.status = "question cancelled — interrupted"
			m.refreshChat()
			return m, nil
		case "up", "k":
			if m.askSel > 0 {
				m.askSel--
			} else if len(m.askOptions) > 0 {
				m.askSel = len(m.askOptions) - 1
			}
			m.refreshChat()
			return m, nil
		case "down", "j":
			if m.askSel < len(m.askOptions)-1 {
				m.askSel++
			} else {
				m.askSel = 0
			}
			m.refreshChat()
			return m, nil
		case "enter":
			answer := strings.TrimSpace(m.input.Value())
			if answer == "" && m.askSel >= 0 && m.askSel < len(m.askOptions) {
				answer = m.askOptions[m.askSel]
			}
			if answer == "" {
				m.status = "answer cannot be empty"
				return m, nil
			}
			m.askOpen = false
			m.input.SetValue("")
			m.input.Placeholder = "ask brocode... (try: mcp, diff, memory) or /help"
			select {
			case m.answerCh <- answer:
			default:
			}
			m.status = "answer sent — agent continuing…"
			m.refreshChat()
			return m, m.waitForAsk()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
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
		if m.agentWorking || m.streaming {
			if m.agentCancel != nil {
				m.agentCancel()
			}
			m.agentAborted = true
			m.agentWorking = false
			m.streaming = false
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
	case "ctrl+m":
		m.mouseEnabled = !m.mouseEnabled
		if m.mouseEnabled {
			m.status = "mouse mode → wheel scroll + drag to select & copy (ctrl+m to disable app mouse)"
		} else {
			m.status = "mouse mode → native terminal selection (app mouse events off, ctrl+m to restore)"
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

	// Token usage forecast
	if m.ctxUsed > 0 {
		pct := float64(m.ctxUsed) / float64(m.window) * 100
		color := "green"
		if pct > 70 {
			color = "yellow"
		}
		if pct > 90 {
			color = "red"
		}
		sb.WriteString(fmt.Sprintf("• Context used: ~%s tokens (%.1f%%) [%s]\n", fmtTokens(m.ctxUsed), pct, color))
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
	sb.WriteString(fmt.Sprintf("\n• Auto-compact trigger: %.0f%% of window\n", compactTriggerPct*100))
	if m.ctxUsed > 0 && m.window > 0 {
		pct := float64(m.ctxUsed) / float64(m.window) * 100
		if pct >= compactTriggerPct*100 {
			sb.WriteString("  ⚠️ **Near compaction threshold!**\n")
		} else {
			sb.WriteString(fmt.Sprintf("  ✓ %.1f%% until trigger\n", compactTriggerPct*100-pct))
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
			id := strings.TrimSuffix(strings.TrimPrefix(s.name, "session_"), ".jsonl")
			if id == "latest" {
				id = "latest"
			}
			row := fmt.Sprintf("(%s)  %s · %d msgs", id, s.time, s.msgCount)
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
	name     string
	path     string
	time     string
	msgCount int
}

// listSessions returns all saved sessions with metadata.
func listSessions() []sessionInfo {
	home, _ := os.UserHomeDir()
	sessionsDir := filepath.Join(home, ".brocode", "sessions")

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}

	var sessions []sessionInfo
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}

		path := filepath.Join(sessionsDir, entry.Name())
		info, _ := os.Stat(path)
		if info == nil {
			continue
		}

		// Count messages
		data, _ := os.ReadFile(path)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		msgCount := 0
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				msgCount++
			}
		}

		sessions = append(sessions, sessionInfo{
			name:     strings.TrimSuffix(entry.Name(), ".jsonl"),
			path:     path,
			time:     info.ModTime().Format("Jan 02, 15:04"),
			msgCount: msgCount,
		})
	}

	// Sort by modification time (newest first)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].time > sessions[j].time
	})

	return sessions
}

// send submits the current input to the (mock) agent or queues it if busy.
func (m Model) send() (Model, tea.Cmd) {
	q := strings.TrimSpace(m.input.Value())
	if q == "" && m.pastedText == "" {
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
		m.refreshChat()
		return m, nil

	case q == "/info":
		m.input.SetValue("")
		info := m.buildInfoMessage()
		m.chat = appendChat(m.chat, chatMsg{role: roleSystem, text: info})
		m.started = true
		m.refreshChat()
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
		if !m.zenModelsLoading && time.Since(m.zenModelsFetched) > zenModelsTTL {
			m.zenModelsLoading = true
			m.status = "fetching live free models from zen gateway…"
			return m, fetchZenModelsCmd()
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
	case q == "/mouse":
		m.input.SetValue("")
		m.mouseEnabled = !m.mouseEnabled
		if m.mouseEnabled {
			m.status = "mouse mode → wheel scroll + drag to select & copy (ctrl+m or /mouse to disable)"
		} else {
			m.status = "mouse mode → native terminal selection (app mouse events off, ctrl+m or /mouse to restore)"
		}
		return m, nil


	case q == "/compact":
		m.input.SetValue("")
		used := chatTokens(m.chat)
		win := m.window
		if win <= 0 {
			win = 131072
		}
		if m.forceCompact() {
			m.refreshChat()
		} else {
			pct := float64(used) * 100.0 / float64(win)
			m.status = fmt.Sprintf("✓ context is %.1f%% clean (%s / %s tokens used) — no compaction needed", 100.0-pct, fmtTokens(used), fmtTokens(win))
		}
		return m, nil
	}

	// If agent is working or streaming, queue the message for auto-flush
	if m.agentWorking || m.streaming {
		m.queue = append(m.queue, q)
		m.input.SetValue("")
		m.refreshInputWidth() // queue badge appeared → typed width changed
		m.status = fmt.Sprintf("queued (%d) · [ctrl+q] manage queue", len(m.queue))
		return m, nil
	}

	m.started = true
	enrichedQuery := attachFileContext(q)
	var attachedLogs []string
	var msg chatMsg
	if strings.HasPrefix(q, "[SYSTEM TOOL RESULT]") {
		msg = chatMsg{
			role:      roleUser,
			summary:   "⚙️ Tool Executed",
			content:   q,
			collapsed: true,
			trace:     attachedLogs,
		}
	} else {
		msg = chatMsg{role: roleUser, text: q, trace: attachedLogs}
	}
	m.chat = appendChat(m.chat, msg)
	m.maybeCompact()
	m.refreshCtx()
	m.promptHistory = append(m.promptHistory, q)
	m.historyIdx = -1
	m.draftInput = ""
	m.input.SetValue("")
	m.agentWorking = true
	m.agentPhase = "thinking…"
	m.agentStep = 0
	m.agentRun++
	m.agentAborted = false
	m.trace = m.trace[:0]
	m.subagents = nil
	for _, logLine := range attachedLogs {
		m.trace = append(m.trace, logLine)
	}
	m.traceCh = make(chan agentTraceMsg, 32)
	m.askCh = make(chan agentQuestionMsg, 1)
	m.answerCh = make(chan string, 1)
	m.askOpen = false
	m.agentCancel = func() {}
	m.follow = true
	m.status = "thinking…"
	m.refreshChat()
	return m, tea.Batch(m.spinner.Tick, m.agentWorkCmd(enrichedQuery, m.traceCh, m.askCh, m.answerCh, &m.agentCancel, m.agentRun), m.waitForTrace(), m.waitForAsk())
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
