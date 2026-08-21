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
		{Name: "notes", Render: renderNotesHints},
		{Name: "knowledge", Render: renderKnowledgeHints},
		{Name: "scope", Render: renderScopeHint},
		{Name: "preflight", Render: renderPreflight},
		{Name: "preflight_autofix", Render: renderPreflightAuto},
		{Name: "active_plan", Render: renderActivePlan},
		{Name: "plan_mode", Render: renderPlanMode},
		// L0 — universal contract: mode header + tunable mode rules.
		{Name: "mode", Always: true, Render: renderMode},
	}
}

func renderIdentity(in *Input) string {
	var sb strings.Builder
	sb.WriteString("You are BroCode CLI, an autonomous AI coding assistant. You operate with the mindset of a pragmatic, perfectionist Senior/Staff Engineer:\n")
	sb.WriteString("  [SENIOR CANDOR & CRITIQUE]: Never shower the user with empty flattery ('Great idea!', 'Awesome question!'). Jump straight into technical facts and execution. If existing code or a requested approach has flaws (anti-patterns, tight coupling, race conditions, security traps, N+1 queries, bad naming), call them out directly and constructively with concrete evidence and better architectural alternatives. If a design is already solid, state it simply ('Ini sudah clean dan solid') without hype.\n")
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
	return "\n\nPROJECT MEMORY (learned in past sessions, use as verified prior knowledge — confirm details against the code when they matter):\n" + in.MemoryWarm
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
	return "\n\n🧠 SELF-AWARE CONTEXT (distilled from past sessions — treat as a starting mental model / verified prior knowledge; confirm details against the code when they matter, and explore freely when uncertain):\n" + in.NotesHints
}

func renderLSP(in *Input) string {
	if in.LSPAvailable > 0 {
		return fmt.Sprintf("\n\nLSP AVAILABLE (%d language server(s)): use `lsp_scan` for project-wide type/lint/deprecated diagnostics and `lsp_diagnostics` per file — that IS your linter, no external install needed.", in.LSPAvailable)
	}
	return "\n\nLSP NOT AVAILABLE this session: `lsp_scan` will fail. Do NOT `go install` external linters (golangci-lint/staticcheck/revive) — ask the user to run `/lsp-install` once, or rely on the project's own `go vet`/`go build`/`tsc --noEmit`."
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
	return "\n\n🎯 ACTIVE TASK PLAN (.brocode/current_plan.md):\nYou are executing this plan. Confine your work strictly to these tasks and impacted files:\n" + strings.TrimSpace(in.ActivePlan)
}
