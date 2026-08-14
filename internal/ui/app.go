package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/glamour"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/hooks"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/lsp"
	"github.com/plumpslabs/bro-code/internal/mcp"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/skill"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/subagent"
	"github.com/plumpslabs/bro-code/internal/tool"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type spinnerTickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// mdRenderers caches a glamour renderer per wrap width so the markdown line
// length follows the terminal width instead of a fixed 90 columns (which left
// most of a wide terminal unused and broke lines too early).
var mdRenderers = struct {
	sync.Mutex
	m map[int]*glamour.TermRenderer
}{m: map[int]*glamour.TermRenderer{}}

func renderMarkdown(text string, wrap int) string {
	if wrap < 40 {
		wrap = 90 // fallback for headless/narrow terminals
	}

	mdRenderers.Lock()
	r, ok := mdRenderers.m[wrap]
	if !ok {
		r, _ = glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(wrap),
		)
		mdRenderers.m[wrap] = r
	}
	mdRenderers.Unlock()

	if r == nil {
		return text
	}
	out, err := r.Render(text)
	if err != nil || strings.TrimSpace(out) == "" {
		return text
	}
	res := strings.TrimSpace(out)

	// Glamour pads every rendered line with trailing spaces so its word wrap
	// is stable — but inside the border box those pad the lines to full width
	// and make tables/paragraphs look ragged ("acak-acakan"). Strip per-line
	// trailing whitespace; the box border is the right edge.
	res = stripTrailingWS(res)

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

// stripTrailingWS removes trailing spaces/tabs from each line (glamour pads
// lines; the border box must be the visual right edge, not invisible spaces).
func stripTrailingWS(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
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

	// MCP (Model Context Protocol) manager — status shown via /mcp command.
	mcpMgr *mcp.Manager

	// LSP code intelligence manager (lazy-spawned language servers).
	lspMgr *lsp.Manager

	// projectCtx is the cached compact project overview (structure + docs)
	// injected into the system prompt each turn.
	projectCtx *search.ProjectContext

	// scoutMgr runs background research tasks (scout tool) whose finished
	// findings the engine drains into the model's context each loop step.
	scoutMgr *subagent.ScoutManager

	// globalIndex is the persistent codebase-wide symbol + reference index
	// (built once per session) that powers the code_locate tool — repo-wide
	// "where is X and who uses it" without LSP spawns or full-file reads.
	globalIndex *search.GlobalIndex

	// memStore is the cross-session project memory (warm start + memory tool
	// + auto-extract on compaction). Built once per session, persisted to
	// .brocode/memory.md.
	memStore *memory.Store

	// Cancelation function for active LLM turn / tool execution
	cancelTurn context.CancelFunc

	// quitting is set when the user quits (ctrl+c) so in-flight turn
	// goroutines stop sending to the (already exiting) program — prevents a
	// blocked Send from leaking the turn goroutine forever.
	quitting bool

	// Spinner animation state
	spinnerIdx int

	// Live token streaming state
	streaming     bool
	pendingStream string

	// activity holds the most recent agent steps (tool calls, reasoning)
	// during a turn. Rendered live in the status slot above the input — never
	// appended to the conversation history.
	activity []string

	// interrupted is set when the user presses ESC to cancel a running turn.
	// The in-flight RunTurn then returns a "context canceled" error which must
	// NOT be shown as an ERROR row (the user already knows they cancelled).
	interrupted bool

	// Log viewport: the conversation scrolls inside a fixed-height window so
	// the terminal never repaints the whole history (flicker fix) and the
	// input/footer stay pinned. renderedLog/renderedKey cache the formatted
	// log so the expensive markdown render only runs on new messages.
	logViewport viewport.Model
	renderedLog string
	renderedKey string
	// lastUserLine is the rendered line where the last user message starts;
	// after a turn completes the viewport parks here so the user's own prompt
	// never disappears from the history (only scrolled past by long answers).
	// foundUserLine distinguishes "park at line 0" from "no user message at all".
	lastUserLine  int
	foundUserLine bool

	// renderedH remembers the log viewport height from the last re-render so
	// the parking logic can re-park the scroll when the viewport SHRINKS (the
	// live activity slot grows while a turn runs) — a height change without a
	// content change used to scroll the user's prompt out of view until the
	// turn finished.
	renderedH int

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
	sessionList      []store.Session
	sessionsSel      int
	sessionsViewport viewport.Model

	// Connect Modal State (multi-step wizard)
	connectStep         int // 0=provider pick, 1=name, 2=API key, 3=base URL, 4=models
	connectProviderSel  int
	connectCustom       bool // true when adding a brand-new custom provider
	connectNameInput    textinput.Model
	connectTextInput    textinput.Model
	connectBaseURLInput textinput.Model
	connectModelsInput  textarea.Model

	// Interactive Ask-User Modal (ask_user tool)
	ask            *askBroker
	showAsk        bool
	askID          string
	askQuestions   []tool.AskQuestion
	askCursor      int // current question index
	askOptionIdx   int // cursor within current question's rows
	askChecked     map[int]map[int]bool
	askSel         map[int]int // real selections (set on Space/select)
	askCursorPos   map[int]int // per-question cursor memory (navigation)
	askCustom      map[int]string
	askCustomQ     int // question index with custom input open, -1 = none
	askCustomInput textinput.Model
	askViewport    viewport.Model

	// mcpSummary is a compact one-liner of connected MCP servers injected into
	// OpenCode CLI prompts, so the CLI model answers MCP questions directly
	// from context instead of exploring config files with bash.
	mcpSummary string
}

type turnResultMsg struct {
	content string
	err     error
}

// maxChatMessages bounds the in-memory message list rendered by the TUI. The
// authoritative history lives in the engine context (compaction-bounded) and
// the SQLite store, so the UI only needs a window — this keeps long sessions
// from growing memory and per-frame render cost without bound.
const maxChatMessages = 200

// appendMessages adds messages to the chat log, trimming the oldest entries
// past maxChatMessages.
func (m *Model) appendMessages(msgs ...string) {
	m.messages = append(m.messages, msgs...)
	if len(m.messages) > maxChatMessages {
		m.messages = append([]string(nil), m.messages[len(m.messages)-maxChatMessages:]...)
	}
}

type statusUpdateMsg string
type stepProgressMsg string
type streamChunkMsg string

