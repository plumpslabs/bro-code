package loop

import (
	"fmt"
	"strings"
	"sync"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tokens"
)

// ModelUsage accumulates tokens and estimated cost for one model.
type ModelUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
}

// UsageTracker accumulates per-model usage across a session.
type UsageTracker struct {
	mu     sync.Mutex
	models map[string]*ModelUsage
}

// NewUsageTracker creates an empty tracker.
func NewUsageTracker() *UsageTracker {
	return &UsageTracker{models: map[string]*ModelUsage{}}
}

// Record adds one completion's usage under a model name.
func (u *UsageTracker) Record(model string, usg provider.Usage) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if model == "" {
		model = "unknown"
	}
	mu, ok := u.models[model]
	if !ok {
		mu = &ModelUsage{}
		u.models[model] = mu
	}
	mu.PromptTokens += usg.PromptTokens
	mu.CompletionTokens += usg.CompletionTokens
	mu.TotalTokens += usg.TotalTokens
	// Route through the shared provider cost function so /cost always matches
	// the per-turn budget math (same table + prompt-cache discount).
	mu.CostUSD += provider.EstimateCostUSDWithCache(model, usg.PromptTokens, usg.PromptCacheHitTokens, usg.CompletionTokens)
}

// TotalCost returns the summed estimated cost across all models.
func (u *UsageTracker) TotalCost() float64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	var total float64
	for _, m := range u.models {
		total += m.CostUSD
	}
	return total
}

// TotalTokens returns the summed token usage.
func (u *UsageTracker) TotalTokens() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	var total int
	for _, m := range u.models {
		total += m.TotalTokens
	}
	return total
}

// Summary renders a compact multi-line cost report (for /cost).
func (u *UsageTracker) Summary() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.models) == 0 {
		return "No LLM usage recorded yet this session."
	}
	var totalTokens int
	var totalCost float64
	var sb strings.Builder
	estimated := false
	for model, m := range u.models {
		totalTokens += m.TotalTokens
		totalCost += m.CostUSD
		fmt.Fprintf(&sb, "%s: %s tokens (in %s / out %s) — $%.4f\n",
			model, fmtTokens(m.TotalTokens), fmtTokens(m.PromptTokens), fmtTokens(m.CompletionTokens), m.CostUSD)
		if method := tokens.CountMethod(model); strings.Contains(method, "estimate") || strings.Contains(method, "approximation") {
			estimated = true
		}
	}
	fmt.Fprintf(&sb, "\nTOTAL: %s tokens — $%.4f", fmtTokens(totalTokens), totalCost)
	if estimated {
		// Honest labeling (PHILOSOPHY Principle 3): raw usage is the provider's
		// true token counts, but token estimates (Claude p50k, DeepSeek ratios,
		// generic heuristics) are approximations that do NOT exactly match the
		// provider's bill. Costs derive from these same figures.
		sb.WriteString("\n\nNote: token figures for non-OpenAI models are local estimates; actual billing may differ.")
	}
	return strings.TrimSpace(sb.String())
}

// fmtTokens renders a token count as a compact human-readable string.
func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// TelemetryAdvisor analyzes session usage patterns and returns optimization suggestions (Fase 5.2).
func (u *UsageTracker) TelemetryAdvisor() string {
	u.mu.Lock()
	defer u.mu.Unlock()

	totalTokens := 0
	for _, m := range u.models {
		totalTokens += m.TotalTokens
	}

	var suggestions []string
	if totalTokens > 100_000 {
		suggestions = append(suggestions, "💡 High token usage detected: consider using /compact or specifying narrower search paths.")
	}
	if len(u.models) > 1 {
		suggestions = append(suggestions, "ℹ️ Multiple models active: fallback routing engaged successfully.")
	}
	if len(suggestions) == 0 {
		return "✅ BroCode session operating efficiently. Token consumption is within optimal bounds."
	}
	return strings.Join(suggestions, "\n")
}
