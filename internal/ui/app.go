package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/glamour"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/tool"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type spinnerTickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

var mdRenderer, _ = glamour.NewTermRenderer(
	glamour.WithStandardStyle("dark"),
	glamour.WithWordWrap(90),
)

func renderMarkdown(text string) string {
	if mdRenderer == nil {
		return text
	}
	out, err := mdRenderer.Render(text)
	if err != nil || strings.TrimSpace(out) == "" {
		return text
	}
	res := strings.TrimSpace(out)

	// Clean up any remaining unparsed **text** into bold lipgloss styling
	if strings.Contains(res, "**") {
		boldStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
		parts := strings.Split(res, "**")
		var sb strings.Builder
		for i, p := range parts {
			if i%2 == 1 && p != "" {
				sb.WriteString(boldStyle.Render(p))
			} else {
				sb.WriteString(p)
			}
		}
		res = sb.String()
	}

	return res
}

// Model defines the Bubble Tea v2 TUI state.
type Model struct {
	width          int
	height         int
	cfg            provider.AppConfig
	activeProvider provider.DetectedProvider
	activeModel    string
	adapter        provider.ProviderAdapter
	tools          *tool.Registry
	context        *bcontext.Manager
	engine         *loop.Engine
	mode           string // "BUILDER" or "PLANNER"
	messages       []string
	status         string

	// Reference to running Bubble Tea program for async event broadcasting
	prog *tea.Program

	// Cancelation function for active LLM turn / tool execution
	cancelTurn context.CancelFunc

	// Spinner animation state
	spinnerIdx int

	// Multi-line prompt input: soft-wraps at terminal width and grows
	// into new lines instead of overflowing the frame.
	promptInput textarea.Model

	// Prompt History (UP/DOWN navigation)
	promptHistory []string
	historyIdx    int

	// Modals
	showModels   bool
	showDebug    bool
	showConnect  bool
	showSessions bool
	modelsQuery  string
	modelOptions map[string][]string
	modelsSel    int

	// Sessions Modal State
	sessionList []store.Session
	sessionsSel int

	// Connect Modal State (2-Step Wizard)
	connectStep        int // 0 = select provider, 1 = enter key
	connectProviderSel int
	connectTextInput   textinput.Model
}

type turnResultMsg struct {
	content string
	err     error
}

type statusUpdateMsg string
type stepProgressMsg string

// NewApp initializes the Bubble Tea v2 TUI model.
func NewApp(
	cfg provider.AppConfig,
	p provider.DetectedProvider,
	modelName string,
	adapter provider.ProviderAdapter,
	tools *tool.Registry,
	ctxMgr *bcontext.Manager,
	initialMsgs ...string,
) Model {
	ti := textarea.New()
	ti.Placeholder = "Type a prompt or command (/help, /sessions, /new)..."
	ti.Prompt = ""
	ti.CharLimit = 0
	ti.DynamicHeight = true
	ti.MinHeight = 1
	ti.MaxHeight = 8
	// ENTER sends the prompt (handled by the app); Alt+Enter inserts a newline.
	ti.KeyMap.InsertNewline.SetKeys("alt+enter")
	ti.Focus()

	cti := textinput.New()
	cti.Placeholder = "Paste or type API Key here..."
	cti.Prompt = ""

	msgs := []string{"⚡ BroCode engine active. Type a prompt or /help for commands."}
	if len(initialMsgs) > 0 {
		msgs = initialMsgs
	}

	m := Model{
		cfg:              cfg,
		activeProvider:   p,
		activeModel:      modelName,
		adapter:          adapter,
		tools:            tools,
		context:          ctxMgr,
		mode:             "BUILDER",
		status:           "Ready",
		messages:         msgs,
		promptInput:      ti,
		connectTextInput: cti,
		promptHistory:    []string{},
		historyIdx:       0,
	}
	m.engine = loop.NewEngine(adapter, tools, ctxMgr, modelName)
	m.modelOptions = provider.DiscoverModels(cfg)
	return m
}

func (m *Model) SetProgram(p *tea.Program) {
	m.prog = p
}

