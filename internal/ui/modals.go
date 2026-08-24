package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/mcp"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tokens"
)

// reloadMCP restarts the MCP manager from disk config and re-registers its
// tools (shared by /mcp-reload, the modal's r key, and the add/delete flows).
func (m *Model) reloadMCP() {
	if m.mcpMgr == nil {
		m.appendMessages("⚠️ MCP manager not initialized.")
		return
	}
	m.mcpMgr.Close()
	m.mcpMgr.LoadDefaults()
	m.mcpMgr.Start(context.Background())
	for _, mt := range m.mcpMgr.Tools() {
		m.tools.Register(mt)
	}
	m.rebuildEngine()
	m.appendNote(m.mcpStatus())
}

// mcpNames returns the sorted configured server names (empty when nil).
func (m *Model) mcpNames() []string {
	if m.mcpMgr == nil {
		return nil
	}
	return m.mcpMgr.ServerNames()
}

// mcpAddNext advances the add wizard; the final step saves the server to
// .mcp.json and reloads.
func (m *Model) mcpAddNext() {
	switch m.mcpAddStep {
	case 0: // transport picked → name
		m.mcpAddStep = 1
		m.mcpAddName.Focus()
	case 1: // name → command (stdio) or URL (http/sse)
		if strings.TrimSpace(m.mcpAddName.Value()) == "" {
			return // name required — stay on the step
		}
		m.mcpAddStep = 2
		if m.mcpAddType == 0 {
			m.mcpAddCmd.Focus()
		} else {
			m.mcpAddURL.Focus()
		}
	case 2: // save
		m.mcpAddName.Blur()
		m.mcpAddCmd.Blur()
		m.mcpAddURL.Blur()
		m.saveMCPAdd()
		m.mcpAddActive = false
		m.mcpAddStep = 0
	}
}

// mcpAddPrev steps the wizard back (or cancels it at the transport step).
func (m *Model) mcpAddPrev() {
	if m.mcpAddStep == 0 {
		m.mcpAddName.Blur()
		m.mcpAddCmd.Blur()
		m.mcpAddURL.Blur()
		m.mcpAddActive = false
		return
	}
	m.mcpAddStep--
	switch m.mcpAddStep {
	case 0:
		m.mcpAddName.Blur()
	case 1:
		m.mcpAddName.Focus()
	}
}

// saveMCPAdd writes the completed wizard form into the project .mcp.json
// (the standard cross-tool convention) and reloads the manager.
func (m *Model) saveMCPAdd() {
	name := strings.TrimSpace(m.mcpAddName.Value())
	if name == "" {
		m.appendMessages("⚠️ MCP server name is required.")
		return
	}
	var cfg mcp.ServerConfig
	switch m.mcpAddType {
	case 1: // http
		cfg = mcp.ServerConfig{Type: "http", URL: strings.TrimSpace(m.mcpAddURL.Value())}
	case 2: // sse
		cfg = mcp.ServerConfig{Type: "sse", URL: strings.TrimSpace(m.mcpAddURL.Value())}
	default: // stdio
		fields := strings.Fields(strings.TrimSpace(m.mcpAddCmd.Value()))
		if len(fields) == 0 {
			m.appendMessages("⚠️ MCP command is required (e.g. npx -y <pkg>).")
			return
		}
		cfg = mcp.ServerConfig{Command: fields[0], Args: fields[1:]}
	}
	if err := mcp.AddServerToFile(mcp.ProjectMCPPath(), name, cfg); err != nil {
		m.appendMessages("❌ Failed to write " + mcp.ProjectMCPPath() + ": " + err.Error())
		return
	}
	m.reloadMCP()
	m.appendMessages(fmt.Sprintf("✅ Added MCP server %q → %s", name, mcp.ProjectMCPPath()))
}

// deleteMCPServer removes a server from .mcp.json and reloads.
func (m *Model) deleteMCPServer(name string) {
	if name == "" {
		return
	}
	if err := mcp.RemoveServerFromFile(mcp.ProjectMCPPath(), name); err != nil {
		m.appendMessages("❌ Failed to update " + mcp.ProjectMCPPath() + ": " + err.Error())
		return
	}
	m.reloadMCP()
	m.appendMessages(fmt.Sprintf("🗑️ Removed MCP server %q from %s", name, mcp.ProjectMCPPath()))
}

