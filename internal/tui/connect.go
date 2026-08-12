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
	{name: "custom", method: "api key (URL|KEY)", detected: false},
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
// provider in /connect order. Uses dynamic discovery when available, falls
// back to static lists when API is unavailable or no credentials exist.
func (m Model) allModelEntries() []modelEntry {
	var out []modelEntry

	// Try to get all models from dynamic discovery
	discovered := DiscoverAllModels()

	for _, p := range defaultProviders {
		var models []string

		// Use discovered models if available, otherwise fallback to static
		if dModels, ok := discovered[p.name]; ok && len(dModels) > 0 {
			models = dModels
		} else {
			// Fallback to static lists
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
			case "custom":
				models = []string{"default", "llama-3-70b", "deepseek-coder", "qwen2-72b"}
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

	// Detect providers
	agyDetected, _ := DetectAntigravity()
	freebuffDetected, _ := DetectFreebuffCredentials()
	codebuffDetected, _ := DetectFreebuffCredentials() // Same credentials for both

	var sb strings.Builder

	// Scroll window: show providers around the selected one
	start := 0
	if m.connectSel >= maxProviders {
		start = m.connectSel - maxProviders + 2
	}
	end := start + maxProviders
	if end > len(defaultProviders) {
		end = len(defaultProviders)
	}

	if start > 0 {
		sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("    ... %d more above", start)))
		sb.WriteString("\n")
	}

	for i := start; i < end; i++ {
		p := defaultProviders[i]
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
			if loadAPIKey(p.name) != "" {
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

	if end < len(defaultProviders) {
		sb.WriteString(m.styles.statusLeft.Render(fmt.Sprintf("    ... %d more below", len(defaultProviders)-end)))
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
	sb.WriteString(m.styles.popoverFooter.Render("1-4 select provider · enter choose · esc/q close"))

	return m.popoverFrame("connect provider", sb.String(), w)
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
