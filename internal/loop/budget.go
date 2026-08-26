package loop

import (
	"strings"
	"time"
)

// SetToolDescBudget caps how many characters of tool descriptions are passed
// in system prompts. 0 means unlimited.
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

// Read-only budget: separates exploration (reads) from execution (edits/writes)
// so a model that reads 12 files still has budget to edit. Without this,
// read_file and edit_file share one pool — aggressive exploration Starves
// execution and the agent reports findings without shipping code.
const (
	// readOnlyWarnRounds — first "stop reading, start editing" reminder.
	// After this many consecutive read-only rounds, inject a prompt that
	// forces the model to act on what it already knows.
	readOnlyWarnRounds = 6
	// readOnlyHardStop — hard cap on consecutive read-only rounds. The model
	// MUST switch to editing or answering after this many reads. This is
	// separate from the total tool budget so reads never consume the full pool.
	readOnlyHardStop = 10
	// minWriteBudget — minimum number of tool rounds reserved for mutations
	// (edit/write/bash). When the read-only counter hits this threshold, the
	// model is told to stop exploring and start implementing, regardless of
	// the total tool budget remaining.
	minWriteBudget = 6
)

// isReadOnlyTool reports whether a tool is pure exploration (no side effects).
// Used to track the separate read-only budget.
func isReadOnlyTool(name string) bool {
	switch name {
	case "read_file", "list_dir", "grep", "glob", "search_code", "fetch_url", "web_search", "doc_lookup":
		return true
	}
	return false
}

// CalculateAdaptiveToolBudget dynamically scales the tool budget based on
// prompt complexity, active mode, and task keywords.
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

// ComplexityTier is the exported alias for classifyTaskComplexity's result,
// so the UI layer can derive an adaptive timeout from the same signal.
type ComplexityTier = complexityTier

// Exported tier constants for use by callers outside this package.
const (
	TierSimple  = tierSimple
	TierMedium  = tierMedium
	TierComplex = tierComplex
)

// ClassifyTaskComplexity is the exported wrapper around classifyTaskComplexity.
func ClassifyTaskComplexity(query string) ComplexityTier {
	return classifyTaskComplexity(query)
}

// timeoutForComplexity maps a task tier to a wall-clock timeout for the turn
// watchdog. Simple tasks should finish fast; complex cross-module refactors
// need more room. The autonomous extension (+15 iterations) is separate and
// does NOT extend the time budget — time is the real safety net.
func timeoutForComplexity(t complexityTier) time.Duration {
	switch t {
	case tierSimple:
		return 5 * time.Minute
	case tierComplex:
		return 15 * time.Minute
	default:
		return 10 * time.Minute
	}
}

// TimeoutForComplexity is the exported wrapper so the UI can derive an
// adaptive wall-clock timeout from the engine's complexity tier.
func TimeoutForComplexity(t ComplexityTier) time.Duration {
	return timeoutForComplexity(t)
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
	case "read_file", "list_dir", "grep", "glob", "search_code", "fetch_url", "web_search", "doc_lookup":
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

// taskType classifies the user's prompt into a task category for adaptive
// tool filtering. Different task types benefit from different tool subsets:
// bug fixes don't need memory or web search; code exploration doesn't need
// edit tools. This shrinks the schema payload and prevents the model from
// being tempted by irrelevant tools — the #1 efficiency gain for short tasks.
type taskType int

const (
	taskGeneric taskType = iota
	taskBugFix          // error, crash, not working, regression
	taskExplore         // explain, what does, how does, architecture
	taskImplement       // add, create, implement, build
	taskRefactor        // refactor, clean up, simplify, optimize
	taskTest            // test, coverage, spec, assertion
)

func classifyTaskType(query string) taskType {
	p := strings.ToLower(strings.TrimSpace(query))

	// Bug fix: error signals, crash, not working
	for _, kw := range []string{"error", "crash", "panic", "failing", "broken", "not working", "regression", "bug", "fix the"} {
		if strings.Contains(p, kw) {
			return taskBugFix
		}
	}
	// Exploration: explain, what does, how does, architecture
	for _, kw := range []string{"explain", "what does", "how does", "architecture", "overview", "why does", "what is", "how is"} {
		if strings.Contains(p, kw) {
			return taskExplore
		}
	}
	// Implementation: add, create, implement, build
	for _, kw := range []string{"add", "create", "implement", "build", "new feature", "add support", "introduce"} {
		if strings.Contains(p, kw) {
			return taskImplement
		}
	}
	// Refactor: refactor, clean up, simplify, optimize
	for _, kw := range []string{"refactor", "clean up", "simplify", "optimize", "deduplicate", "restructure"} {
		if strings.Contains(p, kw) {
			return taskRefactor
		}
	}
	// Test: test, coverage, spec
	for _, kw := range []string{"test", "coverage", "spec", "assertion", "unit test", "integration test"} {
		if strings.Contains(p, kw) {
			return taskTest
		}
	}
	return taskGeneric
}

// taskExcludeTools maps task types to tools that should be EXCLUDED for that
// task type. This reduces schema noise and prevents the model from wasting
// tool calls on irrelevant actions. E.g., a bug-fix task doesn't need
// memory, web_search, or doc_lookup — only read, edit, bash, grep.
var taskExcludeTools = map[taskType]map[string]bool{
	taskBugFix: {
		"memory": true, "web_search": true, "doc_lookup": true,
		"code_outline": true, "blast_radius": true, "impact": true,
	},
	taskExplore: {
		"write_file": true, "edit_file": true, "delete_file": true,
		"memory": true, "lsp_fix": true, "lsp_rename": true,
	},
	taskTest: {
		"memory": true, "web_search": true, "doc_lookup": true,
		"code_outline": true, "blast_radius": true,
	},
}
