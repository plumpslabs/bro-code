package provider

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
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
	Limits ModelLimits `json:"limit"`
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
	// FallbackPolicy controls automatic model routing when the primary fails:
	// "auto" (default), "confirm" (ask before cross-vendor fallback), or
	// "primary_only" (never fall back).
	FallbackPolicy string `json:"fallback_policy,omitempty"`
	SearchKey      string `json:"search_key,omitempty"`      // Tavily or Exa API key
	SearchProvider string `json:"search_provider,omitempty"` // "tavily" or "exa"
	Context7Key    string `json:"context7_key,omitempty"`    // Context7 Documentation API key
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

// opencodeDirs returns opencode's config and data directories, following the
// same platform conventions opencode itself uses: XDG-style ~/.config and
// ~/.local/share on Unix (including macOS), and %APPDATA% / %LOCALAPPDATA% on
// Windows. This keeps the import bridge working cross-platform.
func opencodeDirs() (configDir, dataDir string) {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		return os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA")
	}
	return filepath.Join(home, ".config", "opencode"), filepath.Join(home, ".local", "share", "opencode")
}

// OpenCodeConfigPath returns the local OpenCode config file path (~/.config/opencode/opencode.jsonc)
func OpenCodeConfigPath() string {
	cfg, _ := opencodeDirs()
	return filepath.Join(cfg, "opencode.jsonc")
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
	if c.FallbackPolicy != "" {
		cfg.FallbackPolicy = c.FallbackPolicy
	}
	if c.SearchKey != "" {
		cfg.SearchKey = c.SearchKey
	}
	if c.SearchProvider != "" {
		cfg.SearchProvider = c.SearchProvider
	}
	if c.Context7Key != "" {
		cfg.Context7Key = c.Context7Key
	}
	maps.Copy(cfg.Providers, c.Providers)
	return cfg
}

// mergeOpenCodeProviders imports the provider blocks of an opencode.jsonc file
// into cfg. Only gap-filling imports are applied: a provider is skipped when
// BroCode already configures a provider with the same ID or pointing at the
// same base URL (its own config is authoritative).
//
// API keys are borrowed from opencode's own credential store
// (~/.local/share/opencode/auth.json), which is where opencode persists them —
// never inline in opencode.jsonc. This lets imported providers authenticate
// without the user re-entering a key ("numpang" opencode's models).
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

	authKeys := readOpenCodeAuthKeys(OpenCodeAuthPath())

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
			// Prefer an inline key; otherwise borrow opencode's stored key so
			// the imported provider can actually authenticate.
			key := pData.Options.APIKey
			if key == "" {
				key = authKeys[pID]
			}
			cfg.Providers[pID] = CustomProviderConfig{
				Protocol: "openai-compatible",
				BaseURL:  pData.Options.BaseURL,
				APIKey:   key,
				Models:   modelIDs,
			}
		}
	}

	return cfg
}

// OpenCodeAuthPath returns opencode's credential store path
// (~/.local/share/opencode/auth.json on Unix, %LOCALAPPDATA%/opencode/auth.json
// on Windows), keyed by provider ID.
func OpenCodeAuthPath() string {
	_, data := opencodeDirs()
	return filepath.Join(data, "auth.json")
}

