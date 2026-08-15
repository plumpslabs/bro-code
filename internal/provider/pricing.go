package provider

import (
	"strings"
	"sync"
)

// defaultPrices holds best-effort list prices in USD per MILLION tokens
// (input, output). Keys are model IDs or family prefixes; the longest prefix
// that matches the model wins. Zero entries mean the model is free/local
// (BroCode free gateway, FreeBuff, Ollama) or unknown (priced as $0 rather
// than blocking the run). These are estimates — real billing depends on the
// provider's current rates, prompt caching, and usage tiers.
var defaultPrices = map[string][2]float64{
	// Anthropic Claude
	"claude-opus-4":     {15, 75},
	"claude-opus-4-1":   {5, 25},
	"claude-sonnet-4-5": {3, 15},
	"claude-sonnet-4":   {3, 15},
	"claude-3-7-sonnet": {3, 15},
	"claude-3-5-sonnet": {3, 15},
	"claude-3-5-haiku":  {0.80, 4},
	"claude-3-haiku":    {0.25, 1.25},
	"claude-3-sonnet":   {3, 15},
	"claude":            {3, 15},
	// OpenAI
	"gpt-4o-mini": {0.15, 0.60},
	"gpt-4o":      {2.50, 10},
	"gpt-4":       {30, 60},
	"o3-mini":     {1.10, 4.40},
	"o1-mini":     {1.10, 4.40},
	"o1":          {15, 60},
	// DeepSeek
	"deepseek-reasoner": {0.55, 2.19},
	"deepseek-chat":     {0.27, 1.10},
	"deepseek-coder":    {0.14, 0.28},
	"deepseek-r1":       {0.55, 2.19},
	"deepseek-v3":       {0.27, 1.10},
	"deepseek":          {0.27, 1.10},
	// Google Gemini
	"gemini-2.5-flash": {0.30, 2.50},
	"gemini-2.5-pro":   {1.25, 10},
	"gemini-2.0-flash": {0.10, 0.40},
	"gemini":           {1.25, 10},
	// Meta Llama (via Groq/OpenRouter/Ollama)
	"llama-3.3-70b": {0.59, 0.79},
	"llama-3.1-8b":  {0.05, 0.08},
	"llama-3.2":     {0.18, 0.20},
	"llama":         {0.59, 0.79},
	// Qwen (via Ollama / local)
	"qwen2.5": {0.40, 1.60},
	"qwen":    {0.40, 1.60},
	// Poolside / Laguna
	"poolside/laguna": {1.00, 2.00},
	// Free / local / unknown → $0
	"free":              {0, 0},
	"deepseek-v4-flash": {0, 0},
}

// priceOverrides holds config-registered prices (from applyConfigPrices);
// they win over the built-in table for the exact model key.
var (
	priceMu        sync.RWMutex
	priceOverrides = map[string][2]float64{}
)

// RegisterModelPrice overrides the price for a model (USD per million input
// and output tokens). Values ≤0 fall back to the built-in table.
func RegisterModelPrice(model string, inputPrice, outputPrice float64) {
	if strings.TrimSpace(model) == "" {
		return
	}
	priceMu.Lock()
	defer priceMu.Unlock()
	if inputPrice > 0 || outputPrice > 0 {
		priceOverrides[model] = [2]float64{inputPrice, outputPrice}
	} else {
		delete(priceOverrides, model)
	}
}

// EstimateCostUSD returns an estimated USD cost for a completion using the
// model's list price. Returns 0 for free/unknown models (unlimited budget for
// local and free-gateway providers).
func EstimateCostUSD(model string, inputTokens, outputTokens int) float64 {
	p, ok := priceFor(model)
	if !ok {
		return 0
	}
	return (float64(inputTokens)/1e6)*p[0] + (float64(outputTokens)/1e6)*p[1]
}

// priceFor resolves the (input, output) USD-per-million-token pair for a model
// by exact match first (config overrides beat the table), then the longest
// matching family prefix.
func priceFor(model string) ([2]float64, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return [2]float64{}, false
	}
	priceMu.RLock()
	if p, ok := priceOverrides[m]; ok {
		priceMu.RUnlock()
		return p, true
	}
	priceMu.RUnlock()
	if p, ok := defaultPrices[m]; ok {
		return p, true
	}
	best := ""
	for k := range defaultPrices {
		if strings.HasPrefix(m, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return [2]float64{}, false
	}
	return defaultPrices[best], true
}
