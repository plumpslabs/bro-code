// Package prompt assembles the system prompt from composable, layered blocks
// instead of one monolithic string. Each block renders one concern (identity,
// repo map, skills, LSP state, memory, pre-flight diagnostics, mode rules) and
// reports its token cost, so the engine can budget the prefix, disable blocks
// at runtime (the tuning surface), and keep the per-turn prompt cache-stable.
//
// Layers:
//
//	L0 — identity + universal contract (always on, mode rules)
//	L1 — dynamic session state (LSP, plan mode, pre-flight, auto-fixes)
//	L2 — skills catalog (relevance-filtered when it grows past a threshold)
//	L3 — project knowledge (repo map, project context, memory warm start)
package prompt

import (
	"fmt"
	"strings"

	"github.com/plumpslabs/bro-code/internal/tokens"
)

// Stack is one detected repo language plus the files evidencing it.
type Stack struct {
	Name  string
	Files []string
}

// SkillEntry is the minimal catalog metadata the prompt needs to advertise a
// skill. The model loads the full SKILL.md itself via read_file (progressive
// disclosure level 2), so only name+description ever enter the prompt.
type SkillEntry struct {
	Name        string
	Description string
	Path        string
}

// Input is everything Assemble needs to render one turn's system prompt.
type Input struct {
	// Mode is the active engine mode: BUILDER, PLANNER, or MINER.
	Mode string
	// Iteration is the loop iteration (1-based). Blocks that must appear only
	// in the first prompt (pre-flight diagnostics, auto-fix results) check it.
	Iteration int
	// ProjectCtx is the compact structural project overview ("" disables).
	ProjectCtx string
	// RepoMap is the deterministic project map ("" disables).
	RepoMap string
	// Stacks are the repo's detected languages ("go", "node", "ts", ...)
	// with their evidence files. They render a one-line STACK hint ("STACK:
	// go (go.mod, main.go)") and bias the skill-catalog ranking so
	// stack-specific skills follow the repo instead of the model's guess.
	Stacks []Stack
	// Skills is the full installed skill catalog (empty disables the block).
	Skills []SkillEntry
	// UserPrompt is the current turn's query, used to relevance-filter the
	// skills catalog when it exceeds the tuning threshold.
	UserPrompt string
	// ScopeHint is the smart-scope markdown (relevance-ranked files extracted
	// from the user prompt) shown only on iteration 1 so the model focuses
	// exploration. Empty disables.
	ScopeHint string
	// MemoryWarm is the BM25-selected cross-session memory excerpt ("" skips).
	MemoryWarm string
	// KnowledgeHints is the Smart Context Graph summary — a compact list of
	// previously analyzed files related to the current prompt, with their
	// content hashes so the model can skip stale re-reads. "" when knowledge
	// store is disabled or no match.
	KnowledgeHints string
	// NotesHints is the distilled self-aware context — facts/decisions/gotchas/
	// hot files recalled from past sessions (from the notes store). Shown only
	// when relevant so the model starts with verified prior knowledge.
	NotesHints string
	// LSPAvailable is the number of reachable language servers (0 = none).
	LSPAvailable int
	// Preflight holds engine-gathered diagnostics + code windows, shown only
	// on iteration 1 so the cached prefix stays stable afterwards.
	Preflight string
	// PreflightAuto reports fixes the engine already applied pre-turn; the
	// model must not redo them. Shown only on iteration 1.
	PreflightAuto string
	// PlanMode renders the read-only PLAN-pass directive when set.
	PlanMode bool
	// ActivePlan carries the active task plan from .brocode/current_plan.md.
	ActivePlan string
	// AgentPrompt carries custom instructions from an active user-defined CustomAgent.
	AgentPrompt string
	// SessionEditSummary lists files changed in the current session so modes
	// like MINER can reference prior edits without re-scanning. Populated by
	// the engine when tool.ChangesLen() > 0 at turn start.
	SessionEditSummary string
	// ModelTier classifies the active model's instruction-following capability
	// so the prompt builder can inject a proportional number of rules:
	// "weak" (5 rules), "medium" (10 rules), "strong" (full 17 rules).
	// Empty defaults to "strong" (all rules).
	ModelTier string
	// MemoryIndex is a compact table of contents for project memory sections.
	// Always loaded into context so the agent knows WHAT knowledge exists
	// even after compaction. Empty disables.
	MemoryIndex string
	// Tuning carries the runtime surface (block toggles, rule toggles, skill
	// catalog budgets). Nil falls back to DefaultTuning.
	Tuning *Tuning
}