// readOpenCodeAuthKeys parses opencode's auth.json and returns a map of
// provider ID → API key. A missing or malformed file yields an empty map. The
// keys are opencode's own credentials; they are only ever reused to call the
// same upstream gateway and are never logged.
func readOpenCodeAuthKeys(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var auth map[string]struct {
		Key string `json:"key"`
	}
	if json.Unmarshal([]byte(stripJSONComments(string(data))), &auth) != nil {
		return out
	}
	for id, e := range auth {
		if e.Key != "" {
			out[id] = e.Key
		}
	}
	return out
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
			switch c {
			case '\\':
				escaped = true
			case '"':
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
				for i+1 < len(input) && (input[i] != '*' || input[i+1] != '/') {
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

// familyContextLimits maps a model-ID prefix to a context window for models
// that are not in the exact builtin table (e.g. a dated model ID such as
// "claude-sonnet-4-6-20260801" that isn't pinned verbatim). Prefixes are
// matched against the model ID; the longest matching prefix wins so a dated
// claude-sonnet-5 ID resolves to 1M (its 4.x-era siblings that cap at 200K
// never share the same prefix). Values follow the same 2026-08 research:
// claude sonnet/opus 4.5+ = 1M, claude 3.x & haiku 4.5 = 200K, gpt-5 family
// = 400K, gpt-4.1 family = 1M, deepseek = 1M, gemini = 1M, llama-3.3 = 131K.
var familyContextLimits = []struct {
	prefix string
	window int
}{
	// Anthropic Claude. Order matters: sonnet/opus 4.6+/5 and fable 5 (1M)
	// must be checked before the generic "claude-" 200K fallback, and the
	// "claude-3-" legacy family must win over it too.
	{"claude-sonnet-4-5", 1_000_000},
	{"claude-sonnet-4-6", 1_000_000},
	{"claude-sonnet-5", 1_000_000},
	{"claude-opus-4-5", 200_000},
	{"claude-opus-4-6", 1_000_000},
	{"claude-opus-4-7", 1_000_000},
	{"claude-opus-4-8", 1_000_000},
	{"claude-opus-5", 1_000_000},
	{"claude-fable-5", 1_000_000},
	{"claude-haiku-4-5", 200_000},
	{"claude-3-", 200_000},
	{"claude-", 200_000},
	// OpenAI. gpt-4.1 family and gpt-5.x are 400K-1M; gpt-4o stays 128K.
	{"gpt-5.5", 1_050_000},
	{"gpt-5.4", 1_050_000},
	{"gpt-5", 400_000},
	{"gpt-4.1", 1_047_576},
	{"gpt-4o", 128_000},
	{"gpt-4", 128_000},
	{"o4-", 200_000},
	{"o3-", 200_000},
	{"o1-", 200_000},
	// DeepSeek: chat/reasoner are v4-flash aliases (1M) since 2026-07.
	{"deepseek-", 1_000_000},
	{"deepseek/", 1_000_000},
	// Google Gemini: every generation serves the 1M window.
	{"gemini-", 1_048_576},
	// NVIDIA Nemotron 3 / 3.5: 1M for the -ultra/-super tiers, 256K for 3.5.
	{"nemotron-3.5", 262_144},
	{"nemotron-3-ultra", 1_000_000},
	{"nemotron-3", 262_144},
	// Meta Llama 3.3 (Groq/OpenRouter) = 131072.
	{"llama-3.3", 131_072},
	{"llama-3.2", 131_072},
	// Qwen coder / general-purpose = 131K.
	{"qwen2.5-coder", 131_072},
	{"qwen", 131_072},
	// Free gateway / Chinese open models: MiMo & MiniMax = 1M natively. Both
	// bare (mimo-v2.5) and FreeBuff wire (mimo/mimo-v2.5) ID forms match.
	{"mimo-", 1_048_576},
	{"mimo/", 1_048_576},
	{"minimax-", 1_048_576},
	{"minimax/", 1_048_576},
}

// familyContextWindow returns the context window for a model ID based on its
// known model family prefix, or 0 when no family rule matches.
func familyContextWindow(model string) int {
	for _, rule := range familyContextLimits {
		if strings.HasPrefix(model, rule.prefix) {
			return rule.window
		}
	}
	return 0
}

// ContextWindowFor returns the real context window (in tokens) for a model,
// resolved in priority order:
//  1. The user-declared per-model "limit" in their config (the opencode.jsonc
//     style block the /connect wizard accepts) — highest priority.
//  2. The live context_length reported by the gateway's /models endpoint
//     (cached during DiscoverModels). It represents the real per-deployment
//     cap — e.g. poolside reports 262144 although the model natively supports
//     1M — but is capped at the researched builtin window for the same model,
//     so the research-backed table stays the ceiling.
//  3. The builtin research-backed table for builtin providers' default models
//     (e.g. opencode free models per-model windows, gemini = 1M, ...).
//  4. A model-family fallback (claude-sonnet-*, gpt-5*, deepseek-*,
//     gemini-*, ...) so dated or unlisted model IDs still resolve to their
//     generation's window instead of collapsing to the 128k default.
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
	// Builtin research-backed window doubles as the ceiling for the live value.
	var builtin int
	for _, bp := range BuiltinProviders {
		if bp.ID != providerID {
			continue
		}
		if w, ok := bp.ContextLimits[model]; ok {
			builtin = w
		}
		break
	}
	// 2. Live context_length from the gateway's /models endpoint: the real
	// per-deployment cap, but never above the researched builtin window.
	if live, ok := liveContextLimits[providerID][model]; ok {
		if builtin == 0 {
			return live
		}
		if live < builtin {
			return live
		}
		return builtin
	}
	if builtin != 0 {
		return builtin
	}
	// 3. Model-family fallback for dated/unlisted model IDs.
	return familyContextWindow(model)
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

// preserveFromDisk reads the current config file and merges non-empty
// properties into cfg so that critical fields (search key, context7 key,
// fallback policy) are never silently clobbered by a partial in-memory
// config from the wizard or model-switch operations.
func preserveFromDisk(cfg AppConfig) AppConfig {
	p := GlobalConfigPath()
	raw, err := os.ReadFile(p)
	if err != nil {
		return cfg
	}
	var disk AppConfig
	if json.Unmarshal([]byte(stripJSONComments(string(raw))), &disk) != nil {
		// Malformed file: cannot safely preserve fields — return cfg unchanged.
		// The caller's in-memory state takes precedence over a corrupted file.
		return cfg
	}
	if cfg.SearchKey == "" && disk.SearchKey != "" {
		cfg.SearchKey = disk.SearchKey
	}
	if cfg.SearchProvider == "" && disk.SearchProvider != "" {
		cfg.SearchProvider = disk.SearchProvider
	}
	if cfg.Context7Key == "" && disk.Context7Key != "" {
		cfg.Context7Key = disk.Context7Key
	}
	if cfg.FallbackPolicy == "" && disk.FallbackPolicy != "" {
		cfg.FallbackPolicy = disk.FallbackPolicy
	}
	return cfg
}

// SaveGlobalConfig saves config to global path (~/.config/brocode/config.json)
// safely with field preservation: search/context7 keys on disk are never
// clobbered by partial in-memory configs.
func SaveGlobalConfig(cfg AppConfig) error {
	cfg = preserveFromDisk(cfg)
	return writeGlobalConfigDirect(cfg)
}

// writeGlobalConfigDirect writes cfg directly to ~/.config/brocode/config.json.
func writeGlobalConfigDirect(cfg AppConfig) error {
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

// SearchProviderStatus describes configured search providers and multi-tier fallback order.
type SearchProviderStatus struct {
	PrimaryProvider   string // "tavily", "exa", or "free"
	PrimaryKey        string
	SecondaryProvider string
	SecondaryKey      string
	Badge             string // " · 🌐:Free", " · 🌐:Tavily", " · 🌐:Exa", " · 🌐:Tavily+Exa"
}

// GetSearchProviderStatus computes the active and fallback search configuration.
func GetSearchProviderStatus() SearchProviderStatus {
	tavilyKey := os.Getenv("TAVILY_API_KEY")
	exaKey := os.Getenv("EXA_API_KEY")

	cfg := LoadConfig()
	if cfg.SearchKey != "" {
		if strings.EqualFold(cfg.SearchProvider, "exa") || strings.HasPrefix(cfg.SearchKey, "exa-") {
			if exaKey == "" {
				exaKey = cfg.SearchKey
			}
		} else {
			if tavilyKey == "" {
				tavilyKey = cfg.SearchKey
			}
		}
	}

	if tavilyKey != "" && exaKey != "" {
		return SearchProviderStatus{
			PrimaryProvider:   "tavily",
			PrimaryKey:        tavilyKey,
			SecondaryProvider: "exa",
			SecondaryKey:      exaKey,
			Badge:             " · 🌐:Tavily+Exa",
		}
	}
	if tavilyKey != "" {
		return SearchProviderStatus{
			PrimaryProvider: "tavily",
			PrimaryKey:      tavilyKey,
			Badge:           " · 🌐:Tavily",
		}
	}
	if exaKey != "" {
		return SearchProviderStatus{
			PrimaryProvider: "exa",
			PrimaryKey:      exaKey,
			Badge:           " · 🌐:Exa",
		}
	}
	return SearchProviderStatus{
		PrimaryProvider: "free",
		Badge:           " · 🌐:Free",
	}
}

// GetActiveSearchKey retrieves the active web search API key and provider ("tavily" or "exa").
// Priority:
// 1. Environment variable TAVILY_API_KEY / EXA_API_KEY
// 2. Saved key in AppConfig (~/.config/brocode/config.json)
func GetActiveSearchKey() (key string, providerName string) {
	st := GetSearchProviderStatus()
	if st.PrimaryProvider == "free" {
		return "", ""
	}
	return st.PrimaryKey, st.PrimaryProvider
}

// SaveSearchKey saves the search API key to global ~/.config/brocode/config.json with auto provider detection.
func SaveSearchKey(key string) error {
	return SaveSearchProviderKey("", key)
}

// SaveSearchProviderKey saves a specific search provider's key (e.g. "tavily" or "exa") to ~/.config/brocode/config.json.
func SaveSearchProviderKey(providerName, key string) error {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	key = strings.TrimSpace(key)
	cfg := LoadConfig()

	if providerName == "clear" || providerName == "reset" || providerName == "delete" || key == "clear" || key == "delete" || (providerName == "" && key == "") {
		cfg.SearchKey = ""
		cfg.SearchProvider = ""
		return writeGlobalConfigDirect(cfg)
	}

	if providerName == "" {
		if strings.HasPrefix(key, "tvly-") {
			providerName = "tavily"
		} else if strings.HasPrefix(key, "exa-") {
			providerName = "exa"
		} else {
			providerName = "tavily"
		}
	}

	// Preserve disk-only fields (context7 key, fallback policy) first,
	// then apply our changes on top — avoids the redundant re-apply pattern.
	cfg = preserveFromDisk(cfg)
	cfg.SearchKey = key
	cfg.SearchProvider = providerName
	return writeGlobalConfigDirect(cfg)
}

// GetActiveContext7Key retrieves the active Context7 API key.
// Priority:
// 1. Environment variable CONTEXT7_API_KEY
// 2. Saved key in AppConfig (~/.config/brocode/config.json)
func GetActiveContext7Key() string {
	if k := os.Getenv("CONTEXT7_API_KEY"); k != "" {
		return k
	}
	cfg := LoadConfig()
	return cfg.Context7Key
}

// SaveContext7Key saves or clears the Context7 API key in ~/.config/brocode/config.json.
func SaveContext7Key(key string) error {
	key = strings.TrimSpace(key)
	cfg := LoadConfig()
	// Preserve disk-only fields (search key, fallback policy) first,
	// then apply our changes on top — avoids the redundant re-apply pattern.
	cfg = preserveFromDisk(cfg)
	if key == "clear" || key == "reset" || key == "delete" || key == "remove" {
		cfg.Context7Key = ""
	} else {
		cfg.Context7Key = key
	}
	return writeGlobalConfigDirect(cfg)
}
