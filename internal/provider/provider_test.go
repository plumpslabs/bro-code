package provider

import (
	"slices"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestAutoDetectInheritsBuiltinModels proves a custom provider that omits its
// model list but matches a built-in provider ID (e.g. "poolside" saved with
// only a base URL + key) inherits the built-in's models and context limits —
// instead of falling back to the placeholder "default" model that silently
// fails on the primary provider and routes every turn through the fallback.
func TestAutoDetectInheritsBuiltinModels(t *testing.T) {
	cfg := AppConfig{
		Providers: map[string]CustomProviderConfig{
			"poolside": {
				Protocol: "openai-compatible",
				BaseURL:  "https://inference.poolside.ai/v1",
				APIKey:   "sky_test-key",
				// No Models declared — the regression case.
			},
		},
	}

	detected := AutoDetect(cfg)
	var poolside *DetectedProvider
	for i := range detected {
		if detected[i].Info.ID == "poolside" {
			poolside = &detected[i]
			break
		}
	}
	if poolside == nil {
		t.Fatal("expected poolside provider to be detected")
	}
	if len(poolside.Info.DefaultModels) == 0 {
		t.Fatal("poolside with no declared models must inherit built-in models")
	}
	if poolside.Info.DefaultModels[0] != "poolside/laguna-s-2.1" {
		t.Errorf("expected built-in poolside/laguna-s-2.1 first, got %q", poolside.Info.DefaultModels[0])
	}
	if poolside.Info.ContextLimits["poolside/laguna-s-2.1"] != 262_144 {
		t.Errorf("expected built-in context limit inherited, got %d", poolside.Info.ContextLimits["poolside/laguna-s-2.1"])
	}
}

// TestResolveModelID proves stale saved model IDs are mapped onto the real
// provider model list (the poolside "laguna-s-2.1" → "poolside/laguna-s-2.1"
// case: configs saved before the API added its vendor prefix must keep
// working instead of 404-ing on the primary provider).
func TestResolveModelID(t *testing.T) {
	models := []string{"poolside/laguna-s-2.1", "poolside/laguna-xs-2.1"}

	// Exact match stays.
	if got := ResolveModelID(models, "poolside/laguna-s-2.1"); got != "poolside/laguna-s-2.1" {
		t.Errorf("exact match broken: %q", got)
	}
	// Stale bare ID resolves by last segment.
	if got := ResolveModelID(models, "laguna-s-2.1"); got != "poolside/laguna-s-2.1" {
		t.Errorf("expected poolside/laguna-s-2.1, got %q", got)
	}
	// Unknown custom IDs pass through untouched.
	if got := ResolveModelID(models, "my-custom-model"); got != "my-custom-model" {
		t.Errorf("unknown model must pass through, got %q", got)
	}
	// Empty stays empty.
	if got := ResolveModelID(models, ""); got != "" {
		t.Errorf("empty model must stay empty, got %q", got)
	}
}

// TestAutoDetectKeepsDeclaredModels proves an explicitly declared model list is
// never clobbered by the built-in inheritance.
func TestAutoDetectKeepsDeclaredModels(t *testing.T) {
	cfg := AppConfig{
		Providers: map[string]CustomProviderConfig{
			"poolside": {
				Protocol: "openai-compatible",
				BaseURL:  "https://inference.poolside.ai/v1",
				APIKey:   "sky_test-key",
				Models:   []string{"my-custom-model"},
			},
		},
	}

	detected := AutoDetect(cfg)
	for _, d := range detected {
		if d.Info.ID != "poolside" {
			continue
		}
		if len(d.Info.DefaultModels) != 1 || d.Info.DefaultModels[0] != "my-custom-model" {
			t.Errorf("declared models must win, got %v", d.Info.DefaultModels)
		}
		return
	}
	t.Fatal("poolside not detected")
}

// TestFetchOpenAIModels hits a fake gateway /models endpoint and proves the
// live model list is parsed.
func TestFetchOpenAIModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("expected /models path, got %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("expected auth header, got %q", got)
		}
		w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"model-a"},{"id":"model-a"}]}`))
	}))
	defer srv.Close()

	models, err := FetchOpenAIModels(srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("FetchOpenAIModels failed: %v", err)
	}
	if len(models) != 2 || models[0] != "model-a" || models[1] != "model-b" {
		t.Errorf("expected sorted deduped [model-a model-b], got %v", models)
	}
}

// TestFetchOpenAIModelsError proves a failing /models endpoint returns an error
// (so callers can fall back to a warning instead of a placeholder model).
func TestFetchOpenAIModelsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := FetchOpenAIModels(srv.URL, "sk-test"); err == nil {
		t.Error("expected error for 401 response")
	}
}

// TestLoadConfigBroCodeOverridesOpenCode proves BroCode's own config is
// authoritative: the same provider ID defined in opencode.jsonc is overridden
// by ~/.config/brocode/config.json (opencode is only a fallback source).
func TestLoadConfigBroCodeOverridesOpenCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ocDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	ocJSON := `{
		"provider": {
			"9router": {
				"options": {"baseURL": "https://from-opencode.example/v1"},
				"models": {"m1": {"name": "M1"}}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(ocDir, "opencode.jsonc"), []byte(ocJSON), 0644); err != nil {
		t.Fatal(err)
	}

	bcDir := filepath.Join(home, ".config", "brocode")
	if err := os.MkdirAll(bcDir, 0755); err != nil {
		t.Fatal(err)
	}
	bcJSON := `{
		"providers": {
			"9router": {"protocol": "openai-compatible", "base_url": "https://from-brocode.example/v1"}
		}
	}`
	if err := os.WriteFile(filepath.Join(bcDir, "config.json"), []byte(bcJSON), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	p, ok := cfg.Providers["9router"]
	if !ok {
		t.Fatalf("expected 9router provider loaded, got %+v", cfg.Providers)
	}
	if p.BaseURL != "https://from-brocode.example/v1" {
		t.Errorf("BroCode config must override opencode.jsonc, got baseURL %q", p.BaseURL)
	}
}

// TestOpenCodeImportDisabled proves BROCODE_NO_OPENCODE=1 makes BroCode fully
// standalone: no opencode.jsonc provider import and no opencode provider
// auto-detection.
// TestLoadConfigJSONCHandEdited proves BroCode's own hand-editable config.jsonc
// (comments allowed, BroCode schema — NOT opencode's format) is read and
// overrides the wizard-written config.json.
func TestLoadConfigJSONCHandEdited(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bcDir := filepath.Join(home, ".config", "brocode")
	if err := os.MkdirAll(bcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Machine-written config.json (wizard format).
	jsonCfg := `{
		"providers": {
			"rs": {"protocol": "openai-compatible", "base_url": "https://from-json.example/v1"}
		}
	}`
	if err := os.WriteFile(filepath.Join(bcDir, "config.json"), []byte(jsonCfg), 0644); err != nil {
		t.Fatal(err)
	}

	// Hand-edited config.jsonc (comments allowed, BroCode schema) overrides it.
	jsoncCfg := `// hand-edited provider
{
	"default_provider": "rs",
	"providers": {
		"rs": {
			"protocol": "openai-compatible",
			"base_url": "https://from-jsonc.example/v1"
		}
	}
}`
	if err := os.WriteFile(filepath.Join(bcDir, "config.jsonc"), []byte(jsoncCfg), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	if cfg.DefaultProvider != "rs" {
		t.Errorf("expected default_provider from jsonc, got %q", cfg.DefaultProvider)
	}
	p, ok := cfg.Providers["rs"]
	if !ok {
		t.Fatalf("expected rs provider loaded, got %+v", cfg.Providers)
	}
	if p.BaseURL != "https://from-jsonc.example/v1" {
		t.Errorf("config.jsonc must override config.json, got baseURL %q", p.BaseURL)
	}
}

// TestOpenCodeImportSkipsDuplicateBaseURL proves an opencode.jsonc provider is
// NOT imported when BroCode already configures a provider pointing at the same
// base URL (e.g. lalarasa from opencode vs kahuna in BroCode config — both
// target the same gateway, so only the BroCode one stays).
func TestOpenCodeImportSkipsDuplicateBaseURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// BroCode global config: provider "kahuna" → 9router URL.
	bcDir := filepath.Join(home, ".config", "brocode")
	if err := os.MkdirAll(bcDir, 0755); err != nil {
		t.Fatal(err)
	}
	bcJSON := `{
		"providers": {
			"kahuna": {
				"protocol": "openai-compatible",
				"base_url": "https://9router.example/v1",
				"api_key": "sk-test",
				"models": ["oc/deepseek-v4-flash-free"]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(bcDir, "config.json"), []byte(bcJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// opencode.jsonc: provider "lalarasa" → SAME base URL (duplicate).
	ocDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	ocJSON := `{
		"provider": {
			"lalarasa": {
				"options": {"baseURL": "https://9router.example/v1"},
				"models": {"oc/deepseek-v4-flash-free": {"name": "M"}}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(ocDir, "opencode.jsonc"), []byte(ocJSON), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	if _, ok := cfg.Providers["lalarasa"]; ok {
		t.Errorf("duplicate opencode provider must NOT be imported when BroCode already has the same base URL")
	}
	if _, ok := cfg.Providers["kahuna"]; !ok {
		t.Errorf("expected BroCode-configured kahuna provider to remain")
	}

	// A DIFFERENT opencode provider (different base URL) is still imported.
	ocJSON2 := `{
		"provider": {
			"other-gw": {
				"options": {"baseURL": "https://other.example/v1"},
				"models": {"m1": {"name": "M1"}}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(ocDir, "opencode.jsonc"), []byte(ocJSON2), 0644); err != nil {
		t.Fatal(err)
	}
	cfg2 := LoadConfig()
	if _, ok := cfg2.Providers["other-gw"]; !ok {
		t.Errorf("expected non-duplicate opencode provider to be imported")
	}
}

// TestPersistedDuplicatePruned proves a duplicate provider that was persisted
// into config.json by an older build (keyless lalarasa next to keyed kahuna,
// same base URL) is pruned on load AND on save — no manual edit needed.
func TestPersistedDuplicatePruned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bcDir := filepath.Join(home, ".config", "brocode")
	if err := os.MkdirAll(bcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// The exact shape the user reported: keyed kahuna + keyless lalarasa,
	// both pointing at the same base URL.
	cfgJSON := `{
		"default_provider": "kahuna",
		"default_model": "oc/deepseek-v4-flash-free",
		"providers": {
			"kahuna": {
				"protocol": "openai-compatible",
				"base_url": "https://9router.example/v1",
				"api_key": "sk-test",
				"models": ["oc/deepseek-v4-flash-free"]
			},
			"lalarasa": {
				"protocol": "openai-compatible",
				"base_url": "https://9router.example/v1",
				"models": ["oc/deepseek-v4-flash-free"]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(bcDir, "config.json"), []byte(cfgJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// On load: the keyless duplicate is gone, the keyed provider stays.
	cfg := LoadConfig()
	if _, ok := cfg.Providers["lalarasa"]; ok {
		t.Errorf("persisted duplicate lalarasa must be pruned on load")
	}
	if _, ok := cfg.Providers["kahuna"]; !ok {
		t.Errorf("keyed kahuna must survive dedupe")
	}

	// On save: the written file no longer contains the duplicate either.
	if err := SaveGlobalConfig(cfg); err != nil {
		t.Fatalf("SaveGlobalConfig: %v", err)
	}
	saved, err := os.ReadFile(filepath.Join(bcDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "lalarasa") {
		t.Errorf("duplicate must not be re-persisted on save: %s", string(saved))
	}
	if !strings.Contains(string(saved), "kahuna") {
		t.Errorf("kahuna must be preserved on save: %s", string(saved))
	}
}

func TestSaveAndGetActiveSearchKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("EXA_API_KEY", "")

	// 1. Initially empty
	key, prov := GetActiveSearchKey()
	if key != "" || prov != "" {
		t.Fatalf("expected empty initially, got key=%q prov=%q", key, prov)
	}

	// 2. Save Tavily Key
	if err := SaveSearchKey("tvly-abcdef123456"); err != nil {
		t.Fatalf("SaveSearchKey failed: %v", err)
	}

	key, prov = GetActiveSearchKey()
	if key != "tvly-abcdef123456" || prov != "tavily" {
		t.Fatalf("expected tavily key, got key=%q prov=%q", key, prov)
	}

	// 3. Clear Key
	if err := SaveSearchKey(""); err != nil {
		t.Fatalf("SaveSearchKey clear failed: %v", err)
	}
	key, prov = GetActiveSearchKey()
	if key != "" || prov != "" {
		t.Fatalf("expected empty after clear, got key=%q prov=%q", key, prov)
	}
}

func TestSaveAndGetActiveContext7Key(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONTEXT7_API_KEY", "")

	// 1. Initially empty
	key := GetActiveContext7Key()
	if key != "" {
		t.Fatalf("expected empty initially, got %q", key)
	}

	// 2. Save Context7 Key
	if err := SaveContext7Key("c7_test_987654321"); err != nil {
		t.Fatalf("SaveContext7Key failed: %v", err)
	}

	key = GetActiveContext7Key()
	if key != "c7_test_987654321" {
		t.Fatalf("expected saved key, got %q", key)
	}

	// 3. Clear Key
	if err := SaveContext7Key("clear"); err != nil {
		t.Fatalf("SaveContext7Key clear failed: %v", err)
	}
	key = GetActiveContext7Key()
	if key != "" {
		t.Fatalf("expected empty after clear, got %q", key)
	}
}

func TestOpenCodeImportDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ocDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	ocJSON := `{"provider": {"9router": {"options": {"baseURL": "https://x.example/v1"}}}}`
	if err := os.WriteFile(filepath.Join(ocDir, "opencode.jsonc"), []byte(ocJSON), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BROCODE_NO_OPENCODE", "1")

	cfg := LoadConfig()
	if _, ok := cfg.Providers["9router"]; ok {
		t.Errorf("opencode.jsonc providers must NOT be imported when BROCODE_NO_OPENCODE=1")
	}

	for _, d := range AutoDetect(cfg) {
		if d.Info.ID == "opencode" {
			t.Errorf("opencode provider must not be auto-detected when BROCODE_NO_OPENCODE=1")
		}
	}
}

// TestOpenCodeAdapterStreamsViaHTTP proves the OpenCode adapter is fully
// standalone: it forwards the request to the HTTP gateway and streams content
// deltas, with NO opencode CLI spawned. A fake SSE server stands in for the
// gateway.
func TestOpenCodeAdapterStreamsViaHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, c := range []string{"FINAL", " ANSWER", " LINE 1", " LINE 2"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := &OpenCodeAdapter{http: NewOpenAIAdapter(srv.URL, "")}
	var progress []string
	res, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "hy3-free",
		Messages: []Message{{Role: "user", Content: "check filters"}},
	}, func(line string) { progress = append(progress, line) })
	if err != nil {
		t.Fatalf("CompleteWithProgress failed: %v", err)
	}
	if res.Content != "FINAL ANSWER LINE 1 LINE 2" {
		t.Errorf("expected streamed content, got %q", res.Content)
	}
	joined := strings.Join(progress, "")
	if !strings.Contains(joined, "FINAL") || !strings.Contains(joined, "LINE 2") {
		t.Errorf("expected streamed progress deltas, got %q", joined)
	}
}

// TestOpenCodeAdapterHTTPFailurePropagates proves a gateway failure returns an
// error instead of silently hanging or spawning a fallback CLI.
func TestOpenCodeAdapterHTTPFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := &OpenCodeAdapter{http: NewOpenAIAdapter(srv.URL, "")}
	_, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "hy3-free",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error from failing gateway, got nil")
	}
}

// TestOpenCodeAdapterStripsRoutingPrefix proves an "opencode/" prefixed model
// id is normalized before hitting the HTTP gateway (the gateway's own
// catalogue has no such prefix).
func TestOpenCodeAdapterStripsRoutingPrefix(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body openAIChatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := &OpenCodeAdapter{http: NewOpenAIAdapter(srv.URL, "")}
	if _, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "opencode/hy3-free",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil); err != nil {
		t.Fatalf("complete failed: %v", err)
	}
	if gotModel != "hy3-free" {
		t.Errorf("expected routing prefix stripped, got model %q", gotModel)
	}
}

// TestOpenCodeStandalone proves the adapter talks to the gateway over HTTP
// only — it never shells out to an opencode binary, so a missing opencode
// install must not break completion (a fake gateway stands in here).
func TestOpenCodeStandalone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := NewOpenCodeAdapter()
	// Point the embedded HTTP adapter at the fake gateway.
	a.http = NewOpenAIAdapter(srv.URL, "")
	if _, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "hy3-free",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil); err != nil {
		t.Fatalf("standalone completion failed: %v", err)
	}
}

func TestStripJSONCommentsPreservesURLs(t *testing.T) {
	in := `// header comment
{
  "providers": {
    "rs": {
      "base_url": "https://9router.example/v1", /* keep */
      "api_key": "sk-test"
    }
  }
}`
	out := stripJSONComments(in)
	if !strings.Contains(out, "https://9router.example/v1") {
		t.Errorf("URL truncated by comment stripper: %q", out)
	}
	if strings.Contains(out, "header comment") || strings.Contains(out, "/*") || strings.Contains(out, "*/") {
		t.Errorf("comments not fully stripped: %q", out)
	}
	var cfg AppConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Errorf("stripped jsonc should parse as JSON: %v (%q)", err, out)
	}
	if cfg.Providers["rs"].BaseURL != "https://9router.example/v1" {
		t.Errorf("expected base URL preserved, got %q", cfg.Providers["rs"].BaseURL)
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

func TestParseModelJSONBareBodyNoBraces(t *testing.T) {
	// User pasted the models block from opencode.jsonc WITHOUT the wrapping
	// braces — the parser must tolerate it.
	input := `"oc/deepseek-v4-flash-free": {
		"name": "DeepSeek v4 Flash Free (OC) 1M",
		"limit": {"context": 1048576, "output": 32768}
	},
	"ps/poolside/laguna-s-2.1": {
		"name": "Laguna S 2.1 (PS) 1M",
		"limit": {"context": 1048576, "output": 32768}
	}`

	ids, detail, err := ParseModelJSON(input)
	if err != nil {
		t.Fatalf("bare body without braces should parse, got error: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 model IDs, got %v", ids)
	}
	if detail["oc/deepseek-v4-flash-free"].Limits.Context != 1048576 {
		t.Errorf("expected context limit parsed, got %+v", detail["oc/deepseek-v4-flash-free"])
	}
	if detail["ps/poolside/laguna-s-2.1"].Name != "Laguna S 2.1 (PS) 1M" {
		t.Errorf("expected name parsed, got %q", detail["ps/poolside/laguna-s-2.1"].Name)
	}

	// Truly invalid input must still fail (not be silently wrapped).
	if _, _, err := ParseModelJSON("not json at all"); err == nil {
		t.Errorf("expected error for garbage input")
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

// TestOpenCodeFreeModelsClean verifies the static free-model list the picker
// uses — the opencode CLI is never spawned at startup, so the list must be
// self-contained and free of gateway alias noise (no lalarasa/ duplicates,
// no opencode/ prefix).
func TestOpenCodeFreeModelsClean(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range OpenCodeFreeModels {
		if m == "" {
			t.Error("empty model in OpenCodeFreeModels")
		}
		if strings.HasPrefix(m, "lalarasa/") {
			t.Errorf("lalarasa alias leaked into static list: %q", m)
		}
		if strings.HasPrefix(m, "opencode/") {
			t.Errorf("opencode/ prefix leaked into static list: %q", m)
		}
		if seen[m] {
			t.Errorf("duplicate model in OpenCodeFreeModels: %q", m)
		}
		seen[m] = true
	}
	if len(seen) == 0 {
		t.Error("OpenCodeFreeModels must not be empty (the picker depends on it)")
	}
}

// TestDiscoverModelsNoCLISpawn verifies that model discovery never executes
// an external binary: AutoDetect + DiscoverModels with an empty config must
// return the static opencode models without launching anything.
func TestDiscoverModelsNoCLISpawn(t *testing.T) {
	models := DiscoverModels(AppConfig{})
	if len(models["opencode"]) == 0 {
		t.Error("expected static opencode free models in discovery")
	}
}

func TestMergeModelLists(t *testing.T) {
	configured := []string{"ps/poolside/laguna-s-2.1", "ps/poolside/laguna-xs-2.1", "oc/deepseek-v4-flash-free"}
	// Live fetch returns the same models under shorter IDs, and one extra.
	fetched := []string{"ps/laguna-s-2.1", "ps/laguna-xs-2.1", "extra-model"}

	merged := mergeModelLists(configured, fetched)

	// All configured models survive (this is the user's reported bug).
	for _, want := range configured {
		found := slices.Contains(merged, want)
		if !found {
			t.Errorf("configured model %q lost in merge: %v", want, merged)
		}
	}

	// Fetched models that duplicate a configured suffix are NOT appended twice.
	suffixCount := map[string]int{}
	for _, m := range merged {
		suffixCount[lastSegment(m)]++
	}
	for seg, n := range suffixCount {
		if n > 1 {
			t.Errorf("model suffix %q listed %d times: %v", seg, n, merged)
		}
	}

	// Fetched-only models ARE appended.
	if !containsStr(merged, "extra-model") {
		t.Errorf("fetched-only model should be appended: %v", merged)
	}
}

func containsStr(list []string, s string) bool {
	return slices.Contains(list, s)
}

func TestContextWindowFor(t *testing.T) {
	cfg := AppConfig{
		Providers: map[string]CustomProviderConfig{
			"my-gateway": {
				BaseURL: "https://api.my-gateway.example/v1",
				ModelMap: map[string]CustomModel{
					"big-model": {Limits: ModelLimits{Context: 1048576, Output: 32768}},
				},
			},
		},
	}

	if got := ContextWindowFor(cfg, "my-gateway", "big-model"); got != 1048576 {
		t.Errorf("expected 1048576 window, got %d", got)
	}
	if got := ContextWindowFor(cfg, "my-gateway", "unknown-model"); got != 0 {
		t.Errorf("expected 0 for unknown model, got %d", got)
	}
	if got := ContextWindowFor(cfg, "unknown-provider", "big-model"); got != 0 {
		t.Errorf("expected 0 for unknown provider, got %d", got)
	}
	if got := ContextWindowFor(AppConfig{}, "my-gateway", "big-model"); got != 0 {
		t.Errorf("expected 0 for empty config, got %d", got)
	}
}

// TestContextWindowForOpenCode anchors the per-model free-tier windows of the
// BroCode free gateway (models.dev, 2026-08): the free tier does NOT cap every
// model at 200K — longcat/nemotron-ultra serve their native 1M, laguna-free is
// 256K, hy3-free is 190K, ling/nemotron-3.5-lightning are 262144.
func TestContextWindowForOpenCode(t *testing.T) {
	cases := map[string]int{
		"deepseek-v4-flash-free":      200_000,
		"hy3-free":                    190_000,
		"mimo-v2.5-free":              200_000,
		"laguna-s-2.1-free":           256_000,
		"ling-3.0-tiny-free":          262_144,
		"longcat-2.0-free":            1_000_000,
		"nemotron-3-ultra-free":       1_000_000,
		"nemotron-3.5-lightning-free": 262_144,
		"big-pickle":                  200_000,
	}
	for model, want := range cases {
		if got := ContextWindowFor(AppConfig{}, "opencode", model); got != want {
			t.Errorf("opencode %s: expected %d window, got %d", model, want, got)
		}
	}
}

// TestContextWindowForDeepSeek anchors the 1M window of every current DeepSeek
// model (2026-07-24: deepseek-chat/reasoner are now aliases of v4-flash).
func TestContextWindowForDeepSeek(t *testing.T) {
	cases := map[string]int{
		"deepseek-chat":     1_000_000,
		"deepseek-reasoner": 1_000_000,
		"deepseek-v4-flash": 1_000_000,
		"deepseek-v4-pro":   1_000_000,
	}
	for model, want := range cases {
		if got := ContextWindowFor(AppConfig{}, "deepseek", model); got != want {
			t.Errorf("deepseek %s: expected %d window, got %d", model, want, got)
		}
	}
}

// TestContextWindowForNewGen anchors the 2025-2026 generation windows: claude
// sonnet/opus 4.6+/5 and fable 5 = 1M, haiku 4.5 and legacy 3.x = 200K,
// gpt-5 family = 400K, gemini-3.x = 1M (models.dev, 2026-08).
func TestContextWindowForNewGen(t *testing.T) {
	cases := []struct {
		provider, model string
		want            int
	}{
		{"anthropic", "claude-sonnet-5", 1_000_000},
		{"anthropic", "claude-opus-5", 1_000_000},
		{"anthropic", "claude-fable-5", 1_000_000},
		{"anthropic", "claude-haiku-4-5", 200_000},
		{"anthropic", "claude-3-7-sonnet-20250219", 200_000},
		{"openai", "gpt-5", 400_000},
		{"openai", "gpt-5-mini", 400_000},
		{"openai", "gpt-5-nano", 400_000},
		{"openai", "gpt-4.1", 1_047_576},
		{"google", "gemini-3-flash-preview", 1_048_576},
		{"google", "gemini-3.1-pro-preview", 1_048_576},
		{"google", "gemini-3.5-flash", 1_048_576},
	}
	for _, c := range cases {
		if got := ContextWindowFor(AppConfig{}, c.provider, c.model); got != c.want {
			t.Errorf("%s %s: expected %d window, got %d", c.provider, c.model, c.want, got)
		}
	}
}

// TestContextWindowForOpenRouter anchors OpenRouter route caps (its own
// /v1/models, 2026-08): deepseek-r1 is capped at 64K there (not 128K),
// llama-3.3-70b-instruct = 131072, and the 2026 claude IDs are 1M.
func TestContextWindowForOpenRouter(t *testing.T) {
	cases := map[string]int{
		"deepseek/deepseek-r1":              64_000,
		"anthropic/claude-sonnet-5":         1_000_000,
		"meta-llama/llama-3.3-70b-instruct": 131_072,
	}
	for model, want := range cases {
		if got := ContextWindowFor(AppConfig{}, "openrouter", model); got != want {
			t.Errorf("openrouter %s: expected %d window, got %d", model, want, got)
		}
	}
}

// TestContextWindowForGroq anchors Groq's route windows (131072 for the
// llama-3.3-70b and deepseek-r1-distill routes, 2026-08).
func TestContextWindowForGroq(t *testing.T) {
	cases := map[string]int{
		"llama-3.3-70b-versatile":       131_072,
		"deepseek-r1-distill-llama-70b": 131_072,
	}
	for model, want := range cases {
		if got := ContextWindowFor(AppConfig{}, "groq", model); got != want {
			t.Errorf("groq %s: expected %d window, got %d", model, want, got)
		}
	}
}

// TestContextWindowForFamily anchors the family-prefix fallback: dated or
// unlisted model IDs still resolve to their generation's window instead of
// collapsing to the 128k default. The longest matching prefix wins, and
// completely unrelated IDs still return 0.
func TestContextWindowForFamily(t *testing.T) {
	cases := map[string]int{
		"claude-sonnet-5-20260908":      1_000_000,
		"claude-opus-5-20260908":        1_000_000,
		"claude-sonnet-4-6-20260801":    1_000_000,
		"claude-opus-4-5-20251101":      200_000,
		"claude-haiku-4-5-20261001":     200_000,
		"claude-3-7-sonnet-20250219-v2": 200_000,
		"gpt-5.4":                       1_050_000,
		"gpt-5.2-20260407":              400_000,
		"gpt-4.1-20250616":              1_047_576,
		"deepseek-v4-pro-20260724":      1_000_000,
		"deepseek/deepseek-r1-0528":     1_000_000,
		"gemini-3.9-flash":              1_048_576,
		"llama-3.3-70b-instruct":        131_072,
		"o3-mini-20260131":              200_000,
		"mimo-v2.5":                     1_048_576,
		"minimax-m3":                    1_048_576,
		"totally-unrelated-model":       0,
	}
	for model, want := range cases {
		if got := ContextWindowFor(AppConfig{}, "some-gateway", model); got != want {
			t.Errorf("family %s: expected %d window, got %d", model, want, got)
		}
	}
}

// TestContextWindowForLiveLimits proves the gateway's live context_length
// (cached by DiscoverModels) wins over the builtin table, capped at the
// researched window: poolside reports 262144 live even though the model is
// natively 1M, and a custom provider with no builtin entry uses its live value.
func TestContextWindowForLiveLimits(t *testing.T) {
	// Custom gateway: no builtin entry → live value is used directly.
	recordLiveContextLimits("test-live-gateway", map[string]int{"widget-1": 700_000})
	if got := ContextWindowFor(AppConfig{}, "test-live-gateway", "widget-1"); got != 700_000 {
		t.Errorf("expected live window 700000 for custom gateway, got %d", got)
	}
	// poolside: live reports 262144 (verified per-key deployment cap), which
	// is below the native 1M → live value wins.
	recordLiveContextLimits("poolside", map[string]int{"poolside/laguna-s-2.1": 262_144})
	if got := ContextWindowFor(AppConfig{}, "poolside", "poolside/laguna-s-2.1"); got != 262_144 {
		t.Errorf("expected live window 262144 for poolside laguna, got %d", got)
	}
}

// TestFetchOpenAIModelsDetailed proves the /models fetch also captures each
// model's context_length alongside the sorted deduplicated ID list.
func TestFetchOpenAIModelsDetailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"model-b","context_length":262144},{"id":"model-a","context_length":1048576},{"id":"model-a"}]}`))
	}))
	defer srv.Close()

	models, limits, err := FetchOpenAIModelsDetailed(srv.URL, "")
	if err != nil {
		t.Fatalf("FetchOpenAIModelsDetailed failed: %v", err)
	}
	if len(models) != 2 || models[0] != "model-a" || models[1] != "model-b" {
		t.Errorf("expected sorted deduped [model-a model-b], got %v", models)
	}
	if limits["model-a"] != 1_048_576 {
		t.Errorf("expected model-a limit 1048576, got %d", limits["model-a"])
	}
	if limits["model-b"] != 262_144 {
		t.Errorf("expected model-b limit 262144, got %d", limits["model-b"])
	}
}

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{512, "512"},
		{999, "999"},
		{1000, "1.0k"},
		{123443, "123.4k"},
		{128000, "128.0k"},
		{200000, "200.0k"},
		{262144, "262.1k"},
		{1048576, "1.0M"},
	}
	for _, c := range cases {
		if got := FormatTokens(c.n); got != c.want {
			t.Errorf("FormatTokens(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestCustomProviderConfigRoundTrip(t *testing.T) {
	cfg := AppConfig{
		Providers: map[string]CustomProviderConfig{
			"my-gateway": {
				Protocol: "openai-compatible",
				BaseURL:  "https://api.my-gateway.example/v1",
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

	p, ok := out.Providers["my-gateway"]
	if !ok {
		t.Fatalf("expected provider my-gateway in round-tripped config")
	}
	if p.BaseURL != "https://api.my-gateway.example/v1" || len(p.Models) != 2 {
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

// TestOpenAIStreamCompleteMidStreamCut simulates a free-tier gateway
// whose session/duration limit expires WHILE the answer is still
// streaming: content arrives, then the connection just closes with no
// finish_reason and no [DONE]. The adapter must return an error — not a
// silently truncated answer — so the engine can fall back to another
// provider instead of presenting half a response as complete.
func TestOpenAIStreamCompleteMidStreamCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"This is only"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":" half an answer"}}]}`+"\n\n")
		// No finish_reason, no [DONE] — connection dropped mid-generation.
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, "test-key")
	_, err := a.StreamComplete(context.Background(), CompletionRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for a stream cut before completion signal")
	}
	if !strings.Contains(err.Error(), "ended unexpectedly") {
		t.Errorf("expected 'ended unexpectedly' error, got %q", err)
	}
}

