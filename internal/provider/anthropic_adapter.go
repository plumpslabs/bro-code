package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// AnthropicAdapter implements ProviderAdapter for Anthropic API.
type AnthropicAdapter struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

// NewAnthropicAdapter creates a new Anthropic provider adapter.
func NewAnthropicAdapter(baseURL, apiKey string) *AnthropicAdapter {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &AnthropicAdapter{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Client:  NewStreamingHTTPClient(),
	}
}

// anthropicCacheControl marks a prompt segment as a cache breakpoint
// (Anthropic prompt caching, ephemeral TTL). Placed on the last system block,
// the last tool, and the last block of the first message, it caches the whole
// stable prefix (system + tools + first turn) so every subsequent loop round
// re-sends only the delta — the biggest cost lever in a multi-round agentic
// turn. Ignored by providers without support; disable via
// BROCODE_NO_PROMPT_CACHE=1 for strict gateways.
type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicContentBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	Thinking     string                 `json:"thinking,omitempty"`
	Signature    string                 `json:"signature,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        json.RawMessage        `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      string                 `json:"content,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicTool struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	InputSchema  map[string]any         `json:"input_schema"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	// System is either a plain string (legacy, cache disabled) or an array of
	// text blocks with cache_control (prompt caching enabled).
	System json.RawMessage `json:"system,omitempty"`
}

type anthropicResponse struct {
	Content []anthropicContentBlock `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	StopReason string `json:"stop_reason"`
}

// promptCacheEnabled reports whether Anthropic prompt-caching breakpoints
// should be emitted. On by default; BROCODE_NO_PROMPT_CACHE=1 disables it for
// strict gateways that reject unknown fields. The breakpoints are also safe
// for Anthropic's own API below the minimum cacheable length (1024 tokens) —
// breakpoints below the minimum are simply ignored, never an error.
func promptCacheEnabled() bool {
	v := strings.ToLower(os.Getenv("BROCODE_NO_PROMPT_CACHE"))
	return v != "1" && v != "true" && v != "yes"
}

func (a *AnthropicAdapter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	apiReq := anthropicRequest{
		Model:     req.Model,
		MaxTokens: 4096,
	}
	cache := promptCacheEnabled()

	var systemPrompt strings.Builder
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemPrompt.WriteString(msg.Content);systemPrompt.WriteString("\n")
			continue
		}

		aMsg := anthropicMessage{Role: msg.Role}
		if msg.Role == "user" && msg.ToolCallID != "" {
			aMsg.Content = append(aMsg.Content, anthropicContentBlock{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   msg.Content,
			})
		} else {
			if msg.Reasoning != "" {
				aMsg.Content = append(aMsg.Content, anthropicContentBlock{
					Type:     "thinking",
					Thinking: msg.Reasoning,
				})
			}
			if msg.Content != "" {
				aMsg.Content = append(aMsg.Content, anthropicContentBlock{
					Type: "text",
					Text: msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				aMsg.Content = append(aMsg.Content, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: json.RawMessage(tc.Arguments),
				})
			}
		}
		apiReq.Messages = append(apiReq.Messages, aMsg)
	}

	// System prompt: with caching enabled it must be an ARRAY of content
	// blocks (Anthropic only accepts cache_control on blocks, not on the plain
	// string form) with the breakpoint on the last block — the system prompt
	// is byte-identical across every round of a turn, so it is the highest
	// value cache segment. Without caching it stays a plain string for maximum
	// gateway compatibility.
	systemText := strings.TrimSpace(systemPrompt.String())
	if cache {
		if systemText != "" {
			apiReq.System, _ = json.Marshal([]anthropicContentBlock{{
				Type: "text",
				Text: systemText,
				CacheControl: &anthropicCacheControl{
					Type: "ephemeral",
				},
			}})
		}
	} else if systemText != "" {
		apiReq.System, _ = json.Marshal(systemText)
	}

	for i, tool := range req.Tools {
		t := anthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Parameters,
		}
		// Breakpoint on the LAST tool: tools are stable across the turn, and
		// Anthropic requires the cache_control marker on the final element.
		if cache && i == len(req.Tools)-1 {
			t.CacheControl = &anthropicCacheControl{Type: "ephemeral"}
		}
		apiReq.Tools = append(apiReq.Tools, t)
	}

	// Breakpoint on the LAST block of the FIRST message: the first message
	// (the user's original query, before any tool results) is part of the
	// stable prefix, so marking it caches system + tools + first turn together
	// as one unit — every later round then re-sends only the growing delta.
	// Anthropic requires the marker on the last content block of a message.
	if cache && len(apiReq.Messages) > 0 {
		first := &apiReq.Messages[0]
		if len(first.Content) > 0 && first.Role == "user" {
			last := &first.Content[len(first.Content)-1]
			// tool_result blocks cannot carry cache_control (Anthropic rejects
			// it) — the first message of a turn is always a plain user text, but
			// guard anyway for resumed sessions that replay tool results.
			if last.Type != "tool_result" && last.Type != "tool_use" {
				last.CacheControl = &anthropicCacheControl{Type: "ephemeral"}
			}
		}
	}

	bodyBytes, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal anthropic request: %w", err)
	}

	// The streaming client has no total timeout by design; non-streaming
	// Anthropic responses still need a wall-clock bound.
	reqCtx, cancel := context.WithTimeout(ctx, TotalTimeout)
	defer cancel()

	endpoint := a.BaseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request to anthropic failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse anthropic response json: %w", err)
	}

	res := &CompletionResponse{
		Usage: Usage{
			PromptTokens:     apiResp.Usage.InputTokens,
			CompletionTokens: apiResp.Usage.OutputTokens,
			TotalTokens:      apiResp.Usage.InputTokens + apiResp.Usage.OutputTokens,
		},
		FinishReason: apiResp.StopReason,
	}

	var textParts []string
	var reasoningParts []string

	for _, block := range apiResp.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "thinking":
			reasoningParts = append(reasoningParts, block.Thinking)
		case "tool_use":
			res.ToolCalls = append(res.ToolCalls, ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: string(block.Input),
			})
		}
	}

	res.Content = strings.Join(textParts, "\n")
	res.Reasoning = strings.Join(reasoningParts, "\n")

	return res, nil
}
