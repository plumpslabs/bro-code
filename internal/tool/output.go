package tool

import (
	"fmt"
	"regexp"
)

// maxCommandOutputChars caps how many characters of a single command's
// output are returned. Even after line-filtering, one huge line (a minified
// JSON, a `cat` of a generated file) could otherwise carry megabytes into the
// model's next turn — 1-2 tool calls would blow the context window and force
// premature compaction. 40k chars ≈ 10k tokens: generous enough that a model
// gets the full picture of a normal file read, bounded so the window is never
// burned by one call.
const maxCommandOutputChars = 40_000

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:sk-[a-zA-Z0-9]{20,})`),
	regexp.MustCompile(`(?i)(?:ghp_[a-zA-Z0-9]{20,})`),
	regexp.MustCompile(`(?i)(?:gho_[a-zA-Z0-9]{20,})`),
	regexp.MustCompile(`(?i)(?:AKIA[0-9A-Z]{16})`),
	regexp.MustCompile(`(?i)(Bearer\s+)[a-zA-Z0-9_\-\.]{25,}`),
}

// MaskSecrets scrubs raw API keys and auth tokens from tool outputs so secrets
// never leak into the model's context window.
func MaskSecrets(s string) string {
	for _, pat := range secretPatterns {
		s = pat.ReplaceAllString(s, "[REDACTED_SECRET]")
	}
	return s
}

// CapOutput truncates tool output to maxCommandOutputChars and redacts any
// accidental secrets, keeping a visible marker so the model knows more output
// exists beyond the cap.
func CapOutput(s string) string {
	s = MaskSecrets(s)
	r := []rune(s)
	if len(r) <= maxCommandOutputChars {
		return s
	}
	return string(r[:maxCommandOutputChars]) + fmt.Sprintf("\n… [output truncated, %d more chars — narrow the query or read a smaller line range]", len(r)-maxCommandOutputChars)
}
