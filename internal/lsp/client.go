// Package lsp integrates Language Server Protocol (LSP) support into BroCode.
//
// A language server (gopls for Go, typescript-language-server for TS, etc.) is
// spawned over stdio and queried for definition, references, hover and
// diagnostics — real code intelligence instead of grep-only exploration.
// The server is launched lazily on first use and kept alive for the session;
// if no server is available for the requested language the tools fail with a
// clear message telling the model to fall back to grep/glob/read_file.
package lsp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// ServerSpec describes how to spawn a language server for a language.
type ServerSpec struct {
	Language string // display name, e.g. "go"
	Command  string // binary, e.g. "gopls"
	Args     []string
}

// specs maps languages to language server binaries. Detection is by binary
// presence on PATH; add more as servers become available.
var specs = []ServerSpec{
	{Language: "go", Command: "gopls", Args: []string{"serve"}},
	{Language: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}},
	{Language: "python", Command: "pyright-langserver", Args: []string{"--stdio"}},
	{Language: "rust", Command: "rust-analyzer"},
	{Language: "c", Command: "clangd"},
	{Language: "cpp", Command: "clangd"},
}

// Client is a live connection to one language server.
type Client struct {
	server   *exec.Cmd
	conn     jsonrpc2.Conn
	rootURI  uri.URI
	lang     string
	mu       sync.Mutex
	closed   bool
	versions map[string]int                   // path → document version
	pushed   map[string][]protocol.Diagnostic // URI → last published diagnostics
}

// Manager owns zero or more language server connections (one per language).
type Manager struct {
	mu      sync.Mutex
	clients map[string]*Client
}

// NewManager creates an empty LSP manager.
func NewManager() *Manager {
	return &Manager{clients: make(map[string]*Client)}
}

// AvailableServers returns the language names whose server binary is on
// PATH — these are the languages LSP tools can actually use right now.
func (m *Manager) AvailableServers() []string {
	var out []string
	for _, s := range specs {
		if binaryExists(s.Command) && !containsStr(out, s.Language) {
			out = append(out, s.Language)
		}
	}
	return out
}

// ActiveServers returns the languages whose server process is currently
// running (spawned by a previous lsp_* tool call this session).
func (m *Manager) ActiveServers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for lang, c := range m.clients {
		if !c.closed {
			out = append(out, lang)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Close shuts down all running language servers.
func (m *Manager) Close() {
	m.mu.Lock()
	clients := m.clients
	m.clients = make(map[string]*Client)
	m.mu.Unlock()
	for _, c := range clients {
		_ = c.shutdown()
	}
}

// specForPath picks the language server spec for a file, or nil if none.
func specForPath(path string) *ServerSpec {
	ext := strings.ToLower(filepath.Ext(path))
	for i := range specs {
		switch specs[i].Language {
		case "go":
			if ext == ".go" {
				return &specs[i]
			}
		case "typescript", "javascript":
			if ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".jsx" || ext == ".mjs" || ext == ".cjs" {
				return &specs[i]
			}
		case "python":
			if ext == ".py" {
				return &specs[i]
			}
		case "rust":
			if ext == ".rs" {
				return &specs[i]
			}
		case "c":
			if ext == ".c" || ext == ".h" {
				return &specs[i]
			}
		case "cpp":
			if ext == ".cc" || ext == ".cpp" || ext == ".hpp" || ext == ".cxx" || ext == ".hxx" {
				return &specs[i]
			}
		}
	}
	return nil
}

// binaryExists reports whether the command is on PATH.
func binaryExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// clientFor returns a live client for the given file's language, starting the
// server if needed.
func (m *Manager) clientFor(ctx context.Context, path string) (*Client, error) {
	spec := specForPath(path)
	if spec == nil {
		return nil, fmt.Errorf("no language server configured for %q — use grep/glob/read_file instead", path)
	}
	if !binaryExists(spec.Command) {
		return nil, fmt.Errorf("language server %q is not installed — install %s (e.g. 'go install golang.org/x/tools/gopls@latest') or use grep/glob/read_file", spec.Command, spec.Command)
	}

	m.mu.Lock()
	if c, ok := m.clients[spec.Language]; ok && !c.closed {
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	root := findProjectRoot(abs)

	c := &Client{
		lang:     spec.Language,
		versions: make(map[string]int),
		pushed:   make(map[string][]protocol.Diagnostic),
	}

	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard // server logs are noise for the agent

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s: %w", spec.Command, err)
	}
	c.server = cmd

	rwc := &pipeRWC{in: stdin, out: stdout}
	conn := jsonrpc2.NewConn(jsonrpc2.NewStream(rwc))
	conn.Go(ctx, func(ctx context.Context, req *jsonrpc2.Request) (any, error) {
		// Capture diagnostics the server pushes so the Diagnostics tool can
		// return them without implementing the complex pull model.
		if req.Method() == "textDocument/publishDiagnostics" {
			var p protocol.PublishDiagnosticsParams
			if err := jsonrpc2.DefaultCodec.Unmarshal(req.Params(), &p); err == nil {
				c.mu.Lock()
				c.pushed[p.URI.String()] = p.Diagnostics
				c.mu.Unlock()
			}
		}
		// Other notifications are noise for the agent.
		return nil, nil
	})
	c.conn = conn

	pid := int32(os.Getpid())
	rootURIObj := uri.File(root)
	initParams := protocol.InitializeParams{
		ProcessID: &pid,
		ClientInfo: protocol.ClientInfo{
			Name:    "brocode",
			Version: protocol.NewOptional("0.1"),
		},
		RootURI: &rootURIObj,
		Capabilities: protocol.ClientCapabilities{
			TextDocument: &protocol.TextDocumentClientCapabilities{
				PublishDiagnostics: &protocol.PublishDiagnosticsClientCapabilities{},
			},
		},
	}
	var initResult protocol.InitializeResult
	if _, err := conn.Call(ctx, "initialize", initParams, &initResult); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("initialize %s failed: %w", spec.Command, err)
	}
	if err := conn.Notify(ctx, "initialized", map[string]any{}); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("initialized notification failed: %w", err)
	}

	m.mu.Lock()
	m.clients[spec.Language] = c
	m.mu.Unlock()
	return c, nil
}