// Block is one composable segment of the system prompt. Render returns "" to
// skip the block. Always marks blocks that render even when tuning disables
// the optional ones (identity + mode rules are the irreducible contract).
type Block struct {
	Name   string
	Always bool
	Render func(in *Input) string
}

// Assemble renders the system prompt from the layered block list and returns
// the prompt plus a per-block token-cost map (useful for budgeting and tests).
// The result is deterministic for a given Input, so the engine can cache it
// once per turn and reuse it across loop iterations (provider prompt caching).
func Assemble(in *Input) (string, map[string]int) {
	if in == nil {
		in = &Input{}
	}
	if in.Tuning == nil {
		in.Tuning = DefaultTuning()
	}
	var sb strings.Builder
	costs := make(map[string]int)
	for _, b := range blocks(in) {
		if !b.Always && !in.Tuning.BlockOn(b.Name) {
			continue
		}
		text := b.Render(in)
		if text == "" {
			continue
		}
		costs[b.Name] = tokens.EstimateTokens(text)
		sb.WriteString(text)
	}
	return sb.String(), costs
}

// blocks returns the ordered block list. The order preserves the historical
// prompt layout (identity → project → skills → LSP → memory → pre-flight →
// plan → mode rules) so the refactor is behavior-neutral: a rendered prompt
// for a given Input is byte-identical to the pre-refactor output. Stable
// ordering also keeps the per-turn cache prefix stable across iterations.
func blocks(_ *Input) []Block {
	return []Block{
		// L0 — identity + project context + detected stack.
		{Name: "identity", Always: true, Render: renderIdentity},
		{Name: "stack", Render: renderStack},
		// L3 — project knowledge (repo map, memory warm start).
		{Name: "repomap", Render: renderRepoMap},
		// L2 — skills catalog (relevance-filtered at scale).
		{Name: "skills", Render: renderSkillsBlock},
		// L1 — dynamic session state (LSP, pre-flight, plan gate, smart scope).
		{Name: "lsp", Render: renderLSP},
		{Name: "memory", Render: renderMemory},
		{Name: "memory_index", Render: renderMemoryIndex},
		{Name: "notes", Render: renderNotesHints},
		{Name: "knowledge", Render: renderKnowledgeHints},
		{Name: "scope", Render: renderScopeHint},
		{Name: "preflight", Render: renderPreflight},
		{Name: "preflight_autofix", Render: renderPreflightAuto},
		{Name: "active_plan", Render: renderActivePlan},
		{Name: "plan_mode", Render: renderPlanMode},
		// Session edit summary: tells MINER/PLANNER about files changed this turn.
		{Name: "session_edits", Render: renderSessionEdits},
		// L0 — custom agent instructions (if active).
		{Name: "custom_agent", Render: renderCustomAgent},
		// L0 — spec-driven workflow for BUILDER mode.
		{Name: "spec_workflow", Render: renderSpecWorkflow},
		// L0 — universal contract: mode header + tunable mode rules.
		{Name: "mode", Always: true, Render: renderMode},
	}
}

