package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// Shared HTTP clients for provider discovery (health pings, /models fetch):
// one Transport per purpose instead of a fresh pool per call.
var (
	httpClientHealth = &http.Client{Timeout: 1 * time.Second}
	httpClientModels = &http.Client{Timeout: 5 * time.Second}
)

// ProviderInfo describes a provider capability & configuration metadata.
type ProviderInfo struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Protocol       string   `json:"protocol"` // "openai-compatible" or "anthropic"
	APIKeyEnvVar   string   `json:"api_key_env_var"`
	DefaultBaseURL string   `json:"default_base_url"`
	DefaultModels  []string `json:"default_models"`
	// ContextLimits maps model ID → context window in tokens. Values are
	// research-backed (vendor docs, 2026): used as the fallback when the user
	// hasn't declared a per-model limit in their config. 0 = unknown → 128k.
	ContextLimits map[string]int `json:"context_limits,omitempty"`
	// ModelsPublic marks an OpenAI-compatible provider whose /models endpoint
	// needs no API key (an open local proxy, e.g. FreeBuff via Freebuff2API).
	// The live list is fetched unconditionally and is AUTHORITATIVE — models
	// the proxy does not serve are never offered in the picker.
	ModelsPublic bool `json:"models_public,omitempty"`
}

// builtinContextLimits are the researched context windows for the models each
// builtin provider lists by default (tokens). Values come from the provider's
// own docs / models.dev (the model DB opencode itself ships), 2026-08 — NOT
// third-party blogs. 0 = unknown → 128k default.
var builtinContextLimits = map[string]map[string]int{
	"opencode": {
		// Per-model free-tier windows from models.dev (the DB opencode itself
		// uses to serve these routes), 2026-08. The free tier does NOT cap
		// every model at 200K — longcat/nemotron-ultra serve their native 1M.
		"deepseek-v4-flash-free":      200_000,
		"hy3-free":                    190_000,
		"mimo-v2.5-free":              200_000,
		"laguna-s-2.1-free":           256_000,
		"ling-3.0-tiny-free":          262_144,
		"longcat-2.0-free":            1_000_000,
		"nemotron-3-ultra-free":       1_000_000,
		"nemotron-3.5-lightning-free": 262_144,
		"big-pickle":                  200_000,
	},
	"deepseek": {
		// deepseek-chat / deepseek-reasoner are now (2026-07-24) aliases of
		// deepseek-v4-flash and serve the same 1M window; deepseek-coder is
		// discontinued and no longer listed. Max output 384K for V4.
		"deepseek-chat":     1_000_000,
		"deepseek-reasoner": 1_000_000,
		"deepseek-v4-flash": 1_000_000,
		"deepseek-v4-pro":   1_000_000,
	},
	"poolside": {
		// VERIFIED HARD CAP (2026-08, live test with a padded request): the
		// inference.poolside.ai API and the 9router gateway both reject inputs
		// above 262,112 tokens for laguna-s-2.1 ("Input length ... exceeds the
		// maximum allowed input length of 262112 tokens") — even though the
		// model natively supports 1M per docs.poolside.ai. The /v1/models
		// context_length field (262144) matches the real per-key deployment
		// cap, so the API value wins over the native 1M claim: using 1M here
		// would let the context grow past what the API accepts and break
		// mid-turn. Requires the poolside/ prefix in the wire model ID (a bare
		// "laguna-s-2.1" returns 404 model-not-found).
		"poolside/laguna-s-2.1":  262_144,
		"poolside/laguna-xs-2.1": 262_144,
	},
	"anthropic": {
		// Legacy 3.x models keep their 200K window; the 2025-2026 generation
		// (sonnet 4.5+/4.6, opus 4.6+/5, fable 5) serves 1M / 128K output,
		// haiku 4.5 stays at 200K / 64K (models.dev + Anthropic docs, 2026-08).
		"claude-3-7-sonnet-20250219": 200_000,
		"claude-3-5-sonnet-20241022": 200_000,
		"claude-3-5-haiku-20241022":  200_000,
		"claude-sonnet-4-5":          1_000_000,
		"claude-sonnet-4-6":          1_000_000,
		"claude-sonnet-5":            1_000_000,
		"claude-opus-4-6":            1_000_000,
		"claude-opus-4-7":            1_000_000,
		"claude-opus-4-8":            1_000_000,
		"claude-opus-5":              1_000_000,
		"claude-fable-5":             1_000_000,
		"claude-haiku-4-5":           200_000,
	},
	"openai": {
		// gpt-5 family = 400K (gpt-5 / mini / nano all 400K); gpt-4.1 family
		// = ~1M; gpt-4o stays 128K; o-series reasoning = 200K. The gpt-5.4/5.5
		// tier lists 1,050,000 per models.dev.
		"gpt-4o":            128_000,
		"gpt-4o-mini":       128_000,
		"o3-mini":           200_000,
		"gpt-5":             400_000,
		"gpt-5-mini":        400_000,
		"gpt-5-nano":        400_000,
		"gpt-5.4":           1_050_000,
		"gpt-5.5-pro":       1_050_000,
		"gpt-4.1":           1_047_576,
		"gpt-4.1-mini":      1_047_576,
		"gpt-4.1-nano":      1_047_576,
		"o3":                200_000,
		"o4-mini":           200_000,
	},
	"openrouter": {
		// OpenRouter routes: deepseek-r1 is capped at 64K on OpenRouter (its
		// /v1/models reports 64000); claude-3.5-sonnet is gone — the 2026
		// generation claude IDs are all 1M; llama-3.3-70b-instruct = 131072.
		"deepseek/deepseek-r1":              64_000,
		"anthropic/claude-sonnet-5":         1_000_000,
		"anthropic/claude-opus-5":           1_000_000,
		"meta-llama/llama-3.3-70b-instruct": 131_072,
	},
	"groq": {
		"llama-3.3-70b-versatile":       131_072,
		"deepseek-r1-distill-llama-70b": 131_072,
	},
	"google": {
		"gemini-2.5-flash":            1_048_576,
		"gemini-2.5-pro":              1_048_576,
		"gemini-2.0-flash":            1_048_576,
		"gemini-3-flash-preview":      1_048_576,
		"gemini-3-pro-preview":        1_048_576,
		"gemini-3.1-flash":            1_048_576,
		"gemini-3.1-pro":              1_048_576,
		"gemini-3.5-flash":            1_048_576,
	}, "freebuff": {
		// Official FreeBuff caps read from the CodebuffAI source tree
		// (FREEBUFF_MODEL_CONTEXT_WINDOWS, 2026-08): MiniMax M3 is capped at
		// 512K on the FreeBuff free tier (native is 1M); models absent from
		// that table (MiMo, Gemini flash lite) fall back to their native 1M
		// window. Model IDs use the official wire IDs (mimo/ prefix, no date
		// suffix on minimax-m3).
		"minimax/minimax-m3":           524_288,
		"mimo/mimo-v2.5":               1_048_576,
		"mimo/mimo-v2.5-pro":           1_048_576,
		"google/gemini-2.5-flash-lite": 1_048_576,
	},
}

