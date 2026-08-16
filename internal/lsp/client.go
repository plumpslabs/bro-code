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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	lastUsed time.Time                        // set on every clientFor — drives idle reaping
	lastDiag time.Time                        // last publishDiagnostics arrival — drives scan settle
	versions map[string]int                   // path → document version
	pushed   map[string][]protocol.Diagnostic // URI → last published diagnostics
}

// Manager owns zero or more language server connections (one per language).
// Servers are spawned lazily on first use and reaped when idle, so a long
// session does not accumulate live language-server processes (the
// "unbounded memory growth" problem other agents have).
type Manager struct {
	mu          sync.Mutex
	clients     map[string]*Client
	stop        chan struct{}
	idleTimeout time.Duration
	// ctx/cancel are manager-owned and live for the whole session, so spawned
	// language servers survive per-turn context cancellation (the per-turn
	// ctx only bounds individual queries). Canceled on Close.
	ctx    context.Context
	cancel context.CancelFunc
}

// DefaultIdleTimeout is how long a language server stays alive after its last
// use before it is shut down to free memory. 10 minutes covers a normal task
// burst while keeping idle processes bounded.
const DefaultIdleTimeout = 10 * time.Minute

// NewManager creates an empty LSP manager with an idle reaper.
func NewManager() *Manager {
	m := &Manager{
		clients:     make(map[string]*Client),
		stop:        make(chan struct{}),
		idleTimeout: DefaultIdleTimeout,
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	go m.reapIdle()
	return m
}

// reapIdle periodically shuts down language servers that have been idle past
// the timeout, so memory does not grow unboundedly across a long session.
func (m *Manager) reapIdle() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.reapIdleOnce(time.Now())
		}
	}
}

// reapIdleOnce shuts down clients idle past the timeout as of now. Extracted
// so tests can drive the reaper deterministically instead of waiting on the
// 30s tick.
func (m *Manager) reapIdleOnce(now time.Time) {
	m.mu.Lock()
	for lang, c := range m.clients {
		c.mu.Lock()
		idle := c.lastUsed.IsZero() || now.Sub(c.lastUsed) > m.idleTimeout
		c.mu.Unlock()
		if idle {
			delete(m.clients, lang)
			go c.shutdown()
		}
	}
	m.mu.Unlock()
}