func renderIdentity(in *Input) string {
	mode := in.Mode
	if mode == "" {
		mode = "BUILDER"
	}
	var sb strings.Builder
	sb.WriteString("You are BroCode CLI, an autonomous AI coding assistant. ")
	switch mode {
	case "MINER":
		sb.WriteString("Currently operating strictly in **MINER mode (🟡)**. You are a dedicated, read-only Project Knowledge & Architecture Mining Agent. Your purpose is to explore the codebase and extract verified architectural facts, gotchas, and conventions into project memory (`.brocode/memory.md`). You CANNOT and DO NOT edit source code files. When asked who you are or what mode you are in, explicitly state that you are BroCode in MINER mode (🟡) and that you explore and remember project knowledge.\n")
	case "PLANNER":
		sb.WriteString("Currently operating strictly in **PLANNER mode (🟣)**. You are a dedicated, read-only Architecture Analysis & Strategy Planning Agent. Your purpose is to inspect the codebase and draft step-by-step implementation plans without modifying source code. You CANNOT and DO NOT edit source code files. When asked who you are or what mode you are in, explicitly state that you are BroCode in PLANNER mode (🟣) and that you analyze architecture and draft plans.\n")
	default:
		sb.WriteString("Currently operating in **BUILDER mode (🟢)**. You are an autonomous coding and software engineering agent. You can read, edit, refactor source code files, and run terminal verification commands. When asked who you are or what mode you are in, explicitly state that you are BroCode in BUILDER mode (🟢).\n")
	}
	sb.WriteString("You operate with the mindset of a pragmatic, perfectionist Senior/Staff Engineer:\n")
	sb.WriteString("  [EVIDENCE-BASED SENIOR CANDOR]: Never shower the user with empty flattery ('Great idea!', 'Awesome question!'). Jump straight into technical facts and execution. Criticism MUST be grounded in concrete evidence: cite the exact file, line number, complexity risk, or failure scenario (e.g. O(N^2) loops, race conditions, memory leaks, unindexed queries, unhandled errors). Never criticize blindly or dogmatically; always provide the technical proof and the minimal, superior alternative. If a design is already solid, state it simply ('Ini sudah clean dan solid') without hype.\n")
	sb.WriteString("  [CONVERSATIONAL INTENT & ZERO HYPERACTIVITY]: When the user's message is a greeting, gratitude, or casual acknowledgment (e.g. 'ok terima kasih', 'thanks', 'siap', 'mantap', 'halo', 'ok done', 'keren', 'makasih'), DO NOT invoke tools, DO NOT explore or search the codebase, DO NOT draft unsolicited plans, and DO NOT start background tasks. Simply reply with a concise, polite acknowledgment (1-2 sentences) and await the next explicit instruction.\n")
	sb.WriteString("  [NATIVE SOVEREIGNTY]: You operate exclusively using BroCode's native tools, workflow, and storage directory (`.brocode/`, `.brocode/current_plan.md`, `.brocode/memory.md`). NEVER look for, create, or modify plans, memory, or context in third-party framework directories (such as `.agents/`, `.cursor/`, `.windsurf/`, or `.claude/`). BroCode native tools are your first-choice primary toolset.\n")
	if strings.TrimSpace(in.ProjectCtx) != "" {
		sb.WriteString("You are working in this project:\n\n")
		sb.WriteString(in.ProjectCtx)
	}
	return sb.String()
}

func renderRepoMap(in *Input) string {
	if in.RepoMap == "" {
		return ""
	}
	return "\n\n" + in.RepoMap
}

func renderStack(in *Input) string {
	if len(in.Stacks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(in.Stacks))
	for _, s := range in.Stacks {
		if len(s.Files) > 0 {
			parts = append(parts, fmt.Sprintf("%s (%s)", s.Name, strings.Join(s.Files, ", ")))
		} else {
			parts = append(parts, s.Name)
		}
	}
	return "\nSTACK: " + strings.Join(parts, ", ")
}

func renderMemory(in *Input) string {
	if in.MemoryWarm == "" {
		return ""
	}
	return "\n\nPROJECT MEMORY (learned in past sessions, use as verified prior knowledge — confirm details against the code when they matter):\n" + in.MemoryWarm + "\n\n⚠️ Do NOT re-read .brocode/memory.md — the relevant facts are already shown above. Only open SOURCE files to confirm specifics."
}

// renderMemoryIndex renders a compact table of contents for project memory.
// This is the "L1 pointer file" — always loaded so the agent knows WHAT
// knowledge exists even after compaction. Use the memory tool to recall details.
func renderMemoryIndex(in *Input) string {
	if in.MemoryIndex == "" {
		return ""
	}
	return "\n\n" + in.MemoryIndex
}

func renderKnowledgeHints(in *Input) string {
	if in.KnowledgeHints == "" {
		return ""
	}
	return "\n\n🧠 SMART CONTEXT (from prior sessions — skip re-reading unchanged files):\n" + in.KnowledgeHints
}

func renderNotesHints(in *Input) string {
	if in.NotesHints == "" {
		return ""
	}
	return "\n\n🧠 SELF-AWARE CONTEXT (distilled from past sessions — treat as a starting mental model / verified prior knowledge; confirm details against the code when they matter):\n" + in.NotesHints + "\n\n⚠️ Do NOT re-read .brocode/memory.md; the distilled insights above are sufficient. Only open SOURCE files to confirm specifics — do not re-scan files already summarized above."
}

