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
	"golang.org/x/term"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/hooks"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/lsp"
	"github.com/plumpslabs/bro-code/internal/mcp"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/repo"
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
	if wrap < 30 {
		wrap = 30
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
	res = formatTableOutput(res, wrap)

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

// formatTableOutput cleans up Glamour table rendering by clamping horizontal
// divider lines to the terminal width and merging orphaned table cell wraps.
func formatTableOutput(text string, wrap int) string {
	if !strings.Contains(text, "│") && !strings.Contains(text, "┼") && !strings.Contains(text, "─") {
		return text
	}
	lines := strings.Split(text, "\n")
	var cleaned []string
	inTable := false

	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		cleanText := ansiRegex.ReplaceAllString(trimmed, "")
		cleanTrimmed := strings.TrimSpace(cleanText)

		// Detect table state
		if strings.Contains(trimmed, "│") {
			inTable = true
		} else if cleanTrimmed == "" {
			inTable = false
		}

		// Clamp horizontal divider lines to terminal wrap width
		if strings.Contains(trimmed, "─") && (strings.Contains(trimmed, "┼") || strings.Contains(trimmed, "│")) {
			runes := []rune(trimmed)
			if wrap > 10 && len(runes) > wrap {
				trimmed = string(runes[:wrap])
			}
		}

		// Merge orphaned table cell wrap lines (lines inside a table that lack column separators)
		if inTable && !strings.Contains(trimmed, "│") && !strings.Contains(trimmed, "─") && cleanTrimmed != "" {
			if len(cleaned) > 0 {
				cleaned[len(cleaned)-1] = cleaned[len(cleaned)-1] + " " + cleanTrimmed
				continue
			}
		}

		cleaned = append(cleaned, trimmed)
	}
	return strings.Join(cleaned, "\n")
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
	mode           string // "BUILDER", "PLANNER", or "MINER"
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

	// turnRunning is true while a turn is in flight. Prompts sent while a turn
	// runs are queued in pendingQueue and auto-sent when it finishes: one turn
	// at a time, because concurrent RunTurn calls clobber the engine's shared
	// per-turn state and can crash the CLI progress goroutine (nil handler).
	turnRunning  bool
	pendingQueue []string

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
	// renderedHistory/historyKey cache the rendered message history so
	// streaming only rebuilds the cheap streaming box, not the whole log,
	// every frame — that is what makes long unbounded history viable.
	renderedHistory string
	historyKey      string
	// trimNoticeShown records whether the "older messages pruned" notice has
	// already been inserted into the chat log, so a pathological session that
	// hits the safety ceiling announces it once instead of silently dropping
	// history.
	trimNoticeShown bool
	initialized     bool

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

	// sessionsConfirmID guards destructive deletes from the sessions modal:
	// "" = no confirm pending, "ALL" = delete every session, otherwise the
	// session ID to delete. The modal blocks until the user answers y/n.
	sessionsConfirmID string

	// File-action confirm bar (create/delete file): replaces the chat input
	// until the user picks Allow once / Always / Discard. showFileConfirm
	// blocks tool execution (the tool layer waits on fileConfirmBroker).
	fileConfirm     *fileConfirmBroker
	showFileConfirm bool
	fileConfirmID   string
	fileConfirmKind string // "create_file" | "delete_file"
	fileConfirmPath string
	fileConfirmSel  int // 0=Allow once, 1=Always allow, 2=Discard

	// filesExpanded toggles the FILES change summary at the end of the answer
	// between compact per-file rows and the full +/- diff (ctrl+f).
	filesExpanded bool

	// mouseMode toggles between "SELECT" (native mouse text selection) and
	// "SCROLL" (mouse wheel viewport scrolling) via ctrl+m.
	mouseMode string

	// Connect Modal State (multi-step wizard)
	connectStep         int // 0=provider pick, 1=name, 2=API key, 3=base URL, 4=models
	connectProviderSel  int
	connectCustom       bool // true when adding a brand-new custom provider
	connectNameInput    textinput.Model
	connectTextInput    textinput.Model
	connectBaseURLInput textinput.Model
	connectModelsInput  textarea.Model

	// Deterministic project intelligence (MINER layer): repo map cache + usage
	// tracking, both persisted under .brocode/. Wired once per session.
	repoMap *repo.Map
	usage   *repo.Usage

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
	// mode is the engine mode (BUILDER/PLANNER/MINER) the turn ran under,
	// captured at send time so the answer can be stamped with a mode badge
	// even if the user toggles mode while the turn is in flight.
	mode string
}