// mcpStatus renders a readable status of connected MCP servers and tools.
func (m *Model) mcpStatus() string {
	if m.mcpMgr == nil {
		return "⚠️ MCP not initialized."
	}
	names := m.mcpMgr.ServerNames()
	if len(names) == 0 {
		return "ℹ️ No MCP servers configured.\n\nCreate a `.mcp.json` in the project root or `~/.config/brocode/mcp.json` (same format as Claude/Cursor):\n```json\n{\"mcpServers\": {\"my-server\": {\"command\": \"npx\", \"args\": [\"-y\", \"pkg\"]}}}\n```\nThen run `/mcp-reload` to connect."
	}

	errs := m.mcpMgr.Errors()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔌 MCP: %d server(s), %d tool(s)\n", len(names), len(m.mcpMgr.Tools())))
	toolNames := make(map[string][]string)
	for _, t := range m.mcpMgr.Tools() {
		toolNames[t.Server()] = append(toolNames[t.Server()], t.ToolName())
	}
	for _, n := range names {
		if e := errs[n]; e != "" {
			sb.WriteString(fmt.Sprintf("❌ %s — %s\n", n, e))
			continue
		}
		ts := toolNames[n]
		sort.Strings(ts)
		sb.WriteString(fmt.Sprintf("✅ %s — %d tool(s): %s\n", n, len(ts), strings.Join(ts, ", ")))
	}
	return strings.TrimSpace(sb.String())
}

// mcpServerDetail renders one server's full status (used by ENTER in the MCP
// modal — the compact list row shows only the tool count).
func (m *Model) mcpServerDetail(name string) string {
	if m.mcpMgr == nil {
		return "⚠️ MCP not initialized."
	}
	var sb strings.Builder
	sb.WriteString("🔌 " + name)
	if e := m.mcpMgr.Errors()[name]; e != "" {
		sb.WriteString(" — ❌ " + e)
		return sb.String()
	}
	ts := m.mcpMgr.ToolNames(name)
	sort.Strings(ts)
	sb.WriteString(fmt.Sprintf(" — ✅ %d tool(s)\n", len(ts)))
	if len(ts) > 0 {
		sb.WriteString("  " + strings.Join(ts, "\n  "))
	}
	return sb.String()
}

// renderMCPModal renders the interactive MCP manager: server list with
// connect status, empty state with add hint, y/n delete confirm, and the
// add-server wizard (transport → name → command/URL).
func (m *Model) renderMCPModal() string {
	var sb strings.Builder
	greenBadge := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	dangerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	names := m.mcpNames()
	toolCount := 0
	if m.mcpMgr != nil {
		toolCount = len(m.mcpMgr.Tools())
	}
	sb.WriteString(fmt.Sprintf("=== MCP Servers (/mcp) — %d server(s) · %d tool(s) ===\n", len(names), toolCount))

	switch {
	case m.mcpAddActive:
		// Add-server wizard.
		transports := []string{"stdio (local subprocess)", "http (streamable)", "sse (server-sent events)"}
		if m.mcpAddStep == 0 {
			sb.WriteString("\nSelect transport:\n")
			for i, t := range transports {
				cursor := "  "
				if i == m.mcpAddType {
					cursor = "❯ "
				}
				sb.WriteString(fmt.Sprintf("%s%s\n", cursor, t))
			}
			sb.WriteString("\n[↑/↓ transport · ENTER next · ESC cancel]")
		} else {
			if m.mcpAddStep == 1 {
				sb.WriteString("\nServer name:\n  " + m.mcpAddName.View())
			} else {
				if m.mcpAddType == 0 {
					sb.WriteString("\nCommand + args (stdio):\n  " + m.mcpAddCmd.View())
				} else {
					sb.WriteString("\nEndpoint URL (" + transports[m.mcpAddType] + "):\n  " + m.mcpAddURL.View())
				}
			}
			sb.WriteString("\n\n[ENTER next · ESC back]")
		}

	case m.mcpConfirm != "":
		// Destructive action pending: block the list and ask explicitly.
		sb.WriteString("\n" + dangerStyle.Render("⚠️  CONFIRM DELETE — cannot be undone\n"))
		sb.WriteString(fmt.Sprintf("Remove MCP server %q from %s?\n", m.mcpConfirm, mcp.ProjectMCPPath()))
		sb.WriteString("\n[y] confirm delete · [n / ESC] cancel")

	case len(names) == 0:
		// Empty state: nothing configured, show the way in.
		sb.WriteString("\nNo MCP servers configured.\n")
		sb.WriteString("Press [a] to add one — or configure " + mcp.ProjectMCPPath() + " directly.\n")
		sb.WriteString("\n[a] add server · [r] reload · ESC close")

	default:
		errs := m.mcpMgr.Errors()
		for i, n := range names {
			cursor := "  "
			if i == m.mcpSel {
				cursor = "❯ "
			}
			if e := errs[n]; e != "" {
				sb.WriteString(fmt.Sprintf("%s❌ %s — %s\n", cursor, n, e))
				continue
			}
			ts := m.mcpMgr.ToolNames(n)
			sort.Strings(ts)
			sb.WriteString(fmt.Sprintf("%s✅ %s — %d tool(s)\n", cursor, n, len(ts)))
		}
		if m.mcpSel >= 0 && m.mcpSel < len(names) && len(m.mcpMgr.ToolNames(names[m.mcpSel])) > 0 {
			sb.WriteString(greenBadge.Render("\nENTER: show tools of the selected server"))
		}
		sb.WriteString("\n\n[↑/↓ navigate · a add · d delete · r reload · ENTER tools · ESC close]")
	}

	body := sb.String()
	w := m.width - 8
	if w > 30 {
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			if len(ln) > w-4 {
				lines[i] = ln[:w-4]
			}
		}
		body = strings.Join(lines, "\n")
	}
	return lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2).Render(body)
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}