func (m Model) Init() tea.Cmd {
	return m.promptInput.Focus()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.width > 0 {
			// Reserve room for the "❯ " prompt and a small right margin so
			// the input soft-wraps inside the terminal instead of overflowing.
			m.promptInput.SetWidth(m.width - 4)
		}

	case spinnerTickMsg:
		if m.status != "Ready" && m.status != "Failed" {
			m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
			return m, tickCmd()
		}
		return m, nil

	case stepProgressMsg:
		str := string(msg)
		m.status = str
		m.messages = append(m.messages, "PROCESS:\n⚡ "+str)
		return m, nil

	case turnResultMsg:
		if msg.err != nil {
			m.messages = append(m.messages, "ERROR: "+msg.err.Error())
			m.status = "Failed"
		} else if msg.content != "" {
			m.messages = append(m.messages, "BROCODE:\n"+msg.content)
			m.status = "Ready"
		}

	case statusUpdateMsg:
		m.status = string(msg)

	case tea.KeyMsg:
		keyStr := msg.String()

		// Intercept explicit paste shortcuts (ctrl+v) using OS clipboard
		if keyStr == "ctrl+v" {
			if clipText, err := clipboard.ReadAll(); err == nil && clipText != "" {
				cleanClip := strings.TrimSpace(strings.ReplaceAll(clipText, "\r\n", "\n"))

				if m.showConnect && m.connectStep == 1 {
					m.connectTextInput.SetValue(m.connectTextInput.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
					return m, nil
				} else if m.showModels {
					m.modelsQuery += strings.ReplaceAll(cleanClip, "\n", "")
					return m, nil
				} else if !m.showConnect && !m.showModels && !m.showDebug && !m.showSessions {
					// Keep newlines: the prompt input is multi-line now.
					m.promptInput.InsertString(cleanClip)
					return m, nil
				}
			}
		}

		switch keyStr {
		case "ctrl+c":
			return m, tea.Quit

		case "tab", "shift+tab":
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions {
				if m.mode == "BUILDER" {
					m.mode = "PLANNER"
				} else {
					m.mode = "BUILDER"
				}
				m.engine.SetMode(m.mode)
				return m, nil
			}

		case "esc":
			if m.showModels {
				m.showModels = false
				return m, nil
			}
			if m.showSessions {
				m.showSessions = false
				return m, nil
			}
			if m.showDebug {
				m.showDebug = false
				return m, nil
			}
			if m.showConnect {
				if m.connectStep > 0 {
					m.connectStep = 0
				} else {
					m.showConnect = false
				}
				return m, nil
			}

			// Interrupt active running turn if user presses ESC
			if m.status != "Ready" && m.status != "Failed" {
				if m.cancelTurn != nil {
					m.cancelTurn()
					m.cancelTurn = nil
				}
				m.status = "Ready"
				m.messages = append(m.messages, "⚡ Interrupted turn execution.")
				return m, nil
			}

		case "enter":
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

			if m.showConnect {
				if m.connectStep == 0 {
					m.connectStep = 1
					m.connectTextInput.SetValue("")
					m.connectTextInput.Focus()
				} else {
					m.applyConnectConfig()
					m.showConnect = false
				}
				return m, nil
			}

			userQuery := strings.TrimSpace(m.promptInput.Value())
			if userQuery == "" {
				return m, nil
			}

			m.promptInput.Reset()

			// Save to Prompt History
			m.promptHistory = append(m.promptHistory, userQuery)
			m.historyIdx = len(m.promptHistory)

			// Handle Slash Commands
			if strings.HasPrefix(userQuery, "/") {
				return m.handleSlashCommand(userQuery)
			}

			m.messages = append(m.messages, "YOU:\n"+userQuery)
			m.status = "Thinking..."

			ctx, cancel := context.WithCancel(context.Background())
			m.cancelTurn = cancel

			runTurnCmd := func() tea.Msg {
				res, err := m.engine.RunTurn(ctx, userQuery, func(state loop.LoopState, info string) {
					if m.prog != nil {
						m.prog.Send(stepProgressMsg(info))
					}
				})
				return turnResultMsg{content: res, err: err}
			}

			return m, tea.Batch(runTurnCmd, tickCmd())

		case "up":
			if m.showModels && m.modelsSel > 0 {
				m.modelsSel--
				return m, nil
			}
			if m.showSessions && m.sessionsSel > 0 {
				m.sessionsSel--
				return m, nil
			}
			if m.showConnect && m.connectStep == 0 && m.connectProviderSel > 0 {
				m.connectProviderSel--
				return m, nil
			}
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !strings.Contains(m.promptInput.Value(), "\n") {
				if len(m.promptHistory) > 0 {
					if m.historyIdx > 0 {
						m.historyIdx--
					}
					m.promptInput.SetValue(m.promptHistory[m.historyIdx])
					return m, nil
				}
			}

		case "down":
			if m.showModels {
				items := m.getModelList()
				if m.modelsSel < len(items)-1 {
					m.modelsSel++
				}
				return m, nil
			}
			if m.showSessions && m.sessionsSel < len(m.sessionList)-1 {
				m.sessionsSel++
				return m, nil
			}
			if m.showConnect && m.connectStep == 0 && m.connectProviderSel < len(provider.BuiltinProviders)-1 {
				m.connectProviderSel++
				return m, nil
			}
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !strings.Contains(m.promptInput.Value(), "\n") {
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
		}
	}

	// Update text inputs based on active mode
	if m.showConnect && m.connectStep == 1 {
		var cmd tea.Cmd
		m.connectTextInput, cmd = m.connectTextInput.Update(msg)
		cmds = append(cmds, cmd)
	} else if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions {
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) handleSlashCommand(cmd string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(cmd)
	switch parts[0] {
	case "/help":
		m.messages = append(m.messages, "📖 Commands:\n/sessions, /history - Switch or manage past sessions\n/new - Create a new clean session\n/models - Open interactive model picker\n/model <provider>/<model> - Switch active model\n/connect - Setup API Key & Provider interactively (2-step wizard)\n/debug-context - View active LLM context & session tokens\n/clear - Clear chat screen")

	case "/sessions", "/history":
		if st := m.context.Store(); st != nil {
			cwd, _ := os.Getwd()
			if list, err := st.ListSessionsByProjectPath(cwd); err == nil {
				m.sessionList = list
				m.sessionsSel = 0
				m.showSessions = true
			} else {
				m.messages = append(m.messages, "❌ Failed to list sessions: "+err.Error())
			}
		} else {
			m.messages = append(m.messages, "⚠️ Session store not initialized.")
		}

	case "/new":
		cwd, _ := os.Getwd()
		newSessID := fmt.Sprintf("sess_%d", time.Now().Unix())
		st := m.context.Store()
		if st != nil {
			_ = st.CreateSession(newSessID, cwd)
		}
		m.context = bcontext.NewManager(newSessID, st, 128000)
		m.engine = loop.NewEngine(m.adapter, m.tools, m.context, m.activeModel)
		m.messages = []string{fmt.Sprintf("✅ Started new session: %s", newSessID)}

	case "/models":
		m.showModels = true
		m.modelsQuery = ""
		m.modelsSel = 0

	case "/connect":
		if len(parts) >= 3 {
			pID := strings.ToLower(parts[1])
			apiKey := parts[2]
			m.saveProviderKey(pID, apiKey)
		} else {
			m.showConnect = true
			m.connectStep = 0
			m.connectTextInput.SetValue("")
			m.connectProviderSel = 0
		}

	case "/debug-context":
		m.showDebug = true

	case "/clear":
		m.messages = []string{"⚡ Chat history cleared."}

	case "/model":
		if len(parts) > 1 {
			target := parts[1]
			sub := strings.Split(target, "/")
			if len(sub) == 2 {
				pID := sub[0]
				m.activeModel = sub[1]
				m.switchProviderAndModel(pID, m.activeModel)
			} else {
				m.activeModel = target
				m.messages = append(m.messages, fmt.Sprintf("✅ Model switched to %s", m.activeModel))
				m.engine = loop.NewEngine(m.adapter, m.tools, m.context, m.activeModel)
			}
		} else {
			m.messages = append(m.messages, "Usage: /model <provider>/<model> or /model <model_name>")
		}
	}
	return *m, nil
}

func (m *Model) applySelectedSession() {
	if m.sessionsSel >= 0 && m.sessionsSel < len(m.sessionList) {
		targetSess := m.sessionList[m.sessionsSel]
		st := m.context.Store()
		m.context = bcontext.NewManager(targetSess.ID, st, 128000)

		// Load past events into context and message log
		m.messages = []string{fmt.Sprintf("✅ Switched to session: %s", targetSess.ID)}
		if st != nil {
			if events, err := st.GetSessionEvents(targetSess.ID); err == nil {
				for _, ev := range events {
					text := bcontext.ExtractEventContent(ev.PayloadJSON)
					if ev.Type == "user_msg" {
						_ = m.context.AppendUserMessage(text)
						m.messages = append(m.messages, "YOU:\n"+text)
					} else if ev.Type == "assistant_msg" {
						_ = m.context.AppendAssistantTurn("", text, nil)
						m.messages = append(m.messages, "BROCODE:\n"+text)
					}
				}
			}
		}
		m.engine = loop.NewEngine(m.adapter, m.tools, m.context, m.activeModel)
	}
}

func (m *Model) isProviderConfigured(pID, envVar string) bool {
	if pID == "opencode" {
		return true
	}
	if custom, ok := m.cfg.Providers[pID]; ok && (custom.APIKey != "" || (custom.APIKeyEnv != "" && os.Getenv(custom.APIKeyEnv) != "")) {
		return true
	}
	if envVar != "" && os.Getenv(envVar) != "" {
		return true
	}
	return false
}

func (m *Model) saveProviderKey(pID, apiKey string) {
	pID = strings.ToLower(pID)
	found := false
	var targetProvider provider.ProviderInfo
	for _, p := range provider.BuiltinProviders {
		if p.ID == pID {
			targetProvider = p
			found = true
			break
		}
	}

	if !found {
		// Custom provider
		targetProvider = provider.ProviderInfo{
			ID:             pID,
			Name:           pID + " (Custom)",
			Protocol:       "openai-compatible",
			DefaultBaseURL: "https://api.openai.com/v1",
			DefaultModels:  []string{"default"},
		}
	}

	if m.cfg.Providers == nil {
		m.cfg.Providers = make(map[string]provider.CustomProviderConfig)
	}

	m.cfg.Providers[pID] = provider.CustomProviderConfig{
		Protocol:  targetProvider.Protocol,
		BaseURL:   targetProvider.DefaultBaseURL,
		APIKeyEnv: targetProvider.APIKeyEnvVar,
		APIKey:    apiKey,
		Models:    targetProvider.DefaultModels,
	}

	if err := provider.SaveGlobalConfig(m.cfg); err != nil {
		m.messages = append(m.messages, fmt.Sprintf("❌ Failed to save config: %v", err))
		return
	}

	m.messages = append(m.messages, fmt.Sprintf("✅ API Key for %s saved to ~/.config/brocode/config.json!", targetProvider.Name))

	// Re-detect providers and switch if appropriate
	m.modelOptions = provider.DiscoverModels(m.cfg)
	m.switchProviderAndModel(pID, targetProvider.DefaultModels[0])
}

func (m *Model) applyConnectConfig() {
	if m.connectProviderSel < 0 || m.connectProviderSel >= len(provider.BuiltinProviders) {
		return
	}
	p := provider.BuiltinProviders[m.connectProviderSel]
	keyVal := strings.TrimSpace(m.connectTextInput.Value())
	if keyVal != "" {
		m.saveProviderKey(p.ID, keyVal)
	}
}

type modelOptionItem struct {
	ProviderID string
	ModelName  string
}

func (m *Model) getModelList() []modelOptionItem {
	var items []modelOptionItem
	var providerIDs []string
	for pID := range m.modelOptions {
		providerIDs = append(providerIDs, pID)
	}
	sort.Strings(providerIDs)

	for _, pID := range providerIDs {
		list := m.modelOptions[pID]
		for _, mod := range list {
			if m.modelsQuery != "" {
				q := strings.ToLower(m.modelsQuery)
				if !strings.Contains(strings.ToLower(mod), q) && !strings.Contains(strings.ToLower(pID), q) {
					continue
				}
			}
			items = append(items, modelOptionItem{ProviderID: pID, ModelName: mod})
		}
	}
	return items
}

func (m *Model) switchProviderAndModel(pID, modelName string) {
	detected := provider.AutoDetect(m.cfg)
	for _, d := range detected {
		if d.Info.ID == pID {
			m.activeProvider = d
			m.activeModel = modelName
			m.cfg.DefaultProvider = pID
			m.cfg.DefaultModel = modelName
			_ = provider.SaveGlobalConfig(m.cfg)

			if pID == "opencode" {
				m.adapter = provider.NewOpenCodeAdapter()
			} else if d.Info.Protocol == "anthropic" {
				m.adapter = provider.NewAnthropicAdapter(d.Info.DefaultBaseURL, d.APIKey)
			} else {
				m.adapter = provider.NewOpenAIAdapter(d.Info.DefaultBaseURL, d.APIKey)
			}
			m.engine = loop.NewEngine(m.adapter, m.tools, m.context, m.activeModel)
			m.engine.SetMode(m.mode)
			m.messages = append(m.messages, fmt.Sprintf("✅ Active model set & saved: %s/%s", pID, modelName))
			return
		}
	}
	m.activeModel = modelName
	m.cfg.DefaultModel = modelName
	_ = provider.SaveGlobalConfig(m.cfg)
	m.messages = append(m.messages, fmt.Sprintf("⚠️ Model set & saved to %s", modelName))
}

func (m *Model) applySelectedModel() {
	items := m.getModelList()
	if m.modelsSel >= 0 && m.modelsSel < len(items) {
		selected := items[m.modelsSel]
		m.switchProviderAndModel(selected.ProviderID, selected.ModelName)
	}
}

func (m Model) View() tea.View {
	var content string
	if m.showModels {
		content = m.renderModelsModal()
	} else if m.showSessions {
		content = m.renderSessionsModal()
	} else if m.showConnect {
		content = m.renderConnectModal()
	} else if m.showDebug {
		content = m.renderDebugModal()
	} else {
		var sb strings.Builder

		// Message Log — wrap every message to the terminal width so long
		// lines fold inside the frame instead of breaking through it.
		contentWidth := m.width - 4
		for _, msg := range m.messages {
			sb.WriteString(formatMessage(msg, contentWidth) + "\n\n")
		}

		// Live Bottom Status Spinner Indicator (when thinking/acting/verifying)
		if m.status != "Ready" && m.status != "Failed" {
			spinnerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
			frame := spinnerFrames[m.spinnerIdx%len(spinnerFrames)]
			sb.WriteString(spinnerStyle.Render(frame+" "+m.status) + "\n\n")
		}

		// Input Box (Borderless & Minimalist, multi-line textarea that grows)
		promptStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
		promptStr := "❯ "
		if m.mode == "PLANNER" {
			promptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
			promptStr = "PLAN ❯ "
			m.promptInput.Placeholder = "Planner Mode: Ask for architecture plans, code analysis, or roadmaps..."
		} else {
			m.promptInput.Placeholder = "Type a prompt or command (/help, /sessions, /new)..."
		}
		sb.WriteString(promptStyle.Render(promptStr) + m.promptInput.View() + "\n\n")

		// STICKY BOTTOM FOOTER BANNER (Never disappears when history grows long)
		bannerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Padding(0, 1)
		if m.width > 0 {
			bannerStyle = bannerStyle.MaxWidth(m.width)
		}
		tokenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
		modeBadgeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
		if m.mode == "PLANNER" {
			modeBadgeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
		}

		sessID := m.context.SessionID()
		if len(sessID) > 16 {
			sessID = sessID[:16] + "..."
		}
		tokensStr := fmt.Sprintf("Tokens: %d/%dk", m.context.TotalTokens(), m.context.MaxWindow()/1000)

		footerBanner := fmt.Sprintf(" BROCODE CLI | Mode: %s | Provider: %s | Model: %s | Session: %s | %s ",
			modeBadgeStyle.Render(m.mode+" (Shift+Tab)"), m.activeProvider.Info.Name, m.activeModel, sessID, tokenStyle.Render(tokensStr))

		sb.WriteString(bannerStyle.Render(footerBanner) + "\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(" ENTER send · Alt+Enter newline · Tab/Shift+Tab mode · ↑/↓ history · /sessions · /models · /help "))
		content = sb.String()
	}

	v := tea.NewView(content)
	return v
}

func (m Model) renderSessionsModal() string {
	var sb strings.Builder
	sb.WriteString("=== Session Manager (/sessions, /history) ===\n\n")

	greenBadge := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	activeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)

	if len(m.sessionList) == 0 {
		sb.WriteString("No previous sessions found in SQLite database.\n")
	} else {
		for i, sess := range m.sessionList {
			cursor := "  "
			if i == m.sessionsSel {
				cursor = "❯ "
			}

			statusTag := ""
			if sess.ID == m.context.SessionID() {
				statusTag = activeStyle.Render(" [active]")
			} else {
				statusTag = greenBadge.Render(" [✓ saved]")
			}

			projName := filepath.Base(sess.ProjectPath)
			if projName == "." || projName == "/" || projName == "" {
				projName = "global"
			}
			dateStr := sess.CreatedAt.Format("2006-01-02 15:04:05")
			sb.WriteString(fmt.Sprintf("%s %-20s (%s) [%s]%s\n", cursor, sess.ID, dateStr, projName, statusTag))
		}
	}

	sb.WriteString("\n[↑/↓ navigate · ENTER switch session · /new create clean session · ESC close]")
	return lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2).Render(sb.String())
}

func (m Model) renderModelsModal() string {
	var sb strings.Builder
	sb.WriteString("=== Select AI Model (/models) ===\n")
	if m.modelsQuery != "" {
		sb.WriteString("Filter: " + m.modelsQuery + "▏\n\n")
	} else {
		sb.WriteString("Type to filter models...\n\n")
	}

	greenBadge := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	activeBadgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)

	items := m.getModelList()
	if len(items) == 0 {
		sb.WriteString("No models found matching filter.\n")
	} else {
		for idx, item := range items {
			cursor := "  "
			if idx == m.modelsSel {
				cursor = "❯ "
			}

			statusTag := ""
			if item.ModelName == m.activeModel && item.ProviderID == m.activeProvider.Info.ID {
				statusTag = activeBadgeStyle.Render(" [active]")
			} else if m.isProviderConfigured(item.ProviderID, "") {
				statusTag = greenBadge.Render(" [✓ ready]")
			}

			sb.WriteString(fmt.Sprintf("%s %-28s (%s)%s\n", cursor, item.ModelName, item.ProviderID, statusTag))
		}
	}

	sb.WriteString("\n[↑/↓ navigate · ENTER apply · ESC close]")
	return lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2).Render(sb.String())
}

