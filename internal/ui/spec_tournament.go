package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/subagent"
)

// specResultMsg carries the finished architectural blueprint specification.
type specResultMsg string

// tournamentResultMsg carries the multi-candidate tournament comparison report.
type tournamentResultMsg string

var nonAlphaNumRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonAlphaNumRe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = "feature_spec"
	}
	return s
}

// executeSpecCommand generates a formal architectural contract spec before coding.
func executeSpecCommand(runner *subagent.Runner, feature string, prog *tea.Program) tea.Cmd {
	return tea.Batch(tickCmd(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
		defer cancel()

		prompt := fmt.Sprintf(
			"You are an expert Principal Architect. Draft a rigorous, production-grade Architectural Blueprint Specification for:\n\n"+
				"\"%s\"\n\n"+
				"Ground truth requirements (EVIDENCE-FIRST ARCHITECTURAL ACCURACY):\n"+
				"1. Language: Formulate your explanations and sections in the SAME language as the prompt (use Bahasa Indonesia if the user wrote in Indonesian).\n"+
				"2. Working Directory: You are ALREADY in the project repository root. Do NOT attempt to run 'cd' or switch directories.\n"+
				"3. Inspect the codebase thoroughly using tools (code_locate, blast_radius, grep, read_file, glob) to inspect real repository files.\n"+
				"4. ZERO ASSUMPTIONS ON MIDDLEWARE: Trace route definitions to handler chains to verify the exact middleware attaching request context (e.g. req.user, req.workspaceSubscription) rather than assuming standard JWT auth middlewares.\n"+
				"5. EXACT PAYLOAD & MAPPING KEYS: For third-party webhooks and services (e.g. Midtrans, Stripe, payment gateways), read the exact controller and service files to cite the actual payload keys (e.g. transaction_status vs generic notification_type) and mapping constants.\n"+
				"6. EXACT GUARD & CONDITIONAL LOGIC: Cite exact file paths (e.g. file.js:Lxx) for state transitions, guard conditions, and error branches.\n"+
				"7. Output ONLY structured markdown with these 6 sections:\n"+
				"## 🎯 1. Objective & Architecture Context\n"+
				"## 📐 2. Interface Contracts, Functions & Data Types\n"+
				"## 🗄️ 3. Database Schema & State Changes\n"+
				"## ⚠️ 4. Blast Radius & Affected Callers\n"+
				"## ✅ 5. Verification & Acceptance Criteria\n"+
				"## 🚀 6. Phased Implementation Checklist\n"+
				"- [ ] Phase 1: Core Data Models & Migrations\n"+
				"- [ ] Phase 2: Internal Services, Domain Logic & Business Rules\n"+
				"- [ ] Phase 3: Route Handlers, Middlewares & Controllers\n"+
				"- [ ] Phase 4: Integration Verification, Tests & Final Blast Radius Check\n\n"+
				"Do NOT write or edit source code files. Focus on architectural precision and actionable phased steps.",
			feature,
		)

		metrics, err := runner.RunWithProgressMetrics(ctx, prompt, "PLANNER", func(state loop.LoopState, info string) {
			if prog != nil {
				prog.Send(stepProgressMsg{state: state, info: info})
			}
		})
		if err != nil {
			return specResultMsg(fmt.Sprintf("❌ `/spec` failed: %v", err))
		}
		ans := metrics.Answer

		// Save to .brocode/specs/YYYY-MM-DD_slug.md
		cwd, _ := os.Getwd()
		specsDir := filepath.Join(cwd, ".brocode", "specs")
		_ = os.MkdirAll(specsDir, 0o755)

		fileName := fmt.Sprintf("%s_%s.md", time.Now().Format("2006-01-02"), slugify(feature))
		specPath := filepath.Join(specsDir, fileName)
		_ = os.WriteFile(specPath, []byte(ans), 0o644)

		relPath := filepath.ToSlash(filepath.Join(".brocode", "specs", fileName))
		return specResultMsg(fmt.Sprintf("SPEC:\n%s\n---\n%s", relPath, ans))
	})
}

