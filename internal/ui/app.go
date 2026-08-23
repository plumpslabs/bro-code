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
	"runtime"
	"runtime/debug"
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
	"github.com/plumpslabs/bro-code/internal/agent"
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
	"github.com/plumpslabs/bro-code/internal/report"
	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/skill"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/subagent"
	"github.com/plumpslabs/bro-code/internal/tokens"
	"github.com/plumpslabs/bro-code/internal/tool"
	"github.com/plumpslabs/bro-code/internal/version"
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
		// Cap cached renderers to 4 recent widths to prevent memory leaks on continuous terminal resizes
		if len(mdRenderers.m) >= 4 {
			for k := range mdRenderers.m {
				delete(mdRenderers.m, k)
				break
			}
		}
		r, _ = glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(wrap),
			glamour.WithPreservedNewLines(),
		)
		if r != nil {
			mdRenderers.m[wrap] = r
		}
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

	// Live token streaming state
	streaming     bool
	pendingStream string

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

// diagnoseResultMsg carries the output of an async /diagnose project scan.
type diagnoseResultMsg string

// diagnoseFixMsg carries a finished project scan whose findings should be
// handed straight to the agent to fix (the `/diagnose fix` command).
type diagnoseFixMsg string

// ephemeralAskResultMsg carries the output of an isolated /ask query without context pollution.
type ephemeralAskResultMsg string

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
	m.tools.Register(&tool.CodeSliceTool{Index: m.globalIndex})
	m.tools.Register(&tool.BlastRadiusTool{Index: m.globalIndex})
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
	if m.repoMap != nil {
		m.engine.SetRepoMap(m.repoMap.String())
		stackHints := make([]prompt.Stack, 0, len(m.repoMap.Stacks))
		for _, s := range m.repoMap.Stacks {
			stackHints = append(stackHints, prompt.Stack{Name: s.Name, Files: s.Files})
		}
		m.engine.SetDetectedStacks(stackHints)
	}
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
	// Auto-install the embedded default skill pack into the global config root (~/.config/brocode/skills),
	// keeping repos clean and unpolluted while providing access to the full catalog.
	skill.EnsureGlobalDefaultsInstalled()
	m.engine.SetSkillCatalog(skillEntries(cwd))

	// Custom Agents & Modes (.brocode/agents/*.md and ~/.config/brocode/agents/*.md)
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
	// User-defined lifecycle hooks (.brocode/hooks.json and active custom agents) fire at turn
	// start/end/error and around tool calls. Loaded lazily on first engine
	// build; engine is rebuilt on model switches but hooks are cheap to reload.
	hk := hooks.Load(cwd)
	if m.activeAgent != nil {
		hk.AddHooks(m.activeAgent.ToHooks())
	}
	m.engine.SetHooks(hk)
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
// .brocode/skills, plus user/project .brocode/skills and the global
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
	return tea.Batch(m.promptInput.Focus(), checkUpdateCmd)
}

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
		// When idle (no turn running, not busy), stop the ticker immediately
		// so BroCode consumes 0.0% CPU and zero battery while waiting for input.
		if !m.turnRunning && (m.status == "Ready" || m.status == "Failed") {
			return m, nil
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

		// Live session memory capture: capture touched files and goals after
		// each turn so .brocode/memory.md is always up-to-date in real-time,
		// without having to wait for the user to exit the CLI (Ctrl+C).
		if m.memStore != nil && m.context != nil && m.context.Store() != nil {
			if events, err := m.context.Store().GetSessionEvents(m.context.SessionID()); err == nil && len(events) > 0 {
				_ = m.memStore.CaptureSession(m.context.SessionID(), events)
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
			if next.Mode != "" {
				m.mode = next.Mode
				m.engine.SetMode(m.mode)
				m.persistMode()
			}
			return m.startTurn(next.Text)
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

		// Queue management mode (Ctrl+K / Alt+K): while active, keys manage the queued
		// prompts instead of typing into the input. ↑/↓ move selection,
		// K/J (or Shift+↑/↓) reorder queue positions, m/Tab cycles mode,
		// e edits prompt + sets mode in input, d deletes it, Esc/Ctrl+K/Alt+K exit.
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
					// Swallow Enter while managing the queue so the input can't
					// accidentally send mid-management; Esc/Ctrl+K exit instead.
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
					// Move selected queued item UP in execution order
					if m.queueSel > 0 {
						m.pendingQueue[m.queueSel], m.pendingQueue[m.queueSel-1] = m.pendingQueue[m.queueSel-1], m.pendingQueue[m.queueSel]
						m.queueSel--
					}
					return m, nil
				case "J", "shift+down":
					// Move selected queued item DOWN in execution order
					if m.queueSel < len(m.pendingQueue)-1 {
						m.pendingQueue[m.queueSel], m.pendingQueue[m.queueSel+1] = m.pendingQueue[m.queueSel+1], m.pendingQueue[m.queueSel]
						m.queueSel++
					}
					return m, nil
				case "m", "M", "tab", "shift+tab":
					// Cycle the target engine mode for the selected queued item
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
					// Edit the selected queued prompt: load it into the input
					// (replacing whatever was typed) and drop it from the queue
					// so it can't auto-send mid-edit.
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
			// If user is typing in the input box, clear the entire input prompt (cross-platform standard readline)
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk && m.promptInput.Value() != "" {
				m.promptInput.Reset()
				m.autocomplete = AutocompleteState{}
				return m, nil
			}
			m.logViewport.HalfPageUp()
			return m, nil

		case "alt+backspace", "ctrl+backspace", "ctrl+delete", "alt+delete":
			// Wipe the input prompt immediately on Mac/Windows word/line delete shortcuts
			if !m.showModels && !m.showConnect && !m.showDebug && !m.showSessions && !m.showMCP && !m.showAsk && m.promptInput.Value() != "" {
				m.promptInput.Reset()
				m.autocomplete = AutocompleteState{}
				return m, nil
			}

		case "ctrl+d":
			m.logViewport.HalfPageDown()
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

		case "alt+k", "ctrl+k", "alt+K", "ctrl+K":
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
				// If no turn is running, update engine mode immediately.
				// If a turn is running, the in-flight turn keeps its running mode,
				// and m.mode applies to any new prompt typed and queued.
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

			// If user is typing and no turn is running, ESC clears the input bar immediately
			if !m.turnRunning && m.promptInput.Value() != "" {
				m.promptInput.Reset()
				m.autocomplete = AutocompleteState{}
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
				// Bump generation so stale turnResultMsg from the cancelled
				// goroutine is safely discarded by the generation guard.
				m.turnGen++
				// Snapshot any partial stream before resetting so history stays connected.
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
				// Drain any already-queued prompts immediately.
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
				m.pendingQueue = append(m.pendingQueue, QueuedPrompt{
					Text: userQuery,
					Mode: m.mode,
				})
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
			// When prompt input is empty and recommendations exist: type 1/2/3 to execute or queue
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
		helpContent := `### 🚀 Core Engineering Commands
- **` + "`/ask <question>`" + `** — Ephemeral Codebase QA: Ask questions without polluting context
- **` + "`/spec <feature>`" + `** — Spec-First Gate: Draft an architectural blueprint contract before coding
- **` + "`/tournament <task>`" + `** — Multi-Candidate Solver: Run 2 parallel candidate agents on difficult bugs
- **` + "`/plan`" + `** — Inspect active plan checklist (` + "`/plan archive`" + ` to archive)
- **` + "`/undo`" + `** — Time-Travel Rollback: Revert all file changes made in the last turn
- **` + "`/diagnose`" + `** — Scan project for type errors/warnings (` + "`/diagnose fix`" + ` to auto-fix)
- **` + "`/cost`" + `** — Live token usage & spend telemetry (USD & IDR)

### ⚙️ Sessions & Configuration
- **` + "`/models`" + `** — Interactive model picker (` + "`/model <id>`" + ` to switch)
- **` + "`/connect`" + `** — 2-step API Key & provider setup wizard
- **` + "`/sessions`" + `**, **` + "`/history`" + `** — Switch or manage past sessions
- **` + "`/memory`" + `** — Inspect cross-session project memory
- **` + "`/mcp`" + `** — Manage connected MCP servers & tools
- **` + "`/lsp`" + `** — Code intelligence status (` + "`/lsp-install`" + ` to install missing servers)
- **` + "`/workspace`" + `** — Inspect multi-repo workspace structure
- **` + "`/clear`" + `**, **` + "`/new`" + `** — Clear chat or start fresh session

### 🔀 Modes (Toggle with ` + "`Shift+Tab`" + `)
- **` + "`BUILDER`" + `** *(Default)* — Autonomous coding agent with full read, write, edit, & run tools
- **` + "`PLANNER`" + `** — Read-only architecture & strategy agent
- **` + "`MINER`" + `** — Read-only knowledge mining agent that persists facts to memory`
		m.appendNote("HELP:\n" + helpContent)

	case "/miner":
		// Jump straight into MINER mode so the next prompt is a knowledge
		// mining pass that persists verified facts into project memory.
		m.mode = "MINER"
		m.engine.SetMode(m.mode)
		m.persistMode()
		m.appendNote("MODE:MINER\n⛏️ MINER mode active — explore the codebase and I'll persist verified knowledge (architecture, build commands, conventions, decisions, gotchas) into project memory. Shift+Tab to switch back to BUILDER.")

	case "/builder":
		m.mode = "BUILDER"
		m.engine.SetMode(m.mode)
		m.persistMode()
		m.appendNote("MODE:BUILDER\n🔨 BUILDER mode active — autonomous coding agent with full read, write, edit, and execution capabilities.")

	case "/planner":
		m.mode = "PLANNER"
		m.engine.SetMode(m.mode)
		m.persistMode()
		m.appendNote("MODE:PLANNER\n📋 PLANNER mode active — read-only architecture and strategy agent.")

	case "/mode":
		if len(parts) > 1 {
			target := strings.ToUpper(strings.TrimSpace(parts[1]))
			if target == "BUILDER" || target == "PLANNER" || target == "MINER" {
				m.mode = target
				m.engine.SetMode(m.mode)
				m.persistMode()
				m.appendNote(fmt.Sprintf("MODE:%s\n✅ Mode switched to %s", m.mode, m.mode))
				return m, nil
			}
		}
		m.appendNote("Usage: /mode <builder|planner|miner> (or toggle with Shift+Tab)")

	case "/plan":
		cwd, _ := os.Getwd()
		if len(parts) > 1 && (parts[1] == "archive" || parts[1] == "clear" || parts[1] == "reset") {
			archPath, err := plan.ArchiveCurrentPlan(cwd)
			if err != nil {
				m.appendNote("PLAN:\n" + fmt.Sprintf("⚠️ **Failed to archive plan:** %v\n\n*(No active plan found in `.brocode/current_plan.md`)*", err))
			} else {
				relPath := archPath
				if rel, err := filepath.Rel(cwd, archPath); err == nil {
					relPath = rel
				}
				m.appendNote("PLAN:\n" + fmt.Sprintf("📦 **Plan archived successfully!**\n\nSaved to: `%s`\n\n💡 Current active plan has been cleared. Switch to **PLANNER** (`Shift+Tab`) to draft a fresh goal.", relPath))
			}
			return m, nil
		}
		curPlan, err := plan.LoadCurrentPlan(cwd)
		if err != nil || curPlan == nil || len(curPlan.Steps) == 0 {
			m.appendNote("PLAN:\n" + "ℹ️ **No active plan found in `.brocode/current_plan.md`**\n\nSwitch to **PLANNER** mode (`Shift+Tab` or `/planner`) to draft an execution plan for your next feature or bugfix.")
		} else {
			m.appendNote("PLAN:\n" + plan.RenderMarkdownPlan(curPlan))
		}

	case "/memory":
		if m.memStore != nil {
			s := m.memStore.List()
			if strings.TrimSpace(s) == "" {
				s = "ℹ️ No long-term project memory entries recorded yet.\n\nSwitch to **MINER** mode (`Shift+Tab` or `/miner`) to explore and persist verified architecture, conventions, and decisions to `.brocode/memory.md`."
			}
			if m.memStore.Path() != "" {
				s += "\n\n📍 *" + m.memStore.Path() + "*"
			}
			m.appendMessages("MEMORY:\n" + s)
		} else {
			m.appendMessages("⚠️ Project memory not initialized.")
		}

	case "/cost":
		m.appendMessages("COST:\n" + m.engine.CostSummary())

	case "/ask":
		query := strings.TrimSpace(strings.TrimPrefix(cmd, "/ask"))
		if query == "" {
			m.appendMessages("Usage: `/ask <question>`\nAsk an isolated question about the codebase without polluting your active task's conversation context.\n\nExample: `/ask Where is the WhatsApp webhook handler defined?`")
			return m, nil
		}
		if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
			m.appendNote("⚠️ Subagent runner is not initialized for /ask.")
			return m, nil
		}
		m.appendNote("CMD:/ask\n" + query)
		m.status = fmt.Sprintf("Answering: %s...", truncatePrompt(query))
		m.turnStart = time.Now()
		runner := m.scoutMgr.Runner
		recentCtx := extractRecentSessionContext(m.messages, 4)
		askPrompt := fmt.Sprintf(
			"%s"+
				"You are an expert codebase answering assistant. The user is asking:\n\n"+
				"\"%s\"\n\n"+
				"Instructions:\n"+
				"1. Be helpful, perceptive, and interpret informal phrasing or typos intelligently.\n"+
				"2. Language: Formulate your answer in the user's language (e.g. Bahasa Indonesia).\n"+
				"3. Working Directory: You are ALREADY in the project repository root. Do NOT attempt to run 'cd' or switch directories.\n"+
				"4. Search and inspect the actual repository using codebase tools (code_locate, grep, read_file, glob) to find relevant code, models, services, functions, and configs.\n"+
				"5. If git history or diffs are requested (e.g. comparing before/after changes), execute read-only git commands directly (e.g. 'git log -n 10 --oneline', 'git diff HEAD~1', 'git show HEAD') without using 'cd'.\n"+
				"6. Provide a clear, direct, and structured explanation citing exact file paths and code references.\n"+
				"7. Anti-loop efficiency: Once you locate the relevant functions/schemas, synthesize and output your answer directly instead of repeatedly reading file slices in small chunks.\n"+
				"8. Do NOT edit or modify any files.",
			recentCtx, query,
		)
		prog := m.prog
		return m, tea.Batch(tickCmd(), func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			ans, err := runner.RunWithProgress(ctx, askPrompt, "BUILDER", func(state loop.LoopState, info string) {
				if prog != nil {
					prog.Send(stepProgressMsg{state: state, info: info})
				}
			})
			if err != nil {
				return ephemeralAskResultMsg(fmt.Sprintf("❌ `/ask` query failed: %v", err))
			}
			return ephemeralAskResultMsg(fmt.Sprintf("ASK:\n%s\n---\n%s", query, ans))
		})

	case "/spec":
		feature := strings.TrimSpace(strings.TrimPrefix(cmd, "/spec"))
		if feature == "" {
			m.appendNote("Usage: `/spec <feature description>`\nDraft a structured Architectural Blueprint Specification Contract (ADR, endpoints, data models, blast radius) before writing code.\n\nExample: `/spec Multi-channel Webhook Dispatcher`")
			return m, nil
		}
		if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
			m.appendNote("⚠️ Subagent runner is not initialized for /spec.")
			return m, nil
		}
		m.appendNote("CMD:/spec\n" + feature)
		m.status = fmt.Sprintf("Drafting spec: %s...", truncatePrompt(feature))
		m.turnStart = time.Now()
		return m, executeSpecCommand(m.scoutMgr.Runner, feature, m.prog)

	case "/tournament":
		task := strings.TrimSpace(strings.TrimPrefix(cmd, "/tournament"))
		if task == "" {
			m.appendNote("Usage: `/tournament <bug or complex task>`\nRuns 2 parallel candidate agents with distinct solving strategies to find the cleanest, verified solution.\n\nExample: `/tournament Fix race condition in connection pooling`")
			return m, nil
		}
		if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
			m.appendNote("⚠️ Subagent runner is not initialized for /tournament.")
			return m, nil
		}
		m.appendNote("CMD:/tournament\n" + task)
		m.status = fmt.Sprintf("Running tournament: %s...", truncatePrompt(task))
		m.turnStart = time.Now()
		return m, executeTournamentCommand(m.scoutMgr.Runner, task, m.prog)

	case "/update", "/upgrade":
		m.status = "🚀 Checking for updates & self-updating in background..."
		m.turnStart = time.Now()
		return m, tea.Batch(tickCmd(), func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			latest, hasUpdate, err := version.CheckLatestVersion(ctx, true)
			if err != nil {
				return ephemeralAskResultMsg(fmt.Sprintf("UPDATE:\n❌ Update check failed: %v", err))
			}
			if !hasUpdate {
				return ephemeralAskResultMsg(fmt.Sprintf("UPDATE:\n✨ You are already on the latest version of BroCode (**%s**)!\n\nNo upgrade is needed at this time.", version.Version))
			}
			msg, err := version.SelfUpdate(ctx, latest)
			if err != nil {
				return ephemeralAskResultMsg(fmt.Sprintf("UPDATE:\n❌ Upgrade failed: %v\n\nYou can manually upgrade with:\n• Windows: `irm https://raw.githubusercontent.com/plumpslabs/bro-code/main/scripts/install.ps1 | iex`\n• macOS/Linux: `curl -fsSL https://raw.githubusercontent.com/plumpslabs/bro-code/main/scripts/install.sh | bash`", err))
			}
			return ephemeralAskResultMsg(fmt.Sprintf("UPDATE:\n%s\n\n👉 Please restart BroCode to run version **%s**.", msg, latest))
		})

	case "/repair":
		errCtx := strings.TrimSpace(strings.TrimPrefix(cmd, "/repair"))
		if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
			m.appendNote("REPAIR:\n⚠️ Subagent runner is not initialized for /repair.")
			return m, nil
		}
		m.appendNote("CMD:/repair\n" + errCtx)
		m.status = "Pipeline Doctor: Diagnosing & fixing failures..."
		m.turnStart = time.Now()
		return m, executeRepairCommand(m.scoutMgr.Runner, errCtx, m.prog)

	case "/diff":
		targetPath := strings.TrimSpace(strings.TrimPrefix(cmd, "/diff"))
		w := m.width
		if w <= 0 {
			w = 100
		}
		diffOut := GenerateSessionDiffSummary(targetPath, w)
		m.appendNote(diffOut)
		return m, nil

	case "/worktree":
		parts := strings.Fields(strings.TrimPrefix(cmd, "/worktree"))
		cwd, _ := os.Getwd()
		wm := tool.NewWorktreeManager(cwd)

		if len(parts) == 0 {
			list, err := wm.ListWorktrees()
			if err != nil || len(list) == 0 {
				m.appendNote("WORKTREE:\nNo isolated background worktrees active.\n\nUsage: `/worktree <task description>` to run an autonomous task in an isolated branch.\n\nSub-commands:\n• `/worktree list` — List all active worktree branches\n• `/worktree merge <branch>` — Merge worktree branch to main\n• `/worktree clean` — Remove all isolated worktrees")
				return m, nil
			}
			var sb strings.Builder
			for _, wt := range list {
				sb.WriteString(fmt.Sprintf("• **%s** (Branch: `%s`)\n  Path: `%s`\n\n", filepath.Base(wt.Directory), wt.Branch, wt.Directory))
			}
			sb.WriteString("👉 Merge a finished worktree with `/worktree merge <branch>` or delete with `/worktree clean`.")
			m.appendNote("WORKTREE:\n" + sb.String())
			return m, nil
		}

		sub := strings.ToLower(parts[0])
		switch sub {
		case "list":
			list, _ := wm.ListWorktrees()
			if len(list) == 0 {
				m.appendNote("WORKTREE:\nNo active isolated worktrees found.")
				return m, nil
			}
			var sb strings.Builder
			for _, wt := range list {
				sb.WriteString(fmt.Sprintf("• **%s** (Branch: `%s`)\n  Path: `%s`\n\n", filepath.Base(wt.Directory), wt.Branch, wt.Directory))
			}
			m.appendNote("WORKTREE:\n" + sb.String())
			return m, nil

		case "merge":
			if len(parts) < 2 {
				m.appendNote("WORKTREE:\nUsage: `/worktree merge <branch-name>`")
				return m, nil
			}
			branch := parts[1]
			out, err := wm.MergeWorktree(branch)
			if err != nil {
				m.appendNote(fmt.Sprintf("WORKTREE:\n❌ Merge failed: %v\nOutput:\n%s", err, out))
			} else {
				m.appendNote(fmt.Sprintf("WORKTREE:\n✅ Successfully merged branch `%s` into active workspace!", branch))
			}
			return m, nil

		case "clean":
			worktreeRoot := filepath.Join(cwd, ".brocode", "worktrees")
			_ = os.RemoveAll(worktreeRoot)
			m.appendNote("WORKTREE:\n🧹 Cleaned up all isolated worktrees in `.brocode/worktrees/`.")
			return m, nil

		default:
			task := strings.Join(parts, " ")
			if m.scoutMgr == nil || m.scoutMgr.Runner == nil {
				m.appendNote("WORKTREE:\n⚠️ Subagent runner is not initialized for /worktree.")
				return m, nil
			}
			wtDir, branch, err := wm.CreateWorktree(task)
			if err != nil {
				m.appendNote(fmt.Sprintf("WORKTREE:\n❌ Failed to create worktree: %v", err))
				return m, nil
			}
			m.appendNote(fmt.Sprintf("WORKTREE:\n🌿 Spawned isolated worktree: `%s` (Branch: `%s`)\n\nStarting background agent in sandbox...", wtDir, branch))
			m.status = fmt.Sprintf("Running isolated worktree task: %s...", truncatePrompt(task))
			m.turnStart = time.Now()

			// Run subagent inside the worktree directory
			return m, tea.Batch(tickCmd(), func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
				defer cancel()
				subTask := subagent.SubAgent{
					ID:        "worktree_" + branch,
					Task:      fmt.Sprintf("Work in directory %s to implement: %s", wtDir, task),
					Mode:      "BUILDER",
					TargetDir: wtDir,
					Mutates:   true,
				}
				answers, rErr := m.scoutMgr.Runner.RunMany(ctx, []subagent.SubAgent{subTask}, false, nil)
				if rErr != nil || len(answers) == 0 {
					return ephemeralAskResultMsg(fmt.Sprintf("WORKTREE:\n❌ Worktree task failed: %v", rErr))
				}
				return ephemeralAskResultMsg(fmt.Sprintf("WORKTREE:\n%s\n---\n✅ Branch: `%s`\nType `/worktree merge %s` to merge into main workspace.", answers[0], branch, branch))
			})
		}

	case "/search-key", "/search":
		arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(cmd, "/search-key"), "/search"))
		if arg == "" {
			st := provider.GetSearchProviderStatus()
			if st.PrimaryProvider != "free" {
				maskedPrimary := st.PrimaryKey
				if len(maskedPrimary) > 8 {
					maskedPrimary = maskedPrimary[:4] + "..." + maskedPrimary[len(maskedPrimary)-4:]
				}
				if st.SecondaryProvider != "" {
					maskedSecondary := st.SecondaryKey
					if len(maskedSecondary) > 8 {
						maskedSecondary = maskedSecondary[:4] + "..." + maskedSecondary[len(maskedSecondary)-4:]
					}
					m.appendNote(fmt.Sprintf("SEARCH:\n• **Mode**: Multi-Tier AI Web Search (Active Cascade)\n• **Primary Provider**: %s (`%s`)\n• **Fallback Provider**: %s (`%s`)\n• **Fallback 2**: Zero-Config Free Engine (DuckDuckGo)\n• **Footer Badge**: `%s`\n\n👉 **Management Commands**:\n• Change primary: `/search-key <key>`\n• Reset to Free Mode: `/search-key clear`", strings.ToUpper(st.PrimaryProvider), maskedPrimary, strings.ToUpper(st.SecondaryProvider), maskedSecondary, strings.TrimPrefix(st.Badge, " · ")))
				} else {
					quotaInfo := "1,000 Free Searches/Month (tavily.com)"
					if st.PrimaryProvider == "exa" {
						quotaInfo = "Exa Neural Search API (exa.ai)"
					}
					m.appendNote(fmt.Sprintf("SEARCH:\n• **Provider**: %s\n• **API Key**: `%s`\n• **Mode**: Dedicated High-Speed AI Web Search\n• **Quota**: %s\n• **Footer Badge**: `%s`\n\n👉 **Management Commands**:\n• Set Tavily key: `/search-key tvly-xxxx` (or `/search-key tavily <key>`)\n• Set Exa key: `/search-key exa-xxxx` (or `/search-key exa <key>`)\n• Reset to Free Mode: `/search-key clear`", strings.ToUpper(st.PrimaryProvider), maskedPrimary, quotaInfo, strings.TrimPrefix(st.Badge, " · ")))
				}
			} else {
				m.appendNote("SEARCH:\n• **Current Status**: Zero-Config Free Mode (DuckDuckGo HTML / Lite / Wikipedia)\n\n👉 **Want dedicated, instant web search with 1,000 free searches/month?**\n1. Sign up for free at **https://tavily.com** (no credit card needed)\n2. Copy your API key (starts with `tvly-...`)\n3. Run: `/search-key tvly-xxxxxxxxxxxx`\n\nBroCode will save it permanently to `~/.config/brocode/config.json` and display `🌐:Tavily` in the bottom status bar!\n\n*(Also supports Exa AI via `/search-key exa <key>` or `/search-key exa-xxxx`)*")
			}
			return m, nil
		}

		lower := strings.ToLower(arg)
		if lower == "clear" || lower == "reset" || lower == "delete" || lower == "remove" {
			_ = provider.SaveSearchKey("")
			m.appendNote("SEARCH:\n🧹 **Search Key Cleared & Removed!**\n\nBroCode has switched to **Zero-Config Free Search Mode** (`🌐:Free`).")
			return m, nil
		}

		parts := strings.Fields(arg)
		prov := ""
		key := arg
		if len(parts) == 2 && (strings.EqualFold(parts[0], "tavily") || strings.EqualFold(parts[0], "exa")) {
			prov = strings.ToLower(parts[0])
			key = parts[1]
		}

		if err := provider.SaveSearchProviderKey(prov, key); err != nil {
			m.appendNote(fmt.Sprintf("SEARCH:\n❌ Failed to save search key: %v", err))
			return m, nil
		}
		_, activeProv := provider.GetActiveSearchKey()
		if activeProv == "" {
			activeProv = "tavily"
		}
		m.appendNote(fmt.Sprintf("SEARCH:\n✅ **Web Search Provider Configured Successfully!**\n\n• **Provider**: %s\n• **Status**: Active & Persisted to `~/.config/brocode/config.json`\n• **Bottom Bar**: `🌐:%s` (Active)\n\nBroCode web search is now configured for high-speed documentation and web research!", strings.ToUpper(activeProv), strings.Title(activeProv)))
		return m, nil

	case "/context7-key", "/c7-key", "/context7":
		arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(cmd, "/context7-key"), "/c7-key"), "/context7"))
		if arg == "" {
			k := provider.GetActiveContext7Key()
			if k != "" {
				masked := k
				if len(k) > 8 {
					masked = k[:4] + "..." + k[len(k)-4:]
				}
				m.appendNote(fmt.Sprintf("CONTEXT7:\n• **Provider**: Context7 Official Docs API (Native REST)\n• **API Key**: `%s`\n• **Status**: Active & Verified\n• **Docs Cascade**: Layer 1 (Local/AST) ➔ Layer 2 (Context7) ➔ Layer 3 (Web Search)\n\n👉 **Management Commands**:\n• Change key: `/context7-key <new-key>`\n• Remove key: `/context7-key clear`", masked))
			} else {
				m.appendNote("CONTEXT7:\n• **Current Status**: Unconfigured (Using Web Search Fallback)\n\n👉 **Want instant, up-to-date official library documentation (Next.js, Tailwind, FastAPI, etc.)?**\n1. Sign up for free at **https://context7.com**\n2. Copy your API key (or use dashboard token)\n3. Run: `/context7-key c7_xxxxxxxxxxxx`\n\nBroCode will save it permanently to `~/.config/brocode/config.json` for zero-latency, verified docs resolution!")
			}
			return m, nil
		}

		lower := strings.ToLower(arg)
		if lower == "clear" || lower == "reset" || lower == "delete" || lower == "remove" {
			_ = provider.SaveContext7Key("")
			m.appendNote("CONTEXT7:\n🧹 **Context7 API Key Cleared & Removed!**\n\nBroCode documentation lookup will fall back to Web Search.")
			return m, nil
		}

		if err := provider.SaveContext7Key(arg); err != nil {
			m.appendNote(fmt.Sprintf("CONTEXT7:\n❌ Failed to save Context7 API key: %v", err))
			return m, nil
		}
		m.appendNote("CONTEXT7:\n✅ **Context7 API Key Configured Successfully!**\n\n• **Mode**: Native High-Speed REST Client (Zero Node.js overhead)\n• **Status**: Active & Persisted to `~/.config/brocode/config.json`\n• **Tool**: `doc_lookup` (Automatic 3-Tier Docs Cascade)\n\nBroCode is now ready to query official documentation directly!")
		return m, nil

	case "/agents":
		cwd, _ := os.Getwd()
		loader := agent.NewLoader(cwd)
		list := loader.All()
		if len(list) == 0 {
			m.appendNote("AGENTS:\nNo custom agents found.\n\nCreate custom agents in `.brocode/agents/*.md` (project) or `~/.config/brocode/agents/*.md` (global).\n\nExample file `.brocode/agents/auditor.md`:\n```markdown\n---\nname: auditor\ndescription: Security Auditor\nmode: PLANNER\ntools:\n  allow: [read_file, grep, code_locate]\n---\nAudit security and code quality...\n```")
			return m, nil
		}
		var sb strings.Builder
		for _, ag := range list {
			src := "global (~/.config/brocode/agents)"
			if ag.IsProject {
				src = "project (.brocode/agents)"
			}
			active := ""
			if m.activeAgent != nil && strings.EqualFold(m.activeAgent.Name, ag.Name) {
				active = " 🟢 [ACTIVE]"
			}
			fmt.Fprintf(&sb, "• **%s**%s [%s]\n  %s\n  Mode: %s | Source: %s\n\n",
				ag.Name, active, ag.Description, truncatePrompt(ag.Prompt), ag.Mode, src)
		}
		sb.WriteString("👉 Activate an agent with `/agent <name>` (or `/agent reset` to return to default).")
		m.appendNote("AGENTS:\n" + sb.String())
		return m, nil

	case "/agent":
		agentName := strings.TrimSpace(strings.TrimPrefix(cmd, "/agent"))
		if agentName == "" {
			if m.activeAgent != nil {
				m.appendNote(fmt.Sprintf("🟢 Active Custom Agent: **%s** (%s)\nMode: %s\nPath: %s\n\nType `/agent reset` to deactivate.",
					m.activeAgent.Name, m.activeAgent.Description, m.activeAgent.Mode, m.activeAgent.Path))
			} else {
				m.appendNote("No custom agent currently active.\n\nUsage: `/agent <name>` or `/agent reset`\nList all available agents with `/agents`.")
			}
			return m, nil
		}
		if agentName == "reset" || agentName == "clear" || agentName == "off" || agentName == "none" {
			m.activeAgent = nil
			m.rebuildEngine()
			m.appendNote("⚪ Custom agent deactivated. Reverted to standard " + m.mode + " mode.")
			return m, nil
		}

		cwd, _ := os.Getwd()
		if m.agentLoader == nil {
			m.agentLoader = agent.NewLoader(cwd)
		}
		targetAg := m.agentLoader.Find(agentName)
		if targetAg == nil {
			m.appendNote(fmt.Sprintf("❌ Custom agent %q not found.\n\nType `/agents` to view all available custom agents in project and global locations.", agentName))
			return m, nil
		}

		m.activeAgent = targetAg
		if targetAg.Mode != "" {
			m.mode = targetAg.Mode
		}
		m.rebuildEngine()
		m.appendNote(fmt.Sprintf("🟢 Switched to Custom Agent: **%s**\nDescription: %s\nMode: %s\nDirectives: Loaded from `%s`",
			targetAg.Name, targetAg.Description, targetAg.Mode, targetAg.Path))
		return m, nil

	case "/lsp":
		m.appendNote("LSP:\n" + m.lspStatus())

	case "/diagnose":
		if m.lspMgr == nil {
			m.appendNote("LSP:\n⚠️ LSP not initialized.")
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
				return diagnoseResultMsg("LSP:\n❌ Diagnose failed: " + err.Error())
			}
			if fixMode {
				return diagnoseFixMsg(out)
			}
			out += "\n\n💡 Type `/diagnose fix` for BroCode to automatically fix all warnings/errors above."
			return diagnoseResultMsg("DIAGNOSE:\n" + out)
		})

	case "/lsp-install":
		if m.lspMgr == nil {
			m.appendNote("LSP:\n⚠️ Language Server Protocol (LSP) manager is not initialized.")
			return m, nil
		}
		lang := ""
		if len(parts) > 1 {
			lang = parts[1]
		}
		hints := m.lspMgr.InstallHints()
		if lang != "" {
			if _, ok := hints[lang]; !ok {
				m.appendNote("LSP:\n⚠️ No install needed for `" + lang + "` (already installed or unknown language).")
				return m, nil
			}
			hints = map[string]string{lang: hints[lang]}
		}
		if len(hints) == 0 {
			m.appendNote("LSP:\n✅ All language servers are already installed and active.")
			return m, nil
		}
		var sb strings.Builder
		sb.WriteString("⬇️ **Installing language servers...**\n\n")
		for l, c := range hints {
			sb.WriteString(fmt.Sprintf("- **%s**: `%s`\n", l, c))
		}
		m.appendNote("LSP:\n" + sb.String())
		m.status = "Installing language servers..."
		m.turnStart = time.Now()
		lsp := m.lspMgr
		return m, tea.Batch(tickCmd(), func() tea.Msg {
			return diagnoseResultMsg("LSP:\n" + runLSPInstalls(lsp, lang))
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
				m.appendNote("ERROR: ❌ Failed to list sessions: " + err.Error())
			}
		} else {
			m.appendNote("ERROR: ⚠️ Session store not initialized.")
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
		m.messages = []string{fmt.Sprintf("MODE:BUILDER\n✅ **Started fresh session:** `%s`\n\nActive chat context has been reset to zero.", newSessID)}

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
		m.messages = []string{"MODE:BUILDER\n⚡ **Chat history cleared.** Ready for next prompt."}

	case "/workspace", "/repos":
		cwd, _ := os.Getwd()
		ws := repo.DiscoverWorkspace(cwd)
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("### 📦 Workspace Root: `%s`\n\n", ws.RootPath))
		if len(ws.Repos) == 0 {
			sb.WriteString("No repositories detected in workspace.\n")
		} else {
			sb.WriteString(fmt.Sprintf("Found **%d repository/repositories**:\n\n", len(ws.Repos)))
			for _, r := range ws.Repos {
				gitBadge := "git"
				if !r.IsGit {
					gitBadge = "non-git"
				}
				sb.WriteString(fmt.Sprintf("- **%s** `[%s]` — `%s`\n", r.Name, gitBadge, r.Path))
			}
		}
		sb.WriteString("\n*Tips:* Subagents and tools can target specific repos using `target_dir: \"<repo_name>\"`.")
		m.appendNote("WORKSPACE:\n" + sb.String())

	case "/undo":
		count := tool.RestoreAllSnapshots()
		if count > 0 {
			m.appendNote(fmt.Sprintf("UNDO:\n↩️ Successfully restored %d file(s) back to pre-turn snapshot.", count))
		} else {
			m.appendNote("UNDO:\n⚠️ No live snapshots available to roll back (no files were modified in the active turn).")
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
				m.appendNote(fmt.Sprintf("MODE:%s\n✅ Model switched to `%s`", m.mode, m.activeModel))
				m.rebuildEngine()
			}
		} else {
			m.appendNote("MODE:" + m.mode + "\n**Usage:** `/model <provider>/<model>` or `/model <model_name>`\n\n**Examples:**\n- `/model openai/gpt-4o`\n- `/model gemini/gemini-2.5-pro`\n- `/model claude-3-5-sonnet`")
		}

	case "/report":
		if m.context == nil || m.context.Store() == nil {
			m.appendNote("REPORT:\n⚠️ Session store is not initialized.")
			return m, nil
		}
		r, err := report.Build(m.context.Store(), m.context.SessionID())
		if err != nil {
			m.appendNote(fmt.Sprintf("REPORT:\n⚠️ Failed to build session report: %v", err))
			return m, nil
		}
		if len(parts) > 1 && (parts[1] == "--json" || parts[1] == "-j" || parts[1] == "json" || parts[1] == "export") {
			jsonData, err := r.RenderJSON()
			if err != nil {
				m.appendNote(fmt.Sprintf("REPORT:\n⚠️ Failed to format JSON report: %v", err))
				return m, nil
			}
			outPath := "report.json"
			if len(parts) > 2 {
				outPath = parts[2]
			}
			if err := os.WriteFile(outPath, []byte(jsonData), 0o644); err != nil {
				m.appendNote(fmt.Sprintf("REPORT:\n⚠️ Failed to write %s: %v", outPath, err))
			} else {
				m.appendNote(fmt.Sprintf("REPORT:\n📊 Privacy-safe session report exported to `%s` (%d bytes).\n\nReady to share with community / devs for benchmarking & optimization!", outPath, len(jsonData)))
			}
			return m, nil
		}
		m.appendNote("REPORT:\n" + r.RenderMarkdown())
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
				wasAtBottom := m.logViewport.AtBottom()
				m.logViewport.SetContent(log)
				if key != m.renderedKey || m.renderedH == 0 {
					// If the user has scrolled up to read history while the agent is running,
					// preserve the user's scroll position instead of snapping back to bottom.
					if wasAtBottom || !m.turnRunning {
						m.parkLogAfterNewContent(log, vpHeight, contentWidth)
					}
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
		streamMode := m.turnMode
		if streamMode == "" {
			streamMode = m.mode
		}
		if streamMode != "" {
			badgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("232")).Bold(true).Padding(0, 1)
			switch streamMode {
			case "PLANNER":
				badgeStyle = badgeStyle.Background(lipgloss.Color("141"))
			case "MINER":
				badgeStyle = badgeStyle.Background(lipgloss.Color("42"))
			default:
				badgeStyle = badgeStyle.Background(lipgloss.Color("205"))
			}
			label += "  " + badgeStyle.Render(streamMode)
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
		sb.WriteString(spinnerStyle.Render(frame) + "  " + normalizeEmojiSpacing(m.status) + elapsed + "\n")
		actStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
		for _, act := range m.activity {
			sb.WriteString(actStyle.Render("  · "+normalizeEmojiSpacing(act)) + "\n")
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n\n")
	}

	// Queued prompts: shown live above the input, never as history rows. In
	// queue mode (Ctrl+K / Alt+K) the selected row is highlighted and a hint names the
	// management keys (e edit · d delete · m mode · ↑/↓ select · Shift+↑/↓ reorder · Esc exit).
	if len(m.pendingQueue) > 0 {
		qHead := lipgloss.NewStyle().Foreground(lipgloss.Color("178")).Bold(true).Render(fmt.Sprintf("⏳ PROMPT QUEUE (%d)", len(m.pendingQueue)))
		if m.queueMode {
			qHead += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("  · e edit · d delete · m mode · ↑/↓ select · Shift+↑/↓ reorder · Esc exit")
		} else {
			qHead += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("  · Ctrl+K / Alt+K manage")
		}
		sb.WriteString(qHead + "\n")
		for i, q := range m.pendingQueue {
			badge := modeBadgeMini(q.Mode)
			row := fmt.Sprintf("  %d · %s %s", i+1, badge, truncatePrompt(q.Text))
			if m.queueMode && i == m.queueSel {
				sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true).Render("▸ "+row) + "\n")
			} else {
				sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(row) + "\n")
			}
		}
		sb.WriteString("\n")
	}

	// Interactive Senior Recommendations:
	if len(m.activeRecommendations) > 0 && !m.showModels && !m.showSessions && !m.showConnect && !m.showDebug && !m.showMCP && !m.showAsk && !m.showFileConfirm {
		if recBar := RenderRecommendationsBar(m.activeRecommendations, m.width); recBar != "" {
			sb.WriteString(recBar + "\n\n")
		}
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

	// Live Web Search provider badge (supports single and multi-tier)
	searchBadge := provider.GetSearchProviderStatus().Badge

	// Lead icon: dynamic animated loader while busy/running, fire emoji when ready
	leadIcon := "🔥"
	if m.turnRunning || (m.status != "Ready" && m.status != "Failed") {
		leadIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true).Render(spinnerFrames[m.spinnerIdx%len(spinnerFrames)])
	}

	footerBanner := fmt.Sprintf("%s %s · P:%s · M:%s · S:%s · %s%s%s",
		leadIcon, modeBadgeStyle.Render(m.mode), m.activeProvider.Info.Name, m.activeModel, sessID, tokenStyle.Render(tokensStr), lspBadge, searchBadge)

	helpStr := " ENTER send · Alt+Enter newline · Tab mode · ↑/↓ history · PgUp/PgDn scroll · Ctrl+P pager · Ctrl+Y copy · Ctrl+M mouse · /help "
	if m.width >= 120 {
		helpStr = " ENTER send · Alt+Enter newline · Tab/Shift+Tab mode · ↑/↓ history · PgUp/PgDn scroll · Ctrl+P pager · Ctrl+Y copy · Ctrl+M mouse mode · /sessions · /models · /lsp · /help "
	} else if m.width >= 90 {
		helpStr = " ENTER send · Alt+Enter newline · Tab mode · ↑/↓ history · Ctrl+P pager · Ctrl+Y copy · Ctrl+M mouse · /sessions · /models · /help "
	}
	// When prompts are queued, advertise the queue-management key.
	if len(m.pendingQueue) > 0 && m.width >= 120 {
		helpStr += " · Ctrl+K / Alt+K queue "
	}
	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	if m.width > 0 {
		helpStyle = helpStyle.MaxWidth(m.width)
	}

	sb.WriteString(bannerStyle.Render(footerBanner) + "\n")
	sb.WriteString(helpStyle.Render(helpStr))

	s := sb.String()
	return s, strings.Count(s, "\n")
}

