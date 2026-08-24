package provider

import (
	"strings"
	"sync"
)

// defaultPrices holds best-effort list prices in USD per MILLION tokens
// (input, output). Keys are model IDs or family prefixes; the longest prefix
// that matches the model wins. Zero entries mean the model is free/local
// (BroCode free gateway, Ollama) or unknown (priced as $0 rather
// than blocking the run). Prices are the vendor list rates as of 2026-08;
// real billing depends on the provider's current rates, prompt caching, and
// usage tiers.
var defaultPrices = map[string][2]float64{
	// Anthropic Claude (official list, 2026-08)
	"claude-opus-4":     {5, 25},
	"claude-opus-4-1":   {5, 25},
	"claude-opus-5":     {5, 25},
	"claude-sonnet-4-5": {3, 15},
	"claude-sonnet-5":   {2, 10},
	"claude-sonnet-4":   {3, 15},
	"claude-3-7-sonnet": {3, 15},
	"claude-3-5-sonnet": {3, 15},
	"claude-3-5-haiku":  {0.80, 4},
	"claude-3-haiku":    {0.25, 1.25},
	"claude-3-sonnet":   {3, 15},
	"claude-haiku-4-5":  {1, 5},
	"claude-fable-5":    {10, 50},
	"claude":            {3, 15},
	// OpenAI (official list, 2026-08)
	"gpt-5-nano":   {0.05, 0.40},
	"gpt-5-mini":   {0.25, 2},
	"gpt-5":        {1.25, 10},
	"gpt-4o-mini":  {0.15, 0.60},
	"gpt-4o":       {2.50, 10},
	"gpt-4.1-nano": {0.10, 0.40},
	"gpt-4.1-mini": {0.40, 1.60},
	"gpt-4.1":      {2, 8},
	"gpt-4":        {30, 60}, // legacy gpt-4-turbo-era list rate
	"o4-mini":      {1.10, 4.40},
	"o3-mini":      {1.10, 4.40},
	"o3":           {2, 8},
	"o1-mini":      {1.10, 4.40},
	"o1":           {15, 60},
	// DeepSeek (official list, 2026-08)
	"deepseek-v4-pro":   {1.74, 3.48},
	"deepseek-v4-flash": {0.14, 0.28},
	"deepseek-v4":       {0.14, 0.28},
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
	// Mistral / Codestral
	"codestral":     {0.20, 0.60},
	"mistral-large": {2.00, 6.00},
	"mistral-small": {0.20, 0.60},
	"ministral":     {0.10, 0.10},
	"mistral":       {0.20, 0.60},
	// Cohere
	"command-r-plus": {2.50, 10.00},
	"command-r":      {0.50, 1.50},
	// Qwen (via Ollama / local)
	"qwen2.5": {0.40, 1.60},
	"qwen":    {0.40, 1.60},
	// Poolside / Laguna
	"poolside/laguna": {1.00, 2.00},
	"poolside-coder":  {0.50, 1.50},
	"laguna-s-2.1":    {0.50, 1.50},
	"laguna-xs-2.1":   {0.20, 0.60},
	// OpenRouter reference (deepseek-r1)
	"deepseek/deepseek-r1": {0.55, 2.19},
	// BroCode free gateway models → $0 (exact keys so they beat the
	// deepseek-v4-flash family prefix, which is the PAID DeepSeek API rate).
	"deepseek-v4-flash-free":      {0, 0},
	"hy3-free":                    {0, 0},
	"mimo-v2.5-free":              {0, 0},
	"laguna-s-2.1-free":           {0, 0},
	"ling-3.0-tiny-free":          {0, 0},
	"longcat-2.0-free":            {0, 0},
	"nemotron-3-ultra-free":       {0, 0},
	"nemotron-3.5-lightning-free": {0, 0},
	"big-pickle":                  {0, 0},
	// Free / local / unknown → $0
	"free": {0, 0},
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

// EstimateCostUSDWithCache prices a completion accounting for prompt-cache
// hits: cacheHitTokens of the input are billed at the family's cache-hit rate
// (cacheHitRatioFor) and the remainder at the full input rate. It falls back
// to the full-input pricing when cacheHitTokens is invalid or no cache ratio
// applies. Returns 0 for free/unknown models.
func EstimateCostUSDWithCache(model string, inputTokens, cacheHitTokens, outputTokens int) float64 {
	p, ok := priceFor(model)
	if !ok {
		return 0
	}
	ratio := cacheHitRatioFor(model)
	if cacheHitTokens <= 0 || cacheHitTokens > inputTokens || ratio <= 0 {
		return (float64(inputTokens)/1e6)*p[0] + (float64(outputTokens)/1e6)*p[1]
	}
	miss := inputTokens - cacheHitTokens
	return (float64(miss)/1e6)*p[0] + (float64(cacheHitTokens)/1e6)*p[0]*ratio + (float64(outputTokens)/1e6)*p[1]
}

// EstimateCostUSD returns an estimated USD cost for a completion using the
// model's list price with no cache discount. Returns 0 for free/unknown models
// (unlimited budget for local and free-gateway providers).
func EstimateCostUSD(model string, inputTokens, outputTokens int) float64 {
	return EstimateCostUSDWithCache(model, inputTokens, 0, outputTokens)
}

// cacheHitRatio maps a model family prefix to the fraction of the input price
// charged for cache-hit (cached/read) input tokens. Research-backed 2026:
//   - claude:   10% of input (all models)
//   - gpt-5:    10% of input (cached $0.125 vs $1.25)
//   - deepseek: ~26% of input (V3/V4 cache hit $0.07 vs miss $0.27)
//   - gemini:   25% of input (all 2.x models)
//
// Longest matching prefix wins. A value ≤0 (or no match) means no cache
// discount is applied.
var cacheHitRatio = map[string]float64{
	"claude":    0.10,
	"gpt-5":     0.10,
	"deepseek-": 0.26,
	"gemini-":   0.25,
}

// cacheHitRatioOverrides holds config-registered ratios; they win over the
// built-in table for the exact model key.
var (
	cacheRatioMu           sync.RWMutex
	cacheHitRatioOverrides = map[string]float64{}
)

// RegisterCacheHitRatio overrides the cache-hit price ratio for a model
// (fraction of the input price billed for cached tokens). A ratio ≤ 0 removes
// the override and falls back to the built-in table.
func RegisterCacheHitRatio(model string, ratio float64) {
	if strings.TrimSpace(model) == "" {
		return
	}
	cacheRatioMu.Lock()
	defer cacheRatioMu.Unlock()
	if ratio > 0 {
		cacheHitRatioOverrides[model] = ratio
	} else {
		delete(cacheHitRatioOverrides, model)
	}
}

// cacheHitRatioFor resolves the cache-hit price ratio for a model by exact
// override first, then the longest matching family prefix.
func cacheHitRatioFor(model string) float64 {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return 0
	}
	cacheRatioMu.RLock()
	if r, ok := cacheHitRatioOverrides[m]; ok {
		cacheRatioMu.RUnlock()
		return r
	}
	cacheRatioMu.RUnlock()
	best := ""
	for k := range cacheHitRatio {
		if strings.HasPrefix(m, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return 0
	}
	return cacheHitRatio[best]
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
