// Package tokens provides LLM token counting.
//
// It prefers an exact BPE tokenizer (tiktoken-go with bundled offline
// vocabularies, so NO runtime network is required) for the common encodings
// (cl100k_base for GPT-3.5/4 legacy, o200k_base for GPT-4o/o1/GPT-5,
// p50k_base for Claude — Anthropic ships no offline tokenizer, so p50k_base is
// the documented community approximation). When the real tokenizer cannot
// initialize (unsupported model, missing vocab, sandbox), EstimateTokens
// provides a deterministic char-count heuristic fallback so a tokenizer
// failure never breaks context-budget decisions.
package tokens

import (
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// modelEncoding maps a model name to its tiktoken encoding name. Order matters:
// more-specific entries (full names) are checked before family prefixes.
var modelEncoding = map[string]string{
	"gpt-4o":        tiktoken.MODEL_O200K_BASE,
	"gpt-4o-mini":   tiktoken.MODEL_O200K_BASE,
	"o1":            tiktoken.MODEL_O200K_BASE,
	"o3":            tiktoken.MODEL_O200K_BASE,
	"o4":            tiktoken.MODEL_O200K_BASE,
	"gpt-4.5":       tiktoken.MODEL_O200K_BASE,
	"gpt-4.1":       tiktoken.MODEL_O200K_BASE,
	"gpt-5":         tiktoken.MODEL_O200K_BASE,
	"gpt-3.5-turbo": tiktoken.MODEL_CL100K_BASE,
	"gpt-4":         tiktoken.MODEL_CL100K_BASE,
	// Claude family: no public BPE vocabulary. p50k_base is the closest
	// documented offline approximation (community standard for counting
	// Claude tokens locally). Note: Claude 4.7+ uses a tokenizer Anthropic
	// reports produces ~30% more tokens — so the p50k estimate is low by
	// ~25-30% for the newest Claude models. The only exact count is the
	// count_tokens API (not billed).
	"claude-": tiktoken.MODEL_P50K_BASE,
}

const (
	modelO200KBase = "o200k_base"
)

// encodingForModel resolves a model name to a tiktoken encoding name, preferring
// the bundled tiktoken-go model table and falling back to prefix matching so
// suffixed model variants (e.g. "gpt-4o-2024-11-20") resolve correctly.
func encodingForModel(model string) string {
	if model == "" {
		return tiktoken.MODEL_CL100K_BASE
	}
	if enc, ok := tiktoken.MODEL_TO_ENCODING[model]; ok {
		return enc
	}
	if enc, ok := modelEncoding[model]; ok {
		return enc
	}
	for prefix, enc := range modelEncoding {
		if len(model) >= len(prefix) && model[:len(prefix)] == prefix {
			return enc
		}
	}
	// Prefix family fallbacks.
	if len(model) >= 4 && model[:4] == "gpt-" && len(model) > 6 && model[5:7] == "4o" {
		return modelO200KBase
	}
	return tiktoken.MODEL_CL100K_BASE
}

// cache holds lazily-built *Tiktoken encoders keyed by encoding name. The
// offline loader is installed once; encoders are built on demand and reused.
var (
	cache   = map[string]*tiktoken.Tiktoken{}
	failed  = map[string]bool{}
	once    sync.Once
	cacheMu sync.Mutex
)

func loadOfflineOnce() {
	once.Do(func() {
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	})
}

// realEncoding returns the tiktoken encoder for the given model's encoding.
// Returns ok=false on any failure so the caller falls back to the heuristic.
func realEncoding(model string) (*tiktoken.Tiktoken, bool) {
	enc := encodingForModel(model)

	cacheMu.Lock()
	if t, ok := cache[enc]; ok {
		cacheMu.Unlock()
		return t, true
	}
	if failed[enc] {
		cacheMu.Unlock()
		return nil, false
	}
	cacheMu.Unlock()

	loadOfflineOnce()
	tk, err := tiktoken.GetEncoding(enc)
	if err != nil || tk == nil {
		cacheMu.Lock()
		failed[enc] = true
		cacheMu.Unlock()
		return nil, false
	}
	cacheMu.Lock()
	cache[enc] = tk
	cacheMu.Unlock()
	return tk, true
}

// CountTokens returns the exact BPE token count for text using the encoding
// appropriate to model. Falls back to the model-aware heuristic
// EstimateTokensForModel when the real tokenizer is unavailable — token
// counting must never fail.
func CountTokens(text, model string) int {
	if text == "" {
		return 0
	}
	tk, ok := realEncoding(model)
	if !ok {
		return EstimateTokensForModel(text, model)
	}
	return len(tk.Encode(text, nil, nil))
}

// CountTokensDefault is the model-agnostic convenience: uses the broadest
// default encoding and falls back to the heuristic on any error.
func CountTokensDefault(text string) int {
	return CountTokens(text, "")
}

// CountMethod describes how CountTokens counts for a model, so the UI can
// label estimates honestly instead of presenting every number as exact
// (PHILOSOPHY Principle 3 — forecast vs settlement must be distinguishable).
func CountMethod(model string) string {
	if model == "" {
		return "estimate (generic heuristic)"
	}
	if strings.HasPrefix(model, "deepseek-") || strings.HasPrefix(model, "deepseek/") {
		return "estimate (DeepSeek official char→token ratios)"
	}
	enc := encodingForModel(model)
	switch enc {
	case tiktoken.MODEL_O200K_BASE, tiktoken.MODEL_CL100K_BASE:
		if _, ok := realEncoding(model); ok {
			return "exact BPE (" + enc + ")"
		}
		return "estimate (tokenizer unavailable, generic heuristic)"
	case tiktoken.MODEL_P50K_BASE:
		// Anthropic ships no offline BPE vocabulary; p50k_base is the documented
		// community approximation and undercounts Claude 4.7+ by ~25-30%.
		return "approximation (p50k_base — Claude, ±25-30% on newest models)"
	}
	return "estimate (generic heuristic)"
}
