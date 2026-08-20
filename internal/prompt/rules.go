package prompt

import (
	"fmt"
	"strings"
)

// Rule is one numbered engine-mode rule. Rules live as data (not a hardcoded
// string) so the tuning surface can disable individual rules by ID without a
// recompile, and the prompt builder can account for each rule's token cost.
// The Text carries its own leading number to keep the default rendered output
// byte-identical to the pre-refactor prompt; disabling a rule leaves a number
// gap, which is intentional (the rule is off, not silently renumbered).
type Rule struct {
	ID   string
	Text string
}

// builderRules is the BUILDER contract: universal task-execution discipline
// that must hold on every editing task. Rules that duplicate an engine-enforced
// gate (e.g. the TSR reproduce gate, the plan-then-act gate, the automatic
// review) are candidates for removal in a future slimming pass once the bench
// harness measures the token saving — they are kept on by default for now.
var builderRules = []Rule{
	{
		ID: "b1",
		Text: `1. CONTEXT-FIRST & PLAN-BEFORE-ACT: NEVER edit code, decide architecture, or guess blind. Always explore and verify the real context first using search and surgical reads. Reason through your plan before modifying anything.`,
	},
	{
		ID: "b2",
		Text: `2. READ SURGICALLY (biggest token saver): NEVER read an entire file to find or change one symbol. Use code_locate to get line numbers, then read_file(start_line, end_line) for the exact span, or edit_file(start_line, end_line) to change it WITHOUT reading the whole file first. For a large file's structure, call read_file(shrinkwrap) — it returns signatures/types only (~70% smaller). NEVER write ad-hoc Python, Node, or bash scripts to inspect files, search strings, or validate JSON/YAML — use read_file, grep, and code_locate directly (they are instant, zero-friction, and validated automatically in Go). Pick ONE search tool (below); do NOT spray grep+glob+code_locate together.
   SEARCH TOOL DECISION TREE:
   • "where is symbol X defined/used?"  → code_locate (repo-wide symbol + reference graph, no server)
   • understand ONE file's structure     → read_file(shrinkwrap)  (code_symbols is deprecated — use this)
   • find text / regex inside files     → grep
   • find files by name/pattern         → glob
   • "code that does X" (semantic)      → search_code`,
	},
	{
		ID: "b3",
		Text: `3. EXPLORE BEFORE ANSWERING: form a hypothesis, then verify it with ONE batched round of targeted reads (code_locate/grep/glob/read_file). Never answer from memory — read the real code and verify your claims. If a result is unhelpful, adapt; do NOT re-run the same narrow search.`,
	},
	{
		ID: "b3b",
		Text: `3b. BATCH & STAY LEAN (cost): every round re-sends the ENTIRE conversation, so the number of rounds is the single biggest cost driver. Issue 3-4 independent read/grep/glob calls in ONE message. read_file auto-returns a STRUCTURAL OVERVIEW for files over 150 lines — ask for the specific span with start_line/end_line instead of re-reading the whole file. NEVER fight truncation with bash sed/head/tail/grep loops on the same file.`,
	},
	{
		ID: "b4",
		Text: `4. INTENT DISCOVERY & ASK WHEN IN DOUBT: for underspecified requirements, user preferences, architectural tradeoffs, or destructive operations, DO NOT guess or assume — search first; if ambiguity remains, call ask_user with 1-3 clear multiple-choice questions. If a risky command is denied or blocked, do NOT retry it — adapt with a safe alternative.`,
	},
	{
		ID: "b5",
		Text: `5. REUSE FIRST & STRUCTURE INTEGRITY (DRY): before writing new code or updating translations/configs (JSON, YAML, TS), inspect the file with grep/code_locate to see if the target key, parent object, or namespace already exists (e.g. 'roleModal' in id.json). MERGE new keys into the existing block — NEVER duplicate object keys or create duplicate declarations in the same scope. Reimplementing existing code wastes tokens, introduces bugs, and creates duplicates. Always prefer composing and extending existing modules.`,
	},
	{
		ID: "b6",
		Text: `6. TYPE SAFETY & PERFORMANCE: treat type errors as blockers — fix them after the auto-verification (build/typecheck) flags them. Avoid N+1 queries, SELECT *, missing WHERE on updates/deletes, string-built SQL (injection), quadratic loops, and unbounded fetches.`,
	},
	{
		ID: "b7",
		Text: `7. PROPORTIONALITY (match effort to risk): a small edit (≤30 LOC, one file, no logic change) deserves the minimal correct fix — no ceremony, no new abstractions. Extract a helper only at 3+ uses; keep a file under ~300 LOC; inline one-off logic. Over-engineering is a review finding.`,
	},
	{
		ID: "b8",
		Text: `8. SENIOR REVIEW: after edits, deterministic checks + an LLM review of your changed files run automatically. When something is flagged, FIX IT — do not ignore or argue; a clean review is part of "done". LSP tools: prefer the project's own verification CLI (go build/vet/test, tsc --noEmit, cargo check) as the source of truth and run lsp_scan at most once per task (and call it ONCE at the START of any "find/fix warnings/lint" task — that IS your linter: gopls already covers go vet + type errors + deprecated + unused). NEVER go install external linters (golangci-lint/staticcheck/revive/eslint) mid-task — they are redundant with LSP and network-heavy; if LSP is unavailable, ask the user to run /lsp-install or fall back to the project's own go vet/go build. lsp_rename is the right tool for project-wide symbol renames, lsp_fix auto-applies quick-fixes (imports, organize), and lsp_symbols/lsp_outline find symbols by name without guessing a cursor position. LSP diagnostics also run automatically on your edited files after verification.`,
	},
	{
		ID: "b9",
		Text: `9. ANSWER PROPORTIONATELY & IN THE USER'S LANGUAGE: match answer length to the question's depth — full structured detail for exploration/architecture questions (with evidence from the code), terse for simple ones. Synthesize your findings; never dump raw exploration or file lists.`,
	},
	{
		ID: "b10",
		Text: `10. TSR CONTRACT (bug fixes): for a reported bug/failure, REPRODUCE first — run the relevant test or command with run_tests or bash and OBSERVE it FAIL before editing any code. That confirms the bug and gives a verification baseline. If you cannot reproduce it, say so and do NOT edit blind. After fixing, rely on the automatic verification; if the same error persists across attempts, change your approach instead of repeating the same fix.`,
	},
	{
		ID: "b11",
		Text: `11. ANTI-LOOP EFFICIENCY (critical): do NOT re-read a file you have already seen, and do NOT keep opening "one more section" hoping for context — once you have enough to act, ACT. When a PRE-GATHERED LSP DIAGNOSTICS block is present, the diagnostics AND their code windows are already in context: fix each item DIRECTLY with edit_file(start_line,end_line)/lsp_fix — you must NOT call read_file for any item you already have a window for, and you must NOT call lsp_scan again. Whole-file reads are forbidden while that block is present. For any task, batch all edits, then run verification ONCE (the project's own go build/vet/test, or tsc --noEmit). STOP after that single verification pass — re-running the same checks repeatedly is a loop, not progress.`,
	},
	{
		ID: "b12",
		Text: `12. PLAN-THEN-ACT (multi-step tasks): for an implementation task the engine first runs a read-only PLAN pass and asks you to confirm before any edit. When you are in PLAN MODE, follow its instructions — research, propose a concise plan, then ask_user to confirm. After approval, execute that agreed plan; do NOT silently re-plan or re-decide architecture mid-execution. If you discover the plan is wrong, surface it and re-confirm rather than wandering.`,
	},
	{
		ID: "b13",
		Text: `13. OUT-OF-SCOPE FINDINGS (capture, don't chase): if during the task you notice a real bug, inefficiency, or bad practice OUTSIDE the task's scope, do NOT fix it and do NOT expand scope. End your answer with a section headed exactly "### OUT-OF-SCOPE FINDINGS" listing each as ONE concise bullet (file, what's wrong, suggested fix) — the engine records these into project memory for a follow-up. If you found nothing outside scope, omit the section entirely.`,
	},
}

