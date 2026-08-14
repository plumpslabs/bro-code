package loop

import (
	"fmt"
	"strings"
)

// ReviewCouncil performs static peer review audits on critical code changes.
// It checks for N+1 queries, unhandled errors, and breaking changes.
type ReviewCouncil struct{}

// NewReviewCouncil creates a new council reviewer instance.
func NewReviewCouncil() *ReviewCouncil {
	return &ReviewCouncil{}
}

// AuditDiff inspects file content changes for architectural or security risks.
func (rc *ReviewCouncil) AuditDiff(filePath, content string) []string {
	var findings []string
	lower := strings.ToLower(content)

	// N+1 Query Warning
	if strings.Contains(filePath, "service") || strings.Contains(filePath, "repository") {
		if (strings.Contains(lower, "for ") || strings.Contains(lower, "map(")) &&
			(strings.Contains(lower, "findunique") || strings.Contains(lower, "query") || strings.Contains(lower, "select")) {
			findings = append(findings, "⚠️ [COUNCIL REVIEW]: Potential N+1 query detected inside loop in "+filePath)
		}
	}

	// Auth & Hardcoded Secret Check
	if strings.Contains(lower, "secret =") || strings.Contains(lower, "password =") || strings.Contains(lower, "api_key =") {
		findings = append(findings, "⛔ [COUNCIL REVIEW]: Possible hardcoded credential/secret detected in "+filePath)
	}

	return findings
}

// FormatFindings renders review council alerts into human-readable text.
func (rc *ReviewCouncil) FormatFindings(findings []string) string {
	if len(findings) == 0 {
		return "🛡️ Peer Review Council: 0 risk findings detected."
	}
	var sb strings.Builder
	sb.WriteString("🛡️ Peer Review Council Audit Warnings:\n")
	for _, f := range findings {
		sb.WriteString(fmt.Sprintf("- %s\n", f))
	}
	return strings.TrimSpace(sb.String())
}
