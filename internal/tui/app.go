// Package tui is the bro-code chat UI, built on Bubble Tea v2 (charm.land).
// Layout follows the coding-gent TUI convention (Claude Code / opencode):
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
	maxHistory  = 40
	maxActivity = 15
	maxTrace    = 30
	maxReplyLen = 10000

	// Context compaction (doctrine P4: fire well before the hard limit, never
	// wait for 80%+ — Claude Code's 83.5% trigger is proven destructive; but
	// never so early that fresh tactical data is destroyed).
	compactTriggerPct = 0.70    // % of window that triggers compaction
	compactTailPct    = 0.25    // verbatim tail budget share of the window
	compactMinMsgs    = 8       // never compact a small session
	compactMinTail    = 4       // keep at least this many recent messages verbatim
	ledgerMaxRunes    = 2000    // bounded ledger — summarizers self-bound (P4)
	maxAttachedRunes  = 100_000 // attached file content cap — fits the 131k window with history (P1)
	panelWidth        = 32
	panelMinW         = 95 // status panel only on wide terminals
	streamFPS         = 30
	streamChunk       = 8 // chars revealed per streaming tick
	agentLatency      = 400 * time.Millisecond
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
	text      string   // always-visible text
	trace     []string // process trace lines for this turn (● Read, ● Edit, diffs)
	summary   string   // collapsible: one-line label shown while collapsed
	content   string   // collapsible: full block shown while expanded
	collapsed bool     // collapsible: current state (hidden by default)
}

// collapsible reports whether the message has a hidden block.
func (cm chatMsg) collapsible() bool { return cm.content != "" }

// agentResultMsg carries the agent's reply plus the real token usage the
// provider reported, so the header/panel can show actual numbers (Principle
// 3) instead of the estimate. run tags which agent invocation produced it:
// a result from an interrupted or superseded run is dropped in Update.
type agentResultMsg struct {
	reply  mockReply
	tokens tokenUsage
	run    int
}

// tokenUsage holds real-time token counts parsed from opencode JSON output.
type tokenUsage struct {
	input      int     // input tokens
	output     int     // output tokens
	reasoning  int     // reasoning/thinking tokens
	cacheRead  int     // cache read tokens
	cacheWrite int     // cache write tokens
	total      int     // total tokens
	cost       float64 // cost in dollars
}

// streamTickMsg reveals the next chunk of the streaming reply.
type streamTickMsg struct{}

// pmCache caches the rendered view of one chat message so refreshChat() can
// reuse unchanged messages instead of re-rendering the whole bounded history
// on every streaming tick (anti-lag rule: never full redraws per token). The
// cache mirrors m.chat 1:1 (bounded at maxHistory by construction) and is
// reconciled on every refresh: if a slot's fields differ from the message it
// now maps to, it is re-rendered and stored.
type pmCache struct {
	role      role
	text      string
	summary   string
	content   string
	collapsed bool
	view      string // cached renderChatMsg output
}

// matches reports whether the cache slot still describes cm — same role and
// every render-affecting field. Go string comparison is length-checked first,
// so an unchanged message compares in O(1) and only the streaming tail (whose
// text grows every tick) actually re-renders.
func (c pmCache) matches(cm chatMsg) bool {
	return c.role == cm.role && c.collapsed == cm.collapsed &&
		c.text == cm.text && c.summary == cm.summary && c.content == cm.content
}

