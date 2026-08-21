package loop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/hooks"
	"github.com/plumpslabs/bro-code/internal/learn"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/plan"
	"github.com/plumpslabs/bro-code/internal/prompt"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/repo"
	"github.com/plumpslabs/bro-code/internal/skill"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/tokens"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// LoopState defines explicit state machine phases (§2.1).
type LoopState int

const (
	StateThinking LoopState = iota
	StateActing
	StateObserving
	StateVerifying
	StateDone
	StateBlocked
	StateFailed
)

func (s LoopState) String() string {
	switch s {
	case StateThinking:
		return "Thinking"
	case StateActing:
		return "Acting"
	case StateObserving:
		return "Observing"
	case StateVerifying:
		return "Verifying"
	case StateDone:
		return "Done"
	case StateBlocked:
		return "Blocked"
	case StateFailed:
		return "Failed"
	default:
		return "Unknown"
	}
}

// AgentTurn enforces thinking before answering (§2.2).
type AgentTurn struct {
	Reasoning string              `json:"reasoning"`
	ToolCalls []provider.ToolCall `json:"tool_calls,omitempty"`
	Answer    string              `json:"answer,omitempty"`
}

// Fallback is an alternative adapter+model pair tried when the primary
// provider fails (automatic model routing).
type Fallback struct {
	// ID is a stable provider identity (e.g. "groq", "opencode") used for
	// health tracking in the adaptive router.
	ID string
	// Protocol is the wire protocol ("anthropic" / "openai-compatible"), used
	// by the "confirm" fallback policy to ask only when the fallback is a
	// different vendor than the primary.
	Protocol string
	Adapter  provider.ProviderAdapter
	Model    string
}

// FallbackPolicy controls automatic model routing when the primary provider
// fails mid-turn.
const (
	// FallbackAuto (default): retry the primary once on transient errors, then
	// route to the next healthy fallback, skipping providers in cooldown.
	FallbackAuto = "auto"
	// FallbackConfirm asks the user before serving a fallback from a DIFFERENT
	// vendor than the primary; same-vendor fallbacks route automatically.
	FallbackConfirm = "confirm"
	// FallbackPrimaryOnly never falls back — a primary failure ends the turn
	// with an error.
	FallbackPrimaryOnly = "primary_only"
)

// defaultModelCallTimeout bounds a single LLM call. Without it a slow/free
// provider can hang the whole turn for minutes with no feedback (the engine
// sits silently inside one completeTurn call). On timeout the adaptive router
// falls back to the next healthy model instead of stalling.
const defaultModelCallTimeout = 120 * time.Second

// ScoutDrainer delivers completed background research findings. Implemented by
// *subagent.ScoutManager; defined here as an interface to avoid an import
// cycle (subagent imports loop for its isolated sub-loops).
type ScoutDrainer interface {
	// Drain returns one formatted report per finished job and removes them.
	Drain() []string
	// Pending returns the number of jobs still running.
	Pending() int
}

// Engine orchestrates the ReAct loop and verification ladder.
type Engine struct {
	adapter           provider.ProviderAdapter
	tools             *tool.Registry
	context           *bcontext.Manager
	model             string
	// compactModel optionally routes the (frequent, low-stakes) compaction
	// summarization to a cheaper/faster model than the main synthesis model.
	// Empty = use e.model. This is the model-routing lever: exploration and
	// housekeeping should not burn the premium model's tokens.
	compactModel      string
	// toolDescBudget caps each tool's description length sent in the request
	// (0 = no trimming). Trimming verbose tool schemas shrinks the per-turn
	// system payload so more of the window is spent on real task context.
	toolDescBudget    int
	mode              string // "BUILDER" or "PLANNER"
	maxIterations     int
	baseMaxIterations int
	// costUSD accumulates the turn's estimated spend (USD) from provider usage
	// reports × list price. budgetUSD is a hard per-task cap: when the turn
	// exceeds it, the loop stops synthesizing a final answer (no extension
	// prompt). 0 disables cost accounting enforcement (but accumulation still
	// runs when a budget is set, and CostUSD always returns the total).
	costUSD         float64
	budgetUSD       float64
	// turnTokens accumulates the raw token count of this turn's completions
	// (for the per-turn HUD, P2 #3). lastChangeEmit tracks how many file
	// changes have already been surfaced to the activity slot this turn, so
	// the real-time diff (P2 #2) emits only on a NEW change (not every round).
	turnTokens int
	// turnProductiveTokens counts completion tokens from rounds that produced
	// the deliverable: a final answer, or a round that executed a file-mutating
	// tool (write/edit/create/delete). The complement of turnTokens is overhead
	// (exploration that changed nothing). Together they form the Productive
	// Token Ratio — BroCode's north-star efficiency metric.
	turnProductiveTokens int
	// lastRoundOutput holds the COMPLETION (output) token count of the most
	// recent completion. The Productive Token Ratio numerator credits OUTPUT
	// tokens of rounds that produced a deliverable (answer or file mutation) —
	// not the round's total — so re-sent context / system-prompt tax (input
	// tokens) is never counted as "work" (the metric must not be gamed; see
	// docs/PHILOSOPHY.md north-star metric). The denominator (turnTokens) still
	// uses each request's full TotalTokens, including compacted/re-sent history.
	lastRoundOutput int
	// turnCompactions counts how many context compactions fired during this
	// turn (delta of the manager's session counter, captured at turn start).
	// Surfaced alongside tokens so a token spike is explainable: a turn that
	// compacted twice consumed tokens to summarize, not to explore.
	turnCompactions int
	// compactionBaseline snapshots the manager's session compaction counter at
	// turn start so TurnCompactions() can report the turn-local delta.
	compactionBaseline int
	// lastChangeEmit counts changes surfaced to the one-line activity HUD;
	// lastChangeDiffEmit is the same watermark for the live chat DIFF entries,
	// so a path that gained a change this round re-emits its cumulative diff.
	lastChangeEmit     int
	lastChangeDiffEmit int
	state           LoopState
	fallbacks       []Fallback
	streamHandler   func(delta string)
	progressHandler TurnOutputHandler
	// lspAvailable is the count of language servers reachable this session,
	// set by the UI at startup so the system prompt can tell the model whether
	// lsp_scan is usable (and steer it away from installing external linters).
	lspAvailable    int
	// preflightBlock holds diagnostics/context the engine gathered proactively at
	// turn start (pre-flight packing) so the model sees them in the FIRST prompt
	// instead of spending tool rounds discovering them. Empty unless the task
	// looks like a diagnostic/LSP-fix and an LSP server is available.
	preflightBlock string
	// preflightActive mirrors "preflightBlock != \"\"" but is set once at turn
	// start; while true, read_file on already-packed files and lsp_scan re-calls
	// are blocked (see guardPreflightRedundant) so the model uses the packed
	// windows instead of re-reading/re-scanning.
	preflightActive bool
	// preflightAutoFix holds the result of auto-fixing safe diagnostics at turn
	// start (lsp_autofix), so the model only handles the MANUAL ones and the task
	// stays small enough to finish within the tool budget instead of looping.
	preflightAutoFix string
	// repoRoot anchors relative paths from pre-flight diagnostics back to real
	// files so their code windows can be read.
	repoRoot string
	// planMode enforces a read-only PLAN pass for multi-step implementation tasks:
	// the model researches and proposes a plan, then confirms via ask_user before
	// any file is mutated. planApproved flips the session to BUILDER once approved.
	// planGateEnabled toggles the whole behavior (on by default).
	planMode         bool
	planApproved     bool
	planGateEnabled  bool
	// Adaptive routing state: primaryID/primaryProtocol identify the active
	// provider for health tracking and cross-vendor confirmation; health is the
	// per-provider circuit breaker; fallbackPolicy is the user's routing
	// preference (auto / confirm / primary_only); fallbackCount counts how many
	// turns a fallback had to serve (surfaced in the UI banner).
	primaryID       string
	primaryProtocol string
	health          *providerHealth
	fallbackPolicy  string
	fallbackCount   int
	// lastFallback records the fallback model actually used in the most recent
	// turn ("" when the primary provider served the turn). The UI surfaces it
	// persistently in the history so a turn answered by a fallback provider is
	// never mistaken for the primary one. lastFallbackReason carries the
	// primary provider's error when a fallback had to serve the turn (e.g.
	// "API error HTTP 429: …queue…") so the UI can tell the user WHY — a
	// free-tier duration/queue limit, an invalid model, an auth failure —
	// instead of silently swapping providers.
	lastFallback       string
	lastFallbackReason string
	// learner is the self-improving control layer: it observes per-turn context
	// utilization and tunes efficiency knobs (compaction trigger ratio) so the
	// agent gets smarter about its own token budget the longer it runs. Nil =
	// adaptive tuning disabled (static defaults).
	learner *learn.Learner
	// lastOverflow records whether the most recent fitMessages() call had to drop
	// context because even the system prompt alone approached the window — a hard
	// overflow signal the learner uses to compact more aggressively next time.
	lastOverflow bool
	// lastToolCall tracks the previous tool invocation within a turn so the
	// loop guard can detect the model repeating the exact same call and stop
	// it from spinning (grep the same file 3x in a row, etc.).
	lastToolCall provider.ToolCall
	// lastToolCallRepeats counts consecutive identical repetitions of
	// lastToolCall. Persisted across loop iterations (not reset each turn
	// iteration) so a model stuck re-issuing the same call is caught.
	lastToolCallRepeats int
	// turnToolCallCounts tracks per-turn invocation counts of normalized tool calls
	// to detect multi-tool cycles (e.g. A -> B -> C -> A -> B -> C).
	turnToolCallCounts map[string]int
	// toolOnlyRounds counts consecutive loop iterations where the model only
	// called tools and never produced an answer. Once it exceeds
	// maxToolOnlyRounds the loop stops so a tool-happy model cannot burn all
	// 25 iterations without ever answering.
	toolOnlyRounds int
	// exploredStalls counts consecutive tool-only rounds that examined NO new
	// file (spinning) vs rounds that discovered fresh files (progress). The
	// abort only fires on a stall — a model still discovering new files gets
	// room to finish its thinking. lastExploredTarget remembers the newest
	// explored entry (the explored list is capped and trims from the front, so
	// length cannot signal progress — the newest entry can).
	exploredStalls     int
	lastExploredTarget string
	// lastReasoning remembers the model's most recent reasoning text, so a
	// tool-budget abort can explain WHAT the agent was stuck on rather than
	// leaving the user with only a list of files.
	lastReasoning string
	// toolReminderSent guards the single "answer now" reminder injected when
	// the tool-only budget is exhausted.
	toolReminderSent bool
	// toolReminder2Sent guards the second, stronger reminder sent one round
	// after the first — with the list of files already explored so a model
	// deep in legitimate exploration can answer from what it has.
	toolReminder2Sent bool
	// explored tracks the files/directories the model has actually read or
	// searched this turn, so the budget reminders can tell it what it already
	// knows instead of just "stop calling tools".
	explored []string
	// projectCtx is a compact structural overview of the project (tree + docs)
	// injected into the system prompt so the agent starts oriented instead of
	// blind-grepping for file locations.
	projectCtx string
	// repoMap is the deterministic project map (entry points, structure, hot
	// files by usage). It renders as the repo context block in the system prompt.
	repoMap string
	// repoFiles is the full project file list (paths only) used for smart
	// scope pre-selection: ranking files by relevance to the user prompt so
	// BroCode can focus exploration. Set via SetScopeFiles.
	repoFiles []string
	// scopeHint caches the smart-scope markdown for injection into the system
	// prompt this turn. Computed at turn start from the user prompt + repoFiles.
	scopeHint string
	// skillsEntries is the installed skill catalog (name + description only).
	// The prompt builder renders it as the AVAILABLE SKILLS block, relevance-
	// filtering it once the catalog grows past the tuning threshold, and the
	// model loads the full SKILL.md itself via read_file when relevant.
	skillsEntries []prompt.SkillEntry
	// tuning is the runtime tuning surface for the system prompt: block and
	// rule toggles plus skill-catalog budgets, loaded from
	// ~/.config/brocode/tuning.json. Nil falls back to prompt.DefaultTuning.
	tuning *prompt.Tuning
	// stacks are the repo's detected languages ("go", "node", "ts", ...) with
	// their evidence files. They render a one-line STACK hint ("STACK: go
	// (go.mod, main.go)") and bias the skill catalog toward the repo's stack
	// so stack-specific skills follow the repo, not the model's guess (e.g.
	// "fix the build" in a Go repo boosts go-workflow).
	stacks []prompt.Stack
	// artifactSeq numbers this turn's spilled tool outputs (.brocode/artifacts)
	// so each gets a unique, stable path for on-demand reads. Reset per turn;
	// cleanupArtifacts wipes the dir at turn start (bounded store).
	artifactSeq int
	// loadedSkills tracks which catalog skills the model actually reached for
	// this turn (a read_file on a known SKILL.md path). Surfaced live in the
	// activity HUD and summarized at turn end, so skill usage — and MISSED
	// triggers (tools used, no skill loaded) — are visible instead of silent.
	loadedSkills map[string]bool
	// skillDirs records, per skill loaded this turn, the directory its SKILL.md
	// lives in — so the self-evolution proposal (GOTCHAS.md patch) lands next
	// to the real skill file, whatever source it was loaded from.
	skillDirs map[string]string
	// turnUsedTools records whether any tool call actually executed this turn
	// (not blocked by a guard). The turn-end skill summary uses it to decide
	// whether a skill COULD have been loaded — a pure-chat turn stays quiet.
	turnUsedTools bool
	// mem is the cross-session project memory store. When set, a warm-start
	// excerpt is injected into the system prompt and compaction summaries are
	// auto-merged into memory so future sessions start warm.
	mem *memory.Store
	// knowledge is the Smart Context Graph backend. When set, the engine queries
	// it at turn-start for relevance-ranked file hints and injects them as a
	// "SMART CONTEXT" block — helping the agent avoid re-scanning previously
	// analyzed files whose content hash hasn't changed.
	knowledge *store.Store
	// usageFn, when set, receives the files the model touched this turn (read,
	// searched, edited) so the UI can persist cross-session usage counts — the
	// "the more BroCode is used, the smarter it gets" layer.
	usageFn func(paths []string)
	// onFileEdited, when set, is called after a write/edit tool succeeds with
	// the edited file path — lets the UI refresh the session-wide symbol index
	// so code_locate stays current instead of serving a stale session-start view.
	onFileEdited func(path string)
	// onChange, when set, is called after a write/edit tool succeeds with the file
	// path and the unified diff of what changed — lets the UI render a live
	// red/green diff entry in the chat as each edit lands, not just a collapsed
	// end-of-turn summary.
	onChange func(path, diff string)
	// editedFiles tracks the paths the model wrote or edited this turn so the
	// convention checker can review them before the turn is declared done.
	editedFiles []string
	// reviewPasses counts how many times the edit-review ran this turn (Layer 1
	// deterministic + Layer 2 LLM). Capped so a model that keeps editing never
	// loops forever on review feedback.
	reviewPasses int
	// reviewLLMEnabled toggles the senior-level LLM code review (Layer 2) that
	// runs after deterministic checks pass. On by default; disabled in tests
	// and headless contexts where an extra completion would be wasteful.
	reviewLLMEnabled bool
	// diagFn optionally runs LSP diagnostics on a file (wired by the UI with
	// the LSP manager). When set, edited files get a native type-error check
	// after the convention review — no LLM needed.
	diagFn func(path string) string
	// symbolsProvider optionally returns defined symbols across the project
	// for DRY reuse checking.
	symbolsProvider func() map[string]map[string]bool
	// reproGateArmed is true when the current task looks like a bug fix (the
	// user query carried a failure signal). While armed and no repro is
	// established, write/edit/delete calls are gated behind a TSR REPRODUCE
	// reminder: the model must observe the failure first, then fix.
	reproGateArmed bool
	// reproEstablished records that the failure was actually reproduced this
	// turn — either the user pasted the error/stack trace in the prompt, or a
	// tool result showed a failing command/test. Only then are edits allowed.
	reproEstablished bool
	// reproReminderSent guards the REPRODUCE reminder against being re-emitted
	// every round once the model has seen it.
	reproReminderSent bool
	// tsrAttempts counts repair cycles this turn (verification failures fed
	// back to the model) for the typed revision contract stop condition.
	tsrAttempts int
	// lastVerifyHash is a normalized signature of the most recent verification
	// failure. Repeating the same hash across attempts means the model is
	// retrying without progress (the error persists unchanged).
	lastVerifyHash string
	// verifyErrorStreak counts consecutive identical verification failures.
	// Reaching 3 stops the repair loop instead of burning iterations.
	verifyErrorStreak int
	// lastVerifyErr remembers the last verification error text so a lesson can
	// be distilled once the repair succeeds.
	lastVerifyErr string
	// hasPassedVerification records whether verification passed earlier in this session
	// to enforce the Regression Obligation Contract (LoopsBench).
	hasPassedVerification bool
	// repairSucceeded is set when a verification that previously failed passes
	// after the model repaired the code — the trigger for lesson extraction.
	repairSucceeded bool
	// lessonFiles snapshots the files involved in a successful repair, taken
	// before the review resets editedFiles, so distillLesson can name them.
	lessonFiles []string
	// sysPromptCached is the system prompt built ONCE per turn and reused for
	// every loop iteration. Without this, the prompt was re-rendered each
	// round and — because the warm-start memory excerpt keyed off the evolving
	// "last user prompt" — the leading bytes changed on every iteration, which
	// defeats provider prompt caching (Anthropic/OpenAI cache hit = identical
	// prefix). A stable per-turn prefix means iterations 2..N re-send the same
	// leading tokens and hit the cache instead of re-billing full input.
	sysPromptCached string
	// Fuzzy loop-break state: bashFamilyStreak counts consecutive ROUNDS whose
	// bash calls share the same leading command word (e.g. round after round of
	// "go test …"). It is incremented at most once per round, and a round that
	// mixes different command families resets it. This catches same-strategy
	// spins where the arguments change (so exact-repeat detection never fires)
	// while tolerating batched same-family calls within a single round.
	lastBashFamily   string
	bashFamilyStreak int
	// iterBashFamily is the family counted for the CURRENT round ("" until the
	// round's first bash call). Guards against double-counting batched calls.
	iterBashFamily string
	// usage accumulates token + cost across the session (per model), so the
	// UI can show live cost tracking. Cost is estimated from the usage the
	// adapters report and the per-model pricing table.
	usage *UsageTracker
	// hooks runs user-defined lifecycle commands (on-turn-start, on-tool-call,
	// on-turn-end, ...) at the corresponding points in the loop. Optional.
	hooks *hooks.Manager
	// scouts runs background research tasks started with the scout tool while
	// the main turn continues. Completed results are drained into the context
	// at each loop iteration. Optional.
	scouts ScoutDrainer
	// autoExtendSession toggles autonomous turn extension for the rest of the session.
	autoExtendSession bool
	// hardCapIterations sets the absolute ceiling (default 100 turns).
	hardCapIterations int
	// askHandler optionally prompts the user when turn limit is reached.
	askHandler func(question string, options []string) (string, error)
	// exploreQuery caches the MINER's file-path context for warm-start relevance filtering.
	exploreQuery string
	// earlyExitOnError stops executing remaining tools in a round if a
	// mutating tool (write_file, edit_file, bash) fails — the model often
	// proceeds to depend on a result it just got, so a failed edit/write/bash
	// usually means downstream calls will error too. Cutting them saves ~2-5
	// rounds of error spam per failure.
	earlyExitOnError bool
}

