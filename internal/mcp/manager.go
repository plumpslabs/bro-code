// Package mcp integrates Model Context Protocol (MCP) servers into BroCode.
//
// MCP servers are spawned as stdio subprocesses (the standard convention used
// by opencode, Claude, Cursor, etc.). Every tool a server exposes becomes a
// native BroCode tool named `mcp__<server>__<tool>`, so the model can call it
// exactly like any built-in tool. Config is read from the standard locations,
// highest priority first:
//
//	.brocode/mcp.json          (project, BroCode-specific)
//	.mcp.json                  (project, standard MCP convention)
//	~/.config/brocode/mcp.json (global, BroCode-specific)
//	~/.config/opencode/opencode.jsonc (the opencode "mcp" block, fallback only;
//	                                skipped when BROCODE_NO_OPENCODE=1)
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/plumpslabs/bro-code/internal/provider"
)

// ServerConfig is a single MCP server definition: the command to spawn plus
// its arguments and environment overrides.
type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// Client is the minimal MCP client surface BroCode needs. *client.Client
// satisfies it; tests inject an in-process client instead of spawning a
// subprocess.
type Client interface {
	Initialize(ctx context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error)
	ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error)
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
	Close() error
}

// ClientFactory creates a client for a server config. The default factory
// spawns the server as a stdio subprocess.
type ClientFactory func(cfg ServerConfig) (Client, error)

// Manager owns all configured MCP servers and the tools they expose.
type Manager struct {
	mu        sync.Mutex
	configs   map[string]ServerConfig
	clients   map[string]Client
	tools     []*MCPTool
	errors    map[string]string
	newClient ClientFactory
	started   bool
}

// NewManager creates an empty MCP manager. Call LoadDefaults then Start, or
// AddServer for programmatic config.
func NewManager() *Manager {
	return &Manager{
		configs:   make(map[string]ServerConfig),
		clients:   make(map[string]Client),
		errors:    make(map[string]string),
		newClient: stdioClientFactory,
	}
}

// SetClientFactory overrides how clients are created (used by tests).
func (m *Manager) SetClientFactory(f ClientFactory) {
	if f != nil {
		m.newClient = f
	}
}

// stdioClientFactory spawns the server command as a subprocess with
// stdin/stdout pipes (the standard MCP stdio transport).
func stdioClientFactory(cfg ServerConfig) (Client, error) {
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	return client.NewStdioMCPClient(cfg.Command, env, cfg.Args...)
}

// AddServer registers a server config under the given name (later adds
// override earlier ones with the same name).
func (m *Manager) AddServer(name string, cfg ServerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[name] = cfg
}

// ServerNames returns the configured server names in stable (sorted) order.
func (m *Manager) ServerNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.configs))
	for n := range m.configs {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

// Errors returns per-server startup errors keyed by server name (empty value
// means the server started cleanly).
func (m *Manager) Errors() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.errors))
	for k, v := range m.errors {
		out[k] = v
	}
	return out
}

// Start connects to every configured server, runs the MCP handshake and
// discovers its tools. A failing server is recorded in Errors() but does not
// abort the others — one broken server must never kill the whole session.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return
	}
	m.started = true

	for name, cfg := range m.configs {
		c, err := m.newClient(cfg)
		if err != nil {
			m.errors[name] = "spawn failed: " + err.Error()
			continue
		}
		// Bound the handshake per server: Start runs with a background ctx, so
		// a hung MCP server (no response on init/list) must not block startup
		// forever — one broken server is skipped, not fatal.
		handshakeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if _, err := c.Initialize(handshakeCtx, mcp.InitializeRequest{
			Params: mcp.InitializeParams{
				ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
				ClientInfo: mcp.Implementation{
					Name:    "brocode",
					Version: "0.1",
				},
			},
		}); err != nil {
			cancel()
			_ = c.Close()
			m.errors[name] = "initialize failed: " + err.Error()
			continue
		}

		toolsRes, err := c.ListTools(handshakeCtx, mcp.ListToolsRequest{})
		cancel()
		if err != nil {
			_ = c.Close()
			m.errors[name] = "list tools failed: " + err.Error()
			continue
		}

		m.clients[name] = c
		for _, t := range toolsRes.Tools {
			m.tools = append(m.tools, &MCPTool{
				server: name,
				client: c,
				def:    t,
			})
		}
	}
}

// Tools returns all discovered MCP tools flattened across servers.
func (m *Manager) Tools() []*MCPTool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tools
}

// Close shuts down all running servers.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, c := range m.clients {
		_ = c.Close()
		delete(m.clients, name)
	}
	m.tools = nil
}

// ---------------- Config loading ----------------

// mcpServersFile is the standard JSON shape for .mcp.json and friends.
type mcpServersFile struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}

// opencodeConfig mirrors the "mcp" block of opencode.jsonc, whose command is
// an array (["npx", "-y", "pkg"]) and env key is "environment".
type opencodeConfig struct {
	MCP map[string]struct {
		Type        string            `json:"type"`
		Command     json.RawMessage   `json:"command"`
		Environment map[string]string `json:"environment,omitempty"`
		Env         map[string]string `json:"env,omitempty"`
	} `json:"mcp"`
}