func (m Model) renderConnectModal() string {
	var sb strings.Builder
	sb.WriteString("=== Connect LLM Provider (/connect) ===\n\n")

	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)

	if m.connectStep == 0 {
		sb.WriteString("Step 1/2: Select Provider to Connect:\n\n")
		for i, p := range provider.BuiltinProviders {
			cursor := "  "
			if i == m.connectProviderSel {
				cursor = "❯ "
			}

			badge := ""
			if p.ID == m.activeProvider.Info.ID {
				badge = greenStyle.Render(" [✓ active]")
			} else if m.isProviderConfigured(p.ID, p.APIKeyEnvVar) {
				badge = greenStyle.Render(" [✓ configured]")
			}

			sb.WriteString(fmt.Sprintf("%s %-25s (ID: %s)%s\n", cursor, p.Name, p.ID, badge))
		}
		sb.WriteString("\n[↑/↓ navigate · ENTER select provider · ESC cancel]")
	} else {
		target := provider.BuiltinProviders[m.connectProviderSel]
		sb.WriteString(fmt.Sprintf("Step 2/2: Enter API Key for %s (%s):\n\n", target.Name, target.ID))

		sb.WriteString("  API Key: " + m.connectTextInput.View() + "\n\n")
		sb.WriteString("[Type or paste API Key (Cmd+V, Ctrl+V, or terminal paste supported) · ENTER save · ESC back]")
	}

	return lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2).Render(sb.String())
}

