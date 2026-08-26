package provider

import (
	"context"
	"regexp"
	"strings"
)

// ansiEscapeRe matches ANSI escape sequences (colors, cursor moves). It is
// shared with ask.go for parsing clarification blocks out of model output.
var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// OpenCodeFreeModels is the static list of OpenCode free-tier models BroCode
// surfaces through model discovery. Used as a FALLBACK when the live /models
// fetch fails. The canonical source of truth is the gateway itself — this list
// is kept in sync manually but the live fetch supersedes it at runtime.
var OpenCodeFreeModels = []string{
	"deepseek-v4-flash-free",
	"hy3-free",
	"mimo-v2.5-free",
	"laguna-s-2.1-free",
	"ling-3.0-tiny-free",
	"longcat-2.0-free",
	"nemotron-3-ultra-free",
	"nemotron-3.5-lightning-free",
	"big-pickle",
}

// FilterOpenCodeFreeModels filters a raw model list from the OpenCode /models
// endpoint down to only free-tier models. The gateway returns ALL models
// (paid + free), so we must filter. Two heuristics:
//  1. Models ending in "-free" are always free-tier.
//  2. Models in the hardcoded OpenCodeFreeModels list are known free-tier.
// This lets new free models appear automatically when the gateway adds them,
// while excluding paid models like claude-opus-5 or gpt-5.6-sol.
func FilterOpenCodeFreeModels(all []string) []string {
	knownFree := make(map[string]bool, len(OpenCodeFreeModels))
	for _, m := range OpenCodeFreeModels {
		knownFree[m] = true
	}
	var out []string
	for _, m := range all {
		if knownFree[m] || strings.HasSuffix(m, "-free") {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return OpenCodeFreeModels // degraded: return hardcoded fallback
	}
	return out
}

// OpenCodeAdapter routes completion requests to the OpenCode free-model
// gateway (an OpenAI-compatible HTTP endpoint). It is intentionally free of any
// dependency on the opencode CLI binary: no subprocess is spawned and no
// output is scraped. BroCode's own engine controls the agent loop, system
// prompt and tools, so the gateway model receives the same native context as
// any other provider — there is no separate "gateway loop" to compensate for.
type OpenCodeAdapter struct {
	http *OpenAIAdapter
}

// NewOpenCodeAdapter creates an OpenCode provider adapter wired to the official
// free-model gateway. The endpoint is BroCode-controlled (never a
// personal/third-party URL), so no opencode installation is required.
func NewOpenCodeAdapter() *OpenCodeAdapter {
	return &OpenCodeAdapter{
		http: NewOpenAIAdapter("https://opencode.ai/zen/v1", ""),
	}
}

func (a *OpenCodeAdapter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	return a.StreamComplete(ctx, req, nil)
}

// StreamComplete forwards the request to the HTTP gateway, streaming
// content deltas to onDelta (the chat streaming handler) token by token.
func (a *OpenCodeAdapter) StreamComplete(ctx context.Context, req CompletionRequest, onDelta func(string)) (*CompletionResponse, error) {
	// Gateway model IDs carry no "opencode/"/"lalarasa/" routing prefix over
	// the HTTP API; strip any stray prefix so the request matches the
	// gateway's own model catalogue.
	model := strings.TrimPrefix(req.Model, "opencode/")
	model = strings.TrimPrefix(model, "lalarasa/")
	req.Model = model
	return a.http.StreamComplete(ctx, req, onDelta)
}

// CompleteWithProgress satisfies the ProgressingAdapter interface for compatibility.
func (a *OpenCodeAdapter) CompleteWithProgress(ctx context.Context, req CompletionRequest, onProgress func(string)) (*CompletionResponse, error) {
	return a.StreamComplete(ctx, req, onProgress)
}