// findProjectRoot walks up from a file to the nearest project marker.
func findProjectRoot(abs string) string {
	dir := filepath.Dir(abs)
	for {
		for _, marker := range []string{"go.mod", "package.json", "Cargo.toml", "pyproject.toml", ".git"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// pipeRWC adapts a cmd stdin/stdout pair into a ReadWriteCloser.
type pipeRWC struct {
	in  io.WriteCloser
	out io.Reader
}

func (p *pipeRWC) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *pipeRWC) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *pipeRWC) Close() error                { return p.in.Close() }

// shutdown gracefully terminates the server connection.
func (c *Client) shutdown() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	ctx := context.Background()
	_ = c.conn.Notify(ctx, "shutdown", nil)
	_ = c.conn.Notify(ctx, "exit", nil)
	_ = c.conn.Close()
	if c.server != nil && c.server.Process != nil {
		_ = c.server.Process.Kill()
	}
	return nil
}

// ensureOpen tells the server about the document (didOpen once, didChange
// afterwards with the latest content).
func (c *Client) ensureOpen(ctx context.Context, path, text string) error {
	c.mu.Lock()
	ver := c.versions[path]
	c.mu.Unlock()

	u := uri.File(filepath.Clean(path))
	if ver == 0 {
		params := protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{
				URI:        u,
				LanguageID: protocol.LanguageKind(c.lang),
				Version:    1,
				Text:       text,
			},
		}
		c.mu.Lock()
		c.versions[path] = 1
		c.mu.Unlock()
		return c.conn.Notify(ctx, "textDocument/didOpen", params)
	}

	c.mu.Lock()
	ver++
	c.versions[path] = ver
	c.mu.Unlock()
	params := protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u},
			Version:                int32(ver),
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangeWholeDocument{Text: text},
		},
	}
	return c.conn.Notify(ctx, "textDocument/didChange", params)
}