// FriendlyName returns the display name for a provider ID, falling back to
// the ID itself for custom providers. The UI uses this everywhere so BroCode
// brands itself as its own product — the underlying gateway tool is never
// shown in the terminal.
func FriendlyName(id string) string {
	for _, p := range BuiltinProviders {
		if p.ID == id {
			return p.Name
		}
	}
	return id
}

// BuiltinProviders maps all pre-registered LLM providers. Context limits come
// from builtinContextLimits (research-backed) unless overridden by the user's
// own config model_map.
var BuiltinProviders = []ProviderInfo{
	{
		ID:             "opencode",
		Name:           "BroCode Free Gateway",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "", // No key required
		DefaultBaseURL: "https://opencode.ai/zen/v1",
		DefaultModels:  OpenCodeFreeModels,
		ContextLimits:  builtinContextLimits["opencode"],
	},
	{
		ID:             "deepseek",
		Name:           "DeepSeek API",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "DEEPSEEK_API_KEY",
		DefaultBaseURL: "https://api.deepseek.com",
		DefaultModels: []string{
			"deepseek-chat",
			"deepseek-v4-flash",
			"deepseek-v4-pro",
		},
		ContextLimits: builtinContextLimits["deepseek"],
	},
	{
		ID:             "poolside",
		Name:           "Poolside / Laguna",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "POOLSIDE_API_KEY",
		DefaultBaseURL: "https://inference.poolside.ai/v1",
		DefaultModels: []string{
			"poolside/laguna-s-2.1",
			"poolside/laguna-xs-2.1",
		},
		ContextLimits: builtinContextLimits["poolside"],
	},
	{
		ID:             "anthropic",
		Name:           "Anthropic Claude",
		Protocol:       "anthropic",
		APIKeyEnvVar:   "ANTHROPIC_API_KEY",
		DefaultBaseURL: "https://api.anthropic.com",
		DefaultModels: []string{
			"claude-sonnet-5",
			"claude-opus-5",
			"claude-haiku-4-5",
			"claude-3-7-sonnet-20250219",
			"claude-3-5-sonnet-20241022",
			"claude-3-5-haiku-20241022",
		},
		ContextLimits: builtinContextLimits["anthropic"],
	},
	{
		ID:             "openai",
		Name:           "OpenAI",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "OPENAI_API_KEY",
		DefaultBaseURL: "https://api.openai.com/v1",
		DefaultModels: []string{
			"gpt-5",
			"gpt-5-mini",
			"gpt-5-nano",
			"gpt-4o",
			"gpt-4o-mini",
			"o3-mini",
		},
		ContextLimits: builtinContextLimits["openai"],
	},
	{
		ID:             "openrouter",
		Name:           "OpenRouter",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "OPENROUTER_API_KEY",
		DefaultBaseURL: "https://openrouter.ai/api/v1",
		DefaultModels: []string{
			"deepseek/deepseek-r1",
			"anthropic/claude-sonnet-5",
			"anthropic/claude-opus-5",
			"meta-llama/llama-3.3-70b-instruct",
		},
		ContextLimits: builtinContextLimits["openrouter"],
	},
	{
		ID:             "groq",
		Name:           "Groq",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "GROQ_API_KEY",
		DefaultBaseURL: "https://api.groq.com/openai/v1",
		DefaultModels: []string{
			"llama-3.3-70b-versatile",
			"deepseek-r1-distill-llama-70b",
		},
		ContextLimits: builtinContextLimits["groq"],
	},
	{
		ID:             "google",
		Name:           "Google Gemini",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "GEMINI_API_KEY",
		DefaultBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/",
		DefaultModels: []string{
			"gemini-2.5-flash",
			"gemini-2.5-pro",
			"gemini-2.0-flash",
			"gemini-3-flash-preview",
			"gemini-3-pro-preview",
		},
		ContextLimits: builtinContextLimits["google"],
	},
	{
		ID:             "ollama",
		Name:           "Ollama (Local)",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "",
		DefaultBaseURL: "http://localhost:11434/v1",
		DefaultModels: []string{
			"qwen2.5-coder",
			"deepseek-r1",
			"llama3.2",
		},
		// Ollama context depends on the local model file — no fixed limit;
		// falls back to the 128k default unless configured by the user.
	},
	{
		ID:             "freebuff",
		Name:           "FreeBuff (Free)",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "", // No env var: token auto-loaded from the FreeBuff CLI credentials file
		DefaultBaseURL: FreeBuffDefaultBaseURL,
		DefaultModels:  FreeBuffModels,
		ContextLimits:  builtinContextLimits["freebuff"],
		ModelsPublic:   true, // open local proxy: /models needs no key, live list wins
	},
}

