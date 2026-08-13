package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ── Explore subagent (AGENTIC_OVERHAUL P8) ─────────────────────────────────
// The main agent can delegate focused read-only research to a NESTED agent
// loop via the native `explore` tool — the opencode explore-subagent pattern:
//
//	main agent → <tool_call>explore "map the rotation pipeline"
//	  → nested loop: model → search/read tool → result → model … (≤ maxExploreRounds)
//	  → condensed report fed back to the main agent as a [SYSTEM TOOL RESULT]
//	  → the main agent reasons over the report and continues
//
// This keeps deep multi-file investigation OUT of the main context (the
// nested loop's tool outputs never pollute it — only the report comes back),
// which is exactly the "elegance" of the opencode delegation in the audit.
// Bounded: ≤ maxExploreRounds model calls, read-only tool surface, tool
// outputs capped by the shared CapOutput / fileSearch budgets (P1).

type exploreConfig struct {
	endpoint string
	model    string
	apiKey   string            // Bearer token for providers that require auth (poolside, custom)
	headers  map[string]string // extra headers for the nested loop call
}

const (
	maxExploreRounds   = 6
	exploreCallTimeout = 120 * time.Second
)

// exploreConfigFor derives the explore config from the active provider, or nil
// when the provider does not speak the OpenAI-compatible tool protocol yet
// (the nested loop reuses the same non-streaming call shape). opencode (zen)
// needs no auth; poolside and custom OpenAI-compatible providers carry their
// key so the nested loop authenticates the same way the main loop does.
func (m Model) exploreConfigFor() *exploreConfig {
	model := m.selectedModel
	switch m.provider {
	case "opencode":
		if model == "" {
			model = openCodeFreeModels[0]
		}
		return &exploreConfig{endpoint: zenEndpoint, model: model}
	case "poolside":
		if model == "" {
			model = poolsideModels[0]
		}
		key := loadAPIKey("poolside")
		if key == "" {
			return nil // same guard as the main poolside path
		}
		return &exploreConfig{endpoint: "https://inference.poolside.ai/v1/chat/completions", model: model, apiKey: key}
	}
	// Custom OpenAI-compatible providers from config.jsonc — same call shape,
	// so the nested loop can delegate on them too.
	cfg := LoadAppConfig()
	cp, ok := cfg.Provider[m.provider]
	if !ok {
		return nil
	}
	endpoint := cp.Options.BaseURL
	if endpoint == "" {
		return nil
	}
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint = strings.TrimRight(endpoint, "/") + "/chat/completions"
	}
	return &exploreConfig{
		endpoint: endpoint,
		model:    model,
		apiKey:   cp.Options.APIKey,
		headers:  cp.Options.Headers,
	}
}

// explorerSystemPrompt is the read-only persona for the nested loop.
func explorerSystemPrompt() string {
	return "You are BroCode's Explore subagent — a fast, read-only codebase researcher.\n" +
		"DIRECTIVES:\n" +
		"1. READ-ONLY: use ONLY the `search` and `read` tools. Never edit files, never run commands.\n" +
		"2. Investigate the RESEARCH QUESTION: search first, then read the most relevant matches.\n" +
		"3. After each tool result, synthesize. When you have enough evidence, STOP and answer with a concise report.\n" +
		"4. Report format: short sections, bullet lists, file:line evidence. Reference files — do not dump their contents.\n" +
		"5. Stop as soon as the question is answerable — never re-read the same files."
}

// explorerTools returns the read-only tool surface for the nested loop
// (search + read only — no bash, no write/edit, no ask).
func explorerTools() []map[string]interface{} {
	var out []map[string]interface{}
	for _, t := range toolsPayload(false) {
		fn, _ := t["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		if name == "search" || name == "read" {
			out = append(out, t)
		}
	}
	return out
}

// runExploreLoop executes the bounded nested research loop and returns the
// condensed report, the number of model calls used, and any terminal error.
// The loop mirrors the main ReAct loop at a smaller scale: model → execute
// read-only tool blocks → feed results back → repeat until the model answers
// with text only (the report). Tool outputs stay in the NESTED history — only
// the final report returns to the caller.
func runExploreLoop(cfg *exploreConfig, question string) (string, int, error) {
	if cfg == nil || cfg.endpoint == "" || cfg.model == "" {
		return "Explore subagent unavailable for this provider yet — the main agent should search/read directly.", 0, nil
	}
	history := []map[string]string{
		{"role": "system", "content": explorerSystemPrompt()},
		{"role": "user", "content": "RESEARCH QUESTION:\n" + question +
			"\n\nInvestigate the workspace now. Use `search` then `read` the most relevant matches. " +
			"When you have enough evidence, STOP and answer with the report only (no more tool calls)."},
	}
	for round := 1; round <= maxExploreRounds; round++ {
		text, err := openaiCompatibleCallCfg(cfg, history, explorerTools())
		if err != nil {
			return "", round, err
		}
		// No tool blocks → the text IS the report.
		if len(toolBlockCommands(text)) == 0 {
			return strings.TrimSpace(text), round, nil
		}
		// Execute the read-only blocks (search/read; plannerMode=true blocks
		// any stray bash) and feed the results back into the nested history.
		_, feedback := applyAgenticToolsDeny(text, nil, true, nil)
		history = append(history,
			map[string]string{"role": "assistant", "content": text},
			map[string]string{"role": "user", "content": "[SYSTEM TOOL RESULT]\n" + feedback},
		)
	}
	return "(explore subagent reached its round limit — report truncated)", maxExploreRounds, nil
}

// openaiCompatibleCall performs one non-streaming OpenAI-compatible request
// with the given messages + tools and returns the parsed text (native tool
// calls already converted to executable blocks by parseZenResponse).
func openaiCompatibleCall(endpoint, model string, msgs []map[string]string, tools []map[string]interface{}) (string, error) {
	return openaiCompatibleCallCfg(&exploreConfig{endpoint: endpoint, model: model}, msgs, tools)
}

// openaiCompatibleCallCfg performs one non-streaming OpenAI-compatible request
// with the given messages + tools and returns the parsed text (native tool
// calls already converted to executable blocks by parseZenResponse). The
// exploreConfig carries optional auth (Bearer key + extra headers) so the
// nested loop authenticates exactly like the main provider path.
func openaiCompatibleCallCfg(cfg *exploreConfig, msgs []map[string]string, tools []map[string]interface{}) (string, error) {
	body, err := json.Marshal(map[string]interface{}{
		"model":       cfg.model,
		"messages":    msgs,
		"tools":       tools,
		"temperature": 0.4,
		"max_tokens":  2048,
		"stream":      false,
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), exploreCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	if cfg.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	}
	for k, v := range cfg.headers {
		req.Header.Set(k, v)
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("explore HTTP %d: %s", resp.StatusCode, clip(string(data), 120))
	}
	text, _, _, err := parseZenResponse(data)
	return text, err
}