// diagnoseResultMsg carries the output of an async /diagnose project scan.
type diagnoseResultMsg string

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

	// Clean look: the textarea defaults add line numbers ("1.") and a
	// background bar on the cursor line. Strip those so the input stays
	// minimal like the previous single-line input.
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
	// Themed block cursor (teal, matching the ❯ prompt) instead of default white.
	styles.Cursor.Color = lipgloss.Color("86")
	ti.SetStyles(styles)
	ti.Focus()

	cti := textinput.New()
	cti.Placeholder = "Paste or type API Key here (leave empty if none)..."
	cti.Prompt = ""

	cni := textinput.New()
	cni.Placeholder = "e.g. my-gateway, local-ai..."
	cni.Prompt = ""

	cbi := textinput.New()
	cbi.Placeholder = "e.g. https://api.my-gateway.example/v1"
	cbi.Prompt = ""

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
	cmi.SetStyles(cmStyles)

	aci := textinput.New()
	aci.Placeholder = "Type your custom answer..."
	aci.Prompt = ""

	brk := newAskBroker()
	if askTool, ok := tools.Lookup("ask_user").(*tool.AskUserTool); ok {
		askTool.Ask = brk.Ask
	}
	// Gated bash commands reuse the same interactive modal for approval.
	tools.SetUserAskHandler(brk.Ask)
	// The OpenCode CLI adapter runs its own agent loop, so its model can't call
	// the ask_user tool — clarification questions come back as text instead.
	// Wire the same interactive modal to that path: structured [Q]/[O] blocks
	// in the CLI output are parsed and shown as the selection modal. Also feed
	// it the connected MCP server names so MCP questions get answered from
	// context instead of filesystem exploration.
	if oa, ok := adapter.(*provider.OpenCodeAdapter); ok {
		oa.AskUser = brk.Ask
		oa.MCPStatus = summarizeMCP(mcpMgr)
	}

	msgs := []string{"⚡ BroCode engine active. Type a prompt or /help for commands."}
	if len(initialMsgs) > 0 {
		msgs = initialMsgs
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
		mode:                "BUILDER",
		status:              "Ready",
		messages:            msgs,
		promptInput:         ti,
		connectTextInput:    cti,
		connectNameInput:    cni,
		connectBaseURLInput: cbi,
		connectModelsInput:  cmi,
		promptHistory:       []string{},
		historyIdx:          0,
		ask:                 brk,
		askCustomInput:      aci, logViewport: viewport.New(),
		askViewport:      viewport.New(),
		sessionsViewport: viewport.New(),
		mcpSummary:       summarizeMCP(mcpMgr),
	}

	// Persistent codebase index + checkpoint tool: built/registered once per
	// session on the shared registry (engine rebuilds reuse both).
	cwd, _ := os.Getwd()
	m.globalIndex = search.BuildGlobalIndex(cwd)
	m.tools.Register(&tool.CodeLocateTool{Index: m.globalIndex})
	m.tools.Register(&tool.CheckpointTool{})

	m.modelOptions = provider.DiscoverModels(cfg)
	m.rebuildEngine()
	return m
}

// contextWindow returns the context window for the active model — from its
// declared limit in the provider config (the /connect wizard's models block)
// — or the 128k default when the model doesn't declare one.
func (m *Model) contextWindow() int {
	if w := provider.ContextWindowFor(m.cfg, m.activeProvider.Info.ID, m.activeModel); w > 0 {
		return w
	}
	return 128000
}

func (m *Model) SetProgram(p *tea.Program) {
	m.prog = p
	if m.ask != nil {
		m.ask.prog = p
	}
}

// buildFallbacks returns automatic fallback adapters for every other detected
// provider, used when the primary provider fails mid-turn.
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
		// Fallback opencode adapters surface clarification questions through the
		// same interactive modal as the primary adapter, and carry the same MCP
		// summary so fallback runs answer MCP questions from context too.
		if oa, ok := a.(*provider.OpenCodeAdapter); ok && m.ask != nil {
			oa.AskUser = m.ask.Ask
			oa.MCPStatus = m.mcpSummary
		}
		model := ""
		if len(d.Info.DefaultModels) > 0 {
			model = d.Info.DefaultModels[0]
		}
		if model == "" {
			model = "deepseek-v4-flash-free"
		}
		fbs = append(fbs, loop.Fallback{Adapter: a, Model: model})
	}
	return fbs
}

// rebuildEngine recreates the loop engine with the current adapter/model and
// wires automatic fallbacks + the project context overview.
func (m *Model) rebuildEngine() {
	m.engine = loop.NewEngine(m.adapter, m.tools, m.context, m.activeModel)
	m.engine.SetMode(m.mode)
	if m.projectCtx == nil {
		// Build the compact project overview once (tree + AGENTS/CLAUDE/README
		// docs) so every turn starts oriented instead of blind-grepping for
		// file locations. Rebuilt on each NewApp, cached for the session.
		cwd, _ := os.Getwd()
		m.projectCtx = search.BuildProjectContext(cwd)
	}
	m.engine.SetProjectContext(m.projectCtx.String())
	// Available skills (.agents/skills, .brocode/skills, global) are listed in
	// the system prompt so the model knows what it can load and use — the
	// general, tool-agnostic standard (never .opencode/ config in the repo).
	cwd, _ := os.Getwd()
	m.engine.SetSkills(renderSkills(cwd))
	// Cross-session project memory: built once, then wired into the engine
	// (warm start + auto-extract on compaction) and the memory tool.
	if m.memStore == nil {
		m.memStore = memory.NewStore(cwd)
	}
	m.engine.SetMemoryStore(m.memStore)
	m.tools.SetMemoryStore(m.memStore)
	// Semantic search: wire an OpenAI-compatible embeddings endpoint when the
	// active provider has one, so search_code re-ranks BM25 hits by vector
	// similarity. Falls back to BM25-only on nil / bad keys / errors.
	m.tools.SetSearchEmbedder(embedderFor(m.activeProvider))
	// Native type-error review after edits: wired to the LSP manager so
	// edited files get real diagnostics (not just regex) before done.
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
	for _, fb := range m.buildFallbacks() {
		m.engine.AddFallback(fb)
	}
	// User-defined lifecycle hooks (.brocode/hooks.json) fire at turn
	// start/end/error and around tool calls. Loaded lazily on first engine
	// build; engine is rebuilt on model switches but hooks are cheap to reload.
	m.engine.SetHooks(hooks.Load(cwd))
	m.engine.SetScoutManager(m.scoutMgr)
}

// embedderFor returns an embeddings endpoint for the active provider, or nil
// when it cannot support one (non-OpenAI-compatible protocol or no reachable
// base URL). The standard text-embedding-3-small model name is tried; the
// semantic re-rank degrades gracefully to BM25 when the gateway rejects it.
func embedderFor(p provider.DetectedProvider) *search.Embedder {
	if p.Info.Protocol != "openai-compatible" || p.Info.DefaultBaseURL == "" {
		return nil
	}
	return search.NewEmbedder(p.Info.DefaultBaseURL, p.APIKey, "text-embedding-3-small")
}

