package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/plumpslabs/bro-code/internal/agent"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/hooks"
	"github.com/plumpslabs/bro-code/internal/learn"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/lsp"
	"github.com/plumpslabs/bro-code/internal/mcp"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/prompt"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/repo"
	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/skill"
	"github.com/plumpslabs/bro-code/internal/subagent"
	"github.com/plumpslabs/bro-code/internal/tool"
	"github.com/plumpslabs/bro-code/internal/version"
)

// appendMessages appends messages to the conversation history.
func (m *Model) appendMessages(msgs ...string) {
	m.historyVersion++
	m.messages = append(m.messages, msgs...)
	if len(m.messages) > maxChatMessages {
		trimmed := len(m.messages) - maxChatMessages
		m.messages = append([]string(nil), m.messages[trimmed:]...)
		if !m.trimNoticeShown {
			m.trimNoticeShown = true
			m.messages[0] = "… older messages pruned from this view (kept in session history) — /sessions to browse …"
		}
	}
}

// upsertDiffMessage appends a live DIFF entry, or replaces the most recent
// one for the same path within the current turn. The engine emits a cumulative
// diff per file for that turn, so repeated edits within a turn grow a single entry,
// while edits across different turns get separate entries in chronological order.
func (m *Model) upsertDiffMessage(path, diff string) {
	m.historyVersion++
	prefix := "DIFF:\n" + path + "\n"
	for i := len(m.messages) - 1; i >= 0; i-- {
		// Stop at the turn boundary — never overwrite diffs from previous user turns
		if strings.HasPrefix(m.messages[i], "YOU:\n") || strings.HasPrefix(m.messages[i], "👤 ") {
			break
		}
		if strings.HasPrefix(m.messages[i], prefix) {
			m.messages[i] = prefix + diff
			return
		}
	}
	m.appendMessages(prefix + diff)
}

// appendNote adds a UI/informational message to the chat AND persists it as a
// system_msg event so slash-command output (e.g. /help, /diagnose) survives a
// -c resume instead of disappearing on reload.
func (m *Model) appendNote(text string) {
	m.appendMessages(text)
	if m.context != nil {
		_ = m.context.AppendSystemNote(text)
	}
}

// startTurn launches a fresh engine turn for userQuery: it records the prompt
// in history, appends it to the chat, resets streaming state, and returns the
// batch that runs the turn.
func (m *Model) startTurn(userQuery string) (tea.Model, tea.Cmd) {
	// A fresh user turn resets the file-change recorder: changes from the
	// previous turn were already summarized at its end, and the recorder must
	// never leak stale entries into the next turn's summary.
	tool.ResetChanges()
	m.filesExpanded = false
	m.turnMode = m.mode
	thisMode := m.turnMode
	m.engine.SetMode(thisMode)
	m.appendMessages("YOU:\n" + userQuery)
	m.status = "Thinking..."
	m.turnStart = time.Now()
	// Clear any stale streaming state from a previous interrupted turn.
	m.streaming = false
	m.pendingStream = ""
	m.activity = nil
	m.turnRunning = true
	// Bump the generation counter so any in-flight goroutine from the previous
	// (interrupted) turn can detect that its turnResultMsg is stale and
	// discard it — preventing premature turnRunning=false on the new turn.
	m.turnGen++
	thisGen := m.turnGen

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTurn = cancel

	m.engine.SetStreamHandler(func(delta string) {
		if m.quitting {
			return
		}
		if m.prog != nil {
			m.prog.Send(streamChunkMsg(delta))
		}
	})

	// Reset active recommendations on fresh user prompt
	m.activeRecommendations = nil

	execQuery := userQuery
	if enhanced := resolveTournamentSelection(userQuery, m.messages); enhanced != "" {
		execQuery = enhanced
	}

	runTurnCmd := func() tea.Msg {
		res, err := m.engine.RunTurn(ctx, execQuery, func(state loop.LoopState, info string) {
			if m.quitting {
				return
			}
			if m.activityLog != nil {
				ts := time.Now().Format("15:04:05")
				fmt.Fprintf(m.activityLog, "[%s] %s %s\n", ts, phaseBadge(state), info)
			}
			if m.prog != nil {
				m.prog.Send(stepProgressMsg{state: state, info: info})
			}
		})
		// The program may already be exiting (ctrl+c): a Send to a
		// closed program can block forever, so drop the result instead.
		if m.quitting {
			return nil
		}
		return turnResultMsg{content: res, err: err, mode: thisMode, gen: thisGen}
	}

	return m, tea.Batch(runTurnCmd, tickCmd())
}

