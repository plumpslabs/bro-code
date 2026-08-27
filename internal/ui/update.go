package ui

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
	"github.com/plumpslabs/bro-code/internal/version"
)

// turnTimeout is the maximum wall-clock duration a single turn may run before
// the watchdog cancels it and recovers the UI. This prevents the spinner
// from spinning forever when a stream silently drops (TCP reset, provider
// hang) or the engine gets stuck between tool iterations.
const turnTimeout = 10 * time.Minute

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.renderedKey = ""
		m.renderedLog = ""
		m.historyKey = ""
		m.renderedHistory = ""
		if m.width > 0 {
			pw := m.width - 12
			if pw < 10 {
				pw = 10
			}
			m.promptInput.SetWidth(pw)
			m.logViewport.SetWidth(m.width)
			m.askViewport.SetWidth(m.width - 8)
			// Connect wizard inputs need an explicit width too, otherwise the
			// textinput renders nothing (Width 0) and typing looks broken. The
			// focused input renders Width+1 (cursor column) and sits behind a
			// two-space prefix inside a Width(m.width-4) box (2 border + 4
			// padding = 6), so cw = m.width-13 keeps the border from being
			// pushed past the terminal edge.
			cw := m.width - 13
			if cw < 10 {
				cw = 10
			}
			m.connectNameInput.SetWidth(cw)
			m.connectTextInput.SetWidth(cw)
			m.connectBaseURLInput.SetWidth(cw)
			m.connectModelsInput.SetWidth(cw)
			m.updateLogHeight()
			m.logViewport.GotoBottom()
		}

	case productiveIterMsg:
		// Reset the turn watchdog timer on productive work (file edits/writes)
		// so the wall-clock safety net does not cut short active implementation.
		if m.turnRunning {
			m.turnStart = time.Now()
		}
		return m, nil

	case spinnerTickMsg:
		// When idle (no turn running, not busy), stop the ticker immediately
		// so BroCode consumes 0.0% CPU and zero battery while waiting for input.
		if !m.turnRunning && (m.status == "Ready" || m.status == "Failed") {
			return m, nil
		}
		// Turn watchdog: if a turn has been running for too long without
		// completing (e.g. silent TCP drop, provider hang, stuck in a loop
		// between tool calls), auto-cancel it so the UI recovers.
		if m.turnRunning && !m.turnStart.IsZero() {
			timeout := m.effectiveTurnTimeout()
			if time.Since(m.turnStart) > timeout {
				if m.cancelTurn != nil {
					m.cancelTurn()
				}
				m.turnRunning = false
				m.streaming = false
				m.pendingStream = ""
				m.activity = nil
				m.status = "Ready"
				m.appendMessages(fmt.Sprintf("⚠️ Turn timed out after %s — auto-recovered", timeout))
				// Drain any pending queued turns that were waiting on this one.
				if cmd := m.drainPendingQueue(); cmd != nil {
					return m, cmd
				}
				return m, tickCmd()
			}
		}
		// While any modal is open the content is static — keep ticker alive without advancing frames.
		if m.showAsk || m.showModels || m.showConnect || m.showDebug || m.showSessions || m.showMCP {
			return m, tickCmd()
		}
		m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		return m, tickCmd()

	case stepProgressMsg:
		if strings.HasPrefix(msg.info, "DIFF:\n") {
			body := strings.TrimPrefix(msg.info, "DIFF:\n")
			path, diff := body, ""
			if nl := strings.Index(body, "\n"); nl >= 0 {
				path, diff = body[:nl], body[nl+1:]
			}
			m.upsertDiffMessage(path, diff)
			return m, nil
		}
		if strings.HasPrefix(msg.info, "TODOS:\n") {
			m.upsertTodosMessage(msg.info)
			return m, nil
		}
		str := msg.info
		// Progress events that land AFTER the turn has already settled (the
		// adapter's stderr goroutine may still flush lines after turnResultMsg)
		// must not clobber the final status or pollute the history.
		if m.status == "Ready" || m.status == "Failed" {
			return m, nil
		}
		// Skip the terminal "Completed" marker — it is not a user-visible step.
		if str == "Completed" {
			m.status = str
			return m, nil
		}
		// Phase badge: make explicit whether the agent is reasoning,
		// observing, or running a tool — so a long "still processing" state is
		// never ambiguous. Messages that already lead with their own glyph
		// (tool calls, warnings, scouts) keep that glyph.
		if badge := phaseBadge(msg.state); badge != "" && !startsWithEmoji(str) {
			str = badge + " " + str
		} else {
			str = normalizeEmojiSpacing(str)
		}
		// When transitioning to tool execution (StateActing), commit the streamed
		// assistant text for this iteration as its own distinct block with vertical line border.
		if msg.state == loop.StateActing && m.streaming && strings.TrimSpace(m.pendingStream) != "" {
			useMode := m.turnMode
			if useMode == "" {
				useMode = m.mode
			}
			stamp := "BROCODE:" + useMode + ":" + m.activeModel + "\n" + strings.TrimSpace(m.pendingStream)
			m.appendMessages(stamp)
			m.pendingStream = ""
			m.streaming = false
		}

		m.status = str
		if len(m.activity) == 0 || m.activity[len(m.activity)-1] != str {
			m.activity = append(m.activity, str)
			if len(m.activity) > 5 {
				m.activity = m.activity[len(m.activity)-5:]
			}
		}
		return m, nil

	case turnResultMsg:
		// Stale-generation guard: if this result is from a previously-cancelled
		// turn (ESC was pressed and/or a new turn has already started), discard it
		// silently. The new turn owns turnRunning and its own streaming state.
		if msg.gen > 0 && msg.gen != m.turnGen {
			return m, nil
		}
		// Snapshot the partial stream BEFORE clearing it: an interrupted turn
		// must leave a trace in history instead of vanishing (the user saw the
		// text appear, so it must not silently disappear — that was a big
		// source of "unstable history" reports).
		partial := m.pendingStream
		m.streaming = false
		m.pendingStream = ""
		m.activity = nil
		// The in-flight turn is done; the queue may start the next one.
		m.turnRunning = false
		if msg.err != nil {
			// A user-initiated interrupt (ESC) aborts the context, which the
			// adapter reports as "context canceled". That is not an error —
			// the interruption notice was already shown when ESC was pressed.
			if strings.Contains(strings.ToLower(msg.err.Error()), "context canceled") {
				m.status = "Ready"
			} else {
				m.appendMessages("ERROR: " + msg.err.Error())
				m.status = "Failed"
			}
		} else if strings.TrimSpace(msg.content) == "" && strings.TrimSpace(partial) == "" && (len(m.messages) == 0 || !strings.HasPrefix(m.messages[len(m.messages)-1], "BROCODE")) {
			// The turn finished but the model returned nothing (a weak model
			// stalling into an empty response). Surface it so the UI never
			// looks stuck on "Thinking..." with no entry — and the queue can
			// still drain below.
			m.appendMessages("⚠️ The model returned an empty response — try rephrasing your request or switching models.")
			m.status = "Ready"
		} else {
			display := strings.TrimSpace(partial)
			if display == "" {
				display = strings.TrimSpace(msg.content)
			}
			alreadyInHistory := false
			if len(m.messages) > 0 {
				last := m.messages[len(m.messages)-1]
				if display != "" && strings.Contains(last, display) {
					alreadyInHistory = true
				}
			}
			if display != "" && !alreadyInHistory {
				if msg.mode != "" {
					m.appendMessages("BROCODE:" + msg.mode + ":" + m.activeModel + "\n" + display)
				} else {
					m.appendMessages("BROCODE:\n" + display)
				}
			}
			// Extract senior recommendations for quick interactive execution
			if recs := ExtractRecommendations(display); len(recs) > 0 {
				m.activeRecommendations = recs
			}
			// When the primary provider failed and a fallback served the turn,
			// say so in the history — otherwise the answer is mistaken for the
			// active provider's (which is exactly the confusion the user saw:
			// groq active, deepseek-v4-flash-free answered).
			if fb := m.engine.LastFallbackModel(); fb != "" {
				// Include WHY the primary failed (duration/queue limit, invalid
				// model, auth error) so the user can act on it — e.g. switch
				// model or provider — instead of wondering.
				reason := m.engine.LastFallbackReason()
				msg := fmt.Sprintf("⚠️ Primary provider failed — this answer came from fallback model %s.", fb)
				if reason != "" && len(reason) < 300 {
					msg += "\nReason: " + reason
				}
				// Adaptive routing status: the circuit breaker's cooldown means
				// the primary is temporarily skipped (it will be retried
				// automatically), and the running fallback count shows whether
				// this is a one-off blip or a persistent condition.
				if cd := m.engine.PrimaryCooldownRemaining(); cd > 0 {
					msg += fmt.Sprintf("\nPrimary is cooling down (%s) — will be retried automatically.", cd.Round(time.Second))
				}
				if n := m.engine.FallbackCount(); n > 1 {
					msg += fmt.Sprintf("\nFallbacks so far this session: %d turn(s).", n)
				}
				m.appendMessages(msg)
			}
			m.status = "Ready"
		}

		// Persist the turn's file changes to the session event log so a future
		// /resume or audit can reconstruct what a turn touched.
		if ch := tool.TakeChanges(); len(ch) > 0 {
			if st := m.context.Store(); st != nil {
				if payload, err := json.Marshal(ch); err == nil {
					_, _ = st.AppendEvent(m.context.SessionID(), "file_changes", string(payload), 0)
				}
			}
		}

		// Live session memory capture: capture touched files and goals after
		// each turn so .brocode/memory.md is always up-to-date in real-time.
		if m.memStore != nil && m.context != nil && m.context.Store() != nil {
			if events, err := m.context.Store().GetSessionEvents(m.context.SessionID()); err == nil && len(events) > 0 {
				_ = m.memStore.CaptureSession(m.context.SessionID(), events)
			}
		}

		// One turn at a time: fire the next queued prompt, if any. The queue
		// drains even after an interrupt/error — a queued message was
		// explicitly requested and must not be silently dropped.
		if len(m.pendingQueue) > 0 {
			return m, m.drainPendingQueue()
		}
		// Queue fully drained — leave queue management mode if the last item
		// was deleted by the user rather than drained.
		if m.queueMode {
			m.queueMode = false
			m.queueSel = 0
		}

		// Turn and queue completed: immediately reclaim memory from large tool outputs,
		// diff buffers, and JSON ASTs back to OS kernel before entering idle sleep.
		runtime.GC()
		debug.FreeOSMemory()

		return m, nil

	case streamChunkMsg:
		if !m.streaming {
			// A new answer is starting — drop out of the pager so the stream
			// renders into the normal chat view.
			if m.pagerActive {
				m.exitPager()
			}
			m.streaming = true
			m.pendingStream = ""
			m.status = "Streaming..."
		}
		m.pendingStream += string(msg)
		return m, nil

	case fileDiffMsg:
		// Live per-edit red/green diff entry in the chat.
		m.upsertDiffMessage(msg.path, msg.diff)
		return m, nil

	case diagnoseResultMsg:
		m.appendNote(string(msg))
		m.status = "Ready"

	case ephemeralAskResultMsg:
		m.appendNote(string(msg))
		m.status = "Ready"

	case specResultMsg:
		m.appendNote(string(msg))
		m.status = "Ready"

	case tournamentResultMsg:
		m.appendNote(string(msg))
		m.status = "Ready"

	case repairResultMsg:
		m.appendNote(string(msg))
		m.status = "Ready"

	case versionCheckResultMsg:
		if msg.hasUpdate {
			m.appendNote(fmt.Sprintf("✨ New version available: **%s** → **%s**! Type `/update` to upgrade instantly.", version.Version, msg.latest))
		}

	case diagnoseFixMsg:
		diag := string(msg)
		m.appendNote(diag)
		// Nothing to fix — don't spawn a pointless turn.
		if strings.Contains(diag, "No diagnostics found") {
			m.appendNote("✅ No diagnostics to fix.")
			m.status = "Ready"
			return m, nil
		}
		m.status = "Fixing diagnostics..."
		prompt := "Based on the LSP diagnostics above, FIX all safe warnings and errors automatically without asking:\n\n" +
			"- Use lsp_fix for quick-fixes (auto-import, organize imports).\n" +
			"- Use edit_file for deprecated APIs, unused imports/symbols, and other manual fixes.\n" +
			"- Use lsp_rename for project-wide symbol renames.\n" +
			"- After finishing, re-run lsp_scan to confirm the diagnostics are resolved. If the project has an obvious build/test CLI (its language's standard toolchain), run it too — but do NOT assume any specific one. Do NOT change behavior—only clean up warnings.\n" +
			"- Prioritize safe changes; if something needs a design decision, skip it and mention it in your answer."
		return m.startTurn(prompt)

	case statusUpdateMsg:
		m.status = string(msg)

	case askUserMsg:
		m.openAsk(msg)
		return m, nil

	case fileConfirmMsg:
		m.openFileConfirm(msg)
		return m, nil

	case tea.MouseMsg:
		if m.showSessions {
			m.sessionsViewport, _ = m.sessionsViewport.Update(msg)
			return m, nil
		}
		m.logViewport, _ = m.logViewport.Update(msg)
		return m, nil

	case tea.KeyMsg:
		keyStr := msg.String()

		// Intercept explicit paste shortcuts (ctrl+v) using OS clipboard
		if keyStr == "ctrl+v" {
			if clipText, err := clipboard.ReadAll(); err == nil && clipText != "" {
				cleanClip := strings.TrimSpace(strings.ReplaceAll(clipText, "\r\n", "\n"))

				if m.showAsk && m.askCustomQ >= 0 {
					m.askCustomInput.SetValue(m.askCustomInput.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
					return m, nil
				} else if m.showConnect {
					switch m.connectStep {
					case 1:
						if m.connectCustom {
							m.connectNameInput.SetValue(m.connectNameInput.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
						} else {
							m.connectTextInput.SetValue(m.connectTextInput.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
						}
					case 2:
						m.connectTextInput.SetValue(m.connectTextInput.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
					case 3:
						m.connectBaseURLInput.SetValue(m.connectBaseURLInput.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
					case 4:
						m.connectModelsInput.InsertString(cleanClip)
					}
					return m, nil
				} else if m.showMCP && m.mcpAddActive {
					switch m.mcpAddStep {
					case 1:
						m.mcpAddName.SetValue(m.mcpAddName.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
					case 2:
						if m.mcpAddType == 0 {
							m.mcpAddCmd.SetValue(m.mcpAddCmd.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
						} else {
							m.mcpAddURL.SetValue(m.mcpAddURL.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
						}
					}
					return m, nil
				} else if m.showModels {
					m.modelsQuery += strings.ReplaceAll(cleanClip, "\n", "")
					return m, nil
				} else if !m.showConnect && !m.showModels && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk {
					m.promptInput.InsertString(cleanClip)
					return m, nil
				}
			}
		}

		// In-TUI pager mode: every key scrolls the last answer directly.
		if m.pagerActive {
			switch keyStr {
			case "q", "esc", "ctrl+p":
				m.exitPager()
				return m, nil
			case "pgup":
				m.logViewport.PageUp()
				return m, nil
			case "pgdown":
				m.logViewport.PageDown()
				return m, nil
			case "home":
				m.logViewport.GotoTop()
				return m, nil
			case "end":
				m.logViewport.GotoBottom()
				return m, nil
			case "up":
				m.logViewport.ScrollUp(1)
				return m, nil
			case "down":
				m.logViewport.ScrollDown(1)
				return m, nil
			case "ctrl+u":
				m.logViewport.HalfPageUp()
				return m, nil
			case "ctrl+d":
				m.logViewport.HalfPageDown()
				return m, nil
			}
		}

		// In Models selection modal: dedicated isolated event handler so every key
		// (letters, 'y', 'n', 'd', numbers, space, punctuation) filters seamlessly.
		if m.showModels {
			switch keyStr {
			case "esc":
				if m.modelsQuery != "" {
					m.modelsQuery = ""
					m.modelsSel = 0
				} else {
					m.showModels = false
				}
				return m, nil

			case "enter":
				m.applySelectedModel()
				m.showModels = false
				return m, nil

			case "up":
				items := m.getModelList()
				if len(items) > 0 {
					if m.modelsSel > 0 {
						m.modelsSel--
					} else {
						m.modelsSel = len(items) - 1 // wrap around to bottom
					}
				}
				return m, nil

			case "down":
				items := m.getModelList()
				if len(items) > 0 {
					if m.modelsSel < len(items)-1 {
						m.modelsSel++
					} else {
						m.modelsSel = 0 // wrap around to top
					}
				}
				return m, nil

			case "pgup":
				m.modelsSel -= 5
				if m.modelsSel < 0 {
					m.modelsSel = 0
				}
				return m, nil

			case "pgdown":
				items := m.getModelList()
				m.modelsSel += 5
				if len(items) > 0 && m.modelsSel >= len(items) {
					m.modelsSel = len(items) - 1
				}
				return m, nil

			case "home":
				m.modelsSel = 0
				return m, nil

			case "end":
				items := m.getModelList()
				if len(items) > 0 {
					m.modelsSel = len(items) - 1
				}
				return m, nil

			case "backspace":
				if len(m.modelsQuery) > 0 {
					m.modelsQuery = m.modelsQuery[:len(m.modelsQuery)-1]
					m.modelsSel = 0
				}
				return m, nil

			case "ctrl+u", "alt+backspace", "ctrl+backspace", "ctrl+delete", "alt+delete":
				m.modelsQuery = ""
				m.modelsSel = 0
				return m, nil

			case "space":
				m.modelsQuery += " "
				m.modelsSel = 0
				return m, nil

			default:
				// Any printable rune typed into the filter box
				if len(keyStr) == 1 && keyStr[0] >= 32 && keyStr[0] != 127 {
					m.modelsQuery += keyStr
					m.modelsSel = 0
					return m, nil
				}
			}
			return m, nil
		}

		// Queue management mode (Ctrl+K / Alt+K):
		if m.queueMode {
			if len(m.pendingQueue) == 0 {
				m.queueMode = false
				m.queueSel = 0
			} else {
				switch keyStr {
				case "esc", "alt+k", "ctrl+k", "alt+K", "ctrl+K":
					m.queueMode = false
					return m, nil
				case "enter":
					return m, nil
				case "up", "k":
					if m.queueSel > 0 {
						m.queueSel--
					}
					return m, nil
				case "down", "j":
					if m.queueSel < len(m.pendingQueue)-1 {
						m.queueSel++
					}
					return m, nil
				case "K", "shift+up":
					if m.queueSel > 0 {
						m.pendingQueue[m.queueSel], m.pendingQueue[m.queueSel-1] = m.pendingQueue[m.queueSel-1], m.pendingQueue[m.queueSel]
						m.queueSel--
					}
					return m, nil
				case "J", "shift+down":
					if m.queueSel < len(m.pendingQueue)-1 {
						m.pendingQueue[m.queueSel], m.pendingQueue[m.queueSel+1] = m.pendingQueue[m.queueSel+1], m.pendingQueue[m.queueSel]
						m.queueSel++
					}
					return m, nil
				case "m", "M", "tab", "shift+tab":
					if m.queueSel >= 0 && m.queueSel < len(m.pendingQueue) {
						cur := m.pendingQueue[m.queueSel].Mode
						if cur == "" {
							cur = "BUILDER"
						}
						switch cur {
						case "BUILDER":
							m.pendingQueue[m.queueSel].Mode = "PLANNER"
						case "PLANNER":
							m.pendingQueue[m.queueSel].Mode = "MINER"
						default:
							m.pendingQueue[m.queueSel].Mode = "BUILDER"
						}
					}
					return m, nil
				case "e":
					if m.queueSel >= 0 && m.queueSel < len(m.pendingQueue) {
						item := m.pendingQueue[m.queueSel]
						m.promptInput.SetValue(item.Text)
						if item.Mode != "" {
							m.mode = item.Mode
						}
						m.pendingQueue = append(m.pendingQueue[:m.queueSel], m.pendingQueue[m.queueSel+1:]...)
					}
					m.queueMode = false
					m.queueSel = 0
					return m, nil
				case "d":
					if m.queueSel >= 0 && m.queueSel < len(m.pendingQueue) {
						m.pendingQueue = append(m.pendingQueue[:m.queueSel], m.pendingQueue[m.queueSel+1:]...)
					}
					if len(m.pendingQueue) == 0 {
						m.queueMode = false
						m.queueSel = 0
					} else if m.queueSel >= len(m.pendingQueue) {
						m.queueSel = len(m.pendingQueue) - 1
					}
					return m, nil
				}
			}
		}

		// Mode-switch confirmation:
		if m.showModeConfirm {
			switch keyStr {
			case "y", "Y", "enter":
				m.mode = m.pendingMode
				m.engine.SetMode(m.mode)
				m.persistMode()
				m.appendMessages(fmt.Sprintf("✅ Mode → %s", m.mode))
			case "n", "N", "esc":
				m.appendMessages("❌ Ganti mode dibatalkan.")
			}
			m.showModeConfirm = false
			m.pendingMode = ""
			return m, nil
		}

		switch keyStr {
		case "ctrl+f":
			if !m.showFileConfirm && !m.showAsk && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug && !m.showMCP {
				m.filesExpanded = !m.filesExpanded
				m.renderedLog = ""
				m.renderedKey = ""
			}
		case "ctrl+m":
			if m.mouseMode == "SCROLL" {
				m.mouseMode = "SELECT"
				m.appendMessages("🖱️ Mouse Mode: SELECT (Native mouse drag highlight & copy enabled)")
			} else {
				m.mouseMode = "SCROLL"
				m.appendMessages("🖱️ Mouse Mode: SCROLL (Mouse wheel viewport scrolling enabled)")
			}
			return m, nil

		case "ctrl+y":
			lastAns := m.lastAssistantAnswer()
			if lastAns != "" {
				if err := clipboard.WriteAll(lastAns); err == nil {
					m.appendMessages("📋 Copied last assistant response to OS clipboard!")
				}
			}
			return m, nil

		case "ctrl+u":
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk && m.promptInput.Value() != "" {
				m.promptInput.Reset()
				m.autocomplete = AutocompleteState{}
				return m, nil
			}
			m.logViewport.HalfPageUp()
			return m, nil

		case "alt+backspace", "ctrl+backspace", "ctrl+delete", "alt+delete":
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk && m.promptInput.Value() != "" {
				m.promptInput.Reset()
				m.autocomplete = AutocompleteState{}
				return m, nil
			}

		case "ctrl+d":
			m.logViewport.HalfPageDown()
			return m, nil

		case "ctrl+p":
			if !m.showFileConfirm && !m.showAsk && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug && !m.showMCP {
				contentWidth := m.width - 4
				if contentWidth < 20 {
					contentWidth = 80
				}
				m.pagerContent = m.buildPagerContent(contentWidth)
				m.pagerWidth = contentWidth
				m.pagerActive = true
				m.logViewport.SetContent(m.pagerContent)
				m.logViewport.GotoTop()
			}
			return m, nil

		case "alt+k", "ctrl+k", "alt+K", "ctrl+K":
			if !m.showFileConfirm && !m.showAsk && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug && !m.showMCP && len(m.pendingQueue) > 0 {
				m.queueMode = !m.queueMode
				if m.queueMode && m.queueSel >= len(m.pendingQueue) {
					m.queueSel = len(m.pendingQueue) - 1
				}
			}
			return m, nil

		case "ctrl+c":
			m.quitting = true
			if m.cancelTurn != nil {
				m.cancelTurn()
				m.cancelTurn = nil
			}
			if m.scoutMgr != nil {
				m.scoutMgr.CancelAll()
			}
			return m, tea.Quit

		case "tab", "shift+tab":
			if m.autocomplete.Active && len(m.autocomplete.Items) > 0 && keyStr == "tab" {
				newVal := ApplyAutocomplete(m.promptInput.Value(), m.autocomplete)
				m.promptInput.SetValue(newVal)
				m.promptInput.CursorEnd()
				m.autocomplete = AutocompleteState{}
				return m, nil
			}
			if m.showAsk {
				if keyStr == "shift+tab" {
					m.askMoveRow(-1)
				} else {
					m.askMoveRow(1)
				}
				return m, nil
			}
			if m.showFileConfirm {
				if keyStr == "shift+tab" {
					m.fileConfirmSel = 0
				} else {
					m.fileConfirmSel = (m.fileConfirmSel + 1) % 3
				}
				return m, nil
			}
			if keyStr == "shift+tab" && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP {
				var next string
				switch m.mode {
				case "BUILDER":
					next = "PLANNER"
				case "PLANNER":
					next = "MINER"
				default:
					next = "BUILDER"
				}
				m.mode = next
				if !m.turnRunning {
					m.engine.SetMode(m.mode)
					m.persistMode()
				}
			}
			return m, nil

		case "esc":
			if m.autocomplete.Active {
				m.autocomplete = AutocompleteState{}
				return m, nil
			}
			if m.showFileConfirm {
				m.discardFileConfirm()
				return m, nil
			}
			if m.showAsk {
				if m.askCustomQ >= 0 {
					m.askCustomQ = -1
					m.askCustomInput.Blur()
					m.askCustomInput.SetValue("")
				} else {
					m.skipAsk()
				}
				return m, nil
			}
			if m.showModels {
				if m.modelsQuery != "" {
					m.modelsQuery = ""
					m.modelsSel = 0
				} else {
					m.showModels = false
				}
				return m, nil
			}
			if m.showSessions {
				if m.sessionsConfirmID != "" {
					m.sessionsConfirmID = ""
				} else {
					m.showSessions = false
				}
				return m, nil
			}
			if m.showMCP {
				if m.mcpAddActive {
					m.mcpAddPrev()
				} else if m.mcpConfirm != "" {
					m.mcpConfirm = ""
				} else {
					m.showMCP = false
				}
				return m, nil
			}
			if m.showDebug {
				m.showDebug = false
				return m, nil
			}
			if m.showConnect {
				m.connectPrev()
				return m, nil
			}

			if !m.turnRunning && m.promptInput.Value() != "" {
				m.promptInput.Reset()
				m.autocomplete = AutocompleteState{}
				return m, nil
			}

			if m.status != "Ready" && m.status != "Failed" {
				if m.cancelTurn != nil {
					m.cancelTurn()
					m.cancelTurn = nil
				}
				if m.scoutMgr != nil {
					m.scoutMgr.CancelAll()
				}
				m.turnGen++
				partial := strings.TrimSpace(m.pendingStream)
				m.streaming = false
				m.pendingStream = ""
				m.turnRunning = false
				m.activity = nil
				m.status = "Ready"
				if partial != "" {
					m.appendMessages("BROCODE:\n💭 (interrupted — partial response)\n\n" + partial)
				}
				m.appendMessages("⚡ Interrupted turn execution.")
				if len(m.pendingQueue) > 0 {
					next := m.pendingQueue[0]
					m.pendingQueue = m.pendingQueue[1:]
					if m.queueSel > 0 {
						m.queueSel--
					}
					if len(m.pendingQueue) == 0 {
						m.queueMode = false
						m.queueSel = 0
					}
					if next.Mode != "" {
						m.mode = next.Mode
						m.engine.SetMode(m.mode)
						m.persistMode()
					}
					return m.startTurn(next.Text)
				}
				m.queueMode = false
				m.queueSel = 0
				return m, nil
			}

		case "enter":
			if m.showFileConfirm {
				m.submitFileConfirm()
				return m, nil
			}
			if m.showAsk {
				if m.askCustomQ >= 0 {
					m.askSaveCustom()
				} else {
					m.submitAsk()
				}
				return m, nil
			}

			if m.showModels {
				m.applySelectedModel()
				m.showModels = false
				return m, nil
			}

			if m.showSessions {
				m.applySelectedSession()
				m.showSessions = false
				return m, nil
			}

			if m.showMCP {
				if m.mcpAddActive {
					m.mcpAddNext()
				} else if m.mcpConfirm != "" {
					m.mcpConfirm = ""
				} else {
					names := m.mcpNames()
					if m.mcpSel >= 0 && m.mcpSel < len(names) {
						m.appendMessages(m.mcpServerDetail(names[m.mcpSel]))
					}
				}
				return m, nil
			}

			if m.showConnect {
				m.connectNext()
				return m, nil
			}

			if m.autocomplete.Active && len(m.autocomplete.Items) > 0 {
				newVal := ApplyAutocomplete(m.promptInput.Value(), m.autocomplete)
				m.promptInput.SetValue(newVal)
				m.promptInput.CursorEnd()
				m.autocomplete = AutocompleteState{}
				return m, nil
			}

			userQuery := strings.TrimSpace(m.promptInput.Value())
			if userQuery == "" {
				return m, nil
			}

			m.promptInput.Reset()

			m.promptHistory = append(m.promptHistory, userQuery)
			if len(m.promptHistory) > 200 {
				m.promptHistory = m.promptHistory[len(m.promptHistory)-200:]
			}
			m.historyIdx = len(m.promptHistory)

			if strings.HasPrefix(userQuery, "/") {
				return m.handleSlashCommand(userQuery)
			}

			if m.turnRunning {
				m.pendingQueue = append(m.pendingQueue, QueuedPrompt{
					Text: userQuery,
					Mode: m.mode,
				})
				m.queueSel = len(m.pendingQueue) - 1
				m.status = "Queued..."
				return m, nil
			}
			return m.startTurn(userQuery)

		case "d", "D":
			if m.showMCP && !m.mcpAddActive && m.mcpConfirm == "" {
				if keyStr == "d" {
					names := m.mcpNames()
					if m.mcpSel >= 0 && m.mcpSel < len(names) {
						m.mcpConfirm = names[m.mcpSel]
					}
				}
				return m, nil
			}
			if !m.showSessions || m.sessionsConfirmID != "" || m.mcpAddActive {
				break
			}
			if keyStr == "d" {
				if m.sessionsSel >= 0 && m.sessionsSel < len(m.sessionList) {
					m.sessionsConfirmID = m.sessionList[m.sessionsSel].ID
				}
			} else {
				if len(m.sessionList) > 0 {
					m.sessionsConfirmID = "ALL"
				}
			}
			return m, nil

		case "y", "n":
			if m.showMCP && m.mcpConfirm != "" && !m.mcpAddActive {
				if keyStr == "y" {
					m.deleteMCPServer(m.mcpConfirm)
				}
				m.mcpConfirm = ""
				return m, nil
			}
			if !m.showSessions || m.sessionsConfirmID == "" || m.mcpAddActive {
				break
			}
			if keyStr == "y" {
				m.confirmDeleteSessions()
			}
			m.sessionsConfirmID = ""
			return m, nil

		case "a", "r":
			if !m.showMCP || m.mcpAddActive || m.mcpConfirm != "" {
				break
			}
			if keyStr == "a" {
				m.mcpAddActive = true
				m.mcpAddStep = 0
				m.mcpAddType = 0
				m.mcpAddName.SetValue("")
				m.mcpAddCmd.SetValue("")
				m.mcpAddURL.SetValue("")
				m.mcpAddName.Blur()
				m.mcpAddCmd.Blur()
				m.mcpAddURL.Blur()
			} else {
				m.reloadMCP()
			}
			return m, nil

		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			if m.showFileConfirm {
				switch keyStr {
				case "1":
					m.fileConfirmSel = 0
				case "2":
					m.fileConfirmSel = 1
				case "3":
					m.fileConfirmSel = 2
				}
				return m, nil
			}
			if m.showAsk && m.askCustomQ < 0 {
				num := int(keyStr[0] - '0')
				m.askSelectQuickOption(num)
				return m, nil
			}
			if m.promptInput.Value() == "" && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP {
				num := int(keyStr[0] - '0')
				if num >= 1 && num <= len(m.activeRecommendations) && !m.activeRecommendations[num-1].Clicked {
					return m.triggerRecommendation(num)
				}
			}

		case "left", "right":
			if m.showFileConfirm {
				if keyStr == "left" {
					m.fileConfirmSel = (m.fileConfirmSel + 2) % 3
				} else {
					m.fileConfirmSel = (m.fileConfirmSel + 1) % 3
				}
				return m, nil
			}

		case "space":
			if m.showAsk && m.askCustomQ < 0 {
				if m.askOnSubmit() {
					m.submitAsk()
				} else {
					m.askToggle()
				}
				return m, nil
			}

		case "pgup":
			if m.showSessions {
				m.sessionsViewport.PageUp()
				return m, nil
			}
			if !m.showAsk && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP {
				m.logViewport.PageUp()
				return m, nil
			}

		case "pgdown":
			if m.showSessions {
				m.sessionsViewport.PageDown()
				return m, nil
			}
			if !m.showAsk && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP {
				m.logViewport.PageDown()
				return m, nil
			}

		case "shift+up", "ctrl+up", "alt+up":
			if !m.showAsk && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP {
				m.logViewport.ScrollUp(3)
				return m, nil
			}

		case "shift+down", "ctrl+down", "alt+down":
			if !m.showAsk && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP {
				m.logViewport.ScrollDown(3)
				return m, nil
			}

		case "ctrl+e":
			if !m.showAsk && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP {
				m.logViewport.ScrollDown(1)
				return m, nil
			}

		case "up":
			if m.autocomplete.Active && len(m.autocomplete.Items) > 0 {
				if m.autocomplete.Selected > 0 {
					m.autocomplete.Selected--
				} else {
					m.autocomplete.Selected = len(m.autocomplete.Items) - 1
				}
				return m, nil
			}
			if m.showAsk && m.askCustomQ < 0 {
				m.askMove(-1)
				return m, nil
			}
			if m.showModels {
				items := m.getModelList()
				if len(items) > 0 {
					if m.modelsSel > 0 {
						m.modelsSel--
					} else {
						m.modelsSel = len(items) - 1
					}
				}
				return m, nil
			}
			if m.showSessions {
				if len(m.sessionList) > 0 {
					if m.sessionsSel > 0 {
						m.sessionsSel--
					} else {
						m.sessionsSel = len(m.sessionList) - 1
					}
				}
				return m, nil
			}
			if m.showMCP && m.mcpAddActive && m.mcpAddStep == 0 {
				if m.mcpAddType > 0 {
					m.mcpAddType--
				} else {
					m.mcpAddType = 2
				}
				return m, nil
			}
			if m.showMCP && !m.mcpAddActive {
				names := m.mcpNames()
				if len(names) > 0 {
					if m.mcpSel > 0 {
						m.mcpSel--
					} else {
						m.mcpSel = len(names) - 1
					}
				}
				return m, nil
			}
			if m.showConnect && m.connectStep == 0 {
				total := len(provider.BuiltinProviders) + 1
				if m.connectProviderSel > 0 {
					m.connectProviderSel--
				} else {
					m.connectProviderSel = total - 1
				}
				return m, nil
			}
			if m.turnRunning && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk {
				m.logViewport.ScrollUp(1)
				return m, nil
			}
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk && !strings.Contains(m.promptInput.Value(), "\n") {
				if len(m.promptHistory) > 0 {
					if m.historyIdx > 0 {
						m.historyIdx--
					}
					m.promptInput.SetValue(m.promptHistory[m.historyIdx])
					return m, nil
				}
			}

		case "down":
			if m.autocomplete.Active && len(m.autocomplete.Items) > 0 {
				if m.autocomplete.Selected < len(m.autocomplete.Items)-1 {
					m.autocomplete.Selected++
				} else {
					m.autocomplete.Selected = 0
				}
				return m, nil
			}
			if m.showAsk && m.askCustomQ < 0 {
				m.askMove(1)
				return m, nil
			}
			if m.turnRunning && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk {
				m.logViewport.ScrollDown(1)
				return m, nil
			}
			if m.showModels {
				items := m.getModelList()
				if len(items) > 0 {
					if m.modelsSel < len(items)-1 {
						m.modelsSel++
					} else {
						m.modelsSel = 0
					}
				}
				return m, nil
			}
			if m.showSessions {
				if len(m.sessionList) > 0 {
					if m.sessionsSel < len(m.sessionList)-1 {
						m.sessionsSel++
					} else {
						m.sessionsSel = 0
					}
				}
				return m, nil
			}
			if m.showMCP && m.mcpAddActive && m.mcpAddStep == 0 {
				if m.mcpAddType < 2 {
					m.mcpAddType++
				} else {
					m.mcpAddType = 0
				}
				return m, nil
			}
			if m.showMCP && !m.mcpAddActive {
				names := m.mcpNames()
				if len(names) > 0 {
					if m.mcpSel < len(names)-1 {
						m.mcpSel++
					} else {
						m.mcpSel = 0
					}
				}
				return m, nil
			}
			if m.showConnect && m.connectStep == 0 {
				total := len(provider.BuiltinProviders) + 1
				if m.connectProviderSel < total-1 {
					m.connectProviderSel++
				} else {
					m.connectProviderSel = 0
				}
				return m, nil
			}
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk && !strings.Contains(m.promptInput.Value(), "\n") {
				if len(m.promptHistory) > 0 {
					if m.historyIdx < len(m.promptHistory)-1 {
						m.historyIdx++
						m.promptInput.SetValue(m.promptHistory[m.historyIdx])
					} else if m.historyIdx == len(m.promptHistory)-1 {
						m.historyIdx = len(m.promptHistory)
						m.promptInput.Reset()
					}
					return m, nil
				}
			}

		case "backspace":
			if m.showModels {
				if len(m.modelsQuery) > 0 {
					m.modelsQuery = m.modelsQuery[:len(m.modelsQuery)-1]
					m.modelsSel = 0
				}
				return m, nil
			}

		default:
			if m.showModels && len(keyStr) == 1 && keyStr[0] >= 32 && keyStr[0] != 127 {
				m.modelsQuery += keyStr
				m.modelsSel = 0
				return m, nil
			}
		}
	}

	// Update text inputs based on active mode
	if m.showAsk && m.askCustomQ >= 0 {
		var cmd tea.Cmd
		m.askCustomInput, cmd = m.askCustomInput.Update(msg)
		cmds = append(cmds, cmd)
		m.refreshAskModal()
	} else if m.showConnect {
		switch m.connectStep {
		case 1:
			var cmd tea.Cmd
			if m.connectCustom {
				m.connectNameInput, cmd = m.connectNameInput.Update(msg)
			} else {
				m.connectTextInput, cmd = m.connectTextInput.Update(msg)
			}
			cmds = append(cmds, cmd)
		case 2:
			var cmd tea.Cmd
			m.connectTextInput, cmd = m.connectTextInput.Update(msg)
			cmds = append(cmds, cmd)
		case 3:
			var cmd tea.Cmd
			m.connectBaseURLInput, cmd = m.connectBaseURLInput.Update(msg)
			cmds = append(cmds, cmd)
		case 4:
			var cmd tea.Cmd
			m.connectModelsInput, cmd = m.connectModelsInput.Update(msg)
			cmds = append(cmds, cmd)
		}
	} else if m.showMCP && m.mcpAddActive {
		var cmd tea.Cmd
		switch m.mcpAddStep {
		case 1:
			m.mcpAddName, cmd = m.mcpAddName.Update(msg)
		case 2:
			if m.mcpAddType == 0 {
				m.mcpAddCmd, cmd = m.mcpAddCmd.Update(msg)
			} else {
				m.mcpAddURL, cmd = m.mcpAddURL.Update(msg)
			}
		}
		cmds = append(cmds, cmd)
	} else if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk && !m.showFileConfirm {
		if !m.promptInput.Focused() {
			cmds = append(cmds, m.promptInput.Focus())
		}
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		cmds = append(cmds, cmd)
		m.autocomplete = DetectAutocomplete(m.promptInput.Value(), m.allProjectFiles(), m.autocomplete)
	}

	return m, tea.Batch(cmds...)
}

// drainPendingQueue fires the next queued prompt, if any, after a turn
// completes or is interrupted. This is the single exit point for queue
// drain — called both from the normal turnResultMsg path and from the
// watchdog timeout path. Returns the tea.Cmd from startTurn, or nil if
// the queue is empty.
func (m *Model) drainPendingQueue() tea.Cmd {
	if len(m.pendingQueue) == 0 {
		return nil
	}
	next := m.pendingQueue[0]
	m.pendingQueue = m.pendingQueue[1:]
	if m.queueSel > 0 {
		m.queueSel--
	}
	if len(m.pendingQueue) == 0 {
		m.queueMode = false
		m.queueSel = 0
	}
	if next.Mode != "" {
		m.mode = next.Mode
		m.engine.SetMode(m.mode)
		m.persistMode()
	}
	_, cmd := m.startTurn(next.Text)
	return cmd
}

// effectiveTurnTimeout returns the configured turn timeout, falling back to
// the default constant when the config value is empty or unparseable.
func (m *Model) effectiveTurnTimeout() time.Duration {
	if m.cfg.TurnTimeout != "" {
		if d, err := time.ParseDuration(m.cfg.TurnTimeout); err == nil && d > 0 {
			return d
		}
	}
	// Adaptive timeout: simple tasks get less time, complex tasks get more.
	// The productiveIterHandler resets turnStart on file edits, so the timer
	// only counts wall-clock time since the LAST productive iteration — not
	// the entire turn duration.
	return loop.TimeoutForComplexity(m.turnTier)
}
