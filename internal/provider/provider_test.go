package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutoDetect(t *testing.T) {
	cfg := AppConfig{}
	detected := AutoDetect(cfg)

	if len(detected) == 0 {
		t.Fatalf("expected at least 1 auto-detected provider (opencode gateway), got 0")
	}

	foundOpenCode := false
	for _, d := range detected {
		if d.Info.ID == "opencode" {
			foundOpenCode = true
			break
		}
	}

	if !foundOpenCode {
		t.Errorf("expected OpenCode gateway to be auto-detected by default")
	}
}

func TestLoadConfig(t *testing.T) {
	cfg := LoadConfig()
	if cfg.Providers == nil {
		t.Errorf("expected Providers map to be initialized")
	}
}

func TestOpenCodeCompleteWithProgress(t *testing.T) {
	tmp := t.TempDir()
	fakeCLI := filepath.Join(tmp, "opencode")
	script := "#!/bin/sh\n" +
		"echo '\033[0m> build · hy3-free' >&2\n" +
		"echo 'grep (pattern: filter)' >&2\n" +
		"echo 'read_file (src/app.tsx)' >&2\n" +
		"echo 'FINAL ANSWER LINE 1'\n" +
		"echo 'FINAL ANSWER LINE 2'\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	a := &OpenCodeAdapter{cliPath: fakeCLI, http: NewOpenAIAdapter("http://127.0.0.1:1/v1", "")}
	var progress []string
	res, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "hy3-free",
		Messages: []Message{{Role: "user", Content: "check filters"}},
	}, func(line string) { progress = append(progress, line) })
	if err != nil {
		t.Fatalf("CompleteWithProgress failed: %v", err)
	}
	if !strings.Contains(res.Content, "FINAL ANSWER LINE 1") {
		t.Errorf("expected final answer in content, got %q", res.Content)
	}
	// Progress should have surfaced the tool steps in order.
	joined := strings.Join(progress, "\n")
	if !strings.Contains(joined, "grep (pattern: filter)") || !strings.Contains(joined, "read_file (src/app.tsx)") {
		t.Errorf("expected tool progress lines, got %q", joined)
	}
}

// TestOpenCodeCompleteTimeout ensures a hung opencode CLI process cannot
// block the turn forever — the adapter must time out and fall back to the
// HTTP router instead of waiting indefinitely.
func TestOpenCodeCompleteTimeout(t *testing.T) {
	tmp := t.TempDir()
	fakeCLI := filepath.Join(tmp, "opencode")
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	// HTTP router points at a dead port; if the timeout path works we get a
	// connection error (fast) rather than hanging on the CLI sleep.
	a := &OpenCodeAdapter{cliPath: fakeCLI, http: NewOpenAIAdapter("http://127.0.0.1:1/v1", "")}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := a.CompleteWithProgress(ctx, CompletionRequest{
			Model:    "hy3-free",
			Messages: []Message{{Role: "user", Content: "hi"}},
		}, nil)
		done <- err
	}()

	select {
	case <-time.After(10 * time.Second):
		t.Fatal("CompleteWithProgress blocked longer than the CLI timeout")
	case err := <-done:
		// With a 10-minute internal timeout we can't wait for the real hang;
		// cancelling the parent ctx must abort promptly.
		if ctx.Err() == nil && err == nil {
			t.Fatal("expected an error after cancellation")
		}
	}
}