func modeBadgeMini(mode string) string {
	switch mode {
	case "PLANNER":
		return lipgloss.NewStyle().Background(lipgloss.Color("141")).Foreground(lipgloss.Color("0")).Bold(true).Render(" PLAN ")
	case "MINER":
		return lipgloss.NewStyle().Background(lipgloss.Color("42")).Foreground(lipgloss.Color("0")).Bold(true).Render(" MINE ")
	default:
		return lipgloss.NewStyle().Background(lipgloss.Color("205")).Foreground(lipgloss.Color("0")).Bold(true).Render(" BUILD ")
	}
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

	if strings.HasPrefix(msg, "CMD:") {
		line, content, _ := strings.Cut(msg, "\n")
		cmdName := strings.TrimPrefix(line, "CMD:")

		color := "86" // default cyan
		switch cmdName {
		case "/spec":
			color = "141" // purple
		case "/tournament":
			color = "220" // gold
		case "/ask":
			color = "39" // bright sky blue
		}

		cmdLabelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true)
		cmdBarStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color(color)).Padding(1, 2)
		if width > 0 {
			cmdBarStyle = cmdBarStyle.Width(width)
		}
		return cmdBarStyle.Render(cmdLabelStyle.Render("YOU ("+cmdName+")") + "\n" + content)
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

	// Ephemeral Codebase QA (/ask):
	if strings.HasPrefix(msg, "ASK:\n") {
		body := strings.TrimPrefix(msg, "ASK:\n")
		query, answer, _ := strings.Cut(body, "\n---\n")
		askCardStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("86")).Padding(0, 1)
		if width > 0 {
			askCardStyle = askCardStyle.Width(width)
		}
		labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
		qStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Bold(true)
		dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
		wrap := width - 6
		if wrap < 30 {
			wrap = 30
		}
		renderedAnswer := renderMarkdown(strings.TrimSpace(answer), wrap)
		header := labelStyle.Render("💬 CODEBASE QA") + "  " + dimStyle.Render("(Ephemeral · Zero Context Pollution)")
		qLine := qStyle.Render("❓ \"" + query + "\"")
		return askCardStyle.Render(header + "\n" + qLine + "\n\n" + renderedAnswer)
	}

	// Architectural Blueprint Spec (/spec):
	if strings.HasPrefix(msg, "SPEC:\n") {
		body := strings.TrimPrefix(msg, "SPEC:\n")
		specPath, specContent, _ := strings.Cut(body, "\n---\n")
		return renderBorderedCard("📋 ARCHITECTURAL BLUEPRINT CONTRACT", "("+specPath+")", specContent, "💡 Next: Switch to BUILDER (Shift+Tab) and say 'Implement spec in "+specPath+"'", "141", width)
	}

	// Multi-Candidate Tournament (/tournament):
	if strings.HasPrefix(msg, "TOURNAMENT:\n") {
		body := strings.TrimPrefix(msg, "TOURNAMENT:\n")
		task, content, _ := strings.Cut(body, "\n---\n")
		return renderBorderedCard("🏆 MULTI-CANDIDATE TOURNAMENT", "(\""+truncatePrompt(task)+"\")", content, "", "220", width)
	}

	// Active Execution Plan (/plan):
	if strings.HasPrefix(msg, "PLAN:\n") {
		content := strings.TrimPrefix(msg, "PLAN:\n")
		footer := "💡 Next: Switch to BUILDER (Shift+Tab) to execute or type `/plan archive` to clear."
		if strings.Contains(content, "Plan archived successfully!") || strings.Contains(content, "No active plan found") {
			footer = ""
		}
		return renderBorderedCard("📋 EXECUTION PLAN & ROADMAP", "(.brocode/current_plan.md)", content, footer, "81", width)
	}

	// Commands & Cheatsheet (/help):
	if strings.HasPrefix(msg, "HELP:\n") {
		content := strings.TrimPrefix(msg, "HELP:\n")
		return renderBorderedCard("📖 BROCODE CLI CHEATSHEET & SHORTCUTS", "(Commands & Keybindings)", content, "", "214", width)
	}

	// Cross-Session Project Memory (/memory):
	if strings.HasPrefix(msg, "MEMORY:\n") {
		content := strings.TrimPrefix(msg, "MEMORY:\n")
		return renderBorderedCard("🧠 PROJECT MEMORY", "(.brocode/memory.md)", content, "", "177", width)
	}

	// Token Economy & Spend Radar (/cost):
	if strings.HasPrefix(msg, "COST:\n") {
		content := strings.TrimPrefix(msg, "COST:\n")
		return renderBorderedCard("📊 TOKEN ECONOMY & COST RADAR", "(Spend Telemetry)", content, "", "42", width)
	}

	// Multi-Repo Workspace (/workspace):
	if strings.HasPrefix(msg, "WORKSPACE:\n") {
		content := strings.TrimPrefix(msg, "WORKSPACE:\n")
		return renderBorderedCard("📦 MULTI-REPO WORKSPACE", "(Discovered Repos)", content, "", "208", width)
	}

	// LSP Intelligence (/lsp):
	if strings.HasPrefix(msg, "LSP:\n") {
		content := strings.TrimPrefix(msg, "LSP:\n")
		return renderBorderedCard("⚡ LANGUAGE SERVER PROTOCOL (LSP)", "(Code Intelligence)", content, "", "39", width)
	}

	// Diagnostics (/diagnose):
	if strings.HasPrefix(msg, "DIAGNOSE:\n") {
		content := strings.TrimPrefix(msg, "DIAGNOSE:\n")
		return renderBorderedCard("🩺 CODEBASE DIAGNOSTICS", "(Diagnostics & Warnings)", content, "", "226", width)
	}

	// Benchmark & Activity Report (/report):
	if strings.HasPrefix(msg, "REPORT:\n") {
		content := strings.TrimPrefix(msg, "REPORT:\n")
		return renderBorderedCard("📊 SESSION ACTIVITY & BENCHMARK REPORT", "(/report --json to export)", content, "", "37", width)
	}

	// Time-Travel Rollback (/undo):
	if strings.HasPrefix(msg, "UNDO:\n") {
		content := strings.TrimPrefix(msg, "UNDO:\n")
		return renderBorderedCard("↩️ TIME-TRAVEL SHADOW ROLLBACK", "(Reverted File Edits)", content, "", "208", width)
	}

	// Web Search Configuration (/search-key, /search):
	if strings.HasPrefix(msg, "SEARCH:\n") {
		content := strings.TrimPrefix(msg, "SEARCH:\n")
		return renderBorderedCard("🌐 WEB SEARCH ENGINE", "(Research & Documentation)", content, "", "33", width)
	}

	// Context7 Documentation Engine (/context7-key, /context7):
	if strings.HasPrefix(msg, "CONTEXT7:\n") {
		content := strings.TrimPrefix(msg, "CONTEXT7:\n")
		return renderBorderedCard("📚 CONTEXT7 & DOCS RESOLVER", "(Native REST API)", content, "", "141", width)
	}

	// Git Worktree Sandbox (/worktree):
	if strings.HasPrefix(msg, "WORKTREE:\n") {
		content := strings.TrimPrefix(msg, "WORKTREE:\n")
		return renderBorderedCard("🌿 GIT WORKTREE SANDBOX", "(Isolated Agent Workspaces)", content, "", "70", width)
	}

	// Custom Agents & Modes (/agents):
	if strings.HasPrefix(msg, "AGENTS:\n") {
		content := strings.TrimPrefix(msg, "AGENTS:\n")
		return renderBorderedCard("🤖 CUSTOM AGENTS & MODES", "(.brocode/agents/*.md)", content, "", "99", width)
	}

	// Pipeline Doctor (/repair):
	if strings.HasPrefix(msg, "REPAIR:\n") {
		content := strings.TrimPrefix(msg, "REPAIR:\n")
		return renderBorderedCard("🩺 PIPELINE DOCTOR & SELF-REPAIR", "(Build & Test Fixer)", content, "", "196", width)
	}

	// Autonomous Self-Updater (/update):
	if strings.HasPrefix(msg, "UPDATE:\n") {
		content := strings.TrimPrefix(msg, "UPDATE:\n")
		return renderBorderedCard("🚀 BROCODE AUTO-UPDATER", "(Release Channel)", content, "", "86", width)
	}

	// Mode Switch Alert:
	if strings.HasPrefix(msg, "MODE:") {
		line, content, _ := strings.Cut(msg, "\n")
		targetMode := strings.TrimPrefix(line, "MODE:")
		return renderBorderedCard("🔀 MODE ACTIVATED: "+targetMode, "(Shift+Tab to toggle)", content, "", "86", width)
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

// renderBorderedCard formats structured cards (plans, specs, help, diagnostics, reports, etc.)
// with a consistent thick left border, title header, optional badge/subtitle, and optional footer.
func renderBorderedCard(title, subtitle, body, footer, colorCode string, width int) string {
	cardStyle := lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color(colorCode)).Padding(0, 1)
	if width > 0 {
		cardStyle = cardStyle.Width(width)
	}
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colorCode)).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	wrap := width - 6
	if wrap < 30 {
		wrap = 30
	}
	renderedBody := renderMarkdown(strings.TrimSpace(body), wrap)

	header := labelStyle.Render(title)
	if subtitle != "" {
		header += "  " + dimStyle.Render(subtitle)
	}

	res := header + "\n\n" + renderedBody
	if footer != "" {
		res += "\n\n" + dimStyle.Render(footer)
	}
	return cardStyle.Render(res)
}

