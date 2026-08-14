package loop

import (
	"fmt"
	"strings"
	"sync"

	"github.com/plumpslabs/bro-code/internal/provider"
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
	mu.CostUSD += estimateCost(model, usg)
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
	for model, m := range u.models {
		totalTokens += m.TotalTokens
		totalCost += m.CostUSD
		sb.WriteString(fmt.Sprintf("%s: %s tokens (in %s / out %s) — $%.4f\n",
			model, fmtTokens(m.TotalTokens), fmtTokens(m.PromptTokens), fmtTokens(m.CompletionTokens), m.CostUSD))
	}
	sb.WriteString(fmt.Sprintf("\nTOTAL: %s tokens — $%.4f", fmtTokens(totalTokens), totalCost))
	return strings.TrimSpace(sb.String())
}

// pricingTable maps model name → (input $/1M, output $/1M). Research-backed
// list prices; free/unknown models are $0 (honest: we'd rather under-report
// than guess wrong).
var pricingTable = map[string][2]float64{
	// DeepSeek V3.1 / V4-era official pricing.
	"deepseek-chat":          {0.27, 1.10},
	"deepseek-coder":         {0.27, 1.10},
	"deepseek-reasoner":      {0.55, 2.19},
	"deepseek-v4-flash":      {0.27, 1.10},
	"deepseek-v4-flash-free": {0, 0},
	// Anthropic.
	"claude-3-5-sonnet-20241022": {3.00, 15.00},
	"claude-3-7-sonnet-20250219": {3.00, 15.00},
	"claude-3-5-haiku-20241022":  {0.80, 4.00},
	// OpenAI.
	"gpt-4o":      {2.50, 10.00},
	"gpt-4o-mini": {0.15, 0.60},
	"o3-mini":     {1.10, 4.40},
	// Poolside Laguna.
	"laguna-s-2.1":   {0.50, 1.50},
	"laguna-xs-2.1":  {0.20, 0.60},
	"poolside-coder": {0.50, 1.50},
	// Google.
	"gemini-2.5-flash": {0.30, 2.50},
	"gemini-2.5-pro":   {1.25, 10.00},
	"gemini-2.0-flash": {0.10, 0.40},
	// Groq (per-token, converted to per-1M).
	"llama-3.3-70b-versatile": {0.59, 0.79},
	// OpenRouter reference (deepseek-r1).
	"deepseek/deepseek-r1": {0.55, 2.19},
}

// estimateCost computes USD cost for one completion from the pricing table.
// Model IDs that are free (opencode free tier, suffix "-free") or unknown
// report $0 — no hallucinated numbers.
func estimateCost(model string, usg provider.Usage) float64 {
	if strings.Contains(model, "-free") {
		return 0
	}
	price, ok := pricingTable[model]
	if !ok {
		// Fallback: try a provider/ prefix stripped.
		if i := strings.LastIndex(model, "/"); i >= 0 {
			price, ok = pricingTable[model[i+1:]]
		}
		if !ok {
			return 0
		}
	}
	in := float64(usg.PromptTokens) / 1_000_000 * price[0]
	out := float64(usg.CompletionTokens) / 1_000_000 * price[1]
	return in + out
}

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
