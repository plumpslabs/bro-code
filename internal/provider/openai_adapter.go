package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIAdapter implements ProviderAdapter for OpenAI-compatible HTTP APIs.
type OpenAIAdapter struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

// NewOpenAIAdapter creates a new adapter for OpenAI-compatible APIs.
func NewOpenAIAdapter(baseURL, apiKey string) *OpenAIAdapter {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIAdapter{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 120 * time.Second},
	}
}

type openAIChatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	Reasoning  string           `json:"reasoning_content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type openAIChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	Tools       []openAITool        `json:"tools,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Role             string           `json:"role"`
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			Reasoning        string           `json:"reasoning"`
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// openAIStreamChunk is a single SSE delta for /chat/completions with stream=true.
type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func buildOpenAIRequest(req CompletionRequest, stream bool) (*openAIChatRequest, error) {
	apiReq := &openAIChatRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		Stream:      stream,
	}

	for _, msg := range req.Messages {
		oMsg := openAIChatMessage{
			Role:       msg.Role,
			Content:    msg.Content,
			Reasoning:  msg.Reasoning,
			ToolCallID: msg.ToolCallID,
		}
		for _, tc := range msg.ToolCalls {
			oMsg.ToolCalls = append(oMsg.ToolCalls, openAIToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		apiReq.Messages = append(apiReq.Messages, oMsg)
	}

	for _, tool := range req.Tools {
		oTool := openAITool{Type: "function"}
		oTool.Function.Name = tool.Name
		oTool.Function.Description = tool.Description
		oTool.Function.Parameters = tool.Parameters
		apiReq.Tools = append(apiReq.Tools, oTool)
	}

	return apiReq, nil
}

func (a *OpenAIAdapter) doPost(ctx context.Context, apiReq *openAIChatRequest) (*http.Response, error) {
	bodyBytes, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	endpoint := a.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if a.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.APIKey)
	}

	resp, err := a.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, fmt.Errorf("API error HTTP %d: %s", resp.StatusCode, string(body))
	}
	return resp, nil
}

func (a *OpenAIAdapter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	apiReq, err := buildOpenAIRequest(req, false)
	if err != nil {
		return nil, err
	}

	resp, err := a.doPost(ctx, apiReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var apiResp openAIChatResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response json: %w", err)
	}

	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("received empty choices from LLM provider")
	}

	choice := apiResp.Choices[0]
	reasoning := choice.Message.ReasoningContent
	if reasoning == "" {
		reasoning = choice.Message.Reasoning
	}

	res := &CompletionResponse{
		Content:   choice.Message.Content,
		Reasoning: reasoning,
		Usage: Usage{
			PromptTokens:     apiResp.Usage.PromptTokens,
			CompletionTokens: apiResp.Usage.CompletionTokens,
			TotalTokens:      apiResp.Usage.TotalTokens,
		},
		FinishReason: choice.FinishReason,
	}

	for _, tc := range choice.Message.ToolCalls {
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	return res, nil
}

// StreamComplete implements StreamingAdapter: content deltas are forwarded via
// onDelta while tool-call fragments accumulate across SSE chunks.
func (a *OpenAIAdapter) StreamComplete(ctx context.Context, req CompletionRequest, onDelta func(string)) (*CompletionResponse, error) {
	apiReq, err := buildOpenAIRequest(req, true)
	if err != nil {
		return nil, err
	}

	resp, err := a.doPost(ctx, apiReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	res := &CompletionResponse{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // tolerate keep-alive/ping frames
		}
		if len(chunk.Choices) == 0 {
			if chunk.Usage.TotalTokens > 0 {
				res.Usage = Usage{
					PromptTokens:     chunk.Usage.PromptTokens,
					CompletionTokens: chunk.Usage.CompletionTokens,
					TotalTokens:      chunk.Usage.TotalTokens,
				}
			}
			continue
		}

		choice := chunk.Choices[0]
		if d := choice.Delta.Content; d != "" {
			res.Content += d
			if onDelta != nil {
				onDelta(d)
			}
		}
		if d := choice.Delta.ReasoningContent; d != "" {
			res.Reasoning += d
		}
		if choice.FinishReason != "" {
			res.FinishReason = choice.FinishReason
		}
		for _, tc := range choice.Delta.ToolCalls {
			for len(res.ToolCalls) <= tc.Index {
				res.ToolCalls = append(res.ToolCalls, ToolCall{})
			}
			if tc.ID != "" && res.ToolCalls[tc.Index].ID == "" {
				res.ToolCalls[tc.Index].ID = tc.ID
			}
			if tc.Function.Name != "" {
				res.ToolCalls[tc.Index].Name = tc.Function.Name
			}
			res.ToolCalls[tc.Index].Arguments += tc.Function.Arguments
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("stream read failed: %w", err)
	}
	return res, nil
}