// confirmDeleteSessions executes the pending sessions-modal delete (single
// session or ALL, per sessionsConfirmID), recreates the active session row so
// the events FK stays valid, and refreshes the list.
func (m *Model) confirmDeleteSessions() {
	st := m.context.Store()
	if st == nil {
		m.appendMessages("⚠️ Session store not initialized.")
		return
	}

	active := m.context.SessionID()
	cwd, _ := os.Getwd()
	target := m.sessionsConfirmID
	var removed int
	var err error

	if target == "ALL" {
		removed, err = st.DeleteAllSessions()
	} else {
		removed, err = st.DeleteSession(target)
	}
	if err != nil {
		m.appendMessages("❌ Failed to delete session(s): " + err.Error())
		return
	}

	// The current session row may have been deleted (single = the active one,
	// ALL = every row). Recreate it so future events still satisfy the FK and
	// keep persisting — the in-memory conversation continues untouched.
	if target == "ALL" || target == active {
		if err := st.CreateSession(active, cwd); err == nil {
			m.appendMessages(fmt.Sprintf("🗑️ Deleted %d session(s) (%d events). Current session reset — history cleared.", len(m.sessionList), removed))
		} else {
			m.appendMessages(fmt.Sprintf("🗑️ Deleted %d session(s) (%d events).", len(m.sessionList), removed))
		}
	} else {
		m.appendMessages(fmt.Sprintf("🗑️ Deleted session %s (%d events).", target, removed))
	}

	// Refresh the list; clamp the cursor into the new bounds.
	if list, lerr := st.ListSessionsByProjectPath(cwd); lerr == nil {
		m.sessionList = list
		if m.sessionsSel >= len(list) {
			m.sessionsSel = len(list) - 1
		}
		if m.sessionsSel < 0 && len(list) > 0 {
			m.sessionsSel = 0
		}
	}
}

func (m *Model) applySelectedSession() {
	m.sessionsConfirmID = "" // switching is not a delete; drop any pending confirm
	if m.sessionsSel >= 0 && m.sessionsSel < len(m.sessionList) {
		targetSess := m.sessionList[m.sessionsSel]
		st := m.context.Store()
		m.context = bcontext.NewManager(targetSess.ID, st, m.contextWindow())

		// Continue in the session's last engine mode (persisted on each mode
		// change) rather than silently dropping back to BUILDER.
		if targetSess.Mode != "" {
			m.mode = targetSess.Mode
		} else {
			m.mode = "BUILDER"
		}
		m.engine.SetMode(m.mode) // apply executor-level policy for the restored mode

		// Load past events into context and message log
		m.messages = []string{fmt.Sprintf("✅ Switched to session: %s", targetSess.ID)}
		if st != nil {
			// Purge history duplicated by old resume logic before restoring.
			if removed, err := st.CleanupReplayDuplicates(targetSess.ID); err == nil && removed > 0 {
				m.appendMessages(fmt.Sprintf("⚡ Purged %d duplicated history events", removed))
			}
			if events, err := st.GetSessionEvents(targetSess.ID); err == nil && len(events) > 0 {
				// Same restore path as `brocode -c`: replay only the newest
				// events that fit the context window, keep assistant tool calls
				// paired with their results, restore file change summaries inline
				// at their original chronological place, and render tool-call-only
				// turns as compact summaries instead of raw JSON.
				m.appendMessages(bcontext.RestoreSession(m.context, events)...)
				// Show the restored FILES: change summaries expanded so the user
				// sees what was edited/created/deleted without pressing ctrl+f.
				for _, msg := range m.messages {
					if strings.HasPrefix(msg, "FILES:\n") {
						m.filesExpanded = true
						break
					}
				}
			}
		}
		// Invalidate the rendered-log cache so the viewport re-renders with the
		// freshly loaded session history instead of showing stale content.
		m.renderedLog = ""
		m.renderedKey = ""
		m.logViewport.SetYOffset(0)
		m.rebuildEngine()
		m.persistMode()
	}
}