// dragSelection tracks a drag-select over the chat viewport (Phase 4). Both
// points live in viewport content coordinates: y is the absolute content line
// (viewport.YOffset() + visible row), x is the display column. The highlight
// is applied in refreshChat, and release extracts the rectangle and copies it
// to the clipboard (OSC 52 + pbcopy/wl-copy/xclip).
type dragSelection struct {
	active bool
	x0, y0 int // anchor (press point)
	x1, y1 int // current point (drag point)
}

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

	opencodeDetected bool   // cached detection state (auto-selects the default provider)
	opencodeModel    string // cached free model name
	selectedModel    string // currently active model name

	// Live free-model list from the Zen gateway, fetched when /models opens
	// (not a static snapshot). Empty while loading or offline → the picker
	// falls back to openCodeFreeModels. zenModelsLoading drives the "⟳
	// fetching…" row; the picker source note reflects which list is shown.
	zenModels        []string
	zenModelsFetched time.Time
	zenModelsLoading bool

	// Real token usage from the last provider response (not estimates).
	actualTokens tokenUsage

	// Context window bookkeeping (doctrine P3/P4). ctxUsed is a calibrated
	// FORECAST of the current transcript (label "~"), recomputed on every
	// chat change; the exact numbers come from actualTokens (settlement).
	// Compaction is tiered (L0 goal pinned / L1 verbatim tail / L2 ledger)
	// and preventive — it fires in send() before the request, never after
	// the window overflows.
	ctxUsed       int // forecast tokens of the current transcript
	compactCount  int // how many times auto-compaction has run this session
	compactedMsgs int // total messages folded into ledgers this session

	modelsOpen  bool   // /models modal visible
	modelsSel   int    // selected model index
	modelsQuery string // /models live search filter

	apikeyOpen   bool            // /connect api key modal visible
	apikeyInput  textinput.Model // text input for API key
	apikeyTarget string          // which provider the key is for

	themeOpen bool // /theme picker modal visible
	themeSel  int  // selected preset index

	historyOpen bool          // /history modal visible
	historySel  int           // selected session index
	sessions    []sessionInfo // cached session list for modal

	queueOpen bool     // /queue modal visible
	queueSel  int      // selected queue item index
	queue     []string // queued user prompts while agent is busy

	suggestSel       int  // highlighted suggestion row
	suggestDismissed bool // popup hidden until the next keystroke

	promptEditOpen bool // ctrl+e long-prompt preview/edit modal visible

	promptHistory []string // history of sent user prompts
	historyIdx    int      // navigation index in history (-1 = typing current draft)
	draftInput    string   // saved draft input before navigating history
	pastedText    string   // full original content of large pasted text snippet

	mouseEnabled bool   // false = text select mode (MouseModeNone), true = wheel scroll mode (MouseModeCellMotion)
	sessionID    string // 8-char hex project session ID
	projectName  string // clean base name of project directory

	subagents []subagentState // live active subagent tasks

	input    textinput.Model
	spinner  spinner.Model
	chat     []chatMsg
	activity []activityItem

	viewport viewport.Model

	msgCache []pmCache // per-message render cache (mirrors chat, bounded maxHistory)
	logoView string    // gradient wordmark, computed once per theme (stable prefix)

	dragSel dragSelection // in-progress drag-select over the chat viewport

	agentWorking bool                  // agent processing (spinner visible)
	agentPhase   string                // live status: "thinking…", "reading files…", etc.
	agentStep    int                   // step counter: incremented on each phase change
	agentRun     int                   // monotonically increasing id of the current agent run
	agentAborted bool                  // user pressed ESC during the current run
	trace        []string              // process log for the current run (dimmed, in-chat)
	traceCh      chan agentTraceMsg    // goroutine → TUI status/trace updates
	askCh        chan agentQuestionMsg // goroutine → TUI question (1 slot)
	answerCh     chan string           // TUI → goroutine answer ("" = cancelled)
	askOpen      bool                  // a question is awaiting the user's answer
	askPrompt    string
	askOptions   []string
	askSel       int
	agentCancel  func() // aborts the in-flight agent request (ESC interrupt)
	streaming    bool   // reply being revealed
	streamBuf    string // remaining reply text
	follow       bool   // keep viewport pinned to bottom
	status       string // transient status text
}

// subagentState represents one spawned subagent worker process.
type subagentState struct {
	name     string // e.g. "finder", "reviewer", "debugger"
	task     string // short task description
	model    string // e.g. "gemini-3.6-flash" or "deepseek-v4-flash-free"
	provider string // e.g. "antigravity" or "opencode"
	status   string // "working" | "done" | "error" | "checkpoint"
}

