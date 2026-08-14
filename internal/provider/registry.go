package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
}

// builtinContextLimits are the researched context windows for the models each
// builtin provider lists by default (tokens).
var builtinContextLimits = map[string]map[string]int{
	"opencode": {
		// OpenCode Zen free tier caps every free model at 200K even when the
		// native model supports more (e.g. deepseek-v4-flash-free is 1M
		// natively but 200K on the free tier).
		"deepseek-v4-flash-free":      200_000,
		"hy3-free":                    200_000,
		"mimo-v2.5-free":              200_000,
		"laguna-s-2.1-free":           200_000,
		"ling-3.0-tiny-free":          200_000,
		"longcat-2.0-free":            200_000,
		"nemotron-3-ultra-free":       200_000,
		"nemotron-3.5-lightning-free": 200_000,
		"big-pickle":                  200_000,
	},
	"deepseek": {
		"deepseek-chat":     128_000,
		"deepseek-coder":    128_000,
		"deepseek-reasoner": 128_000,
	},
	"poolside": {
		"laguna-s-2.1":   1_048_576,
		"poolside-coder": 1_048_576,
	},
	"anthropic": {
		"claude-3-7-sonnet-20250219": 200_000,
		"claude-3-5-sonnet-20241022": 200_000,
		"claude-3-5-haiku-20241022":  200_000,
	},
	"openai": {
		"gpt-4o":      128_000,
		"gpt-4o-mini": 128_000,
		"o3-mini":     200_000,
	},
	"openrouter": {
		"deepseek/deepseek-r1":              128_000,
		"anthropic/claude-3.5-sonnet":       200_000,
		"meta-llama/llama-3.3-70b-instruct": 128_000,
	},
	"groq": {
		"llama-3.3-70b-versatile":       128_000,
		"deepseek-r1-distill-llama-70b": 128_000,
	},
	"google": {
		"gemini-2.5-flash": 1_048_576,
		"gemini-2.5-pro":   1_048_576,
		"gemini-2.0-flash": 1_048_576,
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
		DefaultBaseURL: "https://router.opencode.ai/v1",
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
			"deepseek-coder",
			"deepseek-reasoner",
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
			"laguna-s-2.1",
			"poolside-coder",
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
			"anthropic/claude-3.5-sonnet",
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
		if d.APIKey != "" && d.Info.Protocol == "openai-compatible" && d.Info.DefaultBaseURL != "" {
			fetched, err := FetchOpenAIModels(d.Info.DefaultBaseURL, d.APIKey)
			if err == nil && len(fetched) > 0 {
				models = mergeModelLists(models, fetched)
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

// FetchOpenAIModels lists the models a gateway exposes via its OpenAI-compatible
// GET /models endpoint. Used to populate a custom provider's model list when
// the user didn't declare one.
func FetchOpenAIModels(baseURL, apiKey string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClientModels.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	var models []string
	seen := map[string]bool{}
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			models = append(models, id)
		}
	}
	sort.Strings(models)
	return models, nil
}