// persistMode writes the current engine mode to the session row so a later
// resume (`-c` or /sessions) continues in the same mode. Best-effort: the
// store is optional (nil when SQLite init failed).
func (m *Model) persistMode() {
	if st := m.context.Store(); st != nil {
		_ = st.UpdateSessionMode(m.context.SessionID(), m.mode)
	}
}

func (m *Model) isProviderConfigured(pID, envVar string) bool {
	if pID == "opencode" {
		return true
	}
	if custom, ok := m.cfg.Providers[pID]; ok && (custom.APIKey != "" || (custom.APIKeyEnv != "" && os.Getenv(custom.APIKeyEnv) != "")) {
		return true
	}
	if envVar != "" && os.Getenv(envVar) != "" {
		return true
	}
	return false
}

func (m *Model) saveProviderKey(pID, apiKey string) {
	pID = strings.ToLower(pID)
	found := false
	var targetProvider provider.ProviderInfo
	for _, p := range provider.BuiltinProviders {
		if p.ID == pID {
			targetProvider = p
			found = true
			break
		}
	}

	if !found {
		// Custom provider
		targetProvider = provider.ProviderInfo{
			ID:             pID,
			Name:           pID + " (Custom)",
			Protocol:       "openai-compatible",
			DefaultBaseURL: "https://api.openai.com/v1",
			DefaultModels:  []string{"default"},
		}
	}

	if m.cfg.Providers == nil {
		m.cfg.Providers = make(map[string]provider.CustomProviderConfig)
	}

	m.cfg.Providers[pID] = provider.CustomProviderConfig{
		Protocol:  targetProvider.Protocol,
		BaseURL:   targetProvider.DefaultBaseURL,
		APIKeyEnv: targetProvider.APIKeyEnvVar,
		APIKey:    apiKey,
		Models:    targetProvider.DefaultModels,
		ModelMap:  nil,
	}

	if err := provider.SaveGlobalConfig(m.cfg); err != nil {
		m.appendMessages(fmt.Sprintf("❌ Failed to save config: %v", err))
		return
	}

	m.appendMessages(fmt.Sprintf("✅ API Key for %s saved to ~/.config/brocode/config.json!", targetProvider.Name))

	// Re-detect providers and switch if appropriate
	m.modelOptions = provider.DiscoverModels(m.cfg)
	m.modelListCache = nil
	m.switchProviderAndModel(pID, targetProvider.DefaultModels[0])
}

// connectNext advances the connect wizard one step (or saves on the last).
func (m *Model) connectNext() {
	switch m.connectStep {
	case 0:
		if m.connectProviderSel >= len(provider.BuiltinProviders) {
			// Custom provider: full multi-step wizard.
			m.connectCustom = true
			m.connectStep = 1
			m.connectNameInput.SetValue("")
			m.connectNameInput.Focus()
		} else {
			// Built-in provider.
			m.connectCustom = false
			p := provider.BuiltinProviders[m.connectProviderSel]
			m.connectBaseURLInput.SetValue(p.DefaultBaseURL)
			if p.APIKeyEnvVar == "" {
				// Keyless provider (BroCode Free Gateway, FreeBuff, Ollama): no
				// API key exists — save straight away.
				m.connectTextInput.SetValue("")
				m.applyConnectConfig()
				m.showConnect = false
				m.appendMessages(fmt.Sprintf("✅ %s connected — no API key needed (token auto-loaded).", p.Name))
				return
			}
			// Built-in provider with a real key: only the API key step is needed.
			m.connectTextInput.SetValue("")
			m.connectStep = 1
			m.connectTextInput.Focus()
		}
	case 1:
		if m.connectCustom {
			m.connectTextInput.SetValue("")
			m.connectStep = 2
			m.connectTextInput.Focus()
		} else {
			// Built-in: single step, save immediately.
			m.applyConnectConfig()
			m.showConnect = false
		}
	case 2:
		m.connectStep = 3
		m.connectBaseURLInput.Focus()
	case 3:
		m.connectModelsInput.SetValue("")
		m.connectStep = 4
		m.connectModelsInput.Focus()
	case 4:
		m.applyConnectConfig()
		m.showConnect = false
	}
}

