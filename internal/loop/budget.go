package loop

import (
	"strings"
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
