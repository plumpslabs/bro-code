package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// apiKeysFile is the path to the stored API keys.
func apiKeysFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".brocode", "keys.json")
}

// loadAPIKey reads the stored API key for a provider.
func loadAPIKey(provider string) string {
	data, err := os.ReadFile(apiKeysFile())
	if err != nil {
		return ""
	}
	var keys map[string]string
	if err := json.Unmarshal(data, &keys); err != nil {
		return ""
	}
	return keys[provider]
}

// saveAPIKey stores an API key for a provider.
func saveAPIKey(provider, key string) error {
	keys := make(map[string]string)
	data, err := os.ReadFile(apiKeysFile())
	if err == nil {
		_ = json.Unmarshal(data, &keys)
	}
	keys[provider] = key
	out, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(apiKeysFile()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(apiKeysFile(), out, 0o600)
}

// provider is one connectable LLM provider.
type provider struct {
	name      string
	method    string
	detected  bool
	freeModel string
}

var defaultProviders = []provider{
	{name: "opencode", method: "native zen · free (no key)", detected: false},
	{name: "freebuff", method: "native · free (saved credentials)", detected: false},
	{name: "codebuff", method: "native · free (saved credentials)", detected: false},
	{name: "antigravity", method: "url login (browser)", detected: false},
	{name: "poolside", method: "api key (Laguna)", detected: false},
	{name: "groq", method: "api key (free tier)", detected: false},
	{name: "deepseek", method: "api key", detected: false},
	{name: "minimax", method: "api key", detected: false},
	{name: "zhipu", method: "api key (GLM)", detected: false},
	{name: "mimo", method: "api key (Xiaomi)", detected: false},
	{name: "claude", method: "api key", detected: false},
}

// GetProviders returns the default providers plus any custom providers from config.jsonc
func GetProviders() []provider {
	list := make([]provider, len(defaultProviders))
	copy(list, defaultProviders)

	cfg := LoadAppConfig()
	for id, p := range cfg.Provider {
		// A settings entry whose id collides with a built-in provider (e.g.
		// "groq") would create a duplicate row — the default row already
		// represents it, and its settings models still show via discovery.
		if hasDefaultProvider(id) {
			continue
		}
		list = append(list, provider{
			name:     id,
			method:   "config (" + p.Name + ")",
			detected: false,
		})
	}
	// Append the legacy "custom" provider at the end — unless the settings
	// already define a provider under that id (no duplicate rows).
	if _, inCfg := cfg.Provider["custom"]; !inCfg {
		list = append(list, provider{name: "custom", method: "api key (URL|KEY)", detected: false})
	}
	return list
}

// hasDefaultProvider reports whether name is one of the built-in providers.
func hasDefaultProvider(name string) bool {
	for _, p := range defaultProviders {
		if p.name == name {
			return true
		}
	}
	return false
}

// providerActive reports whether a provider is actually usable right now:
// defined in config.jsonc (custom providers), authenticated (native / free /
// detected CLI), or holding a saved API key. Unconfigured providers never
// contribute models to the picker, so the /models list cannot show phantom
// models for providers that were never set up (user report: "provider belum
// active udah di list modelna").
func providerActive(name string) bool {
	cfg := LoadAppConfig()
	if _, inCfg := cfg.Provider[name]; inCfg {
		return true // custom providers from settings are always active
	}
	switch name {
	case "opencode":
		return true // free zen gateway, no key needed
	case "freebuff", "codebuff":
		detected, _ := DetectFreebuffCredentials()
		return detected
	case "antigravity":
		detected, _ := DetectAntigravity()
		return detected
	case "custom":
		// Legacy custom (keys.json "url|key") is only "configured" when it
		// exists in settings — the model list must not invent models for it.
		return false
	default:
		return loadAPIKey(name) != ""
	}
}

// Static fallback models (used when API is unavailable or no credentials)
// These are kept as fallbacks; the system prefers dynamic discovery.
var openCodeFreeModels = []string{
	"deepseek-v4-flash-free",
	"mimo-v2.5-free",
	"laguna-s-2.1-free",
	"ling-3.0-flash-free",
	"ling-3.0-tiny-free",
	"longcat-2.0-free",
	"nemotron-3-ultra-free",
	"nemotron-3.5-lightning-free",
	"north-mini-code-free",
	"big-pickle",
}

var antigravityModels = []string{
	"gemini-2.5-flash",
	"gemini-2.5-pro",
	"gemini-2.0-flash",
}

var groqModels = []string{
	"llama-3.3-70b-versatile",
	"llama-3.1-8b-instant",
	"mixtral-8x7b-32768",
	"gemma2-9b-it",
}

var poolsideModels = []string{
	"poolside/laguna-s-2.1",
	"poolside/laguna-xs-2.1",
}

// MiniMax models (OpenAI-compatible API)
var minimaxModels = []string{
	"MiniMax-M3",
	"MiniMax-M2.7",
	"MiniMax-M2.7-highspeed",
}

// Zhipu/GLM models (OpenAI-compatible API)
var zhipuModels = []string{
	"glm-5.2",
	"glm-5.1",
	"glm-5-turbo",
	"glm-4.7",
}

// MiMo models (Xiaomi, OpenAI-compatible API)
var mimoModels = []string{
	"mimo-v2.5-pro",
	"mimo-v2.5",
}

// Freebuff native models (served through Freebuff backend)
var freebuffNativeModels = []string{
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"mimo-v2.5",
	"minimax-m3",
	"glm-5.2",
	"gemini-3.1-flash-lite",
}

// Codebuff native models (served through Codebuff backend)
var codebuffNativeModels = []string{
	"minimax-m2.7",
	"minimax-m3",
	"deepseek-v4-flash",
	"mimo-v2.5",
	"gemini-3.1-flash-lite",
	"deepseek-v4-pro",
	"mimo-v2.5-pro",
	"kimi-k2.6",
}

// modelEntry is one selectable model in the /models picker: the model name
// plus the provider that serves it, so a cross-provider list can label each
// row and switch the active provider on selection.
type modelEntry struct {
	provider string
	model    string
}

// zenModelsEndpoint is the OpenCode Zen gateway model list — the source of
// truth for the free models, fetched live when /models opens instead of
// trusting a static snapshot (doctrine P3: real data over curated lists).
const zenModelsEndpoint = "https://opencode.ai/zen/v1/models"

// zenModelsTTL caps how long a fetched list is trusted before the next
// /models open refetches it. The list rarely changes; the TTL keeps the
// picker live without hammering the gateway on every toggle.
const zenModelsTTL = 5 * time.Minute

// fetchZenModels pulls the current free-model list from the Zen gateway.
// The endpoint lists ALL gateway models (paid included) — free ones are the
// "…-free" IDs plus big-pickle. endpoint is injectable for tests. Sorted +
// deduped so the picker rows are stable across refreshes.
func fetchZenModels(endpoint string) ([]string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zen models HTTP %d", resp.StatusCode)
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
	var out []string
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		lower := strings.ToLower(id)
		// Free models are the "…-free" IDs plus the grandfathered big-pickle.
		// A suffix check (not a substring) avoids leaking a paid model whose
		// id happens to contain "free".
		if !strings.HasSuffix(lower, "-free") && lower != "big-pickle" {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) == 0 {
		// A successful fetch with zero free models means the gateway changed
		// its naming — treat it as a failure so the picker shows the honest
		// static fallback instead of a lying "0 live / fetch failed" state.
		return nil, fmt.Errorf("no free models in zen response")
	}
	return out, nil
}

// allModelEntries returns every known model across all providers, grouped by
// provider in /connect order. Uses the on-disk model cache (NEVER network —
// opening the picker must not freeze the UI; a stale cache is refreshed in
// the background via modelsRefreshCmd), and falls back to static lists for
// active providers without cached entries.
func (m Model) allModelEntries() []modelEntry {
	var out []modelEntry

	// Cached/discovered models — network-free. A stale cache shows config
	// providers + static opencode immediately while the background refresh
	// (modelsRefreshCmd, kicked on /models open) fills in the live lists.
	discovered := cachedModelEntries()

	for _, p := range GetProviders() {
		var models []string

		// Use discovered models if available, otherwise fallback to static
		if dModels, ok := discovered[p.name]; ok && len(dModels) > 0 {
			models = dModels
		} else {
			// Only show static fallbacks for providers that are actually
			// active (native/free, authenticated, or holding a saved key).
			// Custom providers get their models exclusively from settings —
			// there is no hardcoded fallback list for them.
			if !providerActive(p.name) {
				continue
			}
			switch p.name {
			case "opencode":
				models = openCodeFreeModels
			case "freebuff":
				models = freebuffNativeModels
			case "codebuff":
				models = codebuffNativeModels
			case "antigravity":
				models = antigravityModels
			case "groq":
				models = groqModels
			case "poolside":
				models = poolsideModels
			case "deepseek":
				models = deepseekStaticModels
			case "minimax":
				models = minimaxModels
			case "zhipu":
				models = zhipuModels
			case "mimo":
				models = mimoModels
			case "claude":
				models = claudeStaticModels
			}
		}

		for _, mod := range models {
			out = append(out, modelEntry{provider: p.name, model: mod})
		}
	}
	return out
}

// filterModelEntries returns the entries whose model name or provider
// contains the query (case-insensitive). Empty query returns everything.
func filterModelEntries(entries []modelEntry, query string) []modelEntry {
	if query == "" {
		return entries
	}
	q := strings.ToLower(query)
	var out []modelEntry
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.model), q) || strings.Contains(strings.ToLower(e.provider), q) {
			out = append(out, e)
		}
	}
	return out
}