func renderLSP(in *Input) string {
	if in.LSPAvailable > 0 {
		return fmt.Sprintf("\n\nLSP AVAILABLE (%d language server(s)): use `lsp_scan` for project-wide type/lint/deprecated diagnostics and `lsp_diagnostics` per file — that IS your linter, no external install needed.", in.LSPAvailable)
	}
	return "\n\nLSP NOT AVAILABLE this session: `lsp_scan` will fail. Do NOT `go install` external linters (golangci-lint/staticcheck/revive) — instead OFFER to run `/lsp-install` for the user (or propose it via ask_user) and rely on the project's own `go vet`/`go build`/`tsc --noEmit` in the meantime. Do not merely instruct the user to do it manually."
}

func renderPreflight(in *Input) string {
	if in.Preflight == "" || in.Iteration != 1 {
		return ""
	}
	return "\n\n" + in.Preflight
}

func renderPreflightAuto(in *Input) string {
	if in.PreflightAuto == "" || in.Iteration != 1 {
		return ""
	}
	return "\n\nPRE-APPLIED AUTO-FIXES (already done by the engine — DO NOT redo):\n" + in.PreflightAuto
}

func renderPlanMode(in *Input) string {
	if !in.PlanMode {
		return ""
	}
	return `

📋 PLAN MODE (this turn is a read-only PLANNING pass): For this implementation task, RESEARCH the codebase with read-only tools (read_file, code_locate, grep, glob, lsp_* inspect tools), then output a concise numbered implementation plan. BEFORE any file is edited you MUST call ask_user to confirm it, with options: "Approve & build", "Revise plan", "Cancel". Do NOT call any mutating tool (write_file, edit_file, delete_file, lsp_fix, lsp_autofix, lsp_rename) — they are blocked until the plan is approved. Once the user picks "Approve & build", you may implement in the next step.`
}

func renderScopeHint(in *Input) string {
	if in.ScopeHint == "" || in.Iteration != 1 {
		return ""
	}
	return "\n\n" + in.ScopeHint
}

func renderActivePlan(in *Input) string {
	if strings.TrimSpace(in.ActivePlan) == "" {
		return ""
	}
	if in.Mode == "PLANNER" {
		return "\n\n📋 EXISTING PLAN IN WORKSPACE (.brocode/current_plan.md):\n" + strings.TrimSpace(in.ActivePlan) + "\n(If the user's request is a continuation, refine or extend this plan. If the user is starting a new unrelated task or bugfix, output the fresh plan and BroCode will automatically archive this previous plan to .brocode/plans/archive/)."
	}
	return "\n\n🎯 ACTIVE TASK PLAN (.brocode/current_plan.md):\nYou are executing this plan. Confine your work strictly to these tasks and impacted files. BroCode automatically tracks file edits and checks off completed tasks in .brocode/current_plan.md:\n" + strings.TrimSpace(in.ActivePlan) + "\n\n⚠️ Do NOT re-read .brocode/current_plan.md — the full plan is already shown above and is authoritative. BroCode auto-syncs your edits and check-offs; trust the text above and act on it directly."
}

func renderCustomAgent(in *Input) string {
	if strings.TrimSpace(in.AgentPrompt) == "" {
		return ""
	}
	return "\n\n### 🤖 CUSTOM AGENT SPECIFICATION & DIRECTIVES:\n" + strings.TrimSpace(in.AgentPrompt)
}

func renderSessionEdits(in *Input) string {
	if strings.TrimSpace(in.SessionEditSummary) == "" {
		return ""
	}
	return "\n📝 SESSION EDITS: " + strings.TrimSpace(in.SessionEditSummary) + "\n"
}

// renderSpecWorkflow injects a mini-spec workflow for BUILDER mode so the
// agent structures its work instead of randomly reading files. Only shown
// on iteration 1 (first prompt) to keep the cached prefix stable.
func renderSpecWorkflow(in *Input) string {
	if in.Mode != "BUILDER" || in.Iteration != 1 {
		return ""
	}
	return `

📋 SPEC-DRIVEN WORKFLOW (follow this for every task):
1. UNDERSTAND: Read 2-3 key files to understand the task scope.
2. PLAN: Output a mini-spec: GOAL (what done looks like), SCOPE (files to touch), ACCEPTANCE (how to verify).
3. EXECUTE: Edit files. Do NOT re-read files you just edited — trust your edit.
4. VERIFY: Run build/test command. If it fails, fix and re-verify. Stop when it passes.
5. DONE: Output a brief summary of changes. Do NOT narrate steps — just show the result.
If the task is simple (typo, rename, one-line fix), skip to step 3 directly.`
}
