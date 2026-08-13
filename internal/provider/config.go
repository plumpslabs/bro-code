package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ModelLimits mirrors opencode.jsonc's per-model "limit" block.
type ModelLimits struct {
	Context int `json:"context,omitempty"` // context window size in tokens
	Output  int `json:"output,omitempty"`  // max output tokens
}

// CustomModel describes a declared model with optional display name and limits.
type CustomModel struct {
	Name   string      `json:"name,omitempty"`
	Limits ModelLimits `json:"limit,omitempty"`
}

// CustomProviderConfig represents custom user provider overrides.
type CustomProviderConfig struct {
	Protocol  string                 `json:"protocol"`            // "openai-compatible" or "anthropic"
	BaseURL   string                 `json:"base_url"`            // API endpoint base URL
	APIKeyEnv string                 `json:"api_key_env"`         // Environment variable name for key
	APIKey    string                 `json:"api_key,omitempty"`   // Stored API key (0600 mode file only)
	Models    []string               `json:"models,omitempty"`    // Pre-declared model IDs
	ModelMap  map[string]CustomModel `json:"model_map,omitempty"` // Model ID → name/limits details
}

// AppConfig represents global and local settings for BroCode.
type AppConfig struct {
	DefaultProvider string                          `json:"default_provider,omitempty"`
	DefaultModel    string                          `json:"default_model,omitempty"`
	Providers       map[string]CustomProviderConfig `json:"providers,omitempty"`
}

// GlobalConfigPath returns the user's global config file path.
func GlobalConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "brocode", "config.json")
}

// ProjectConfigPath returns the current working directory's config file path.
func ProjectConfigPath() string {
	return filepath.Join(".brocode", "config.json")
}

// OpenCodeConfigPath returns the local OpenCode config file path (~/.config/opencode/opencode.jsonc)
func OpenCodeConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "opencode", "opencode.jsonc")
}

// LoadConfig loads configuration from BroCode and OpenCode locations with merging.
func LoadConfig() AppConfig {
	cfg := AppConfig{
		Providers: make(map[string]CustomProviderConfig),
	}

	// 1. Read Global BroCode Config (~/.config/brocode/config.json)
	if data, err := os.ReadFile(GlobalConfigPath()); err == nil {
		var globalCfg AppConfig
		if json.Unmarshal(data, &globalCfg) == nil {
			if globalCfg.DefaultProvider != "" {
				cfg.DefaultProvider = globalCfg.DefaultProvider
			}
			if globalCfg.DefaultModel != "" {
				cfg.DefaultModel = globalCfg.DefaultModel
			}
			for k, v := range globalCfg.Providers {
				cfg.Providers[k] = v
			}
		}
	}

	// 2. Read Project BroCode Config (.brocode/config.json)
	if data, err := os.ReadFile(ProjectConfigPath()); err == nil {
		var projectCfg AppConfig
		if json.Unmarshal(data, &projectCfg) == nil {
			if projectCfg.DefaultProvider != "" {
				cfg.DefaultProvider = projectCfg.DefaultProvider
			}
			if projectCfg.DefaultModel != "" {
				cfg.DefaultModel = projectCfg.DefaultModel
			}
			for k, v := range projectCfg.Providers {
				cfg.Providers[k] = v
			}
		}
	}

	// 3. Read OpenCode Config (~/.config/opencode/opencode.jsonc)
	if data, err := os.ReadFile(OpenCodeConfigPath()); err == nil {
		cleanJSON := stripJSONComments(string(data))
		var openCodeCfg struct {
			Provider map[string]struct {
				Name    string `json:"name"`
				Options struct {
					BaseURL string `json:"baseURL"`
					APIKey  string `json:"apiKey"`
				} `json:"options"`
				Models map[string]struct {
					Name string `json:"name"`
				} `json:"models"`
			} `json:"provider"`
		}

		if json.Unmarshal([]byte(cleanJSON), &openCodeCfg) == nil {
			for pID, pData := range openCodeCfg.Provider {
				if pData.Options.BaseURL == "" {
					continue
				}
				var modelIDs []string
				for mID := range pData.Models {
					modelIDs = append(modelIDs, mID)
				}
				cfg.Providers[pID] = CustomProviderConfig{
					Protocol: "openai-compatible",
					BaseURL:  pData.Options.BaseURL,
					APIKey:   pData.Options.APIKey,
					Models:   modelIDs,
				}
			}
		}
	}

	return cfg
}

func stripJSONComments(input string) string {
	lines := strings.Split(input, "\n")
	var clean []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if idx := strings.Index(l, "//"); idx >= 0 {
			l = l[:idx]
		}
		clean = append(clean, l)
	}
	return strings.Join(clean, "\n")
}

// ParseModelJSON parses the models block entered in the custom-provider
// wizard. It accepts the opencode.jsonc shape (object keyed by model ID with
// name/limit) OR a plain JSON array of model ID strings, and returns the
// ordered model IDs plus the per-model detail map.
func ParseModelJSON(input string) ([]string, map[string]CustomModel, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil, nil
	}

	// Array form: ["model-a", "model-b"]
	var ids []string
	if err := json.Unmarshal([]byte(input), &ids); err == nil {
		var clean []string
		for _, id := range ids {
			if id = strings.TrimSpace(id); id != "" {
				clean = append(clean, id)
			}
		}
		if len(clean) == 0 {
			return nil, nil, fmt.Errorf("models array is empty")
		}
		return clean, nil, nil
	}

	// Object form: {"model-a": {"name": "...", "limit": {"context": .., "output": ..}}}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &obj); err != nil {
		return nil, nil, fmt.Errorf("invalid models JSON (expected an object or array of model IDs): %v", err)
	}

	models := make([]string, 0, len(obj))
	detail := make(map[string]CustomModel, len(obj))
	for id, raw := range obj {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		models = append(models, id)

		// Value may be a plain string (model name) or an object.
		var name string
		if json.Unmarshal(raw, &name) == nil && name != "" {
			detail[id] = CustomModel{Name: name}
			continue
		}
		var cm CustomModel
		if err := json.Unmarshal(raw, &cm); err != nil {
			// Tolerate unknown/malformed entries; still register the ID.
			detail[id] = CustomModel{}
			continue
		}
		detail[id] = cm
	}

	if len(models) == 0 {
		return nil, nil, fmt.Errorf("models object is empty")
	}
	return models, detail, nil
}

// SaveGlobalConfig saves config to global path (~/.config/brocode/config.json) safely.
func SaveGlobalConfig(cfg AppConfig) error {
	p := GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0600)
}