// SetHooks wires a lifecycle hooks manager. Nil disables hooks.
func (e *Engine) SetHooks(h *hooks.Manager) {
	e.hooks = h
}

// SetEarlyExitOnError enables stopping remaining tool execution in a round
// when a mutating tool fails. This prevents the model from cascading into
// error-after-error when a prerequisite edit/build command fails. Enabled by
// default.
func (e *Engine) SetEarlyExitOnError(v bool) {
	e.earlyExitOnError = v
}

// SetScoutManager wires the background scout manager. Nil disables scout
// result delivery (the scout tool itself then reports an error).
func (e *Engine) SetScoutManager(sm ScoutDrainer) {
	e.scouts = sm
}

// SetCompactModel routes compaction summarization to a (cheaper) model. Empty
// keeps it on the main synthesis model.
func (e *Engine) SetCompactModel(m string) {
	e.compactModel = m
}

// SetLearner attaches the self-improving control layer. It immediately applies
// the learned compaction ratio so the session starts with the tuned threshold
// rather than the default, and observes each turn to keep converging.
func (e *Engine) SetLearner(l *learn.Learner) {
	e.learner = l
	if l != nil {
		bcontext.SetCompactionRatio(l.CompactionRatio())
	}
}

// learnObserve feeds the just-finished turn's real context utilization into the
// learner so the compaction threshold converges to this project's actual usage.
// Called via defer in RunTurn, so it runs on every return path. A hard overflow
// (fitMessages could not fit even the smallest request) is the strongest signal
// and is reported separately so the ratio drops faster.
func (e *Engine) learnObserve() {
	if e.learner == nil {
		return
	}
	if e.lastOverflow {
		e.learner.ObserveOverflow()
		e.lastOverflow = false
	}
	win := e.context.MaxWindow()
	if win > 0 {
		util := float64(e.context.TotalContextTokens()) / float64(win)
		e.learner.ObserveTurn(util)
	}
	// Re-apply the (possibly nudged) ratio so the next turn compacts with it.
	bcontext.SetCompactionRatio(e.learner.CompactionRatio())
	_ = e.learner.Save()
}

// LearnerStats returns the self-improving layer's status line for the HUD/debug.
func (e *Engine) LearnerStats() string {
	if e.learner == nil {
		return ""
	}
	return e.learner.Stats()
}

// SetToolDescBudget caps each tool description to n characters in the request
// (0 = send full descriptions). A lean tool surface frees window space.
func (e *Engine) SetToolDescBudget(n int) {
	if n < 0 {
		n = 0
	}
	e.toolDescBudget = n
}

// Tool-only budget: a model that keeps calling tools without answering is
// nudged EARLY (so a rabbit-hole exploration like "search the schema for more
// models" is cut before it burns a dollar of tokens) and aborted shortly
// after. A few rounds of pure tool calls is plenty for most tasks (a big
// monorepo overview, an LSP-warning sweep); the first reminder lands early so
// the agent answers instead of reading itself in circles.
const (
	// toolWarnRounds — first "stop and answer" reminder.
	toolWarnRounds = 6
	// toolFinalWarnRounds — second, firmer warning.
	toolFinalWarnRounds = 8
	// maxToolOnlyRounds — abort once a spinning model stalls. Still well below
	// the 25-iteration cap.
	maxToolOnlyRounds = 14
	// maxToolOnlyAbsolute — unconditional abort even for a model that keeps
	// discovering new files (freedom is bounded, never infinite). 16 leaves
	// room for the final-warning pattern (ONE last targeted read, then the
	// answer) to complete instead of being cut right after the read.
	maxToolOnlyAbsolute = 16
	// finalWarnHardStop — after the FINAL WARNING the model may make at most this
	// many more tool rounds before it is forced to synthesize. The warning is
	// otherwise toothless: the model kept reading "one more section" forever.
	finalWarnHardStop = 2
	// exploredWarnCap — once the agent has read this many DISTINCT files/paths in
	// a row without answering, accelerate the final warning. Reading several
	// files for a codebase task is normal exploration, but unbounded reading is capped.
	exploredWarnCap = 12
)

// CalculateAdaptiveToolBudget dynamically scales the tool budget based on
// prompt complexity, active mode, and task keywords (Fase 2.2).
// complexityTier classifies a task's effort by prompt signals so the engine
// budgets iterations proportionally — a one-line typo fix must not be granted
// the same 25-round runway as a cross-module refactor. Deterministic and free
// (no LLM): keyword + length signals, matching the spirit of rule b7
// (PROPORTIONALITY). The autonomous extension path still rescues a
// misclassified task, so a wrong tier never traps a genuinely big task.
type complexityTier int

const (
	tierSimple complexityTier = iota
	tierMedium
	tierComplex
)

// classifyTaskComplexity scores a user query into simple / medium / complex.
func classifyTaskComplexity(query string) complexityTier {
	p := strings.ToLower(strings.TrimSpace(query))
	words := len(strings.Fields(p))

	simpleHits := 0
	for _, kw := range []string{"typo", "rename", "explain", "what does", "why does", "what is", "why is", "format", "comment", "spelling", "fix the doc"} {
		if strings.Contains(p, kw) {
			simpleHits++
		}
	}
	complexHits := 0
	for _, kw := range []string{"refactor", "migrate", "migration", "audit", "architecture", "implement", "feature", "rewrite", "integrate", "add support", "end-to-end", "multi-file", "multiple files", "schema", "cross-module", "monorepo"} {
		if strings.Contains(p, kw) {
			complexHits++
		}
	}

	// Multi-part task lists (bulleted/numbered lines) signal real scope.
	multiPart := strings.Contains(p, "\n-") || strings.Contains(p, "\n*") || strings.Contains(p, "\n1.")

	switch {
	case complexHits >= 2 || (complexHits >= 1 && words >= 25) || (complexHits >= 1 && multiPart):
		return tierComplex
	case (simpleHits >= 2 && complexHits == 0) || (simpleHits >= 1 && words <= 8 && complexHits == 0):
		return tierSimple
	default:
		return tierMedium
	}
}

// iterationsForComplexity maps a task tier to its per-turn iteration budget.
// Simple tasks get a tight runway (finish fast, no ceremony); complex tasks
// keep the historical 25; the autonomous extension still adds +15 on top when
// the model proves the task needs more room.
func iterationsForComplexity(t complexityTier) int {
	switch t {
	case tierSimple:
		return 10
	case tierComplex:
		return 25
	default:
		return 16
	}
}

// explorationBudget returns the adaptive warn, final warn, and absolute caps
// based on the active mode (PLANNER/MINER vs BUILDER) and prompt complexity.
func (e *Engine) explorationBudget() (warnRounds, finalWarnRounds, maxAbsolute, exploredCap int) {
	mode := e.Mode()
	prompt := e.context.LastUserPrompt()
	tier := classifyTaskComplexity(prompt)

	if mode == "PLANNER" || mode == "MINER" {
		// Deep research & architectural discovery modes need wider runway.
		if tier == tierComplex {
			return 14, 18, 24, 25
		}
		return 10, 14, 20, 18
	}

	// BUILDER mode: keep standard coding turns snappy and focused on shipping edits.
	if tier == tierComplex {
		return 10, 14, 18, 18
	}
	return toolWarnRounds, toolFinalWarnRounds, maxToolOnlyAbsolute, exploredWarnCap
}

func CalculateAdaptiveToolBudget(prompt string, mode string) int {
	base := 16
	if mode == "MINER" || mode == "PLANNER" {
		base = 20
	}
	p := strings.ToLower(prompt)
	words := len(strings.Fields(p))
	if words > 30 || strings.Contains(p, "refactor") || strings.Contains(p, "audit") || strings.Contains(p, "migrate") || strings.Contains(p, "architecture") || strings.Contains(p, "fix") {
		base += 6
	}
	if base > 28 {
		return 28
	}
	if base < 10 {
		return 10
	}
	return base
}

// maxParallelReadOnlyTools caps how many read-only tools execute concurrently
// in one round. Read-only calls are stateless, so parallel execution cuts
// multi-read rounds from ~N×latency to ~N/cap×latency — the biggest per-turn
// speedup available without changing model behavior. The cap keeps a 20-tool
// batch from hammering the disk, the network, or the LSP server at once.
const maxParallelReadOnlyTools = 4

// isParallelReadOnly reports whether a tool is safe to execute concurrently:
// read-only, stateless, no interactive prompt, no session-affecting side
// effects. Everything else (write_file, edit_file, delete_file, bash,
// ask_user, git, undo, review_changes, memory) stays sequential so side
// effects and user prompts keep their order.
func isParallelReadOnly(name string) bool {
	switch name {
	case "read_file", "list_dir", "grep", "glob", "search_code", "fetch_url", "web_search":
		return true
	}
	return false
}

// isMutatingTool reports whether a tool has filesystem/process side effects
// that warrant early-exit-on-error (a failure here usually invalidates
// downstream calls in the same round).
func isMutatingTool(name string) bool {
	switch name {
	case "write_file", "edit_file", "delete_file", "bash", "git", "undo",
		"create_directory", "rename_file", "lsp_autofix":
		return true
	}
	return false
}

// SetProjectContext injects a compact structural overview of the project into
// every turn's system prompt (see search.BuildProjectContext). Empty disables.
func (e *Engine) SetProjectContext(pc string) {
	e.projectCtx = pc
}

// SetSkillCatalog injects the installed skill catalog (name + description
// only; the model loads each SKILL.md itself). Empty disables the skills
// block. The catalog is relevance-filtered by the prompt builder when it
// exceeds the tuning threshold.
func (e *Engine) SetSkillCatalog(entries []prompt.SkillEntry) {
	e.skillsEntries = entries
}

// SetTuning replaces the runtime tuning surface (block/rule toggles, skill
// catalog budgets). Nil keeps the defaults.
func (e *Engine) SetTuning(t *prompt.Tuning) {
	if t != nil {
		e.tuning = t
	}
}

// SetDetectedStacks wires the repo's detected languages (with evidence files)
// so the prompt builder can render a STACK hint and bias the skill-catalog
// ranking toward the repo.
func (e *Engine) SetDetectedStacks(stacks []prompt.Stack) {
	e.stacks = stacks
}

// skillForRead returns the catalog name of the skill whose SKILL.md a read_file
// call targets, or "" when the path is not a known skill file. Matching is
// suffix-based so both relative (.brocode/skills/go-workflow/SKILL.md) and
// absolute read paths resolve to the same skill.
func (e *Engine) skillForRead(path string) string {
	if len(e.skillsEntries) == 0 {
		return ""
	}
	clean := filepath.ToSlash(strings.TrimSpace(path))
	if filepath.Base(clean) != "SKILL.md" {
		return ""
	}
	for _, s := range e.skillsEntries {
		if s.Path == "" {
			continue
		}
		rel := filepath.ToSlash(s.Path)
		if strings.HasSuffix(clean, rel) || strings.HasSuffix(clean, "/"+s.Name+"/SKILL.md") {
			return s.Name
		}
	}
	return ""
}

// SetRepoMap injects the deterministic project map (entry points, structure,
// hot files by usage) into every turn's system prompt. Empty disables.
func (e *Engine) SetRepoMap(rm string) {
	e.repoMap = rm
}

// SetScopeFiles injects the full project file list for smart scope
// pre-selection: given a user prompt, ScoreFiles() ranks files by relevance
// so BroCode can focus exploration on the most likely targets instead of
// scanning the entire workspace. Empty disables.
func (e *Engine) SetScopeFiles(files []string) {
	e.repoFiles = files
}

// SetMemoryStore wires the cross-session project memory. When set, a
// warm-start excerpt of past sessions' learnings is injected into the system
// prompt, and compaction summaries are auto-merged back into memory.
func (e *Engine) SetMemoryStore(st *memory.Store) {
	e.mem = st
	// Auto-prune stale memory facts (>30 days old) to keep the memory file
	// focused on the current project state. Cheap (single file read), runs once
	// per session. Non-blocking: errors are silently ignored.
	if st != nil {
		go st.PruneStale()
	}
}

// SetKnowledgeStore wires the Smart Context Graph backend. When set, the engine
// queries it at turn-start for relevance-ranked file hints and injects them
// as a "SMART CONTEXT" block in the system prompt — helping the agent avoid
// re-scanning files it has already analyzed in prior sessions.
func (e *Engine) SetKnowledgeStore(st *store.Store) {
	e.knowledge = st
	// Best-effort prune of stale knowledge entries on session start. Cheap
	// (single indexed DELETE with WHERE) and safe to run synchronously —
	// avoids a goroutine race window during turn-one system prompt build.
	if st != nil {
		_, _ = st.PruneKnowledge()
	}
}

// SetUsageRecorder wires a callback that receives the files the model touched
// each turn (read, searched, edited) so usage can persist across sessions.
func (e *Engine) SetUsageRecorder(fn func(paths []string)) {
	e.usageFn = fn
}

// SetOnFileEdited wires a callback invoked whenever a write/edit tool succeeds,
// so the host can keep session-scoped caches (e.g. the symbol index) fresh.
func (e *Engine) SetOnFileEdited(fn func(path string)) {
	e.onFileEdited = fn
}

// SetOnChange wires a callback invoked whenever a write/edit tool succeeds, with
// the file path and its unified diff — so the host can render a live red/green
// diff entry in the chat as each edit lands.
func (e *Engine) SetOnChange(fn func(path, diff string)) {
	e.onChange = fn
}

// SetDiagnosticsChecker wires a native type-error checker (the UI provides
// one backed by the LSP manager). It runs on edited files after the
// convention review, catching type errors without waiting for a full build.
func (e *Engine) SetDiagnosticsChecker(fn func(path string) string) {
	e.diagFn = fn
}

// SetSymbolsProvider wires a global symbol provider for cross-file duplicate
// detection and DRY enforcement.
func (e *Engine) SetSymbolsProvider(fn func() map[string]map[string]bool) {
	e.symbolsProvider = fn
}

// SetLSPStatus records how many language servers are available this session.
// The system prompt reads it to decide whether lsp_scan is usable and to steer
// the model away from installing external linters when LSP is missing.
func (e *Engine) SetLSPStatus(n int) {
	e.lspAvailable = n
}