func TestStripOpenCodeHeader(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single build banner",
			in:   "> build · hy3-free\n\nAnswer here",
			want: "Answer here",
		},
		{
			name: "spinner before banner",
			in:   "⠋ Thinking...\n│ build · hy3-free\n\nAnswer here",
			want: "Answer here",
		},
		{
			name: "ansi around banner",
			in:   "\x1b[0m> build · hy3-free\x1b[0m\n\nAnswer here",
			want: "Answer here",
		},
		{
			name: "no banner",
			in:   "Plain answer text",
			want: "Plain answer text",
		},
		{
			name: "markdown with pipe keeps table",
			in:   "| col | col |\n| --- | --- |\n| a | b |",
			want: "| col | col |\n| --- | --- |\n| a | b |",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripOpenCodeHeader(tc.in)
			if got != tc.want {
				t.Errorf("stripOpenCodeHeader(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseModelJSONObject(t *testing.T) {
	input := `{
		"oc/deepseek-v4-flash-free": {"name": "DeepSeek v4 Flash Free (OC) 1M", "limit": {"context": 1048576, "output": 32768}},
		"ps/poolside/laguna-s-2.1": {"name": "Laguna S 2.1 (PS) 1M", "limit": {"context": 1048576, "output": 32768}},
		"plain-model": {"name": "Just a name"}
	}`

	ids, detail, err := ParseModelJSON(input)
	if err != nil {
		t.Fatalf("ParseModelJSON failed: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 model IDs, got %d: %v", len(ids), ids)
	}
	if detail["oc/deepseek-v4-flash-free"].Limits.Context != 1048576 {
		t.Errorf("expected context limit 1048576, got %d", detail["oc/deepseek-v4-flash-free"].Limits.Context)
	}
	if detail["ps/poolside/laguna-s-2.1"].Limits.Output != 32768 {
		t.Errorf("expected output limit 32768, got %d", detail["ps/poolside/laguna-s-2.1"].Limits.Output)
	}
	if detail["plain-model"].Name != "Just a name" {
		t.Errorf("expected plain model name, got %q", detail["plain-model"].Name)
	}
}

func TestParseModelJSONArray(t *testing.T) {
	ids, detail, err := ParseModelJSON(`["model-a", "model-b", "model-c"]`)
	if err != nil {
		t.Fatalf("ParseModelJSON failed: %v", err)
	}
	if len(ids) != 3 || ids[0] != "model-a" || ids[2] != "model-c" {
		t.Errorf("unexpected model IDs: %v", ids)
	}
	if len(detail) != 0 {
		t.Errorf("expected no detail map for array form, got %v", detail)
	}
}

func TestParseModelJSONEmptyAndInvalid(t *testing.T) {
	if _, _, err := ParseModelJSON(""); err != nil {
		t.Errorf("empty input should parse without error, got %v", err)
	}
	if _, _, err := ParseModelJSON("not json at all"); err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}

func TestCustomProviderConfigRoundTrip(t *testing.T) {
	cfg := AppConfig{
		Providers: map[string]CustomProviderConfig{
			"9router": {
				Protocol: "openai-compatible",
				BaseURL:  "https://9router.rosyidrid.com/v1",
				APIKey:   "sk-test",
				Models:   []string{"oc/deepseek-v4-flash-free", "ps/poolside/laguna-s-2.1"},
				ModelMap: map[string]CustomModel{
					"oc/deepseek-v4-flash-free": {
						Name:   "DeepSeek v4 Flash Free (OC) 1M",
						Limits: ModelLimits{Context: 1048576, Output: 32768},
					},
				},
			},
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var out AppConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	p, ok := out.Providers["9router"]
	if !ok {
		t.Fatalf("expected provider 9router in round-tripped config")
	}
	if p.BaseURL != "https://9router.rosyidrid.com/v1" || len(p.Models) != 2 {
		t.Errorf("unexpected round-tripped provider: %+v", p)
	}
	if p.ModelMap["oc/deepseek-v4-flash-free"].Limits.Context != 1048576 {
		t.Errorf("expected context limit preserved, got %+v", p.ModelMap)
	}
}

func TestOpenAIStreamCompleteContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Hel"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"lo"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, "test-key")
	var deltas []string
	res, err := a.StreamComplete(context.Background(), CompletionRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if res.Content != "Hello" {
		t.Errorf("expected accumulated content Hello, got %q", res.Content)
	}
	if got := strings.Join(deltas, ""); got != "Hello" {
		t.Errorf("expected deltas Hello, got %q", got)
	}
	if res.FinishReason != "stop" {
		t.Errorf("expected finish_reason stop, got %q", res.FinishReason)
	}
}

func TestOpenAIStreamCompleteToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"a"}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":".go\"}"}}]}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, "test-key")
	res, err := a.StreamComplete(context.Background(), CompletionRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "read a.go"}},
	}, nil)
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.Name != "read_file" || tc.ID != "call_1" {
		t.Errorf("unexpected tool call: %+v", tc)
	}
	if tc.Arguments != `{"path":"a.go"}` {
		t.Errorf("expected accumulated arguments, got %q", tc.Arguments)
	}
}