// maxChatMessages is a SAFETY CEILING, not a display window: the chat log
// keeps every message of the session in memory (the user's history must never
// be pruned from the screen), and rendering stays cheap via the
// renderedHistory cache. Only a pathological session (thousands of entries)
// hits this ceiling, and even then the oldest entries remain in session
// history in SQLite — with a one-time notice instead of silent loss.
const maxChatMessages = 5000

// appendMessages adds messages to the chat log. The history is kept whole;
// only an extreme safety ceiling (maxChatMessages) can trim it, and that is
// announced once so it is never mistaken for a bug.
func (m *Model) appendMessages(msgs ...string) {
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

// startTurn launches a fresh engine turn for userQuery: it records the prompt
// in history, appends it to the chat, resets streaming state, and returns the
// batch that runs the turn. Callers must ensure no turn is already running
// (the queue in handleEnter / turnResultMsg enforces one at a time).
func (m *Model) startTurn(userQuery string) (tea.Model, tea.Cmd) {
	// A fresh user turn resets the file-change recorder: changes from the
	// previous turn were already summarized at its end, and the recorder must
	// never leak stale entries into the next turn's summary.
	tool.ResetChanges()
	m.filesExpanded = false
	m.appendMessages("YOU:\n" + userQuery)
	m.status = "Thinking..."
	// Clear any stale streaming state from a previous interrupted turn.
	m.streaming = false
	m.pendingStream = ""
	m.activity = nil
	m.turnRunning = true

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
		return turnResultMsg{content: res, err: err, mode: m.mode}
	}

	return m, tea.Batch(runTurnCmd, tickCmd())
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
	// Critical file actions (create/delete) use a compact confirm bar that
	// replaces the chat input until the user answers — a different, lighter
	// interaction than the full question modal.
	fbrk := newFileConfirmBroker()
	tools.SetFileActionHandler(fbrk.Confirm)
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
		mouseMode:           "SELECT",
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
		fileConfirm:         fbrk,
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
	m.initialized = true
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
		// summary + native intelligence layer so fallback runs answer context
		// questions and use learned project knowledge too.
		if oa, ok := a.(*provider.OpenCodeAdapter); ok {
			if m.ask != nil {
				oa.AskUser = m.ask.Ask
			}
			oa.MCPStatus = m.mcpSummary
			oa.ProjectCtx = m.intelligenceBlock()
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
	}
	if m.projectCtx == nil {
		// Build the compact project overview once (tree + AGENTS/CLAUDE/README
		// docs) so every turn starts oriented instead of blind-grepping for
		// file locations. Rebuilt on each NewApp, cached for the session.
		cwd, _ := os.Getwd()
		m.projectCtx = search.BuildProjectContext(cwd)
	}
	m.engine.SetProjectContext(m.projectCtx.String())
	// Deterministic project map (entry points, structure, hot files) — built
	// without the LLM, cached by content hash, so the agent starts oriented
	// without spending tokens re-discovering the repo every session.
	cwd, _ := os.Getwd()
	if m.repoMap == nil || m.usage == nil {
		m.usage = repo.NewUsage(cwd)
		m.repoMap = repo.BuildMap(cwd, m.usage)
	}
	m.engine.SetRepoMap(m.repoMap.String())
	// The free-gateway (opencode CLI) loop runs with the gateway's own system
	// prompt, so its model would never see the native intelligence layer
	// (repo map, memory, project overview). Inject it into the CLI prompt so
	// free models benefit from what BroCode has learned too.
	if oa, ok := m.adapter.(*provider.OpenCodeAdapter); ok {
		oa.ProjectCtx = m.intelligenceBlock()
	}
	// Cross-session usage: every file the model touches this turn feeds the
	// hot-file intelligence ("the more BroCode is used, the smarter it gets").
	m.engine.SetUsageRecorder(func(paths []string) {
		m.usage.Record(paths)
		m.usage.Save()
	})
	// Available skills (.agents/skills, .brocode/skills, global) are listed in
	// the system prompt so the model knows what it can load and use — the
	// general, tool-agnostic standard (never .opencode/ config in the repo).
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

// intelligenceBlock renders BroCode's native project knowledge (repo map +
// project overview + memory warm start) as a prompt block for the opencode
// CLI gateway loop, whose own system prompt would otherwise hide all of it.
// Returns "" when no project context exists yet (e.g. before first build).
func (m *Model) intelligenceBlock() string {
	var sb strings.Builder
	if m.repoMap != nil {
		if s := m.repoMap.String(); s != "" {
			sb.WriteString(s + "\n\n")
		}
	}
	if m.projectCtx != nil {
		sb.WriteString(m.projectCtx.String() + "\n\n")
	}
	if m.memStore != nil {
		if ws := m.memStore.WarmStart(); ws != "" {
			sb.WriteString("PROJECT MEMORY (learned in past sessions, use as verified prior knowledge):\n" + ws)
		}
	}
	return strings.TrimSpace(sb.String())
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
		// Snapshot the partial stream BEFORE clearing it: an interrupted turn
		// must leave a trace in history instead of vanishing (the user saw the
		// text appear, so it must not silently disappear — that was a big
		// source of "history tidak stabil" reports).
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
			// Keep whatever the model had already streamed so the conversation
			// stays connected (labeled as partial, never confused with a
			// complete answer).
			if m.interrupted {
				m.interrupted = false
				if partial != "" {
					m.appendMessages("BROCODE:\n💭 (interrupted — partial response)\n\n" + partial)
				}
			} else {
				m.appendMessages("ERROR: " + msg.err.Error())
				m.status = "Failed"
			}
		} else if strings.TrimSpace(msg.content) == "" {
			// The turn finished but the model returned nothing (a weak model
			// stalling into an empty response). Surface it so the UI never
			// looks stuck on "Thinking..." with no entry — and the queue can
			// still drain below.
			m.appendMessages("⚠️ The model returned an empty response — try rephrasing your request or switching models.")
			m.status = "Ready"
		} else if msg.content != "" {
			// Surface the model's reasoning (thinking) above the answer, like
			// opencode, so the agent's deliberation is visible — not just the
			// final text. Falls back to plain content when there is no reasoning.
			display := msg.content
			if r := m.lastAssistantReasoning(); r != "" {
				display = "💭 " + r + "\n\n" + msg.content
			}
			// Stamp the mode the turn ran under ("BROCODE:PLANNER\n...") so
			// every answer shows which engine mode produced it — the mode badge
			// is rendered by formatMessage. Empty mode falls back to the legacy
			// unstamped format (e.g. restored sessions).
			if msg.mode != "" {
				m.appendMessages("BROCODE:" + msg.mode + "\n" + display)
			} else {
				m.appendMessages("BROCODE:\n" + display)
			}
			// When the primary provider failed and a fallback served the turn,
			// say so in the history — otherwise the answer is mistaken for the
			// active provider's (which is exactly the confusion the user saw:
			// groq active, deepseek-v4-flash-free answered).
			if fb := m.engine.LastFallbackModel(); fb != "" {
				// Include WHY the primary failed (duration/queue limit, invalid
				// model, auth error) so the user can act on it — e.g. switch
				// model or restart the FreeBuff session — instead of wondering.
				reason := m.engine.LastFallbackReason()
				msg := fmt.Sprintf("⚠️ Primary provider failed — this answer came from fallback model %s.", fb)
				if reason != "" && len(reason) < 300 {
					msg += "\nReason: " + reason
				}
				m.appendMessages(msg)
			}
			m.status = "Ready"
		}

		// Append the turn's file-change summary (create/edit/delete) right
		// after the answer, so the user sees exactly which files were touched
		// with +/- line markers — collapsed to one row per file, expandable
		// with ctrl+f. Appended even on error/interrupt so partial edits are
		// never silently lost.
		if ch := tool.TakeChanges(); len(ch) > 0 {
			if files := tool.FileChangesMessage(ch); files != "" {
				m.filesExpanded = false
				m.appendMessages(files)
			}
		}

		// One turn at a time: fire the next queued prompt, if any. The queue
		// drains even after an interrupt/error — a queued message was
		// explicitly requested and must not be silently dropped.
		if len(m.pendingQueue) > 0 {
			next := m.pendingQueue[0]
			m.pendingQueue = m.pendingQueue[1:]
			return m.startTurn(next)
		}
		return m, nil

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

	case fileConfirmMsg:
		m.openFileConfirm(msg)
		return m, nil

	case tea.MouseMsg:
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
						// Step 1 is the API key for built-in providers but the
						// provider name for custom ones — mirror the render.
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
		case "ctrl+f":
			// Toggle the FILES change summary at the end of the last answer
			// between compact per-file rows and the full +/- diff.
			if !m.showFileConfirm && !m.showAsk && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug {
				m.filesExpanded = !m.filesExpanded
				m.renderedLog = "" // force re-render of the cached log
				m.renderedKey = ""
			}
		case "ctrl+m":
			// Toggle mouse mode between SELECT (native text selection) and SCROLL (mouse wheel scrolling)
			if m.mouseMode == "SCROLL" {
				m.mouseMode = "SELECT"
				m.appendMessages("🖱️ Mouse Mode: SELECT (Native mouse drag highlight & copy enabled)")
			} else {
				m.mouseMode = "SCROLL"
				m.appendMessages("🖱️ Mouse Mode: SCROLL (Mouse wheel viewport scrolling enabled)")
			}
			return m, nil

		case "ctrl+y":
			// Copy the last assistant response directly to OS clipboard
			lastAns := m.lastAssistantAnswer()
			if lastAns != "" {
				if err := clipboard.WriteAll(lastAns); err == nil {
					m.appendMessages("📋 Copied last assistant response to OS clipboard!")
				}
			}
			return m, nil

		case "ctrl+u":
			m.logViewport.HalfPageUp()
			return m, nil

		case "ctrl+p":
			// Open full conversation log in system pager (`less -R`)
			if !m.showFileConfirm && !m.showAsk && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug {
				contentWidth := m.width - 4
				if contentWidth < 20 {
					contentWidth = 80
				}
				fullLog := m.buildLog(contentWidth)
				cmd := exec.Command("less", "-R")
				cmd.Stdin = strings.NewReader(fullLog)
				cmd.Stdout = os.Stdout
				return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
					return statusUpdateMsg("Ready")
				})
			}
			return m, nil

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
				// Shift+Tab cycles BUILDER → PLANNER → MINER → BUILDER.
				switch m.mode {
				case "BUILDER":
					m.mode = "PLANNER"
				case "PLANNER":
					m.mode = "MINER"
				default:
					m.mode = "BUILDER"
				}
				m.engine.SetMode(m.mode)
				return m, nil
			}

		case "esc":
			// Input-bar file-action confirm: ESC discards (denies) the action.
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
				m.showModels = false
				return m, nil
			}
			if m.showSessions {
				// ESC first cancels a pending delete confirmation, then closes
				// the modal — a stray ESC must never silently skip a confirm.
				if m.sessionsConfirmID != "" {
					m.sessionsConfirmID = ""
				} else {
					m.showSessions = false
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
			// Input-bar file-action confirm: ENTER submits the current choice.
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

			// One turn at a time: a prompt sent while a turn is in flight is
			// queued and auto-sent when the current turn finishes. Concurrent
			// turns would clobber the engine's shared per-turn state and could
			// crash the CLI progress goroutine (nil-handler panic).
			if m.turnRunning {
				m.pendingQueue = append(m.pendingQueue, userQuery)
				m.appendMessages("⏳ Previous turn still running — message queued and will send automatically when it finishes.")
				m.status = "Queued..."
				return m, nil
			}
			return m.startTurn(userQuery)

		case "d", "D":
			// Sessions modal: d = delete the selected session, D = delete all.
			// Outside the modal these must fall through to the prompt input
			// below the switch (plain letters still type normally).
			if !m.showSessions || m.sessionsConfirmID != "" {
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
			// Sessions modal confirmation: y executes the pending delete, n
			// cancels. Outside a pending confirm these fall through to normal
			// typing.
			if !m.showSessions || m.sessionsConfirmID == "" {
				break
			}
			if keyStr == "y" {
				m.confirmDeleteSessions()
			}
			m.sessionsConfirmID = ""
			return m, nil

		case "left", "right", "1", "2", "3":
			// Input-bar file-action confirm: arrows or 1/2/3 move the cursor.
			if m.showFileConfirm {
				switch keyStr {
				case "left", "1":
					m.fileConfirmSel = 0
				case "2":
					m.fileConfirmSel = 1
				case "right", "3":
					m.fileConfirmSel = 2
				}
				return m, nil
			}

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
			// Step 1 is the API key for built-in providers but the provider
			// name for custom ones — route to whichever input is actually
			// focused, otherwise keystrokes vanish (unfocused textinputs
			// drop all input).
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
		m.appendMessages("📖 Commands:\n/sessions, /history - Switch, or manage past sessions (d = delete, D = delete all, with confirm)\n/new - Create a new clean session\n/models - Open interactive model picker\n/model <provider>/<model> - Switch active model\n/connect - Setup API Key & Provider interactively (2-step wizard)\n/mcp - Show connected MCP servers & tools\n/lsp - Show code intelligence status (gopls, tsserver, ...)\n/lsp-install - Auto-install missing language servers\n/diagnose - Scan project for type errors, warnings & deprecated APIs\n/memory - Show cross-session project memory\n/miner - Switch to MINER mode (learn + persist knowledge)\n/cost - Show session token & estimated cost per model\n/debug-context - View active LLM context & session tokens\n/clear - Clear chat screen\n\nModes (Shift+Tab): BUILDER (edit code) → PLANNER (read-only analysis) → MINER (read-only, persists verified knowledge to memory — the more you use BroCode, the smarter it gets)")

	case "/miner":
		// Jump straight into MINER mode so the next prompt is a knowledge
		// mining pass that persists verified facts into project memory.
		m.mode = "MINER"
		m.engine.SetMode(m.mode)
		m.appendMessages("⛏️ MINER mode active — explore the codebase and I'll persist verified knowledge (architecture, build commands, conventions, decisions, gotchas) into project memory. Shift+Tab to switch back to BUILDER.")

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
			cwd, _ := os.Getwd()
			if list, err := st.ListSessionsByProjectPath(cwd); err == nil {
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

// confirmDeleteSessions executes the pending sessions-modal delete (single
// session or ALL, per sessionsConfirmID), recreates the active session row so
// the events FK stays valid, and refreshes the list.
func (m *Model) confirmDeleteSessions() {
	st := m.context.Store()
	if st == nil {
		m.appendMessages("⚠️ Session store not initialized.")
		return
	}

	active := m.context.SessionID()
	cwd, _ := os.Getwd()
	target := m.sessionsConfirmID
	var removed int
	var err error

	if target == "ALL" {
		removed, err = st.DeleteAllSessions()
	} else {
		removed, err = st.DeleteSession(target)
	}
	if err != nil {
		m.appendMessages("❌ Failed to delete session(s): " + err.Error())
		return
	}

	// The current session row may have been deleted (single = the active one,
	// ALL = every row). Recreate it so future events still satisfy the FK and
	// keep persisting — the in-memory conversation continues untouched.
	if target == "ALL" || target == active {
		if err := st.CreateSession(active, cwd); err == nil {
			m.appendMessages(fmt.Sprintf("🗑️ Deleted %d session(s) (%d events). Current session reset — history cleared.", len(m.sessionList), removed))
		} else {
			m.appendMessages(fmt.Sprintf("🗑️ Deleted %d session(s) (%d events).", len(m.sessionList), removed))
		}
	} else {
		m.appendMessages(fmt.Sprintf("🗑️ Deleted session %s (%d events).", target, removed))
	}

	// Refresh the list; clamp the cursor into the new bounds.
	if list, lerr := st.ListSessionsByProjectPath(cwd); lerr == nil {
		m.sessionList = list
		if m.sessionsSel >= len(list) {
			m.sessionsSel = len(list) - 1
		}
		if m.sessionsSel < 0 && len(list) > 0 {
			m.sessionsSel = 0
		}
	}
}

func (m *Model) applySelectedSession() {
	m.sessionsConfirmID = "" // switching is not a delete; drop any pending confirm
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
			// Built-in provider.
			m.connectCustom = false
			p := provider.BuiltinProviders[m.connectProviderSel]
			m.connectBaseURLInput.SetValue(p.DefaultBaseURL)
			if p.APIKeyEnvVar == "" {
				// Keyless provider (BroCode Free Gateway, FreeBuff, Ollama): no
				// API key exists — asking for one would confuse (e.g. FreeBuff
				// authenticates via its CLI token / local proxy). Save straight
				// away with an empty key.
				m.connectTextInput.SetValue("")
				m.applyConnectConfig()
				m.showConnect = false
				m.appendMessages(fmt.Sprintf("✅ %s connected — no API key needed (token auto-loaded).", p.Name))
				return
			}
			// Built-in provider with a real key: only the API key step is needed.
			m.connectTextInput.SetValue("")
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

	// A provider without a declared model list is unusable (falls back to the
	// placeholder "default") — try the gateway's live /models endpoint before
	// saving so the provider actually works on the first turn.
	if len(modelIDs) == 0 && keyVal != "" {
		if fetched, ferr := provider.FetchOpenAIModels(baseURL, keyVal); ferr == nil && len(fetched) > 0 {
			modelIDs = fetched
		}
	}

	info := provider.ProviderInfo{
		ID:             pID,
		Name:           pID + " (Custom)",
		Protocol:       "openai-compatible",
		DefaultBaseURL: baseURL,
		DefaultModels:  modelIDs,
	}
	m.saveProviderConfig(pID, info, keyVal, baseURL, modelIDs, modelMap)
	if len(modelIDs) == 0 {
		m.appendMessages("⚠️ No models found — open /models to pick a model, or re-run /connect and paste the models JSON block.")
	}
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

		// Chrome below the log (activity slot + input + banner + help) is built
		// FIRST and measured, so the log viewport can be sized to exactly fill
		// the remaining terminal height. Hardcoding the chrome line count is
		// what made the view crop history and flicker — measuring the rendered
		// chrome can never drift.
		chrome, chromeLines := m.buildLogChrome()

		// buildLog always ends with "\n\n" (per-message separator), so the log
		// occupies count+1 rows. This decides between the two rendering paths:
		// natural growth when the content fits, a scrollable window when it
		// overflows the terminal.
		logLines := strings.Count(log, "\n") + 1

		if m.width > 0 {
			// The viewport must be sized before it can be rendered (it returns
			// "" when width/height are 0). Ensure it tracks the terminal even
			// before the first WindowSizeMsg lands.
			if m.logViewport.Width() != m.width {
				m.logViewport.SetWidth(m.width)
			}
			h := m.height - chromeLines
			if h < 3 {
				h = 3
			}
			if h != m.logViewport.Height() {
				m.logViewport.SetHeight(h)
			}
			if m.streaming {
				if log != m.renderedLog {
					m.logViewport.SetContent(log)
					m.logViewport.GotoBottom()
					m.renderedLog = log
				}
			} else if key := m.logKey(); key != m.renderedKey || h != m.renderedH {
				m.logViewport.SetContent(log)
				// Land on the newest content: after a turn completes, the answer
				// (and any FILES summary) was appended, so the reader must end up at
				// the END of the answer. Parking at the user's prompt instead left
				// long answers cut off below the fold and made earlier history look
				// like it vanished (it was only scrolled above the parked view).
				if key != m.renderedKey || m.renderedH == 0 {
					m.logViewport.GotoBottom()
				}
				// Height-only change (the live activity slot grew/shrunk): content
				// is identical, so preserve the reading position — the viewport's
				// rendering clamps safely when it shrinks.
				m.renderedLog = log
				m.renderedKey = key
				m.renderedH = h
			}
			// Hybrid log rendering:
			//  - Content FITS in the available space → render it naturally (raw
			//    string, trailing newlines trimmed) so the chat GROWS like a
			//    normal terminal and a short session never shows a giant blank
			//    gap. The viewport pads short content to its full height, which
			//    after `-c` with a short restored history looked like one empty
			//    screen between the chat and the input.
			//  - Content OVERFLOWS → render through the viewport window: it
			//    clips the log to the available height, honours the scroll
			//    position, lands newest content at the bottom (GotoBottom above)
			//    and keeps older history reachable by scrolling up instead of
			//    being silently dropped by the renderer.
			// In both paths the chrome is appended below, so the input/footer
			// stay pinned to the bottom of the terminal and the total height
			// never exceeds it (no flicker, nothing cropped).
			if logLines <= h {
				sb.WriteString(strings.TrimRight(log, "\n"))
			} else {
				sb.WriteString(m.logViewport.View())
			}
		} else {
			sb.WriteString(log)
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

func clipToTerminalBounds(content string, maxH int) string {
	if maxH <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) > maxH {
		lines = lines[:maxH]
	}
	return strings.Join(lines, "\n")
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
		label := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true).Render("BROCODE")
		bar := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("205")).Padding(0, 1)
		w := contentWidth
		if w <= 0 {
			w = getTerminalWidth() - 2
		}
		bar = bar.Width(w)
		out.WriteString(bar.Render(label+"\n"+m.pendingStream) + "\n\n")
	}
	return out.String()
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

// logKey is a cheap fingerprint of the message list (count + last message) so
// the cache only invalidates when the history actually changes.
func (m *Model) logKey() string {
	if len(m.messages) == 0 {
		return "0|"
	}
	return fmt.Sprintf("%d|%s", len(m.messages), m.messages[len(m.messages)-1])
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

	// Input area: while a critical file action (create/delete) awaits
	// approval, the chat input is temporarily replaced by the confirm bar.
	if m.showFileConfirm {
		sb.WriteString(m.renderFileConfirmBar() + "\n\n")
	} else {
		// Input Box (Borderless & Minimalist, multi-line textarea that grows)
		promptStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
		promptStr := "❯ "
		switch m.mode {
		case "PLANNER":
			promptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
			promptStr = "PLAN ❯ "
			m.promptInput.Placeholder = "Planner Mode: Ask for architecture plans, code analysis, or roadmaps..."
		case "MINER":
			promptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("178")).Bold(true)
			promptStr = "MINE ❯ "
			m.promptInput.Placeholder = "Miner Mode: Explore the codebase and persist verified knowledge to project memory..."
		default:
			m.promptInput.Placeholder = "Type a prompt or command (/help, /sessions, /new)..."
		}
		sb.WriteString(promptStyle.Render(promptStr) + m.promptInput.View() + "\n\n")
	}

	// STICKY BOTTOM FOOTER BANNER (Never disappears when history grows long)
	bannerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Padding(0, 1)
	if m.width > 0 {
		bannerStyle = bannerStyle.MaxWidth(m.width)
	}
	tokenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
	modeBadgeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	switch m.mode {
	case "PLANNER":
		modeBadgeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	case "MINER":
		modeBadgeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("178"))
	}

	sessID := m.context.SessionID()
	if len(sessID) > 16 {
		sessID = sessID[:16] + "..."
	}
	tokensStr := fmt.Sprintf("Tokens: %s/%s", provider.FormatTokens(m.context.TotalTokens()), provider.FormatTokens(m.context.MaxWindow()))
	// Live cost estimate in the footer (per-session, from adapter usage).
	if m.engine != nil {
		if cost := m.engine.SessionCostUSD(); cost > 0 {
			tokensStr += fmt.Sprintf(" · Cost: $%.4f", cost)
		}
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

	helpStr := " ENTER send · Alt+Enter newline · Tab mode · ↑/↓ history · PgUp/PgDn scroll · Ctrl+Y copy · Ctrl+M mouse · /help "
	if m.width >= 120 {
		helpStr = " ENTER send · Alt+Enter newline · Tab/Shift+Tab mode · ↑/↓ history · PgUp/PgDn scroll · Ctrl+Y copy · Ctrl+M mouse mode · /sessions · /models · /lsp · /help "
	} else if m.width >= 90 {
		helpStr = " ENTER send · Alt+Enter newline · Tab mode · ↑/↓ history · Ctrl+Y copy · Ctrl+M mouse · /sessions · /models · /help "
	}
	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	if m.width > 0 {
		helpStyle = helpStyle.MaxWidth(m.width)
	}

	sb.WriteString(bannerStyle.Render(footerBanner) + "\n")
	sb.WriteString(helpStyle.Render(helpStr))

	// The chrome is appended AFTER the viewport output (which renders exactly
	// its height and ends WITHOUT a trailing newline), so the extra terminal
	// rows it occupies equal its newline count — not count+1.
	s := sb.String()
	return s, strings.Count(s, "\n")
}

// updateLogHeight sizes the log viewport to exactly fill the terminal below
// the chrome (activity slot + input + banner + help), so the whole view is one
// terminal tall and the renderer never crops history. Returns the computed
// height so View() can re-park when it changes.
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

func (m *Model) renderSessionsModal() string {
	// Build the session list inside a scrollable viewport so long histories
	// never overflow the terminal (previously /history silently cut off the
	// older sessions).
	var sb strings.Builder

	greenBadge := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	activeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)

	if len(m.sessionList) == 0 && m.sessionsConfirmID == "" {
		sb.WriteString("No previous sessions found in SQLite database.\n")
	} else if m.sessionsConfirmID != "" {
		// Destructive action pending: block the list and ask explicitly. The
		// user must answer y (execute) or n/ESC (cancel) before anything else.
		dangerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
		sb.WriteString(dangerStyle.Render("⚠️  CONFIRM DELETION — this cannot be undone\n\n"))
		if m.sessionsConfirmID == "ALL" {
			sb.WriteString(fmt.Sprintf("Delete ALL %d sessions? Every conversation in every project is permanently removed.\n", len(m.sessionList)))
		} else {
			for _, sess := range m.sessionList {
				if sess.ID != m.sessionsConfirmID {
					continue
				}
				dateStr := sess.CreatedAt.Format("2006-01-02 15:04:05")
				sb.WriteString(fmt.Sprintf("Delete session %s (%s)? Its history is permanently removed.\n", sess.ID, dateStr))
				break
			}
		}
		sb.WriteString("\n[y] confirm delete · [n / ESC] cancel")
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

	sb.WriteString("\n[↑/↓ navigate · ENTER switch session · d delete · D delete all · PgUp/PgDn scroll · ESC close]")

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
	style := lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2)
	if m.width > 0 {
		style = style.Width(m.width - 4)
	}
	return style.Render(sb.String())
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

	style := lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2)
	if m.width > 0 {
		style = style.Width(m.width - 4)
	}
	return style.Render(sb.String())
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

var multiNewlineRegex = regexp.MustCompile(`\n{3,}`)

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
	res = multiNewlineRegex.ReplaceAllString(res, "\n\n")
	return strings.TrimSpace(res)
}

func getTerminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 20 {
		return w
	}
	return 120
}

// FormatMessageForTerminal renders a formatted message string for stdout stream printing.
func FormatMessageForTerminal(msg string, width int) string {
	return formatMessage(msg, width, false)
}

func formatMessage(msg string, width int, filesExpanded bool) string {
	if width <= 0 {
		width = getTerminalWidth() - 2
	}

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

	// File-change summary (see tool.FileChangesMessage): compact per-file rows
	// by default, full +/- diff when the user pressed ctrl+f.
	if strings.HasPrefix(msg, "FILES:\n") && strings.Contains(msg, tool.FileChangesSep) {
		compact, diff, _ := strings.Cut(msg, tool.FileChangesSep)
		compact = strings.TrimPrefix(compact, "FILES:\n")
		fileBarStyle := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
		if width > 0 {
			fileBarStyle = fileBarStyle.Width(width)
		}
		labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("178")).Bold(true)
		var body string
		if filesExpanded {
			body = compact + "\n\n" + formatDiffLines(diff)
		} else {
			body = compact
		}
		return fileBarStyle.Render(labelStyle.Render("FILES") + "\n" + body)
	}

	if strings.HasPrefix(msg, "PROCESS:\n") {
		content := strings.TrimPrefix(msg, "PROCESS:\n")
		formatted := formatDiffLines(content)
		return processBarStyle.Render(processLabelStyle.Render(formatted))
	}

	// Assistant messages may be mode-stamped ("BROCODE:PLANNER\n...") so each
	// answer shows which engine mode produced it as a colored badge next to
	// the BROCODE label. Legacy "BROCODE:\n" and "🤖 " forms carry no mode
	// and render without a badge (e.g. sessions restored from disk).
	mode := ""
	var content string
	if strings.HasPrefix(msg, "BROCODE:") {
		rest := strings.TrimPrefix(msg, "BROCODE:")
		if i := strings.Index(rest, "\n"); i >= 0 {
			mode = rest[:i]
			content = rest[i+1:]
		} else {
			content = rest
		}
	} else if strings.HasPrefix(msg, "🤖 ") {
		content = strings.TrimPrefix(msg, "🤖 ")
	} else {
		// Not an assistant message at all — let the caller's other branches
		// (YOU/PROCESS/ERROR/plain) decide.
		content = ""
	}
	if strings.HasPrefix(msg, "BROCODE:") || strings.HasPrefix(msg, "🤖 ") {
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
		wrap := width - 6
		if wrap < 30 {
			wrap = 30
		}
		formattedBody := renderMarkdown(body, wrap)
		if strings.Contains(formattedBody, "--- ") || strings.Contains(formattedBody, "+++ ") || strings.Contains(formattedBody, "@@ ") {
			formattedBody = formatDiffLines(formattedBody)
		}

		// Mode badge: a small colored chip next to the BROCODE label so the
		// user always knows which engine mode produced this answer.
		label := botLabelStyle.Render("BROCODE")
		if mode == "" {
			mode = "BUILDER"
		}
		badge := lipgloss.NewStyle().Bold(true).Padding(0, 1)
		switch mode {
		case "BUILDER":
			badge = badge.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("42"))
		case "PLANNER":
			badge = badge.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("39"))
		case "MINER":
			badge = badge.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("214"))
		default:
			badge = badge.Foreground(lipgloss.Color("241"))
		}
		label += " " + badge.Render(mode)

		if thinking != "" {
			return botBarStyle.Render(label + "\n" +
				thinkingStyle.Render(thinking) + "\n\n" + formattedBody)
		}
		return botBarStyle.Render(label + "\n" + formattedBody)
	}

	if strings.HasPrefix(msg, "ERROR: ") || strings.HasPrefix(msg, "❌ ") {
		return errStyle.Render(msg)
	}

	if width > 0 {
		return lipgloss.NewStyle().Width(width).Render(msg)
	}
	return msg
}
