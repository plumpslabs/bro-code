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
				"7. Output ONLY structured markdown with these 5 sections:\n"+
				"## 🎯 1. Objective & Architecture Context\n"+
				"## 📐 2. Interface Contracts, Functions & Data Types\n"+
				"## 🗄️ 3. Database Schema & State Changes\n"+
				"## ⚠️ 4. Blast Radius & Affected Callers\n"+
				"## ✅ 5. Verification & Acceptance Criteria\n\n"+
				"Do NOT write or edit source code files. Focus on architectural precision.",
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

// executeTournamentCommand runs 2 parallel candidate agents to solve a difficult task or bug.
func executeTournamentCommand(runner *subagent.Runner, task string, prog *tea.Program) tea.Cmd {
	return tea.Batch(tickCmd(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
		defer cancel()

		agents := []subagent.SubAgent{
			{
				ID: "Candidate-Alpha (Minimal Surgical Fix)",
				Task: "Goal: " + task + "\n" +
					"Context: You are ALREADY in the repository root. Use codebase tools (code_locate, grep, read_file, glob) to inspect files directly without 'cd'.\n" +
					"Language: Formulate your explanations and analysis in the SAME language as the task (use Bahasa Indonesia if the prompt is in Indonesian).\n" +
					"Efficiency: Locate key functions and schemas directly using code_locate and grep. Read targeted function sections instead of small repetitive micro-slices.\n" +
					"Strategy: Find the exact root cause and formulate the most MINIMAL, high-precision surgical fix with zero collateral damage.\n" +
					"Structure your output cleanly with:\n" +
					"1. 🔍 Root Cause & Evidence (cite file:Lxx)\n" +
					"2. 💡 Proposed Surgical Patch / Fix\n" +
					"3. 📊 Risk Assessment (Blast Radius: LOW, minimal churn)\n" +
					"Do NOT edit source files yet. Formulate precise analysis and diff.",
				Mode: "PLANNER",
			},
			{
				ID: "Candidate-Beta (Defensive Robust Refactor)",
				Task: "Goal: " + task + "\n" +
					"Context: You are ALREADY in the repository root. Use codebase tools (code_locate, grep, read_file, glob) to inspect files directly without 'cd'.\n" +
					"Language: Formulate your explanations and analysis in the SAME language as the task (use Bahasa Indonesia if the prompt is in Indonesian).\n" +
					"Efficiency: Locate key functions and schemas directly using code_locate and grep. Read targeted function sections instead of small repetitive micro-slices.\n" +
					"Strategy: Find the root cause and formulate a ROBUST, defensive fix with comprehensive type safety, input validation, and edge-case handling.\n" +
					"Structure your output cleanly with:\n" +
					"1. 🔍 Root Cause & Architecture Vulnerabilities (cite file:Lxx)\n" +
					"2. 💡 Proposed Robust Implementation & Defensive Guard\n" +
					"3. 📊 Long-term Trade-offs (Blast Radius: MEDIUM/HIGH, future-proof)\n" +
					"Do NOT edit source files yet. Formulate precise analysis and diff.",
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
		sb.WriteString("### ⚖️ ARBITER DECISION MATRIX\n")
		sb.WriteString("- **Choose Candidate-Alpha**: Best for urgent production hotfixes, lowest regression risk, and minimal lines of code changed.\n")
		sb.WriteString("- **Choose Candidate-Beta**: Best for long-term architectural stability, deep edge-case guards, and paying down technical debt.\n\n")
		sb.WriteString("👉 **Next Action:** In **BUILDER** mode (`Shift+Tab`), simply say `Apply Alpha` or `Apply Beta` to execute the chosen patch.")
		return tournamentResultMsg(fmt.Sprintf("TOURNAMENT:\n%s\n---\n%s", task, sb.String()))
	})
}