// triggerRecommendation executes or queues a senior quick recommendation.
func (m *Model) triggerRecommendation(idx int) (tea.Model, tea.Cmd) {
	for i := range m.activeRecommendations {
		if m.activeRecommendations[i].Index == idx && !m.activeRecommendations[i].Clicked {
			m.activeRecommendations[i].Clicked = true
			rec := m.activeRecommendations[i]
			if m.turnRunning {
				// Agent is busy executing a turn — queue for auto-run when current turn completes
				m.pendingQueue = append(m.pendingQueue, QueuedPrompt{
					Text: rec.Prompt,
					Mode: m.mode,
				})
				m.appendNote(fmt.Sprintf("📥 Queued recommendation [%d]: %s (\"%s\")", rec.Index, rec.Title, rec.Prompt))
				return m, nil
			}
			// Agent is idle — start execution immediately
			return m.startTurn(rec.Prompt)
		}
	}
	return m, nil
}

type statusUpdateMsg string
type stepProgressMsg struct {
	state loop.LoopState
	info  string
}
type streamChunkMsg string
type fileDiffMsg struct {
	path string
	diff string
}
type diagnoseResultMsg string
type diagnoseFixMsg string
type ephemeralAskResultMsg string

// phaseBadge returns a short emoji prefix for an engine phase.
func phaseBadge(s loop.LoopState) string {
	switch s {
	case loop.StateThinking:
		return "🧠"
	case loop.StateObserving:
		return "👀"
	case loop.StateVerifying:
		return "✅"
	case loop.StateBlocked:
		return "⚠️"
	case loop.StateActing:
		return "🔧"
	default:
		return ""
	}
}

// startsWithEmoji reports whether the first rune is a symbol/emoji glyph.
func startsWithEmoji(s string) bool {
	r, _ := utf8.DecodeRuneInString(strings.TrimSpace(s))
	return r >= 0x2000
}

// normalizeEmojiSpacing ensures that leading emojis have clean 1-space separation.
func normalizeEmojiSpacing(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	if r >= 0x2000 {
		rest := s[size:]
		if len(rest) > 0 {
			r2, size2 := utf8.DecodeRuneInString(rest)
			if r2 == 0xfe0f || r2 == 0xfe0e {
				size += size2
				rest = s[size:]
			}
		}
		emoji := s[:size]
		trimmedRest := strings.TrimLeft(rest, " ")
		if trimmedRest != "" {
			return emoji + " " + trimmedRest
		}
		return emoji
	}
	return s
}

