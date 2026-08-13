package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// ProviderInfo describes a provider capability & configuration metadata.
type ProviderInfo struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Protocol       string   `json:"protocol"` // "openai-compatible" or "anthropic"
	APIKeyEnvVar   string   `json:"api_key_env_var"`
	DefaultBaseURL string   `json:"default_base_url"`
	DefaultModels  []string `json:"default_models"`
}

// BuiltinProviders maps all pre-registered LLM providers.
var BuiltinProviders = []ProviderInfo{
	{
		ID:             "opencode",
		Name:           "OpenCode CLI & Zen Gateway (Free)",
		Protocol:       "openai-compatible",
		APIKeyEnvVar:   "", // No key required
		DefaultBaseURL: "https://router.opencode.ai/v1",
		DefaultModels:  OpenCodeFreeModels,
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
	opencodeInfo := BuiltinProviders[0]
	detected = append(detected, DetectedProvider{
		Info:   opencodeInfo,
		APIKey: "",
	})
	seen[opencodeInfo.ID] = true

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
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(url)
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
		// If key is present and provider supports /models, attempt fetch
		if d.APIKey != "" && d.Info.Protocol == "openai-compatible" && d.Info.DefaultBaseURL != "" {
			fetched, err := fetchOpenAIModels(d.Info.DefaultBaseURL, d.APIKey)
			if err == nil && len(fetched) > 0 {
				models = fetched
			}
		}
		result[d.Info.ID] = models
	}

	// Try fetching models directly from local OpenCode CLI binary if installed
	if cliModels, err := fetchOpenCodeCLIModels(); err == nil && len(cliModels) > 0 {
		result["opencode"] = cliModels
	}

	return result
}

func fetchOpenCodeCLIModels() ([]string, error) {
	detected, binPath := DetectOpenCode()
	if !detected || binPath == "" {
		return nil, fmt.Errorf("opencode cli not found")
	}

	cmd := exec.Command(binPath, "models")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(out), "\n")
	var models []string
	seen := map[string]bool{}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// Clean up opencode/ or lalarasa/ prefix
		clean := l
		if strings.HasPrefix(clean, "opencode/") {
			clean = strings.TrimPrefix(clean, "opencode/")
		}
		if !seen[clean] {
			seen[clean] = true
			models = append(models, clean)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no models found")
	}
	sort.Strings(models)
	return models, nil
}

func fetchOpenAIModels(baseURL, apiKey string) ([]string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
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
