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
}
