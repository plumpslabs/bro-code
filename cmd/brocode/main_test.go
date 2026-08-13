package main

import (
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/provider"
)

func TestIsEngineReminder(t *testing.T) {
	cases := map[string]bool{
		"⚠️ You have been calling tools for many rounds": true,
		"⚠️ [LOOP GUARD]: You are repeating tool call":   true,
		"Level 1 verification check failed:":             true,
		"halo bro, tolong fix bug ini":                   false,
		"ok bro lanjutkan":                               false,
	}
	for in, want := range cases {
		if got := isEngineReminder(in); got != want {
			t.Errorf("isEngineReminder(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestToolCallSummary(t *testing.T) {
	got := toolCallSummary([]provider.ToolCall{
		{Name: "glob", Arguments: `{"pattern":"/ConversationService*"}`},
		{Name: "grep", Arguments: `{"pattern":"filter"}`},
	})
	if got != "glob → grep" {
		t.Errorf("toolCallSummary = %q, want %q", got, "glob → grep")
	}
	// Must NOT contain raw JSON/arguments.
	if strings.Contains(got, `{"pattern`) {
		t.Errorf("toolCallSummary leaked raw arguments: %q", got)
	}
}