func (m Model) renderDebugModal() string {
	var sb strings.Builder
	sb.WriteString("=== Active LLM Context (/debug-context) ===\n\n")
	sb.WriteString(fmt.Sprintf("Session ID: %s\nTotal Tokens: %d / %d\nEvents Count: %d\n\n",
		m.context.SessionID(), m.context.TotalTokens(), m.context.MaxWindow(), len(m.context.Messages())))

	for i, msg := range m.context.Messages() {
		sb.WriteString(fmt.Sprintf("[%d] %s:\n%s\n\n", i+1, msg.Role, msg.Content))
	}
	sb.WriteString("[ESC to return]")
	style := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(1, 2)
	if m.width > 0 {
		style = style.Width(m.width - 4)
	}
	return style.Render(sb.String())
}

func formatDiffLines(text string) string {
	lines := strings.Split(text, "\n")
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	var sb strings.Builder
	for i, line := range lines {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			sb.WriteString(greenStyle.Render(line))
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			sb.WriteString(redStyle.Render(line))
		} else if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			sb.WriteString(dimStyle.Render(line))
		} else {
			sb.WriteString(line)
		}
		if i < len(lines)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func sanitizeLLMOutput(content string) string {
	content = ansiRegex.ReplaceAllString(content, "")

	lines := strings.Split(content, "\n")
	var cleanLines []string
	skippingHeader := true

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if skippingHeader {
			if strings.Contains(trimmed, "build ·") || strings.HasPrefix(trimmed, "> build") || strings.HasPrefix(trimmed, "│ build") || trimmed == "[0m" || trimmed == "" {
				continue
			}
			skippingHeader = false
		}
		cleanLines = append(cleanLines, line)
	}

	res := strings.TrimSpace(strings.Join(cleanLines, "\n"))
	res = strings.TrimPrefix(res, "[0m")
	res = strings.TrimSuffix(res, "[0m")
	return strings.TrimSpace(res)
}

