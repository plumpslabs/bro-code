package tokens

import "testing"

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
