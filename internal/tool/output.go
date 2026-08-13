package tool

import "fmt"

// maxCommandOutputChars caps how many characters of a single command's
// output are returned. Even after line-filtering, one huge line (a minified
// JSON, a `cat` of a generated file) could otherwise carry megabytes into the
// model's next turn — 1-2 tool calls would blow the context window and force
// premature compaction. 40k chars ≈ 10k tokens: generous enough that a model
// gets the full picture of a normal file read, bounded so the window is never
// burned by one call.
const maxCommandOutputChars = 40_000

// CapOutput truncates tool output to maxCommandOutputChars, keeping a visible
// marker so the model knows more output exists beyond the cap.
//
// The slice is rune-safe: cutting at a byte boundary could split a multi-byte
// UTF-8 rune (CJK comments, emoji) and leave an invalid tail in the model's
// context — so the cap lands on a rune boundary via a clipped []rune slice.
func CapOutput(s string) string {
	r := []rune(s)
	if len(r) <= maxCommandOutputChars {
		return s
	}
	return string(r[:maxCommandOutputChars]) + fmt.Sprintf("\n… [output truncated, %d more chars]", len(r)-maxCommandOutputChars)
}
