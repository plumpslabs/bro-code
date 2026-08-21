package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/glamour"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/hooks"
	"github.com/plumpslabs/bro-code/internal/learn"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/lsp"
	"github.com/plumpslabs/bro-code/internal/mcp"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/plan"
	"github.com/plumpslabs/bro-code/internal/prompt"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/repo"
	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/skill"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/subagent"
	"github.com/plumpslabs/bro-code/internal/tokens"
	"github.com/plumpslabs/bro-code/internal/tool"
	"golang.org/x/term"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type spinnerTickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg {
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
			glamour.WithPreservedNewLines(),
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

	// budgetUSD is the per-task cost cap passed via the -budget flag; the
	// engine stops a turn gracefully once estimated spend exceeds it.
	budgetUSD float64

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

	// queueSel is the highlighted index into pendingQueue while queueMode is
	// active (Alt+K): the queued prompts are shown in the activity slot above
	// the input — never in the conversation history — and e/d edit or delete
	// the selected one.
	queueMode bool
	queueSel  int

	// quitting is set when the user quits (ctrl+c) so in-flight turn
	// goroutines stop sending to the (already exiting) program — prevents a
	// blocked Send from leaking the turn goroutine forever.
	quitting bool

	// Spinner animation state
	spinnerIdx int
	// turnStart marks when the current busy phase (turn or slash scan) began,
	// so the activity slot can show an elapsed timer and the user can tell a
	// slow generation apart from a real hang.
	turnStart time.Time

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
	historyVersion  uint64
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

	// Autocomplete state for slash commands and file mentions
	autocomplete AutocompleteState

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
	// modelListCache memoizes the sorted+filtered model list so the models modal
	// doesn't re-fetch and re-sort every provider on every keystroke while open.
	modelListCache      []modelOptionItem
	modelListCacheQuery string

	// Sessions Modal State
	sessionList      []store.Session
	sessionsSel      int
	sessionsViewport viewport.Model

	// sessionsConfirmID guards destructive deletes from the sessions modal:
	// "" = no confirm pending, "ALL" = delete every session, otherwise the
	// session ID to delete. The modal blocks until the user answers y/n.
	sessionsConfirmID string

	// MCP Modal State: /mcp opens an interactive server list (like /models /
	// /sessions). a = add (wizard), d = delete (with y/n confirm), r = reload.
	showMCP    bool
	mcpSel     int
	mcpConfirm string // server name awaiting y/n delete confirm ("" = none)
	// MCP add wizard (mirrors the /connect multi-step wizard): 0 = transport
	// pick, 1 = name, 2 = command (stdio) or URL (http/sse).
	mcpAddActive bool
	mcpAddStep   int // 0=transport, 1=name, 2=command/url
	mcpAddType   int // 0=stdio, 1=http, 2=sse
	mcpAddName   textinput.Model
	mcpAddCmd    textinput.Model
	mcpAddURL    textinput.Model

	// File-action confirm bar (create/delete file): replaces the chat input
	// until the user picks Allow once / Always / Discard. showFileConfirm
	// blocks tool execution (the tool layer waits on fileConfirmBroker).
	fileConfirm     *fileConfirmBroker
	showFileConfirm bool
	fileConfirmID   string
	fileConfirmKind string // "create_file" | "delete_file"
	fileConfirmPath string
	fileConfirmSel  int // 0=Allow once, 1=Always allow, 2=Discard

	// filesExpanded toggles expansion of file-change entries (both the live
	// per-edit DIFF entries and restored FILES: recaps) between a compact
	// (+N −M) row and the full red/green +/- diff. Toggled with ctrl+f.
	filesExpanded bool

	// activityLog, when set (via the --log flag), receives a real-time stream of
	// engine activity (phase transitions + status) so the user can `tail -f` it in
	// another terminal to see what BroCode is doing during a (possibly slow) turn.
	activityLog io.Writer

	// mouseMode toggles between "SELECT" (native mouse text selection) and
	// "SCROLL" (mouse wheel viewport scrolling) via ctrl+m. Defaults to SCROLL
	// so the wheel works out of the box.
	mouseMode string

	// pagerActive is the in-TUI full-answer pager (ctrl+p). While active the
	// log viewport is locked to the last assistant answer and keys scroll it
	// directly; q/Esc/Ctrl+P exit back to the normal chat view.
	pagerActive  bool
	pagerContent string
	pagerWidth   int

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
	askCursor      int // current question index (derived from askFlat)
	askOptionIdx   int // cursor within current question's rows (derived)
	askFlat        int // flat cursor over all option rows + the submit row
	askChecked     map[int]map[int]bool
	askSel         map[int]int // real selections (set on Space/select)
	askCursorPos   map[int]int // per-question cursor memory (navigation)
	askCustom      map[int]string
	askCustomQ     int // question index with custom input open, -1 = none
	askCustomInput textinput.Model
	askViewport    viewport.Model

	// Mode-switch confirmation. Shift+Tab flips the agent mode (BUILDER/
	// PLANNER/MINER), but doing so mid-turn would silently re-tune an in-flight
	// agent. When a turn is running we instead stage the switch and require an
	// explicit confirm (y/Enter = apply, n/Esc = cancel) so it is always a
	// deliberate, acknowledged action.
	showModeConfirm bool
	pendingMode     string
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
// one for the same path. The engine emits a cumulative diff per file, so
// repeated edits grow a single entry in the history instead of one entry per
// edit — while an edit to a NEW file still appends a fresh entry.
func (m *Model) upsertDiffMessage(path, diff string) {
	m.historyVersion++
	prefix := "DIFF:\n" + path + "\n"
	for i := len(m.messages) - 1; i >= 0; i-- {
		if strings.HasPrefix(m.messages[i], prefix) {
			m.messages[i] = prefix + diff
			return
		}
	}
	m.appendMessages(prefix + diff)
}

// appendNote adds a UI/informational message to the chat AND persists it as a
// system_msg event so slash-command output (e.g. /help, /diagnose) survives a
// -c resume instead of disappearing on reload. Transient confirmations (copy,
// mouse mode, interrupts) should keep using appendMessages directly.
func (m *Model) appendNote(text string) {
	m.appendMessages(text)
	if m.context != nil {
		_ = m.context.AppendSystemNote(text)
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
	m.engine.SetMode(m.mode)
	m.appendMessages("YOU:\n" + userQuery)
	m.status = "Thinking..."
	m.turnStart = time.Now()
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
		return turnResultMsg{content: res, err: err, mode: m.mode}
	}

	return m, tea.Batch(runTurnCmd, tickCmd())
}

type statusUpdateMsg string
type stepProgressMsg struct {
	state loop.LoopState
	info  string
}
type streamChunkMsg string
// fileDiffMsg carries a live per-edit unified diff from the engine so the chat can
// show a red/green diff entry as each file is changed (real-time, not just the
// collapsed end-of-turn FILES summary).
type fileDiffMsg struct {
	path string
	diff string
}

// phaseBadge returns a short emoji prefix for an engine phase so the live
// activity slot makes the agent's current state explicit — reasoning vs
// observing vs running a tool — instead of an ambiguous "still processing".
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

// startsWithEmoji reports whether the first rune is a symbol/emoji glyph
// (the message already carries its own visual marker, so skip the badge).
func startsWithEmoji(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r >= 0x2000
}

// diagnoseResultMsg carries the output of an async /diagnose project scan.
type diagnoseResultMsg string

// diagnoseFixMsg carries a finished project scan whose findings should be
// handed straight to the agent to fix (the `/diagnose fix` command).
type diagnoseFixMsg string

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

	// MCP add-wizard inputs (clean, un-focused by default).
	mcpName := textinput.New()
	mcpName.Placeholder = "e.g. github, filesystem, my-tools..."
	mcpName.Prompt = ""
	mcpCmd := textinput.New()
	mcpCmd.Placeholder = "e.g. npx -y @modelcontextprotocol/server-github"
	mcpCmd.Prompt = ""
	mcpURL := textinput.New()
	mcpURL.Placeholder = "e.g. https://mcp.notion.com/mcp"
	mcpURL.Prompt = ""

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
	// The OpenCode adapter is fully standalone (HTTP gateway, no CLI spawn),
	// so its model runs inside BroCode's own engine loop and uses the native
	// ask_user tool and intelligence layer like any other provider — no
	// prompt-injection shims are needed.

	// Fresh session: lead with the compact hero banner (blue-gradient logo +
	// version + hint) as the first log entry. It scrolls up naturally once the
	// user prompts. A resumed session starts from its own history instead —
	// the banner never reappears over an existing conversation.
	msgs := []string{welcomeBanner()}
	if len(initialMsgs) > 0 {
		msgs = initialMsgs
	}

	// Resume the session's last engine mode (persisted on every mode change)
	// so `brocode -c` and /sessions keep working in the mode the user left it
	// in — a PLANNER session resumes in PLANNER, not BUILDER.
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
		// Seed the up/down prompt-history with prompts from a resumed session
		// so ArrowUp recalls previous prompts even before anything is typed
		// this run (see CLI: -c / -continue / -session).
		promptHistory: previousPrompts,
		historyIdx:    len(previousPrompts),
		ask:                 brk,
		fileConfirm:         fbrk,
		askCustomInput:      aci, logViewport: viewport.New(),
		askViewport:      viewport.New(),
		sessionsViewport: viewport.New(),
		mcpSummary:       summarizeMCP(mcpMgr),
		activityLog:      activityLog,
	}

	// Persistent codebase index + checkpoint tool: built/registered once per
	// session on the shared registry (engine rebuilds reuse both).
	cwd, _ := os.Getwd()
	m.globalIndex = search.BuildGlobalIndex(cwd)
	m.tools.Register(&tool.CodeLocateTool{Index: m.globalIndex})
	m.tools.Register(&tool.CheckpointTool{})
	m.tools.Register(&tool.RunTestsTool{Plan: loop.TestCommandPlan})
	if m.scoutMgr != nil && m.scoutMgr.Runner != nil {
		m.tools.Register(&subagent.SwarmTool{Runner: m.scoutMgr.Runner})
	}

	// A restored session already carries FILES: change summaries — show them
	// expanded so the user immediately sees what was edited/created/deleted
	// without pressing ctrl+f.
	for _, msg := range msgs {
		if strings.HasPrefix(msg, "FILES:\n") {
			m.filesExpanded = true
			break
		}
	}

	m.modelOptions = provider.DiscoverModels(cfg)
	m.modelListCache = nil
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