// NewApp initializes the Bubble Tea v2 TUI model.
func NewApp(
	cfg provider.AppConfig,
	p provider.DetectedProvider,
	modelName string,
	adapter provider.ProviderAdapter,
	tools *tool.Registry,
	ctxMgr *bcontext.Manager,
	mcpMgr *mcp.Manager,
	lspMgr *lsp.Manager,
	scoutMgr *subagent.ScoutManager,
	budgetUSD float64,
	previousPrompts []string,
	activityLog io.Writer,
	initialMsgs ...string,
) Model {
	ti := textarea.New()
	ti.Placeholder = "Type a prompt or command (/help, /sessions, /new)..."
	ti.Prompt = ""
	ti.CharLimit = 0
	ti.DynamicHeight = true
	ti.MinHeight = 1
	ti.MaxHeight = 8
	ti.SetWidth(80)
	ti.SetHeight(1)
	// ENTER sends the prompt (handled by the app); Alt+Enter inserts a newline.
	ti.KeyMap.InsertNewline.SetKeys("alt+enter")

	// Clean look: strip line numbers and background bar
	ti.ShowLineNumbers = false
	clean := ti.Styles().Focused
	clean.Base = lipgloss.NewStyle()
	clean.CursorLine = lipgloss.NewStyle()
	clean.CursorLineNumber = lipgloss.NewStyle()
	clean.EndOfBuffer = lipgloss.NewStyle()
	clean.LineNumber = lipgloss.NewStyle()
	clean.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	clean.Prompt = lipgloss.NewStyle()
	clean.Text = lipgloss.NewStyle()
	styles := ti.Styles()
	styles.Focused = clean
	styles.Blurred = clean
	// Themed solid block cursor (teal, matching the ❯ prompt)
	styles.Cursor.Color = lipgloss.Color("86")
	styles.Cursor.Shape = tea.CursorBlock
	styles.Cursor.Blink = false
	ti.SetStyles(styles)
	ti.SetVirtualCursor(true)
	ti.Focus()

	applyInputCursorStyle := func(inp *textinput.Model) {
		inp.SetVirtualCursor(true)
		st := inp.Styles()
		st.Cursor.Color = lipgloss.Color("86")
		st.Cursor.Shape = tea.CursorBlock
		st.Cursor.Blink = false
		inp.SetStyles(st)
	}

	cti := textinput.New()
	cti.Placeholder = "Paste or type API Key here (leave empty if none)..."
	cti.Prompt = ""
	applyInputCursorStyle(&cti)

	cni := textinput.New()
	cni.Placeholder = "e.g. my-gateway, local-ai..."
	cni.Prompt = ""
	applyInputCursorStyle(&cni)

	cbi := textinput.New()
	cbi.Placeholder = "e.g. https://api.my-gateway.example/v1"
	cbi.Prompt = ""
	applyInputCursorStyle(&cbi)

	cmi := textarea.New()
	cmi.Placeholder = "Optional: JSON models block, e.g.\n{\"model-a\":{\"name\":\"Model A\",\"limit\":{\"context\":1048576,\"output\":32768}}}\n(outer braces optional — a bare key:value list also works)\n\nOr a plain JSON array: [\"model-a\", \"model-b\"]"
	cmi.Prompt = ""
	cmi.CharLimit = 0
	cmi.ShowLineNumbers = false
	cmi.MaxHeight = 6
	cmi.DynamicHeight = true
	cleanCM := cmi.Styles().Focused
	cleanCM.Base = lipgloss.NewStyle()
	cleanCM.CursorLine = lipgloss.NewStyle()
	cleanCM.EndOfBuffer = lipgloss.NewStyle()
	cleanCM.LineNumber = lipgloss.NewStyle()
	cleanCM.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cleanCM.Prompt = lipgloss.NewStyle()
	cleanCM.Text = lipgloss.NewStyle()
	cmStyles := cmi.Styles()
	cmStyles.Focused = cleanCM
	cmStyles.Blurred = cleanCM
	cmStyles.Cursor.Color = lipgloss.Color("86")
	cmStyles.Cursor.Shape = tea.CursorBlock
	cmStyles.Cursor.Blink = false
	cmi.SetStyles(cmStyles)
	cmi.SetVirtualCursor(true)

	aci := textinput.New()
	aci.Placeholder = "Type your custom answer..."
	aci.Prompt = ""
	applyInputCursorStyle(&aci)

	// MCP add-wizard inputs (clean, un-focused by default).
	mcpName := textinput.New()
	mcpName.Placeholder = "e.g. github, filesystem, my-tools..."
	mcpName.Prompt = ""
	applyInputCursorStyle(&mcpName)
	mcpCmd := textinput.New()
	mcpCmd.Placeholder = "e.g. npx -y @modelcontextprotocol/server-github"
	mcpCmd.Prompt = ""
	applyInputCursorStyle(&mcpCmd)
	mcpURL := textinput.New()
	mcpURL.Placeholder = "e.g. https://mcp.notion.com/mcp"
	mcpURL.Prompt = ""
	applyInputCursorStyle(&mcpURL)

	brk := newAskBroker()
	if askTool, ok := tools.Lookup("ask_user").(*tool.AskUserTool); ok {
		askTool.Ask = brk.Ask
	}
	tools.SetUserAskHandler(brk.Ask)
	fbrk := newFileConfirmBroker()
	tools.SetFileActionHandler(fbrk.Confirm)

	msgs := []string{welcomeBanner()}
	if len(initialMsgs) > 0 {
		msgs = initialMsgs
	}

	initialMode := "BUILDER"
	if st := ctxMgr.Store(); st != nil {
		if mode, err := st.GetSessionMode(ctxMgr.SessionID()); err == nil && mode != "" {
			initialMode = mode
		}
	}

	m := Model{
		cfg:                 cfg,
		activeProvider:      p,
		activeModel:         modelName,
		adapter:             adapter,
		tools:               tools,
		context:             ctxMgr,
		mcpMgr:              mcpMgr,
		lspMgr:              lspMgr,
		scoutMgr:            scoutMgr,
		mode:                initialMode,
		budgetUSD:           budgetUSD,
		mouseMode:           "SCROLL",
		status:              "Ready",
		messages:            msgs,
		promptInput:         ti,
		connectTextInput:    cti,
		connectNameInput:    cni,
		connectBaseURLInput: cbi,
		connectModelsInput:  cmi,
		mcpAddName:          mcpName,
		mcpAddCmd:           mcpCmd,
		mcpAddURL:           mcpURL,
		promptHistory:       previousPrompts,
		historyIdx:          len(previousPrompts),
		ask:                 brk,
		fileConfirm:         fbrk,
		askCustomInput:      aci,
		logViewport:         func() viewport.Model { vp := viewport.New(); vp.MouseWheelDelta = 1; return vp }(),
		askViewport:         func() viewport.Model { vp := viewport.New(); vp.MouseWheelDelta = 1; return vp }(),
		sessionsViewport:    func() viewport.Model { vp := viewport.New(); vp.MouseWheelDelta = 1; return vp }(),
		mcpSummary:          summarizeMCP(mcpMgr),
		activityLog:         activityLog,
	}

	cwd, _ := os.Getwd()
	m.globalIndex = search.BuildGlobalIndex(cwd)
	m.tools.Register(&tool.CodeLocateTool{Index: m.globalIndex})
	m.tools.Register(&tool.CodeSliceTool{Index: m.globalIndex})
	m.tools.Register(&tool.BlastRadiusTool{Index: m.globalIndex})
	m.tools.Register(&tool.CheckpointTool{})
	m.tools.Register(&tool.RunTestsTool{Plan: loop.TestCommandPlan})
	if m.scoutMgr != nil && m.scoutMgr.Runner != nil {
		m.tools.Register(&subagent.SwarmTool{Runner: m.scoutMgr.Runner})
	}

	for _, msg := range msgs {
		if strings.HasPrefix(msg, "FILES:\n") {
			m.filesExpanded = true
			break
		}
	}

	m.modelOptions = provider.DiscoverModels(cfg)
	m.modelListCache = nil
	m.lastLiveModelsVersion = provider.LiveModelsVersion()

	// If global index is still building or live models haven't arrived yet,
	// show a spinner so the user knows something is happening.
	if (m.globalIndex != nil && !m.globalIndex.IsReady()) || m.lastLiveModelsVersion == 0 {
		m.status = "Initializing..."
	}
	m.rebuildEngine()
	m.initialized = true
	return m
}