// renderSkills builds the AVAILABLE SKILLS block for the system prompt from
// the general tool-agnostic skill locations (.agents/skills, .brocode/skills,
// and the global ~/.config/brocode/skills). It never reads .opencode/ from the
// repo — skills are the agnostic standard, opencode config is not.
func renderSkills(workspaceDir string) string {
	loader := skill.NewLoader(workspaceDir)
	skills := loader.All()
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, s := range skills {
		if s.Description != "" {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", s.Name, s.Description))
		} else {
			sb.WriteString(fmt.Sprintf("- %s\n", s.Name))
		}
	}
	return strings.TrimSpace(sb.String())
}

func (m Model) Init() tea.Cmd {
	return m.promptInput.Focus()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.width > 0 {
			// Reserve room for the "❯ " prompt and a small right margin so
			// the input soft-wraps inside the terminal instead of overflowing.
			m.promptInput.SetWidth(m.width - 4)
			m.logViewport.SetWidth(m.width)
			m.askViewport.SetWidth(m.width - 8)
			m.updateLogHeight()
		}

	case spinnerTickMsg:
		// While any modal is open the content is static — keep ticking would
		// re-render every 150ms and cause visible flicker on the modal.
		if m.showAsk || m.showModels || m.showConnect || m.showDebug || m.showSessions {
			return m, nil
		}
		if m.status != "Ready" && m.status != "Failed" {
			m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
			return m, tickCmd()
		}
		return m, nil

	case stepProgressMsg:
		str := string(msg)
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
		// Live agent activity is shown in the status slot ABOVE the input — it
		// must never be appended to the conversation history (that made process
		// rows pile up above the answer and hid the user's prompt).
		m.status = str
		if len(m.activity) == 0 || m.activity[len(m.activity)-1] != str {
			m.activity = append(m.activity, str)
			if len(m.activity) > 5 {
				m.activity = m.activity[len(m.activity)-5:]
			}
		}
		return m, nil

	case turnResultMsg:
		m.streaming = false
		m.pendingStream = ""
		m.activity = nil
		if msg.err != nil {
			// A user-initiated interrupt (ESC) aborts the context, which the
			// adapter reports as "context canceled". That is not an error —
			// the interruption notice was already shown when ESC was pressed.
			if m.interrupted {
				m.interrupted = false
				m.status = "Ready"
				return m, nil
			}
			m.appendMessages("ERROR: " + msg.err.Error())
			m.status = "Failed"
		} else if msg.content != "" {
			// Surface the model's reasoning (thinking) above the answer, like
			// opencode, so the agent's deliberation is visible — not just the
			// final text. Falls back to plain content when there is no reasoning.
			display := msg.content
			if r := m.lastAssistantReasoning(); r != "" {
				display = "💭 " + r + "\n\n" + msg.content
			}
			m.appendMessages("BROCODE:\n" + display)
			m.status = "Ready"
		}

	case streamChunkMsg:
		if !m.streaming {
			m.streaming = true
			m.pendingStream = ""
			m.status = "Streaming..."
		}
		m.pendingStream += string(msg)
		return m, nil

	case diagnoseResultMsg:
		m.appendMessages(string(msg))
		m.status = "Ready"

	case statusUpdateMsg:
		m.status = string(msg)

	case askUserMsg:
		m.openAsk(msg)
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
						m.connectNameInput.SetValue(m.connectNameInput.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
					case 2:
						m.connectTextInput.SetValue(m.connectTextInput.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
					case 3:
						m.connectBaseURLInput.SetValue(m.connectBaseURLInput.Value() + strings.ReplaceAll(cleanClip, "\n", ""))
					case 4:
						// Models block is JSON — keep newlines.
						m.connectModelsInput.InsertString(cleanClip)
					}
					return m, nil
				} else if m.showModels {
					m.modelsQuery += strings.ReplaceAll(cleanClip, "\n", "")
					return m, nil
				} else if !m.showConnect && !m.showModels && !m.showDebug && !m.showSessions && !m.showAsk {
					// Keep newlines: the prompt input is multi-line now.
					m.promptInput.InsertString(cleanClip)
					return m, nil
				}
			}
		}

		switch keyStr {
		case "ctrl+c":
			// Cancel any running turn first: without this, the turn goroutine
			// keeps running after the program exits and its prog.Send calls
			// can block forever — a silent goroutine leak holding the engine,
			// context and adapter.
			m.quitting = true
			if m.cancelTurn != nil {
				m.cancelTurn()
				m.cancelTurn = nil
			}
			return m, tea.Quit

		case "tab", "shift+tab":
			if m.showAsk {
				if keyStr == "shift+tab" {
					m.askNextQuestion(-1)
				} else {
					m.askNextQuestion(1)
				}
				return m, nil
			}
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
				m.connectPrev()
				return m, nil
			}

			// Interrupt active running turn if user presses ESC
			if m.status != "Ready" && m.status != "Failed" {
				if m.cancelTurn != nil {
					m.cancelTurn()
					m.cancelTurn = nil
				}
				m.interrupted = true
				m.activity = nil
				m.status = "Ready"
				m.appendMessages("⚡ Interrupted turn execution.")
				return m, nil
			}

		case "enter":
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

			if m.showConnect {
				m.connectNext()
				return m, nil
			}

			userQuery := strings.TrimSpace(m.promptInput.Value())
			if userQuery == "" {
				return m, nil
			}

			m.promptInput.Reset()

			// Save to Prompt History
			m.promptHistory = append(m.promptHistory, userQuery)
			// Bound in-memory prompt history so a long session cannot grow it
			// without limit (arrows navigate the last 200 prompts — plenty).
			if len(m.promptHistory) > 200 {
				m.promptHistory = m.promptHistory[len(m.promptHistory)-200:]
			}
			m.historyIdx = len(m.promptHistory)

			// Handle Slash Commands
			if strings.HasPrefix(userQuery, "/") {
				return m.handleSlashCommand(userQuery)
			}

			m.appendMessages("YOU:\n" + userQuery)
			m.status = "Thinking..."
			// Clear any stale streaming state from a previous interrupted turn.
			m.streaming = false
			m.pendingStream = ""
			m.activity = nil

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

			runTurnCmd := func() tea.Msg {
				res, err := m.engine.RunTurn(ctx, userQuery, func(state loop.LoopState, info string) {
					if m.quitting {
						return
					}
					if m.prog != nil {
						m.prog.Send(stepProgressMsg(info))
					}
				})
				// The program may already be exiting (ctrl+c): a Send to a
				// closed program can block forever, so drop the result instead.
				if m.quitting {
					return nil
				}
				return turnResultMsg{content: res, err: err}
			}

			return m, tea.Batch(runTurnCmd, tickCmd())

		case "space":
			if m.showAsk && m.askCustomQ < 0 {
				m.askToggle()
				return m, nil
			}

		case "pgup":
			if m.showSessions {
				m.sessionsViewport.PageUp()
				return m, nil
			}
			if !m.showAsk && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions {
				m.logViewport.PageUp()
				return m, nil
			}

		case "pgdown":
			if m.showSessions {
				m.sessionsViewport.PageDown()
				return m, nil
			}
			if !m.showAsk && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions {
				m.logViewport.PageDown()
				return m, nil
			}

		case "up":
			if m.showAsk && m.askCustomQ < 0 {
				m.askMove(-1)
				return m, nil
			}
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
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showAsk && !strings.Contains(m.promptInput.Value(), "\n") {
				if len(m.promptHistory) > 0 {
					if m.historyIdx > 0 {
						m.historyIdx--
					}
					m.promptInput.SetValue(m.promptHistory[m.historyIdx])
					return m, nil
				}
			}

		case "down":
			if m.showAsk && m.askCustomQ < 0 {
				m.askMove(1)
				return m, nil
			}
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
			if m.showConnect && m.connectStep == 0 && m.connectProviderSel < len(provider.BuiltinProviders) {
				m.connectProviderSel++
				return m, nil
			}
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showAsk && !strings.Contains(m.promptInput.Value(), "\n") {
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
	if m.showAsk && m.askCustomQ >= 0 {
		var cmd tea.Cmd
		m.askCustomInput, cmd = m.askCustomInput.Update(msg)
		cmds = append(cmds, cmd)
		m.refreshAskModal()
	} else if m.showConnect {
		switch m.connectStep {
		case 1:
			var cmd tea.Cmd
			m.connectNameInput, cmd = m.connectNameInput.Update(msg)
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
	} else if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showAsk {
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
		m.appendMessages("📖 Commands:\n/sessions, /history - Switch or manage past sessions\n/new - Create a new clean session\n/models - Open interactive model picker\n/model <provider>/<model> - Switch active model\n/connect - Setup API Key & Provider interactively (2-step wizard)\n/mcp - Show connected MCP servers & tools\n/lsp - Show code intelligence status (gopls, tsserver, ...)\n/lsp-install - Auto-install missing language servers\n/diagnose - Scan project for type errors, warnings & deprecated APIs\n/memory - Show cross-session project memory\n/cost - Show session token & estimated cost per model\n/debug-context - View active LLM context & session tokens\n/clear - Clear chat screen")

	case "/memory":
		if m.memStore != nil {
			s := m.memStore.List()
			if m.memStore.Path() != "" {
				s += "\n\n📍 " + m.memStore.Path()
			}
			m.appendMessages(s)
		} else {
			m.appendMessages("⚠️ Project memory not initialized.")
		}

	case "/cost":
		m.appendMessages(m.engine.CostSummary())

	case "/lsp":
		m.appendMessages(m.lspStatus())

	case "/diagnose":
		if m.lspMgr == nil {
			m.appendMessages("⚠️ LSP not initialized.")
			return m, nil
		}
		cwd, _ := os.Getwd()
		m.status = "Scanning project diagnostics..."
		lsp := m.lspMgr
		return m, func() tea.Msg {
			out, err := lsp.ScanDiagnostics(context.Background(), cwd)
			if err != nil {
				return diagnoseResultMsg("❌ Diagnose failed: " + err.Error())
			}
			return diagnoseResultMsg(out)
		}

	case "/lsp-install":
		if m.lspMgr == nil {
			m.appendMessages("⚠️ LSP not initialized.")
			return m, nil
		}
		lang := ""
		if len(parts) > 1 {
			lang = parts[1]
		}
		hints := m.lspMgr.InstallHints()
		if lang != "" {
			if _, ok := hints[lang]; !ok {
				m.appendMessages("⚠️ No install needed for " + lang + " (already installed or unknown).")
				return m, nil
			}
			hints = map[string]string{lang: hints[lang]}
		}
		if len(hints) == 0 {
			m.appendMessages("✅ All language servers are installed.")
			return m, nil
		}
		var sb strings.Builder
		sb.WriteString("⬇️ Installing language servers...")
		for l, c := range hints {
			sb.WriteString(fmt.Sprintf("\n  %-10s %s", l, c))
		}
		m.appendMessages(sb.String())
		m.status = "Installing language servers..."
		lsp := m.lspMgr
		return m, func() tea.Msg {
			return diagnoseResultMsg(runLSPInstalls(lsp, lang))
		}

	case "/mcp":
		m.appendMessages(m.mcpStatus())

	case "/mcp-reload":
		if m.mcpMgr != nil {
			m.mcpMgr.Close()
			m.mcpMgr.LoadDefaults()
			m.mcpMgr.Start(context.Background())
			for _, mt := range m.mcpMgr.Tools() {
				m.tools.Register(mt)
			}
			m.rebuildEngine()
			m.appendMessages(m.mcpStatus())
		} else {
			m.appendMessages("⚠️ MCP manager not initialized.")
		}

	case "/sessions", "/history":
		if st := m.context.Store(); st != nil {
			// Show ALL sessions (any project) so /history never hides older
			// conversations; each row already shows its project name.
			if list, err := st.ListSessions(); err == nil {
				m.sessionList = list
				m.sessionsSel = 0
				m.sessionsViewport.GotoTop()
				m.showSessions = true
			} else {
				m.appendMessages("❌ Failed to list sessions: " + err.Error())
			}
		} else {
			m.appendMessages("⚠️ Session store not initialized.")
		}

	case "/new":
		cwd, _ := os.Getwd()
		newSessID := fmt.Sprintf("sess_%d", time.Now().Unix())
		st := m.context.Store()
		if st != nil {
			_ = st.CreateSession(newSessID, cwd)
		}
		m.context = bcontext.NewManager(newSessID, st, m.contextWindow())
		m.rebuildEngine()
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
			m.connectCustom = false
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
				m.appendMessages(fmt.Sprintf("✅ Model switched to %s", m.activeModel))
				m.rebuildEngine()
			}
		} else {
			m.appendMessages("Usage: /model <provider>/<model> or /model <model_name>")
		}
	}
	return m, nil
}