// InstallHints returns, for each supported language whose server binary is
// missing, the official command to install it (empty map = all installed).
// The platform-dependent clangd hint is resolved here so callers (UI, tools)
// never need runtime.GOOS themselves.
func (m *Manager) InstallHints() map[string]string {
	hints := map[string]string{
		"go":         "go install golang.org/x/tools/gopls@latest",
		"typescript": "npm install -g typescript-language-server typescript",
		"python":     "npm install -g pyright",
		"rust":       "rustup component add rust-analyzer",
	}
	switch runtime.GOOS {
	case "darwin":
		hints["c"] = "brew install llvm && echo 'add $(brew --prefix llvm)/bin to your PATH (clangd is keg-only)'"
		hints["cpp"] = "brew install llvm && echo 'add $(brew --prefix llvm)/bin to your PATH (clangd is keg-only)'"
	case "linux":
		hints["c"] = "sudo apt-get install -y clangd"
		hints["cpp"] = "sudo apt-get install -y clangd"
	default:
		hints["c"] = "see https://clangd.llvm.org/installation"
		hints["cpp"] = "see https://clangd.llvm.org/installation"
	}

	out := make(map[string]string)
	for _, s := range specs {
		if !binaryExists(s.Command) {
			if c, ok := hints[s.Language]; ok {
				if _, dup := out[s.Language]; !dup {
					out[s.Language] = c
				}
			}
		}
	}
	return out
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
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if !closed {
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

// Close shuts down all running language servers and stops the idle reaper.
func (m *Manager) Close() {
	m.mu.Lock()
	clients := m.clients
	m.clients = make(map[string]*Client)
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	m.mu.Unlock()
	m.cancel()
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
	if c, ok := m.clients[spec.Language]; ok {
		// closed is written by shutdown and the crash watcher under c.mu;
		// read it under the same lock (order m.mu → c.mu, matching reaper).
		c.mu.Lock()
		closed := c.closed
		if !closed {
			c.lastUsed = time.Now()
		}
		c.mu.Unlock()
		if !closed {
			m.mu.Unlock()
			return c, nil
		}
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

	// Server processes are bound to the manager's long-lived ctx (not the
	// per-call ctx) so they survive between turns — re-spawning + re-indexing
	// per turn would be slow and memory-hungry.
	cmd := exec.CommandContext(m.ctx, spec.Command, spec.Args...)
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
	conn.Go(m.ctx, func(ctx context.Context, req *jsonrpc2.Request) (any, error) {
		// Capture diagnostics the server pushes so the Diagnostics tool can
		// return them without implementing the complex pull model.
		if req.Method() == "textDocument/publishDiagnostics" {
			var p protocol.PublishDiagnosticsParams
			if err := jsonrpc2.DefaultCodec.Unmarshal(req.Params(), &p); err == nil {
				c.mu.Lock()
				c.pushed[p.URI.String()] = p.Diagnostics
				c.lastDiag = time.Now()
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

	c.lastUsed = time.Now()

	// Crash detection: if the server process exits for any reason (crash,
	// OOM, user killed it), mark the client dead so the next lsp_* call
	// spawns a fresh server instead of hanging on a stale connection.
	go func() {
		_ = cmd.Wait()
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
	}()

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
	return truncate(formatHover(hover.Contents), maxHoverChars, "(hover truncated)"), nil
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
	// A cold server must index the module before it can analyze, so wait
	// until it goes quiet instead of a fixed short window.
	if err := m.waitForDiagnostics(ctx, diagSettle); err != nil {
		return "", err
	}

	u := uri.File(filepath.Clean(path)).String()
	c.mu.Lock()
	diags := append([]protocol.Diagnostic(nil), c.pushed[u]...)
	c.mu.Unlock()
	if len(diags) == 0 {
		return "No diagnostics reported for " + path + ".", nil
	}
	var sb strings.Builder
	for i, d := range diags {
		if i >= maxDiagsPerFile {
			sb.WriteString(fmt.Sprintf("... and %d more diagnostics\n", len(diags)-i))
			break
		}
		sb.WriteString(formatDiagnostic(d) + "\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

// formatDiagnostic renders one LSP diagnostic as a readable line, surfacing
// the diagnostic tags servers emit for stale code: [deprecated] marks an API
// that has a newer replacement (the classic "there is a new way" signal) and
// [unnecessary] marks dead/redundant code.
func formatDiagnostic(d protocol.Diagnostic) string {
	sev := "warning"
	if d.Severity == protocol.DiagnosticSeverityError {
		sev = "error"
	}
	tag := ""
	for _, t := range d.Tags.Slice() {
		switch t {
		case protocol.DiagnosticTagDeprecated:
			tag = " [deprecated]"
		case protocol.DiagnosticTagUnnecessary:
			tag = " [unnecessary]"
		}
	}
	return fmt.Sprintf("%s%s %d:%d  %s", sev, tag, d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message)
}

func hasTag(d protocol.Diagnostic, tag protocol.DiagnosticTag) bool {
	for _, t := range d.Tags.Slice() {
		if t == tag {
			return true
		}
	}
	return false
}

// scanSkipDirs are dependency/build/cache directories never worth opening in
// a diagnostics scan — they would drown the signal in third-party noise.
var scanSkipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, "bower_components": true,
	"dist": true, "build": true, "out": true, "coverage": true,
	".next": true, ".nuxt": true, ".turbo": true, ".cache": true,
	"__pycache__": true, ".venv": true, "venv": true, "env": true,
	".idea": true, ".vscode": true, "target": true,
	".pytest_cache": true, ".mypy_cache": true, "public": true, "assets": true,
}

// scanMaxFiles bounds how many source files are opened in one scan so it
// stays cheap on big repositories.
const scanMaxFiles = 60

// scanMaxLines bounds the rendered output of one scan.
const scanMaxLines = 80

// scanSettle caps how long ScanDiagnostics waits for servers to publish
// diagnostics after opening files. A fresh server (cold gopls on a real
// module) has to index the module before it can analyze, which easily takes
// >3s — a fixed short window would miss everything. The wait is adaptive: it
// returns as soon as every client goes quiet for scanQuietAfter and never
// exceeds scanSettle. A var so tests can shorten it.
var scanSettle = 15 * time.Second

// diagSettle caps the wait in Diagnostics (single file).
const diagSettle = 8 * time.Second

// scanPollInterval is how often the settle loop re-checks client activity.
const scanPollInterval = 200 * time.Millisecond

// scanMinWait is the minimum settle time before the quiet check may fire, so
// even a fast clean server gets a beat to push its (empty) diagnostics.
const scanMinWait = 800 * time.Millisecond

// scanQuietAfter is how long with no new publishDiagnostics before a server
// is treated as done. Cold gopls publishes in waves while indexing; the first
// quiet gap after any event means the module load finished.
const scanQuietAfter = 2 * time.Second

// diagLastActivity returns the newest publishDiagnostics arrival across all
// live clients, or the zero time if none has ever pushed.
func (m *Manager) diagLastActivity() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	var last time.Time
	for _, c := range m.clients {
		c.mu.Lock()
		if c.lastDiag.After(last) {
			last = c.lastDiag
		}
		c.mu.Unlock()
	}
	return last
}

// waitForDiagnostics blocks until servers go quiet publishing diagnostics (a
// quiet gap after the first event) or settle elapses. A cold server — which
// must index before it can analyze — is given the time it needs instead of
// a fixed short window; a warm one returns as soon as it is done.
func (m *Manager) waitForDiagnostics(ctx context.Context, settle time.Duration) error {
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(scanPollInterval):
		}
		if time.Since(start) < scanMinWait {
			continue
		}
		last := m.diagLastActivity()
		if !last.IsZero() && time.Since(last) >= scanQuietAfter {
			return nil
		}
		if time.Since(start) >= settle {
			return nil
		}
	}
}

// collectSupportedFiles walks root and returns up to max files whose language
// has an installed server, skipping dependency/build dirs. Shared by
// ScanDiagnostics and WarmUp.
func collectSupportedFiles(root string, max int) []string {
	var files []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && scanSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= max {
			return fs.SkipAll
		}
		spec := specForPath(p)
		if spec == nil || !binaryExists(spec.Command) {
			return nil
		}
		files = append(files, p)
		return nil
	})
	return files
}

// WarmUp spawns language servers for supported files under root in the
// background, so the first lsp_* call of the session is instant instead of
// paying spawn + initialize + (cache-warm) index on first use — the persistent
// per-session gap. Servers the user never touches are shut down by the idle
// reaper after idleTimeout, so unused warm-up costs nothing in the long run.
// Never blocks; errors are ignored (lazy spawn still happens on first call).
func (m *Manager) WarmUp(root string) {
	files := collectSupportedFiles(root, 10)
	seen := map[string]bool{}
	for _, f := range files {
		spec := specForPath(f)
		if spec == nil || seen[spec.Language] {
			continue
		}
		seen[spec.Language] = true
		go func(path string) {
			ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
			defer cancel()
			_, _ = m.clientFor(ctx, path)
		}(f)
	}
}

// ScanDiagnostics proactively scans a project for compiler/linter diagnostics:
// errors, warnings and deprecated usages across source files — a full health
// check without running a build. It opens at most scanMaxFiles supported files
// (dependency/build dirs skipped), gives the servers one short settle window,
// then aggregates everything they publish. Files whose language server is not
// installed are skipped silently. Returns a compact report, or a clean line
// when no issues were found.
func (m *Manager) ScanDiagnostics(ctx context.Context, root string) (string, error) {
	files := collectSupportedFiles(root, scanMaxFiles)
	if len(files) == 0 {
		return "No supported source files found (no language server available for this project).", nil
	}

	opened := make(map[string]string, len(files)) // uri → path
	clients := make(map[string]*Client, len(files))
	for _, f := range files {
		c, err := m.clientFor(ctx, f)
		if err != nil {
			continue
		}
		text, err := textAt(f)
		if err != nil {
			continue
		}
		if err := c.ensureOpen(ctx, f, text); err != nil {
			continue
		}
		u := uri.File(filepath.Clean(f)).String()
		opened[u] = f
		clients[u] = c
	}

	// One settle window for all servers to publish diagnostics.
	if err := m.waitForDiagnostics(ctx, scanSettle); err != nil {
		return "", ctx.Err()
	}

	// Gather per-file diagnostics (only files that actually have any).
	type fileDiag struct {
		rel   string
		diags []protocol.Diagnostic
	}
	order := make([]string, 0, len(opened))
	for u := range opened {
		order = append(order, u)
	}
	fds := make([]fileDiag, 0, len(order))
	for _, u := range order {
		c := clients[u]
		c.mu.Lock()
		diags := append([]protocol.Diagnostic(nil), c.pushed[u]...)
		c.mu.Unlock()
		if len(diags) == 0 {
			continue
		}
		rel, err := filepath.Rel(root, opened[u])
		if err != nil {
			rel = opened[u]
		}
		fds = append(fds, fileDiag{rel, diags})
	}
	// Surface the worst files first (most issues) so the model sees the highest
	// leverage fixes without reading every file. Per-file + total caps keep the
	// report compact enough to never hit the 40k tool-output truncation.
	sort.Slice(fds, func(i, j int) bool { return len(fds[i].diags) > len(fds[j].diags) })

	const scanMaxPerFile = 5
	var totalErrs, totalWarns, totalDeps, totalDiags int
	var out []string
	for _, fd := range fds {
		fileLines := []string{fd.rel}
		shown := 0
		for _, d := range fd.diags {
			if totalDiags >= scanMaxLines || shown >= scanMaxPerFile {
				break
			}
			fileLines = append(fileLines, "  "+formatDiagnostic(d))
			totalDiags++
			shown++
			switch {
			case d.Severity == protocol.DiagnosticSeverityError:
				totalErrs++
			case hasTag(d, protocol.DiagnosticTagDeprecated):
				totalDeps++
			default:
				totalWarns++
			}
		}
		out = append(out, strings.Join(fileLines, "\n"))
		if totalDiags >= scanMaxLines {
			break
		}
	}
	if len(out) == 0 {
		return "✅ No diagnostics found in the scanned files.", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🧹 Project diagnostics scan (%d files, %d issues shown):\n", len(opened), totalDiags)
	fmt.Fprintf(&sb, "  ✗ %d errors · ⚠ %d warnings · ♻ %d deprecated\n", totalErrs, totalWarns, totalDeps)
	if totalDiags >= scanMaxLines {
		sb.WriteString(fmt.Sprintf("  (capped at %d issues — fix these, then re-scan)\n", scanMaxLines))
	}
	sb.WriteString("\n")
	sb.WriteString(strings.Join(out, "\n"))
	sb.WriteString("\n\nUse read_file(start_line, end_line) on a flagged file for the exact span — do not read whole files.")
	return sb.String(), nil
}

// maxLocations caps how many reference locations are returned so a hot
// symbol's references (which can be hundreds) never floods the context.
const maxLocations = 40

// maxHoverChars caps hover output — servers often return long doc blocks the
// model only needs a summary of.
const maxHoverChars = 1500

// maxDiagsPerFile caps how many diagnostics are returned per file, so a
// cascade of errors (e.g. a missing import breaking 80 lines) is summarized
// instead of flooding the context.
const maxDiagsPerFile = 30

// truncate cuts s to n runes, appending a tail note when something was cut.
func truncate(s string, n int, tail string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n… " + tail
}

// formatLocations renders LSP locations as readable file:line:col entries,
// capped to keep token cost bounded — a hot symbol can have hundreds of
// references and the model rarely needs more than the first handful.
func formatLocations(locs []protocol.Location) string {
	var out []string
	for _, l := range locs {
		out = append(out, fmt.Sprintf("%s:%d:%d",
			l.URI.FsPath(), l.Range.Start.Line+1, l.Range.Start.Character+1))
	}
	sort.Strings(out)
	if len(out) > maxLocations {
		out = append(out[:maxLocations], fmt.Sprintf("... and %d more (use grep for a broader view)", len(out)-maxLocations))
	}
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

// CodeAction applies the first auto-applicable code action the server offers
// for the file's current diagnostics — e.g. auto-import, organize imports, or
// a quick-fix rewrite — preferring the server's "preferred" action. The
// resulting edits are written to disk and recorded for the turn's diff,
// undo snapshots and verification. Returns a summary of what was applied, or
// the available action titles when none carried an auto-applyable edit.
func (m *Manager) CodeAction(ctx context.Context, path string) (string, error) {
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
	// Wait for diagnostics so the context carries the current issues the
	// server will offer fixes for.
	if err := m.waitForDiagnostics(ctx, diagSettle); err != nil {
		return "", err
	}

	u := uri.File(filepath.Clean(path)).String()
	c.mu.Lock()
	diags := append([]protocol.Diagnostic(nil), c.pushed[u]...)
	c.mu.Unlock()

	clean := filepath.Clean(path)
	params := protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(clean)},
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: uint32(strings.Count(text, "\n")), Character: 0},
		},
		Context: protocol.CodeActionContext{Diagnostics: diags},
	}

	var actions []protocol.CodeAction
	if _, err := c.conn.Call(ctx, "textDocument/codeAction", params, &actions); err != nil {
		return "", err
	}
	var editable []protocol.CodeAction
	for _, a := range actions {
		if a.Edit != nil {
			editable = append(editable, a)
		}
	}
	if len(editable) == 0 {
		if len(actions) == 0 {
			return "No code actions offered by the language server for this file.", nil
		}
		var titles []string
		for _, a := range actions {
			if a.Title != "" {
				titles = append(titles, a.Title)
			}
		}
		return "Server offered actions without auto-applyable edits (commands only): " + strings.Join(titles, "; ") + ". Fix manually instead.", nil
	}

	// Prefer the server's preferred action (e.g. organize imports), else the
	// first edit-bearing action.
	chosen := editable[0]
	for _, a := range editable {
		if a.IsPreferred != nil && *a.IsPreferred {
			chosen = a
			break
		}
	}
	applied, err := applyWorkspaceEdit(chosen.Edit)
	if err != nil {
		return "", fmt.Errorf("applying code action %q: %w", chosen.Title, err)
	}
	return fmt.Sprintf("Code action %q applied.\n%s", chosen.Title, applied), nil
}

// Rename renames the symbol under the cursor across the whole project using
// the server's semantic rename, applying the resulting edits to disk. The
// verification ladder catches anything the rename missed. Returns a per-file
// summary of what changed.
func (m *Manager) Rename(ctx context.Context, path string, line, col int, newName string) (string, error) {
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

	params := protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(filepath.Clean(path))},
			Position:     positionFor(line, col),
		},
		NewName: newName,
	}
	var we protocol.WorkspaceEdit
	if _, err := c.conn.Call(ctx, "textDocument/rename", params, &we); err != nil {
		return "", err
	}
	applied, err := applyWorkspaceEdit(&we)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Renamed %q across the project.\n%s", newName, applied), nil
}

