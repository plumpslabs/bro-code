package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// CustomProviderConfig represents custom user provider overrides.
type CustomProviderConfig struct {
	Protocol  string   `json:"protocol"`          // "openai-compatible" or "anthropic"
	BaseURL   string   `json:"base_url"`          // API endpoint base URL
	APIKeyEnv string   `json:"api_key_env"`       // Environment variable name for key
	APIKey    string   `json:"api_key,omitempty"` // Stored API key (0600 mode file only)
	Models    []string `json:"models,omitempty"`  // Pre-declared models
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
