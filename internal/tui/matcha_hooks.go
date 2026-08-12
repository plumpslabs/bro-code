// matcha_hooks.go — Native agentic hooks integration: programmatic enforcement
// of the 6-Checkpoint Filter, safety shield, and post-write cleanup.
// This file executes these checks natively in Go, dropping the Node.js dependency.
package tui

import (
	"fmt"
	"os"
	"strings"
)

// ─── Planning Gate ──────────────────────────────────────────────────────────

// checkPlanningGate verifies natively that a valid Intent Discovery plan exists
// before code modifications. Returns empty string if the gate passes, or a
// message explaining why the action is blocked.
func checkPlanningGate(toolName, targetFile string) string {
	planPath := ".agents/plan/current.md"
	content, err := os.ReadFile(planPath)
	if err != nil {
		// If no plan, block edit
		return "🍵 Planning Gate Blocked\nNo plan file found at .agents/plan/current.md. Create one before modifying code."
	}
	
	text := string(content)
	if !strings.Contains(text, "Intent Discovery") {
		return "🍵 Planning Gate Blocked\nThe plan file does not contain an Intent Discovery block."
	}

	return ""
}

// ─── Shield Check ───────────────────────────────────────────────────────────

// checkShield verifies a shell command is safe natively.
// Returns empty string if safe, or a warning message if dangerous.
func checkShield(command string) string {
	lowerCmd := strings.ToLower(strings.TrimSpace(command))
	
	dangerousPatterns := []string{
		"rm -rf /",
		"rm -rf *",
		"git push --force",
		"git reset --hard",
	}

	for _, pattern := range dangerousPatterns {
		if strings.Contains(lowerCmd, pattern) {
			return fmt.Sprintf("🛡️ Shield Blocked: Command contains dangerous pattern '%s'", pattern)
		}
	}
	return ""
}

// ─── Post-Write Scan ────────────────────────────────────────────────────────

// checkPostWrite scans a file natively for cleanup issues.
// Returns cleanup findings or empty string if clean.
func checkPostWrite(filePath string) string {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return ""
	}
	
	text := string(content)
	findings := []string{}
	
	if strings.Contains(text, "TODO:") || strings.Contains(text, "FIXME:") {
		findings = append(findings, "Found TODO/FIXME markers.")
	}
	if strings.Contains(text, "console.log(") || strings.Contains(text, "fmt.Println(\"test") {
		findings = append(findings, "Found debug print statements.")
	}
	
	if len(findings) > 0 {
		return "Post-write scan findings: " + strings.Join(findings, " ")
	}
	
	return ""
}

// ─── Intent Discovery Enforcer ──────────────────────────────────────────────

// enforceIntentDiscovery checks if the current task requires a plan before
// proceeding. Returns a trace message explaining the check, or empty if no
// gate is needed.
func enforceIntentDiscovery(query string) string {
	q := strings.TrimSpace(query)
	if len(q) < 10 || strings.HasPrefix(q, "/") || strings.HasPrefix(q, "?") {
		return ""
	}

	planPath := ".agents/plan/current.md"
	if _, err := os.Stat(planPath); err == nil {
		return "● Intent Discovery: plan exists ✓"
	}
	return "● Intent Discovery: no plan found — consider /matcha:why first for complex tasks"
}

// ─── Agentic Status ─────────────────────────────────────────────────────────

// matchaStatus returns a summary of the native integration state.
func matchaStatus() string {
	status := []string{"🟢 native hooks active"}
	planPath := ".agents/plan/current.md"
	if _, err := os.Stat(planPath); err == nil {
		status = append(status, "📋 plan: active")
	} else {
		status = append(status, "📋 plan: none")
	}
	return strings.Join(status, " · ")
}