// Symbols searches the whole workspace for symbols matching a name using
// workspace/symbol — semantic lookup that does not need a cursor position
// (unlike definition/references). path anchors the language server to use.
func (m *Manager) Symbols(ctx context.Context, path, query string) (string, error) {
	c, err := m.clientFor(ctx, path)
	if err != nil {
		return "", err
	}
	params := protocol.WorkspaceSymbolParams{Query: query}
	var syms protocol.WorkspaceSymbolSlice
	if _, err := c.conn.Call(ctx, "workspace/symbol", params, &syms); err != nil {
		return "", err
	}
	if len(syms) == 0 {
		return "No symbols found.", nil
	}
	return formatSymbols(syms), nil
}

// Outline returns the hierarchical symbol tree of a file (functions, types,
// methods, fields) via documentSymbol — a semantic map of a file's shape.
func (m *Manager) Outline(ctx context.Context, path string) (string, error) {
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
	params := protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(filepath.Clean(path))},
	}
	var syms []protocol.DocumentSymbol
	if _, err := c.conn.Call(ctx, "textDocument/documentSymbol", params, &syms); err != nil {
		return "", err
	}
	if len(syms) == 0 {
		return "No symbols found in file.", nil
	}
	return formatDocumentSymbols(syms), nil
}

// formatSymbols renders workspace symbol results as readable rows.
func formatSymbols(syms []protocol.WorkspaceSymbol) string {
	var out []string
	for _, s := range syms {
		loc := "?"
		switch l := s.Location.(type) {
		case *protocol.Location:
			loc = fmt.Sprintf("%s:%d:%d", l.URI.FsPath(), l.Range.Start.Line+1, l.Range.Start.Character+1)
		case *protocol.LocationUriOnly:
			loc = l.URI.FsPath()
		}
		container := ""
		if s.ContainerName != nil && *s.ContainerName != "" {
			container = *s.ContainerName + " → "
		}
		out = append(out, fmt.Sprintf("%s%s (%s) — %s", container, s.Name, symbolKindLabel(s.Kind), loc))
	}
	if len(out) > maxLocations {
		out = append(out[:maxLocations], fmt.Sprintf("... and %d more symbols", len(out)-maxLocations))
	}
	return strings.Join(out, "\n")
}