// connectPrev steps the wizard back one step (or closes it at step 0).
func (m *Model) connectPrev() {
	if m.connectStep == 0 {
		m.showConnect = false
		return
	}
	m.connectStep--
	switch m.connectStep {
	case 1:
		if m.connectCustom {
			m.connectNameInput.Focus()
		} else {
			// Built-in: step 1 (API key) goes back to provider pick.
			m.connectTextInput.Blur()
		}
	case 2:
		m.connectTextInput.Focus()
	case 3:
		m.connectBaseURLInput.Focus()
	case 4:
		m.connectModelsInput.Focus()
	}
}

// applyConnectConfig saves the completed wizard form as a provider config and
// switches the active provider/model to it.
func (m *Model) applyConnectConfig() {
	if m.connectCustom {
		m.saveCustomProvider()
		return
	}
	if m.connectProviderSel < 0 || m.connectProviderSel >= len(provider.BuiltinProviders) {
		return
	}
	p := provider.BuiltinProviders[m.connectProviderSel]
	keyVal := strings.TrimSpace(m.connectTextInput.Value())
	baseURL := strings.TrimSpace(m.connectBaseURLInput.Value())
	if keyVal == "" && (baseURL == "" || baseURL == p.DefaultBaseURL) {
		m.appendMessages("⚠️ Nothing to save — API key is empty.")
		return
	}
	m.saveProviderConfig(p.ID, p, keyVal, baseURL, nil, nil)
}

// saveCustomProvider persists a brand-new custom provider from wizard fields.
func (m *Model) saveCustomProvider() {
	pID := strings.TrimSpace(m.connectNameInput.Value())
	if pID == "" {
		m.appendMessages("❌ Provider name is required.")
		return
	}
	pID = strings.ToLower(strings.ReplaceAll(pID, " ", "-"))

	keyVal := strings.TrimSpace(m.connectTextInput.Value())
	baseURL := strings.TrimSpace(m.connectBaseURLInput.Value())
	if baseURL == "" {
		m.appendMessages("❌ Base URL is required for a custom provider.")
		return
	}

	modelIDs, modelMap, err := provider.ParseModelJSON(m.connectModelsInput.Value())
	if err != nil {
		m.appendMessages("❌ Models JSON invalid: " + err.Error())
		return
	}

	// A provider without a declared model list is unusable (falls back to the
	// placeholder "default") — try the gateway's live /models endpoint before
	// saving so the provider actually works on the first turn.
	if len(modelIDs) == 0 && keyVal != "" {
		if fetched, ferr := provider.FetchOpenAIModels(baseURL, keyVal); ferr == nil && len(fetched) > 0 {
			modelIDs = fetched
		}
	}

	info := provider.ProviderInfo{
		ID:             pID,
		Name:           pID + " (Custom)",
		Protocol:       "openai-compatible",
		DefaultBaseURL: baseURL,
		DefaultModels:  modelIDs,
	}
	m.saveProviderConfig(pID, info, keyVal, baseURL, modelIDs, modelMap)
	if len(modelIDs) == 0 {
		m.appendMessages("⚠️ No models found — open /models to pick a model, or re-run /connect and paste the models JSON block.")
	}
}