// contextWindow returns the context window for the active model.
func (m *Model) contextWindow() int {
	if w := provider.ContextWindowFor(m.cfg, m.activeProvider.Info.ID, m.activeModel); w > 0 {
		return w
	}
	return 128000
}

// swarmCheapModel returns the model used for mechanical swarm roles (BUILDER / AUDITOR).
func (m *Model) swarmCheapModel() string {
	if cm := os.Getenv("BROCODE_SWARM_CHEAP_MODEL"); cm != "" {
		return cm
	}
	return os.Getenv("BROCODE_COMPACT_MODEL")
}

func (m *Model) allProjectFiles() []string {
	if m.globalIndex != nil {
		raw := m.globalIndex.Files()
		cwd, _ := os.Getwd()
		if cwd == "" {
			return raw
		}
		res := make([]string, 0, len(raw))
		for _, f := range raw {
			if rel, err := filepath.Rel(cwd, f); err == nil && !strings.HasPrefix(rel, "..") {
				res = append(res, filepath.ToSlash(rel))
			} else {
				res = append(res, filepath.ToSlash(f))
			}
		}
		return res
	}
	return nil
}

func (m *Model) SetProgram(p *tea.Program) {
	m.prog = p
	if m.ask != nil {
		m.ask.prog = p
	}
}