// DetectedProvider contains provider metadata and resolved API key.
type DetectedProvider struct {
	Info   ProviderInfo
	APIKey string
}

// AutoDetect scans environment variables and configuration to find usable providers.
func AutoDetect(cfg AppConfig) []DetectedProvider {
	var detected []DetectedProvider
	seen := map[string]bool{}

	// 1. Built-in OpenCode gateway / CLI is always available as default
	// (skipped when BROCODE_NO_OPENCODE=1 for fully standalone operation).
	if OpenCodeImportEnabled() {
		opencodeInfo := BuiltinProviders[0]
		detected = append(detected, DetectedProvider{
			Info:   opencodeInfo,
			APIKey: "",
		})
		seen[opencodeInfo.ID] = true
	}

	// 2. Scan Custom Providers in AppConfig (including OpenCode opencode.jsonc auto-imported ones)
	for id, custom := range cfg.Providers {
		key := custom.APIKey
		if key == "" && custom.APIKeyEnv != "" {
			key = os.Getenv(custom.APIKeyEnv)
		}
		info := ProviderInfo{
			ID:             id,
			Name:           id + " (Custom)",
			Protocol:       custom.Protocol,
			APIKeyEnvVar:   custom.APIKeyEnv,
			DefaultBaseURL: custom.BaseURL,
			DefaultModels:  custom.Models,
		}
		if info.Protocol == "" {
			info.Protocol = "openai-compatible"
		}
		// A custom provider that omits its model list but matches a built-in
		// provider ID (e.g. "poolside" with only a base URL + key) inherits the
		// built-in's models and context limits — otherwise it would fall back to
		// the placeholder "default" model, every turn would fail on the primary
		// provider and silently route through the fallback gateway instead.
		if len(info.DefaultModels) == 0 {
			for _, p := range BuiltinProviders {
				if p.ID == id {
					info.DefaultModels = p.DefaultModels
					info.ContextLimits = p.ContextLimits
					break
				}
			}
		}
		detected = append(detected, DetectedProvider{
			Info:   info,
			APIKey: key,
		})
		seen[id] = true
	}

	// 3. Scan Built-in providers against environment variables
	for _, p := range BuiltinProviders {
		if seen[p.ID] {
			continue
		}
		key := ""
		if p.APIKeyEnvVar != "" {
			key = os.Getenv(p.APIKeyEnvVar)
		}
		if p.ID == "google" && key == "" {
			key = os.Getenv("GOOGLE_API_KEY")
		}

		if p.ID == "ollama" {
			// Healthcheck Ollama endpoint before auto-detecting it
			if isEndpointAlive("http://localhost:11434/v1/models") {
				detected = append(detected, DetectedProvider{
					Info:   p,
					APIKey: "",
				})
				seen[p.ID] = true
			}
		} else if p.ID == "freebuff" {
			// FreeBuff serves through the local Freebuff2API proxy (its backend
			// is not directly OpenAI-compatible, see freebuff.go). Detect only
			// when BOTH the proxy is alive AND the FreeBuff CLI is logged in on
			// this machine — so the provider never appears when port 8080 hosts
			// some unrelated service or the user has no FreeBuff account. The
			// token is only a presence signal; requests go to the proxy.
			if LoadFreeBuffToken() != "" && isEndpointAlive(FreeBuffDefaultBaseURL+"/models") {
				detected = append(detected, DetectedProvider{
					Info:   p,
					APIKey: "",
				})
				seen[p.ID] = true
			}
		} else if key != "" {
			detected = append(detected, DetectedProvider{
				Info:   p,
				APIKey: key,
			})
			seen[p.ID] = true
		}
	}

	return detected
}