// DetectOpenCode checks if OpenCode CLI or config exists locally. Returns
// the best available model (dynamically discovered if possible).
func DetectOpenCode() (bool, string) {
	// Check standard PATH or ~/.opencode/bin/opencode location
	if _, err := exec.LookPath("opencode"); err == nil {
		if models, ok := DiscoverProviderModels("opencode"); ok && len(models) > 0 {
			return true, models[0]
		}
		return true, openCodeFreeModels[0]
	}

	home, err := os.UserHomeDir()
	if err == nil {
		paths := []string{
			filepath.Join(home, ".opencode", "bin", "opencode"),
			filepath.Join(home, ".config", "opencode"),
			filepath.Join(home, ".local", "share", "opencode"),
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				if models, ok := DiscoverProviderModels("opencode"); ok && len(models) > 0 {
					return true, models[0]
				}
				return true, openCodeFreeModels[0]
			}
		}
	}
	return false, ""
}

// DetectAntigravity checks if Antigravity native config or CLI (agy) exists locally.
// Returns the best available model (dynamically discovered if possible).
func DetectAntigravity() (bool, string) {
	if key := os.Getenv("AGY_API_KEY"); key != "" {
		if models, ok := DiscoverProviderModels("antigravity"); ok && len(models) > 0 {
			return true, models[0]
		}
		return true, antigravityModels[0]
	}
	if _, err := exec.LookPath("agy"); err == nil {
		if models, ok := DiscoverProviderModels("antigravity"); ok && len(models) > 0 {
			return true, models[0]
		}
		return true, antigravityModels[0]
	}

	home, err := os.UserHomeDir()
	if err == nil {
		paths := []string{
			filepath.Join(home, ".gemini", "antigravity-cli"),
			filepath.Join(home, ".config", "antigravity"),
			filepath.Join(home, ".local", "bin", "agy"),
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				if models, ok := DiscoverProviderModels("antigravity"); ok && len(models) > 0 {
					return true, models[0]
				}
				return true, antigravityModels[0]
			}
		}
	}
	return false, ""
}