// buildFallbacks returns automatic fallback adapters for every other detected provider.
func (m *Model) buildFallbacks() []loop.Fallback {
	var fbs []loop.Fallback
	for _, d := range provider.AutoDetect(m.cfg) {
		if m.activeProvider.Info.ID != "" && d.Info.ID == m.activeProvider.Info.ID {
			continue
		}
		var a provider.ProviderAdapter
		switch {
		case d.Info.ID == "opencode":
			a = provider.NewOpenCodeAdapter()
		case d.Info.Protocol == "anthropic":
			a = provider.NewAnthropicAdapter(d.Info.DefaultBaseURL, d.APIKey)
		default:
			a = provider.NewOpenAIAdapter(d.Info.DefaultBaseURL, d.APIKey)
		}
		model := ""
		if len(d.Info.DefaultModels) > 0 {
			model = d.Info.DefaultModels[0]
		}
		if model == "" {
			model = "deepseek-v4-flash-free"
		}
		fbs = append(fbs, loop.Fallback{ID: d.Info.ID, Protocol: d.Info.Protocol, Adapter: a, Model: model})
	}
	return fbs
}

// rebuildEngine recreates the loop engine with the current adapter/model and
// wires automatic fallbacks + the project context overview.
func (m *Model) rebuildEngine() {
	m.engine = loop.NewEngine(m.adapter, m.tools, m.context, m.activeModel)
	m.engine.SetMode(m.mode)
	m.engine.SetBudgetUSD(m.budgetUSD)
	if m.ask != nil {
		m.engine.SetAskHandler(func(question string, options []string) (string, error) {
			results, err := m.ask.Ask(context.Background(), []tool.AskQuestion{
				{
					Question: question,
					Options:  options,
					Multi:    false,
				},
			})
			if err != nil || len(results) == 0 {
				return "", err
			}
			if len(results[0].Answers) > 0 {
				return results[0].Answers[0], nil
			}
			if results[0].Custom != "" {
				return results[0].Custom, nil
			}
			return "", nil
		})
		if m.scoutMgr != nil && m.scoutMgr.Runner != nil {
			m.scoutMgr.Runner.Adapter = m.adapter
			m.scoutMgr.Runner.Model = m.activeModel
			m.scoutMgr.Runner.ContextWindow = m.contextWindow()
			m.scoutMgr.Runner.CheapModel = m.swarmCheapModel()
			m.scoutMgr.Runner.UsageTracker = m.engine.UsageTracker()
			m.scoutMgr.Runner.StreamHandler = func(delta string) {
				if m.quitting {
					return
				}
				if m.prog != nil {
					m.prog.Send(streamChunkMsg(delta))
				}
			}
			m.scoutMgr.Runner.Ask = func(question string, opts []string) (string, error) {
				results, err := m.ask.Ask(context.Background(), []tool.AskQuestion{
					{
						Question: question,
						Options:  opts,
						Multi:    false,
					},
				})
				if err != nil || len(results) == 0 {
					return "", err
				}
				if len(results[0].Answers) > 0 {
					return results[0].Answers[0], nil
				}
				return results[0].Custom, nil
			}
		}
	} else if m.scoutMgr != nil && m.scoutMgr.Runner != nil {
		m.scoutMgr.Runner.Adapter = m.adapter
		m.scoutMgr.Runner.Model = m.activeModel
		m.scoutMgr.Runner.ContextWindow = m.contextWindow()
		m.scoutMgr.Runner.CheapModel = m.swarmCheapModel()
		m.scoutMgr.Runner.UsageTracker = m.engine.UsageTracker()
		m.scoutMgr.Runner.StreamHandler = func(delta string) {
			if m.quitting {
				return
			}
			if m.prog != nil {
				m.prog.Send(streamChunkMsg(delta))
			}
		}
	}
	if m.projectCtx == nil {
		cwd, _ := os.Getwd()
		m.projectCtx = search.BuildProjectContext(cwd)
	}
	m.engine.SetProjectContext(m.projectCtx.String())
	cwd, _ := os.Getwd()
	if m.repoMap == nil || m.usage == nil {
		m.usage = repo.NewUsage(cwd)
		m.repoMap = repo.BuildMap(cwd, m.usage)
	}
	if m.repoMap != nil {
		m.engine.SetRepoMap(m.repoMap.String())
		stackHints := make([]prompt.Stack, 0, len(m.repoMap.Stacks))
		for _, s := range m.repoMap.Stacks {
			stackHints = append(stackHints, prompt.Stack{Name: s.Name, Files: s.Files})
		}
		m.engine.SetDetectedStacks(stackHints)
	}
	m.engine.SetUsageRecorder(func(paths []string) {
		m.usage.Record(paths)
		m.usage.Save()
	})
	m.engine.SetOnFileEdited(func(path string) {
		if m.globalIndex != nil {
			m.globalIndex.RefreshFile(path)
		}
	})
	m.engine.SetGlobalIndex(m.globalIndex)
	m.engine.SetOnChange(func(path, diff string) {
		if m.prog != nil {
			m.prog.Send(fileDiffMsg{path: path, diff: diff})
		}
	})
	skill.EnsureGlobalDefaultsInstalled()
	m.engine.SetSkillCatalog(skillEntries(cwd))

	if m.agentLoader == nil {
		m.agentLoader = agent.NewLoader(cwd)
	}
	if m.activeAgent != nil {
		m.engine.SetAgentPrompt(m.activeAgent.Prompt)
		m.tools.SetToolFilter(m.activeAgent.IsToolAllowed)
		m.tools.SetCommandFilter(m.activeAgent.CheckCommand)
	} else {
		m.engine.SetAgentPrompt("")
		m.tools.SetToolFilter(nil)
		m.tools.SetCommandFilter(nil)
	}
	m.engine.SetTuning(prompt.LoadTuning(prompt.DefaultTuningPath()))
	if m.memStore == nil {
		m.memStore = memory.NewStore(cwd)
	}
	m.engine.SetMemoryStore(m.memStore)
	m.tools.SetMemoryStore(m.memStore)
	if st := m.context.Store(); st != nil {
		m.engine.SetKnowledgeStore(st)
		m.tools.SetKnowledgeStore(st)
	}
	m.tools.SetSearchEmbedder(embedderFor(m.activeProvider))
	m.memStore.SetEmbedder(embedderFor(m.activeProvider))
	m.engine.SetDiagnosticsChecker(func(path string) string {
		if m.lspMgr == nil {
			return ""
		}
		out, err := m.lspMgr.Diagnostics(context.Background(), path)
		if err != nil {
			return ""
		}
		return out
	})
	m.engine.SetSymbolsProvider(func() map[string]map[string]bool {
		if m.globalIndex != nil {
			return m.globalIndex.AllSymbols()
		}
		return nil
	})
	if m.lspMgr != nil {
		m.engine.SetLSPStatus(len(m.lspMgr.AvailableServers()))
	}
	for _, fb := range m.buildFallbacks() {
		m.engine.AddFallback(fb)
	}
	m.engine.SetPrimaryIdentity(m.activeProvider.Info.ID, m.activeProvider.Info.Protocol)
	m.engine.SetFallbackPolicy(m.cfg.FallbackPolicy)
	hk := hooks.Load(cwd)
	if m.activeAgent != nil {
		hk.AddHooks(m.activeAgent.ToHooks())
	}
	m.engine.SetHooks(hk)
	m.engine.SetScoutManager(m.scoutMgr)
	if cm := os.Getenv("BROCODE_COMPACT_MODEL"); cm != "" {
		m.engine.SetCompactModel(cm)
	}
	if bd := os.Getenv("BROCODE_TOOL_DESC_BUDGET"); bd != "" {
		if n, err := strconv.Atoi(bd); err == nil {
			m.engine.SetToolDescBudget(n)
		}
	}
	if lp := learn.DefaultPath(); lp != "" {
		m.engine.SetLearner(learn.NewLearner(lp))
	}
}


// embedderFor returns an embeddings endpoint for the active provider.
func embedderFor(p provider.DetectedProvider) *search.Embedder {
	if p.Info.Protocol != "openai-compatible" || p.Info.DefaultBaseURL == "" {
		return nil
	}
	return search.NewEmbedder(p.Info.DefaultBaseURL, p.APIKey, "text-embedding-3-small")
}

// skillEntries converts installed skills into catalog entries.
func skillEntries(workspaceDir string) []prompt.SkillEntry {
	all := skill.NewLoader(workspaceDir).All()
	entries := make([]prompt.SkillEntry, 0, len(all))
	for _, s := range all {
		entries = append(entries, prompt.SkillEntry{
			Name:        s.Name,
			Description: s.Description,
			Path:        s.Path,
		})
	}
	return entries
}

type versionCheckResultMsg struct {
	latest    string
	hasUpdate bool
}

func checkUpdateCmd() tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	latest, hasUpdate, err := version.CheckLatestVersion(ctx, false)
	if err == nil && hasUpdate {
		return versionCheckResultMsg{latest: latest, hasUpdate: true}
	}
	return nil
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.promptInput.Focus(), checkUpdateCmd, liveModelsRefreshCmd(), tickCmd(), startupReadinessCheckCmd())
}
