package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/plumpslabs/bro-code/internal/subagent"
)

// repairResultMsg carries the finished diagnostic and repair report.
type repairResultMsg string

// executeRepairCommand runs the autonomous Pipeline Doctor loop to fix test/build failures.
func executeRepairCommand(runner *subagent.Runner, errorContext string, prog *tea.Program) tea.Cmd {
	return tea.Batch(tickCmd(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
		defer cancel()

		if strings.TrimSpace(errorContext) == "" {
			errorContext = "Recent build or test failure in this workspace."
		}

		prompt := fmt.Sprintf(
			"You are the BroCode Pipeline Doctor & Automated Triage Specialist.\n\n"+
				"FAILURE / ERROR CONTEXT:\n%s\n\n"+
				"YOUR MISSION (SYSTEMATIC ROOT-CAUSE DIAGNOSIS & VERIFIED SURGICAL REPAIR):\n"+
				"1. Language: Formulate your explanations in the SAME language as the prompt (use Bahasa Indonesia if requested in Indonesian).\n"+
				"2. Working Directory: You are ALREADY in the project repository root. Do NOT attempt to run 'cd' to switch directories.\n"+
				"3. STEP 1 (DIAGNOSIS): If a specific test or error log is provided, trace the failing stack trace to the exact file and line number. Use 'code_slice' or 'code_locate' to inspect the failing function without reading 3000-line whole files. If no error is given, run tests first via 'run_tests' or 'bash' to capture failures.\n"+
				"4. STEP 2 (SURGICAL FIX): Apply the minimal surgical fix using 'edit_symbol' (preferred for functions/methods) or 'edit_file'. Do NOT rewrite unrelated code.\n"+
				"5. STEP 3 (VERIFICATION): Re-run the relevant test suite or compiler check to PROVE that the failure is resolved and tests pass!\n"+
				"6. STEP 4 (SUMMARY): Present a concise report with:\n"+
				"   • 🔍 Root Cause (why it failed)\n"+
				"   • 🛠️ Surgical Patch Applied (file:lines)\n"+
				"   • ✅ Verification Outcome (test pass output)\n",
			errorContext,
		)

		res, err := runner.RunWithProgress(ctx, prompt, "BUILDER", nil)
		if err != nil {
			return repairResultMsg(fmt.Sprintf("❌ /repair failed: %v", err))
		}

		return repairResultMsg(res)
	})
}