// renderConnectModalBox renders the framed modal box for /connect with auto-detection badges.
// Height is capped; if more providers exist, a scroll indicator shows.
func (m Model) renderConnectModalBox() string {
	w := min(62, m.width-4)
	if w < 32 {
		w = 32
	}

	// Height budget: header(2) + footer(2) + padding(2) = 6 lines overhead
	maxProviders := m.height - 8
	if maxProviders < 3 {
		maxProviders = 3
	}
	if maxProviders > 15 {
		maxProviders = 15
	}

	var sb strings.Builder
	provs := GetProviders()

	// Scroll window: show providers around the selected one
	start := 0
	if m.connectSel >= maxProviders {
		start = m.connectSel - maxProviders + 2
	}
	end := start + maxProviders
	if end > len(provs) {
		end = len(provs)
	}

	if start > 0 {
		sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("    ... %d more above", start)))
		sb.WriteString("\n")
	}

	// Detect providers
	agyDetected, _ := DetectAntigravity()
	freebuffDetected, _ := DetectFreebuffCredentials()
	codebuffDetected, _ := DetectFreebuffCredentials() // Same credentials for both

	for i := start; i < end; i++ {
		p := provs[i]
		statusStr := p.method
		if p.name == "opencode" {
			statusStr = m.styles.ok.Render("✓ free (zen)")
		} else if p.name == "freebuff" {
			if freebuffDetected {
				statusStr = m.styles.ok.Render("✓ credentials found")
			} else {
				statusStr = m.styles.statusLeft.Render("○ install: npm i -g freebuff && freebuff")
			}
		} else if p.name == "codebuff" {
			if codebuffDetected {
				statusStr = m.styles.ok.Render("✓ credentials found")
			} else {
				statusStr = m.styles.statusLeft.Render("○ install: npm i -g codebuff && codebuff")
			}
		} else if p.name == "antigravity" && agyDetected {
			statusStr = m.styles.ok.Render("✓ configured")
		} else if strings.Contains(p.method, "api key") {
			if keySaved(p.name) {
				statusStr = m.styles.ok.Render("✓ key saved")
			} else {
				statusStr = m.styles.err.Render("✗ no key")
			}
		}

		row := fmt.Sprintf("%d  %-12s %s", i+1, p.name, statusStr)
		if i == m.connectSel {
			sb.WriteString("  ")
			sb.WriteString(m.styles.sideSel.Render(" " + row + " "))
			sb.WriteString("\n")
		} else {
			sb.WriteString("  ")
			sb.WriteString(m.styles.statusLeft.Render(row))
			sb.WriteString("\n")
		}
	}

	if end < len(provs) {
		sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("    ... %d more below", len(provs)-end)))
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
	sb.WriteString(m.styles.popoverFooter.Render("1-4 select provider · enter choose · esc/q close"))

	return m.popoverFrame("connect provider", sb.String(), w)
}

