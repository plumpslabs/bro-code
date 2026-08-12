package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// modelCache is the on-disk cache structure for discovered models.
type modelCache struct {
	Models    map[string][]string `json:"models"`     // provider → model IDs
	FetchedAt time.Time           `json:"fetched_at"` // when the cache was written
}

// modelCacheTTL controls how long cached models are trusted before refresh.
// Models rarely change; 24h keeps the picker live without hammering APIs.
const modelCacheTTL = 24 * time.Hour

// modelCacheFile returns the path to the cached model list.
func modelCacheFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".brocode", "models_cache.json")
}

// loadModelCache reads the cached model list from disk.
func loadModelCache() modelCache {
	var cache modelCache
	data, err := os.ReadFile(modelCacheFile())
	if err != nil {
		return cache
	}
	_ = json.Unmarshal(data, &cache)
	return cache
}

// saveModelCache writes the model list to disk.
func saveModelCache(cache modelCache) error {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(modelCacheFile()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(modelCacheFile(), data, 0o600)
}

// isCacheFresh reports whether the cache is still within TTL.
func isCacheFresh(cache modelCache) bool {
	return !cache.FetchedAt.IsZero() && time.Since(cache.FetchedAt) < modelCacheTTL
}

// discoverModelsFromAPI fetches models from an OpenAI-compatible /v1/models endpoint.
// Works for: OpenAI, Groq, DeepSeek, and any OpenAI-compatible server.
func discoverModelsFromAPI(baseURL, apiKeyHeader, apiKey string) ([]string, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("no API key provided")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, baseURL)
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

	seen := map[string]bool{}
	var models []string
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}