// runLSPInstalls executes the install command(s) for missing language servers
// (bounded 5 min each, so a slow package manager cannot hang the UI forever)
// and returns a report for the chat.
func runLSPInstalls(mgr *lsp.Manager, onlyLang string) string {
	hints := mgr.InstallHints()
	if onlyLang != "" {
		if c, ok := hints[onlyLang]; ok {
			hints = map[string]string{onlyLang: c}
		} else {
			return "⚠️ No install needed for " + onlyLang + "."
		}
	}
	var sb strings.Builder
	for lang, cmd := range hints {
		sb.WriteString(fmt.Sprintf("\n⬇️ %s: %s\n", lang, cmd))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		out, err := c.CombinedOutput()
		cancel()
		if err != nil {
			sb.WriteString("❌ " + lang + " install failed: " + err.Error() + "\n" + truncateString(string(out), 500))
		} else {
			sb.WriteString("✅ " + lang + " installed\n")
		}
	}
	sb.WriteString("\n🧠 Available now: ")
	if av := mgr.AvailableServers(); len(av) > 0 {
		sb.WriteString(strings.Join(av, ", "))
	} else {
		sb.WriteString("none")
	}
	return sb.String()
}

// truncateString shortens s to n runes with an ellipsis.
func truncateString(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// lspStatus renders the code intelligence status: which language servers are
// installed and can be used by the lsp_* tools.
func (m *Model) lspStatus() string {
	if m.lspMgr == nil {
		return "ℹ️ LSP not initialized."
	}
	langs := map[string]string{
		"go":         "gopls",
		"typescript": "typescript-language-server",
		"python":     "pyright-langserver",
		"rust":       "rust-analyzer",
		"c":          "clangd",
		"cpp":        "clangd",
	}
	var sb strings.Builder
	sb.WriteString("🧠 LSP code intelligence (lsp_definition, lsp_references, lsp_hover, lsp_diagnostics)\n")
	for lang, bin := range langs {
		_, err := exec.LookPath(bin)
		if err == nil {
			sb.WriteString(fmt.Sprintf("  ✅ %-12s %s\n", lang, bin))
		} else {
			sb.WriteString(fmt.Sprintf("  ❌ %-12s %s (not installed)\n", lang, bin))
		}
	}
	if hints := m.lspMgr.InstallHints(); len(hints) > 0 {
		sb.WriteString("\nRun /lsp-install to auto-install the missing servers, or install manually:")
		for lang, cmd := range hints {
			sb.WriteString(fmt.Sprintf("\n  %-10s %s", lang, cmd))
		}
	}
	sb.WriteString("\nThe model falls back to grep/glob/read_file when a server is missing.")
	if m.globalIndex != nil {
		sb.WriteString(fmt.Sprintf("\n🗺️ code_locate: %d files indexed (persistent per-session symbol + reference graph, no server needed)\n", m.globalIndex.FileCount()))
	}
	return strings.TrimSpace(sb.String())
}

// summarizeMCP returns a compact one-liner of connected MCP servers (names
// only) injected into OpenCode CLI prompts so the model answers MCP questions
// from context instead of exploring config files. Empty when nothing is
// connected.
func summarizeMCP(mgr *mcp.Manager) string {
	if mgr == nil {
		return ""
	}
	names := mgr.ServerNames()
	if len(names) == 0 {
		return ""
	}
	return "Connected MCP servers: " + strings.Join(names, ", ")
}

// mcpStatus renders a readable status of connected MCP servers and tools.
func (m *Model) mcpStatus() string {
	if m.mcpMgr == nil {
		return "⚠️ MCP not initialized."
	}
	names := m.mcpMgr.ServerNames()
	if len(names) == 0 {
		return "ℹ️ No MCP servers configured.\n\nCreate a `.mcp.json` in the project root or `~/.config/brocode/mcp.json` (same format as Claude/Cursor):\n```json\n{\"mcpServers\": {\"my-server\": {\"command\": \"npx\", \"args\": [\"-y\", \"pkg\"]}}}\n```\nThen run `/mcp-reload` to connect."
	}

	errs := m.mcpMgr.Errors()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔌 MCP: %d server(s), %d tool(s)\n", len(names), len(m.mcpMgr.Tools())))
	toolNames := make(map[string][]string)
	for _, t := range m.mcpMgr.Tools() {
		toolNames[t.Server()] = append(toolNames[t.Server()], t.ToolName())
	}
	for _, n := range names {
		if e := errs[n]; e != "" {
			sb.WriteString(fmt.Sprintf("❌ %s — %s\n", n, e))
			continue
		}
		ts := toolNames[n]
		sort.Strings(ts)
		sb.WriteString(fmt.Sprintf("✅ %s — %d tool(s): %s\n", n, len(ts), strings.Join(ts, ", ")))
	}
	return strings.TrimSpace(sb.String())
}

func (m *Model) applySelectedSession() {
	if m.sessionsSel >= 0 && m.sessionsSel < len(m.sessionList) {
		targetSess := m.sessionList[m.sessionsSel]
		st := m.context.Store()
		m.context = bcontext.NewManager(targetSess.ID, st, m.contextWindow())

		// Load past events into context and message log
		m.messages = []string{fmt.Sprintf("✅ Switched to session: %s", targetSess.ID)}
		if st != nil {
			// Purge history duplicated by old resume logic before restoring.
			if removed, err := st.CleanupReplayDuplicates(targetSess.ID); err == nil && removed > 0 {
				m.appendMessages(fmt.Sprintf("⚡ Purged %d duplicated history events", removed))
			}
			if events, err := st.GetSessionEvents(targetSess.ID); err == nil && len(events) > 0 {
				// Same restore path as `brocode -c`: replay only the newest
				// events that fit the context window, keep assistant tool calls
				// paired with their results, and render tool-call-only turns as
				// compact summaries instead of raw JSON.
				m.appendMessages(bcontext.RestoreSession(m.context, events)...)
			}
		}
		// Invalidate the rendered-log cache so the viewport re-renders with the
		// freshly loaded session history instead of showing stale content.
		m.renderedLog = ""
		m.renderedKey = ""
		m.logViewport.SetYOffset(0)
		m.rebuildEngine()
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
		ModelMap:  nil,
	}

	if err := provider.SaveGlobalConfig(m.cfg); err != nil {
		m.appendMessages(fmt.Sprintf("❌ Failed to save config: %v", err))
		return
	}

	m.appendMessages(fmt.Sprintf("✅ API Key for %s saved to ~/.config/brocode/config.json!", targetProvider.Name))

	// Re-detect providers and switch if appropriate
	m.modelOptions = provider.DiscoverModels(m.cfg)
	m.switchProviderAndModel(pID, targetProvider.DefaultModels[0])
}

// connectNext advances the connect wizard one step (or saves on the last).
//
// Flow:
//   - Custom provider: name → API key → base URL → models (multi-step)
//   - Built-in provider: API key only (single step) — base URL/models are
//     not forced, matching the provider's known defaults.
func (m *Model) connectNext() {
	switch m.connectStep {
	case 0:
		if m.connectProviderSel >= len(provider.BuiltinProviders) {
			// Custom provider: full multi-step wizard.
			m.connectCustom = true
			m.connectStep = 1
			m.connectNameInput.SetValue("")
			m.connectNameInput.Focus()
		} else {
			// Built-in provider: only the API key is needed.
			m.connectCustom = false
			p := provider.BuiltinProviders[m.connectProviderSel]
			m.connectTextInput.SetValue("")
			m.connectBaseURLInput.SetValue(p.DefaultBaseURL)
			m.connectStep = 1
			m.connectTextInput.Focus()
		}
	case 1:
		if m.connectCustom {
			m.connectTextInput.SetValue("")
			m.connectStep = 2
			m.connectTextInput.Focus()
		} else {
			// Built-in: single step, save immediately.
			m.applyConnectConfig()
			m.showConnect = false
		}
	case 2:
		m.connectStep = 3
		m.connectBaseURLInput.Focus()
	case 3:
		m.connectModelsInput.SetValue("")
		m.connectStep = 4
		m.connectModelsInput.Focus()
	case 4:
		m.applyConnectConfig()
		m.showConnect = false
	}
}

// connectPrev steps the wizard back one step (or closes it at step 0).
func (m *Model) connectPrev() {
	if m.connectStep == 0 {
		m.showConnect = false
		return
	}
	m.connectStep--
	switch m.connectStep {
	case 1:
		if m.connectCustom {
			m.connectNameInput.Focus()
		} else {
			// Built-in: step 1 (API key) goes back to provider pick.
			m.connectTextInput.Blur()
		}
	case 2:
		m.connectTextInput.Focus()
	case 3:
		m.connectBaseURLInput.Focus()
	case 4:
		m.connectModelsInput.Focus()
	}
}

// applyConnectConfig saves the completed wizard form as a provider config and
// switches the active provider/model to it.
func (m *Model) applyConnectConfig() {
	if m.connectCustom {
		m.saveCustomProvider()
		return
	}
	if m.connectProviderSel < 0 || m.connectProviderSel >= len(provider.BuiltinProviders) {
		return
	}
	p := provider.BuiltinProviders[m.connectProviderSel]
	keyVal := strings.TrimSpace(m.connectTextInput.Value())
	baseURL := strings.TrimSpace(m.connectBaseURLInput.Value())
	if keyVal == "" && (baseURL == "" || baseURL == p.DefaultBaseURL) {
		m.appendMessages("⚠️ Nothing to save — API key is empty.")
		return
	}
	m.saveProviderConfig(p.ID, p, keyVal, baseURL, nil, nil)
}

// saveCustomProvider persists a brand-new custom provider from wizard fields.
func (m *Model) saveCustomProvider() {
	pID := strings.TrimSpace(m.connectNameInput.Value())
	if pID == "" {
		m.appendMessages("❌ Provider name is required.")
		return
	}
	pID = strings.ToLower(strings.ReplaceAll(pID, " ", "-"))

	keyVal := strings.TrimSpace(m.connectTextInput.Value())
	baseURL := strings.TrimSpace(m.connectBaseURLInput.Value())
	if baseURL == "" {
		m.appendMessages("❌ Base URL is required for a custom provider.")
		return
	}

	modelIDs, modelMap, err := provider.ParseModelJSON(m.connectModelsInput.Value())
	if err != nil {
		m.appendMessages("❌ Models JSON invalid: " + err.Error())
		return
	}

	info := provider.ProviderInfo{
		ID:             pID,
		Name:           pID + " (Custom)",
		Protocol:       "openai-compatible",
		DefaultBaseURL: baseURL,
		DefaultModels:  modelIDs,
	}
	m.saveProviderConfig(pID, info, keyVal, baseURL, modelIDs, modelMap)
}

// saveProviderConfig writes a provider into the global config and switches to it.
func (m *Model) saveProviderConfig(pID string, info provider.ProviderInfo, keyVal, baseURL string, modelIDs []string, modelMap map[string]provider.CustomModel) {
	if m.cfg.Providers == nil {
		m.cfg.Providers = make(map[string]provider.CustomProviderConfig)
	}

	m.cfg.Providers[pID] = provider.CustomProviderConfig{
		Protocol: info.Protocol,
		BaseURL:  baseURL,
		APIKey:   keyVal,
		Models:   modelIDs,
		ModelMap: modelMap,
	}

	if err := provider.SaveGlobalConfig(m.cfg); err != nil {
		m.appendMessages(fmt.Sprintf("❌ Failed to save config: %v", err))
		return
	}

	m.modelOptions = provider.DiscoverModels(m.cfg)
	model := "default"
	if len(info.DefaultModels) > 0 {
		model = info.DefaultModels[0]
	}
	m.switchProviderAndModel(pID, model)
	m.appendMessages(fmt.Sprintf("✅ Provider %s saved to ~/.config/brocode/config.json!", pID))
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
			if oa, ok := m.adapter.(*provider.OpenCodeAdapter); ok && m.ask != nil {
				oa.AskUser = m.ask.Ask
			}
			m.rebuildEngine()
			// The session's context window follows the newly selected model's
			// declared limit (e.g. 1M models get a 1M window).
			m.context.SetMaxWindow(m.contextWindow())
			m.appendMessages(fmt.Sprintf("✅ Active model set & saved: %s/%s", pID, modelName))
			return
		}
	}
	m.activeModel = modelName
	m.cfg.DefaultModel = modelName
	_ = provider.SaveGlobalConfig(m.cfg)
	m.appendMessages(fmt.Sprintf("⚠️ Model set & saved to %s", modelName))
}

