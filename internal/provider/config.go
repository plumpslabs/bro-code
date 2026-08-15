package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ModelLimits mirrors opencode.jsonc's per-model "limit" block.
type ModelLimits struct {
	Context int `json:"context,omitempty"` // context window size in tokens
	Output  int `json:"output,omitempty"`  // max output tokens
	// Optional list prices in USD per million tokens. Override the built-in
	// price table when a custom provider/model bills differently.
	InputPrice  float64 `json:"input_price,omitempty"`  // USD per M input tokens
	OutputPrice float64 `json:"output_price,omitempty"` // USD per M output tokens
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

// GlobalConfigPath returns the user's global config file path (machine-written
// by the wizard).
func GlobalConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "brocode", "config.json")
}

// GlobalJSONCConfigPath returns the hand-editable global config path (BroCode's
// own format, JSONC — comments allowed). Overrides config.json when both exist.
func GlobalJSONCConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "brocode", "config.jsonc")
}

// ProjectConfigPath returns the current working directory's config file path
// (machine-written by the wizard).
func ProjectConfigPath() string {
	return filepath.Join(".brocode", "config.json")
}

// ProjectJSONCConfigPath returns the hand-editable project config path
// (BroCode's own format, JSONC — comments allowed). Overrides config.json when
// both exist.
func ProjectJSONCConfigPath() string {
	return filepath.Join(".brocode", "config.jsonc")
}

// OpenCodeConfigPath returns the local OpenCode config file path (~/.config/opencode/opencode.jsonc)
func OpenCodeConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "opencode", "opencode.jsonc")
}

// OpenCodeImportEnabled reports whether BroCode may borrow opencode's config
// (provider blocks + MCP servers) and auto-detect the opencode provider.
// BroCode has its own standalone configs (.brocode/, ~/.config/brocode/); the
// opencode import is only a convenience bridge. Set BROCODE_NO_OPENCODE=1 to
// run fully standalone: BroCode configs only, no opencode.jsonc import and no
// opencode provider auto-detection.
func OpenCodeImportEnabled() bool {
	v := strings.ToLower(os.Getenv("BROCODE_NO_OPENCODE"))
	return v != "1" && v != "true" && v != "yes"
}

// LoadConfig loads configuration from BroCode and OpenCode locations with
// merging. Precedence (highest wins): project BroCode → global BroCode →
// opencode.jsonc. BroCode's own config is authoritative; opencode.jsonc only
// fills gaps — a provider is imported only when BroCode configures NO provider
// with the same ID or the same base URL (so a duplicate like lalarasa vs a
// BroCode-configured gateway never shows up twice).
func LoadConfig() AppConfig {
	cfg := AppConfig{
		Providers: make(map[string]CustomProviderConfig),
	}

	// 1. BroCode's own global config. config.json is machine-written by the
	// wizard; config.jsonc is the hand-editable form (comments allowed, same
	// schema) and overrides config.json when both exist.
	cfg = mergeBroCodeConfig(cfg, GlobalConfigPath())
	cfg = mergeBroCodeConfig(cfg, GlobalJSONCConfigPath())

	// 2. BroCode's own project config — highest priority, same .json/.jsonc
	// pairing.
	cfg = mergeBroCodeConfig(cfg, ProjectConfigPath())
	cfg = mergeBroCodeConfig(cfg, ProjectJSONCConfigPath())

	// 3. Read OpenCode Config (~/.config/opencode/opencode.jsonc) LAST as a
	// gap-filler: imports never override BroCode's own providers and never
	// duplicate one that already targets the same base URL.
	if OpenCodeImportEnabled() {
		if data, err := os.ReadFile(OpenCodeConfigPath()); err == nil {
			cfg = mergeOpenCodeProviders(cfg, data)
		}
	}

	// Final pass: drop providers persisted as duplicates in an older config
	// file (e.g. an imported lalarasa saved alongside the keyed kahuna with
	// the same base URL) so they stop appearing even before the next save.
	cfg = dedupeProvidersByBaseURL(cfg)

	// Register any configured per-model prices so EstimateCostUSD reflects
	// custom provider billing instead of the built-in table.
	applyConfigPrices(cfg)
	return cfg
}

// applyConfigPrices registers InputPrice/OutputPrice overrides from the config
// into the package-level pricing table used by EstimateCostUSD.
func applyConfigPrices(cfg AppConfig) {
	for _, p := range cfg.Providers {
		for model, m := range p.ModelMap {
			if m.Limits.InputPrice > 0 || m.Limits.OutputPrice > 0 {
				RegisterModelPrice(model, m.Limits.InputPrice, m.Limits.OutputPrice)
			}
		}
	}
}

// mergeBroCodeConfig reads one BroCode config file (JSON or JSONC — comments
// tolerated) into cfg. Missing or invalid files are skipped silently.
func mergeBroCodeConfig(cfg AppConfig, path string) AppConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var c AppConfig
	if json.Unmarshal([]byte(stripJSONComments(string(data))), &c) != nil {
		return cfg
	}
	if c.DefaultProvider != "" {
		cfg.DefaultProvider = c.DefaultProvider
	}
	if c.DefaultModel != "" {
		cfg.DefaultModel = c.DefaultModel
	}
	for k, v := range c.Providers {
		cfg.Providers[k] = v
	}
	return cfg
}