// localizeVerifyFailure enriches a CLI verification failure with LSP
// diagnostics from the edited files, so the repair loop sees a structured
// file:line view alongside the raw build output — errors the CLI buries in a
// wall of text become actionable. Best-effort: no checker wired (or a clean
// file) means no localization, never a new failure.
func (e *Engine) localizeVerifyFailure() string {
	if e.diagFn == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range e.editedFiles {
		if out := e.diagFn(p); out != "" && !strings.HasPrefix(out, "No diagnostics") {
			sb.WriteString("• ");sb.WriteString(p);sb.WriteString("\n")
			sb.WriteString(out);sb.WriteString("\n")
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// SetReviewLLM toggles the senior-level LLM code review (Layer 2) that runs
// after the deterministic checks pass. On by default; turn off where an extra
// completion per edit is not worth it (tests, cheap/headless contexts).
func (e *Engine) SetReviewLLM(on bool) {
	e.reviewLLMEnabled = on
}

// CostSummary returns the session's estimated cost report (per model + total).
func (e *Engine) CostSummary() string {
	if e.usage == nil {
		return "No LLM usage recorded yet this session."
	}
	return e.usage.Summary()
}

// SessionCostUSD returns the total estimated spend so far (for the footer).
func (e *Engine) SessionCostUSD() float64 {
	if e.usage == nil {
		return 0
	}
	return e.usage.TotalCost()
}

// AddFallback registers a fallback provider+model tried on primary failure.
func (e *Engine) AddFallback(fb Fallback) {
	e.fallbacks = append(e.fallbacks, fb)
}

// SetPrimaryIdentity tells the router which provider is the active primary and
// its wire protocol, so health tracking keys correctly and the "confirm"
// policy can distinguish cross-vendor fallbacks.
func (e *Engine) SetPrimaryIdentity(id, protocol string) {
	e.primaryID = id
	e.primaryProtocol = protocol
}

// SetFallbackPolicy sets the routing policy (FallbackAuto / FallbackConfirm /
// FallbackPrimaryOnly). The default is FallbackAuto.
func (e *Engine) SetFallbackPolicy(policy string) {
	if policy == "" {
		policy = FallbackAuto
	}
	e.fallbackPolicy = policy
}

// FallbackCount returns how many turns a fallback provider has served.
func (e *Engine) FallbackCount() int {
	return e.fallbackCount
}

// PrimaryCooldownRemaining reports how long the primary provider is currently
// cooling down from recent failures (0 when healthy). While positive, the
// router skips the primary and goes straight to a healthy fallback.
func (e *Engine) PrimaryCooldownRemaining() time.Duration {
	if e.health == nil {
		return 0
	}
	_, rem := e.health.inCooldown(e.primaryID)
	return rem
}

// SetStreamHandler wires a callback receiving content deltas while the model
// streams its answer. Nil disables streaming (adapters fall back to Complete).
func (e *Engine) SetStreamHandler(fn func(delta string)) {
	e.streamHandler = fn
}

// SetAskHandler wires an interactive question handler for turn extensions.
func (e *Engine) SetAskHandler(fn func(question string, options []string) (string, error)) {
	e.askHandler = fn
}

// NewEngine creates an agent loop engine instance.
func NewEngine(adapter provider.ProviderAdapter, tools *tool.Registry, ctxMgr *bcontext.Manager, model string) *Engine {
	if ctxMgr != nil {
		ctxMgr.SetModel(model)
	}
	return &Engine{
		adapter:           adapter,
		tools:             tools,
		context:           ctxMgr,
		model:             model,
		mode:              "BUILDER",
		maxIterations:     25,
		// baseMaxIterations 0 = unset: the per-turn reset then derives the
		// iteration budget from the task's complexity tier (simple 10 / medium
		// 16 / complex 25) instead of a flat 25. SetMaxIterations (config,
		// bench harness) raises it above 0 so an explicit cap always wins.
		baseMaxIterations: 0,
		hardCapIterations: 100,
		state:             StateThinking,
		usage:             NewUsageTracker(),
		reviewLLMEnabled:  true,
		health:            newProviderHealth(),
		fallbackPolicy:    FallbackAuto,
		repoRoot:          tools.RepoRoot(),
		planGateEnabled:   false,
		tuning:            prompt.DefaultTuning(),
		earlyExitOnError: true, // default: stop on mutating tool failure
	}
}

func (e *Engine) SetMode(m string) {
	e.mode = m
	e.applyModePolicy()
}

// applyModePolicy hard-enforces the mode at the tool executor level: PLANNER
// and MINER mark the registry read-only (mutating tools blocked everywhere,
// even from subagents), and PLANNER additionally blocks bash. This is the
// structural backstop — the prompt and the loop filters are advisory; the
// executor cannot mutate.
func (e *Engine) applyModePolicy() {
	switch e.mode {
	case "PLANNER":
		e.tools.SetExecutionPolicy(true, true)
	case "MINER":
		e.tools.SetExecutionPolicy(true, false)
	default: // BUILDER
		e.tools.SetExecutionPolicy(false, false)
	}
}

// SetMaxIterations overrides the loop iteration cap (default 25). Used by the
// benchmark harness to bound each case. Setting it also marks the cap as
// explicit (baseMaxIterations > 0), so the per-turn reset honors it instead of
// re-deriving a complexity tier.
func (e *Engine) SetMaxIterations(n int) {
	if n > 0 {
		e.maxIterations = n
		e.baseMaxIterations = n
	}
}

// SetBudgetUSD caps the turn's total estimated spend in USD. 0 (the default)
// disables the cap. Applied per user turn — the counter resets when the turn
// starts.
func (e *Engine) SetBudgetUSD(usd float64) {
	e.budgetUSD = usd
}

// CostUSD returns the accumulated estimated spend (USD) for the current turn.
func (e *Engine) CostUSD() float64 {
	return e.costUSD
}

// TurnTokens returns the raw token count consumed by this turn's completions
// (per-turn HUD, P2 #3).
func (e *Engine) TurnTokens() int {
	return e.turnTokens
}

// TurnTokenStats returns the turn's token economy: total tokens vs. the
// productive subset (answer + file-mutating rounds). The ratio is BroCode's
// north-star efficiency metric — a high ratio means the agent went straight to
// the result instead of thrashing through the codebase.
func (e *Engine) TurnTokenStats() tokens.TurnTokenStats {
	return tokens.NewTurnTokenStats(e.turnTokens, e.turnProductiveTokens)
}

// TurnCompactions returns how many context compactions fired during the most
// recent turn (delta of the session counter, so repeated turns accumulate).
func (e *Engine) TurnCompactions() int {
	return e.turnCompactions
}

func (e *Engine) Mode() string {
	if e.mode == "" {
		return "BUILDER"
	}
	return e.mode
}

// State returns current engine phase.
func (e *Engine) State() LoopState {
	return e.state
}

// RunTurn executes the ReAct loop until a terminal state is reached.
type TurnOutputHandler func(state LoopState, info string)

// RunTurnWithUsage runs a full turn and additionally returns the raw token
// count and estimated cost attributed to it. Both are available on the error
// path too, so callers (sub-agents, phase attribution) can bill partial work
// when a turn fails partway through.
func (e *Engine) RunTurnWithUsage(ctx context.Context, userQuery string, onUpdate TurnOutputHandler) (answer string, tokens int, cost float64, compactions int, err error) {
	answer, err = e.RunTurn(ctx, userQuery, onUpdate)
	e.turnCompactions = e.context.CompactCount() - e.compactionBaseline
	return answer, e.TurnTokens(), e.CostUSD(), e.TurnCompactions(), err
}

func (e *Engine) RunTurn(ctx context.Context, userQuery string, onUpdate TurnOutputHandler) (answer string, err error) {
	// Self-improving control layer: after the turn settles, observe context
	// utilization and nudge the compaction ratio toward the high-signal band.
	// Runs on every return path via defer.
	defer e.learnObserve()

	e.progressHandler = onUpdate
	// Reset the fallback marker for this turn (set when a fallback provider
	// serves the turn; "" when the primary does).
	e.lastFallback = ""
	e.lastFallbackReason = ""
	// Reset progress/stall tracking for this turn. The sentinel can never be a
	// real path, so the first iteration always registers as "new".
	e.exploredStalls = 0
	e.lastExploredTarget = "\x00"
	e.lastReasoning = ""
	// Reset the TSR contract state for this turn: the reproduce gate, repair
	// attempt budget, and identical-error streak all start fresh.
	e.reproGateArmed = false
	e.reproEstablished = false
	e.reproReminderSent = false
	e.tsrAttempts = 0
	e.lastVerifyHash = ""
	e.verifyErrorStreak = 0
	e.lastVerifyErr = ""
	e.repairSucceeded = false
	// Reset per-turn tool budget, repetition, and exploration counters
	e.toolOnlyRounds = 0
	e.toolReminderSent = false
	e.toolReminder2Sent = false
	e.exploredStalls = 0
	e.lastExploredTarget = ""
	e.lastToolCall = provider.ToolCall{}
	e.lastToolCallRepeats = 0
	e.explored = nil
	// Reset the per-turn cost budget counter.
	e.costUSD = 0
	e.turnTokens = 0
	e.turnProductiveTokens = 0
	e.lastRoundOutput = 0
	e.turnCompactions = 0
	e.compactionBaseline = e.context.CompactCount()
	e.lastChangeEmit = 0
	e.lastChangeDiffEmit = 0
	e.artifactSeq = 0
	e.loadedSkills = map[string]bool{}
	e.skillDirs = map[string]string{}
	e.turnUsedTools = false
	e.applyModePolicy()
	// Reset the turn's recorded file changes so the review complexity gate and
	// the UI summary only ever see THIS turn's edits (headless contexts like
	// the bench harness and tests have no UI ResetChanges call).
	tool.ResetChanges()
	// Prompt cache and fuzzy loop-break state are per-turn too: a new turn
	// rebuilds its own stable prefix and resets the same-family streak.
	e.sysPromptCached = ""
	e.lastBashFamily = ""
	e.bashFamilyStreak = 0
	e.iterBashFamily = ""
	e.turnToolCallCounts = make(map[string]int)
	if !e.autoExtendSession {
		if e.baseMaxIterations > 0 {
			e.maxIterations = e.baseMaxIterations
		} else {
			// Proportional effort: the iteration budget matches the task's
			// complexity tier (simple 10 / medium 16 / complex 25) instead of
			// granting every task the same 25-round runway. The autonomous
			// extension still adds +15 when the task proves bigger than its
			// tier, so a misclassification never traps a real task.
			tier := classifyTaskComplexity(userQuery)
			e.maxIterations = iterationsForComplexity(tier)
			if tier == tierSimple && onUpdate != nil {
				onUpdate(e.state, fmt.Sprintf("⚡ Simple task detected — reduced iteration budget (%d, complex tasks get 25)", e.maxIterations))
			}
		}
	}
	// Pre-flight context packing: for a diagnostic/LSP-fix task, FIRST auto-fix
	// the safe (auto-applicable) diagnostics with lsp_autofix so the model is left
	// with only the MANUAL ones (deprecated APIs, unused symbols) — a much smaller
	// task that finishes well inside the tool budget instead of looping. Then scan
	// the (post-autofix) state and pack the remaining diagnostics + code windows
	// into the first prompt so the model fixes in place instead of re-scanning.
	e.preflightBlock = ""
	e.preflightAutoFix = ""
	if e.lspAvailable > 0 && looksLikeLSPFixTask(userQuery) {
		if at := e.tools.ToolByName("lsp_autofix"); at != nil {
			if fr, ferr := at.Execute(ctx, `{"target":"all"}`); ferr == nil && fr != "" {
				e.preflightAutoFix = fr
				if onUpdate != nil {
					onUpdate(e.state, "🤖 Pre-flight: auto-fixed safe diagnostics")
				}
			}
		}
		if packed := e.preflightLSP(ctx); packed != "" {
			e.preflightBlock = packed
			if onUpdate != nil {
				onUpdate(e.state, "📡 Pre-flight: scanned LSP diagnostics & packed context")
			}
		}
	}
	// While pre-flight diagnostics are in context, whole-file reads are blocked:
	// the model already has every diagnostic's code window, so re-reading a whole
	// file is pure waste (tokens + latency). It must fix/verify from the packed
	// windows or re-run lsp_scan instead.
	e.preflightActive = e.preflightBlock != ""
	// Plan-then-act gate: for a multi-step implementation task, force a read-only
	// PLAN pass first — the model researches and proposes a plan, then confirms via
	// ask_user before any file is mutated. Approval (detected on the ask_user
	// result) flips the session to BUILDER. Toggled by planGateEnabled; off once
	// planApproved for the session, and never applied to read/question prompts or
	// non-BUILDER modes.
	e.planMode = false
	if e.planGateEnabled && !e.planApproved && e.mode == "BUILDER" && looksLikeImplTask(userQuery) {
		e.planMode = true
		e.tools.SetExecutionPolicy(true, false) // read-only; research only
		if onUpdate != nil {
			onUpdate(e.state, "📋 Plan mode: researching & drafting plan (no edits until you approve)")
		}
	}
	defer func() { e.progressHandler = nil }()
	// Skill-load summary: surface the turn's skill usage in the activity HUD so
	// a MISSED trigger — the agent used tools but never loaded any of the
	// catalog's skills — is visible instead of silent. Fires before the
	// progress handler is cleared, and only on successful turns that actually
	// used tools (a pure-chat turn cannot have loaded a skill, so it is quiet).
	defer func() {
		if err != nil || onUpdate == nil || len(e.skillsEntries) == 0 || !e.turnUsedTools {
			return
		}
		if len(e.loadedSkills) == 0 {
			onUpdate(e.state, fmt.Sprintf("📚 No skills loaded this turn — %d skills in the catalog (list .agents/skills and .brocode/skills)", len(e.skillsEntries)))
			return
		}
		names := make([]string, 0, len(e.loadedSkills))
		for n := range e.loadedSkills {
			names = append(names, n)
		}
		slices.Sort(names)
		onUpdate(e.state, "📚 Skills loaded: "+strings.Join(names, ", "))
	}()
	// Lifecycle hook on every exit path: fire on-turn-end when the turn
	// produced an answer (or a hard abort message), on-turn-error otherwise.
	defer func() {
		ev := hooks.EventTurnEnd
		if err != nil {
			ev = hooks.EventTurnError
		}
		e.hookRun(context.Background(), ev, map[string]string{
			"error":  errString(err),
			"mode":   e.Mode(),
			"answer": answer,
			"query":  userQuery,
		})
	}()

	// A real user prompt is the accept signal for last turn's snapshots:
	// each snapshot lives exactly one turn, then the rollback window closes.
	if cleaned := tool.CleanupStaleSnapshots(); cleaned > 0 && onUpdate != nil {
		onUpdate(e.state, fmt.Sprintf("Cleaned %d stale snapshots", cleaned))
	}
	// Wipe last turn's tool-output artifacts: the store is bounded to one turn
	// (Principle 5 — every persistent store has a lifetime).
	e.cleanupArtifacts()

	if userQuery != "" {
		// Stale-context detection: if the new prompt is semantically unrelated
		// to the ongoing conversation (keyword overlap < 30%), do a partial
		// context reset (keep session metadata, drop the message thread) to
		// avoid the model charging ahead in the wrong direction.
		if e.context != nil && e.context.Len() > 5 {
			stale, _ := e.context.IsStaleContext(userQuery)
			if stale {
				e.context.ResetStaleContext(userQuery)
				if onUpdate != nil {
					onUpdate(e.state, "🔄 Stale context detected — partial reset (new topic)")
				}
			}
		}

		// Smart scope pre-selection: if the prompt contains recognizable
		// keywords, pre-compute relevance-ranked files and inject a
		// "SMART SCOPE" hint into the system prompt so the model focuses
		// exploration on the relevant subset instead of scanning everything.
		var scopeHint string
		if len(e.repoFiles) > 0 {
			results := repo.ScoreFiles(e.repoFiles, userQuery, 8)
			if len(results) > 0 {
				scopeHint = repo.SummarizeScope(results, userQuery)
				if onUpdate != nil {
					onUpdate(e.state, fmt.Sprintf("🎯 Smart scope: %d relevant files identified", len(results)))
				}
			}
		}
		e.scopeHint = scopeHint // set before buildSystemPrompt

		if err := e.context.AppendUserMessage(userQuery); err != nil {
			return "", err
		}
		// TSR REPRODUCE gate: arm only when the task looks like a genuine RUNTIME
		// bug fix (panic/crash/regression/"not working") that must be reproduced
		// before fixing. Diagnostic/LSP-fix tasks (warnings, errors, lint,
		// deprecations) are COMPILE-TIME — the LSP scan IS the source of truth,
		// so arming the gate would only force a pointless reproduce round that
		// makes the agent spin (go test / go vet) before it even edits. If the
		// user already pasted the error/stack trace, treat the repro as provided.
		e.reproGateArmed = looksLikeBugFixTask(userQuery) && !looksLikeLSPFixTask(userQuery)
		if looksLikeProvidedRepro(userQuery) {
			e.reproEstablished = true
		}
		if e.reproGateArmed && len(planVerification()) == 0 {
			e.reproGateArmed = false
		}
	}

	// Lifecycle hook: turn start (before any LLM call). Output is informational
	// only — hooks cannot replace a user prompt.
	e.hookRun(ctx, hooks.EventTurnStart, map[string]string{
		"query": userQuery,
		"mode":  e.Mode(),
	})

	// Deliver any scout findings that finished since the last turn ended.
	e.drainScouts(onUpdate)

	// Inject progress callback into the tool-execution context so blocking tools
	// like subagent/scout can stream interim updates to the TUI instead of
	// freezing for 10-30s. Uses the engine's current state.
	toolCtx := tool.WithProgress(ctx, func(state string, info string) {
		if onUpdate != nil {
			onUpdate(e.state, info)
		}
	})
	// Track files touched this turn so the Smart Context Graph can build
	// co-read/edit edges (the "graph" part of the self-aware context layer).
	toolCtx = tool.WithTurnFiles(toolCtx)

	iteration := 0

	for {
		iteration++
		if iteration > e.maxIterations {
			hardCap := e.hardCapIterations
			if hardCap <= 0 {
				hardCap = 100
			}

			if iteration <= hardCap {
				if e.autoExtendSession {
					e.maxIterations += 15
					if onUpdate != nil {
						onUpdate(e.state, fmt.Sprintf("⚡ Autonomous Mode Active: Auto-extending turn limit to %d iterations", e.maxIterations))
					}
					continue
				}

				if e.askHandler != nil {
					q := fmt.Sprintf("🤖 BroCode Agent evaluated task as incomplete (Turn %d/%d, %d files explored) and requires further tool calls. Grant extension?", e.maxIterations, hardCap, len(e.explored))
					opts := []string{
						"Allow Once (+15 turns)",
						"Always Allow for this session",
						"Reject & Synthesize Now",
					}
					ans, err := e.askHandler(q, opts)
					if err == nil {
						if strings.HasPrefix(ans, "Allow Once") || strings.HasPrefix(ans, "[1]") || strings.Contains(strings.ToLower(ans), "once") {
							e.maxIterations += 15
							if onUpdate != nil {
								onUpdate(e.state, fmt.Sprintf("⚡ Extended turn limit to %d iterations", e.maxIterations))
							}
							continue
						} else if strings.HasPrefix(ans, "Always Allow") || strings.HasPrefix(ans, "[2]") || strings.Contains(strings.ToLower(ans), "always") {
							e.autoExtendSession = true
							e.maxIterations += 15
							if onUpdate != nil {
								onUpdate(e.state, fmt.Sprintf("⚡ Autonomous Mode Active: Extended turn limit to %d iterations", e.maxIterations))
							}
							continue
						}
					}
				}
			}

			// Iteration budget exhausted: graceful synthesis (no extension).
			return e.finalSynth(ctx, fmt.Sprintf("MAXIMUM ITERATIONS REACHED (%d): You have reached the maximum iteration limit for this task.", e.maxIterations), "Batas Maksimal 25 Ronde Tercapai")
		}

		// Hard cost budget: once the estimated spend exceeds the per-task USD
		// cap, stop the turn gracefully — budget is a hard limit, so no
		// extension prompt is offered.
		if e.budgetUSD > 0 && e.costUSD >= e.budgetUSD {
			if onUpdate != nil {
				onUpdate(e.state, fmt.Sprintf("⚠️ Cost budget exceeded ($%.4f) — synthesizing final answer from explored context...", e.costUSD))
			}
			return e.finalSynth(ctx, fmt.Sprintf("COST BUDGET EXCEEDED ($%.4f): The per-task cost budget has been exhausted.", e.costUSD), "Cost Budget Reached")
		}

		// Progress detection: a tool-only round that examined NO new file is a
		// stall; a model still discovering fresh files is genuinely thinking
		// and gets room to finish instead of being cut mid-thought. The newest
		// explored entry is the signal (the list is capped, so its length
		// plateaus and cannot indicate progress).
		newest := ""
		if n := len(e.explored); n > 0 {
			newest = e.explored[n-1]
		}
		if newest == e.lastExploredTarget {
			e.exploredStalls++
		} else {
			e.exploredStalls = 0
			e.lastExploredTarget = newest
		}

		// Tool-only budget: if the model keeps calling tools and never writes
		// an answer, remind it at the threshold with what it has already
		// explored, remind it again more firmly one round later, and only then
		// abort. Freedom is adaptive: a model still discovering NEW files is
		// allowed to keep going (up to the absolute cap), while a spinning
		// model (no new files for several rounds) is cut off. The reminders
		// never demand a fabricated answer — the model may honestly report
		// what it knows and what context is still missing.
		warnRounds, finalWarnRounds, maxAbsolute, exploredCap := e.explorationBudget()

		if e.toolOnlyRounds >= warnRounds && !e.toolReminderSent {
			e.toolReminderSent = true
			// The reminder must NOT forbid tools absolutely: a model that
			// knows exactly which one more read (a specific line range of a
			// big file) would settle the answer should be allowed to make it,
			// then answer. An absolute "do not call tools" left models stuck
			// deliberating in reasoning forever ("warning says stop but I
			// genuinely need lines 60-100") until the budget aborted them.
			_ = e.context.AppendUserMessage(fmt.Sprintf("⚠️ You have called tools %d times in a row without answering, and already examined %d files. If you know exactly which ONE more read would settle the answer (a specific file or line range), make that single read — then answer. Otherwise answer NOW using what you have read; do not keep exploring. If you genuinely do NOT have enough context, stop and say exactly what is missing instead of guessing."+e.exploredSummary(), e.toolOnlyRounds, len(e.explored)))
			if onUpdate != nil {
				onUpdate(e.state, "⚠️ Tool budget nearly exhausted — one final read allowed, then answer")
			}
			continue
		}
		if e.toolOnlyRounds >= finalWarnRounds && !e.toolReminder2Sent {
			e.toolReminder2Sent = true
			_ = e.context.AppendUserMessage("⚠️ FINAL WARNING: This is your LAST chance. You may make AT MOST ONE more tool call — only a specific read you already know you need (a file or line range) — and then you MUST write your answer in the next response. No further exploration, no re-reading. If you cannot fully answer, give a partial answer with what you know and state clearly what context is still missing — never fabricate." + e.exploredSummary())
			if onUpdate != nil {
				onUpdate(e.state, "⚠️ Final warning — one final read max, then answer or stop")
			}
			continue
		}
		// Accelerate the final warning when the agent has already read many
		// DISTINCT files yet still hasn't answered — continued reading is now
		// over-exploration, not progress (it was resetting the stall counter by
		// touching a new file each round).
		if !e.toolReminder2Sent && e.toolOnlyRounds >= warnRounds && len(e.explored) >= exploredCap {
			e.toolReminder2Sent = true
			_ = e.context.AppendUserMessage("⚠️ FINAL WARNING: You have already examined " + fmt.Sprintf("%d", len(e.explored)) + " distinct files without answering. Make AT MOST ONE more targeted read if you truly need it, then write your answer. Do NOT keep opening new files." + e.exploredSummary())
			if onUpdate != nil {
				onUpdate(e.state, "⚠️ Final warning — too much reading, answer now")
			}
			continue
		}
		// 1. Thinking State
		e.state = StateThinking
		if onUpdate != nil {
			onUpdate(e.state, fmt.Sprintf("Turn %d/%d reasoning...", iteration, e.maxIterations))
		}

		currentMode := e.Mode()
		// System prompt is built ONCE per turn and cached: the full mode rules,
		// project context, repo map, skills, and warm-start memory excerpt form
		// a stable prefix so every later iteration sends the same leading bytes
		// (provider prompt caching). A fresh turn rebuilds it for the new query.
		if e.sysPromptCached == "" {
			e.sysPromptCached = e.buildSystemPrompt(currentMode, iteration, onUpdate)
		}
		sysPrompt := e.sysPromptCached
		// Account for the system prompt in the context budget so compaction (and
		// the wire-size guard in fitMessages) fire before the real request can
		// overflow the model window.
		e.context.SetSystemPromptTokens(bcontext.EstimateTokens(sysPrompt))

	// Deliver any scout findings that finished since the last iteration —
	// a scout started mid-turn reaches the model at the NEXT reasoning step
	// instead of only at the next turn's start. (Turn start also drains, so
	// findings parked while the loop was idle still arrive.)
	e.drainScouts(onUpdate)

		adaptiveCap := CalculateAdaptiveToolBudget(e.context.LastUserPrompt(), e.Mode())
		// Hard stop after the FINAL WARNING: the model was already told "one more
		// read, then answer" — if it is still only calling tools after the grace
		// rounds, force synthesis instead of letting it read in circles.
		toolBudgetExhausted := e.toolOnlyRounds >= adaptiveCap && (e.exploredStalls >= 4 || e.toolOnlyRounds >= maxAbsolute)
		if e.toolReminder2Sent && e.toolOnlyRounds >= finalWarnRounds+finalWarnHardStop {
			toolBudgetExhausted = true
		}
		if toolBudgetExhausted {
			if onUpdate != nil {
				onUpdate(e.state, "⚠️ Tool exploration limit reached — applying known fixes, then synthesizing...")
			}
			// FIX-TASK SAFETY NET: when the task was to fix diagnostics and an LSP is
			// available, actually apply the auto-fixable ones NOW (deterministically,
			// via the batch lsp_autofix tool) instead of punting a TODO list to the
			// user. This is the core "efficiency by design" guarantee — the machine
			// finishes the mechanical fixes even when the model loop ran out of budget.
			var autoFixResult string
			if e.lspAvailable > 0 && (e.preflightBlock != "" || looksLikeLSPFixTask(e.context.LastUserPrompt())) {
				if at := e.tools.ToolByName("lsp_autofix"); at != nil {
					if fr, ferr := at.Execute(ctx, `{"target":"all"}`); ferr == nil {
						autoFixResult = fr
					}
				}
			}
			// Graceful Recovery: make ONE final completion request WITHOUT tools,
			// forcing the model to synthesize a helpful answer from context so far.
			synthPrompt := "⚠️ TOOL EXPLORATION BUDGET REACHED: tool calls are now DISABLED for this final response. "
			if autoFixResult != "" {
				synthPrompt += "The engine already applied auto-fixes via lsp_autofix (results below) — report exactly what was fixed. "
			}
			synthPrompt += "Synthesize a helpful, comprehensive response for the user in the user's language based ONLY on the files and context explored so far. If the task was to fix warnings/deprecations, report the fixes that were applied and list any that remain for a follow-up turn — do NOT tell the user to apply the fixes themselves. Answer as much of the user's prompt as possible; note any genuinely missing context." + e.exploredSummary()
			if autoFixResult != "" {
				synthPrompt += "\n\nENGINE AUTO-FIX RESULTS:\n" + autoFixResult
			}
			_ = e.context.AppendUserMessage(synthPrompt)

			reqMessages := append([]provider.Message{
				{Role: "system", Content: sysPrompt},
	}, e.fitMessages(sysPrompt)...)

			synthReq := provider.CompletionRequest{
				Model:       e.model,
				Messages:    reqMessages,
				Tools:       nil, // DISABLE TOOLS to force text response!
				Temperature: 0.2,
			}

			synthResp, synthErr := e.complete(ctx, synthReq)
			if synthErr == nil && synthResp != nil && strings.TrimSpace(synthResp.Content) == "" {
				// The model stalled into an empty reply (or kept emitting tool
				// calls despite tools being disabled). Give the no-tools request
				// ONE more attempt before giving up.
				synthResp, synthErr = e.complete(ctx, synthReq)
			}
			if synthErr == nil && synthResp != nil && strings.TrimSpace(synthResp.Content) != "" {
				res := synthResp.Content + "\n\n---\n*💡 Exploration paused for token efficiency. Send a follow-up prompt to continue deep exploration.*"
				_ = e.context.AppendAssistantTurn(e.Mode(), e.model, "", res, nil)
				e.state = StateDone
				if onUpdate != nil {
					onUpdate(e.state, "Completed with graceful context synthesis")
				}
				return res, nil
			}

			// Deterministic fallback when the synthesis completion itself fails
			// or returns empty: construct a helpful answer from what the agent
			// already explored.
			e.state = StateDone
			msg := "Exploration paused for token efficiency — here is the verified context summary:\n" + e.exploredSummary()
			if autoFixResult != "" {
				msg = "Exploration paused for token efficiency; automated fixes applied first:\n" + autoFixResult + "\n\n" + msg
			}
			if e.lastReasoning != "" {
				msg += "\n\n**Last Analysis Focus**: " + e.lastReasoning
			}
			msg += "\n\n---\n*💡 Send a follow-up prompt to continue deep exploration.*"
			_ = e.context.AppendAssistantTurn(e.Mode(), e.model, "", msg, nil)
			if onUpdate != nil {
				onUpdate(e.state, "Completed with fallback context synthesis")
			}
			return msg, nil
		}

		// Auto-compact context if token count exceeds threshold
		if e.context.NeedsCompaction() {
			// Prefer a model-written summary (preserves real context); fall back
			// to a deterministic placeholder on any failure so compaction never
			// blocks the turn.
			summary, ok := e.modelCompactionSummary(ctx)
			if !ok {
			summary = bcontext.CompactionSummary{
				Goal:           "Continue active conversation",
				FilesTouched:   []string{"codebase"},
				DecisionsMade:  []string{"Compacted older context turns to preserve memory window"},
				NextAction:     "Proceed with the current user request",
				Constraints:    "Preserve verified facts; do not re-run work already done",
				OpenQuestions:  []string{"Proceed with user request"},
				LastKnownState: "Context compacted successfully",
			}
			}
			_ = e.context.Compact(summary)
			// Auto-extract: durable session context is merged into project
			// memory so a future session starts warm instead of cold.
			if e.mem != nil {
				_ = e.mem.MergeCompaction(summary.Goal, summary.DecisionsMade, summary.LastKnownState)
			}
		}

		reqMessages := append([]provider.Message{
			{Role: "system", Content: sysPrompt},
		}, e.fitMessages(sysPrompt)...)

		req := provider.CompletionRequest{
			Model:       e.model,
			Messages:    reqMessages,
			Tools:       e.toolsForMode(currentMode),
			Temperature: 0.2,
		}

		if onUpdate != nil {
			onUpdate(e.state, "Thinking & analyzing request...")
		}

		resp, fbModel, err := e.completeTurn(ctx, req, onUpdate)
		if err != nil {
			e.state = StateFailed
			return "", err
		}
		if fbModel != "" {
			e.lastFallback = fbModel
		}

		// Normalize think tags and pseudo-XML tool calls emitted in Content by models like Poolside laguna / DeepSeek
		if resp.Content != "" {
			if r, cleaned := provider.ExtractEmbeddedReasoning(resp.Content); r != "" {
				if resp.Reasoning == "" {
					resp.Reasoning = r
				} else {
					resp.Reasoning += "\n\n" + r
				}
				resp.Content = cleaned
			}
		}
		if len(resp.ToolCalls) == 0 && resp.Content != "" {
			if extracted, cleaned := provider.ExtractEmbeddedToolCalls(resp.Content); len(extracted) > 0 {
				resp.ToolCalls = extracted
				resp.Content = cleaned
			}
		}
		// North-star metric: a completion that ends the turn with an answer
		// (no further tool calls) is productive — credit its tokens now.
		// (Rounds that spawn file-mutating tools are credited separately below.)
		if len(resp.ToolCalls) == 0 {
			e.turnProductiveTokens += e.lastRoundOutput
		}

		// Remember the model's last reasoning so a tool-budget abort can tell
		// the user WHAT the agent was stuck on ("search for more models…")
		// instead of just dumping a file list.
		if resp.Reasoning != "" {
			e.lastReasoning = resp.Reasoning
		}

		// Thinking enforcement (§2.2)
		reasoning := resp.Reasoning
		if reasoning == "" && len(resp.ToolCalls) == 0 && resp.Content == "" {
			reasoning = "Analyzing request and context."
		}

		// Append assistant turn to store and context
		if err := e.context.AppendAssistantTurn(e.Mode(), e.model, reasoning, resp.Content, resp.ToolCalls); err != nil {
			return "", err
		}

		// 2. Check if Model wants to call tools (Acting & Observing State)
		if len(resp.ToolCalls) > 0 {
			e.state = StateActing
			// This round was tool-only (no answer text); count it toward the
			// budget so a model that never answers gets cut off.
			e.toolOnlyRounds++
			// A fresh round has not counted any bash family yet.
			e.iterBashFamily = ""

			// PHASE 1 — DECISION (sequential, in call order): classify every
			// tool call as either blocked (guard/deny/override message) or
			// pending real execution. This phase MUST stay sequential: the
			// mode guards, repeat detection and permission gate are stateful
			// or interactive (the gate can pop a confirmation modal), and the
			// tool-call hook can veto/override. Outcomes are stored in an
			// index-aligned slice so the append phase below can emit results
			// in the model's ORIGINAL call order — providers require the
			// tool_call → tool_result pairing to be in order.
			type pendingTool struct {
				tc     provider.ToolCall
				exec   bool   // true → run the tool for real
				output string // guard/denied/override message when !exec
			}
			pending := make([]pendingTool, len(resp.ToolCalls))
			execCount := 0
			// Tracks whether this round executed a file-mutating tool, so its
			// completion tokens can be credited as productive (north-star metric).
			roundMutation := false

			for i, tc := range resp.ToolCalls {
				// Strict mode tool guards: PLANNER is fully read-only (no bash,
				// no writes); MINER is read-only on source files but may run
				// read-only bash (git log/status) and, crucially, may retain
				// verified facts into project memory.
				switch e.Mode() {
				case "PLANNER":
					if tc.Name == "write_file" || tc.Name == "edit_file" || tc.Name == "delete_file" || tc.Name == "bash" {
						guardMsg := fmt.Sprintf("⚠️ [PLANNER GUARD]: Tool '%s' is disabled in PLANNER mode (read-only architecture mode). Switch to BUILDER mode (Shift+Tab) to execute code changes.", tc.Name)
						if onUpdate != nil {
							onUpdate(e.state, guardMsg)
						}
						pending[i] = pendingTool{tc: tc, output: guardMsg}
						continue
					}
				case "MINER":
					if tc.Name == "write_file" || tc.Name == "edit_file" || tc.Name == "delete_file" {
						guardMsg := fmt.Sprintf("⚠️ [MINER GUARD]: Tool '%s' is blocked in MINER mode (read-only knowledge agent). Switch to BUILDER mode (Shift+Tab) to modify code.", tc.Name)
						if onUpdate != nil {
							onUpdate(e.state, guardMsg)
						}
						pending[i] = pendingTool{tc: tc, output: guardMsg}
						continue
					}
				}

				// TSR REPRODUCE gate: while the task is armed as a bug fix and no
				// reproduction has been observed, code edits are blocked with a
				// Off-Task Edit Gate: block write/edit on a file the model has NEVER
				// read, grepped, or explicitly fetched in this turn. This prevents
				// the model from making destructive edits to the wrong file when it
				// panics near the stall/budget limit and guesses a target.
				if tc.Name == "write_file" || tc.Name == "edit_file" || tc.Name == "delete_file" {
					var editPath string
					var args map[string]any
					if json.Unmarshal([]byte(tc.Arguments), &args) == nil {
						editPath, _ = args["path"].(string)
					}
					if editPath != "" {
						if abs, err := filepath.Abs(editPath); err == nil {
							editPath = abs
						}
						// If the file does not exist yet on disk (creating a new file with write_file),
						// it cannot be read first — allow new file creation immediately.
						_, statErr := os.Stat(editPath)
						isNewFile := os.IsNotExist(statErr)

						// Count non-bash explored entries to see if the model has
						// performed any file reads this turn. If it has only run bash
						// commands (no read_file/grep/list_dir), the gate does not
						// trigger — the model may be writing a new file or doing a
						// straightforward edit without needing to read first.
						fileReads := 0
						for _, entry := range e.explored {
							if !strings.HasPrefix(entry, "bash: ") {
								fileReads++
							}
						}
						if !isNewFile && fileReads > 0 && !e.wasExplored(editPath) {
							offTaskMsg := fmt.Sprintf(
								"⚠️ [OFF-TASK EDIT GATE]: You are trying to edit '%s' but you have NOT read or examined it during this turn (you explored: %s). Read it first before editing, or confirm this is the correct target file.",
								editPath, e.exploredPathList(),
							)
							if onUpdate != nil {
								onUpdate(e.state, "⚠️ Edit blocked — file not read during this turn")
							}
							pending[i] = pendingTool{tc: tc, output: offTaskMsg}
							continue
						}
					}
				}

				// TSR contract: for bug-fix tasks, block any edit until the failure
				// is reproduced first. This is a guard against fixing a bug without
				// first confirming it exists.
				// reminder to reproduce the failure first (run the failing test /
				// command and watch it FAIL) before fixing. This is the
				// REPRODUCE→LOCALIZE→SOLVE→VERIFY contract: a fix verified against
				// an unreproduced bug is not verified at all. Edits stay blocked
				// until a tool result shows a failing command/test, the user
				// already pasted the failure, or the model chooses to answer
				// without editing (it may honestly report it cannot reproduce).
				if e.reproGateArmed && !e.reproEstablished &&
					(tc.Name == "write_file" || tc.Name == "edit_file" || tc.Name == "delete_file") {
					var reproGuard string
					if !e.reproReminderSent {
						e.reproReminderSent = true
						reproGuard = "⚠️ [TSR REPRODUCE GATE]: You tried to change code, but the reported failure has NOT been reproduced yet. TSR contract: REPRODUCE first — run the relevant test/command and OBSERVE it fail (that confirms the bug and gives you a verification baseline), THEN fix it. Do not edit before reproducing. If you cannot reproduce the failure, do not edit — answer directly and state that the bug could not be reproduced."
					} else {
						reproGuard = "⚠️ [TSR REPRODUCE GATE]: Still no reproduction observed. Edit blocked. Reproduce the failure first (run the test/command and see it fail), or answer directly explaining that you cannot reproduce it — do not edit blind."
					}
					if onUpdate != nil {
						onUpdate(e.state, "⚠️ TSR reproduce gate: edit blocked until failure is reproduced")
					}
					pending[i] = pendingTool{tc: tc, output: reproGuard}
					continue
				}

				toolInfo := formatToolCallInfo(tc.Name, tc.Arguments)
				if onUpdate != nil {
					onUpdate(e.state, toolInfo)
				}

				// Track what the model has actually explored so budget reminders
				// can tell it what it already knows ("you've read X, Y, Z —
				// answer now") instead of a generic stop message.
				e.recordExplored(tc)

				// SKILL-LOAD TRACING: a read_file aimed at a known skill's
				// SKILL.md means the model reached for that skill — surface it in
				// the activity HUD (once per skill per turn). Skills fail
				// silently in other agents when the model never triggers them;
				// tracing every load makes both usage and absence visible.
				if tc.Name == "read_file" {
					var am map[string]any
					if json.Unmarshal([]byte(tc.Arguments), &am) == nil {
						if p, _ := am["path"].(string); p != "" {
						if name := e.skillForRead(p); name != "" && !e.loadedSkills[name] {
							e.loadedSkills[name] = true
							e.skillDirs[name] = filepath.Dir(p)
							if onUpdate != nil {
								onUpdate(e.state, "📚 Skill loaded: "+name)
							}
							}
						}
					}
				}

				// 1. Native Sovereignty Gate: block reading/writing 3rd party agent framework paths
				if tc.Name == "read_file" || tc.Name == "edit_file" || tc.Name == "write_file" {
					var args struct {
						Path string `json:"path"`
					}
					if json.Unmarshal([]byte(tc.Arguments), &args) == nil && args.Path != "" {
						cleanPath := filepath.ToSlash(args.Path)
						if strings.HasPrefix(cleanPath, ".agents/plan") || strings.HasPrefix(cleanPath, ".agents/rules") ||
							strings.HasPrefix(cleanPath, ".cursor/") || strings.HasPrefix(cleanPath, ".windsurf/") {
							guardMsg := fmt.Sprintf("⚠️ [NATIVE SOVEREIGNTY]: '%s' is a third-party framework directory. BroCode operates exclusively using native tools and storage (.brocode/current_plan.md). Do not search for or read plans there.", args.Path)
							pending[i] = pendingTool{tc: tc, output: guardMsg}
							continue
						}
					}
				}

				// 2. Multi-tool cycle and consecutive repetition detection:
				key := normalizedToolCallKey(tc)
				if e.turnToolCallCounts == nil {
					e.turnToolCallCounts = make(map[string]int)
				}
				e.turnToolCallCounts[key]++
				callCount := e.turnToolCallCounts[key]

				if isRepeatToolCall(tc, e.lastToolCall) {
					e.lastToolCallRepeats++
				} else {
					e.lastToolCallRepeats = 0
				}
				e.lastToolCall = tc

				if callCount >= 2 || e.lastToolCallRepeats >= 2 {
					if callCount >= 4 || e.lastToolCallRepeats >= 4 {
						// The model ignored the guard warning and is still
						// spinning — abort the whole turn instead of burning iterations.
						e.state = StateBlocked
						msg := fmt.Sprintf("Turn aborted: the model kept repeating tool call '%s' with identical arguments (%d times) after being told to stop. Context already contains the requested information. Please rephrase your request or ask for a more specific task.", tc.Name, callCount)
						if onUpdate != nil {
							onUpdate(e.state, msg)
						}
						return msg, nil
					}
					guardMsg := fmt.Sprintf("⚠️ [LOOP GUARD]: You already called '%s' with these exact arguments earlier in this turn (call #%d). The result is ALREADY in your conversation context above. Do NOT re-run the same tool call — synthesize what you already gathered or proceed with next steps.", tc.Name, callCount)
					if onUpdate != nil {
						onUpdate(e.state, fmt.Sprintf("⚠️ Loop detected: '%s' repeated %d× — blocking redundant call", tc.Name, callCount))
					}
					pending[i] = pendingTool{tc: tc, output: guardMsg}
					continue
				}

				// Fuzzy loop-break: exact repeats are caught above, but a model
				// can spin by re-issuing the SAME command family with different
				// arguments across rounds (go test ./a → go test ./b → …).
				// Each bash family is counted AT MOST once per round; a round
				// that mixes different families resets the streak (varied
				// exploration is not spinning, and batched same-family calls
				// in one round are legitimate). Once the streak is high, block
				// the call with a "change strategy" instruction.
				if tc.Name == "bash" {
					fam := bashFamily(tc.Arguments)
					if fam != "" {
						if e.iterBashFamily == "" {
							e.iterBashFamily = fam
							e.lastBashFamily, e.bashFamilyStreak = advanceBashFamily(fam, e.lastBashFamily, e.bashFamilyStreak)
						} else if fam != e.iterBashFamily {
							e.lastBashFamily = ""
							e.bashFamilyStreak = 0
						}
					}
					if e.bashFamilyStreak >= 6 {
						fuzzyGuard := fmt.Sprintf("⚠️ [LOOP GUARD]: You have spent %d consecutive rounds running similar '%s' commands without converging. Stop re-running the same command family — change your approach, answer directly with what you have, or state precisely what information is still missing.", e.bashFamilyStreak, e.lastBashFamily)
						if onUpdate != nil {
							onUpdate(e.state, "⚠️ Same-command loop detected — instructing model to change strategy")
						}
						pending[i] = pendingTool{tc: tc, output: fuzzyGuard}
						continue
					}
				}

				// Permission gate: risky bash commands ask the user for approval
				// (Allow once / Always allow / Deny) via the interactive modal.
				approved, reason, gerr := e.tools.GateAction(ctx, tc)
				if gerr != nil {
					pending[i] = pendingTool{tc: tc, output: fmt.Sprintf("Tool error: %v", gerr)}
					continue
				}
				if !approved {
					guardMsg := fmt.Sprintf("⛔ [PERMISSION DENIED]: %s", reason)
					if onUpdate != nil {
						onUpdate(e.state, guardMsg)
					}
					pending[i] = pendingTool{tc: tc, output: guardMsg}
					continue
				}

				// Lifecycle hook: before tool execution. A policy hook may veto or
				// override the tool by returning non-empty output, which REPLACES
				// the normal tool result.
				hookOverride := e.hookRun(ctx, hooks.EventToolCall, map[string]string{
					"tool": tc.Name,
					"args": tc.Arguments,
				})
				if hookOverride != "" {
					pending[i] = pendingTool{tc: tc, output: hookOverride}
					continue
				}

			pending[i] = pendingTool{tc: tc, exec: true}
			e.turnUsedTools = true // any tool reaching execution = tools used this turn
			if isFileMutationTool(tc.Name) {
				roundMutation = true
			}
			execCount++
		}
		// A round that executed a file mutation is productive: credit the
		// completion tokens that produced it (the deliverable, not overhead).
		if roundMutation {
			e.turnProductiveTokens += e.lastRoundOutput
		}

			// PHASE 2 — EXECUTION: run every pending call. Read-only tools
			// (read_file, grep, glob, list_dir, search_code, fetch_url,
			// web_search) execute CONCURRENTLY — they are stateless
			// and their results land in index-aligned slots, so ordering is
			// preserved regardless of completion order. Mutating/interactive
			// tools (write_file, edit_file, delete_file, bash, ask_user, git,
			// undo, review_changes, memory) run sequentially to keep their
			// side effects and user prompts ordered.
			if execCount > 0 {
				sem := make(chan struct{}, maxParallelReadOnlyTools)
				var wg sync.WaitGroup
				for i := range pending {
					if !pending[i].exec || !isParallelReadOnly(pending[i].tc.Name) {
						continue
					}
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						select {
						case sem <- struct{}{}:
							defer func() { <-sem }()
						case <-ctx.Done():
							pending[idx].output = "Tool call cancelled: " + ctx.Err().Error()
							return
						}
						// Deterministic guardrail: while pre-flight diagnostics are in
						// context, block reads of files already packed + redundant
						// lsp_scan re-calls (the model must fix in place instead).
						if pending[idx].tc.Name == "read_file" || pending[idx].tc.Name == "lsp_scan" {
							if reject := e.guardPreflightRedundant(pending[idx].tc.Name, pending[idx].tc.Arguments); reject != "" {
								if onUpdate != nil {
									onUpdate(e.state, "⛔ "+reject)
								}
								pending[idx].output = reject
								return
							}
						}
						out, err := e.tools.Execute(toolCtx, pending[idx].tc.Name, pending[idx].tc.Arguments)
						if err != nil {
							out = fmt.Sprintf("Tool error: %v", err)
						}
						// Truncate-and-pointer: long outputs spill to
						// .brocode/artifacts/ and only a head+tail digest enters
						// context (the tail holds test failures/stack traces).
						out = e.capToolOutput(pending[idx].tc.Name, out)
						pending[idx].output = out
						e.hookRun(ctx, hooks.EventToolResult, map[string]string{
							"tool":   pending[idx].tc.Name,
							"output": out,
						})
					}(i)
				}
				wg.Wait()
				if ctx.Err() != nil {
					return "", ctx.Err()
				}

				// Sequential pass for non-parallel tools (mutating / interactive).
				// Early-exit-on-error: if a mutating tool fails, skip the rest of
				// the round — downstream calls will almost certainly depend on the
				// failed result (edited file, bash result). Saves ~2-5 wasted
				// error rounds per failure.
				mutatingError := false
				for i := range pending {
					if !pending[i].exec || isParallelReadOnly(pending[i].tc.Name) {
						continue
					}
					if mutatingError {
						pending[i].output = "⏭️ Skipped: earlier mutating tool failed (early exit)"
						continue
					}
					// Deterministic guardrail: while pre-flight diagnostics are in
					// context, block reads of files already packed + redundant
					// lsp_scan re-calls (the model must fix in place instead).
					if pending[i].tc.Name == "read_file" || pending[i].tc.Name == "lsp_scan" {
						if reject := e.guardPreflightRedundant(pending[i].tc.Name, pending[i].tc.Arguments); reject != "" {
							if onUpdate != nil {
								onUpdate(e.state, "⛔ "+reject)
							}
							pending[i].output = reject
							continue
						}
					}
					out, err := e.tools.Execute(toolCtx, pending[i].tc.Name, pending[i].tc.Arguments)
					if err != nil {
						out = fmt.Sprintf("Tool error: %v", err)
						// Trigger early exit only for mutating tools (write/edit/bash)
						if e.earlyExitOnError && isMutatingTool(pending[i].tc.Name) {
							mutatingError = true
						}
					}
					// Truncate-and-pointer: long outputs spill to
					// .brocode/artifacts/ and only a head+tail digest enters
					// context (the tail holds test failures/stack traces).
					out = e.capToolOutput(pending[i].tc.Name, out)
					pending[i].output = out

					// Plan-then-act: the user approving the plan (any ask_user result
					// containing "approve") flips the session from read-only PLAN to
					// BUILDER, restoring mutating tools for the execution phase.
					if e.planMode && pending[i].tc.Name == "ask_user" && strings.Contains(strings.ToLower(out), "approve") {
						e.planApproved = true
						e.planMode = false
						e.tools.SetExecutionPolicy(false, false) // restore BUILDER
						if onUpdate != nil {
							onUpdate(e.state, "✅ Plan approved — switching to BUILDER to implement")
						}
					}

					// Track files the model edited so the native convention checker
					// can review them (debug leftovers, markers, type safety,
					// duplicate symbols) before the turn is declared done.
				if pending[i].tc.Name == "write_file" || pending[i].tc.Name == "edit_file" || pending[i].tc.Name == "delete_file" || pending[i].tc.Name == "lsp_fix" || pending[i].tc.Name == "lsp_rename" || pending[i].tc.Name == "run_tests" {
					// Active mutation or verification is tangible task progress:
					// reset the read-only exploration stall counters so complex refactoring
					// tasks across multiple files have a full runway to complete.
					e.toolOnlyRounds = 0
					e.toolReminderSent = false
					e.toolReminder2Sent = false
					e.exploredStalls = 0
					if pending[i].tc.Name == "write_file" || pending[i].tc.Name == "edit_file" || pending[i].tc.Name == "edit_symbol" {
						if p := extractToolPath(pending[i].tc.Arguments); p != "" {
							e.editedFiles = append(e.editedFiles, p)
							// Keep the session symbol index current after real edits.
							if e.onFileEdited != nil && err == nil {
								e.onFileEdited(p)
							}
							// Real-time post-edit LSP diagnostic hook (Gap 5)
							if e.diagFn != nil && err == nil {
								if lspDiag := e.diagFn(p); lspDiag != "" && !strings.HasPrefix(lspDiag, "No diagnostics") {
									out += "\n\n⚡ [REAL-TIME LSP DIAGNOSTIC]:\n" + lspDiag
									pending[i].output = out
								}
							}
						}
					}
				}

					// Lifecycle hook: after tool execution, with the tool's output.
					e.hookRun(ctx, hooks.EventToolResult, map[string]string{
						"tool":   pending[i].tc.Name,
						"output": out,
					})
				}
			}

			// PHASE 3 — APPEND (original call order): emit every result — real
			// outputs AND guard/deny/override messages — in the exact order the
			// model issued its tool calls. Providers require the tool_call →
			// tool_result pairing to be sequential; parallel execution must
			// never reorder it.
			e.state = StateObserving
			for i := range pending {
				if err := e.context.AppendToolResult(pending[i].tc.ID, pending[i].output); err != nil {
					return "", err
				}
				// A failing tool result is a REPRODUCTION: the bug is confirmed
				// by a command/test that actually failed, so the TSR reproduce
				// gate opens and the model may fix it. Only real outputs count —
				// guard/deny/override messages are never a reproduction.
				if pending[i].exec && e.reproGateArmed && !e.reproEstablished && looksLikeFailure(pending[i].output) {
					e.reproEstablished = true
					if onUpdate != nil {
						onUpdate(e.state, "🧪 Failure reproduced — TSR reproduce gate open, edits allowed")
					}
				}
			}

			// Real-time compact file-change summary into the activity slot
			// (P2 #2): emit only when NEW changes appeared this round, so the
			// user sees "what just got edited" live instead of a silent spinner.
			if onUpdate != nil {
				if n := tool.ChangesLen(); n > e.lastChangeEmit {
					e.lastChangeEmit = n
					onUpdate(e.state, "📝 "+tool.FileChangesOneLine(tool.PeekChanges()))
				}
			}

			// Live red/green diff entry per edit: surface a cumulative
			// diff for every path that gained a NEW change this round directly via onUpdate
			// and persist as a file_diff event so -c resume preserves exact chronological placement.
			if n := tool.ChangesLen(); n > e.lastChangeDiffEmit {
				chs := tool.PeekChanges()[e.lastChangeDiffEmit:n]
				e.lastChangeDiffEmit = n
				seen := make(map[string]bool)
				for _, ch := range chs {
					if seen[ch.Path] {
						continue
					}
					seen[ch.Path] = true
					if d := tool.CumulativeChangeDiff(ch.Path); d != "" {
						if onUpdate != nil {
							onUpdate(e.state, "DIFF:\n"+ch.Path+"\n"+d)
						}
						if e.context != nil {
							_ = e.context.AppendFileDiff(ch.Path, d)
						}
					}
				}
			}

			// Continuation rule: loop back to StateThinking automatically!
			continue
		}

		// 3. Verifying State (§2.4 Verification Ladder Level 1 & 2). Language-
		// agnostic: the project type (Go / JS-TS / Python / Rust / Java) is
		// detected from its config files and the matching checks run. Runs when
		// the model has actually edited files this turn (tracked on the engine,
		// so edits from EARLIER tool-only rounds are verified the moment the
		// model answers — a per-iteration hasCodeChanges flag would be dead
		// code, since tool-call rounds always continue before this block).
		if len(e.editedFiles) > 0 {
			e.state = StateVerifying
			if onUpdate != nil {
				msg := "Running verification..."
				if desc := describeVerification(); desc != "" {
					msg = "Running verification: " + desc
				}
				onUpdate(e.state, msg)
			}

			if vetErr := runVerification(ctx, e.editedFiles...); vetErr != "" {
				// Typed revision contract (§ TSR REPAIR/STOP): track repair
				// attempts and detect when the SAME error persists across fixes
				// (the model is retrying without progress). Stop the repair loop
				// gracefully — never burn iterations on a stuck fix.
				e.tsrAttempts++
				e.lastVerifyErr = vetErr
				hash := verifyErrorSignature(vetErr)
				if hash != "" && hash == e.lastVerifyHash {
					e.verifyErrorStreak++
				} else {
					e.lastVerifyHash = hash
					e.verifyErrorStreak = 1
				}
				if e.tsrAttempts >= maxTSRAttempts || e.verifyErrorStreak >= 3 {
					msg := fmt.Sprintf(
						"⚠️ Fix could not be verified after %d attempt(s) (same error %d×): stopping the repair loop.\n\nError still failing:\n%s\n\nSuggestions: try a different approach, split the change, or ask the user for clarification.",
						e.tsrAttempts, e.verifyErrorStreak, vetErr)
					_ = e.context.AppendAssistantTurn(e.Mode(), e.model, "", msg, nil)
					e.state = StateDone
					if onUpdate != nil {
						onUpdate(e.state, "Repair loop stopped (attempts exhausted / identical error persisted)")
					}
					return msg, nil
				}
				msg := "Level 1 verification check failed:\n" + vetErr + "\nPlease fix the issues."
				if e.hasPassedVerification {
					msg += "\n\n🚨 [REGRESSION OBLIGATION VIOLATION]: The verification suite previously passed in this session, but your latest changes broke it. You must resolve this regression before finishing."
				}
				if e.verifyErrorStreak >= 2 {
					msg += "\n\n⚠️ [STRATEGY INVALIDATION]: This error has persisted across multiple attempts. Your initial hypothesis or syntax tweak is invalid. Step back, re-read the context, and pivot your strategy rather than repeating the same fix."
				}
				if localized := e.localizeVerifyFailure(); localized != "" {
					msg += "\n\nLSP-localized view of the failing files (from the language server):\n" + localized
				}
				_ = e.context.AppendUserMessage(msg)
				continue
			}
			// Verification passed: record that verification is green
			e.hasPassedVerification = true

			// Verification passed after a previous failure: the repair
			// succeeded — record it so a durable lesson can be distilled.
			if e.tsrAttempts > 0 {
				e.repairSucceeded = true
				e.lessonFiles = append([]string{}, e.editedFiles...)
			}

			// Native code review (no LLM): debug leftovers, work markers,
			// type-safety red flags, and duplicate symbols in edited files,
			// plus LSP diagnostics when wired. Findings are fed back so the
			// model fixes them before declaring done.
			if review := e.reviewEditedFiles(ctx); review != "" {
				_ = e.context.AppendUserMessage("Code review:\n" + review + "\nPlease fix these issues before finishing.")
				continue
			}
		}

		// 4. Terminal Done State — the model answered, so any tool-only budget
		// accumulated earlier no longer applies.
		e.toolOnlyRounds = 0
		e.toolReminderSent = false
		e.toolReminder2Sent = false
		e.reviewPasses = 0
		e.state = StateDone

		// Persist usage: the files the model actually touched this turn feed
		// cross-session hot-file intelligence. Called before explored resets.
		if e.usageFn != nil {
			paths := make([]string, 0, len(e.editedFiles)+len(e.explored))
			paths = append(paths, e.editedFiles...)
			for _, ex := range e.explored {
				if !strings.Contains(ex, " ") { // skip bash commands/find strings
					paths = append(paths, ex)
				}
			}
			e.usageFn(paths)
		}

		// MINER mode: persist what this turn actually examined plus the
		// model's own synthesized summary into project memory, so a MINER run
		// leaves durable knowledge even when the model never called the memory
		// retain tool (its only other path). Deterministic, no extra LLM call.
		//
		// Context-aware learning: the memory query is derived from the explored
		// files' paths so WarmStartRelevant surfaces only memory facts that
		// relate to THIS investigation (not unrelated old notes).
		if e.Mode() == "MINER" && e.mem != nil {
			var explored []string
			for _, ex := range e.explored {
				if !strings.Contains(ex, " ") { // skip bash command strings
					explored = append(explored, ex)
				}
			}
			// Cache explored file names as the MINER's context query for
			// warm-start relevance filtering (only recall memory related to
			// files actually examined this turn, not generic "learn" notes).
			e.exploreQuery = strings.Join(explored, " ")
			_ = e.mem.CaptureMinerFindings(resp.Content, explored)
		}

		// PLANNER mode (or any turn producing a structured execution plan):
		// Automatically parse and save to .brocode/current_plan.md and project memory
		// so BUILDER mode immediately inherits the active plan without language bias!
		parsedPlan := plan.ParseMarkdownPlan(resp.Content)
		if parsedPlan != nil && len(parsedPlan.Steps) >= 1 && (e.Mode() == "PLANNER" || len(parsedPlan.Steps) >= 2) {
			_ = plan.SaveCurrentPlan(e.repoRoot, parsedPlan)
			if onUpdate != nil {
				onUpdate(e.state, fmt.Sprintf("📋 Active plan saved with %d step(s) to .brocode/current_plan.md", len(parsedPlan.Steps)))
			}
			if e.mem != nil {
				for _, step := range parsedPlan.Steps {
					clean := step.Description
					if len(clean) > 150 {
						clean = clean[:150] + "…"
					}
					_, _ = e.mem.Retain("Active Plan", clean)
				}
			}
		}

		// Out-of-scope findings capture (rule b13): a BUILDER turn that
		// noticed real issues OUTSIDE its task scope ends its answer with a
		// "### OUT-OF-SCOPE FINDINGS" section; persist those to project
		// memory so a follow-up task can pick them up instead of losing them
		// to the chat history. Deterministic parse, no extra LLM call.
		if e.Mode() == "BUILDER" {
			if len(e.editedFiles) > 0 {
				_, _ = plan.MarkStepsDoneByEditedFiles(e.repoRoot, e.editedFiles)
				if archived, archPath, _ := plan.AutoArchiveIfDone(e.repoRoot); archived {
					if onUpdate != nil {
						onUpdate(e.state, fmt.Sprintf("📦 All plan tasks completed — archived to %s", filepath.Base(archPath)))
					}
					if e.mem != nil {
						_, _ = e.mem.Retain("Active Plan", fmt.Sprintf("[Completed] Plan finished & archived to %s", filepath.Base(archPath)))
					}
				}
			}
			if e.mem != nil {
				if n := e.mem.CaptureOutOfScopeFindings(resp.Content); n > 0 {
					if onUpdate != nil {
						onUpdate(e.state, fmt.Sprintf("📋 %d out-of-scope finding(s) captured to project memory", n))
					}
				}
			}
		}

		// Lesson auto-extract: a repair that started failing and ended passing
		// is the highest-value failure signal a harness can capture — distill a
		// one-line durable lesson into project memory (## Gotchas) so future
		// sessions start knowing this failure mode instead of re-discovering it.
		if e.repairSucceeded && e.mem != nil && e.lastVerifyErr != "" {
			if lesson := e.distillLesson(ctx); lesson != "" {
				_, _ = e.mem.Retain("Gotchas", lesson)
				e.tagSkillLesson(lesson)
			}
		}

		// Full self-evolution: when the same skill has accumulated ≥2 distilled
		// gotchas from real repairs, propose a patch to its SKILL.md (written as
		// GOTCHAS.md next to the skill — never into SKILL.md itself, so official
		// skill updates keep applying). Runs once per loaded skill, best-effort.
		for sk := range e.loadedSkills {
			e.proposeSkillEvolution(sk)
			break
		}
		e.explored = nil

		// Self-aware reflection at turn end: consolidate this session's
		// captured experience notes into durable facts/gotchas for future
		// sessions. Best-effort and detached so it never adds turn latency.
		if e.knowledge != nil {
			go func(s *store.Store) {
				defer func() { recover() }()
				_, _ = bcontext.Reflect(s)
			}(e.knowledge)
		}

		if onUpdate != nil {
			onUpdate(e.state, "Completed")
		}
		return resp.Content, nil
	}
}

// proposeSkillEvolution proposes a patch when a skill has accumulated enough
// real gotchas: ≥2 distilled lessons for the same skill across sessions. The
// proposal is written as GOTCHAS.md in the skill's directory (never SKILL.md)
// and surfaced in the HUD for review. Best-effort — a failure changes nothing.
func (e *Engine) proposeSkillEvolution(sk string) {
	if e.mem == nil || sk == "" {
		return
	}
	gotchas := e.mem.SkillGotchas(sk)
	if len(gotchas) < 2 {
		return // threshold: the same skill needs 2+ distilled gotchas
	}
	dir := e.skillDirs[sk]
	if dir == "" {
		return // skill location unknown — never guess where to write
	}
	if n := skill.ProposeGotchasPatch(dir, sk, gotchas); n > 0 {
		if e.progressHandler != nil {
			e.progressHandler(e.state, fmt.Sprintf("📝 %s: %d gotcha(s) from your repairs — proposed patch in %s (review & merge into SKILL.md)", sk, n, filepath.Join(dir, "GOTCHAS.md")))
		}
	}
}

// tagSkillLesson is the minimal self-evolution hook: when a repair produced a
// durable lesson and a skill was loaded this turn, tag the lesson with that
// skill's name in project memory (## Skill Notes) so future sessions learn
// WHICH workflow the gotcha belongs to — the first step toward skills that
// evolve from real repairs. Memory-only; skill files are never written.
func (e *Engine) tagSkillLesson(lesson string) {
	if e.mem == nil || lesson == "" {
		return
	}
	for sk := range e.loadedSkills {
		_, _ = e.mem.Retain("Skill Notes", sk+": "+lesson)
		break
	}
}

// distillLesson condenses a repaired verification failure into one durable
// line for project memory. It builds a deterministic fallback first (never
// blocks the turn) and, when LLM review is enabled, polishes it into a terse
// insight via one bounded completion — the LLM version carries the "why"/fix,
// the deterministic version is always safe if that call fails.
func (e *Engine) distillLesson(ctx context.Context) string {
	files := e.lessonFiles
	if len(files) == 0 {
		files = e.editedFiles
	}
	fileList := "unknown files"
	if len(files) > 0 {
		fileList = strings.Join(files, ", ")
		if len(fileList) > 120 {
			fileList = fileList[:120] + "…"
		}
	}
	errHead := e.lastVerifyErr
	if len(errHead) > 300 {
		errHead = errHead[:300] + "…"
	}
	fallback := "Verification failed on " + fileList + ": " + errHead + " — fixed after " + formatTSRAttempts(e.tsrAttempts) + "."
	if !e.reviewLLMEnabled {
		return fallback
	}

	reviewCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req := provider.CompletionRequest{
		Model: e.model,
		Messages: []provider.Message{{
			Role:    "user",
			Content: "You are a codebase lesson extractor. One sentence (max 40 words) about this failure so a future session avoids repeating the mistake. Focus on the root cause / fix, not the process. Files: " + fileList + "\nFailure was:\n" + errHead + "\n\nReply with only the lesson sentence, no prefix, no quotes.",
		}},
		Temperature: 0.2,
	}
	resp, err := e.complete(reviewCtx, req)
	if err != nil || resp == nil {
		return fallback
	}
	lesson := strings.TrimSpace(resp.Content)
	if lesson == "" || len(lesson) > 400 {
		return fallback
	}
	return lesson
}

// formatTSRAttempts renders the repair attempt count for lesson text.
func formatTSRAttempts(n int) string {
	switch n {
	case 1:
		return "1 repair attempt"
	case 2:
		return "2 repair attempts"
	default:
		return fmt.Sprintf("%d repair attempts", n)
	}
}

// buildSystemPrompt renders the full system prompt for the current mode via the
// layered prompt builder (internal/prompt): identity + project context, repo
// map, skills catalog (relevance-filtered), LSP state, memory warm start,
// pre-flight diagnostics, plan-mode gate, and the tunable mode rules. Called
// ONCE per turn and cached on the engine so every loop iteration sends
// byte-identical leading tokens (provider prompt caching). The warm-start
// memory excerpt is derived from the user's initial query.
func (e *Engine) buildSystemPrompt(currentMode string, iteration int, onUpdate TurnOutputHandler) string {
	var activePlanStr string
	if curPlan, err := plan.LoadCurrentPlan(e.repoRoot); err == nil && curPlan != nil && len(curPlan.Steps) > 0 {
		activePlanStr = plan.RenderMarkdownPlan(curPlan)
	}

	in := &prompt.Input{
		Mode:          currentMode,
		Iteration:     iteration,
		ProjectCtx:    e.projectCtx,
		RepoMap:       e.repoMap,
		Stacks:        e.stacks,
		Skills:        e.skillsEntries,
		UserPrompt:    e.context.LastUserPrompt(),
		ScopeHint:     e.scopeHint,
		LSPAvailable:  e.lspAvailable,
		Preflight:     e.preflightBlock,
		PreflightAuto: e.preflightAutoFix,
		PlanMode:      e.planMode,
		ActivePlan:    activePlanStr,
		Tuning:        e.tuning,
	}
	if e.mem != nil {
		// Adaptive warm-start budget (memory/adaptive caps): before injecting
		// memory into the system prompt, shrink its byte cap to fit the
		// REMAINING window. A 25KB default block is affordable on a fresh turn
		// but counter-productive when the turn is already near the compaction
		// threshold — it would just buy a compaction the model didn't need.
		if remain := e.context.MaxWindow() - e.context.TotalContextTokens(); remain < 32*1024 {
			budget := max(remain / 2, 4 * 1024)
			e.mem.SetWarmStartBudget(budget)
		} else {
			e.mem.SetWarmStartBudget(0)
		}
		in.MemoryWarm = e.mem.WarmStartRelevant(in.UserPrompt)
		// In MINER mode, augment the memory query with explored file names so
		// warm-start recalls facts related to the specific files being learned,
		// not just the generic "learn the codebase" prompt.
		if currentMode == "MINER" && e.exploreQuery != "" {
			in.MemoryWarm = e.mem.WarmStartRelevant(in.UserPrompt + " " + e.exploreQuery)
		}
		if in.MemoryWarm != "" && onUpdate != nil && iteration == 1 {
			onUpdate(e.state, "🧠 Warm Start: Recalled project memory & hot files")
		}
	}

	// Smart Context Graph: inject knowledge hints at turn-start so the model
	// can avoid re-scanning previously analyzed files whose content hash is
	// unchanged. Only on iteration 1 (system prompt is cached per-turn).
	if e.knowledge != nil && iteration == 1 {
		hints, err := e.knowledge.QueryKnowledge(in.UserPrompt)
		if err == nil && len(hints) > 0 {
			in.KnowledgeHints = store.FormatKnowledgeHints(hints, in.UserPrompt)
			if onUpdate != nil {
				onUpdate(e.state, "🧠 Smart Context: Recalled previously analyzed files")
			}
		}
		// Self-aware context: distilled facts/decisions/gotchas/hot files from
		// past sessions, recalled by relevance to the current prompt. When the
		// prompt is vague (few keyword matches), seed the top-weighted notes
		// anyway so the agent starts each session with an architecture-aware
		// mental model — without re-reading the whole codebase. Bounded and
		// advisory: it never suppresses exploration.
		notes, _ := e.knowledge.QueryNotesForPrompt(in.UserPrompt, 8)
		if len(notes) < 3 {
			if extra, _ := e.knowledge.TopNotes(
				[]store.NoteKind{store.NoteFact, store.NoteDecision, store.NoteGotcha, store.NoteHotfile}, 8); len(extra) > 0 {
				notes = mergeUniqueNotes(notes, extra)
			}
		}
		if len(notes) > 0 {
			in.NotesHints = formatNotesHints(notes)
			if onUpdate != nil {
				onUpdate(e.state, "🧠 Self-Aware Context: Recalled past insights")
			}
		}
	}
	p, _ := prompt.Assemble(in)
	return p
}

// formatNotesHints renders distilled self-aware notes (facts/decisions/gotchas/
// hot files) as a compact, relevance-ordered block for the system prompt.
func formatNotesHints(notes []store.Note) string {
	var b strings.Builder
	for _, n := range notes {
		fmt.Fprintf(&b, "• [%s] %s — %s", strings.ToUpper(string(n.Kind)), n.Subject, n.Content)
		if n.Provenance != "" {
			fmt.Fprintf(&b, " (%s)", n.Provenance)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// mergeUniqueNotes appends notes from `extra` that are not already present
// (by subject), keeping the relevance-ordered `base` first. Bounded by the
// base slice's original length guard so the primer never explodes.
func mergeUniqueNotes(base, extra []store.Note) []store.Note {
	seen := make(map[string]bool, len(base))
	for _, n := range base {
		seen[n.Subject] = true
	}
	for _, n := range extra {
		if !seen[n.Subject] {
			seen[n.Subject] = true
			base = append(base, n)
		}
		if len(base) >= 8 {
			break
		}
	}
	return base
}

// toolsForMode returns the tool surface exposed to the model for the current
// mode. Structural pruning: read-only modes (PLANNER, MINER) simply DO NOT
// receive the mutating tools — write_file/edit_file/delete_file (and the LSP
// tools that write to disk, lsp_fix/lsp_rename) are never offered, so the
// model cannot propose them, cannot waste rounds on guard messages, and pays
// fewer schema tokens per request. PLANNER additionally drops bash entirely.
// BUILDER gets the full surface. The runtime mode guards stay as a backstop
// (MCP/subagent tools bypass this filter), but the LLM is never tempted by
// tools its mode forbids.
func (e *Engine) toolsForMode(mode string) []provider.ToolDefinition {
	defs := e.tools.Definitions()
	// Tool-description lean (P5): trim verbose schemas in every mode so more of
	// the window is free for real task context. Applied before mode pruning.
	if e.toolDescBudget > 0 {
		lean := make([]provider.ToolDefinition, len(defs))
		for i, d := range defs {
			if len(d.Description) > e.toolDescBudget {
				d.Description = strings.TrimSpace(d.Description[:e.toolDescBudget]) + "…"
			}
			lean[i] = d
		}
		defs = lean
	}
	if mode == "BUILDER" {
		return defs
	}
	exclude := map[string]bool{
		"write_file":  true,
		"edit_file":   true,
		"delete_file": true,
		"lsp_fix":     true,
		"lsp_rename":  true,
	}
	if mode == "PLANNER" {
		exclude["bash"] = true
	}
	out := make([]provider.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		if exclude[d.Name] {
			continue
		}
		if e.toolDescBudget > 0 && len(d.Description) > e.toolDescBudget {
			d.Description = strings.TrimSpace(d.Description[:e.toolDescBudget]) + "…"
		}
		out = append(out, d)
	}
	return out
}

// LastFallbackModel returns the fallback model used in the most recent turn
// ("" when the primary provider served it).
func (e *Engine) LastFallbackModel() string {
	return e.lastFallback
}

// LastFallbackReason returns the primary provider's error that triggered the
// fallback in the most recent turn ("" when the primary served it). This is
// how the UI tells the user WHY — e.g. a FreeBuff duration/queue limit or an
// invalid model — rather than silently swapping providers.
func (e *Engine) LastFallbackReason() string {
	return e.lastFallbackReason
}

// recordExplored keeps a capped, de-duplicated list of files/directories the
// model has touched this turn (read_file, list_dir, grep, glob, bash find).
func (e *Engine) recordExplored(tc provider.ToolCall) {
	var target string
	var m map[string]any
	if json.Unmarshal([]byte(tc.Arguments), &m) == nil {
		switch tc.Name {
		case "read_file", "edit_file", "write_file":
			target, _ = m["path"].(string)
			if tc.Name == "edit_file" || tc.Name == "write_file" {
				e.exploredStalls = 0
			}
			// A deliberate line-range read of a large file (read_file with
			// start_line) is genuine progress — the model is covering the file in
			// sections — NOT spinning, even though the path is the same. Reset the
			// stall counter so the budget does not kill a model methodically
			// reading a big file (the exact abort the user kept hitting). True
			// spinning is still caught: identical repeated calls are blocked by
			// repeat detection, and the absolute cap bounds everything.
			if tc.Name == "read_file" {
				if _, hasRange := m["start_line"]; hasRange {
					e.exploredStalls = 0
				}
			}
		case "list_dir", "grep", "glob":
			target, _ = m["path"].(string)
			if target == "" {
				target, _ = m["pattern"].(string)
			}
		case "bash":
			// Every bash command counts as an explored target (prefixed so the
			// space-filter in usageFn/MINER never mistakes a command for a file
			// path). Same-path reads/dedup make REPEATED commands stall — but a
			// model running DIFFERENT commands (git log, ls, grep -rn, find) is
			// genuinely exploring, and the prompts even encourage bash for repo
			// state. Previously only "find" was credited, so bash-based
			// exploration was miscounted as spinning and aborted early — the
			// same class of bug as the range-read fix.
			if cmd, _ := m["command"].(string); strings.TrimSpace(cmd) != "" {
				target = "bash: " + strings.TrimSpace(cmd)
			}
		}
	}
	if target == "" {
		return
	}
	if slices.Contains(e.explored, target) {
		return
	}
	e.explored = append(e.explored, target)
	if len(e.explored) > 12 {
		e.explored = e.explored[len(e.explored)-12:]
	}
}

// wasExplored returns true if the given absolute file path was read or searched
// during this turn. Used by the Off-Task Edit Gate.
func (e *Engine) wasExplored(absPath string) bool {
	for _, entry := range e.explored {
		// Normalize: entry may be a bare path or "bash: <cmd>"
		if strings.HasPrefix(entry, "bash: ") {
			continue
		}
		entryAbs, err := filepath.Abs(entry)
		if err != nil {
			entryAbs = entry
		}
		if entryAbs == absPath {
			return true
		}
		// Also allow if the explored entry is a parent directory of the target
		// (e.g. the model listed_dir the locales folder and then edits id.json)
		if strings.HasPrefix(absPath, entryAbs+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// exploredPathList returns a compact comma-separated list of explored paths
// for use in Off-Task Edit Gate messages.
func (e *Engine) exploredPathList() string {
	var parts []string
	cwd, _ := os.Getwd()
	for _, entry := range e.explored {
		if strings.HasPrefix(entry, "bash: ") {
			continue
		}
		rel := strings.TrimPrefix(entry, cwd+string(os.PathSeparator))
		parts = append(parts, rel)
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

// projectContextBlock renders the injected project overview (tree + docs) as
// a system-prompt section, or an empty string when none was provided.

// exploredSummary renders the list of files/directories the model has already
// read or searched, used by the tool-budget reminders and the abort message.
func (e *Engine) exploredSummary() string {
	if len(e.explored) == 0 {
		return ""
	}
	return "\n\nFiles you have already examined: " + strings.Join(e.explored, ", ")
}

// diagLineRe matches a scanned diagnostic line: "  error [deprecated] 12:5  msg".
var diagLineRe = regexp.MustCompile(`^\s*(error|warning|info|hint)(?:\s*\[(deprecated|unnecessary)\])?\s+(\d+):(\d+)\s+(.*)$`)

func looksLikeLSPFixTask(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if strings.HasSuffix(q, "?") {
		return false
	}
	return strings.Contains(q, "lint") || strings.Contains(q, "lsp") ||
		strings.Contains(q, "warning") || strings.Contains(q, "diagnostic") ||
		strings.Contains(q, "deprecat") || strings.Contains(q, "vet") ||
		strings.Contains(q, "tsc")
}

// preflightLSP runs lsp_scan proactively and packs the result — plus the exact
// code window around each diagnostic — into a single block for the first prompt.
// It is BEST-EFFORT: any failure (no LSP, unreadable file) degrades gracefully to
// just the scan report rather than erroring the turn. Path resolution uses the
// repo root so relative paths from the scan map back to real files.
func (e *Engine) preflightLSP(ctx context.Context) string {
	scanTool := e.tools.ToolByName("lsp_scan")
	if scanTool == nil {
		return ""
	}
	// Bound the scan so a slow language server cannot stall turn start.
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := scanTool.Execute(sctx, "{}")
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	if strings.Contains(out, "No supported source files") {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("PRE-GATHERED LSP DIAGNOSTICS (already scanned for you — do NOT call lsp_scan again). The exact code window around each item is shown below, so you already have the file:line AND the lines to change. FIX EACH ITEM IN PLACE: use edit_file(start_line,end_line) or lsp_fix DIRECTLY — do NOT read the file first. If the request is only to CHECK whether warnings/errors are resolved, just report what this scan shows (resolved vs remaining) — do NOT read files to verify; the scan IS the source of truth. Batch all fixes in one message, then verify ONCE.\n\n")
	sb.WriteString(out)

	// Pack the actual code around each diagnostic so the model can fix in place
	// without a separate read round. Parse the scan report's file→line structure.
	lines := strings.Split(out, "\n")
	root := e.repoRoot
	if root == "" {
		if wd, werr := os.Getwd(); werr == nil {
			root = wd
		}
	}
	var curFile string
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		// A non-indented line with no "N:M" diagnostic marker is a file path.
		if !strings.HasPrefix(ln, " ") && !strings.HasPrefix(ln, "\t") && !diagLineRe.MatchString(ln) {
			curFile = trimmed
			continue
		}
		m := diagLineRe.FindStringSubmatch(ln)
		if m == nil || curFile == "" {
			continue
		}
		lineNo := atoiSafe(m[3])
		if lineNo <= 0 {
			continue
		}
		abs := curFile
		if !strings.HasPrefix(abs, "/") && root != "" {
			abs = root + string(os.PathSeparator) + curFile
		}
		win := readLinesWindow(abs, lineNo-3, lineNo+3)
		if win == "" {
			continue
		}
		fmt.Fprintf(&sb, "\n--- %s:%d ---\n%s\n", curFile, lineNo, win)
	}
	return sb.String()
}

// readLinesWindow returns lines [lo,hi] (1-indexed, clamped) of the file, or ""
// if the file is unreadable. Best-effort; never errors.
func readLinesWindow(path string, lo, hi int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if lo < 1 {
		lo = 1
	}
	var sb strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		if n < lo {
			continue
		}
		if n > hi {
			break
		}
		fmt.Fprintf(&sb, "%d: %s\n", n, sc.Text())
	}
	_ = sc.Err()
	if sb.Len() == 0 {
		return ""
	}
	return strings.TrimRight(sb.String(), "\n")
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// guardPreflightRedundant returns a non-empty rejection when a tool call is
// redundant with the pre-flight diagnostics already packed into context:
//   - read_file on a file whose diagnostics + code window are ALREADY in the
//     PRE-GATHERED LSP DIAGNOSTICS block (whether whole-file or line-range — the
//     window is already there, so any re-read is pure waste that makes the agent
//     spin instead of fixing);
//   - lsp_scan called AGAIN after pre-flight already scanned and packed results.
//
// The model must fix in place (edit_file/lsp_fix) or re-run lsp_scan ONLY when it
// genuinely needs a fresh scan — not re-read/re-scan what it was already given.
func (e *Engine) guardPreflightRedundant(name, argsJSON string) string {
	if !e.preflightActive {
		return ""
	}
	switch name {
	case "read_file":
		var m map[string]any
		if json.Unmarshal([]byte(argsJSON), &m) == nil {
			if p, _ := m["path"].(string); p != "" {
				if strings.Contains(e.preflightBlock, p) {
					return "READ BLOCKED: " + p + " already has its diagnostics + code window packed in PRE-GATHERED LSP DIAGNOSTICS — re-reading it wastes tokens and turns. Fix in place with edit_file(start_line,end_line) or lsp_fix, or re-run lsp_scan to verify. Do NOT read this file again."
				}
			}
		}
	case "lsp_scan":
		return "lsp_scan already ran during pre-flight and its result is packed in PRE-GATHERED LSP DIAGNOSTICS — do NOT scan again. Fix the listed items in place with edit_file/lsp_fix."
	}
	return ""
}

// looksLikeImplTask reports whether a user query is a multi-step implementation
// task that benefits from a PLAN pass before any edits — so the engine can gate
// it behind plan-then-act. Pure read/question prompts and single-shot lint-fix
// tasks (handled by pre-flight) are deliberately excluded.
func looksLikeImplTask(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" || strings.HasSuffix(q, "?") {
		return false
	}
	// Pure read/question phrasing -> not an implementation task.
	readPrefixes := []string{"explain", "what", "how", "why", "show", "describe",
		"where", "when", "who", "list", "summar", "find", "search", "which",
		"tell me", "does ", "is ", "are ", "can you explain"}
	for _, w := range readPrefixes {
		if strings.HasPrefix(q, w) {
			return false
		}
	}
	// Multi-step build/implement intent. Deliberately excludes "add", "refactor",
	// "fix", "support" — those are often single-file/small edits that B5
	// (proportionality) says should run with minimal ceremony, not a plan gate.
	implWords := []string{"implement", "build a", "build the", "create",
		"scaffold", "set up", "new feature", "new module", "new service",
		"new endpoint", "migrate", "introduce"}
	for _, w := range implWords {
		if strings.Contains(q, w) {
			return true
		}
	}
	return false
}

// finalSynth is the graceful turn-abort path (max iterations or cost budget
// exceeded): one bounded completion with tools disabled that synthesizes a
// final answer from the explored context, with a deterministic fallback that
// NEVER surfaces a raw error. reason is the abort headline (no "⚠️ " prefix);
// marker labels the synthesized answer in the history.
func (e *Engine) finalSynth(ctx context.Context, reason string, marker string) (string, error) {
	synthPrompt := fmt.Sprintf("⚠️ %s Tools are now DISABLED for this final response. You MUST now synthesize a helpful, comprehensive response for the user in the user's language based ONLY on the files and context you have explored so far. Answer as much of the user's prompt as possible, summarize your findings, and clearly state any missing context or next steps.%s", reason, e.exploredSummary())
	_ = e.context.AppendUserMessage(synthPrompt)

	// Reuse the per-turn cached system prompt so the final synthesis keeps the
	// byte-identical prefix (provider prompt cache hits).
	localSysPrompt := e.sysPromptCached
	if localSysPrompt == "" {
		// Rare: the turn aborted before the first iteration built the cached
		// prompt. Build a full one via the layered prompt builder.
		localSysPrompt = e.buildSystemPrompt(e.Mode(), 1, nil)
	}
	reqMessages := append([]provider.Message{
		{Role: "system", Content: localSysPrompt},
	}, e.fitMessages(localSysPrompt)...)

	synthReq := provider.CompletionRequest{
		Model:       e.model,
		Messages:    reqMessages,
		Tools:       nil,
		Temperature: 0.2,
	}

	synthResp, synthErr := e.complete(ctx, synthReq)
	if synthErr == nil && synthResp != nil && strings.TrimSpace(synthResp.Content) != "" {
		res := synthResp.Content + "\n\n---\n*⚠️ Respon ini dirangkum dari hasil eksplorasi parsial (" + marker + ").* "
		_ = e.context.AppendAssistantTurn(e.Mode(), e.model, "", res, nil)
		e.state = StateDone
		return res, nil
	}

	// Deterministic fallback — NEVER surface a raw error to the user.
	e.state = StateDone
	fallbackMsg := fmt.Sprintf("⚠️ %s\n\nHere is what was verified from the explored codebase:\n%s", reason, e.exploredSummary())
	if e.lastReasoning != "" {
		fallbackMsg += "\n\n**Last Working Focus**: " + e.lastReasoning
	}
	fallbackMsg += "\n\n---\n*⚠️ Respon ini dirangkum dari hasil eksplorasi parsial (" + marker + ").* "
	_ = e.context.AppendAssistantTurn(e.Mode(), e.model, "", fallbackMsg, nil)
	return fallbackMsg, nil
}

// isRepeatToolCall reports whether a tool call is an exact repeat of the
// previous one in this turn (same name and identical arguments).
// isFileMutationTool reports whether a tool name changes the filesystem
// (write/edit/create/delete). Rounds that execute these are credited as
// productive for the Productive Token Ratio metric.
func isFileMutationTool(name string) bool {
	switch name {
	case "write_file", "edit_file", "create_file", "delete_file":
		return true
	}
	return false
}

func isRepeatToolCall(tc, prev provider.ToolCall) bool {
	if prev.Name == "" {
		return false
	}
	if tc.Name != prev.Name {
		return false
	}
	// Normalize whitespace-only differences in the arguments JSON so
	// formatting changes don't defeat the detection.
	a := strings.TrimSpace(tc.Arguments)
	b := strings.TrimSpace(prev.Arguments)
	if a == b {
		return true
	}
	// Structural comparison: identical JSON objects with different key order
	// should still count as repeats.
	var ma, mb map[string]any
	if json.Unmarshal([]byte(a), &ma) == nil && json.Unmarshal([]byte(b), &mb) == nil {
		ja, _ := json.Marshal(ma)
		jb, _ := json.Marshal(mb)
		return string(ja) == string(jb)
	}
	return false
}

func normalizedToolCallKey(tc provider.ToolCall) string {
	a := strings.TrimSpace(tc.Arguments)
	var m map[string]any
	if json.Unmarshal([]byte(a), &m) == nil {
		b, _ := json.Marshal(m)
		return tc.Name + ":" + string(b)
	}
	return tc.Name + ":" + a
}

// bashFamily returns the leading command word of a bash tool call (e.g.
// "git" for `git status`, "grep" for `grep -rn foo`). It is the coarse signal
// used by the fuzzy loop-break to detect same-strategy spins where the
// arguments change but the command family never does. Empty for non-bash use.
func bashFamily(argsJSON string) string {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Command == "" {
		return ""
	}
	cmd := strings.TrimSpace(args.Command)
	if i := strings.IndexAny(cmd, " \t"); i > 0 {
		cmd = cmd[:i]
	}
	return strings.ToLower(cmd)
}

// advanceBashFamily tracks the consecutive same-family bash streak across
// ROUNDS: given the current round's family and the engine's last-round family
// + streak, it returns the updated (family, streak) pair. A switch to a
// different family (or an empty family) resets the streak to 1.
func advanceBashFamily(fam, last string, streak int) (string, int) {
	if fam != "" && fam == last {
		return fam, streak + 1
	}
	return fam, 1
}

// complete runs a completion through the primary adapter, streaming when the
// adapter supports it and a stream handler is wired.
func (e *Engine) complete(ctx context.Context, req provider.CompletionRequest) (resp *provider.CompletionResponse, err error) {
	// Panic recovery: a provider adapter panic (nil deref, unexpected JSON)
	// must NEVER crash the engine loop — recover and surface as a plain error
	// so fallback routing (circuit breaker / fallback model) can engage.
	defer func() {
		if rc := recover(); rc != nil {
			fmt.Fprintf(os.Stderr, "[WARN] engine.complete panicked: %v — returning error, turn continues\n", rc)
			err = fmt.Errorf("provider panicked: %v", rc)
		}
	}()
	return e.completeWith(ctx, e.adapter, req)
}

// modelCompactionSummary asks the active model to write the structured 5-part
// compaction summary from the messages that are about to be dropped. Returns
// ok=false on any failure (network, bad JSON, empty) so the caller falls back
// to the deterministic boilerplate summary — compaction must never break a turn.
func (e *Engine) modelCompactionSummary(ctx context.Context) (bcontext.CompactionSummary, bool) {
	msgs := e.context.Messages()
	if len(msgs) == 0 {
		return bcontext.CompactionSummary{}, false
	}

	// Compact() keeps the last 4 messages; summarize exactly what gets dropped.
	keep := 4
	if len(msgs) <= keep {
		keep = 0
	}
	drop := msgs[:len(msgs)-keep]

	transcript := compactionTranscript(drop)
	if strings.TrimSpace(transcript) == "" {
		return bcontext.CompactionSummary{}, false
	}

	prompt := "You are summarizing an ongoing software-agent conversation for context compaction. " +
		"Below is the transcript that is about to be compacted away. Produce a concise structured summary " +
		"that captures everything a continuing agent still needs to know. " +
		"Respond with ONLY a JSON object (no markdown fences, no prose) using EXACTLY this schema:\n" +
		"{\"goal\": string, \"files_touched\": [string], \"decisions_made\": [string], " +
		"\"next_action\": string, \"constraints\": string, \"open_questions\": [string], " +
		"\"last_known_state\": string}\n\n" +
		"next_action = the single most useful next step for the continuing agent. " +
		"constraints = hard rules it must not violate (verified facts, things already tried that failed, " +
		"scope boundaries). Keep each field tight.\n\n" +
		"TRANSCRIPT TO COMPACT:\n" + transcript

	summCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := e.adapter.Complete(summCtx, provider.CompletionRequest{
		Model:       e.compactionModel(),
		Messages:    []provider.Message{{Role: "user", Content: prompt}},
		Temperature: 0.2,
	})
	if err != nil {
		return bcontext.CompactionSummary{}, false
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return bcontext.CompactionSummary{}, false
	}

	summary, ok := parseCompactionJSON(resp.Content)
	if !ok {
		return bcontext.CompactionSummary{}, false
	}
	if strings.TrimSpace(summary.Goal) == "" && strings.TrimSpace(summary.LastKnownState) == "" {
		return bcontext.CompactionSummary{}, false
	}
	return summary, true
}

// compactionModel returns the model to use for compaction summarization: the
// cheaper routing model when configured, otherwise the main synthesis model.
func (e *Engine) compactionModel() string {
	if e.compactModel != "" {
		return e.compactModel
	}
	return e.model
}

// compactionTranscript renders the to-be-dropped messages as a bounded text
// transcript so a summarizing model sees real context without re-bloating the
// window we are trying to shrink.
func compactionTranscript(msgs []provider.Message) string {
	var sb strings.Builder
	const maxTranscriptChars = 60000
	for _, m := range msgs {
		role := m.Role
		if m.ToolCallID != "" {
			role = "tool_result"
		}
		var part strings.Builder; part.WriteString(role);part.WriteString(": ")
		if m.Content != "" {
			part.WriteString(m.Content)
		}
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&part, "\n  [tool_call %s(%s)]", tc.Name, tc.Arguments)
			}
		}
		if sb.Len()+len(part.String()) > maxTranscriptChars {
			sb.WriteString("\n...[transcript truncated for compaction]...")
			break
		}
		sb.WriteString(part.String())
		sb.WriteString("\n")
	}
	return sb.String()
}

// parseCompactionJSON extracts a CompactionSummary from a model reply, tolerating
// surrounding markdown fences and stray prose before/after the JSON object.
func parseCompactionJSON(raw string) (bcontext.CompactionSummary, bool) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return bcontext.CompactionSummary{}, false
	}
	end := strings.LastIndexByte(raw, '}')
	if end < start {
		return bcontext.CompactionSummary{}, false
	}
	var summary bcontext.CompactionSummary
	if err := json.Unmarshal([]byte(raw[start:end+1]), &summary); err != nil {
		return bcontext.CompactionSummary{}, false
	}
	return summary, true
}

func (e *Engine) completeWith(ctx context.Context, a provider.ProviderAdapter, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	// Snapshot the live handlers ONCE. Progressing adapters (e.g. the opencode
	// CLI) forward output from their own goroutines that can outlive this call:
	// the subprocess keeps streaming stderr while the turn is already wrapping
	// up, and RunTurn's deferred reset sets e.progressHandler to nil on exit.
	// Reading the field inside the callback would then nil-panic (and race the
	// reset). Capturing the value here keeps in-flight goroutines safe — the
	// callback checks its own snapshot, never the field.
	progress := e.progressHandler
	stream := e.streamHandler
	st := e.state
	var resp *provider.CompletionResponse
	var err error
	if sa, ok := a.(provider.StreamingAdapter); ok && stream != nil {
		resp, err = sa.StreamComplete(ctx, req, stream)
	} else if pa, ok := a.(provider.ProgressingAdapter); ok && progress != nil {
		resp, err = pa.CompleteWithProgress(ctx, req, func(line string) {
			if progress != nil {
				progress(st, line)
			}
		})
	} else {
		resp, err = a.Complete(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	// Live cost tracking: accumulate reported usage into the session tracker.
	if resp != nil && e.usage != nil {
		e.usage.Record(req.Model, resp.Usage)
	}
	// Per-turn cost accumulation for the USD budget: providers report exact
	// token usage; multiply by the model list price, applying the prompt-cache
	// discount for cache-hit input tokens.
	if resp != nil && (resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0) {
		e.costUSD += provider.EstimateCostUSDWithCache(req.Model, resp.Usage.PromptTokens, resp.Usage.PromptCacheHitTokens, resp.Usage.CompletionTokens)
		e.turnTokens += resp.Usage.TotalTokens
		e.lastRoundOutput = resp.Usage.CompletionTokens
	}
	return resp, nil
}

// fitMessages returns the conversation messages trimmed from the oldest end so
// the full wire request — system prompt + messages + a reserved completion
// budget — fits the model's context window. It is the final safety net against
// context overflow: a too-large request makes some providers return a 200 with
// empty content (instead of an error), which the UI surfaces as "the model
// returned an empty response". NeedsCompaction handles the common case; this
// guarantees the request that actually goes on the wire is never over budget,
// even when the system prompt alone is large or a single message is huge.
func (e *Engine) fitMessages(sysPrompt string) []provider.Message {
	msgs := e.context.Messages()
	if len(msgs) == 0 {
		return msgs
	}
	reserved := int(float64(e.context.MaxWindow()) * 0.15) // completion budget
	limit := e.context.MaxWindow() - reserved
	if limit < 0 {
		limit = e.context.MaxWindow()
	}
	budget := limit - bcontext.EstimateTokens(sysPrompt)
	if budget <= 0 {
		// System prompt alone eats the whole window — keep only the newest
		// message so the request is never empty. This is a hard overflow: even
		// the smallest request does not fit, so flag it for the learner.
		e.lastOverflow = true
		return []provider.Message{msgs[len(msgs)-1]}
	}
	// Accumulate from the newest message backward; stop before the budget breaks.
	total := 0
	start := len(msgs)
	for i, msg := range slices.Backward(msgs) {
		c := bcontext.EstimateTokens(msg.Content) + bcontext.EstimateTokens(msg.Reasoning)
		if total+c > budget && start < len(msgs) {
			break
		}
		total += c
		start = i
	}
	return msgs[start:]
}

// completeTurn runs a completion through the adaptive router. It returns the
// response, plus the fallback model that served it ("" when the primary
// answered). Routing policy (see FallbackPolicy): retry the primary once on
// transient errors, skip providers in cooldown, and honor the user's
// fallback policy. Every non-2xx/network outcome feeds the circuit breaker so
// a chronically failing provider is skipped on later turns instead of burning
// a full timeout each time.
func (e *Engine) completeTurn(ctx context.Context, req provider.CompletionRequest, onUpdate TurnOutputHandler) (*provider.CompletionResponse, string, error) {
	timeout := defaultModelCallTimeout
	// Log that we are now blocking on the LLM so the activity log is never
	// silent during a slow generation (otherwise it looks "stuck").
	if onUpdate != nil {
		onUpdate(e.state, fmt.Sprintf("⏳ %s: generating response…", req.Model))
	}

	// Fast path: the primary is cooling down from a recent failure — don't
	// burn a full timeout on it again; go straight to the first healthy
	// fallback. The cooldown is a hint, not a hard block: if nothing is
	// available we still try the primary as a last resort.
	if cd, _ := e.health.inCooldown(e.primaryID); cd {
		if resp, fb, fbErr := e.tryFallbacks(ctx, req, onUpdate); resp != nil {
			return resp, fb, nil
		} else if fbErr != nil {
			return nil, "", fbErr
		}
		// fall through → try the primary anyway
	}

	// Each attempt gets its OWN timeout so a single slow call can't hang the
	// whole turn — on timeout we fall back to the next healthy model instead.
	callCtx, callCancel := context.WithTimeout(ctx, timeout)
	resp, err := e.complete(callCtx, req)
	callCancel()
	if err == nil {
		e.health.recordSuccess(e.primaryID)
		return resp, "", nil
	}

	primaryErr := err
	e.health.recordFailure(e.primaryID)
	if e.fallbackPolicy == FallbackPrimaryOnly {
		return nil, "", primaryErr
	}
	// Surface a timeout clearly so the user knows we're re-routing, not frozen.
	if errors.Is(err, context.DeadlineExceeded) && onUpdate != nil {
		onUpdate(e.state, fmt.Sprintf("⚠️ %s timed out after %s — routing to fallback…", req.Model, timeout))
	}

	// A transient primary failure (stream stall, timeout, 429/5xx) deserves
	// ONE retry on the same provider before switching models. Permanent errors
	// (auth, invalid model, user ESC) are never retried.
	if provider.IsRetryable(err) {
		retryCtx, retryCancel := context.WithTimeout(ctx, timeout)
		rresp, rerr := e.complete(retryCtx, req)
		retryCancel()
		if rerr == nil {
			e.health.recordSuccess(e.primaryID)
			return rresp, "", nil
		}
	}

	// Primary still failing — route to the next healthy fallback.
	e.lastFallbackReason = primaryErr.Error()
	if resp, fb, fbErr := e.tryFallbacks(ctx, req, onUpdate); resp != nil {
		return resp, fb, nil
	} else if fbErr != nil {
		return nil, "", fbErr
	}
	return nil, "", primaryErr
}

// tryFallbacks routes the completion to the first healthy fallback in
// registration order, skipping providers currently in cooldown. Returns the
// response plus the fallback model on success. fbErr is non-nil only when a
// fallback was SELECTED but failed; (nil, "", nil) means nothing was tried
// (no fallbacks, all in cooldown, or the confirm policy declined).
func (e *Engine) tryFallbacks(ctx context.Context, req provider.CompletionRequest, onUpdate TurnOutputHandler) (resp *provider.CompletionResponse, fallbackModel string, fbErr error) {
	var lastErr error
	for _, fb := range e.fallbacks {
		if cd, _ := e.health.inCooldown(fb.ID); cd {
			continue
		}
		// Confirm policy: only ask when the fallback is a DIFFERENT vendor
		// than the primary; same-vendor fallbacks (e.g. a sibling model on the
		// same gateway) route automatically.
		if e.fallbackPolicy == FallbackConfirm && fb.Protocol != "" && fb.Protocol != e.primaryProtocol {
			ok, err := e.askFallbackConfirmation(fb.Model, fb.ID)
			if err != nil {
				return nil, "", err
			}
			if !ok {
				return nil, "", nil // user declined → stop routing
			}
		}
		fbReq := req
		fbReq.Model = fb.Model
		// Bound each fallback attempt so a slow fallback can't hang either.
		fbCtx, fbCancel := context.WithTimeout(ctx, defaultModelCallTimeout)
		fbResp, err := e.completeWith(fbCtx, fb.Adapter, fbReq)
		fbCancel()
		if err == nil {
			e.health.recordSuccess(fb.ID)
			e.lastFallback = fb.Model
			e.fallbackCount++
			if onUpdate != nil {
				onUpdate(e.state, fmt.Sprintf("⚠️ Primary provider failed — using fallback model %s", fb.Model))
			}
			return fbResp, fb.Model, nil
		}
		e.health.recordFailure(fb.ID)
		lastErr = err
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", nil
}

// askFallbackConfirmation asks the user before routing to a fallback from a
// different vendor than the primary. With no interactive layer wired it
// defaults to allow, preserving the auto behavior.
func (e *Engine) askFallbackConfirmation(model, _ string) (bool, error) {
	if e.askHandler == nil {
		return true, nil
	}
	ans, err := e.askHandler(fmt.Sprintf(
		"⚠️ Primary provider (%s) failed. Route this turn to fallback model %s?",
		e.primaryID, model,
	), []string{"✅ Use fallback", "🚫 Stop this turn"})
	if err != nil {
		return false, err
	}
	return !strings.Contains(ans, "Stop") && !strings.Contains(ans, "Deny"), nil
}

// hookRun fires a lifecycle hook event with structured env data. Output is
// returned so on-tool-call hooks can override tool results; other events
// discard it.
func (e *Engine) hookRun(ctx context.Context, ev hooks.Event, data map[string]string) string {
	if e.hooks == nil {
		return ""
	}
	return e.hooks.Run(ctx, ev, data)
}

// drainScouts delivers completed background scout findings into the model's
// context as tool results so the next reasoning step can use them.
func (e *Engine) drainScouts(onUpdate TurnOutputHandler) {
	if e.scouts == nil {
		return
	}
	reports := e.scouts.Drain()
	if len(reports) == 0 {
		return
	}
	for _, r := range reports {
		_ = e.context.AppendToolResult("scout_result", r)
		if onUpdate != nil {
			onUpdate(e.state, "📡 Scout findings delivered")
		}
	}
}

// errString converts an error into a stable string for hook env (empty when
// the error is nil).
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func formatToolCallInfo(name, argsJSON string) string {
	var m map[string]any
	if json.Unmarshal([]byte(argsJSON), &m) == nil {
		if name == "edit_file" {
			path, _ := m["path"].(string)
			return fmt.Sprintf("📝 edit_file %s", shortenPath(path))
		}
		if name == "write_file" {
			path, _ := m["path"].(string)
			return fmt.Sprintf("✍️  write_file %s", shortenPath(path))
		}
		if name == "delete_file" {
			path, _ := m["path"].(string)
			return fmt.Sprintf("🗑️  delete_file %s", shortenPath(path))
		}
		if name == "read_file" {
			path, _ := m["path"].(string)
			if s, ok := m["start_line"].(float64); ok && s > 0 {
				return fmt.Sprintf("📖  read_file %s:L%d", shortenPath(path), int(s))
			}
			return fmt.Sprintf("📖  read_file %s", shortenPath(path))
		}
		if path, ok := m["path"].(string); ok && path != "" {
			return fmt.Sprintf("🔧  %s %s", name, shortenPath(path))
		}
		if pattern, ok := m["pattern"].(string); ok && pattern != "" {
			return fmt.Sprintf("🔧  %s %s", name, pattern)
		}
		if cmd, ok := m["command"].(string); ok && cmd != "" {
			firstLine := strings.TrimSpace(strings.Split(cmd, "\n")[0])
			if len(firstLine) > 50 {
				firstLine = firstLine[:47] + "…"
			}
			return fmt.Sprintf("⚙️  %s %s", name, firstLine)
		}
	}
	return name
}

func shortenPath(p string) string {
	cwd, err := os.Getwd()
	if err == nil && strings.HasPrefix(p, cwd) {
		rel := strings.TrimPrefix(p, cwd)
		return strings.TrimPrefix(rel, string(os.PathSeparator))
	}
	return p
}