// LoadDefaults reads MCP servers from all standard locations. Precedence
// (highest wins): project BroCode → project .mcp.json → global BroCode →
// opencode.jsonc. BroCode's own configs are authoritative; the opencode block
// only contributes servers BroCode knows nothing about (and is skipped when
// BROCODE_NO_OPENCODE=1 for fully standalone operation). It never errors —
// missing files simply contribute nothing.
func (m *Manager) LoadDefaults() {
	// 1. opencode.jsonc "mcp" block FIRST so BroCode's own configs below
	// override it — BroCode is authoritative.
	if provider.OpenCodeImportEnabled() {
		m.loadOpenCodeConfig()
	}
	// 2. Global BroCode config.
	m.loadFile(mcpGlobalPath())
	// 3. Project .mcp.json (standard MCP convention).
	m.loadFile(".mcp.json")
	// 4. Project BroCode config — highest priority.
	m.loadFile(filepath.Join(".brocode", "mcp.json"))
}

func mcpGlobalPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "brocode", "mcp.json")
}

// loadFile parses a standard mcpServers JSON file into the manager.
func (m *Manager) loadFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f mcpServersFile
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	for name, cfg := range f.Servers {
		if strings.TrimSpace(cfg.Command) == "" {
			continue
		}
		m.AddServer(name, cfg)
	}
}

// loadOpenCodeConfig reads the "mcp" block from ~/.config/opencode/opencode.jsonc.
func (m *Manager) loadOpenCodeConfig() {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.jsonc"))
	if err != nil {
		return
	}
	var cfg opencodeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return
	}
	for name, s := range cfg.MCP {
		if s.Type != "" && s.Type != "stdio" {
			continue // only stdio transport is supported
		}
		cmd, args := parseCommand(s.Command)
		if cmd == "" {
			continue
		}
		env := s.Environment
		if env == nil {
			env = s.Env
		}
		m.AddServer(name, ServerConfig{Command: cmd, Args: args, Env: env})
	}
}

// parseCommand accepts either a plain string or an array of strings.
func parseCommand(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var single string
	if json.Unmarshal(raw, &single) == nil && single != "" {
		return single, nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		return list[0], list[1:]
	}
	return "", nil
}

// ---------------- MCPTool (native tool wrapper) ----------------

// toolNameRe sanitizes MCP tool names (which may contain "/", "-", etc.) into
// identifiers the model can emit: mcp__<server>__<tool>.
var toolNameRe = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// MCPTool adapts a remote MCP tool to the native tool.Tool interface.
type MCPTool struct {
	server string
	client Client
	def    mcp.Tool
}

// FullName returns the namespaced tool name registered in the registry.
func (t *MCPTool) FullName() string {
	return "mcp__" + toolNameRe.ReplaceAllString(t.server, "_") + "__" + toolNameRe.ReplaceAllString(t.def.Name, "_")
}

// Server returns the MCP server name this tool belongs to.
func (t *MCPTool) Server() string { return t.server }

// ToolName returns the raw tool name as reported by the MCP server.
func (t *MCPTool) ToolName() string { return t.def.Name }

// Name implements tool.Tool.
func (t *MCPTool) Name() string { return t.FullName() }

// Description implements tool.Tool.
func (t *MCPTool) Description() string {
	desc := t.def.Description
	if desc == "" {
		desc = t.def.Title
	}
	if desc == "" {
		desc = "MCP tool provided by server " + t.server
	}
	return fmt.Sprintf("[MCP:%s] %s", t.server, desc)
}

// Parameters implements tool.Tool, converting the MCP JSON schema.
func (t *MCPTool) Parameters() map[string]any {
	schema := t.def.InputSchema
	params := map[string]any{
		"type": "object",
	}
	if schema.Properties != nil {
		params["properties"] = schema.Properties
	}
	if len(schema.Required) > 0 {
		params["required"] = schema.Required
	}
	return params
}

// Execute implements tool.Tool, forwarding the call to the MCP server and
// returning the text content of the result.
func (t *MCPTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args map[string]any
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("mcp tool %s: invalid arguments: %w", t.def.Name, err)
		}
	}

	// Bound each call: a hung MCP server (no response, network stall) must not
	// stall the agent loop past the timeout — the tool call fails cleanly and
	// the model adapts.
	callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	res, err := t.client.CallTool(callCtx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      t.def.Name,
			Arguments: args,
		},
	})
	if err != nil {
		return "", fmt.Errorf("mcp tool %s: %w", t.def.Name, err)
	}

	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	out := strings.TrimSpace(strings.Join(parts, "\n"))
	if res.IsError {
		if out == "" {
			out = "MCP tool returned an error (no message)."
		}
		return out, fmt.Errorf("mcp tool %s failed: %s", t.def.Name, out)
	}
	if out == "" {
		return "Tool executed successfully with no output.", nil
	}
	return out, nil
}

// ProviderDefinitions converts MCP tools into provider tool definitions for
// the LLM (used by the engine alongside built-in definitions).
func ProviderDefinitions(tools []*MCPTool) []provider.ToolDefinition {
	defs := make([]provider.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, provider.ToolDefinition{
			Name:        t.FullName(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return defs
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
