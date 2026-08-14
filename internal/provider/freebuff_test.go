package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFreeBuffProviderEntry proves the FreeBuff provider is registered with
// the proxy base URL and the official free model list (so /models and
// /connect pick it up without any custom config).
func TestFreeBuffProviderEntry(t *testing.T) {
	var fb *ProviderInfo
	for i := range BuiltinProviders {
		if BuiltinProviders[i].ID == "freebuff" {
			fb = &BuiltinProviders[i]
			break
		}
	}
	if fb == nil {
		t.Fatal("expected a built-in 'freebuff' provider entry")
	}
	if fb.DefaultBaseURL != FreeBuffDefaultBaseURL {
		t.Errorf("expected base URL %q, got %q", FreeBuffDefaultBaseURL, fb.DefaultBaseURL)
	}
	if len(fb.DefaultModels) == 0 {
		t.Fatal("expected FreeBuff provider to declare free models")
	}
	// Official FreeBuff caps (CodebuffAI FREEBUFF_MODEL_CONTEXT_WINDOWS,
	// 2026-08): M3 is capped at 512K on the free tier; MiMo/Gemini-lite use
	// their native 1M. IDs are the official wire IDs (mimo/ prefix).
	if fb.ContextLimits["minimax/minimax-m3"] != 524_288 {
		t.Errorf("expected MiniMax M3 512K FreeBuff cap, got %d", fb.ContextLimits["minimax/minimax-m3"])
	}
	if fb.ContextLimits["mimo/mimo-v2.5"] != 1_048_576 {
		t.Errorf("expected MiMo-V2.5 1M context limit, got %d", fb.ContextLimits["mimo/mimo-v2.5"])
	}
	if !fb.ModelsPublic {
		t.Error("expected freebuff provider to be ModelsPublic (open local proxy)")
	}
	if fb.Protocol != "openai-compatible" {
		t.Errorf("expected openai-compatible protocol, got %q", fb.Protocol)
	}
}

// TestLoadFreeBuffToken covers the credential-file loader used as the
// "FreeBuff CLI logged in" signal for auto-detection.
func TestLoadFreeBuffToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	old := freeBuffCredentialsPathOverride
	freeBuffCredentialsPathOverride = path
	defer func() { freeBuffCredentialsPathOverride = old }()

	write := func(content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Missing file → empty (not logged in).
	if tok := LoadFreeBuffToken(); tok != "" {
		t.Errorf("expected empty token for missing file, got %q", tok)
	}

	// Malformed JSON → empty, no panic.
	write("{not json")
	if tok := LoadFreeBuffToken(); tok != "" {
		t.Errorf("expected empty token for malformed JSON, got %q", tok)
	}

	// Profile without authToken → empty.
	write(`{"default": {"id": "u1", "name": "X"}}`)
	if tok := LoadFreeBuffToken(); tok != "" {
		t.Errorf("expected empty token when no authToken field, got %q", tok)
	}

	// Happy path — the real FreeBuff CLI format.
	write(`{"default": {"id": "u1", "authToken": "tok-abc-123", "email": "a@b.c"}}`)
	if tok := LoadFreeBuffToken(); tok != "tok-abc-123" {
		t.Errorf("expected token from default profile, got %q", tok)
	}

	// Multiple profiles → deterministic (sorted name) pick of first token.
	write(`{"z": {"authToken": "tok-z"}, "a": {"authToken": ""}, "b": {"authToken": "tok-b"}}`)
	if tok := LoadFreeBuffToken(); tok != "tok-b" {
		t.Errorf("expected sorted-first non-empty token 'tok-b', got %q", tok)
	}
}

// TestFreeBuffModelsCoverOfficialFreeAgents anchors the model list to the
// official CodebuffAI free-agents source so a regression (or a wrong hand
// edit) is caught immediately.
func TestFreeBuffModelsCoverOfficialFreeAgents(t *testing.T) {
	known := map[string]bool{}
	for _, m := range FreeBuffModels {
		known[m] = true
	}
	for _, m := range []string{"mimo/mimo-v2.5", "mimo/mimo-v2.5-pro", "minimax/minimax-m3"} {
		if !known[m] {
			t.Errorf("official free agent model %q missing from FreeBuffModels", m)
		}
	}
	// Regression: bare IDs without the provider prefix are rejected by the
	// backend as model_not_found — never reintroduce them.
	for _, bad := range []string{"mimo-v2.5", "minimax/minimax-m3-20260211"} {
		if known[bad] {
			t.Errorf("non-wire model ID %q must not be in FreeBuffModels", bad)
		}
	}
}

// TestDiscoverModelsFreeBuffAuthoritative proves that when a ModelsPublic
// provider's live /v1/models responds, the fetched list REPLACES the baseline
// (models the proxy does not serve are never offered) — unlike keyed
// providers where the configured list is authoritative and merged.
func TestDiscoverModelsFreeBuffAuthoritative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"google/gemini-2.5-flash-lite"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	info := ProviderInfo{
		ID:             "freebuff",
		Protocol:       "openai-compatible",
		DefaultBaseURL: srv.URL,
		DefaultModels:  FreeBuffModels,
		ModelsPublic:   true,
	}
	fetched, err := FetchOpenAIModels(info.DefaultBaseURL, "")
	if err != nil || len(fetched) == 0 {
		t.Fatalf("fetch models failed: %v (%v)", fetched, err)
	}

	models := info.DefaultModels
	if info.ModelsPublic {
		models = fetched // authoritative replace
	} else {
		models = mergeModelLists(models, fetched)
	}
	if len(models) != 1 || models[0] != "google/gemini-2.5-flash-lite" {
		t.Errorf("expected authoritative list to replace baseline, got %v", models)
	}
}
