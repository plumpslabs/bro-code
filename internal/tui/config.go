package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type AppConfig struct {
	Schema            string                       `json:"$schema,omitempty"`
	MCP               map[string]MCPConfig         `json:"mcp,omitempty"`
	DisabledProviders []string                     `json:"disabled_providers,omitempty"`
	Provider          map[string]CustomProviderCfg `json:"provider,omitempty"`
	// CompactTriggerPct is the fraction of the context window (0–1) that
	// triggers auto-compaction. Optional — when unset, the tuned default
	// (70%) is used. Example: 0.5 compacts at half the window.
	CompactTriggerPct float64 `json:"compact_trigger_pct,omitempty"`
}

type MCPConfig struct {
	Type    string   `json:"type"`
	Command []string `json:"command"`
	Enabled bool     `json:"enabled"`
}

type CustomProviderCfg struct {
	Name    string                 `json:"name"`
	Options CustomProviderOptions  `json:"options"`
	Models  map[string]CustomModel `json:"models"`
}

type CustomProviderOptions struct {
	BaseURL string            `json:"baseURL"`
	APIKey  string            `json:"apiKey,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type CustomModel struct {
	Name  string           `json:"name"`
	Limit CustomModelLimit `json:"limit"`
}

type CustomModelLimit struct {
	Context int `json:"context"`
	Output  int `json:"output,omitempty"`
}

// ConfigFile is the path to the main BroCode configuration.
func ConfigFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".brocode", "config.jsonc")
}

// Config cache — LoadAppConfig is called on the render hot path (ctxColor
// → compactTriggerPct), so a raw os.ReadFile per frame would waste disk I/O.
// Instead we validate with a cheap os.Stat (mtime + size): the file body is
// only re-read when it actually changed, so a freshly written config.jsonc
// is picked up immediately (e.g. from the web dashboard) while an unchanged
// config costs one stat per call, never a full disk read.
var (
	cfgCache     AppConfig
	cfgCachePath string
	cfgCacheMod  time.Time
	cfgCacheSize int64
	cfgCacheMiss bool // last load found no config file
)

// LoadAppConfig reads the configuration file (stat-validated cache).
func LoadAppConfig() AppConfig {
	path := ConfigFile()
	fi, err := os.Stat(path)
	exists := err == nil
	var mod time.Time
	var size int64
	if exists {
		mod = fi.ModTime()
		size = fi.Size()
	}
	if cfgCachePath == path && exists == !cfgCacheMiss && mod.Equal(cfgCacheMod) && size == cfgCacheSize {
		return cfgCache
	}
	cfgCachePath = path
	cfgCacheMiss = !exists
	cfgCacheMod = mod
	cfgCacheSize = size
	var cfg AppConfig
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.Provider == nil {
		cfg.Provider = make(map[string]CustomProviderCfg)
	}
	cfgCache = cfg
	return cfg
}
