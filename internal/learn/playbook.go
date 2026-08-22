package learn

import (
	"fmt"
	"strings"

	"github.com/plumpslabs/bro-code/internal/store"
)

// ExtractErrorPattern simplifies and normalizes a compiler/test error string into a compact pattern.
func ExtractErrorPattern(rawErr string) string {
	rawErr = strings.TrimSpace(rawErr)
	if rawErr == "" {
		return ""
	}
	lines := strings.Split(rawErr, "\n")
	var keyLines []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		// Skip stacktrace file lines like "at /path/to/file.js:123:45"
		if strings.HasPrefix(trimmed, "at ") || strings.HasPrefix(trimmed, "goroutine ") {
			continue
		}
		keyLines = append(keyLines, trimmed)
		if len(keyLines) >= 3 {
			break
		}
	}
	pattern := strings.Join(keyLines, " ")
	if len(pattern) > 200 {
		pattern = pattern[:200]
	}
	return pattern
}

// FormatPlaybookHint formats a discovered playbook into a high-value prompt hint.
func FormatPlaybookHint(pb *store.Playbook) string {
	if pb == nil || pb.Solution == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("💡 [SELF-HEALING PLAYBOOK #%s] (solved %d×):\n", pb.ID, pb.Occurrences))
	if pb.RootCause != "" {
		sb.WriteString(fmt.Sprintf("  • Root Cause: %s\n", pb.RootCause))
	}
	sb.WriteString(fmt.Sprintf("  • Proven Fix: %s\n", pb.Solution))
	return strings.TrimSpace(sb.String())
}