// saveProviderConfig writes a provider into the global config and switches to it.
func (m *Model) saveProviderConfig(pID string, info provider.ProviderInfo, keyVal, baseURL string, modelIDs []string, modelMap map[string]provider.CustomModel) {
	m.cfg = provider.LoadConfig()
	if m.cfg.Providers == nil {
		m.cfg.Providers = make(map[string]provider.CustomProviderConfig)
	}

	m.cfg.Providers[pID] = provider.CustomProviderConfig{
		Protocol: info.Protocol,
		BaseURL:  baseURL,
		APIKey:   keyVal,
		Models:   modelIDs,
		ModelMap: modelMap,
	}

	if err := provider.SaveGlobalConfig(m.cfg); err != nil {
		m.appendMessages(fmt.Sprintf("❌ Failed to save config: %v", err))
		return
	}

	m.modelOptions = provider.DiscoverModels(m.cfg)
	m.modelListCache = nil
	model := "default"
	if len(info.DefaultModels) > 0 {
		model = info.DefaultModels[0]
	}
	m.switchProviderAndModel(pID, model)
	m.appendMessages(fmt.Sprintf("✅ Provider %s saved to ~/.config/brocode/config.json!", pID))
}

type modelOptionItem struct {
	ProviderID string
	ModelName  string
}

func (m *Model) getModelList() []modelOptionItem {
	// Memoize: the modal re-renders on every keystroke, but the underlying list
	// only changes when the filter query (or the discovered models) changes.
	if m.modelListCache != nil && m.modelListCacheQuery == m.modelsQuery {
		return m.modelListCache
	}
	var items []modelOptionItem
	var providerIDs []string
	for pID := range m.modelOptions {
		providerIDs = append(providerIDs, pID)
	}
	sort.Strings(providerIDs)

	for _, pID := range providerIDs {
		list := m.modelOptions[pID]
		for _, mod := range list {
			if m.modelsQuery != "" {
				q := strings.ToLower(m.modelsQuery)
				if !strings.Contains(strings.ToLower(mod), q) && !strings.Contains(strings.ToLower(pID), q) {
					continue
				}
			}
			items = append(items, modelOptionItem{ProviderID: pID, ModelName: mod})
		}
	}
	m.modelListCache = items
	m.modelListCacheQuery = m.modelsQuery
	return items
}

func (m *Model) switchProviderAndModel(pID, modelName string) {
	m.cfg = provider.LoadConfig()
	detected := provider.AutoDetect(m.cfg)
	for _, d := range detected {
		if d.Info.ID == pID {
			m.activeProvider = d
			m.activeModel = modelName
			m.cfg.DefaultProvider = pID
			m.cfg.DefaultModel = modelName
			_ = provider.SaveGlobalConfig(m.cfg)

			if pID == "opencode" {
				m.adapter = provider.NewOpenCodeAdapter()
			} else if d.Info.Protocol == "anthropic" {
				m.adapter = provider.NewAnthropicAdapter(d.Info.DefaultBaseURL, d.APIKey)
			} else {
				m.adapter = provider.NewOpenAIAdapter(d.Info.DefaultBaseURL, d.APIKey)
			}
			// OpenCode adapter is a standalone HTTP gateway; no CLI shims.
			m.rebuildEngine()
			// The session's context window follows the newly selected model's
			// declared limit (e.g. 1M models get a 1M window).
			m.context.SetMaxWindow(m.contextWindow())
			m.appendMessages(fmt.Sprintf("✅ Active model set & saved: %s/%s", pID, modelName))
			return
		}
	}
	m.activeModel = modelName
	m.cfg.DefaultModel = modelName
	_ = provider.SaveGlobalConfig(m.cfg)
	m.appendMessages(fmt.Sprintf("⚠️ Model set & saved to %s", modelName))
}

func (m *Model) applySelectedModel() {
	items := m.getModelList()
	if m.modelsSel >= 0 && m.modelsSel < len(items) {
		selected := items[m.modelsSel]
		m.switchProviderAndModel(selected.ProviderID, selected.ModelName)
	}
}

func (m *Model) renderPager() string {
	var sb strings.Builder
	if m.width > 0 {
		if m.logViewport.Width() != m.width {
			m.logViewport.SetWidth(m.width)
		}
		chrome, chromeLines := m.buildLogChrome()
		avail := m.height - chromeLines
		if avail < 3 {
			avail = 3
		}
		if m.logViewport.Height() != avail {
			m.logViewport.SetHeight(avail)
		}
		contentWidth := m.width - 4
		if contentWidth < 20 {
			contentWidth = 80
		}
		if m.pagerContent == "" || m.pagerWidth != contentWidth {
			m.pagerContent = m.buildPagerContent(contentWidth)
			m.pagerWidth = contentWidth
			m.logViewport.SetContent(m.pagerContent)
			m.logViewport.GotoTop()
		}
		sb.WriteString(m.logViewport.View())
		sb.WriteString(chrome)
	} else {
		sb.WriteString(clipToTerminalBounds(m.pagerContent, getTerminalHeight()-4))
	}
	return sb.String()
}