// New creates the chat model. version/commit come from build-time ldflags.
// resume=true tries to load the last saved session (~/.brocode/sessions).
func New(ix *search.Index, version, commit string, resume bool) Model {
	ti := textinput.New()
	ti.Placeholder = "ask brocode... (try: mcp, diff, memory) or /help"
	ti.Prompt = ""
	ti.Focus()
	// Dimmed placeholder style (color 238 = very muted gray)
	ti.SetStyles(textinput.Styles{
		Focused: textinput.StyleState{
			Placeholder: lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Faint(true),
		},
		Blurred: textinput.StyleState{
			Placeholder: lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Faint(true),
		},
	})

	st := newStyles(DefaultTheme)

	// API key input (hidden by default, shown when selecting api-key providers)
	aki := textinput.New()
	aki.Placeholder = "paste your API key here"
	aki.Prompt = "> "
	aki.EchoMode = textinput.EchoPassword
	aki.EchoCharacter = '•'
	aki.SetWidth(44) // bounded so long keys scroll instead of overflowing the modal

	m := Model{
		index:        ix,
		version:      version,
		commit:       commit,
		themeName:    "default",
		styles:       st,
		input:        ti,
		apikeyInput:  aki,
		spinner:      spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(st.spinner)),
		viewport:     viewport.New(),
		panel:        gitInfo(),
		follow:       true,
		historyIdx:   -1,
		mouseEnabled: true,                                 // default: wheel scroll mode enabled (mouse wheel up/down)
		sessionID:    GetProjectSessionID(),                // 8-char hex project session ID
		projectName:  GetProjectName(),                     // clean base name of project directory
		logoView:     renderGradientLogo(logoArt, st.logo), // cached once per theme (stable prefix)
	}

	// Zero-Setup native environment initialization
	_ = EnsureGlobalSetup()

	// Auto-detect providers on startup (priority: opencode > freebuff > codebuff > antigravity)
	if detected, model := DetectOpenCode(); detected {
		m.opencodeDetected = true
		m.opencodeModel = model
		m.selectedModel = model
		m.provider = "opencode"
		m.window = modelWindowFor(m.provider, m.selectedModel)
	} else if fbDetected, _ := DetectFreebuffCredentials(); fbDetected {
		m.selectedModel = freebuffNativeModels[0]
		m.provider = "freebuff"
		m.window = modelWindowFor(m.provider, m.selectedModel)
	} else if agyDetected, agyModel := DetectAntigravity(); agyDetected {
		m.selectedModel = agyModel
		m.provider = "antigravity"
		m.window = modelWindowFor(m.provider, m.selectedModel)
	}

	cfg := LoadConfig()
	if cfg.LastProvider != "" && cfg.LastModel != "" {
		m.provider = cfg.LastProvider
		m.selectedModel = cfg.LastModel
		m.window = modelWindowFor(m.provider, m.selectedModel)
	}

	if resume {
		if msgs, err := LoadSession(); err == nil && len(msgs) > 0 {
			// The resume notice lives in the status line only — never in the
			// transcript, so repeated -c cycles don't stack notices in the
			// session file.
			m.chat = appendChat(nil, msgs...)
			m.started = true
			for _, cm := range msgs {
				if cm.role == roleUser && strings.TrimSpace(cm.text) != "" {
					m.promptHistory = append(m.promptHistory, cm.text)
				}
			}
			m.refreshCtx() // compacted ledgers in the file count toward the window
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

// SessionID returns the project session ID.
func (m Model) SessionID() string { return m.sessionID }

// ProjectName returns the clean project name.
func (m Model) ProjectName() string { return m.projectName }

func (m Model) Init() tea.Cmd {
	return m.input.Focus()
}

type oauthSuccessMsg struct {
	email string
}

// agentTraceMsg carries a live status update from the background agent
// goroutine: a phase change for the spinner line (phase) and/or a full trace
// line (line) appended to the process log shown in the chat area.
type agentTraceMsg struct {
	phase string // spinner label, e.g. "thinking…" ("" = no change)
	line  string // dimmed process line, e.g. "→ grep \"agentWorkCmd\" internal/tui/"
}

// agentQuestionMsg asks the user to choose an option or type their own
// answer. The agent goroutine sends it via askCh, then blocks on answerCh
// until the user submits (or cancels with esc). run tags which invocation
// asked, so a question left in the channel buffer by an interrupted run is
// dropped instead of opening a phantom prompt during the next run.
type agentQuestionMsg struct {
	prompt  string
	options []string
	run     int
}

// zenModelsMsg delivers the freshly fetched free-model list from the Zen
// gateway to the /models picker. zenModelsErrMsg reports a failed fetch —
// the picker then falls back to the static list and says so.
type zenModelsMsg struct{ models []string }
type zenModelsErrMsg struct{ err error }

// fetchZenModelsCmd fetches the live free-model list in the background so
// /models never blocks the UI on the network.
func fetchZenModelsCmd() tea.Cmd {
	return func() tea.Msg {
		models, err := fetchZenModels(zenModelsEndpoint)
		if err != nil {
			return zenModelsErrMsg{err: err}
		}
		return zenModelsMsg{models: models}
	}
}

// clip returns the first n runes of s, appending "…" if truncated.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// fmtTokens formats a token count for display (e.g. 131072 → "131k").
func fmtTokens(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
