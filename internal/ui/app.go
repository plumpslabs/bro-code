package ui

import (
	"context"
	"io"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/plumpslabs/bro-code/internal/agent"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/lsp"
	"github.com/plumpslabs/bro-code/internal/mcp"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/repo"
	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/subagent"
	"github.com/plumpslabs/bro-code/internal/tool"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type spinnerTickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// liveModelsRefreshMsg fires periodically to re-read the live models cache
// and update m.modelOptions so providers whose background /models fetch
// completes after startup (OpenRouter, custom gateways) appear in the
// model picker without requiring a /models modal reopen.
type liveModelsRefreshMsg struct{}

// liveModelsRefreshCmd polls every 5 seconds for live model updates.
// It stops automatically once the background fetch has populated the cache
// (detected via LiveModelsVersion) and the UI has refreshed at least once.
func liveModelsRefreshCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return liveModelsRefreshMsg{}
	})
}

// startupReadinessCheckCmd polls periodically to detect when background
// initialization (global index build, live model fetch) has completed.
func startupReadinessCheckCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return startupReadinessCheckMsg{}
	})
}

type startupReadinessCheckMsg struct{}



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

	// agentLoader discovers custom agents and modes (.brocode/agents and ~/.config/brocode/agents).
	agentLoader *agent.Loader
	activeAgent *agent.CustomAgent

	// Cancelation function for active LLM turn / tool execution
	cancelTurn context.CancelFunc

	// turnRunning is true while a turn is in flight. Prompts sent while a turn
	// runs are queued in pendingQueue and auto-sent when it finishes: one turn
	// at a time, because concurrent RunTurn calls clobber the engine's shared
	// per-turn state and can crash the CLI progress goroutine (nil handler).
	turnRunning  bool
	turnMode     string
	// turnGen is an monotonically-increasing counter, incremented each time a
	// new turn starts. turnResultMsg carries the gen value at dispatch time —
	// if the message arrives with a stale gen (the turn was interrupted and a
	// new one has already started), it is discarded instead of overwriting the
	// new turn's live state. This prevents the "ESC → Enter → new turn starts
	// → old goroutine finishes → turnRunning=false prematurely" race.
	turnGen      int
	pendingQueue []QueuedPrompt

	// queueSel is the highlighted index into pendingQueue while queueMode is
	// active (Ctrl+K / Alt+K): the queued prompts are shown in the activity slot above
	// the input — never in the conversation history — and e/d/m edit, delete, or
	// change mode of the selected one.
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
	// turnTier stores the complexity tier of the current turn so the
	// watchdog can derive an adaptive wall-clock timeout.
	turnTier loop.ComplexityTier

	// Live token streaming state & memoized formatting cache
	streaming          bool
	pendingStream      string
	streamRenderCached string
	streamRenderRaw    string
	streamRenderWrap   int

	// activity holds the most recent agent steps (tool calls, reasoning)
	// during a turn. Rendered live in the status slot above the input — never
	// appended to the conversation history.
	activity []string

	// activeRecommendations holds interactive follow-up suggestions from the latest turn
	activeRecommendations []QuickRecommendation


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

	// lastLiveModelsVersion tracks the liveModelsCache generation at the time
	// modelOptions was last refreshed, so the periodic poll only re-runs
	// DiscoverModels when the background fetch has actually populated new data.
	lastLiveModelsVersion int64

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

// QueuedPrompt holds a user prompt waiting to run, along with the engine mode
// (BUILDER/PLANNER/MINER) under which it should be executed when drained.
type QueuedPrompt struct {
	Text string `json:"text"`
	Mode string `json:"mode"`
}

type turnResultMsg struct {
	content string
	err     error
	// mode is the engine mode (BUILDER/PLANNER/MINER) the turn ran under,
	// captured at send time so the answer can be stamped with a mode badge
	// even if the user toggles mode while the turn is in flight.
	mode string
	// gen is the turn-generation counter at the time this turn was started.
	// When a stale turnResultMsg arrives (from a goroutine whose turn was
	// interrupted and a new turn already started), it is silently discarded
	// rather than clobbering the new turn's turnRunning/streaming state.
	gen int
}

// productiveIterMsg is sent by the engine's productiveIterHandler after a
// file-mutation round so the Bubble Tea loop can reset the turn watchdog timer.
type productiveIterMsg struct{}

// maxChatMessages is a SAFETY CEILING, not a display window: the chat log
// keeps every message of the session in memory (the user's history must never
// be pruned from the screen), and rendering stays cheap via the
// renderedHistory cache. Only a pathological session (thousands of entries)
// hits this ceiling, and even then the oldest entries remain in session
// history in SQLite — with a one-time notice instead of silent loss.
const maxChatMessages = 5000
