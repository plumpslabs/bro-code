package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// newInProcessFactory returns a ClientFactory that serves every config through
// a single in-process MCP server (no subprocess spawned).
func newInProcessFactory(t *testing.T, toolName, toolDesc string) ClientFactory {
	t.Helper()
	srv := server.NewMCPServer("test-server", "1.0.0")
	srv.AddTool(mcp.NewTool(toolName, mcp.WithDescription(toolDesc), mcp.WithString("msg", mcp.Required(), mcp.Description("message"))), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		msg, _ := args["msg"].(string)
		return mcp.NewToolResultText("echo: " + msg), nil
	})
	return func(cfg ServerConfig) (Client, error) {
		return client.NewInProcessClient(srv)
	}
}

func TestLoadFileStandardShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	data := `{
		"mcpServers": {
			"filesystem": {
				"command": "npx",
				"args": ["-y", "@modelcontextprotocol/server-filesystem", "."],
				"env": {"FOO": "bar"}
			},
			"broken": {"command": ""}
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	m.loadFile(path)
	names := m.ServerNames()
	if len(names) != 1 || names[0] != "filesystem" {
		t.Fatalf("expected only 'filesystem', got %v", names)
	}
	cfg := m.configs["filesystem"]
	if cfg.Command != "npx" || len(cfg.Args) != 3 || cfg.Env["FOO"] != "bar" {
		t.Fatalf("config not parsed correctly: %+v", cfg)
	}
}

// TestLoadDefaultsBroCodeOnly proves BroCode loads its own configs and ignores external files.
func TestLoadDefaultsBroCodeOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bcDir := filepath.Join(home, ".config", "brocode")
	if err := os.MkdirAll(bcDir, 0755); err != nil {
		t.Fatal(err)
	}
	bcJSON := `{"mcpServers": {"git": {"command": "brocode-cmd"}}}`
	if err := os.WriteFile(filepath.Join(bcDir, "mcp.json"), []byte(bcJSON), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	m.LoadDefaults()
	cfg, ok := m.configs["git"]
	if !ok {
		t.Fatalf("expected git server loaded, got %v", m.ServerNames())
	}
	if cfg.Command != "brocode-cmd" {
		t.Errorf("expected command brocode-cmd, got %q", cfg.Command)
	}
}

func TestLoadFileHTTPTransport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	data := `{
		"mcpServers": {
			"remote": {
				"type": "http",
				"url": "https://mcp.example.com/sse",
				"headers": {"Authorization": "Bearer x"}
			},
			"legacy": {"url": "https://legacy.example.com/mcp"}
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	m.loadFile(path)
	names := m.ServerNames()
	if len(names) != 2 {
		t.Fatalf("expected 2 http servers, got %v", names)
	}
	cfg := m.configs["remote"]
	if cfg.Transport() != "http" || cfg.URL != "https://mcp.example.com/sse" || cfg.Headers["Authorization"] != "Bearer x" {
		t.Fatalf("http server not parsed: %+v", cfg)
	}
	legacy := m.configs["legacy"]
	if legacy.Transport() != "http" || legacy.URL != "https://legacy.example.com/mcp" {
		t.Fatalf("url-only server should default to http: %+v", legacy)
	}
}

func TestStartDiscoversAndCallsTools(t *testing.T) {
	m := NewManager()
	m.SetClientFactory(newInProcessFactory(t, "echo_tool", "Echo a message back"))
	m.AddServer("srv1", ServerConfig{Command: "ignored", Args: nil})

	m.Start(context.Background())
	if len(m.Errors()) != 0 {
		t.Fatalf("unexpected errors: %v", m.Errors())
	}
	tools := m.Tools()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	tool := tools[0]
	if tool.FullName() != "mcp__srv1__echo_tool" {
		t.Fatalf("unexpected full name: %s", tool.FullName())
	}
	if !strings.Contains(tool.Description(), "Echo a message back") {
		t.Fatalf("description not forwarded: %s", tool.Description())
	}
	params := tool.Parameters()
	if params["type"] != "object" {
		t.Fatalf("params type wrong: %v", params)
	}
	if _, ok := params["properties"]; !ok {
		t.Fatalf("params missing properties: %v", params)
	}

	out, err := tool.Execute(context.Background(), `{"msg": "halo dunia"}`)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if out != "echo: halo dunia" {
		t.Fatalf("unexpected output: %q", out)
	}

	m.Close()
}

func TestStartIsTolerantOfBrokenServer(t *testing.T) {
	m := NewManager()
	m.SetClientFactory(func(cfg ServerConfig) (Client, error) {
		return nil, os.ErrNotExist // simulate missing binary
	})
	m.AddServer("bad", ServerConfig{Command: "does-not-exist"})

	m.Start(context.Background())
	errs := m.Errors()
	if errs["bad"] == "" {
		t.Fatalf("expected an error recorded for broken server, got %v", errs)
	}
	if len(m.Tools()) != 0 {
		t.Fatalf("expected 0 tools from broken server, got %d", len(m.Tools()))
	}
}

func TestProviderDefinitions(t *testing.T) {
	m := NewManager()
	m.SetClientFactory(newInProcessFactory(t, "my_tool", "My tool description"))
	m.AddServer("api", ServerConfig{Command: "ignored"})
	m.Start(context.Background())

	defs := ProviderDefinitions(m.Tools())
	if len(defs) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(defs))
	}
	if defs[0].Name != "mcp__api__my_tool" {
		t.Fatalf("unexpected def name: %s", defs[0].Name)
	}
	if _, err := json.Marshal(defs[0].Parameters); err != nil {
		t.Fatalf("parameters not JSON-serializable: %v", err)
	}
}

// TestAddRemoveServerToFile verifies the config-edit helpers: add merges into
// the mcpServers file preserving other servers, remove deletes only the
// target, unknown names are idempotent, missing files are created, and the
// result round-trips through the manager's loader.
func TestAddRemoveServerToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	existing := `{"mcpServers": {"github": {"command": "npx", "args": ["-y", "pkg"]}}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	// Add merges, preserving the existing server.
	if err := AddServerToFile(path, "filesystem", ServerConfig{Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "github") || !strings.Contains(string(data), "filesystem") {
		t.Fatalf("add must preserve existing servers:\n%s", data)
	}

	// Remove only the target.
	if err := RemoveServerFromFile(path, "github"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "github") || !strings.Contains(string(data), "filesystem") {
		t.Fatalf("remove must keep other servers:\n%s", data)
	}

	// Unknown name and missing file are idempotent.
	if err := RemoveServerFromFile(path, "nope"); err != nil {
		t.Fatalf("unknown-name remove: %v", err)
	}
	if err := RemoveServerFromFile(filepath.Join(dir, "missing.json"), "x"); err != nil {
		t.Fatalf("missing-file remove: %v", err)
	}

	// Add to a missing file creates it, and the manager reloads it.
	p2 := filepath.Join(dir, "new.json")
	if err := AddServerToFile(p2, "remote", ServerConfig{Type: "http", URL: "https://mcp.example.com/mcp"}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager()
	mgr.loadFile(p2)
	names := mgr.ServerNames()
	if len(names) != 1 || names[0] != "remote" {
		t.Fatalf("reload mismatch: %v", names)
	}
	cfg, ok := mgr.Config("remote")
	if !ok || cfg.URL != "https://mcp.example.com/mcp" || cfg.Transport() != "http" {
		t.Fatalf("reloaded config mismatch: %+v ok=%v", cfg, ok)
	}
}