// TestOpenAIStreamCompleteFinishReasonNoDone proves a stream that ends with
// finish_reason but without a [DONE] frame (some providers omit it) is still
// accepted as complete — only the absence of BOTH signals counts as a cut.
func TestOpenAIStreamCompleteFinishReasonNoDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Done"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, "test-key")
	res, err := a.StreamComplete(context.Background(), CompletionRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if res.Content != "Done" || res.FinishReason != "stop" {
		t.Errorf("unexpected result: content=%q finish=%q", res.Content, res.FinishReason)
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

// TestOpenCodeImportBringsAuthKey proves an imported opencode.jsonc provider
// borrows its API key from opencode's credential store (auth.json) when the
// key is not inline in opencode.jsonc — so the imported provider can actually
// authenticate instead of failing with a 401.
func TestOpenCodeImportBringsAuthKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ocDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	ocJSON := `{
		"provider": {
			"mygw": {
				"options": {"baseURL": "https://gw.example/v1"},
				"models": {"m1": {"name": "M1"}}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(ocDir, "opencode.jsonc"), []byte(ocJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// opencode persists the key out-of-band in auth.json, never inline.
	authDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(authDir, 0755); err != nil {
		t.Fatal(err)
	}
	authJSON := `{"mygw": {"type": "api", "key": "oc-secret-key"}}`
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(authJSON), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	p, ok := cfg.Providers["mygw"]
	if !ok {
		t.Fatalf("expected mygw imported, got %+v", cfg.Providers)
	}
	if p.BaseURL != "https://gw.example/v1" {
		t.Errorf("base URL mismatch: %q", p.BaseURL)
	}
	if p.APIKey != "oc-secret-key" {
		t.Errorf("expected imported provider to borrow opencode auth key, got %q", p.APIKey)
	}
}