// textAt returns the current on-disk content of a file.
func textAt(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// positionFor converts 1-based line/col (as the model thinks) to LSP
// zero-based Position.
func positionFor(line, col int) protocol.Position {
	l := line - 1
	if l < 0 {
		l = 0
	}
	if col < 1 {
		col = 1
	}
	return protocol.Position{Line: uint32(l), Character: uint32(col - 1)}
}

// ---------------- Query operations ----------------

// Definition returns the source location(s) of the symbol under the cursor.
func (m *Manager) Definition(ctx context.Context, path string, line, col int) (string, error) {
	c, err := m.clientFor(ctx, path)
	if err != nil {
		return "", err
	}
	text, err := textAt(path)
	if err != nil {
		return "", err
	}
	if err := c.ensureOpen(ctx, path, text); err != nil {
		return "", err
	}

	var locations []protocol.Location
	params := protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(filepath.Clean(path))},
			Position:     positionFor(line, col),
		},
	}
	if _, err := c.conn.Call(ctx, "textDocument/definition", params, &locations); err != nil {
		return "", err
	}
	if len(locations) == 0 {
		return "No definition found.", nil
	}
	return formatLocations(locations), nil
}

// References returns all references to the symbol under the cursor.
func (m *Manager) References(ctx context.Context, path string, line, col int) (string, error) {
	c, err := m.clientFor(ctx, path)
	if err != nil {
		return "", err
	}
	text, err := textAt(path)
	if err != nil {
		return "", err
	}
	if err := c.ensureOpen(ctx, path, text); err != nil {
		return "", err
	}

	var locations []protocol.Location
	params := protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(filepath.Clean(path))},
			Position:     positionFor(line, col),
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: true},
	}
	if _, err := c.conn.Call(ctx, "textDocument/references", params, &locations); err != nil {
		return "", err
	}
	if len(locations) == 0 {
		return "No references found.", nil
	}
	return formatLocations(locations), nil
}

// Hover returns the hover documentation for the symbol under the cursor.
func (m *Manager) Hover(ctx context.Context, path string, line, col int) (string, error) {
	c, err := m.clientFor(ctx, path)
	if err != nil {
		return "", err
	}
	text, err := textAt(path)
	if err != nil {
		return "", err
	}
	if err := c.ensureOpen(ctx, path, text); err != nil {
		return "", err
	}

	var hover protocol.Hover
	params := protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(filepath.Clean(path))},
			Position:     positionFor(line, col),
		},
	}
	if _, err := c.conn.Call(ctx, "textDocument/hover", params, &hover); err != nil {
		return "", err
	}
	if hover.Contents == nil {
		return "No hover information.", nil
	}
	return formatHover(hover.Contents), nil
}

// Diagnostics returns the current diagnostics (errors/warnings) for a file.
func (m *Manager) Diagnostics(ctx context.Context, path string) (string, error) {
	c, err := m.clientFor(ctx, path)
	if err != nil {
		return "", err
	}
	text, err := textAt(path)
	if err != nil {
		return "", err
	}
	if err := c.ensureOpen(ctx, path, text); err != nil {
		return "", err
	}

	// Give the server time to publish diagnostics after the open/change.
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(1500 * time.Millisecond):
	}

	u := uri.File(filepath.Clean(path)).String()
	c.mu.Lock()
	diags := append([]protocol.Diagnostic(nil), c.pushed[u]...)
	c.mu.Unlock()
	if len(diags) == 0 {
		return "No diagnostics reported for " + path + ".", nil
	}
	var sb strings.Builder
	for _, d := range diags {
		sev := "warning"
		if d.Severity == protocol.DiagnosticSeverityError {
			sev = "error"
		}
		sb.WriteString(fmt.Sprintf("%s %d:%d  %s\n", sev, d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message))
	}
	return strings.TrimSpace(sb.String()), nil
}

// formatLocations renders LSP locations as readable file:line:col entries.
func formatLocations(locs []protocol.Location) string {
	var out []string
	for _, l := range locs {
		out = append(out, fmt.Sprintf("%s:%d:%d",
			l.URI.FsPath(), l.Range.Start.Line+1, l.Range.Start.Character+1))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// formatHover renders the hover contents union as plain text.
func formatHover(contents protocol.HoverContents) string {
	switch v := contents.(type) {
	case *protocol.MarkupContent:
		return strings.TrimSpace(v.Value)
	case protocol.String:
		return strings.TrimSpace(string(v))
	case *protocol.MarkedStringWithLanguage:
		return strings.TrimSpace(v.Value)
	case protocol.MarkedStringSlice:
		var parts []string
		for _, ms := range v {
			if s, ok := ms.(protocol.String); ok {
				parts = append(parts, strings.TrimSpace(string(s)))
			} else if mswl, ok := ms.(*protocol.MarkedStringWithLanguage); ok {
				parts = append(parts, strings.TrimSpace(mswl.Value))
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}
