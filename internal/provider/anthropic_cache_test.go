package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureAnthropicBody runs one Complete against a fake endpoint and returns
// the decoded request body so tests can assert cache_control placement.
func captureAnthropicBody(t *testing.T, messages []Message, tools []ToolDefinition, cacheEnv string) anthropicRequest {
	t.Helper()
	if cacheEnv != "" {
		t.Setenv("BROCODE_NO_PROMPT_CACHE", cacheEnv)
	} else {
		t.Setenv("BROCODE_NO_PROMPT_CACHE", "")
	}

	var got anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body anthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		got = body
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(srv.URL, "sk-test")
	req := CompletionRequest{Model: "claude-x", Messages: messages, Tools: tools}
	if _, err := a.Complete(t.Context(), req); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	return got
}

// anthropicCacheMessages builds the canonical request shape: system prompt,
// user query, one assistant tool round, one tool result — the same prefix
// (system + tools + first user turn) that repeats across every loop round.
func anthropicCacheMessages() []Message {
	return []Message{
		{Role: "system", Content: "You are BroCode CLI."},
		{Role: "user", Content: "explore the codebase"},
		{Role: "assistant", Content: "", Reasoning: "looking", ToolCalls: []ToolCall{
			{ID: "tc1", Name: "read_file", Arguments: `{"path":"a.go"}`},
		}},
		{Role: "user", Content: "package a", ToolCallID: "tc1"},
	}
}

func anthropicCacheTools() []ToolDefinition {
	return []ToolDefinition{
		{Name: "read_file", Description: "read", Parameters: map[string]any{"type": "object"}},
		{Name: "grep", Description: "grep", Parameters: map[string]any{"type": "object"}},
	}
}

// TestAnthropicPromptCacheMarkers proves the three cache breakpoints are
// emitted on the stable prefix: system block, last tool, last block of the
// first message. This is the agent-loop caching pattern — every round after
// the first re-sends only the delta.
func TestAnthropicPromptCacheMarkers(t *testing.T) {
	got := captureAnthropicBody(t, anthropicCacheMessages(), anthropicCacheTools(), "")

	// 1. System must be an ARRAY of blocks with cache_control on the last
	// (only) block — Anthropic rejects cache_control on the string form.
	var sysBlocks []anthropicContentBlock
	if err := json.Unmarshal(got.System, &sysBlocks); err != nil {
		t.Fatalf("system should be a block array when caching enabled, got %s: %v", got.System, err)
	}
	if len(sysBlocks) != 1 || sysBlocks[0].CacheControl == nil || sysBlocks[0].CacheControl.Type != "ephemeral" {
		t.Errorf("expected cache_control on system block, got %+v", sysBlocks)
	}

	// 2. Last tool carries the breakpoint; earlier tools do not.
	if len(got.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(got.Tools))
	}
	if got.Tools[0].CacheControl != nil {
		t.Error("cache_control must only be on the LAST tool")
	}
	if got.Tools[1].CacheControl == nil || got.Tools[1].CacheControl.Type != "ephemeral" {
		t.Error("expected cache_control on last tool")
	}

	// 3. First message's last content block carries the breakpoint. (The
	// system message is extracted out of the array, so 3 remain: the user
	// query, the assistant tool round, and the tool result.)
	if len(got.Messages) != 3 {
		t.Fatalf("expected 3 messages (system extracted), got %d", len(got.Messages))
	}
	first := got.Messages[0]
	if first.Role != "user" {
		t.Fatalf("expected first message role user, got %q", first.Role)
	}
	lastBlock := first.Content[len(first.Content)-1]
	if lastBlock.CacheControl == nil || lastBlock.CacheControl.Type != "ephemeral" {
		t.Error("expected cache_control on last block of first message")
	}
	// The other messages must NOT carry breakpoints (only the stable prefix
	// is marked; the growing delta is never cached).
	for i, m := range got.Messages[1:] {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				t.Errorf("message %d block must not have cache_control (outside stable prefix)", i+1)
			}
		}
	}
}

// TestAnthropicPromptCacheOptOut proves BROCODE_NO_PROMPT_CACHE=1 restores the
// legacy wire format (plain string system, no cache_control anywhere) for
// strict gateways that reject unknown fields.
func TestAnthropicPromptCacheOptOut(t *testing.T) {
	got := captureAnthropicBody(t, anthropicCacheMessages(), anthropicCacheTools(), "1")

	var sysStr string
	if err := json.Unmarshal(got.System, &sysStr); err != nil {
		t.Fatalf("system should be a plain string when caching disabled, got %s: %v", got.System, err)
	}
	if !strings.Contains(sysStr, "You are BroCode CLI.") {
		t.Errorf("unexpected system text %q", sysStr)
	}
	for _, tool := range got.Tools {
		if tool.CacheControl != nil {
			t.Error("cache_control must not appear when caching disabled")
		}
	}
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				t.Error("cache_control must not appear when caching disabled")
			}
		}
	}
}

// TestAnthropicPromptCacheDisabledByDefaultHelper documents that the default
// (no env set) enables caching — covered by TestAnthropicPromptCacheMarkers.
// This test pins the empty-value default so a regression in the env parsing
// (e.g. treating empty string as disabled) is caught.
func TestAnthropicPromptCacheDefaultEnabled(t *testing.T) {
	t.Setenv("BROCODE_NO_PROMPT_CACHE", "")
	if !promptCacheEnabled() {
		t.Error("prompt caching should be enabled by default")
	}
	t.Setenv("BROCODE_NO_PROMPT_CACHE", "1")
	if promptCacheEnabled() {
		t.Error("BROCODE_NO_PROMPT_CACHE=1 should disable prompt caching")
	}
}

// TestAnthropicCacheReadUsageParsing proves cache_read_input_tokens is mapped
// onto Usage.PromptCacheHitTokens so /cost can apply the cache discount.
func TestAnthropicCacheReadUsageParsing(t *testing.T) {
	t.Setenv("BROCODE_NO_PROMPT_CACHE", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1000,"output_tokens":50,"cache_read_input_tokens":800,"cache_creation_input_tokens":200}}`))
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(srv.URL, "sk-test")
	resp, err := a.Complete(t.Context(), CompletionRequest{
		Model:    "claude-sonnet-5",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if resp.Usage.PromptTokens != 1000 {
		t.Errorf("PromptTokens = %d, want 1000", resp.Usage.PromptTokens)
	}
	if resp.Usage.PromptCacheHitTokens != 800 {
		t.Errorf("PromptCacheHitTokens = %d, want 800 (cache_read_input_tokens)", resp.Usage.PromptCacheHitTokens)
	}
}
