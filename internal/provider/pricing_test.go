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
		// DeepSeek V4 (official 2026 list rates).
		{"deepseek-v4-flash", 1_000_000, 1_000_000, 0.14 + 0.28},
		{"deepseek-v4-pro", 1_000_000, 0, 1.74},
		// 2026 Claude / OpenAI list rates.
		{"claude-sonnet-5", 1_000_000, 0, 2.00},
		{"claude-opus-5", 1_000_000, 0, 5.00},
		{"gpt-5", 1_000_000, 0, 1.25},
		{"gpt-5-mini", 0, 1_000_000, 2.00},
		// Unknown / free models cost $0 (never block a run).
		{"some-new-model", 1_000_000, 1_000_000, 0},
		{"free", 1_000_000, 1_000_000, 0},
		// BroCode free-gateway model is free even though the base
		// deepseek-v4-flash model is now a PAID DeepSeek API rate.
		{"deepseek-v4-flash-free", 1_000_000, 1_000_000, 0},
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
	// A deepseek-v4-flash variant with an extra suffix resolves to the paid
	// flash rate (longest-prefix wins over the "deepseek" family default).
	if got := EstimateCostUSD("deepseek-v4-flash-1m", 1_000_000, 0); math.Abs(got-0.14) > 1e-6 {
		t.Fatalf("expected deepseek-v4-flash price for suffixed model, got %.4f", got)
	}
}

func TestEstimateCostUSDWithCache(t *testing.T) {
	close := func(got, want float64) bool { return math.Abs(got-want) < 1e-6 }
	cases := []struct {
		name           string
		model          string
		in, cache, out int
		want           float64
	}{
		{
			name:  "Claude all-cached input billed at 10%",
			model: "claude-sonnet-5", in: 1_000_000, cache: 1_000_000, out: 0,
			want: 2.00 * 0.10,
		},
		{
			name:  "GPT-5 cached input at 10%",
			model: "gpt-5", in: 1_000_000, cache: 1_000_000, out: 0,
			want: 1.25 * 0.10,
		},
		{
			name:  "DeepSeek half-cached input at 26%",
			model: "deepseek-v4-flash", in: 1_000_000, cache: 500_000, out: 0,
			want: 500_000.0/1e6*0.14 + 500_000.0/1e6*0.14*0.26,
		},
		{
			name:  "No cache ratio for unknown family -> full price",
			model: "qwen2.5", in: 1_000_000, cache: 1_000_000, out: 0,
			want: 0.40,
		},
		{
			name:  "cacheHit > input clamps to full price",
			model: "claude-sonnet-5", in: 100, cache: 999_999, out: 0,
			want: 100.0 / 1e6 * 2.00,
		},
		{
			name:  "Free gateway model stays $0 even with cache",
			model: "deepseek-v4-flash-free", in: 1_000_000, cache: 1_000_000, out: 1_000_000,
			want: 0,
		},
	}
	for _, c := range cases {
		if got := EstimateCostUSDWithCache(c.model, c.in, c.cache, c.out); !close(got, c.want) {
			t.Errorf("%s: EstimateCostUSDWithCache(%q, %d, %d, %d) = %.6f, want %.6f", c.name, c.model, c.in, c.cache, c.out, got, c.want)
		}
	}
	// The no-cache wrapper is unchanged from the base table.
	if got := EstimateCostUSD("deepseek-v4-flash", 1_000_000, 1_000_000); math.Abs(got-(0.14+0.28)) > 1e-6 {
		t.Errorf("EstimateCostUSD wrapper regression: got %.6f", got)
	}
}
