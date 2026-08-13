package agentic

import (
	"strings"
	"testing"
	"time"
)

// TestCapOutputBoundsToolFeedback pins the character budget on tool output:
// a single command or read that dumps a huge amount of text must come back
// bounded, with a visible marker, so the model knows more exists without the
// context window being burned by one call.
func TestCapOutputBoundsToolFeedback(t *testing.T) {
	short := "hello world"
	if got := CapOutput(short); got != short {
		t.Fatalf("short output must pass through unchanged, got %q", got)
	}

	big := strings.Repeat("x", maxCommandOutputChars+5000)
	got := CapOutput(big)
	if len(got) > maxCommandOutputChars+80 {
		t.Fatalf("output not capped: %d chars", len(got))
	}
	if !strings.Contains(got, "[output truncated") {
		t.Fatalf("expected truncation marker, got tail: %q", got[len(got)-60:])
	}
}

// TestRunCommandNativeCapsOutput verifies the shell runner itself never
// returns more than the character budget even when the command spews output
// faster than the line filter can bound it (e.g. one huge minified line).
func TestRunCommandNativeCapsOutput(t *testing.T) {
	out, err := RunCommandNative("printf '%*s' 50000 '' | tr ' ' x", ToolOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) > maxCommandOutputChars+80 {
		t.Fatalf("command output not capped: %d chars", len(out))
	}
}
