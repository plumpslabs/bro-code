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

func TestLoadOpenCodeConfigShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data := `{
		"mcp": {
			"git": {
				"type": "stdio",
				"command": ["npx", "-y", "@modelcontextprotocol/server-git"],
				"environment": {"TOKEN": "abc"}
			},
			"http-only": {"type": "sse", "url": "https://example.com"}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	m.loadOpenCodeConfig()
	names := m.ServerNames()
	if len(names) != 1 || names[0] != "git" {
		t.Fatalf("expected only stdio 'git' server (sse skipped), got %v", names)
	}
	cfg := m.configs["git"]
	if cfg.Command != "npx" || len(cfg.Args) != 2 || cfg.Env["TOKEN"] != "abc" {
		t.Fatalf("opencode config not parsed correctly: %+v", cfg)
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