func isEndpointAlive(url string) bool {
	resp, err := httpClientHealth.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ModelEntry maps provider ID and model ID for picker UI.
type ModelEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// DiscoverModels returns all models from detected providers (cached or freshly fetched).
func DiscoverModels(cfg AppConfig) map[string][]string {
	result := make(map[string][]string)

	detected := AutoDetect(cfg)
	for _, d := range detected {
		models := d.Info.DefaultModels
		if len(models) == 0 {
			models = []string{"default"}
		}
		// If key is present and provider supports /models, attempt fetch. The
		// fetched list is MERGED with the configured one (not a replacement):
		// configured models stay visible even when the gateway's live list
		// omits them or reports them under a different ID.
		// Fetch the live model list: keyed providers fetch when a key exists;
		// public (open-local-proxy) providers like FreeBuff fetch unconditionally.
		if (d.APIKey != "" || d.Info.ModelsPublic) && d.Info.Protocol == "openai-compatible" && d.Info.DefaultBaseURL != "" {
			fetched, liveLimits, err := FetchOpenAIModelsDetailed(d.Info.DefaultBaseURL, d.APIKey)
			if err == nil && len(fetched) > 0 {
				// Cache the gateway's reported context_length per model so
				// ContextWindowFor can prefer the live per-deployment cap
				// (e.g. poolside reports 262144 even though native is 1M).
				recordLiveContextLimits(d.Info.ID, liveLimits)
				if d.Info.ModelsPublic {
					// Open proxy: the live list is authoritative — the proxy
					// rejects any model not in it, so never offer dead models.
					models = fetched
				} else {
					models = mergeModelLists(models, fetched)
				}
			}
		}
		result[d.Info.ID] = models
	}

	// NOTE: the local opencode CLI binary is deliberately NOT spawned here.
	// The opencode provider's free models come from the built-in static list
	// (OpenCodeFreeModels), so starting BroCode never launches an external
	// process and never flashes its spinner/output on the terminal.

	return result
}

// mergeModelLists merges the live-fetched model list with the configured one.
// Configured models come first (they are the declared IDs) and are
// authoritative; a fetched model is appended only when no existing model
// shares its last path segment — so ps/poolside/laguna-s-2.1 and
// ps/laguna-s-2.1 count as the same model and are never listed twice.
func mergeModelLists(configured, fetched []string) []string {
	suffix := map[string]bool{}
	out := make([]string, 0, len(configured)+len(fetched))
	for _, m := range configured {
		out = append(out, m)
		suffix[lastSegment(m)] = true
	}
	for _, m := range fetched {
		if suffix[lastSegment(m)] {
			continue
		}
		out = append(out, m)
		suffix[lastSegment(m)] = true
	}
	return out
}

// lastSegment returns the final path segment of a model ID (the part after
// the last "/"), used for suffix-based deduplication.
func lastSegment(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
}

// ResolveModelID maps a possibly-stale saved model ID onto a real model in the
// provider's list. Exact match wins; otherwise a listed model that shares the
// last path segment ("laguna-s-2.1" → "poolside/laguna-s-2.1") is chosen so
// configs saved before an API added its vendor prefix keep working; otherwise
// the input is returned unchanged (unknown custom IDs still go through as-is).
func ResolveModelID(models []string, model string) string {
	if model == "" {
		return model
	}
	if slices.Contains(models, model) {
		return model
	}
	seg := lastSegment(model)
	if seg != "" {
		for _, m := range models {
			if lastSegment(m) == seg {
				return m
			}
		}
	}
	return model
}

// FetchOpenAIModels lists the models a gateway exposes via its OpenAI-compatible
// GET /models endpoint. Used to populate a custom provider's model list when
// the user didn't declare one.
func FetchOpenAIModels(baseURL, apiKey string) ([]string, error) {
	models, _, err := FetchOpenAIModelsDetailed(baseURL, apiKey)
	return models, err
}

// ModelInfo is one entry from a gateway's /models endpoint: the wire model ID
// plus its context_length when the gateway reports one (OpenAI-compatible
// gateways expose context_length per model; poolside uses it to report the
// real per-key deployment cap, which differs from the native model window).
type ModelInfo struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
}