func formatMessage(msg string, width int) string {
	userLabelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
	userBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("86")).Padding(0, 1)

	botLabelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	botBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("205")).Padding(0, 1)

	processLabelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	processBarStyle := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color("240")).Padding(0, 1)

	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	// Constrain the frame to the terminal width so long lines wrap inside
	// the border instead of pushing through it.
	if width > 0 {
		userBarStyle = userBarStyle.Width(width)
		botBarStyle = botBarStyle.Width(width)
		processBarStyle = processBarStyle.Width(width)
		errStyle = errStyle.Width(width)
	}

	if strings.HasPrefix(msg, "YOU:\n") || strings.HasPrefix(msg, "👤 ") {
		content := strings.TrimPrefix(strings.TrimPrefix(msg, "YOU:\n"), "👤 ")
		return userBarStyle.Render(userLabelStyle.Render("YOU") + "\n" + content)
	}

	if strings.HasPrefix(msg, "PROCESS:\n") {
		content := strings.TrimPrefix(msg, "PROCESS:\n")
		formatted := formatDiffLines(content)
		return processBarStyle.Render(processLabelStyle.Render(formatted))
	}

	if strings.HasPrefix(msg, "BROCODE:\n") || strings.HasPrefix(msg, "🤖 ") {
		content := strings.TrimPrefix(strings.TrimPrefix(msg, "BROCODE:\n"), "🤖 ")
		content = sanitizeLLMOutput(content)

		formattedContent := renderMarkdown(content)
		if strings.Contains(formattedContent, "--- ") || strings.Contains(formattedContent, "+++ ") || strings.Contains(formattedContent, "@@ ") {
			formattedContent = formatDiffLines(formattedContent)
		}
		return botBarStyle.Render(botLabelStyle.Render("BROCODE") + "\n" + formattedContent)
	}

	if strings.HasPrefix(msg, "ERROR: ") || strings.HasPrefix(msg, "❌ ") {
		return errStyle.Render(msg)
	}

	if width > 0 {
		return lipgloss.NewStyle().Width(width).Render(msg)
	}
	return msg
}