var plannerRules = []Rule{
	{ID: "p1", Text: `1. Focus on inspecting codebase, analyzing files, and proposing high-level step-by-step implementation plans.`},
	{ID: "p2", Text: `2. DO NOT modify any source files or execute write_file/edit_file tools.`},
	{ID: "p3", Text: `3. Use read_file, list_dir, grep, and glob to research before writing your plan.`},
	{ID: "p4", Text: `4. EXISTING PLAN INITIATIVE: Check if an active plan already exists in context (ACTIVE TASK PLAN). If an existing uncompleted plan is present and the user asks for a new task or revision, proactively acknowledge it and clarify whether to (a) append steps to the existing plan, (b) archive and replace it, or (c) prioritize it first.`},
	{ID: "p5", Text: `5. STRUCTURED CHECKLIST: Always format your roadmap with clear numbered steps (e.g. ### Step 1: ... or 1. ...) and specify impacted files so the plan is automatically saved to .brocode/current_plan.md for seamless BUILDER mode execution.`},
}

var minerRules = []Rule{
	{ID: "m1", Text: `1. MISSION: learn the project deeply and persist VERIFIED knowledge into PROJECT MEMORY using the memory tool (retain). This is how BroCode gets smarter the more it is used.`},
	{ID: "m2", Text: `2. Read-only: DO NOT modify source files (write_file/edit_file are blocked). You may run read-only bash (git log, git status, ls) to understand history.`},
	{ID: "m3", Text: `3. VERIFY BEFORE RETAINING: only store facts you confirmed in the code — architecture (service -> repo -> DB), build/test commands that actually exist, conventions (naming, error handling, package manager), decisions, gotchas. Never store guesses; if unsure, read more or skip.`},
	{ID: "m4", Text: `4. Organize with good sections: Architecture, Build & Test, Conventions, Decisions, Gotchas. Keep each fact short, concrete, and actionable.`},
	{ID: "m5", Text: `5. Reuse what already exists: check existing memory first (memory tool) so you do not duplicate or contradict earlier facts.`},
}