// keySaved reports whether the provider actually holds a usable saved key.
// The legacy custom entry is stored as "url|key" — a bare "|" (empty URL,
// empty key) must NOT count as configured.
func keySaved(name string) bool {
	key := loadAPIKey(name)
	if key == "" {
		return false
	}
	if name == "custom" {
		return strings.Split(key, "|")[0] != ""
	}
	return true
}

// renderConnect is the full-viewport /connect view helper.
func (m Model) renderConnect() string {
	bodyH := m.height - 5
	if bodyH < 8 {
		bodyH = 8
	}
	box := m.renderConnectModalBox()
	return lipgloss.Place(m.width, bodyH, lipgloss.Center, lipgloss.Center, box)
}

// renderAPIKeyModalBox renders the API key input modal for a provider.
func (m Model) renderAPIKeyModalBox() string {
	w := min(56, m.width-4)
	if w < 36 {
		w = 36
	}

	// Bound the masked input to the modal content width so a long pasted key
	// scrolls horizontally inside the box instead of overflowing it
	// ("current: xxxx…xxxx" preview shows the tail, not the full key).
	m.apikeyInput.SetWidth(w - 6)

	var sb strings.Builder
	sb.WriteString(m.styles.statusLeft.Render("  Paste your API key from console:"))
	sb.WriteString("\n")
	sb.WriteString("  " + m.apikeyInput.View())
	sb.WriteString("\n\n")
	sb.WriteString(m.styles.popoverFooter.Render("  enter save · esc cancel"))
	if existing := loadAPIKey(m.apikeyTarget); existing != "" {
		sb.WriteString("\n")
		sb.WriteString(m.styles.ok.Render(fmt.Sprintf("  current: %s…%s", existing[:min(4, len(existing))], existing[max(0, len(existing)-4):])))
	}

	return m.popoverFrame(m.apikeyTarget+" api key", sb.String(), w)
}