func (m Model) renderSessionsModal() string {
	var sb strings.Builder
	greenBadge := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	activeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)

	if len(m.sessionList) == 0 && m.sessionsConfirmID == "" {
		sb.WriteString("No previous sessions found in SQLite database.\n")
	} else if m.sessionsConfirmID != "" {
		dangerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
		sb.WriteString(dangerStyle.Render("⚠️  CONFIRM DELETION — this cannot be undone\n\n"))
		if m.sessionsConfirmID == "ALL" {
			sb.WriteString(fmt.Sprintf("Delete ALL %d sessions? Every conversation in every project is permanently removed.\n", len(m.sessionList)))
		} else {
			for _, sess := range m.sessionList {
				if sess.ID != m.sessionsConfirmID {
					continue
				}
				dateStr := sess.CreatedAt.Format("2006-01-02 15:04:05")
				sb.WriteString(fmt.Sprintf("Delete session %s (%s)? Its history is permanently removed.\n", sess.ID, dateStr))
				break
			}
		}
		sb.WriteString("\n[y] confirm delete · [n / ESC] cancel")
	} else {
		for i, sess := range m.sessionList {
			cursor := "  "
			if i == m.sessionsSel {
				cursor = "❯ "
			}

			statusTag := ""
			if sess.ID == m.context.SessionID() {
				statusTag = activeStyle.Render(" [active]")
			} else {
				statusTag = greenBadge.Render(" [✓ saved]")
			}

			projName := filepath.Base(sess.ProjectPath)
			if projName == "." || projName == "/" || projName == "" {
				projName = "global"
			}
			dateStr := sess.CreatedAt.Format("2006-01-02 15:04:05")
			sb.WriteString(fmt.Sprintf("%s %-20s (%s) [%s]%s\n", cursor, sess.ID, dateStr, projName, statusTag))
		}
	}

	sb.WriteString("\n[↑/↓ navigate · ENTER switch session · d delete · D delete all · PgUp/PgDn scroll · ESC close]")

	body := sb.String()
	m.sessionsViewport.SetContent(body)
	h := m.height - 4
	if h < 6 {
		h = 6
	}
	m.sessionsViewport.SetHeight(h)
	w := m.width - 8
	if w < 10 {
		w = 10
	}
	m.sessionsViewport.SetWidth(w)

	return lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2).Render(m.sessionsViewport.View())
}

func (m Model) renderModelsModal() string {
	var sb strings.Builder
	sb.WriteString("=== Select AI Model (/models) ===\n")
	if m.modelsQuery != "" {
		sb.WriteString("Filter: " + m.modelsQuery + "▏\n\n")
	} else {
		sb.WriteString("Type to filter models...\n\n")
	}

	greenBadge := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	activeBadgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)

	items := m.getModelList()
	if len(items) == 0 {
		sb.WriteString("No models found matching filter.\n")
	} else {
		for idx, item := range items {
			cursor := "  "
			if idx == m.modelsSel {
				cursor = "❯ "
			}

			statusTag := ""
			if item.ModelName == m.activeModel && item.ProviderID == m.activeProvider.Info.ID {
				statusTag = activeBadgeStyle.Render(" [active]")
			} else if m.isProviderConfigured(item.ProviderID, "") {
				statusTag = greenBadge.Render(" [✓ ready]")
			}

			sb.WriteString(fmt.Sprintf("%s %-28s (%s)%s\n", cursor, item.ModelName, provider.FriendlyName(item.ProviderID), statusTag))
		}
	}

	sb.WriteString("\n[↑/↓ navigate · ENTER apply · ESC close]")
	style := lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2)
	if m.width > 0 {
		style = style.Width(m.width - 4)
	}
	return style.Render(sb.String())
}