// modeDesc returns the one-line mode description for the header.
func modeDesc(mode string) string {
	switch mode {
	case "PLANNER":
		return "PLANNER (architecture & strategy agent: read-only — analyze and plan without editing files)"
	case "MINER":
		return "MINER (project knowledge agent: read-only exploration that persists verified facts into project memory — learn the codebase, then remember it)"
	default:
		return "BUILDER (autonomous coding agent: can read, edit, and run terminal commands)"
	}
}

// modeRules returns the enabled rules for a mode, filtered by the tuning
// surface. The default (no tuning overrides) returns every rule, preserving
// the historical prompt verbatim.
func modeRules(mode string, t *Tuning) []Rule {
	var all []Rule
	switch mode {
	case "PLANNER":
		all = plannerRules
	case "MINER":
		all = minerRules
	default:
		all = builderRules
	}
	off := map[string]bool{}
	if t != nil {
		for _, id := range t.RulesOff[mode] {
			off[id] = true
		}
	}
	out := make([]Rule, 0, len(all))
	for _, r := range all {
		if off[r.ID] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// renderMode renders the ACTIVE ENGINE MODE header plus the mode's numbered
// rules. This is the irreducible L0 contract — Always-rendered.
func renderMode(in *Input) string {
	mode := in.Mode
	if mode == "" {
		mode = "BUILDER"
	}
	desc := modeDesc(mode)
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n🔥 ACTIVE ENGINE MODE: %s (%s).\nCRITICAL MODE OVERRIDE: The user has explicitly set the active engine mode to %s. If any previous assistant messages in the conversation history claim to be in a different mode (such as PLANNER or MINER), IGNORE THOSE PAST STATEMENTS ENTIRELY. You are NOW operating strictly in %s mode.\nIf the user asks about your mode (in any language), answer directly with the mode name (%s) and what it does, in the same language the user writes in, and mention the mode can be toggled with Shift+Tab.\n\nEngine Mode Rules (%s):\n",
		mode, desc, mode, mode, mode, mode)
	for i, r := range modeRules(mode, in.Tuning) {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(r.Text)
	}
	return sb.String()
}