// FetchOpenAIModelsDetailed lists the models a gateway exposes via its
// OpenAI-compatible GET /models endpoint, keeping each model's context_length.
// Returns the deduplicated sorted IDs plus a map of model ID → context_length
// for the entries that report one.
func FetchOpenAIModelsDetailed(baseURL, apiKey string) ([]string, map[string]int, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClientModels.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	var payload struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, err
	}

	var models []string
	limits := make(map[string]int)
	seen := map[string]bool{}
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
		if item.ContextLength > 0 {
			limits[id] = item.ContextLength
		}
	}
	sort.Strings(models)
	return models, limits, nil
}

// liveContextLimits caches the context_length values reported by a gateway's
// /models endpoint (keyed by provider ID → model ID). Filled in during
// DiscoverModels; ContextWindowFor consults it ahead of the static builtin
// table because the API's per-deployment cap is authoritative (poolside
// reports 262144 even though the model natively supports 1M).
var liveContextLimits = map[string]map[string]int{}

// recordLiveContextLimits stores per-model context_lengths fetched from a
// provider's /models endpoint. Empty/nil input is a no-op so callers can pass
// the fetch result unconditionally.
func recordLiveContextLimits(providerID string, limits map[string]int) {
	if len(limits) == 0 {
		return
	}
	if liveContextLimits[providerID] == nil {
		liveContextLimits[providerID] = make(map[string]int, len(limits))
	}
	for id, w := range limits {
		liveContextLimits[providerID][id] = w
	}
}