func (m *Model) applySelectedModel() {
	items := m.getModelList()
	if m.modelsSel >= 0 && m.modelsSel < len(items) {
		selected := items[m.modelsSel]
		m.switchProviderAndModel(selected.ProviderID, selected.ModelName)
	}
}

func (m *Model) View() tea.View {
	var content string
	if m.showAsk {
		content = m.renderAskModal()
	} else if m.showModels {
		content = m.renderModelsModal()
	} else if m.showSessions {
		content = m.renderSessionsModal()
	} else if m.showConnect {
		content = m.renderConnectModal()
	} else if m.showDebug {
		content = m.renderDebugModal()
	} else {
		var sb strings.Builder

		// Message Log — scrolls inside a viewport; the formatted log is cached
		// so markdown only re-renders when a message actually changes.
		contentWidth := m.width - 4
		if contentWidth < 0 {
			contentWidth = 0
		}
		log := m.buildLog(contentWidth)
		if m.width > 0 {
			h := m.updateLogHeight()
			if m.streaming {
				if log != m.renderedLog {
					m.logViewport.SetContent(log)
					m.logViewport.GotoBottom()
					m.renderedLog = log
				}
			} else if key := m.logKey(); key != m.renderedKey || h != m.renderedH {
				m.logViewport.SetContent(log)
				// Park the scroll at the user's own prompt so it never silently
				// disappears from the history after a long answer scrolls past it
				// — or when the viewport shrinks because the live activity slot
				// grew while a turn is running.
				// lastUserLine == 0 is a VALID position (prompt is the first
				// rendered line), so use an explicit found-flag, not "> 0".
				if m.foundUserLine {
					m.parkAtUserPrompt()
				} else {
					m.logViewport.GotoBottom()
				}
				m.renderedLog = log
				m.renderedKey = key
				m.renderedH = h
			}
			sb.WriteString(m.logViewport.View())
		} else {
			sb.WriteString(log)
		}

		// Live agent activity slot: spinner + current step + the last few tool
		// calls, rendered ABOVE the input. Activity is transient — it never
		// enters the conversation history (that's what made process rows pile
		// up above the answer and hide the user's prompt).
		if m.status != "Ready" && m.status != "Failed" {
			spinnerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
			frame := spinnerFrames[m.spinnerIdx%len(spinnerFrames)]
			sb.WriteString(spinnerStyle.Render(frame+" "+m.status) + "\n")
			actStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
			for _, act := range m.activity {
				sb.WriteString(actStyle.Render("  · "+act) + "\n")
			}
			sb.WriteString("\n")
		} else {
			sb.WriteString("\n\n")
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
		tokensStr := fmt.Sprintf("Tokens: %s/%s", provider.FormatTokens(m.context.TotalTokens()), provider.FormatTokens(m.context.MaxWindow()))
		// Live cost estimate in the footer (per-session, from adapter usage).
		if cost := m.engine.SessionCostUSD(); cost > 0 {
			tokensStr += fmt.Sprintf(" · Cost: $%.4f", cost)
		}

		// Live LSP indicator: count of available language servers (binary on
		// PATH) with a colored badge — teal when at least one is usable.
		lspBadge := ""
		if m.lspMgr != nil {
			n := len(m.lspMgr.AvailableServers())
			if n > 0 {
				lspStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
				lspBadge = " | " + lspStyle.Render(fmt.Sprintf("🧠 LSP:%d", n))
			}
		}

		footerBanner := fmt.Sprintf(" BROCODE🔥 | Mode: %s | Provider: %s | Model: %s | Session: %s | %s%s ",
			modeBadgeStyle.Render(m.mode+" (Shift+Tab)"), m.activeProvider.Info.Name, m.activeModel, sessID, tokenStyle.Render(tokensStr), lspBadge)

		sb.WriteString(bannerStyle.Render(footerBanner) + "\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(" ENTER send · Alt+Enter newline · Tab/Shift+Tab mode · ↑/↓ history · PgUp/PgDn scroll · /sessions · /models · /lsp · /help "))
		content = sb.String()
	}

	v := tea.NewView(content)
	return v
}

// buildLog renders the message history + live streaming block. It also
// records the rendered line where the last user message starts, so the view
// can park the scroll position there after a turn completes (the user's own
// prompt must never silently disappear from the history).
func (m *Model) buildLog(contentWidth int) string {
	var sb strings.Builder
	line := 0
	m.lastUserLine = 0
	m.foundUserLine = false
	for _, msg := range m.messages {
		isUser := strings.HasPrefix(msg, "YOU:\n") || strings.HasPrefix(msg, "👤 ")
		if isUser {
			m.lastUserLine = line
			m.foundUserLine = true
		}
		rendered := formatMessage(msg, contentWidth)
		sb.WriteString(rendered + "\n\n")
		line += strings.Count(rendered, "\n") + 3 // rendered lines + blank separator
	}
	if m.streaming && m.pendingStream != "" {
		label := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true).Render("BROCODE")
		bar := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("205")).Padding(0, 1)
		if contentWidth > 0 {
			bar = bar.Width(contentWidth)
		}
		sb.WriteString(bar.Render(label+"\n"+m.pendingStream) + "\n\n")
	}
	return sb.String()
}