// resolveTournamentSelection detects if a prompt is applying a candidate from a
// recent tournament (e.g. "Apply Beta") and enriches the execution prompt with
// that candidate's exact root cause analysis, target files, and proposed patch.
func resolveTournamentSelection(query string, messages []string) string {
	q := strings.TrimSpace(strings.ToLower(query))
	target := ""
	if strings.Contains(q, "apply alpha") || strings.Contains(q, "pilih alpha") || strings.Contains(q, "terapkan alpha") {
		target = "Candidate-Alpha"
	} else if strings.Contains(q, "apply beta") || strings.Contains(q, "pilih beta") || strings.Contains(q, "terapkan beta") {
		target = "Candidate-Beta"
	}
	if target == "" {
		return ""
	}

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if strings.HasPrefix(msg, "TOURNAMENT:\n") {
			body := strings.TrimPrefix(msg, "TOURNAMENT:\n")
			startMarker := "### 🥊 " + target
			startIdx := strings.Index(body, startMarker)
			if startIdx >= 0 {
				candidateSection := body[startIdx:]
				if endIdx := strings.Index(candidateSection, "### ⚖️"); endIdx >= 0 {
					candidateSection = candidateSection[:endIdx]
				} else if nextCand := strings.Index(candidateSection, "### 🥊"); nextCand > 0 {
					candidateSection = candidateSection[:nextCand]
				}
				candidateSection = strings.TrimSpace(candidateSection)
				return fmt.Sprintf(
					"Goal: Execute %s's verified patch and fix from the tournament.\n\n"+
						"Language: Automatically detect and mirror the user's conversation language in your explanations and final summary (e.g. Bahasa Indonesia, English).\n\n"+
						"Verified Analysis & Proposal from %s:\n"+
						"%s\n\n"+
						"Action Required:\n"+
						"1. Locate and inspect the target file(s) and specific line(s) specified in the proposal above.\n"+
						"2. Apply the fix using edit_file (or write_file).\n"+
						"3. Summarize the changes made clearly in the user's language.",
					target, target, candidateSection,
				)
			}
		}
	}
	return ""
}

