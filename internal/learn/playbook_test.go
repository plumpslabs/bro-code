package learn

import (
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/store"
)

func TestExtractErrorPatternAndFormatHint(t *testing.T) {
	rawErr := `TypeError: Cannot read properties of undefined (reading 'sendMessage')
    at WhatsappService.send (/app/src/services/whatsapp.js:45:12)
    at runMicrotasks (<anonymous>)`

	pattern := ExtractErrorPattern(rawErr)
	if !strings.Contains(pattern, "TypeError") || !strings.Contains(pattern, "sendMessage") {
		t.Errorf("expected pattern to contain error type and symbol, got: %s", pattern)
	}
	if strings.Contains(pattern, "at WhatsappService.send") {
		t.Errorf("pattern should strip stacktrace lines, got: %s", pattern)
	}

	pb := &store.Playbook{
		ID:          "pb_12345",
		Pattern:     pattern,
		RootCause:   "Client instance is undefined when disconnected",
		Solution:    "Verify client connection state before calling sendMessage",
		Occurrences: 3,
	}

	hint := FormatPlaybookHint(pb)
	if !strings.Contains(hint, "SELF-HEALING PLAYBOOK") || !strings.Contains(hint, "solved 3×") {
		t.Errorf("expected formatted hint to mention playbook and occurrences, got:\n%s", hint)
	}
	if !strings.Contains(hint, "Verify client connection state") {
		t.Errorf("expected hint to contain solution, got:\n%s", hint)
	}
}