// discoverGeminiModels fetches models from Google Gemini API.
func discoverGeminiModels(apiKey string) ([]string, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("no API key provided")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models?key=%s", apiKey)

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from Gemini API", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Models []struct {
			Name              string   `json:"name"`
			DisplayName       string   `json:"displayName"`
			GenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var models []string
	for _, m := range payload.Models {
		// Only include models that support generateContent (chat)
		supportsChat := false
		for _, method := range m.GenerationMethods {
			if method == "generateContent" {
				supportsChat = true
				break
			}
		}
		if !supportsChat {
			continue
		}

		// Strip "models/" prefix for clean IDs
		id := strings.TrimPrefix(m.Name, "models/")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}

// discoverAnthropicModels fetches models from Anthropic API.
func discoverAnthropicModels(apiKey string) ([]string, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("no API key provided")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", "https://api.anthropic.com/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from Anthropic API", resp.StatusCode)
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

	seen := map[string]bool{}
	var models []string
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}

// DiscoverAllModels fetches models from all configured providers.
// Returns a map of provider → model IDs. Providers without credentials are skipped.
func DiscoverAllModels() map[string][]string {
	cache := loadModelCache()
	if isCacheFresh(cache) && len(cache.Models) > 0 {
		return cache.Models
	}

	models := make(map[string][]string)

	// OpenCode Zen gateway (free, no key needed)
	if zenModels, err := fetchZenModels(zenModelsEndpoint); err == nil && len(zenModels) > 0 {
		models["opencode"] = zenModels
	} else {
		models["opencode"] = openCodeFreeModels // fallback to static
	}

	// Groq (OpenAI-compatible)
	if groqKey := loadAPIKey("groq"); groqKey != "" {
		if groqModels, err := discoverModelsFromAPI("https://api.groq.com", "Authorization", groqKey); err == nil && len(groqModels) > 0 {
			models["groq"] = groqModels
		}
	}
	if len(models["groq"]) == 0 {
		models["groq"] = groqModels // fallback to static
	}

	// Poolside (OpenAI-compatible)
	if poolKey := loadAPIKey("poolside"); poolKey != "" {
		if poolModels, err := discoverModelsFromAPI("https://inference.poolside.ai", "Authorization", poolKey); err == nil && len(poolModels) > 0 {
			models["poolside"] = poolModels
		}
	}
	if len(models["poolside"]) == 0 {
		models["poolside"] = poolsideModels // fallback to static
	}


	// DeepSeek (OpenAI-compatible)
	if dsKey := loadAPIKey("deepseek"); dsKey != "" {
		if dsModels, err := discoverModelsFromAPI("https://api.deepseek.com", "Authorization", dsKey); err == nil && len(dsModels) > 0 {
			models["deepseek"] = dsModels
		}
	}
	if len(models["deepseek"]) == 0 {
		models["deepseek"] = deepseekStaticModels // fallback to static
	}

	// Anthropic
	if antKey := loadAPIKey("anthropic"); antKey != "" {
		if antModels, err := discoverAnthropicModels(antKey); err == nil && len(antModels) > 0 {
			models["anthropic"] = antModels
		}
	}
	if len(models["anthropic"]) == 0 {
		models["claude"] = claudeStaticModels // fallback to static (note: provider name mismatch handled in allModelEntries)
	}

	// Google Gemini (for Antigravity)
	if gemKey := loadAPIKey("gemini"); gemKey != "" {
		if gemModels, err := discoverGeminiModels(gemKey); err == nil && len(gemModels) > 0 {
			models["antigravity"] = gemModels
		}
	}
	if len(models["antigravity"]) == 0 {
		models["antigravity"] = antigravityStaticModels // fallback to static
	}

	// MiniMax (OpenAI-compatible)
	if mmKey := loadAPIKey("minimax"); mmKey != "" {
		if mmModels, err := discoverModelsFromAPI("https://api.minimax.io", "Authorization", mmKey); err == nil && len(mmModels) > 0 {
			models["minimax"] = mmModels
		}
	}
	if len(models["minimax"]) == 0 {
		models["minimax"] = minimaxModels // fallback to static
	}

	// Zhipu/GLM (OpenAI-compatible)
	if zpKey := loadAPIKey("zhipu"); zpKey != "" {
		if zpModels, err := discoverModelsFromAPI("https://api.z.ai/api/paas/v4", "Authorization", zpKey); err == nil && len(zpModels) > 0 {
			models["zhipu"] = zpModels
		}
	}
	if len(models["zhipu"]) == 0 {
		models["zhipu"] = zhipuModels // fallback to static
	}

	// MiMo (Xiaomi, OpenAI-compatible)
	if mmKey := loadAPIKey("mimo"); mmKey != "" {
		if mmModels, err := discoverModelsFromAPI("https://api.xiaomimimo.com", "Authorization", mmKey); err == nil && len(mmModels) > 0 {
			models["mimo"] = mmModels
		}
	}
	if len(models["mimo"]) == 0 {
		models["mimo"] = mimoModels // fallback to static
	}

	// Freebuff (fetch from GitHub source)
	if fbModels, err := discoverFreebuffModels(); err == nil && len(fbModels) > 0 {
		models["freebuff"] = fbModels
	} else {
		models["freebuff"] = freebuffNativeModels // fallback to static
	}

	// Codebuff (same models as Freebuff)
	models["codebuff"] = models["freebuff"]

	// Save cache
	cache = modelCache{
		Models:    models,
		FetchedAt: time.Now(),
	}
	_ = saveModelCache(cache)

	return models
}

// discoverFreebuffModels fetches available models from Freebuff's GitHub source.
// Freebuff doesn't have a /models endpoint, so we fetch from the source constants.
func discoverFreebuffModels() ([]string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	url := "https://raw.githubusercontent.com/CodebuffAI/codebuff/main/common/src/constants/free-agents.ts"

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from Freebuff source", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Parse model IDs from the TypeScript source
	// Look for patterns like: model: "provider/model-name"
	seen := map[string]bool{}
	var models []string

	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		// Match model definitions
		if strings.Contains(line, "model:") {
			// Extract model ID between quotes
			start := strings.Index(line, "\"")
			if start >= 0 {
				end := strings.Index(line[start+1:], "\"")
				if end >= 0 {
					modelID := line[start+1 : start+1+end]
					// Clean up model ID (remove provider prefix if present)
					if idx := strings.Index(modelID, "/"); idx >= 0 {
						modelID = modelID[idx+1:]
					}
					if modelID != "" && !seen[modelID] {
						seen[modelID] = true
						models = append(models, modelID)
					}
				}
			}
		}
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("no models found in Freebuff source")
	}

	sort.Strings(models)
	return models, nil
}