func (m Model) renderConnectModal() string {
	var sb strings.Builder
	sb.WriteString("=== Connect LLM Provider (/connect) ===\n\n")

	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	stepStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Bold(true)
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	switch m.connectStep {
	case 0:
		sb.WriteString(stepStyle.Render("Step 1 — Select Provider") + "\n\n")
		for i, p := range provider.BuiltinProviders {
			cursor := "  "
			if i == m.connectProviderSel {
				cursor = "❯ "
			}

			badge := ""
			if p.ID == m.activeProvider.Info.ID {
				badge = greenStyle.Render(" [✓ active]")
			} else if m.isProviderConfigured(p.ID, p.APIKeyEnvVar) {
				badge = greenStyle.Render(" [✓ configured]")
			}

			sb.WriteString(fmt.Sprintf("%s %-25s (ID: %s)%s\n", cursor, p.Name, p.ID, badge))
		}
		cursor := "  "
		if m.connectProviderSel == len(provider.BuiltinProviders) {
			cursor = "❯ "
		}
		sb.WriteString(fmt.Sprintf("%s %-25s\n", cursor, "✨ Custom Provider..."))
		sb.WriteString("\n" + hintStyle.Render("[↑/↓ navigate · ENTER select · ESC cancel]"))

	case 1:
		if m.connectCustom {
			sb.WriteString(stepStyle.Render("Step 2/5 — Custom Provider Name") + "\n\n")
			sb.WriteString(labelStyle.Render("Provider ID (lowercase, no spaces):") + "\n\n")
			sb.WriteString("  " + m.connectNameInput.View() + "\n\n")
			sb.WriteString(hintStyle.Render("[Type provider ID e.g. my-gateway · ENTER next · ESC back]"))
		} else {
			target := "Custom Provider"
			if m.connectProviderSel < len(provider.BuiltinProviders) {
				target = provider.BuiltinProviders[m.connectProviderSel].Name
			}
			sb.WriteString(stepStyle.Render("Step 2/2 — API Key") + "\n\n")
			sb.WriteString(labelStyle.Render("API Key for "+target+":") + "\n\n")
			sb.WriteString("  " + m.connectTextInput.View() + "\n\n")
			sb.WriteString(hintStyle.Render("[Type or paste API Key (Ctrl+V supported) · ENTER save · ESC back]"))
		}

	case 2:
		sb.WriteString(stepStyle.Render("Step 3/5 — API Key") + "\n\n")
		sb.WriteString(labelStyle.Render("API Key (leave empty if none):") + "\n\n")
		sb.WriteString("  " + m.connectTextInput.View() + "\n\n")
		sb.WriteString(hintStyle.Render("[Type or paste API Key (Ctrl+V supported) · ENTER next · ESC back]"))

	case 3:
		sb.WriteString(stepStyle.Render("Step 4/5 — Base URL") + "\n\n")
		sb.WriteString(labelStyle.Render("API Base URL (OpenAI-compatible /v1 endpoint):") + "\n\n")
		sb.WriteString("  " + m.connectBaseURLInput.View() + "\n\n")
		sb.WriteString(hintStyle.Render("[e.g. https://api.my-gateway.example/v1 · ENTER next · ESC back]"))

	case 4:
		sb.WriteString(stepStyle.Render("Step 5/5 — Models (optional)") + "\n\n")
		sb.WriteString(labelStyle.Render("Models JSON (can be more than 1):") + "\n\n")
		sb.WriteString("  " + m.connectModelsInput.View() + "\n\n")
		sb.WriteString(hintStyle.Render("[" +
			"{\"model-a\":{\"name\":\"Model A\",\"limit\":{\"context\":1048576,\"output\":32768}}} " +
			"or [\"model-a\",\"model-b\"]" +
			" · ENTER save · ESC back]"))
	}

	style := lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2)
	if m.width > 0 {
		style = style.Width(m.width - 4)
	}
	return style.Render(sb.String())
}

func (m Model) renderDebugModal() string {
	var sb strings.Builder
	sb.WriteString("=== Active LLM Context (/debug-context) ===\n\n")
	u, a, t := m.context.TokenBreakdown()
	sb.WriteString(fmt.Sprintf("Session ID: %s\nTotal Tokens: %s / %s\nEvents Count: %d\nTokenizer: %s\nTokens by kind (cumulative): user %d · assistant %d · tool output %d\n\n",
		m.context.SessionID(), provider.FormatTokens(m.context.TotalTokens()), provider.FormatTokens(m.context.MaxWindow()), len(m.context.Messages()), tokens.CountMethod(m.activeModel), u, a, t))

	for i, msg := range m.context.Messages() {
		sb.WriteString(fmt.Sprintf("[%d] %s:\n%s\n\n", i+1, msg.Role, msg.Content))
	}
	sb.WriteString("[ESC to return]")
	style := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(1, 2)
	if m.width > 0 {
		style = style.Width(m.width - 4)
	}
	return style.Render(sb.String())
}
