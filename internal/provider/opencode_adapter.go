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
// surfaces through model discovery. BroCode is fully standalone: it talks to
// the OpenCode OpenAI-compatible gateway over HTTP and never spawns the
// opencode CLI binary — model availability comes from this list plus the
// provider registry, not by shelling out to any external process.
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