// mergeOpenCodeProviders imports the provider blocks of an opencode.jsonc file
// into cfg. Only gap-filling imports are applied: a provider is skipped when
// BroCode already configures a provider with the same ID or pointing at the
// same base URL (its own config is authoritative).
func mergeOpenCodeProviders(cfg AppConfig, data []byte) AppConfig {
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
			// BroCode's own config wins: never overwrite an existing ID, and
			// never import a duplicate gateway (same base URL).
			if _, exists := cfg.Providers[pID]; exists {
				continue
			}
			if baseURLAlreadyConfigured(cfg, pData.Options.BaseURL) {
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

	return cfg
}

// baseURLAlreadyConfigured reports whether any BroCode-configured provider
// already targets the given base URL (trailing slashes ignored).
func baseURLAlreadyConfigured(cfg AppConfig, baseURL string) bool {
	target := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, p := range cfg.Providers {
		if strings.TrimRight(strings.TrimSpace(p.BaseURL), "/") == target {
			return true
		}
	}
	return false
}

// stripJSONComments removes // line comments and /* */ block comments from
// JSONC input. It is string-aware: // inside a string literal (e.g. the https://
// in a base URL) is preserved, so URLs are never truncated.
func stripJSONComments(input string) string {
	var sb strings.Builder
	inString := false
	escaped := false
	for i := 0; i < len(input); i++ {
		c := input[i]
		if inString {
			sb.WriteByte(c)
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			sb.WriteByte(c)
		case '/':
			if i+1 < len(input) && input[i+1] == '/' {
				// Line comment: skip to end of line, emit a newline.
				for i < len(input) && input[i] != '\n' {
					i++
				}
				sb.WriteByte('\n')
			} else if i+1 < len(input) && input[i+1] == '*' {
				// Block comment: skip to closing */.
				i += 2
				for i+1 < len(input) && !(input[i] == '*' && input[i+1] == '/') {
					i++
				}
				i++ // skip the closing '/'
			} else {
				sb.WriteByte(c)
			}
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// ContextWindowFor returns the real context window (in tokens) for a model,
// resolved in priority order:
//  1. The user-declared per-model "limit" in their config (the opencode.jsonc
//     style block the /connect wizard accepts) — highest priority.
//  2. The builtin research-backed table for builtin providers' default models
//     (e.g. opencode free models = 200K on the free tier, gemini = 1M, ...).
//
// Returns 0 when nothing is known — callers fall back to their default window.
func ContextWindowFor(cfg AppConfig, providerID, model string) int {
	// 1. User-declared limit wins.
	if cfg.Providers != nil {
		if p, ok := cfg.Providers[providerID]; ok {
			if cm, ok := p.ModelMap[model]; ok && cm.Limits.Context > 0 {
				return cm.Limits.Context
			}
		}
	}
	// 2. Builtin research-backed table.
	for _, bp := range BuiltinProviders {
		if bp.ID != providerID {
			continue
		}
		if w, ok := bp.ContextLimits[model]; ok {
			return w
		}
		break
	}
	return 0
}

// FormatTokens renders a token count compactly and readably for the UI:
// 512 → "512", 123443 → "123.4k", 1048576 → "1.0M".
func FormatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ParseModelJSON parses the models block entered in the custom-provider
// wizard. It accepts the opencode.jsonc shape (object keyed by model ID with
// name/limit) OR a plain JSON array of model ID strings, and returns the
// ordered model IDs plus the per-model detail map.
//
// It is also tolerant of a bare object body pasted without the wrapping
// braces (a very common copy-paste slip when taking the "models" block out of
// opencode.jsonc) — the body is wrapped in { } automatically.
func ParseModelJSON(input string) ([]string, map[string]CustomModel, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil, nil
	}

	// Tolerate a bare object body without the outer braces: if the input does
	// not start with { or [, try wrapping it — the wrapped form must parse as
	// an object for this to apply, otherwise we fall through to the error.
	if !strings.HasPrefix(input, "{") && !strings.HasPrefix(input, "[") {
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte("{"+input+"}"), &probe) == nil {
			input = "{" + input + "}"
		}
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

// SaveGlobalConfig saves config to global path (~/.config/brocode/config.json)
// safely. Duplicate providers (same base URL) are pruned first so imported
// opencode providers that were persisted by an older build self-clean on the
// next save.
func SaveGlobalConfig(cfg AppConfig) error {
	p := GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(dedupeProvidersByBaseURL(cfg), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0600)
}

// dedupeProvidersByBaseURL removes redundant providers that target the same
// base URL: when two share a base URL, the one WITHOUT an API key (typically a
// keyless opencode.jsonc import persisted next to a keyed BroCode provider
// like lalarasa next to kahuna) is dropped, keeping the keyed one. When ALL
// duplicates are keyless, only the alphabetically-first ID is kept. Providers
// with distinct base URLs are never touched.
func dedupeProvidersByBaseURL(cfg AppConfig) AppConfig {
	hasKey := func(p CustomProviderConfig) bool {
		return p.APIKey != "" || (p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "")
	}

	byURL := map[string][]string{}
	for id := range cfg.Providers {
		url := strings.TrimRight(strings.TrimSpace(cfg.Providers[id].BaseURL), "/")
		if url == "" {
			continue
		}
		byURL[url] = append(byURL[url], id)
	}

	drop := map[string]bool{}
	for _, ids := range byURL {
		if len(ids) < 2 {
			continue
		}
		keyed := 0
		for _, id := range ids {
			if hasKey(cfg.Providers[id]) {
				keyed++
			}
		}
		if keyed == len(ids) {
			continue // all carry keys — treat as intentional, keep all
		}
		if keyed > 0 {
			for _, id := range ids {
				if !hasKey(cfg.Providers[id]) {
					drop[id] = true // keyless duplicate of a keyed provider
				}
			}
		} else {
			sort.Strings(ids)
			for _, id := range ids[1:] {
				drop[id] = true // all keyless: keep the alphabetically-first only
			}
		}
	}

	for id := range drop {
		delete(cfg.Providers, id)
	}
	return cfg
}
