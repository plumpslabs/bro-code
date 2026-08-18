package tokens

import (
	"strings"
	"testing"
)

func TestTiktokenOfflineSmoke(t *testing.T) {
	got := CountTokens("Hello world, this is a test of the BPE tokenizer for BroCode.", "gpt-4o")
	if got <= 0 {
		t.Fatalf("expected positive token count, got %d", got)
	}
	t.Logf("gpt-4o tokens=%d", got)
	// heuristic fallback path for empty/unknown
	if CountTokens("", "gpt-4o") != 0 {
		t.Fatal("empty text must be 0")
	}
	// default falls back to cl100k
	if CountTokens("halo dunia", "") <= 0 {
		t.Fatal("default count should be positive")
	}
	// Claude resolves offline via the p50k_base approximation (no network).
	claude := CountTokens("The quick brown fox jumps over the lazy dog.", "claude-sonnet-4-5")
	if claude <= 0 {
		t.Fatal("claude count should be positive")
	}
	t.Logf("claude-sonnet-4-5 tokens=%d", claude)
	// GPT-5 resolves via o200k_base.
	if c := CountTokens("The quick brown fox jumps over the lazy dog.", "gpt-5"); c <= 0 {
		t.Fatal("gpt-5 count should be positive")
	}
}

// TestCountMethodLabels verifies the accuracy label distinguishes exact BPE
// from the Claude approximation and the DeepSeek heuristic — so the UI can
// present forecast-vs-exact honestly instead of every number looking exact.
func TestCountMethodLabels(t *testing.T) {
	if got := CountMethod("gpt-4o"); !strings.HasPrefix(got, "exact BPE") {
		t.Errorf("gpt-4o should be exact BPE, got %q", got)
	}
	if got := CountMethod("claude-sonnet-4-5"); !strings.Contains(got, "p50k_base") || !strings.Contains(got, "±25-30%") {
		t.Errorf("claude should be labeled as an approximation, got %q", got)
	}
	if got := CountMethod("deepseek-v4-flash"); !strings.Contains(got, "DeepSeek") {
		t.Errorf("deepseek should use the family heuristic, got %q", got)
	}
	if got := CountMethod(""); !strings.Contains(got, "estimate") {
		t.Errorf("unknown/empty model should be labeled as an estimate, got %q", got)
	}
}