// lastAssistantReasoning returns the reasoning text of the most recent
// assistant turn stored in the context manager (used to show thinking).
func (m *Model) lastAssistantReasoning() string {
	msgs := m.context.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && msgs[i].Reasoning != "" {
			return msgs[i].Reasoning
		}
	}
	return ""
}

// logKey is a cheap fingerprint of the message list (count + last message) so
// the cache only invalidates when the history actually changes.
func (m *Model) logKey() string {
	if len(m.messages) == 0 {
		return "0|"
	}
	return fmt.Sprintf("%d|%s", len(m.messages), m.messages[len(m.messages)-1])
}

// updateLogHeight fits the log viewport between the status slot, the (dynamic
// height) input, and the footer so the terminal never scrolls the history.
// Returns the computed height so View() can re-park when it changes.
func (m *Model) updateLogHeight() int {
	taH := m.promptInput.Height()
	if taH < 1 {
		taH = 1
	}
	// Reserve room for the live activity slot (spinner + up to 5 steps) while
	// a turn is running, plus the input, banner and hint.
	actH := 0
	if m.status != "Ready" && m.status != "Failed" && len(m.activity) > 0 {
		actH = len(m.activity)
	}
	h := m.height - taH - 5 - actH
	if h < 3 {
		h = 3
	}
	if h != m.logViewport.Height() {
		m.logViewport.SetHeight(h)
	}
	return h
}