// extractRecentSessionContext extracts concise context from recent turns so isolated subagents
// (like /ask) understand contextual references (e.g. "masalah tadi", "before after fix ini").
func extractRecentSessionContext(messages []string, maxCount int) string {
	if len(messages) == 0 {
		return ""
	}
	var snippets []string
	count := 0
	for i := len(messages) - 1; i >= 0 && count < maxCount; i-- {
		msg := messages[i]
		if strings.HasPrefix(msg, "YOU:\n") {
			body := strings.TrimPrefix(msg, "YOU:\n")
			snippets = append([]string{"- User: " + truncatePrompt(body)}, snippets...)
			count++
		} else if strings.HasPrefix(msg, "BROCODE:") {
			parts := strings.SplitN(msg, "\n", 2)
			if len(parts) > 1 {
				snippets = append([]string{"- Assistant: " + truncatePrompt(parts[1])}, snippets...)
				count++
			}
		} else if strings.HasPrefix(msg, "TOURNAMENT:\n") {
			parts := strings.SplitN(msg, "\n", 2)
			if len(parts) > 1 {
				snippets = append([]string{"- Tournament Task: " + truncatePrompt(parts[1])}, snippets...)
				count++
			}
		} else if strings.HasPrefix(msg, "CMD:/") {
			snippets = append([]string{"- Command: " + truncatePrompt(msg)}, snippets...)
			count++
		}
	}
	if len(snippets) == 0 {
		return ""
	}
	return "Active Conversation Context (for resolving references like 'itu', 'masalah tadi', or recent fixes):\n" + strings.Join(snippets, "\n") + "\n\n"
}
