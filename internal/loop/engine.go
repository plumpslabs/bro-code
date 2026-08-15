package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/hooks"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/provider"
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
	Adapter provider.ProviderAdapter
	Model   string
}

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
	state           LoopState
	fallbacks       []Fallback
	streamHandler   func(delta string)
	progressHandler TurnOutputHandler
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
	// lastToolCall tracks the previous tool invocation within a turn so the
	// loop guard can detect the model repeating the exact same call and stop
	// it from spinning (grep the same file 3x in a row, etc.).
	lastToolCall provider.ToolCall
	// lastToolCallRepeats counts consecutive identical repetitions of
	// lastToolCall. Persisted across loop iterations (not reset each turn
	// iteration) so a model stuck re-issuing the same call is caught.
	lastToolCallRepeats int
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
	// files by usage) injected alongside the project context so the agent
	// knows where to start without spending tokens re-discovering it.
	repoMap string
	// skillsCtx lists the available skills (from .agents/skills, .brocode/skills,
	// and the global skills dir) so the model knows what it can load and use.
	skillsCtx string
	// mem is the cross-session project memory store. When set, a warm-start
	// excerpt is injected into the system prompt and compaction summaries are
	// auto-merged into memory so future sessions start warm.
	mem *memory.Store
	// usageFn, when set, receives the files the model touched this turn (read,
	// searched, edited) so the UI can persist cross-session usage counts — the
	// "the more BroCode is used, the smarter it gets" layer.
	usageFn func(paths []string)
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
}

// SetHooks wires a lifecycle hooks manager. Nil disables hooks.
func (e *Engine) SetHooks(h *hooks.Manager) {
	e.hooks = h
}

// SetScoutManager wires the background scout manager. Nil disables scout
// result delivery (the scout tool itself then reports an error).
func (e *Engine) SetScoutManager(sm ScoutDrainer) {
	e.scouts = sm
}

// Tool-only budget: a model that keeps calling tools without answering is
// nudged EARLY (so a rabbit-hole exploration like "search the schema for more
// models" is cut before it burns a dollar of tokens) and aborted shortly
// after. 10 rounds of pure tool calls is plenty for a legitimate overview in
// a big monorepo; the first reminder lands right after that.
const (
	// toolWarnRounds — first "stop and answer" reminder.
	toolWarnRounds = 10
	// toolFinalWarnRounds — second, firmer warning.
	toolFinalWarnRounds = 12
	// maxToolOnlyRounds — abort once a spinning model stalls. Still well below
	// the 25-iteration cap.
	maxToolOnlyRounds = 14
	// maxToolOnlyAbsolute — unconditional abort even for a model that keeps
	// discovering new files (freedom is bounded, never infinite). 20 leaves
	// room for the final-warning pattern (ONE last targeted read, then the
	// answer) to complete instead of being cut right after the read.
	maxToolOnlyAbsolute = 20
)