// formatDocumentSymbols renders a file's hierarchical symbol tree with
// indentation, capped like the other format helpers.
func formatDocumentSymbols(syms []protocol.DocumentSymbol) string {
	var out []string
	var walk func(syms []protocol.DocumentSymbol, depth int)
	walk = func(syms []protocol.DocumentSymbol, depth int) {
		for _, s := range syms {
			out = append(out, fmt.Sprintf("%s%s %s — line %d",
				strings.Repeat("  ", depth), symbolKindLabel(s.Kind), s.Name, s.Range.Start.Line+1))
			walk(s.Children, depth+1)
		}
	}
	walk(syms, 0)
	if len(out) > maxLocations {
		out = append(out[:maxLocations], fmt.Sprintf("... and %d more symbols", len(out)-maxLocations))
	}
	return strings.Join(out, "\n")
}

// symbolKindLabel renders an LSP symbol kind as a short human label.
func symbolKindLabel(k protocol.SymbolKind) string {
	switch k {
	case protocol.SymbolKindFile:
		return "file"
	case protocol.SymbolKindModule:
		return "module"
	case protocol.SymbolKindNamespace:
		return "namespace"
	case protocol.SymbolKindPackage:
		return "package"
	case protocol.SymbolKindClass:
		return "class"
	case protocol.SymbolKindMethod:
		return "method"
	case protocol.SymbolKindProperty:
		return "property"
	case protocol.SymbolKindField:
		return "field"
	case protocol.SymbolKindConstructor:
		return "constructor"
	case protocol.SymbolKindEnum:
		return "enum"
	case protocol.SymbolKindInterface:
		return "interface"
	case protocol.SymbolKindFunction:
		return "function"
	case protocol.SymbolKindVariable:
		return "variable"
	case protocol.SymbolKindConstant:
		return "constant"
	case protocol.SymbolKindString:
		return "string"
	case protocol.SymbolKindNumber:
		return "number"
	case protocol.SymbolKindBoolean:
		return "boolean"
	case protocol.SymbolKindArray:
		return "array"
	case protocol.SymbolKindObject:
		return "object"
	case protocol.SymbolKindKey:
		return "key"
	case protocol.SymbolKindNull:
		return "null"
	case protocol.SymbolKindEnumMember:
		return "enum-member"
	case protocol.SymbolKindStruct:
		return "struct"
	case protocol.SymbolKindEvent:
		return "event"
	case protocol.SymbolKindOperator:
		return "operator"
	case protocol.SymbolKindTypeParameter:
		return "type-param"
	}
	return "symbol"
}