// swarmCheapModel returns the model used for mechanical swarm roles (BUILDER /
// AUDITOR). Resolution: BROCODE_SWARM_CHEAP_MODEL env → BROCODE_COMPACT_MODEL
// env (same "cheap work" tier) → "" (every role uses the active model). Empty
// means no routing; the swarm falls back to a single model.
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
		// Fallback opencode adapters are standalone HTTP gateways, so they need
		// no CLI-specific shims — BroCode's engine drives them natively.
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
		// Parallel orchestration confirm-gate: mutating sub-agents route their
		// approval question through the same user-ask channel.
		if m.scoutMgr != nil && m.scoutMgr.Runner != nil {
			m.scoutMgr.Runner.Adapter = m.adapter
			m.scoutMgr.Runner.Model = m.activeModel
			m.scoutMgr.Runner.ContextWindow = m.contextWindow()
			m.scoutMgr.Runner.CheapModel = m.swarmCheapModel()
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
	// Smart scope pre-selection: pass the full file list so the engine can
	// rank files by relevance to the user's prompt and focus exploration.
	m.engine.SetScopeFiles(repo.ListProjectFiles(cwd))
	// Detected stack (go/node/ts/...) with evidence files biases the skill
	// catalog toward the repo and renders a one-line STACK hint ("STACK: go
	// (go.mod, main.go)") in the system prompt.
	stackHints := make([]prompt.Stack, 0, len(m.repoMap.Stacks))
	for _, s := range m.repoMap.Stacks {
		stackHints = append(stackHints, prompt.Stack{Name: s.Name, Files: s.Files})
	}
	m.engine.SetDetectedStacks(stackHints)
	// The free-gateway (opencode CLI) loop runs with the gateway's own system
	// prompt, so its model would never see the native intelligence layer
	// (repo map, memory, project overview). Inject it into the CLI prompt so
	// free models benefit from what BroCode has learned too.
	// Cross-session usage: every file the model touches this turn feeds the
	// hot-file intelligence ("the more BroCode is used, the smarter it gets").
	m.engine.SetUsageRecorder(func(paths []string) {
		m.usage.Record(paths)
		m.usage.Save()
	})
	// Keep the session symbol index fresh after edits so code_locate answers
	// stay current instead of reflecting only session-start state.
	m.engine.SetOnFileEdited(func(path string) {
		if m.globalIndex != nil {
			m.globalIndex.RefreshFile(path)
		}
	})
	// Live red/green diff per edit: when a write/edit tool succeeds, surface the
	// unified diff as a chat entry in real time so the user sees what changed
	// (not just the collapsed end-of-turn FILES summary).
	m.engine.SetOnChange(func(path, diff string) {
		if m.prog != nil {
			m.prog.Send(fileDiffMsg{path: path, diff: diff})
		}
	})
	// Auto-install the embedded default skill pack (.brocode/skills), then
	// advertise the full catalog (name + description only) so the engine's
	// prompt builder can relevance-filter it as the catalog grows. Skills are
	// the general, tool-agnostic standard (never .opencode/ config in the repo).
	skill.EnsureDefaultsInstalled(cwd)
	m.engine.SetSkillCatalog(skillEntries(cwd))
	// Runtime tuning surface (~/.config/brocode/tuning.json): block/rule
	// toggles + skill-catalog budgets for the system prompt. Missing/corrupt
	// file falls back to defaults — tuning never breaks a run.
	m.engine.SetTuning(prompt.LoadTuning(prompt.DefaultTuningPath()))
	// Cross-session project memory: built once, then wired into the engine
	// (warm start + auto-extract on compaction) and the memory tool.
	if m.memStore == nil {
		m.memStore = memory.NewStore(cwd)
	}
	m.engine.SetMemoryStore(m.memStore)
	m.tools.SetMemoryStore(m.memStore)
	// Smart Context Graph: wire the same SQLite store used by the context
	// manager into both the engine (warm-start hints) and tool registry
	// (async knowledge updates on read, sync invalidation on edit). Reuses the
	// existing session DB — no new files or separate DB handle needed.
	if st := m.context.Store(); st != nil {
		m.engine.SetKnowledgeStore(st)
		m.tools.SetKnowledgeStore(st)
	}
	// Semantic search: wire an OpenAI-compatible embeddings endpoint when the
	// active provider has one, so search_code re-ranks BM25 hits by vector
	// similarity. Falls back to BM25-only on nil / bad keys / errors.
	m.tools.SetSearchEmbedder(embedderFor(m.activeProvider))
	// Hybrid memory retrieval: the same embedder re-ranks BM25 memory facts by
	// semantic similarity for the warm-start excerpt. Nil keeps BM25-only.
	m.memStore.SetEmbedder(embedderFor(m.activeProvider))
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
		// Global symbols provider for DRY reuse checking
		m.engine.SetSymbolsProvider(func() map[string]map[string]bool {
			if m.globalIndex != nil {
				return m.globalIndex.AllSymbols()
			}
			return nil
		})
		// Tell the engine how many LSP servers are reachable so its system
		// prompt can steer the model to lsp_scan (and away from go install).
		if m.lspMgr != nil {
			m.engine.SetLSPStatus(len(m.lspMgr.AvailableServers()))
		}
	for _, fb := range m.buildFallbacks() {
		m.engine.AddFallback(fb)
	}
	// Adaptive routing identity + policy: health tracking keys off the active
	// provider ID, cross-vendor confirmation compares protocols, and the
	// routing policy comes from config (auto / confirm / primary_only).
	m.engine.SetPrimaryIdentity(m.activeProvider.Info.ID, m.activeProvider.Info.Protocol)
	m.engine.SetFallbackPolicy(m.cfg.FallbackPolicy)
	// User-defined lifecycle hooks (.brocode/hooks.json) fire at turn
	// start/end/error and around tool calls. Loaded lazily on first engine
	// build; engine is rebuilt on model switches but hooks are cheap to reload.
	m.engine.SetHooks(hooks.Load(cwd))
	m.engine.SetScoutManager(m.scoutMgr)
	// Model routing (P3): route frequent low-stakes compaction summarization to a
	// cheaper model so the premium model is reserved for synthesis. Opt-in via
	// env; empty = same model.
	if cm := os.Getenv("BROCODE_COMPACT_MODEL"); cm != "" {
		m.engine.SetCompactModel(cm)
	}
	// Tool-description lean (P5): trim verbose tool schemas to free window space.
	// Opt-in via env BROCODE_TOOL_DESC_BUDGET (chars); 0/empty = full descriptions.
	if bd := os.Getenv("BROCODE_TOOL_DESC_BUDGET"); bd != "" {
		if n, err := strconv.Atoi(bd); err == nil {
			m.engine.SetToolDescBudget(n)
		}
	}
	// Self-improving control layer (B1): observe per-turn context utilization and
	// tune the compaction trigger across sessions, persisted under
	// ~/.config/brocode/learn.json so every future session starts warm.
	if lp := learn.DefaultPath(); lp != "" {
		m.engine.SetLearner(learn.NewLearner(lp))
	}
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

// skillEntries converts the installed skills (embedded defaults installed into
// .brocode/skills, plus user/project .agents/skills and the global
// ~/.config/brocode/skills) into catalog entries for the engine's prompt
// builder. Only name + description enter the prompt (progressive disclosure
// level 1); the model loads each SKILL.md itself via read_file when relevant.
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

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.promptInput.Focus(), tickCmd())
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
		// While any modal is open the content is static — keep ticker alive without advancing frames.
		if m.showAsk || m.showModels || m.showConnect || m.showDebug || m.showSessions || m.showMCP {
			return m, tickCmd()
		}
		// Keep the ticker ALIVE for the whole session.
		if m.turnRunning || (m.status != "Ready" && m.status != "Failed") {
			m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		}
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
		}
		// When transitioning to tool execution (StateActing), commit the streamed
		// assistant text for this iteration as its own distinct block with vertical line border.
		if msg.state == loop.StateActing && m.streaming && strings.TrimSpace(m.pendingStream) != "" {
			stamp := "BROCODE:" + m.mode + ":" + m.activeModel + "\n" + strings.TrimSpace(m.pendingStream)
			m.appendMessages(stamp)
			m.pendingStream = ""
			m.streaming = false
		}
		// When tools execute, record a live process block in the chat stream for read/search tools.
		// File mutations (edit_file/write_file/delete_file) are handled by upsertDiffMessage (DIFF/CREATE/DELETE bars).
		if msg.state == loop.StateActing && (strings.HasPrefix(msg.info, "📖 ") || strings.HasPrefix(msg.info, "🔧 ") || strings.HasPrefix(msg.info, "⚙️ ") || strings.HasPrefix(msg.info, "📡 ") || strings.HasPrefix(msg.info, "🧪 ")) {
			procMsg := "PROCESS:\n" + strings.TrimSpace(msg.info)
			if len(m.messages) == 0 || m.messages[len(m.messages)-1] != procMsg {
				m.appendMessages(procMsg)
			}
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
		// /resume or audit can reconstruct what a turn touched. The live per-edit
		// DIFF entries (engine onChange → fileDiffMsg) already streamed into the
		// chat during the turn, so we deliberately do NOT append a batched
		// end-of-turn FILES summary — that would suddenly flood the chat once the
		// agent finishes (noisy, non-predictable). Restored sessions still get a
		// recap from this event (rendered by the FILES: branch in formatMessage).
		if ch := tool.TakeChanges(); len(ch) > 0 {
			if st := m.context.Store(); st != nil {
				if payload, err := json.Marshal(ch); err == nil {
					_, _ = st.AppendEvent(m.context.SessionID(), "file_changes", string(payload), 0)
				}
			}
		}

		// One turn at a time: fire the next queued prompt, if any. The queue
		// drains even after an interrupt/error — a queued message was
		// explicitly requested and must not be silently dropped.
		if len(m.pendingQueue) > 0 {
			next := m.pendingQueue[0]
			m.pendingQueue = m.pendingQueue[1:]
			// The first item just ran; shift the queue selection onto the new
			// head (or clamp) so Alt+K management stays on a valid row.
			if m.queueSel > 0 {
				m.queueSel--
			}
			if len(m.pendingQueue) == 0 {
				m.queueMode = false
				m.queueSel = 0
			}
			return m.startTurn(next)
		}
		// Queue fully drained — leave queue management mode if the last item
		// was deleted by the user rather than drained.
		if m.queueMode {
			m.queueMode = false
			m.queueSel = 0
		}
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
		m.logViewport.GotoBottom()
		return m, nil

	case fileDiffMsg:
		// Live per-edit red/green diff entry in the chat. Kept compact
		// (file path + collapsed-by-default diff lines) and rendered by
		// formatMessage, which colorizes the +/- lines. Upserted per path:
		// the engine sends a CUMULATIVE diff, so the file's previous entry
		// is replaced and one file keeps one growing entry instead of a
		// flood of per-edit duplicates in the history.
		m.upsertDiffMessage(msg.path, msg.diff)
		return m, nil

	case diagnoseResultMsg:
		m.appendNote(string(msg))
		m.status = "Ready"

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
		// Hand the LSP findings straight to the agent: fix every safe issue
		// (lsp_fix for quick-fixes, edit_file for deprecated/unused, lsp_rename
		// for renames) without asking, then verify. Kept in English like the
		// rest of the engine's auto-prompts for consistency and best LLM behavior.
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
					return m, nil			} else if m.showConnect {
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
			} else if m.showMCP && m.mcpAddActive {
				// Paste into the MCP add-wizard input for the current step.
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
					// Keep newlines: the prompt input is multi-line now.
					m.promptInput.InsertString(cleanClip)
					return m, nil
				}
			}
		}

		// In-TUI pager mode: every key scrolls the last answer directly instead
		// of reaching the input/history handlers. q/Esc/Ctrl+P exit.
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

		// Queue management mode (Alt+K): while active, keys manage the queued
		// prompts instead of typing into the input. ↑/↓ move the selection,
		// e loads the selected prompt into the input for editing (removing it
		// from the queue), d deletes it, Esc/Alt+K exit.
		if m.queueMode {
			if len(m.pendingQueue) == 0 {
				m.queueMode = false
				m.queueSel = 0
			} else {
				switch keyStr {
				case "esc", "alt+k":
					m.queueMode = false
					return m, nil
				case "enter":
					// Swallow Enter while managing the queue so the input can't
					// accidentally send mid-management; Esc/Alt+K exit instead.
					return m, nil
				case "up":
					if m.queueSel > 0 {
						m.queueSel--
					}
					return m, nil
				case "down":
					if m.queueSel < len(m.pendingQueue)-1 {
						m.queueSel++
					}
					return m, nil
				case "e":
					// Edit the selected queued prompt: load it into the input
					// (replacing whatever was typed) and drop it from the queue
					// so it can't auto-send mid-edit. The user presses Enter
					// when done — it re-queues or runs then.
					if m.queueSel >= 0 && m.queueSel < len(m.pendingQueue) {
						m.promptInput.SetValue(m.pendingQueue[m.queueSel])
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

		// Mode-switch confirmation: while confirm is pending, only y/Enter
		// (apply) or n/Esc (cancel) are handled; everything else is ignored.
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
			// Toggle the FILES change summary at the end of the last answer
			// between compact per-file rows and the full +/- diff.
			if !m.showFileConfirm && !m.showAsk && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug && !m.showMCP {
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
			// In-TUI pager for the last assistant answer (no subprocess): the
			// viewport locks to the answer, keys scroll it, q/Esc/Ctrl+P exit.
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

		case "alt+k":
			// Toggle queue management mode (see the intercept above for the
			// keys it handles). Only useful while prompts are queued.
			if !m.showFileConfirm && !m.showAsk && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug && !m.showMCP && len(m.pendingQueue) > 0 {
				m.queueMode = !m.queueMode
				if m.queueMode && m.queueSel >= len(m.pendingQueue) {
					m.queueSel = len(m.pendingQueue) - 1
				}
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
			// Mode switching is Shift+Tab ONLY. A bare Tab must never flip the
			// mode (it's reserved for in-modal navigation), so it is a no-op
			// here — and the key is still consumed so it can't bubble elsewhere.
			if keyStr == "shift+tab" && !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP {
				// While a turn is actively running, ignore mode switching so it never
				// disrupts the in-flight agent execution.
				if m.turnRunning {
					return m, nil
				}
				next := m.mode
				switch m.mode {
				case "BUILDER":
					next = "PLANNER"
				case "PLANNER":
					next = "MINER"
				default:
					next = "BUILDER"
				}
				m.mode = next
				m.engine.SetMode(m.mode)
				m.persistMode()
			}
			return m, nil

		case "esc":
			if m.autocomplete.Active {
				m.autocomplete = AutocompleteState{}
				return m, nil
			}
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
			if m.showMCP {
				// ESC cancels the add wizard first, then a pending delete
				// confirm, then closes the modal.
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

			// Interrupt active running turn if user presses ESC
			if m.status != "Ready" && m.status != "Failed" {
				if m.cancelTurn != nil {
					m.cancelTurn()
					m.cancelTurn = nil
				}
				// Background scouts inherit the turn context, so cancelling the
				// turn above already aborts their goroutines; CancelAll is the
				// explicit backstop for scouts whose context outlived the turn.
				if m.scoutMgr != nil {
					m.scoutMgr.CancelAll()
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

			if m.showMCP {
				if m.mcpAddActive {
					m.mcpAddNext()
				} else if m.mcpConfirm != "" {
					m.mcpConfirm = "" // ENTER clears a pending delete confirm
				} else {
					// ENTER on a server: show its tools in the chat.
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
				// The queue is rendered live in the activity slot above the
				// input (see buildLogChrome) — never as a history row, so a
				// queued prompt can't pollute the conversation below the first
				// user prompt.
				m.queueSel = len(m.pendingQueue) - 1
				m.status = "Queued..."
				return m, nil
			}
			return m.startTurn(userQuery)

		case "d", "D":
			// MCP modal: d = delete the selected server (armed, then y/n).
			if m.showMCP && !m.mcpAddActive && m.mcpConfirm == "" {
				if keyStr == "d" {
					names := m.mcpNames()
					if m.mcpSel >= 0 && m.mcpSel < len(names) {
						m.mcpConfirm = names[m.mcpSel]
					}
				}
				return m, nil
			}
			// Sessions modal: d = delete the selected session, D = delete all.
			// Outside the modal these must fall through to the prompt input
			// below the switch (plain letters still type normally).
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
			// Modal confirmations: sessions y/n deletes, MCP y/n deletes a
			// server. Outside a pending confirm these fall through to typing.
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
			// MCP modal: a = start the add-server wizard, r = reload config.
			// Ignored while the wizard is active (those keys type into the
			// form) or outside the modal.
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
			if m.showModels && m.modelsSel > 0 {
				m.modelsSel--
				return m, nil
			}
			if m.showSessions && m.sessionsSel > 0 {
				m.sessionsSel--
				return m, nil
			}
			if m.showMCP && m.mcpAddActive && m.mcpAddStep == 0 && m.mcpAddType > 0 {
				m.mcpAddType--
				return m, nil
			}
			if m.showMCP && !m.mcpAddActive && m.mcpSel > 0 {
				m.mcpSel--
				return m, nil
			}
			if m.showConnect && m.connectStep == 0 && m.connectProviderSel > 0 {
				m.connectProviderSel--
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
			if m.showMCP && m.mcpAddActive && m.mcpAddStep == 0 && m.mcpAddType < 2 {
				m.mcpAddType++
				return m, nil
			}
			if m.showMCP && !m.mcpAddActive {
				names := m.mcpNames()
				if m.mcpSel < len(names)-1 {
					m.mcpSel++
				}
				return m, nil
			}
			if m.showConnect && m.connectStep == 0 && m.connectProviderSel < len(provider.BuiltinProviders) {
				m.connectProviderSel++
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
	} else if m.showMCP && m.mcpAddActive {
		// MCP add wizard: route keys to whichever input the current step uses.
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
	} else if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk {
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		cmds = append(cmds, cmd)
		m.autocomplete = DetectAutocomplete(m.promptInput.Value(), m.allProjectFiles(), m.autocomplete)
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) handleSlashCommand(cmd string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(cmd)
	switch parts[0] {
	case "/help":
		m.appendNote("📖 Commands:\n/sessions, /history - Switch, or manage past sessions (d = delete, D = delete all, with confirm)\n/new - Create a new clean session\n/undo - Time-Travel Rollback: Revert all file changes made in the last turn\n/models - Open interactive model picker\n/model <provider>/<model> - Switch active model\n/connect - Setup API Key & Provider interactively (2-step wizard)\n/mcp - Show connected MCP servers & tools\n/lsp - Show code intelligence status (gopls, tsserver, ...)\n/lsp-install - Auto-install missing language servers\n/diagnose - Scan project for type errors, warnings & deprecated APIs\n/diagnose fix - Scan, then auto-fix all safe warnings/errors via the agent\n/memory - Show cross-session project memory\n/miner - Switch to MINER mode (learn + persist knowledge)\n/cost - Show session token & estimated cost per model\n/debug-context - View active LLM context & session tokens\n/clear - Clear chat screen\n\nModes (Shift+Tab): BUILDER (edit code) → PLANNER (read-only analysis) → MINER (read-only, persists verified knowledge to memory — the more you use BroCode, the smarter it gets)")

	case "/miner":
		// Jump straight into MINER mode so the next prompt is a knowledge
		// mining pass that persists verified facts into project memory.
		m.mode = "MINER"
		m.engine.SetMode(m.mode)
		m.persistMode()
		m.appendNote("⛏️ MINER mode active — explore the codebase and I'll persist verified knowledge (architecture, build commands, conventions, decisions, gotchas) into project memory. Shift+Tab to switch back to BUILDER.")

	case "/builder":
		m.mode = "BUILDER"
		m.engine.SetMode(m.mode)
		m.persistMode()
		m.appendNote("🔨 BUILDER mode active — autonomous coding agent with full read, write, edit, and execution capabilities.")

	case "/planner":
		m.mode = "PLANNER"
		m.engine.SetMode(m.mode)
		m.persistMode()
		m.appendNote("📋 PLANNER mode active — read-only architecture and strategy agent.")

	case "/mode":
		if len(parts) > 1 {
			target := strings.ToUpper(strings.TrimSpace(parts[1]))
			if target == "BUILDER" || target == "PLANNER" || target == "MINER" {
				m.mode = target
				m.engine.SetMode(m.mode)
				m.persistMode()
				m.appendNote(fmt.Sprintf("✅ Mode switched to %s", m.mode))
				return m, nil
			}
		}
		m.appendMessages("Usage: /mode <builder|planner|miner> (or toggle with Shift+Tab)")

	case "/plan":
		cwd, _ := os.Getwd()
		if len(parts) > 1 && (parts[1] == "archive" || parts[1] == "clear" || parts[1] == "reset") {
			archPath, err := plan.ArchiveCurrentPlan(cwd)
			if err != nil {
				m.appendMessages(fmt.Sprintf("⚠️ Failed to archive plan: %v (no active plan in .brocode/current_plan.md)", err))
			} else {
				m.appendNote(fmt.Sprintf("📦 Plan archived to %s", archPath))
			}
			return m, nil
		}
		curPlan, err := plan.LoadCurrentPlan(cwd)
		if err != nil || curPlan == nil || len(curPlan.Steps) == 0 {
			m.appendMessages("ℹ️ No active plan found in `.brocode/current_plan.md`.\nSwitch to PLANNER mode (Shift+Tab or `/planner`) to draft an execution plan.")
		} else {
			m.appendMessages(plan.RenderMarkdownPlan(curPlan) + "\n💡 Run `/plan archive` when finished to archive and clear this plan.")
		}

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
		fixMode := len(parts) > 1 && strings.TrimSpace(parts[1]) == "fix"
		cwd, _ := os.Getwd()
		m.status = "Scanning project diagnostics..."
		m.turnStart = time.Now()
		lsp := m.lspMgr
		return m, tea.Batch(tickCmd(), func() tea.Msg {
			out, err := lsp.ScanDiagnostics(context.Background(), cwd)
			if err != nil {
				return diagnoseResultMsg("❌ Diagnose failed: " + err.Error())
			}
			if fixMode {
				return diagnoseFixMsg(out)
			}
			out += "\n\n💡 Type `/diagnose fix` for BroCode to automatically fix all warnings/errors above."
			return diagnoseResultMsg(out)
		})

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
		m.appendNote(sb.String())
		m.status = "Installing language servers..."
		m.turnStart = time.Now()
		lsp := m.lspMgr
		return m, tea.Batch(tickCmd(), func() tea.Msg {
			return diagnoseResultMsg(runLSPInstalls(lsp, lang))
		})

	case "/mcp":
		// Interactive MCP manager modal: server list with connect status,
		// empty state, add wizard (a) and delete with confirm (d).
		m.showMCP = true
		m.mcpSel = 0
		m.mcpConfirm = ""
		m.mcpAddActive = false

	case "/mcp-reload":
		m.reloadMCP()

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

	case "/workspace", "/repos":
		cwd, _ := os.Getwd()
		ws := repo.DiscoverWorkspace(cwd)
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("📦 **Multi-Repo Workspace: %s**\n\n", ws.RootPath))
		if len(ws.Repos) == 0 {
			sb.WriteString("No repositories detected in workspace.\n")
		} else {
			sb.WriteString(fmt.Sprintf("Found %d repository/repositories in workspace:\n", len(ws.Repos)))
			for i, r := range ws.Repos {
				gitBadge := "git"
				if !r.IsGit {
					gitBadge = "non-git"
				}
				sb.WriteString(fmt.Sprintf("%d. **%s** `[%s]` — %s\n", i+1, r.Name, gitBadge, r.Path))
			}
		}
		sb.WriteString("\n*Tips:* Delegated subagents and tools can target specific repos using `target_dir: \"<repo_name>\"`.")
		m.appendNote(sb.String())

	case "/undo":
		count := tool.RestoreAllSnapshots()
		if count > 0 {
			m.appendMessages(fmt.Sprintf("↩️ Time-Travel Rollback: Successfully restored %d file(s) back to pre-turn snapshot.", count))
		} else {
			m.appendMessages("⚠️ No live snapshots available to roll back.")
		}

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

// reloadMCP restarts the MCP manager from disk config and re-registers its
// tools (shared by /mcp-reload, the modal's r key, and the add/delete flows).
func (m *Model) reloadMCP() {
	if m.mcpMgr == nil {
		m.appendMessages("⚠️ MCP manager not initialized.")
		return
	}
	m.mcpMgr.Close()
	m.mcpMgr.LoadDefaults()
	m.mcpMgr.Start(context.Background())
	for _, mt := range m.mcpMgr.Tools() {
		m.tools.Register(mt)
	}
	m.rebuildEngine()
	m.appendNote(m.mcpStatus())
}

// mcpNames returns the sorted configured server names (empty when nil).
func (m *Model) mcpNames() []string {
	if m.mcpMgr == nil {
		return nil
	}
	return m.mcpMgr.ServerNames()
}

// mcpAddNext advances the add wizard; the final step saves the server to
// .mcp.json and reloads.
func (m *Model) mcpAddNext() {
	switch m.mcpAddStep {
	case 0: // transport picked → name
		m.mcpAddStep = 1
		m.mcpAddName.Focus()
	case 1: // name → command (stdio) or URL (http/sse)
		if strings.TrimSpace(m.mcpAddName.Value()) == "" {
			return // name required — stay on the step
		}
		m.mcpAddStep = 2
		if m.mcpAddType == 0 {
			m.mcpAddCmd.Focus()
		} else {
			m.mcpAddURL.Focus()
		}
	case 2: // save
		m.mcpAddName.Blur()
		m.mcpAddCmd.Blur()
		m.mcpAddURL.Blur()
		m.saveMCPAdd()
		m.mcpAddActive = false
		m.mcpAddStep = 0
	}
}

// mcpAddPrev steps the wizard back (or cancels it at the transport step).
func (m *Model) mcpAddPrev() {
	if m.mcpAddStep == 0 {
		m.mcpAddName.Blur()
		m.mcpAddCmd.Blur()
		m.mcpAddURL.Blur()
		m.mcpAddActive = false
		return
	}
	m.mcpAddStep--
	switch m.mcpAddStep {
	case 0:
		m.mcpAddName.Blur()
	case 1:
		m.mcpAddName.Focus()
	}
}

// saveMCPAdd writes the completed wizard form into the project .mcp.json
// (the standard cross-tool convention) and reloads the manager.
func (m *Model) saveMCPAdd() {
	name := strings.TrimSpace(m.mcpAddName.Value())
	if name == "" {
		m.appendMessages("⚠️ MCP server name is required.")
		return
	}
	var cfg mcp.ServerConfig
	switch m.mcpAddType {
	case 1: // http
		cfg = mcp.ServerConfig{Type: "http", URL: strings.TrimSpace(m.mcpAddURL.Value())}
	case 2: // sse
		cfg = mcp.ServerConfig{Type: "sse", URL: strings.TrimSpace(m.mcpAddURL.Value())}
	default: // stdio
		fields := strings.Fields(strings.TrimSpace(m.mcpAddCmd.Value()))
		if len(fields) == 0 {
			m.appendMessages("⚠️ MCP command is required (e.g. npx -y <pkg>).")
			return
		}
		cfg = mcp.ServerConfig{Command: fields[0], Args: fields[1:]}
	}
	if err := mcp.AddServerToFile(mcp.ProjectMCPPath(), name, cfg); err != nil {
		m.appendMessages("❌ Failed to write " + mcp.ProjectMCPPath() + ": " + err.Error())
		return
	}
	m.reloadMCP()
	m.appendMessages(fmt.Sprintf("✅ Added MCP server %q → %s", name, mcp.ProjectMCPPath()))
}

// deleteMCPServer removes a server from .mcp.json and reloads.
func (m *Model) deleteMCPServer(name string) {
	if name == "" {
		return
	}
	if err := mcp.RemoveServerFromFile(mcp.ProjectMCPPath(), name); err != nil {
		m.appendMessages("❌ Failed to update " + mcp.ProjectMCPPath() + ": " + err.Error())
		return
	}
	m.reloadMCP()
	m.appendMessages(fmt.Sprintf("🗑️ Removed MCP server %q from %s", name, mcp.ProjectMCPPath()))
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

// mcpServerDetail renders one server's full status (used by ENTER in the MCP
// modal — the compact list row shows only the tool count).
func (m *Model) mcpServerDetail(name string) string {
	if m.mcpMgr == nil {
		return "⚠️ MCP not initialized."
	}
	var sb strings.Builder
	sb.WriteString("🔌 " + name)
	if e := m.mcpMgr.Errors()[name]; e != "" {
		sb.WriteString(" — ❌ " + e)
		return sb.String()
	}
	ts := m.mcpMgr.ToolNames(name)
	sort.Strings(ts)
	sb.WriteString(fmt.Sprintf(" — ✅ %d tool(s)\n", len(ts)))
	if len(ts) > 0 {
		sb.WriteString("  " + strings.Join(ts, "\n  "))
	}
	return sb.String()
}

// renderMCPModal renders the interactive MCP manager: server list with
// connect status, empty state with add hint, y/n delete confirm, and the
// add-server wizard (transport → name → command/URL).
func (m *Model) renderMCPModal() string {
	var sb strings.Builder
	greenBadge := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	dangerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	names := m.mcpNames()
	toolCount := 0
	if m.mcpMgr != nil {
		toolCount = len(m.mcpMgr.Tools())
	}
	sb.WriteString(fmt.Sprintf("=== MCP Servers (/mcp) — %d server(s) · %d tool(s) ===\n", len(names), toolCount))

	switch {
	case m.mcpAddActive:
		// Add-server wizard.
		transports := []string{"stdio (local subprocess)", "http (streamable)", "sse (server-sent events)"}
		if m.mcpAddStep == 0 {
			sb.WriteString("\nSelect transport:\n")
			for i, t := range transports {
				cursor := "  "
				if i == m.mcpAddType {
					cursor = "❯ "
				}
				sb.WriteString(fmt.Sprintf("%s%s\n", cursor, t))
			}
			sb.WriteString("\n[↑/↓ transport · ENTER next · ESC cancel]")
		} else {
			if m.mcpAddStep == 1 {
				sb.WriteString("\nServer name:\n  " + m.mcpAddName.View())
			} else {
				if m.mcpAddType == 0 {
					sb.WriteString("\nCommand + args (stdio):\n  " + m.mcpAddCmd.View())
				} else {
					sb.WriteString("\nEndpoint URL (" + transports[m.mcpAddType] + "):\n  " + m.mcpAddURL.View())
				}
			}
			sb.WriteString("\n\n[ENTER next · ESC back]")
		}

	case m.mcpConfirm != "":
		// Destructive action pending: block the list and ask explicitly.
		sb.WriteString("\n" + dangerStyle.Render("⚠️  CONFIRM DELETE — cannot be undone\n"))
		sb.WriteString(fmt.Sprintf("Remove MCP server %q from %s?\n", m.mcpConfirm, mcp.ProjectMCPPath()))
		sb.WriteString("\n[y] confirm delete · [n / ESC] cancel")

	case len(names) == 0:
		// Empty state: nothing configured, show the way in.
		sb.WriteString("\nNo MCP servers configured.\n")
		sb.WriteString("Press [a] to add one — or configure " + mcp.ProjectMCPPath() + " directly.\n")
		sb.WriteString("\n[a] add server · [r] reload · ESC close")

	default:
		errs := m.mcpMgr.Errors()
		for i, n := range names {
			cursor := "  "
			if i == m.mcpSel {
				cursor = "❯ "
			}
			if e := errs[n]; e != "" {
				sb.WriteString(fmt.Sprintf("%s❌ %s — %s\n", cursor, n, e))
				continue
			}
			ts := m.mcpMgr.ToolNames(n)
			sort.Strings(ts)
			sb.WriteString(fmt.Sprintf("%s✅ %s — %d tool(s)\n", cursor, n, len(ts)))
		}
		if m.mcpSel >= 0 && m.mcpSel < len(names) && len(m.mcpMgr.ToolNames(names[m.mcpSel])) > 0 {
			sb.WriteString(greenBadge.Render("\nENTER: show tools of the selected server"))
		}
		sb.WriteString("\n\n[↑/↓ navigate · a add · d delete · r reload · ENTER tools · ESC close]")
	}

	body := sb.String()
	w := m.width - 8
	if w < 30 {
		w = 30
	}
	// Cap very long lines (tool lists) to the modal width.
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if len(ln) > w-4 {
			lines[i] = ln[:w-4]
		}
	}
	return lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2).Render(strings.Join(lines, "\n"))
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

		// Continue in the session's last engine mode (persisted on each mode
		// change) rather than silently dropping back to BUILDER.
		if targetSess.Mode != "" {
			m.mode = targetSess.Mode
		} else {
			m.mode = "BUILDER"
		}
		m.engine.SetMode(m.mode) // apply executor-level policy for the restored mode

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
				// paired with their results, restore file change summaries inline
				// at their original chronological place, and render tool-call-only
				// turns as compact summaries instead of raw JSON.
				m.appendMessages(bcontext.RestoreSession(m.context, events)...)
				// Show the restored FILES: change summaries expanded so the user
				// sees what was edited/created/deleted without pressing ctrl+f.
				for _, msg := range m.messages {
					if strings.HasPrefix(msg, "FILES:\n") {
						m.filesExpanded = true
						break
					}
				}
			}
		}
		// Invalidate the rendered-log cache so the viewport re-renders with the
		// freshly loaded session history instead of showing stale content.
		m.renderedLog = ""
		m.renderedKey = ""
		m.logViewport.SetYOffset(0)
		m.rebuildEngine()
		m.persistMode()
	}
}

// persistMode writes the current engine mode to the session row so a later
// resume (`-c` or /sessions) continues in the same mode. Best-effort: the
// store is optional (nil when SQLite init failed).
func (m *Model) persistMode() {
	if st := m.context.Store(); st != nil {
		_ = st.UpdateSessionMode(m.context.SessionID(), m.mode)
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
	m.modelListCache = nil
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
	m.modelListCache = nil
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
	// Memoize: the modal re-renders on every keystroke, but the underlying list
	// only changes when the filter query (or the discovered models) changes.
	if m.modelListCache != nil && m.modelListCacheQuery == m.modelsQuery {
		return m.modelListCache
	}
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
	m.modelListCache = items
	m.modelListCacheQuery = m.modelsQuery
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
			// OpenCode adapter is a standalone HTTP gateway; no CLI shims.
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
			logLines = strings.Count(log, "\n") + 1
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
				m.logViewport.SetContent(log)
				if key != m.renderedKey || m.renderedH == 0 {
					m.parkLogAfterNewContent(log, vpHeight, contentWidth)
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
		label := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true).Render("BROCODE")
		if m.mode != "" {
			badgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("232")).Bold(true).Padding(0, 1)
			switch m.mode {
			case "PLANNER":
				badgeStyle = badgeStyle.Background(lipgloss.Color("141"))
			case "MINER":
				badgeStyle = badgeStyle.Background(lipgloss.Color("42"))
			default:
				badgeStyle = badgeStyle.Background(lipgloss.Color("205"))
			}
			label += "  " + badgeStyle.Render(m.mode)
		}
		if m.activeModel != "" {
			modelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
			label += "  " + modelStyle.Render(m.activeModel)
		}
		botBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("205")).Padding(1, 2)
		w := contentWidth
		if w <= 0 {
			w = getTerminalWidth() - 2
		}
		botBarStyle = botBarStyle.Width(w)
		out.WriteString(botBarStyle.Render(label+"\n\n"+m.pendingStream) + "\n\n")
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

// renderPager renders the in-TUI pager: the viewport sized to the terminal
// minus chrome, holding the last answer, with the pager bar replacing the
// input. Rebuilds the answer content if the terminal width changed so wrapping
// stays correct.
func (m *Model) renderPager() string {
	var sb strings.Builder
	if m.width > 0 {
		if m.logViewport.Width() != m.width {
			m.logViewport.SetWidth(m.width)
		}
		chrome, chromeLines := m.buildLogChrome()
		avail := m.height - chromeLines
		if avail < 3 {
			avail = 3
		}
		if m.logViewport.Height() != avail {
			m.logViewport.SetHeight(avail)
		}
		contentWidth := m.width - 4
		if contentWidth < 20 {
			contentWidth = 80
		}
		if m.pagerContent == "" || m.pagerWidth != contentWidth {
			m.pagerContent = m.buildPagerContent(contentWidth)
			m.pagerWidth = contentWidth
			m.logViewport.SetContent(m.pagerContent)
			m.logViewport.GotoTop()
		}
		sb.WriteString(m.logViewport.View())
		sb.WriteString(chrome)
	} else {
		sb.WriteString(clipToTerminalBounds(m.pagerContent, getTerminalHeight()-4))
	}
	return sb.String()
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
		spinnerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
		frame := spinnerFrames[m.spinnerIdx%len(spinnerFrames)]
		elapsed := ""
		if !m.turnStart.IsZero() {
			d := time.Since(m.turnStart)
			elapsed = fmt.Sprintf("  ⏱ %d:%02d", int(d.Minutes()), int(d.Seconds())%60)
		}
		sb.WriteString(spinnerStyle.Render(frame) + " " + m.status + elapsed + "\n")
		actStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
		for _, act := range m.activity {
			sb.WriteString(actStyle.Render("  · "+act) + "\n")
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n\n")
	}

	// Queued prompts: shown live above the input, never as history rows. In
	// queue mode (Alt+K) the selected row is highlighted and a hint names the
	// management keys (e edit · d delete · ↑/↓ select · Esc exit).
	if len(m.pendingQueue) > 0 {
		qHead := lipgloss.NewStyle().Foreground(lipgloss.Color("178")).Bold(true).Render(fmt.Sprintf("⏳ PROMPT QUEUE (%d)", len(m.pendingQueue)))
		if m.queueMode {
			qHead += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("  · e edit · d delete · ↑/↓ select · Esc exit")
		} else {
			qHead += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("  · Alt+K manage")
		}
		sb.WriteString(qHead + "\n")
		for i, q := range m.pendingQueue {
			row := "  " + fmt.Sprintf("%d", i+1) + " · " + truncatePrompt(q)
			if m.queueMode && i == m.queueSel {
				sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true).Render("▸ "+row) + "\n")
			} else {
				sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(row) + "\n")
			}
		}
		sb.WriteString("\n")
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
	modeBadgeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	switch m.mode {
	case "PLANNER":
		modeBadgeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	case "MINER":
		modeBadgeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("178"))
	}

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

	footerBanner := fmt.Sprintf("🔥 %s · P:%s · M:%s · S:%s · %s%s",
		modeBadgeStyle.Render(m.mode), m.activeProvider.Info.Name, m.activeModel, sessID, tokenStyle.Render(tokensStr), lspBadge)

	helpStr := " ENTER send · Alt+Enter newline · Tab mode · ↑/↓ history · PgUp/PgDn scroll · Ctrl+P pager · Ctrl+Y copy · Ctrl+M mouse · /help "
	if m.width >= 120 {
		helpStr = " ENTER send · Alt+Enter newline · Tab/Shift+Tab mode · ↑/↓ history · PgUp/PgDn scroll · Ctrl+P pager · Ctrl+Y copy · Ctrl+M mouse mode · /sessions · /models · /lsp · /help "
	} else if m.width >= 90 {
		helpStr = " ENTER send · Alt+Enter newline · Tab mode · ↑/↓ history · Ctrl+P pager · Ctrl+Y copy · Ctrl+M mouse · /sessions · /models · /help "
	}
	// When prompts are queued, advertise the queue-management key. Only on wide
	// terminals so the hint never pushes the help bar onto two wrapped lines;
	// the queue block itself already shows "Alt+K manage" on narrow screens.
	if len(m.pendingQueue) > 0 && m.width >= 120 {
		helpStr += " · Alt+K queue "
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
	u, a, t := m.context.TokenBreakdown()
	sb.WriteString(fmt.Sprintf("Session ID: %s\nTotal Tokens: %s / %s\nEvents Count: %d\nTokenizer: %s\nTokens by kind (cumulative): user %d · assistant %d · tool output %d\n\n",
		m.context.SessionID(), provider.FormatTokens(m.context.TotalTokens()), provider.FormatTokens(m.context.MaxWindow()), len(m.context.Messages()), tokens.CountMethod(m.activeModel), u, a, t))

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

// diffStat counts added/removed lines in a unified diff (ignoring the
// +++/--- file headers and @@ hunk markers) so a collapsed DIFF entry can
// show a compact (+N −M) summary.
func diffStat(diff string) (add, del int) {
	for _, line := range strings.Split(diff, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case '+':
			if strings.HasPrefix(line, "+++") {
				continue
			}
			add++
		case '-':
			if strings.HasPrefix(line, "---") {
				continue
			}
			del++
		}
	}
	return
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

func getTerminalHeight() int {
	if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && h > 10 {
		return h
	}
	return 40
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

		// Live per-edit diff (engine onChange → fileDiffMsg): show the changed
		// file path immediately as each edit lands. Collapsed by default to a
		// (+N −M) stat so the chat stays quiet; ctrl+f expands to the full
		// red/green unified diff.
		if strings.HasPrefix(msg, "DIFF:\n") {
			body := strings.TrimPrefix(msg, "DIFF:\n")
			path, diff := body, ""
			if nl := strings.Index(body, "\n"); nl >= 0 {
				path, diff = body[:nl], body[nl+1:]
			}
			diffBarStyle := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
			if width > 0 {
				diffBarStyle = diffBarStyle.Width(width)
			}
			add, del := diffStat(diff)
			actionLabel := "DIFF"
			labelColor := "178" // gold for modified
			if del == 0 && add > 0 && (!strings.Contains(diff, "@@ -") || strings.Contains(diff, "@@ -0,0 +")) {
				actionLabel = "CREATE"
				labelColor = "42" // green for newly created
			} else if add == 0 && del > 0 {
				actionLabel = "DELETE"
				labelColor = "196" // red for deleted
			}
			labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(labelColor)).Bold(true)
			if filesExpanded {
				return diffBarStyle.Render(labelStyle.Render(actionLabel) + "  " + path + "\n" + formatDiffLines(diff))
			}
			statStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
			return diffBarStyle.Render(labelStyle.Render(actionLabel) + "  " + path + "  " + statStyle.Render(fmt.Sprintf("(+%d −%d) · [press Ctrl+F for diff]", add, del)))
		}


	if strings.HasPrefix(msg, "PROCESS:\n") {
		content := strings.TrimPrefix(msg, "PROCESS:\n")
		formatted := formatDiffLines(content)
		return processBarStyle.Render(processLabelStyle.Render(formatted))
	}

	// Assistant messages may be mode-stamped ("BROCODE:PLANNER\n...") so each
	// answer shows which engine mode produced it as a colored badge next to
	// the BROCODE label. The active model rides after the mode separated by a
	// colon ("BROCODE:PLANNER:poolside/x\n...") and renders dimmed next to the
	// badge. Legacy "BROCODE:\n" and "🤖 " forms carry no mode and render
	// without a badge (e.g. sessions restored from disk).
	mode := ""
	model := ""
	var content string
	if strings.HasPrefix(msg, "BROCODE:") {
		rest := strings.TrimPrefix(msg, "BROCODE:")
		if i := strings.Index(rest, "\n"); i >= 0 {
			stamp := rest[:i]
			content = rest[i+1:]
			if s, m, ok := strings.Cut(stamp, ":"); ok {
				mode = s
				model = m
			} else {
				mode = stamp
			}
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
		// Dimmed model label right after the badge ("BROCODE BUILDER
		// poolside/laguna-s-2.1") so it's clear which provider/model answered.
		if model != "" {
			modelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
			label += " " + modelStyle.Render(model)
		}

		if thinking != "" {
			return botBarStyle.Render(label + "\n\n" +
				thinkingStyle.Render(thinking) + "\n\n" + formattedBody)
		}
		return botBarStyle.Render(label + "\n\n" + formattedBody)
	}

	if strings.HasPrefix(msg, "ERROR: ") || strings.HasPrefix(msg, "❌ ") {
		return errStyle.Render(msg)
	}

	if width > 0 {
		return lipgloss.NewStyle().Width(width).Render(msg)
	}
	return msg
}