// CalculateAdaptiveToolBudget dynamically scales the tool budget based on
// prompt complexity, active mode, and task keywords (Fase 2.2).
func CalculateAdaptiveToolBudget(prompt string, mode string) int {
	base := 14
	if mode == "MINER" || mode == "PLANNER" {
		base = 18
	}
	p := strings.ToLower(prompt)
	words := len(strings.Fields(p))
	if words > 30 || strings.Contains(p, "refactor") || strings.Contains(p, "audit") || strings.Contains(p, "migrate") || strings.Contains(p, "architecture") {
		base += 6
	}
	if base > 25 {
		return 25
	}
	if base < 8 {
		return 8
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

// SetProjectContext injects a compact structural overview of the project into
// every turn's system prompt (see search.BuildProjectContext). Empty disables.
func (e *Engine) SetProjectContext(pc string) {
	e.projectCtx = pc
}

// SetSkills injects the list of available skills into every turn's system
// prompt. Empty disables.
func (e *Engine) SetSkills(sc string) {
	e.skillsCtx = sc
}

// SetRepoMap injects the deterministic project map (entry points, structure,
// hot files by usage) into every turn's system prompt. Empty disables.
func (e *Engine) SetRepoMap(rm string) {
	e.repoMap = rm
}

// SetMemoryStore wires the cross-session project memory. When set, a
// warm-start excerpt of past sessions' learnings is injected into the system
// prompt, and compaction summaries are auto-merged back into memory.
func (e *Engine) SetMemoryStore(st *memory.Store) {
	e.mem = st
}

// SetUsageRecorder wires a callback that receives the files the model touched
// each turn (read, searched, edited) so usage can persist across sessions.
func (e *Engine) SetUsageRecorder(fn func(paths []string)) {
	e.usageFn = fn
}

// SetDiagnosticsChecker wires a native type-error checker (the UI provides
// one backed by the LSP manager). It runs on edited files after the
// convention review, catching type errors without waiting for a full build.
func (e *Engine) SetDiagnosticsChecker(fn func(path string) string) {
	e.diagFn = fn
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
	return &Engine{
		adapter:           adapter,
		tools:             tools,
		context:           ctxMgr,
		model:             model,
		mode:              "BUILDER",
		maxIterations:     25,
		baseMaxIterations: 25,
		hardCapIterations: 100,
		state:             StateThinking,
		usage:             NewUsageTracker(),
		reviewLLMEnabled:  true,
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
// benchmark harness to bound each case.
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

func (e *Engine) RunTurn(ctx context.Context, userQuery string, onUpdate TurnOutputHandler) (answer string, err error) {
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
	// Reset the per-turn cost budget counter.
	e.costUSD = 0
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
	if !e.autoExtendSession {
		if e.baseMaxIterations > 0 {
			e.maxIterations = e.baseMaxIterations
		} else {
			e.maxIterations = 25
		}
	}
	defer func() { e.progressHandler = nil }()
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

	if userQuery != "" {
		if err := e.context.AppendUserMessage(userQuery); err != nil {
			return "", err
		}
		// TSR REPRODUCE gate: arm only when the task looks like a bug fix AND
		// there is a verification command to reproduce it with. If the user
		// already pasted the error/stack trace, treat the repro as provided.
		e.reproGateArmed = looksLikeBugFixTask(userQuery)
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
			return e.finalSynth(ctx, fmt.Sprintf("COST BUDGET EXCEEDED ($%.4f): The per-task cost budget has been exhausted.", e.costUSD), "Batas Biaya Tercapai")
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
		if e.toolOnlyRounds >= toolWarnRounds && !e.toolReminderSent {
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
		if e.toolOnlyRounds >= toolFinalWarnRounds && !e.toolReminder2Sent {
			e.toolReminder2Sent = true
			_ = e.context.AppendUserMessage("⚠️ FINAL WARNING: This is your LAST chance. You may make AT MOST ONE more tool call — only a specific read you already know you need (a file or line range) — and then you MUST write your answer in the next response. No further exploration, no re-reading. If you cannot fully answer, give a partial answer with what you know and state clearly what context is still missing — never fabricate." + e.exploredSummary())
			if onUpdate != nil {
				onUpdate(e.state, "⚠️ Final warning — one final read max, then answer or stop")
			}
			continue
		}
		// 1. Thinking State
		e.state = StateThinking
		if onUpdate != nil {
			onUpdate(e.state, fmt.Sprintf("Turn %d reasoning...", iteration))
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

		adaptiveCap := CalculateAdaptiveToolBudget(e.context.LastUserPrompt(), e.Mode())
		if e.toolOnlyRounds >= adaptiveCap && (e.exploredStalls >= 4 || e.toolOnlyRounds >= maxToolOnlyAbsolute) {
			if onUpdate != nil {
				onUpdate(e.state, "⚠️ Tool exploration limit reached — synthesizing graceful answer from explored context...")
			}
			// Graceful Recovery: Instead of a cold abort message, make ONE final
			// completion request WITHOUT tools, forcing the model to synthesize a
			// helpful answer for the user based on the context explored so far.
			synthPrompt := "⚠️ TOOL EXPLORATION BUDGET REACHED: You have reached the tool call limit for this task. Tools are now DISABLED for this final response. You MUST now synthesize a helpful, comprehensive response for the user in the user's language based ONLY on the files and context you have explored so far. Answer as much of the user's prompt as possible, summarize your findings, and clearly note any missing context or next steps." + e.exploredSummary()
			_ = e.context.AppendUserMessage(synthPrompt)

			reqMessages := append([]provider.Message{
				{Role: "system", Content: sysPrompt},
			}, e.context.Messages()...)

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
				res := synthResp.Content + "\n\n---\n*⚠️ Respon ini dirangkum dari hasil eksplorasi parsial (Batas Tool Limit Tercapai).* "
				_ = e.context.AppendAssistantTurn(e.Mode(), e.model, "", res, nil)
				e.state = StateDone
				if onUpdate != nil {
					onUpdate(e.state, "Completed with graceful context synthesis")
				}
				return res, nil
			}

			// Deterministic fallback when the synthesis completion itself fails
			// or returns empty: NEVER dump a raw error or a cold "paused"
			// status line — construct a helpful answer from what the agent
			// already explored, mirroring the success path so the conversation
			// stays connected instead of ending on an abrupt notice.
			e.state = StateDone
			msg := "⚠️ Tool budget limit reached — here is what was verified from the explored context:\n" + e.exploredSummary()
			if e.lastReasoning != "" {
				msg += "\n\n**Last Working Focus**: " + e.lastReasoning
			}
			msg += "\n\n---\n*⚠️ Respon ini dirangkum dari hasil eksplorasi parsial (Batas Tool Limit Tercapai).* "
			_ = e.context.AppendAssistantTurn(e.Mode(), e.model, "", msg, nil)
			if onUpdate != nil {
				onUpdate(e.state, "Completed with fallback context synthesis")
			}
			return msg, nil
		}

		// Auto-compact context if token count exceeds threshold
		if e.context.NeedsCompaction() {
			summary := bcontext.CompactionSummary{
				Goal:           "Continue active conversation",
				FilesTouched:   []string{"codebase"},
				DecisionsMade:  []string{"Compacted older context turns to preserve memory window"},
				OpenQuestions:  []string{"Proceed with user request"},
				LastKnownState: "Context compacted successfully",
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
		}, e.context.Messages()...)

		req := provider.CompletionRequest{
			Model:       e.model,
			Messages:    reqMessages,
			Tools:       e.toolsForMode(currentMode),
			Temperature: 0.2,
		}

		if onUpdate != nil {
			onUpdate(e.state, "Thinking & analyzing request...")
		}

		resp, err := e.complete(ctx, req)
		if err != nil {
			// Automatic model routing: try each fallback provider in order.
			// Remember WHY the primary failed so the UI can surface it.
			primaryErr := err
			for _, fb := range e.fallbacks {
				fbReq := req
				fbReq.Model = fb.Model
				resp, err = e.completeWith(ctx, fb.Adapter, fbReq)
				if err == nil {
					e.lastFallback = fb.Model
					e.lastFallbackReason = primaryErr.Error()
					if onUpdate != nil {
						onUpdate(e.state, fmt.Sprintf("⚠️ Primary provider failed — using fallback model %s", fb.Model))
					}
					break
				}
			}
			if resp == nil {
				e.state = StateFailed
				return "", fmt.Errorf("LLM completion failed: %w", err)
			}
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
					onUpdate(e.state, fmt.Sprintf("%s", toolInfo))
				}

				// Track what the model has actually explored so budget reminders
				// can tell it what it already knows ("you've read X, Y, Z —
				// answer now") instead of a generic stop message.
				e.recordExplored(tc)

				// Same-call repetition detection: identical consecutive calls
				// (same tool + same arguments) indicate the model is stuck.
				// The repeat counter lives on the engine (not per iteration) so
				// re-issuing the same call across loop iterations is caught.
				if isRepeatToolCall(tc, e.lastToolCall) {
					e.lastToolCallRepeats++
					if e.lastToolCallRepeats >= 4 {
						// The model ignored the guard warning and is still
						// spinning — abort the whole turn instead of burning the
						// remaining iterations on a loop that will never finish.
						e.state = StateBlocked
						msg := fmt.Sprintf("Turn aborted: the model kept repeating tool call '%s' with identical arguments after being told to stop. Please rephrase your request or ask for a more specific task.", tc.Name)
						if onUpdate != nil {
							onUpdate(e.state, msg)
						}
						return msg, nil
					}
					if e.lastToolCallRepeats >= 2 {
						guardMsg := fmt.Sprintf("⚠️ [LOOP GUARD]: You are repeating tool call '%s' with identical arguments — stop and answer directly using the information you already gathered. Do NOT call the same tool again.", tc.Name)
						if onUpdate != nil {
							onUpdate(e.state, "⚠️ Loop detected — instructing model to answer directly")
						}
						pending[i] = pendingTool{tc: tc, output: guardMsg}
						continue
					}
				} else {
					e.lastToolCallRepeats = 0
				}
				e.lastToolCall = tc

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
				execCount++
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
						out, err := e.tools.Execute(ctx, pending[idx].tc.Name, pending[idx].tc.Arguments)
						if err != nil {
							out = fmt.Sprintf("Tool error: %v", err)
						}
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
				for i := range pending {
					if !pending[i].exec || isParallelReadOnly(pending[i].tc.Name) {
						continue
					}
					out, err := e.tools.Execute(ctx, pending[i].tc.Name, pending[i].tc.Arguments)
					if err != nil {
						out = fmt.Sprintf("Tool error: %v", err)
					}
					pending[i].output = out

					// Track files the model edited so the native convention checker
					// can review them (debug leftovers, markers, type safety,
					// duplicate symbols) before the turn is declared done.
					if pending[i].tc.Name == "write_file" || pending[i].tc.Name == "edit_file" {
						if p := extractToolPath(pending[i].tc.Arguments); p != "" {
							e.editedFiles = append(e.editedFiles, p)
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

			if vetErr := runVerification(ctx); vetErr != "" {
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
				_ = e.context.AppendUserMessage("Level 1 verification check failed:\n" + vetErr + "\nPlease fix the issues.")
				continue
			}
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
		if e.Mode() == "MINER" && e.mem != nil {
			var explored []string
			for _, ex := range e.explored {
				if !strings.Contains(ex, " ") { // skip bash command strings
					explored = append(explored, ex)
				}
			}
			_ = e.mem.CaptureMinerFindings(resp.Content, explored)
		}

		// Lesson auto-extract: a repair that started failing and ended passing
		// is the highest-value failure signal a harness can capture — distill a
		// one-line durable lesson into project memory (## Gotchas) so future
		// sessions start knowing this failure mode instead of re-discovering it.
		if e.repairSucceeded && e.mem != nil && e.lastVerifyErr != "" {
			if lesson := e.distillLesson(ctx); lesson != "" {
				_, _ = e.mem.Retain("Gotchas", lesson)
			}
		}
		e.explored = nil

		if onUpdate != nil {
			onUpdate(e.state, "Completed")
		}
		return resp.Content, nil
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

// buildSystemPrompt renders the full system prompt for the current mode. It is
// called ONCE per turn and the result is cached on the engine so every loop
// iteration sends byte-identical leading tokens (provider prompt caching).
// The warm-start memory excerpt is derived from the user's initial query.
func (e *Engine) buildSystemPrompt(currentMode string, iteration int, onUpdate TurnOutputHandler) string {
	// Mode descriptions are language-agnostic on purpose: the model is told to
	// answer in whatever language the user writes in, and must not be biased
	// by hardcoded phrases or foreign product names.
	modeDesc := "BUILDER (autonomous coding agent: can read, edit, and run terminal commands)"
	switch currentMode {
	case "PLANNER":
		modeDesc = "PLANNER (architecture & strategy agent: read-only — analyze and plan without editing files)"
	case "MINER":
		modeDesc = "MINER (project knowledge agent: read-only exploration that persists verified facts into project memory — learn the codebase, then remember it)"
	}

	sysPrompt := fmt.Sprintf(`You are BroCode CLI, an autonomous AI coding assistant.
%s`, e.projectContextBlock())
	if e.repoMap != "" {
		sysPrompt += "\n\n" + e.repoMap
	}
	if e.skillsCtx != "" {
		sysPrompt += "\n\nAVAILABLE SKILLS:\n" + e.skillsCtx + "\nWhen a task matches a skill, load its SKILL.md file (read_file) and follow its instructions."
	}
	if e.mem != nil {
		if ws := e.mem.WarmStartRelevant(e.context.LastUserPrompt()); ws != "" {
			sysPrompt += "\n\nPROJECT MEMORY (learned in past sessions, use as verified prior knowledge — confirm details against the code when they matter):\n" + ws
			if onUpdate != nil && iteration == 1 {
				onUpdate(e.state, "🧠 Warm Start: Recalled project memory & hot files")
			}
		}
	}
	sysPrompt += fmt.Sprintf(`
🔥 ACTIVE ENGINE MODE: %s (%s).
CRITICAL MODE OVERRIDE: The user has explicitly set the active engine mode to %s. If any previous assistant messages in the conversation history claim to be in a different mode (such as PLANNER or MINER), IGNORE THOSE PAST STATEMENTS ENTIRELY. You are NOW operating strictly in %s mode.
If the user asks about your mode (in any language), answer directly with the mode name (%s) and what it does, in the same language the user writes in, and mention the mode can be toggled with Shift+Tab.

Engine Mode Rules (%s):
`, currentMode, modeDesc, currentMode, currentMode, currentMode, currentMode)

	if currentMode == "PLANNER" {
		sysPrompt += `1. Focus on inspecting codebase, analyzing files, and proposing high-level step-by-step implementation plans.
2. DO NOT modify any source files or execute write_file/edit_file tools.
3. Use read_file, list_dir, grep, and glob to research before writing your plan.`
	} else if currentMode == "MINER" {
		sysPrompt += `1. MISSION: learn the project deeply and persist VERIFIED knowledge into PROJECT MEMORY using the memory tool (retain). This is how BroCode gets smarter the more it is used.
2. Read-only: DO NOT modify source files (write_file/edit_file are blocked). You may run read-only bash (git log, git status, ls) to understand history.
3. VERIFY BEFORE RETAINING: only store facts you confirmed in the code — architecture (service -> repo -> DB), build/test commands that actually exist, conventions (naming, error handling, package manager), decisions, gotchas. Never store guesses; if unsure, read more or skip.
4. Organize with good sections: Architecture, Build & Test, Conventions, Decisions, Gotchas. Keep each fact short, concrete, and actionable.
5. Reuse what already exists: check existing memory first (memory tool) so you do not duplicate or contradict earlier facts.`
	} else {
		sysPrompt += `1. PLAN & CONTINUE: reason through your plan BEFORE acting, then keep the tool loop running until the goal is achieved — do not stop to ask unless technical ambiguity cannot be resolved by tools. Use native function calling.
2. EXPLORE BEFORE ANSWERING: form a hypothesis about where the answer lives, then verify it with ONE batched round of targeted reads (glob/grep/read_file/code_locate/search_code; git tool for repo state; fetch_url/web_search for docs). Never answer from memory — read the real code and verify your claims. If a result is unhelpful, adapt; do NOT re-run the same narrow search.
3. BATCH & STAY LEAN (cost): every round re-sends the ENTIRE conversation, so the number of rounds is the single biggest cost driver. Issue 3-4 independent read/grep/glob calls in ONE message. A read_file over 250 lines truncates — cover the rest with 1-2 range reads (start_line/end_line) then answer; NEVER fight truncation with bash sed/head/tail/grep loops on the same file. Range reads of a large file ARE progress.
4. ASK ONLY WHEN TOOLS CANNOT DECIDE: for preferences, architecture choices, or destructive operations, call ask_user with 1-3 clear multiple-choice questions. If a risky command is denied or blocked, do NOT retry it — adapt with a safe alternative.
5. REUSE FIRST: before writing new code, use code_locate and search_code to check whether the symbol/function already exists — reimplementing existing code wastes tokens and creates duplicates. Report what you reused.
6. TYPE SAFETY & PERFORMANCE: treat type errors as blockers — fix them after the auto-verification (build/typecheck) flags them. Avoid N+1 queries, SELECT *, missing WHERE on updates/deletes, string-built SQL (injection), quadratic loops, and unbounded fetches.
7. PROPORTIONALITY (match effort to risk): a small edit (≤30 LOC, one file, no logic change) deserves the minimal correct fix — no ceremony, no new abstractions. Extract a helper only at 3+ uses; keep a file under ~300 LOC; inline one-off logic. Over-engineering is a review finding.
8. SENIOR REVIEW: after edits, deterministic checks + an LLM review of your changed files run automatically. When something is flagged, FIX IT — do not ignore or argue; a clean review is part of "done". LSP tools are selective: prefer the project's own verification CLI (go build/vet/test, tsc --noEmit, cargo check) as the source of truth, and run lsp_scan at most once per task.
9. ANSWER PROPORTIONATELY & IN THE USER'S LANGUAGE: match answer length to the question's depth — full structured detail for exploration/architecture questions (with evidence from the code), terse for simple ones. Synthesize your findings; never dump raw exploration or file lists.
10. TSR CONTRACT (bug fixes): for a reported bug/failure, REPRODUCE first — run the relevant test or command with run_tests or bash and OBSERVE it FAIL before editing any code. That confirms the bug and gives a verification baseline. If you cannot reproduce it, say so and do NOT edit blind. After fixing, rely on the automatic verification; if the same error persists across attempts, change your approach instead of repeating the same fix.`
	}
	return sysPrompt
}

// toolsForMode returns the tool surface exposed to the model for the current
// mode. Structural pruning: read-only modes (PLANNER, MINER) simply DO NOT
// receive the mutating tools — write_file/edit_file/delete_file are never
// offered, so the model cannot propose them, cannot waste rounds on guard
// messages, and pays fewer schema tokens per request. PLANNER additionally
// drops bash entirely. BUILDER gets the full surface. The runtime mode guards
// stay as a backstop (MCP/subagent tools bypass this filter), but the LLM is
// never tempted by tools its mode forbids.
func (e *Engine) toolsForMode(mode string) []provider.ToolDefinition {
	defs := e.tools.Definitions()
	if mode == "BUILDER" {
		return defs
	}
	exclude := map[string]bool{
		"write_file":  true,
		"edit_file":   true,
		"delete_file": true,
	}
	if mode == "PLANNER" {
		exclude["bash"] = true
	}
	out := make([]provider.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		if exclude[d.Name] {
			continue
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
	for _, ex := range e.explored {
		if ex == target {
			return
		}
	}
	e.explored = append(e.explored, target)
	if len(e.explored) > 12 {
		e.explored = e.explored[len(e.explored)-12:]
	}
}

// projectContextBlock renders the injected project overview (tree + docs) as
// a system-prompt section, or an empty string when none was provided.
func (e *Engine) projectContextBlock() string {
	if strings.TrimSpace(e.projectCtx) == "" {
		return ""
	}
	return "You are working in this project:\n\n" + e.projectCtx
}

// exploredSummary renders the list of files/directories the model has already
// read or searched, used by the tool-budget reminders and the abort message.
func (e *Engine) exploredSummary() string {
	if len(e.explored) == 0 {
		return ""
	}
	return "\n\nFiles you have already examined: " + strings.Join(e.explored, ", ")
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
		localSysPrompt = fmt.Sprintf("You are BroCode CLI, an autonomous AI coding assistant.\n%s", e.projectContextBlock())
	}
	reqMessages := append([]provider.Message{
		{Role: "system", Content: localSysPrompt},
	}, e.context.Messages()...)

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
func (e *Engine) complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return e.completeWith(ctx, e.adapter, req)
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
	if pa, ok := a.(provider.ProgressingAdapter); ok && progress != nil {
		resp, err = pa.CompleteWithProgress(ctx, req, func(line string) {
			if progress != nil {
				progress(st, line)
			}
		})
	} else if sa, ok := a.(provider.StreamingAdapter); ok && stream != nil {
		resp, err = sa.StreamComplete(ctx, req, stream)
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
	// token usage; multiply by the model list price.
	if resp != nil && (resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0) {
		e.costUSD += provider.EstimateCostUSD(req.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	return resp, nil
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
		if path, ok := m["path"].(string); ok && path != "" {
			return fmt.Sprintf("%s (%s)", name, path)
		}
		if pattern, ok := m["pattern"].(string); ok && pattern != "" {
			return fmt.Sprintf("%s (pattern: '%s')", name, pattern)
		}
		if cmd, ok := m["command"].(string); ok && cmd != "" {
			return fmt.Sprintf("%s (%s)", name, cmd)
		}
		if target, ok := m["target"].(string); ok && target != "" {
			return fmt.Sprintf("%s (%s)", name, target)
		}
	}
	return name
}