// parkAtUserPrompt sets the log scroll so the user's last prompt stays
// visible. SetYOffset clamps to the viewport's max offset, so when the content
// is short the viewport lands at the bottom (prompt visible); when a long
// answer scrolls past it the offset stays at the prompt line.
func (m *Model) parkAtUserPrompt() {
	if !m.foundUserLine {
		m.logViewport.GotoBottom()
		return
	}
	m.logViewport.SetYOffset(m.lastUserLine)
}

func (m *Model) renderSessionsModal() string {
	// Build the session list inside a scrollable viewport so long histories
	// never overflow the terminal (previously /history silently cut off the
	// older sessions).
	var sb strings.Builder

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

	sb.WriteString("\n[↑/↓ navigate · ENTER switch session · PgUp/PgDn scroll · /new create clean session · ESC close]")

	body := sb.String()
	// NOTE: do not GotoTop here — that would reset the user's manual scroll
	// on every key press. Scroll position is reset when the modal opens.
	m.sessionsViewport.SetContent(body)
	h := m.height - 4
	if h < 6 {
		h = 6
	}
	m.sessionsViewport.SetHeight(h)
	w := m.width - 8
	if w < 10 {
		w = 10
	}
	m.sessionsViewport.SetWidth(w)

	return lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2).Render(m.sessionsViewport.View())
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

			sb.WriteString(fmt.Sprintf("%s %-28s (%s)%s\n", cursor, item.ModelName, provider.FriendlyName(item.ProviderID), statusTag))
		}
	}

	sb.WriteString("\n[↑/↓ navigate · ENTER apply · ESC close]")
	return lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2).Render(sb.String())
}

