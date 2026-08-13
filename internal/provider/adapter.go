package provider

import (
	"context"
)

// Message represents a chat message in the harness.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall represents a function call invoked by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition defines a tool schema for native function calling.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// CompletionRequest is the generic payload sent to any provider adapter.
type CompletionRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
}

// CompletionResponse is the generic output returned by a provider adapter.
type CompletionResponse struct {
	Content      string     `json:"content"`
	Reasoning    string     `json:"reasoning"`
	ToolCalls    []ToolCall `json:"tool_calls"`
	Usage        Usage      `json:"usage"`
	FinishReason string     `json:"finish_reason"`
}

// Usage tracks token consumption.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ProviderAdapter defines the unified contract for LLM communication.
type ProviderAdapter interface {
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
}

// StreamingAdapter is an optional capability: providers that can emit content
// deltas token-by-token. onDelta receives each content fragment as it arrives;
// the returned response still holds the fully accumulated result.
type StreamingAdapter interface {
	ProviderAdapter
	StreamComplete(ctx context.Context, req CompletionRequest, onDelta func(string)) (*CompletionResponse, error)
}

// ProgressingAdapter is an optional capability: providers whose execution
// produces realtime status/progress lines (e.g. a local CLI running tools).
// onProgress receives each status line as it appears; the returned response
// still holds the final accumulated result.
type ProgressingAdapter interface {
	ProviderAdapter
	CompleteWithProgress(ctx context.Context, req CompletionRequest, onProgress func(string)) (*CompletionResponse, error)
}