// executeTournamentCommand runs 2 parallel candidate agents to solve a difficult task or bug,
// followed by an autonomous Arbiter evaluation to score and recommend the best patch.
func executeTournamentCommand(runner *subagent.Runner, task string, prog *tea.Program) tea.Cmd {
	return tea.Batch(tickCmd(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 360*time.Second)
		defer cancel()

		if prog != nil {
			prog.Send(stepProgressMsg{state: loop.StateThinking, info: "🥊 [Tournament Arena]: Initializing Candidate-Alpha & Candidate-Beta..."})
		}

		agents := []subagent.SubAgent{
			{
				ID: "Candidate-Alpha (Minimal Surgical Fix)",
				Task: "Goal: " + task + "\n\n" +
					"Role: You are Principal Staff Engineer Alpha. Your mandate is to find the exact root cause and formulate the cleanest, lowest-risk SURGICAL HOTFIX with zero collateral damage.\n\n" +
					"Evidence-First Grounding:\n" +
					"1. Language: Formulate your reasoning and response in the SAME language as the task (use Bahasa Indonesia if the prompt is in Indonesian).\n" +
					"2. Working Directory: You are ALREADY in the repository root. Use codebase tools (code_locate, grep, read_file, read_files, glob) directly without 'cd'.\n" +
					"3. Deep Root Cause Analysis: Trace callers, routes, payload schemas, and sanitization logic. Cite exact file paths and line numbers (e.g. service.js:L123).\n" +
					"4. Formulate Hypothesis: Clearly state WHY the bug happens (e.g. unescaped characters, missing return fields, wrong type mapping).\n" +
					"5. Verbatim Surgical Patch: Provide exact target and replacement code snippets so the fix can be applied instantly.\n\n" +
					"Output Structure:\n" +
					"### 🔍 1. Root Cause & Code Evidence (cite file:Lxx)\n" +
					"### 💡 2. Proposed Surgical Patch (verbatim code diff)\n" +
					"### 📊 3. Risk & Blast Radius Assessment (Regression risk: LOW)\n" +
					"Do NOT edit source files directly. Produce complete, production-grade analysis.",
				Mode: "PLANNER",
			},
			{
				ID: "Candidate-Beta (Defensive Robust Refactor)",
				Task: "Goal: " + task + "\n\n" +
					"Role: You are Principal Architect Beta. Your mandate is to eliminate the root cause while building ROBUST, DEFENSIVE ARCHITECTURAL GUARDS against future regressions.\n\n" +
					"Evidence-First Grounding:\n" +
					"1. Language: Formulate your reasoning and response in the SAME language as the task (use Bahasa Indonesia if the prompt is in Indonesian).\n" +
					"2. Working Directory: You are ALREADY in the repository root. Use codebase tools (code_locate, grep, read_file, read_files, glob) directly without 'cd'.\n" +
					"3. Architectural Root Cause: Audit not just the immediate symptom, but also upstream callers, input validation, and downstream handlers.\n" +
					"4. Defensive Hardening: Formulate robust sanitization, fallback defaults, strict type-checking, and comprehensive error boundaries.\n" +
					"5. Verbatim Implementation: Provide exact target and replacement code snippets and test verification steps.\n\n" +
					"Output Structure:\n" +
					"### 🔍 1. Root Cause & Architectural Flaws (cite file:Lxx)\n" +
					"### 💡 2. Defensive Solution & Hardened Guard (verbatim code diff)\n" +
					"### 📊 3. Long-term Stability & Trade-offs (Future-proof: HIGH)\n" +
					"Do NOT edit source files directly. Produce complete, production-grade analysis.",
				Mode: "PLANNER",
			},
		}

		metricsList, err := runner.RunManyMetrics(ctx, agents, true, func(state loop.LoopState, info string) {
			if prog != nil {
				prog.Send(stepProgressMsg{state: state, info: info})
			}
		})
		if err != nil {
			return tournamentResultMsg(fmt.Sprintf("❌ `/tournament` failed: %v", err))
		}

		var sb strings.Builder
		for i, rep := range metricsList {
			agentID := agents[i].ID
			sb.WriteString(fmt.Sprintf("### 🥊 %s\n%s\n\n---\n\n", agentID, rep.Answer))
		}

		// Phase 3: Autonomous Arbiter Judge
		if prog != nil {
			prog.Send(stepProgressMsg{state: loop.StateThinking, info: "⚖️ [Arbiter Judge]: Evaluating Candidate solutions & scoring trade-offs..."})
		}

		alphaAns := ""
		betaAns := ""
		if len(metricsList) > 0 {
			alphaAns = metricsList[0].Answer
		}
		if len(metricsList) > 1 {
			betaAns = metricsList[1].Answer
		}

		judgePrompt := fmt.Sprintf(
			"You are the Chief Technology Arbiter. Evaluate these two competing engineering solutions for the following problem:\n\n"+
				"PROBLEM STATEMENT:\n%s\n\n"+
				"CANDIDATE-ALPHA (Minimal Surgical Fix):\n%s\n\n"+
				"CANDIDATE-BETA (Defensive Robust Refactor):\n%s\n\n"+
				"Analyze both solutions impartially and output a concise, structured evaluation in the user's language (Bahasa Indonesia if the problem statement is in Indonesian):\n\n"+
				"### ⚖️ ARBITER VERDICT & SCORING MATRIX\n"+
				"| Criterion | Candidate-Alpha (Surgical) | Candidate-Beta (Robust) |\n"+
				"| :--- | :--- | :--- |\n"+
				"| 🎯 Root Cause Accuracy | [Score 1-10 & note] | [Score 1-10 & note] |\n"+
				"| 🛡️ Blast Radius / Risk | [Low/Med/High & note] | [Low/Med/High & note] |\n"+
				"| ⚡ Implementation Speed | [Immediate / Moderate] | [Moderate / High Effort] |\n"+
				"| 💎 Code Cleanliness | [Score 1-10] | [Score 1-10] |\n\n"+
				"🏆 **RECOMMENDED CHOICE**: State clearly whether Alpha or Beta is recommended and WHY.\n"+
				"👉 **APPLY INSTRUCTION**: State clearly how the user can apply it in BUILDER mode (e.g. `Apply Alpha` or `Apply Beta`).",
			task, alphaAns, betaAns,
		)

		judgeMetrics, jErr := runner.RunWithProgressMetrics(ctx, judgePrompt, "PLANNER", func(state loop.LoopState, info string) {
			if prog != nil {
				prog.Send(stepProgressMsg{state: state, info: "⚖️ Arbiter: " + info})
			}
		})

		if jErr == nil && strings.TrimSpace(judgeMetrics.Answer) != "" {
			sb.WriteString(judgeMetrics.Answer)
		} else {
			// Deterministic fallback matrix
			sb.WriteString("### ⚖️ ARBITER DECISION MATRIX\n")
			sb.WriteString("- **Choose Candidate-Alpha**: Best for urgent production hotfixes, lowest regression risk, and minimal lines of code changed.\n")
			sb.WriteString("- **Choose Candidate-Beta**: Best for long-term architectural stability, deep edge-case guards, and paying down technical debt.\n\n")
			sb.WriteString("👉 **Next Action:** In **BUILDER** mode (`Shift+Tab`), simply say `Apply Alpha` or `Apply Beta` to execute the chosen patch.")
		}

		return tournamentResultMsg(fmt.Sprintf("TOURNAMENT:\n%s\n---\n%s", task, sb.String()))
	})
}