func (m Model) renderConnectModal() string {
	var sb strings.Builder
	sb.WriteString("=== Connect LLM Provider (/connect) ===\n\n")

	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	stepStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Bold(true)
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	switch m.connectStep {
	case 0:
		sb.WriteString(stepStyle.Render("Step 1 — Select Provider") + "\n\n")
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
		// Custom entry at the end of the list
		cursor := "  "
		if m.connectProviderSel == len(provider.BuiltinProviders) {
			cursor = "❯ "
		}
		sb.WriteString(fmt.Sprintf("%s %-25s\n", cursor, "✨ Custom Provider..."))
		sb.WriteString("\n" + hintStyle.Render("[↑/↓ navigate · ENTER select · ESC cancel]"))

	case 1:
		if m.connectCustom {
			sb.WriteString(stepStyle.Render("Step 2/5 — Custom Provider Name") + "\n\n")
			sb.WriteString(labelStyle.Render("Provider ID (lowercase, no spaces):") + "\n\n")
			sb.WriteString("  " + m.connectNameInput.View() + "\n\n")
			sb.WriteString(hintStyle.Render("[Type provider ID e.g. my-gateway · ENTER next · ESC back]"))
		} else {
			target := "Custom Provider"
			if m.connectProviderSel < len(provider.BuiltinProviders) {
				target = provider.BuiltinProviders[m.connectProviderSel].Name
			}
			sb.WriteString(stepStyle.Render("Step 2/2 — API Key") + "\n\n")
			sb.WriteString(labelStyle.Render("API Key for "+target+":") + "\n\n")
			sb.WriteString("  " + m.connectTextInput.View() + "\n\n")
			sb.WriteString(hintStyle.Render("[Type or paste API Key (Ctrl+V supported) · ENTER save · ESC back]"))
		}

	case 2:
		sb.WriteString(stepStyle.Render("Step 3/5 — API Key") + "\n\n")
		sb.WriteString(labelStyle.Render("API Key (leave empty if none):") + "\n\n")
		sb.WriteString("  " + m.connectTextInput.View() + "\n\n")
		sb.WriteString(hintStyle.Render("[Type or paste API Key (Ctrl+V supported) · ENTER next · ESC back]"))

	case 3:
		sb.WriteString(stepStyle.Render("Step 4/5 — Base URL") + "\n\n")
		sb.WriteString(labelStyle.Render("API Base URL (OpenAI-compatible /v1 endpoint):") + "\n\n")
		sb.WriteString("  " + m.connectBaseURLInput.View() + "\n\n")
		sb.WriteString(hintStyle.Render("[e.g. https://api.my-gateway.example/v1 · ENTER next · ESC back]"))

	case 4:
		sb.WriteString(stepStyle.Render("Step 5/5 — Models (optional)") + "\n\n")
		sb.WriteString(labelStyle.Render("Models JSON (can be more than 1):") + "\n\n")
		sb.WriteString("  " + m.connectModelsInput.View() + "\n\n")
		sb.WriteString(hintStyle.Render("[" +
			"{\"model-a\":{\"name\":\"Model A\",\"limit\":{\"context\":1048576,\"output\":32768}}} " +
			"or [\"model-a\",\"model-b\"]" +
			" · ENTER save · ESC back]"))
	}

	return lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2).Render(sb.String())
}

func (m Model) renderDebugModal() string {
	var sb strings.Builder
	sb.WriteString("=== Active LLM Context (/debug-context) ===\n\n")
	sb.WriteString(fmt.Sprintf("Session ID: %s\nTotal Tokens: %s / %s\nEvents Count: %d\n\n",
		m.context.SessionID(), provider.FormatTokens(m.context.TotalTokens()), provider.FormatTokens(m.context.MaxWindow()), len(m.context.Messages())))

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

// isStatusLine reports whether a trimmed line looks like an OpenCode CLI
// status header (spinner frames, "build · model" banners, prompt prefixes)
// that must be stripped from the answer.
func isStatusLine(trimmed string) bool {
	if trimmed == "" || trimmed == "[0m" || trimmed == "[?25l" || trimmed == "[?25h" {
		return true
	}
	// "build · <model>" banner can appear with various prefixes (>, │, |, •).
	if strings.Contains(trimmed, "build ·") || strings.Contains(trimmed, "build •") || strings.Contains(trimmed, "build·") {
		return true
	}
	// OpenCode spinner frames and status prefixes. Plain ASCII "|" is NOT a
	// status prefix — markdown tables start with it — only the box-drawing
	// variants and spinner glyphs are.
	for _, p := range []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "❯", "→", "├", "│", "┃", "⬢"} {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

func sanitizeLLMOutput(content string) string {
	content = ansiRegex.ReplaceAllString(content, "")

	lines := strings.Split(content, "\n")
	var cleanLines []string
	skippingHeader := true

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if skippingHeader {
			// Skip consecutive status lines until the first real content line.
			if isStatusLine(trimmed) {
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
	userBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("86")).Padding(1, 2)

	botLabelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	botBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("205")).Padding(1, 2)

	thinkingStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Italic(true)

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

		// A "💭 " prefix carries the model's reasoning (thinking). Render it
		// dimmed and italic above the actual answer so the agent's deliberation
		// is visible, like opencode's thinking block.
		body := content
		var thinking string
		if strings.HasPrefix(body, "💭 ") {
			if idx := strings.Index(body, "\n\n"); idx >= 0 {
				thinking = body[:idx]
				body = body[idx+2:]
			}
		}

		// Wrap markdown to the actual content width (border 1 + padding 2×2 are
		// consumed by the box), so lines use the full terminal instead of a
		// hardcoded 90 columns.
		wrap := width - 5
		if wrap < 40 {
			wrap = 90
		}
		formattedBody := renderMarkdown(body, wrap)
		if strings.Contains(formattedBody, "--- ") || strings.Contains(formattedBody, "+++ ") || strings.Contains(formattedBody, "@@ ") {
			formattedBody = formatDiffLines(formattedBody)
		}

		if thinking != "" {
			return botBarStyle.Render(botLabelStyle.Render("BROCODE") + "\n" +
				thinkingStyle.Render(thinking) + "\n\n" + formattedBody)
		}
		return botBarStyle.Render(botLabelStyle.Render("BROCODE") + "\n" + formattedBody)
	}

	if strings.HasPrefix(msg, "ERROR: ") || strings.HasPrefix(msg, "❌ ") {
		return errStyle.Render(msg)
	}

	if width > 0 {
		return lipgloss.NewStyle().Width(width).Render(msg)
	}
	return msg
}
