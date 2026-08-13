package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunExploreLoopNestedRounds drives the nested loop through two model
// calls: round 1 returns a search tool call (which the loop executes and
// feeds back), round 2 returns the plain-text report — which terminates the
// loop. Asserts the round count, the report, and that the nested request
// carried the read-only tool surface.
func TestRunExploreLoopNestedRounds(t *testing.T) {
	var calls int
	var toolNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string                   `json:"model"`
			Messages []map[string]string      `json:"messages"`
			Tools    []map[string]interface{} `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		for _, tl := range req.Tools {
			fn, _ := tl["function"].(map[string]interface{})
			if n, _ := fn["name"].(string); n != "" {
				toolNames = append(toolNames, n)
			}
		}
		// Round 1: emit a search tool call. Round 2: emit the final report.
		if calls == 1 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"",
				"tool_calls":[{"id":"c1","type":"function","function":{"name":"search","arguments":"{\"query\":\"rotation\"}"}}]}}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"## Rotation pipeline\n- ` + "`" + `LeadRotationService.js:505` + "`" + ` gates on enable_auto_rotation\n"}}]}`))
	}))
	defer srv.Close()

	cfg := &exploreConfig{endpoint: srv.URL, model: "test-model"}
	report, rounds, err := runExploreLoop(cfg, "map the rotation pipeline")
	if err != nil {
		t.Fatalf("runExploreLoop error: %v", err)
	}
	if rounds != 2 {
		t.Fatalf("expected 2 rounds (tool round + report round), got %d", rounds)
	}
	if !strings.Contains(report, "LeadRotationService.js:505") {
		t.Fatalf("expected the report content, got: %q", report)
	}
	// The nested surface must be read-only: search + read only.
	for _, n := range toolNames {
		if n != "search" && n != "read" {
			t.Fatalf("nested loop exposed non-read-only tool %q", n)
		}
	}
}

// TestRunExploreLoopTextOnlyReport: a model that answers immediately with
// text (no tool calls) terminates the loop after ONE round.
func TestRunExploreLoopSendsAuthHeader(t *testing.T) {
	// A poolside/custom exploreConfig carries an apiKey — the nested loop
	// must authenticate with it (the main provider path does, so the
	// subagent must too, or a poolside session would delegate to a 401).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer pool-key" {
			t.Fatalf("expected Bearer pool-key auth, got %q", got)
		}
		if got := r.Header.Get("X-Custom"); got != "yes" {
			t.Fatalf("expected custom header forwarded, got %q", got)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Report done."}}]}`))
	}))
	defer srv.Close()

	cfg := &exploreConfig{endpoint: srv.URL, model: "test-model", apiKey: "pool-key", headers: map[string]string{"X-Custom": "yes"}}
	report, rounds, err := runExploreLoop(cfg, "any question")
	if err != nil {
		t.Fatalf("runExploreLoop error: %v", err)
	}
	if rounds != 1 || !strings.Contains(report, "Report done.") {
		t.Fatalf("expected 1 round with the report, got rounds=%d report=%q", rounds, report)
	}
}

func TestRunExploreLoopTextOnlyReport(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"No evidence found for the question."}}]}`))
	}))
	defer srv.Close()

	cfg := &exploreConfig{endpoint: srv.URL, model: "test-model"}
	report, rounds, err := runExploreLoop(cfg, "is there a webhook handler?")
	if err != nil {
		t.Fatalf("runExploreLoop error: %v", err)
	}
	if rounds != 1 {
		t.Fatalf("expected 1 round, got %d", rounds)
	}
	if !strings.Contains(report, "No evidence found") {
		t.Fatalf("expected the report, got: %q", report)
	}
}

// TestRunExploreLoopUnavailable: a nil config reports the graceful message
// instead of erroring — the main agent then falls back to search/read.
func TestRunExploreLoopUnavailable(t *testing.T) {
	report, rounds, err := runExploreLoop(nil, "anything")
	if err != nil {
		t.Fatalf("nil config must not error, got %v", err)
	}
	if rounds != 0 {
		t.Fatalf("expected 0 rounds for nil config, got %d", rounds)
	}
	if !strings.Contains(report, "unavailable") {
		t.Fatalf("expected the unavailable message, got: %q", report)
	}
}

// TestExplorerToolsSurfaceReadOnly: explorerTools must be a strict subset of
// the full payload containing exactly search + read.
func TestExploreConfigForSupportsPoolsideAndCustom(t *testing.T) {
	// REGRESSION: exploreConfigFor used to return nil for EVERY provider but
	// opencode — so the poolside path (rs · ps/poolside/laguna-s-2.1) got
	// "explore unavailable" and the model dumped full file reads into the
	// main context (5.9k → 12.9k token bloat per round). Poolside and custom
	// OpenAI-compatible providers must get a working config (with their key).

	// Sandbox HOME so saveAPIKey/loadAPIKey never touch the real user's
	// ~/.brocode/keys.json.
	t.Setenv("HOME", t.TempDir())

	// Poolside WITHOUT a key → nil (same guard as the main path).
	mNoKey := newTestModel()
	mNoKey.provider = "poolside"
	if ec := mNoKey.exploreConfigFor(); ec != nil {
		t.Fatal("poolside without a key must yield nil explore config")
	}

	// Poolside with a saved key → config with endpoint + key.
	m := newTestModel()
	m.provider = "poolside"
	m.selectedModel = "poolside/laguna-s-2.1"
	if err := saveAPIKey("poolside", "test-pool-key"); err != nil {
		t.Fatal(err)
	}
	ec := m.exploreConfigFor()
	if ec == nil {
		t.Fatal("poolside explore config must not be nil")
	}
	if !strings.Contains(ec.endpoint, "inference.poolside.ai") {
		t.Fatalf("expected poolside endpoint, got %q", ec.endpoint)
	}
	if ec.apiKey != "test-pool-key" {
		t.Fatalf("poolside explore config must carry the api key, got %q", ec.apiKey)
	}
	if ec.model != "poolside/laguna-s-2.1" {
		t.Fatalf("expected selected model, got %q", ec.model)
	}

	// Custom provider from config.jsonc → endpoint + key + headers. The file
	// lives at $HOME/.brocode/config.jsonc (sandboxed above).
	cfgDir := filepath.Join(os.Getenv("HOME"), ".brocode")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.jsonc"), []byte(`{"provider":{"myapi":{"options":{"baseURL":"https://api.example.com/v1","apiKey":"cfg-key","headers":{"X-Custom":"yes"}}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mc := newTestModel()
	mc.provider = "myapi"
	mc.selectedModel = "myapi/m-1"
	ec2 := mc.exploreConfigFor()
	if ec2 == nil {
		t.Fatal("custom provider explore config must not be nil")
	}
	if ec2.endpoint != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("expected /chat/completions appended, got %q", ec2.endpoint)
	}
	if ec2.apiKey != "cfg-key" || ec2.headers["X-Custom"] != "yes" {
		t.Fatalf("custom config must carry key+headers, got %+v", ec2)
	}
}

func TestExplorerToolsSurfaceReadOnly(t *testing.T) {
	got := explorerTools()
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 tools (search, read), got %d: %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, tl := range got {
		fn, _ := tl["function"].(map[string]interface{})
		names[fn["name"].(string)] = true
	}
	if !names["search"] || !names["read"] {
		t.Fatalf("expected search + read, got %v", names)
	}
}
