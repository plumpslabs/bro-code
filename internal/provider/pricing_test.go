package provider

import (
	"math"
	"testing"
)

func TestEstimateCostUSD(t *testing.T) {
	close := func(got, want float64) bool { return math.Abs(got-want) < 1e-6 }
	cases := []struct {
		model   string
		in, out int
		want    float64
	}{
		{"gpt-4o", 1_000_000, 0, 2.50},
		{"gpt-4o-mini", 0, 1_000_000, 0.60},
		{"claude-3-5-sonnet", 1_000_000, 0, 3.00},
		{"claude-3-5-haiku", 0, 1_000_000, 4.00},
		{"deepseek-chat", 1_000_000, 1_000_000, 0.27 + 1.10},
		// Unknown / free models cost $0 (never block a run).
		{"some-new-model", 1_000_000, 1_000_000, 0},
		{"free", 1_000_000, 1_000_000, 0},
	}
	for _, c := range cases {
		if got := EstimateCostUSD(c.model, c.in, c.out); !close(got, c.want) {
			t.Errorf("EstimateCostUSD(%q, %d, %d) = %.4f, want %.4f", c.model, c.in, c.out, got, c.want)
		}
	}
}

func TestPriceForPrefixMatchAndOverride(t *testing.T) {
	// Family prefix: gpt-4o-2024-08-06 should resolve to the gpt-4o price.
	if got := EstimateCostUSD("gpt-4o-2024-08-06", 1_000_000, 0); math.Abs(got-2.50) > 1e-6 {
		t.Fatalf("expected family-prefix match for gpt-4o-2024-08-06, got %.4f", got)
	}
	// Config override wins for the exact model key.
	RegisterModelPrice("gpt-4o", 10, 40)
	defer RegisterModelPrice("gpt-4o", 0, 0) // clear override
	if got := EstimateCostUSD("gpt-4o", 1_000_000, 0); math.Abs(got-10.0) > 1e-6 {
		t.Fatalf("expected override price for gpt-4o, got %.4f", got)
	}
	// Prefix still matches the base table for non-overridden models.
	if got := EstimateCostUSD("claude-3-5-sonnet", 1_000_000, 0); math.Abs(got-3.0) > 1e-6 {
		t.Fatalf("expected table price for claude-3-5-sonnet, got %.4f", got)
	}
}
