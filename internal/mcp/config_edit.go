package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectMCPPath returns the standard project-scope MCP config path (the
// Claude/Cursor-compatible .mcp.json convention that BroCode already reads).
func ProjectMCPPath() string {
	return ".mcp.json"
}

// GlobalMCPPath returns the per-user MCP config path.
func GlobalMCPPath() string {
	return mcpGlobalPath()
}

// mcpServersMap is the on-disk shape kept as raw values so an edit preserves
// every other server and any fields BroCode does not model.
type mcpServersMap struct {
	Servers map[string]map[string]json.RawMessage `json:"mcpServers"`
}

// readServersFile loads the mcpServers map from path (empty when missing).
func readServersFile(path string) mcpServersMap {
	out := mcpServersMap{Servers: map[string]map[string]json.RawMessage{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	if out.Servers == nil {
		out.Servers = map[string]map[string]json.RawMessage{}
	}
	return out
}

func writeServersFile(path string, f mcpServersMap) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// AddServerToFile merges a server config into the mcpServers JSON file at path
// (creating the file when missing), preserving every other server and any
// fields BroCode does not model. A server with the same name is replaced.
func AddServerToFile(path, name string, cfg ServerConfig) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("server name is required")
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	f := readServersFile(path)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	f.Servers[name] = obj
	return writeServersFile(path, f)
}

// RemoveServerFromFile deletes a server from the mcpServers file at path. A
// missing file or unknown name is not an error (idempotent delete).
func RemoveServerFromFile(path, name string) error {
	f := readServersFile(path)
	if _, ok := f.Servers[name]; !ok {
		return nil
	}
	delete(f.Servers, name)
	return writeServersFile(path, f)
}
