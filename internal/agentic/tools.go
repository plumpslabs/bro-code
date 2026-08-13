package agentic

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ToolOptions defines limits for tool execution.
type ToolOptions struct {
	Timeout time.Duration
}

// maxCommandOutputChars caps how many characters of a single command's
// output are returned. Even after line-filtering, one huge line (a minified
// JSON, a `cat` of a generated file) could otherwise carry megabytes into the
// model's next turn — 1-2 tool calls would blow the context window and force
// premature compaction. 40k chars ≈ 10k tokens: generous enough that a
// free model gets the full picture of a normal file read (quality-first for
// weaker models — truncating mid-file makes them reason over partial
// evidence), bounded so the window is still never burned by one call.
const maxCommandOutputChars = 40_000

// CapOutput truncates tool output (command stdout or a read file's content)
// to maxCommandOutputChars, keeping a visible marker so the model knows more
// output exists beyond the cap. Exported so the TUI's read-tool path applies
// the same budget as the shell-runner path.
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

// RunCommandNative runs a shell command with a timeout.
func RunCommandNative(cmdString string, opts ToolOptions) (string, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", cmdString)

	// Set up the generic output filters (Truncate to 1000 lines, Dedup)
	filters := NewFilterChain(
		&DedupFilter{},
		&TruncateFilter{MaxLines: 1000},
	)

	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("command timed out after %v", timeout)
	}

	// Stream the captured output through the filters
	var filteredSb strings.Builder
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if processed := filters.ProcessLine(line); processed != "" {
			filteredSb.WriteString(processed + "\n")
		}
	}
	if flushed := filters.Flush(); flushed != "" {
		filteredSb.WriteString(flushed + "\n")
	}
	if exitMsg := filters.OnExit(cmd.ProcessState.ExitCode()); exitMsg != "" {
		filteredSb.WriteString(exitMsg + "\n")
	}

	// Cap the final output by characters too — line filtering alone lets a
	// single megabyte-wide line through untouched.
	return CapOutput(strings.TrimSpace(filteredSb.String())), err
}
