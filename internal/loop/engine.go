package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	adapter         provider.ProviderAdapter
	tools           *tool.Registry
	context         *bcontext.Manager
	model           string
	mode            string // "BUILDER" or "PLANNER"
	maxIterations   int
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
	// discovering new files (freedom is bounded, never infinite).
	maxToolOnlyAbsolute = 18
)

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

// NewEngine creates an agent loop engine instance.
func NewEngine(adapter provider.ProviderAdapter, tools *tool.Registry, ctxMgr *bcontext.Manager, model string) *Engine {
	return &Engine{
		adapter:          adapter,
		tools:            tools,
		context:          ctxMgr,
		model:            model,
		mode:             "BUILDER",
		maxIterations:    25,
		state:            StateThinking,
		usage:            NewUsageTracker(),
		reviewLLMEnabled: true,
	}
}

func (e *Engine) SetMode(m string) {
	e.mode = m
}

// SetMaxIterations overrides the loop iteration cap (default 25). Used by the
// benchmark harness to bound each case.
func (e *Engine) SetMaxIterations(n int) {
	if n > 0 {
		e.maxIterations = n
	}
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
			e.state = StateFailed
			if onUpdate != nil {
				onUpdate(e.state, "Max loop iterations reached")
			}
			return "", fmt.Errorf("reached max iterations (%d)", e.maxIterations)
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
			_ = e.context.AppendUserMessage(fmt.Sprintf("⚠️ You have called tools %d times in a row without answering, and already examined %d files. If you have enough context, answer the user's question NOW using what you have read — do not explore further. If you genuinely do NOT have enough context, stop anyway and say exactly what is missing instead of guessing."+e.exploredSummary(), e.toolOnlyRounds, len(e.explored)))
			if onUpdate != nil {
				onUpdate(e.state, "⚠️ Tool budget nearly exhausted — answer with what you have")
			}
			continue
		}
		if e.toolOnlyRounds >= toolFinalWarnRounds && !e.toolReminder2Sent {
			e.toolReminder2Sent = true
			_ = e.context.AppendUserMessage("⚠️ FINAL WARNING: This is your LAST chance. Do NOT call any more tools. Write your answer now based on what you have read. If you cannot fully answer, give a partial answer with what you know and state clearly what context is still missing — never fabricate." + e.exploredSummary())
			if onUpdate != nil {
				onUpdate(e.state, "⚠️ Final warning — answer now or the turn will be stopped")
			}
			continue
		}
		if e.toolOnlyRounds >= maxToolOnlyRounds && (e.exploredStalls >= 4 || e.toolOnlyRounds >= maxToolOnlyAbsolute) {
			e.state = StateBlocked
			// Hand the user something actionable, not just a file dump: what
			// the agent was last trying to figure out, plus what it examined.
			// The user can then rephrase, guide, or let it continue.
			msg := "Turn aborted: the model kept calling tools without producing an answer after two warnings. "
			if e.lastReasoning != "" {
				msg += "\n\nWhat the agent was last working on: " + e.lastReasoning
			}
			msg += e.exploredSummary()
			if onUpdate != nil {
				onUpdate(e.state, msg)
			}
			return msg, nil
		}

		// Scout results that completed during the previous loop iteration are
		// delivered before the model reasons again, so it can incorporate
		// background findings without re-invoking anything.
		e.drainScouts(onUpdate)

		// 1. Thinking State
		e.state = StateThinking
		if onUpdate != nil {
			onUpdate(e.state, fmt.Sprintf("Turn %d reasoning...", iteration))
		}

		currentMode := e.Mode()
		// Mode descriptions are language-agnostic on purpose: the model is
		// told to answer in whatever language the user writes in, and must not
		// be biased by hardcoded phrases or foreign product names.
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
			if ws := e.mem.WarmStart(); ws != "" {
				sysPrompt += "\n\nPROJECT MEMORY (learned in past sessions, use as verified prior knowledge — confirm details against the code when they matter):\n" + ws
			}
		}
		sysPrompt += fmt.Sprintf(`
Active engine mode: %s (%s).
If the user asks about your mode (in any language), answer directly with the mode name and what it does, in the same language the user writes in, and mention the mode can be toggled with Shift+Tab.

Engine Mode Rules (%s):
`, currentMode, modeDesc, currentMode)

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
			sysPrompt += `1. Always reason through your plan BEFORE executing any tool or returning an answer.
2. CONTINUATION RULE: After receiving tool execution results, DO NOT stop to ask the user unless technical ambiguity cannot be resolved by tools. Continue the tool loop until the goal is achieved.
3. Use native function calling for tool execution.
4. DEEP EXPLORATION: Before answering questions about code, thoroughly explore the codebase first. Use glob to find relevant files, grep to search for definitions and usages, read_file to read the actual code, and bash (git status/log) to understand repo state. Never answer from memory or a single grep hit — read the relevant files and verify your claims against the real code before answering.
5. When you need a decision, preference, or confirmation that tools cannot determine (e.g. choosing a database or architecture, destructive operations, or unclear requirements), call the ask_user tool with 1-3 clear multiple-choice questions instead of guessing.
6. Some risky shell commands (rm, sudo, git push --force, etc.) require the user's approval. If a command is denied or blocked, do NOT retry it — adapt and use a safe alternative.
7. Use the git tool to inspect repo state (status/diff/log/branch), fetch_url to read a specific page, web_search to find docs/errors on the web, review_changes to let the user approve or roll back your edits, and undo to revert a bad file edit made this turn.
8. Answer in the same language the user writes in.
9. REUSE FIRST: before writing new code, use code_locate and search_code to check whether the functionality or symbol already exists in the codebase — reimplementing existing code wastes tokens and creates duplicates. Report what you reused.
10. TYPE SAFETY: treat type errors as blockers. After editing, rely on the auto verification (project build/typecheck CLI + native review) and fix any type errors before declaring done; use lsp_diagnostics only on specific edited files when the auto checks are not conclusive.
11. PERFORMANCE & SQL AWARENESS: avoid N+1 query patterns (DB query inside a loop — batch load instead), SELECT * without need, missing WHERE on updates/deletes, string-built SQL (injection risk), quadratic loops, and unbounded fetches. Mention the Big-O of hot paths.
12. SENIOR REVIEW: after editing, a senior-level code review runs automatically (deterministic checks + LLM review of your changed files for N+1, SQL, error handling, concurrency, reuse, security). When the review flags an issue, FIX IT — do not ignore or argue; a clean review is part of "done".
13. PROPORTIONALITY (match effort to risk): a small edit (≤30 LOC, one file, no logic change) deserves the minimal correct fix — no ceremony. Named constants, validation helpers, error envelopes, and heavy abstractions apply to real product logic (auth, DB, cross-cutting), NOT to guard clauses or 10-line bug fixes. Over-engineering is a review finding.
14. DECISION MATRIX: reuse over rewrite — if the codebase already has a function/utility for it, use it. Extract a helper only at 3+ uses (DRY threshold). Keep a file under ~300 LOC; split only when it handles >3 concerns. Inline one-off logic instead of abstracting it. When two approaches are viable, prefer the simpler one unless there is measured evidence the other matters.
15. TOOL ECONOMY (LSP is selective, not default): lsp_* tools (lsp_definition, lsp_references, lsp_hover, lsp_diagnostics, lsp_scan) are token- and resource-heavy — language servers index the whole project and their responses are verbose. Use them only where structural accuracy matters: cross-file refactors, real type errors, deprecated API detection. For cheap lookups ("where is X used", "what does Y do") prefer grep, glob, search_code, read_file first. Run lsp_scan at most once per task. Prefer the project's own verification CLI (go build/vet/test, tsc --noEmit, bun test, cargo check) as the source of truth — LSP complements it, never replaces it.
16. ANSWER PROPORTIONALITY: match answer length to the question's depth. Exploration/architecture questions deserve thorough, detailed answers — structure, evidence from the code, examples. Do NOT compress a full explanation into a terse summary; the user wants the detail. Short answers are for simple questions only. Never pad a short answer into an essay either.
17. BATCH YOUR TOOL CALLS (cost-critical): every round re-sends the ENTIRE conversation to the model, so the number of rounds is the single biggest cost driver. Issue MULTIPLE independent tool calls in ONE message — e.g. 3-4 read_file/grep/glob/list_dir calls together instead of one per round. They execute in sequence within the round. A senior consultant explores with a few high-signal batch reads, not dozens of narrow single-file greps.
18. SENIOR CONSULTANT POSTURE: think before you act. For a question, first form a hypothesis about where the answer lives (likely files/symbols), then verify it with ONE batched round of targeted reads, then answer directly with what you verified. Do not dump raw exploration or file lists into your answer — synthesize. If a tool result is unhelpful, say so and adapt; never re-run the same narrow search hoping for a different outcome.
19. LARGE FILES & TRUNCATION: read_file of a file over 200 lines returns only the first 100 — read specific ranges with start_line/end_line, or use code_search to locate symbols first, then read the exact lines. If a tool result is truncated, narrow the range ONCE and move on; NEVER fight truncation with bash sed/head/tail/grep loops on the same file hoping for different output — that burns rounds and gets the turn aborted. Two range reads per file max, then answer from what you have.`
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
			Tools:       e.tools.Definitions(),
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
		if err := e.context.AppendAssistantTurn(reasoning, resp.Content, resp.ToolCalls); err != nil {
			return "", err
		}

		// 2. Check if Model wants to call tools (Acting & Observing State)
		hasCodeChanges := false
		if len(resp.ToolCalls) > 0 {
			e.state = StateActing
			// This round was tool-only (no answer text); count it toward the
			// budget so a model that never answers gets cut off.
			e.toolOnlyRounds++
			// Loop guard: if the model repeats the exact same tool call
			// (name + args) multiple times in a row it is spinning, not
			// progressing. Block the repeat and tell the model to answer from
			// the results it already has instead of re-running the tool.
			for _, tc := range resp.ToolCalls {
				if tc.Name == "write_file" || tc.Name == "edit_file" || tc.Name == "delete_file" {
					hasCodeChanges = true
				}

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
						_ = e.context.AppendToolResult(tc.ID, guardMsg)
						continue
					}
				case "MINER":
					if tc.Name == "write_file" || tc.Name == "edit_file" || tc.Name == "delete_file" {
						guardMsg := fmt.Sprintf("⚠️ [MINER GUARD]: Tool '%s' is blocked in MINER mode (read-only knowledge agent). Switch to BUILDER mode (Shift+Tab) to modify code.", tc.Name)
						if onUpdate != nil {
							onUpdate(e.state, guardMsg)
						}
						_ = e.context.AppendToolResult(tc.ID, guardMsg)
						continue
					}
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
						_ = e.context.AppendToolResult(tc.ID, guardMsg)
						continue
					}
				} else {
					e.lastToolCallRepeats = 0
				}
				e.lastToolCall = tc

				// Permission gate: risky bash commands ask the user for approval
				// (Allow once / Always allow / Deny) via the interactive modal.
				approved, reason, gerr := e.tools.GateAction(ctx, tc)
				if gerr != nil {
					_ = e.context.AppendToolResult(tc.ID, fmt.Sprintf("Tool error: %v", gerr))
					continue
				}
				if !approved {
					guardMsg := fmt.Sprintf("⛔ [PERMISSION DENIED]: %s", reason)
					if onUpdate != nil {
						onUpdate(e.state, guardMsg)
					}
					_ = e.context.AppendToolResult(tc.ID, guardMsg)
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
					e.state = StateObserving
					if err := e.context.AppendToolResult(tc.ID, hookOverride); err != nil {
						return "", err
					}
					continue
				}

				toolOutput, err := e.tools.Execute(ctx, tc.Name, tc.Arguments)
				if err != nil {
					toolOutput = fmt.Sprintf("Tool error: %v", err)
				}

				// Track files the model edited so the native convention checker
				// can review them (debug leftovers, markers, type safety,
				// duplicate symbols) before the turn is declared done.
				if tc.Name == "write_file" || tc.Name == "edit_file" {
					if p := extractToolPath(tc.Arguments); p != "" {
						e.editedFiles = append(e.editedFiles, p)
					}
				}

				// Lifecycle hook: after tool execution, with the tool's output.
				e.hookRun(ctx, hooks.EventToolResult, map[string]string{
					"tool":   tc.Name,
					"output": toolOutput,
				})

				e.state = StateObserving
				if err := e.context.AppendToolResult(tc.ID, toolOutput); err != nil {
					return "", err
				}
			}

			// Continuation rule: loop back to StateThinking automatically!
			continue
		}

		// 3. Verifying State (§2.4 Verification Ladder Level 1 & 2). Language-
		// agnostic: the project type (Go / JS-TS / Python / Rust / Java) is
		// detected from its config files and the matching checks run.
		if hasCodeChanges {
			e.state = StateVerifying
			if onUpdate != nil {
				msg := "Running verification..."
				if desc := describeVerification(); desc != "" {
					msg = "Running verification: " + desc
				}
				onUpdate(e.state, msg)
			}

			if vetErr := runVerification(ctx); vetErr != "" {
				_ = e.context.AppendUserMessage("Level 1 verification check failed:\n" + vetErr + "\nPlease fix the issues.")
				continue
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
		e.explored = nil

		if onUpdate != nil {
			onUpdate(e.state, "Completed")
		}
		return resp.Content, nil
	}
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
		case "list_dir", "grep", "glob":
			target, _ = m["path"].(string)
			if target == "" {
				target, _ = m["pattern"].(string)
			}
		case "bash":
			cmd, _ := m["command"].(string)
			if strings.HasPrefix(strings.TrimSpace(cmd), "find ") {
				target = cmd
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