// DiscoverProviderModels fetches models for a specific provider.
// Returns the model list and whether the fetch was successful.
func DiscoverProviderModels(provider string) ([]string, bool) {
	switch provider {
	case "opencode":
		if models, err := fetchZenModels(zenModelsEndpoint); err == nil && len(models) > 0 {
			return models, true
		}
		return openCodeFreeModels, false

	case "groq":
		if key := loadAPIKey("groq"); key != "" {
			if models, err := discoverModelsFromAPI("https://api.groq.com", "Authorization", key); err == nil && len(models) > 0 {
				return models, true
			}
		}
		return groqModels, false

	case "deepseek":
		if key := loadAPIKey("deepseek"); key != "" {
			if models, err := discoverModelsFromAPI("https://api.deepseek.com", "Authorization", key); err == nil && len(models) > 0 {
				return models, true
			}
		}
		return deepseekStaticModels, false

	case "claude", "anthropic":
		if key := loadAPIKey("anthropic"); key != "" {
			if models, err := discoverAnthropicModels(key); err == nil && len(models) > 0 {
				return models, true
			}
		}
		return claudeStaticModels, false

	case "antigravity":
		if key := loadAPIKey("gemini"); key != "" {
			if models, err := discoverGeminiModels(key); err == nil && len(models) > 0 {
				return models, true
			}
		}
		return antigravityStaticModels, false

	case "minimax":
		if key := loadAPIKey("minimax"); key != "" {
			if models, err := discoverModelsFromAPI("https://api.minimax.io", "Authorization", key); err == nil && len(models) > 0 {
				return models, true
			}
		}
		return minimaxModels, false

	case "zhipu":
		if key := loadAPIKey("zhipu"); key != "" {
			if models, err := discoverModelsFromAPI("https://api.z.ai/api/paas/v4", "Authorization", key); err == nil && len(models) > 0 {
				return models, true
			}
		}
		return zhipuModels, false

	case "mimo":
		if key := loadAPIKey("mimo"); key != "" {
			if models, err := discoverModelsFromAPI("https://api.xiaomimimo.com", "Authorization", key); err == nil && len(models) > 0 {
				return models, true
			}
		}
		return mimoModels, false

	case "freebuff":
		if models, err := discoverFreebuffModels(); err == nil && len(models) > 0 {
			return models, true
		}
		return freebuffNativeModels, false

	case "codebuff":
		if models, err := discoverFreebuffModels(); err == nil && len(models) > 0 {
			return models, true
		}
		return codebuffNativeModels, false

	default:
		return nil, false
	}
}

// Static fallback models (used when API is unavailable or no credentials)
var (
	deepseekStaticModels = []string{
		"deepseek-chat",
		"deepseek-coder",
		"deepseek-reasoner",
	}

	claudeStaticModels = []string{
		"claude-sonnet-4-20250514",
		"claude-haiku-4-20250414",
		"claude-opus-4-20250514",
	}

	antigravityStaticModels = []string{
		"gemini-2.5-flash",
		"gemini-2.5-pro",
		"gemini-2.0-flash",
	}

	minimaxStaticModels = []string{
		"MiniMax-M3",
		"MiniMax-M2.7",
		"MiniMax-M2.7-highspeed",
	}

	zhipuStaticModels = []string{
		"glm-5.2",
		"glm-5.1",
		"glm-5-turbo",
		"glm-4.7",
	}

	mimoStaticModels = []string{
		"mimo-v2.5-pro",
		"mimo-v2.5",
	}
)
