package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/plumpslabs/bro-code/internal/tool"
)

// ---------------- Unit tests (pure functions) ----------------

func TestSpecForPath(t *testing.T) {
	cases := map[string]string{
		"main.go":      "go",
		"app.ts":       "typescript",
		"App.tsx":      "typescript",
		"index.js":     "typescript",
		"util.py":      "python",
		"lib.rs":       "rust",
		"main.c":       "c",
		"foo.hpp":      "cpp",
		"README.md":    "",
		"noext":        "",
		"style.css":    "",
		"package.json": "",
	}
	for path, want := range cases {
		spec := specForPath(path)
		if want == "" {
			if spec != nil {
				t.Errorf("specForPath(%q) = %v, want nil", path, spec)
			}
			continue
		}
		if spec == nil {
			t.Errorf("specForPath(%q) = nil, want %q", path, want)
			continue
		}
		if spec.Language != want {
			t.Errorf("specForPath(%q) language = %q, want %q", path, spec.Language, want)
		}
	}
}

func TestPositionFor(t *testing.T) {
	p := positionFor(1, 1)
	if p.Line != 0 || p.Character != 0 {
		t.Errorf("positionFor(1,1) = %+v, want {0 0}", p)
	}
	p = positionFor(5, 10)
	if p.Line != 4 || p.Character != 9 {
		t.Errorf("positionFor(5,10) = %+v, want {4 9}", p)
	}
	p = positionFor(0, 0) // out-of-range clamps
	if p.Line != 0 || p.Character != 0 {
		t.Errorf("positionFor(0,0) = %+v, want {0 0}", p)
	}
}

func TestFindProjectRoot(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	// No marker: walks to the top of the temp tree.
	got := findProjectRoot(filepath.Join(sub, "x.go"))
	if got != sub && got != dir {
		t.Logf("findProjectRoot without marker = %q (climbed as far as possible)", got)
	}

	// With a marker, stops there.
	marker := filepath.Join(dir, "a")
	if err := os.WriteFile(filepath.Join(marker, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got = findProjectRoot(filepath.Join(sub, "x.go"))
	if got != marker {
		t.Errorf("findProjectRoot = %q, want %q", got, marker)
	}
}

func TestFormatLocations(t *testing.T) {
	locs := []protocol.Location{
		{URI: mustFileURI(t, "/tmp/a.go"), Range: protocol.Range{Start: protocol.Position{Line: 3, Character: 2}}},
		{URI: mustFileURI(t, "/tmp/b.go"), Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}}},
	}
	out := formatLocations(locs)
	if !strings.Contains(out, "/tmp/a.go:4:3") || !strings.Contains(out, "/tmp/b.go:1:1") {
		t.Errorf("formatLocations = %q", out)
	}
}

func TestFormatHover(t *testing.T) {
	cases := []struct {
		name string
		in   protocol.HoverContents
		want string
	}{
		{"markup", &protocol.MarkupContent{Kind: "markdown", Value: "  **hello**  "}, "**hello**"},
		{"plain", protocol.String("plain text"), "plain text"},
		{"marked", &protocol.MarkedStringWithLanguage{Language: "go", Value: "func foo()"}, "func foo()"},
		{"slice", protocol.MarkedStringSlice{protocol.String("one"), &protocol.MarkedStringWithLanguage{Value: "two"}}, "one\n\ntwo"},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		got := formatHover(c.in)
		if got != c.want {
			t.Errorf("%s: formatHover = %q, want %q", c.name, got, c.want)
		}
	}
}

func mustFileURI(t *testing.T, path string) uri.URI {
	t.Helper()
	return uri.File(path)
}

// ---------------- Fake in-process LSP server ----------------

// TestMain re-executes the test binary as a fake language server when
// FAKE_LSP_SERVER is set, avoiding a separate compiled fixture.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_LSP_SERVER") == "1" {
		runFakeServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runFakeServer speaks a minimal LSP over stdio using Content-Length framing.
func runFakeServer() {
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)

	handle := func(req map[string]any) {
		id, hasID := req["id"]
		method, _ := req["method"].(string)
		params, _ := req["params"].(map[string]any)

		switch method {
		case "initialize":
			reply(writer, id, map[string]any{"capabilities": map[string]any{}})
		case "initialized":
			// nothing
		case "textDocument/definition", "textDocument/references":
			doc, _ := params["textDocument"].(map[string]any)
			uri, _ := doc["uri"].(string)
			locs := []map[string]any{
				{"uri": uri, "range": map[string]any{
					"start": map[string]any{"line": 0, "character": 0},
					"end":   map[string]any{"line": 0, "character": 5},
				}},
			}
			if method == "textDocument/references" {
				locs = append(locs, map[string]any{
					"uri": uri, "range": map[string]any{
						"start": map[string]any{"line": 2, "character": 1},
						"end":   map[string]any{"line": 2, "character": 3},
					},
				})
			}
			if hasID {
				reply(writer, id, locs)
			}
		case "textDocument/hover":
			if hasID {
				reply(writer, id, map[string]any{
					"contents": map[string]any{"kind": "markdown", "value": "**hover docs**"},
				})
			}
		case "textDocument/codeAction":
			doc, _ := params["textDocument"].(map[string]any)
			uri, _ := doc["uri"].(string)
			// One preferred action that rewrites the first five characters.
			if hasID {
				reply(writer, id, []map[string]any{{
					"title":       "Fix first word",
					"isPreferred": true,
					"kind":        "quickfix",
					"edit": map[string]any{
						"changes": map[string]any{
							uri: []map[string]any{{
								"range": map[string]any{
									"start": map[string]any{"line": 0, "character": 0},
									"end":   map[string]any{"line": 0, "character": 5},
								},
								"newText": "PACKA",
							}},
						},
					},
				}})
			}
		case "textDocument/rename":
			doc, _ := params["textDocument"].(map[string]any)
			uri, _ := doc["uri"].(string)
			newName, _ := params["newName"].(string)
			if hasID {
				// Rename the identifier "foo" (line 0, chars 5-8).
				reply(writer, id, map[string]any{
					"changes": map[string]any{
						uri: []map[string]any{{
							"range": map[string]any{
								"start": map[string]any{"line": 0, "character": 5},
								"end":   map[string]any{"line": 0, "character": 8},
							},
							"newText": newName,
						}},
					},
				})
			}
		case "workspace/symbol":
			if hasID {
				reply(writer, id, []map[string]any{{
					"name":          "Foo",
					"kind":          12,
					"containerName": "pkg",
					"location": map[string]any{
						"uri": uri.File("/workspace/main.go").String(),
						"range": map[string]any{
							"start": map[string]any{"line": 1, "character": 0},
							"end":   map[string]any{"line": 1, "character": 3},
						},
					},
				}})
			}
		case "textDocument/documentSymbol":
			if hasID {
				reply(writer, id, []map[string]any{{
					"name": "Foo",
					"kind": 12,
					"range": map[string]any{
						"start": map[string]any{"line": 1, "character": 0},
						"end":   map[string]any{"line": 1, "character": 3},
					},
					"selectionRange": map[string]any{
						"start": map[string]any{"line": 1, "character": 0},
						"end":   map[string]any{"line": 1, "character": 3},
					},
					"children": []map[string]any{},
				}})
			}
		case "textDocument/didOpen", "textDocument/didChange":
			doc, _ := params["textDocument"].(map[string]any)
			uri, _ := doc["uri"].(string)
			// Push diagnostics so the Diagnostics tool and ScanDiagnostics have
			// something to show: one error and one deprecated-warning (tag 2).
			go func() {
				time.Sleep(30 * time.Millisecond)
				notify(writer, "textDocument/publishDiagnostics", map[string]any{
					"uri": uri,
					"diagnostics": []map[string]any{
						{
							"range": map[string]any{
								"start": map[string]any{"line": 0, "character": 0},
								"end":   map[string]any{"line": 0, "character": 1},
							},
							"severity": 1,
							"message":  "test diagnostic",
						},
						{
							"range": map[string]any{
								"start": map[string]any{"line": 1, "character": 0},
								"end":   map[string]any{"line": 1, "character": 1},
							},
							"severity": 2,
							"message":  "legacyFunc is deprecated, use newFunc",
							"tags":     []int{2},
						},
					},
				})
			}()
		}
	}

	for {
		msg, err := readFrame(reader)
		if err != nil {
			return
		}
		var req map[string]any
		if err := json.Unmarshal(msg, &req); err != nil {
			continue
		}
		handle(req)
	}
}

// readFrame reads one Content-Length framed JSON-RPC message.
func readFrame(r *bufio.Reader) ([]byte, error) {
	var length int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			length, _ = strconv.Atoi(strings.TrimSpace(line[len("content-length:"):]))
		}
	}
	body := make([]byte, length)
	if _, err := ioReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func writeFrame(w *bufio.Writer, payload []byte) {
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload))
	w.Write(payload)
	w.Flush()
}

func reply(w *bufio.Writer, id any, result any) {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	writeFrame(w, payload)
}

func notify(w *bufio.Writer, method string, params any) {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	writeFrame(w, payload)
}

// ---------------- Integration test against the fake server ----------------

// fakeSpec replaces the built-in specs with a self re-executing binary and
// restores the original specs after the test (specs is package-global, so
// without the restore later tests would silently use the fake server).
func fakeSpec(t *testing.T, lang string) {
	t.Helper()
	old := specs
	t.Cleanup(func() { specs = old })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	specs = []ServerSpec{{Language: lang, Command: exe, Args: []string{}}}
	// The child must know it is the server.
	t.Setenv("FAKE_LSP_SERVER", "1")
}

func TestLSPDefinitionReferencesHover(t *testing.T) {
	fakeSpec(t, "go")
	m := NewManager()
	defer m.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	def, err := m.Definition(ctx, src, 1, 1)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if !strings.Contains(def, "main.go:1:1") {
		t.Errorf("Definition = %q, want location main.go:1:1", def)
	}

	refs, err := m.References(ctx, src, 1, 1)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if !strings.Contains(refs, "main.go:1:1") || !strings.Contains(refs, "main.go:3:2") {
		t.Errorf("References = %q, want both locations", refs)
	}

	hover, err := m.Hover(ctx, src, 1, 1)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if !strings.Contains(hover, "hover docs") {
		t.Errorf("Hover = %q, want hover docs", hover)
	}
}

func TestLSPDiagnostics(t *testing.T) {
	fakeSpec(t, "go")
	m := NewManager()
	defer m.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	diags, err := m.Diagnostics(ctx, src)
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if !strings.Contains(diags, "error") || !strings.Contains(diags, "test diagnostic") {
		t.Errorf("Diagnostics = %q, want error diagnostic with message", diags)
	}
	// Deprecated tag (2) must be surfaced as [deprecated] with severity.
	if !strings.Contains(diags, "[deprecated]") || !strings.Contains(diags, "legacyFunc is deprecated") {
		t.Errorf("Diagnostics = %q, want deprecated tag surfaced", diags)
	}
}

func TestFormatDiagnosticTags(t *testing.T) {
	r := protocol.Range{Start: protocol.Position{Line: 2, Character: 3}, End: protocol.Position{Line: 2, Character: 4}}

	plain := protocol.Diagnostic{Severity: protocol.DiagnosticSeverityError, Range: r, Message: protocol.String("boom")}
	if got := formatDiagnostic(plain); got != "error 3:4  boom" {
		t.Errorf("formatDiagnostic(plain) = %q", got)
	}

	dep := protocol.Diagnostic{Severity: protocol.DiagnosticSeverityWarning, Range: r, Message: protocol.String("old API"), Tags: protocol.NewDiagnosticTags(protocol.DiagnosticTagDeprecated)}
	if got := formatDiagnostic(dep); got != "warning [deprecated] 3:4  old API" {
		t.Errorf("formatDiagnostic(deprecated) = %q", got)
	}

	un := protocol.Diagnostic{Severity: protocol.DiagnosticSeverityWarning, Range: r, Message: protocol.String("dead"), Tags: protocol.NewDiagnosticTags(protocol.DiagnosticTagUnnecessary)}
	if got := formatDiagnostic(un); got != "warning [unnecessary] 3:4  dead" {
		t.Errorf("formatDiagnostic(unnecessary) = %q", got)
	}
}

func TestScanDiagnostics(t *testing.T) {
	fakeSpec(t, "go")
	m := NewManager()
	defer m.Close()

	dir := t.TempDir()
	for _, name := range []string{"main.go", "util.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package main\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// node_modules must be skipped: no diagnostics, no delay.
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "dep.go"), []byte("package pkg\n"), 0644); err != nil {
		t.Fatal(err)
	}

	old := scanSettle
	scanSettle = 1 * time.Second
	defer func() { scanSettle = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := m.ScanDiagnostics(ctx, dir)
	if err != nil {
		t.Fatalf("ScanDiagnostics: %v", err)
	}
	if !strings.Contains(out, "2 errors") {
		t.Errorf("ScanDiagnostics summary = %q, want '2 errors'", out)
	}
	if !strings.Contains(out, "2 deprecated") {
		t.Errorf("ScanDiagnostics summary = %q, want '2 deprecated'", out)
	}
	if !strings.Contains(out, "main.go") || !strings.Contains(out, "util.go") {
		t.Errorf("ScanDiagnostics = %q, want both scanned files listed", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("ScanDiagnostics = %q, node_modules must be skipped", out)
	}
}

func TestScanDiagnosticsNoFiles(t *testing.T) {
	m := NewManager()
	defer m.Close()

	out, err := m.ScanDiagnostics(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("ScanDiagnostics: %v", err)
	}
	if !strings.Contains(out, "No supported source files") {
		t.Errorf("ScanDiagnostics = %q, want 'no supported files' message", out)
	}
}

func TestFormatLocationsCapped(t *testing.T) {
	r := protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 1}}
	locs := make([]protocol.Location, 0, 100)
	for i := range 100 {
		locs = append(locs, protocol.Location{URI: mustFileURI(t, fmt.Sprintf("/tmp/f%d.go", i)), Range: r})
	}
	out := formatLocations(locs)
	if strings.Count(out, ".go:") != maxLocations {
		t.Errorf("formatLocations cap: got %d locations, want %d", strings.Count(out, ".go:"), maxLocations)
	}
	if !strings.Contains(out, "more") {
		t.Errorf("formatLocations = %q, want truncation note", out)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10, "tail"); got != "hello" {
		t.Errorf("truncate short = %q", got)
	}
	got := truncate("hello world", 5, "cut")
	if !strings.Contains(got, "hello") || !strings.Contains(got, "cut") {
		t.Errorf("truncate long = %q, want prefix + tail note", got)
	}
}

func TestIdleReaperShutsDownIdleServer(t *testing.T) {
	fakeSpec(t, "go")
	m := NewManager()
	defer m.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Spawn the server, then pretend it has been idle far past the timeout.
	if _, err := m.Definition(ctx, src, 1, 1); err != nil {
		t.Fatalf("Definition: %v", err)
	}
	m.mu.Lock()
	c := m.clients["go"]
	m.mu.Unlock()
	if c == nil {
		t.Fatal("expected a spawned client")
	}

	m.idleTimeout = time.Millisecond
	m.reapIdleOnce(time.Now().Add(10 * time.Minute))

	// Give the shutdown goroutine a moment, then verify the client is gone
	// and marked closed.
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	_, stillThere := m.clients["go"]
	m.mu.Unlock()
	if stillThere {
		t.Error("idle server was not reaped")
	}

	// A recent client must NOT be reaped.
	m2 := NewManager()
	defer m2.Close()
	if _, err := m2.Definition(ctx, src, 1, 1); err != nil {
		t.Fatalf("Definition (m2): %v", err)
	}
	m2.reapIdleOnce(time.Now())
	m2.mu.Lock()
	_, stillThere = m2.clients["go"]
	m2.mu.Unlock()
	if !stillThere {
		t.Error("recently used server was reaped too early")
	}
}

// TestInstallHints covers the auto-install surface: hints exist for known
// languages with a missing server, and the clangd hint is platform-
// appropriate. Uses an impossible binary so the test is machine-independent.
func TestInstallHints(t *testing.T) {
	oldSpecs := specs
	defer func() { specs = oldSpecs }()
	specs = []ServerSpec{
		{Language: "go", Command: "definitely-not-installed-gopls"},
		{Language: "typescript", Command: "definitely-not-installed-tsserver"},
		{Language: "c", Command: "definitely-not-installed-clangd"},
	}

	m := NewManager()
	defer m.Close()

	hints := m.InstallHints()
	// go/typescript have known install commands.
	for _, lang := range []string{"go", "typescript"} {
		if _, ok := hints[lang]; !ok {
			t.Errorf("expected install hint for %s (missing server should be listed)", lang)
		}
	}
	// clangd hint must exist and be platform-appropriate.
	if c, ok := hints["c"]; !ok || c == "" {
		t.Errorf("expected platform clangd hint, got %q", c)
	} else if c != "see https://clangd.llvm.org/installation" && !strings.Contains(c, "brew") && !strings.Contains(c, "apt") {
		t.Errorf("unexpected clangd hint %q", c)
	}
	// No unknown language gets a hint.
	for lang, c := range hints {
		switch lang {
		case "go", "typescript", "c":
		default:
			t.Errorf("unexpected hint for %s: %q", lang, c)
		}
	}
}

func TestServerCrashRespawnsOnNextCall(t *testing.T) {
	fakeSpec(t, "go")
	m := NewManager()
	defer m.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := m.Definition(ctx, src, 1, 1); err != nil {
		t.Fatalf("Definition #1: %v", err)
	}

	// Kill the server process out from under the manager (simulating a crash).
	m.mu.Lock()
	c := m.clients["go"]
	m.mu.Unlock()
	if c == nil || c.server == nil || c.server.Process == nil {
		t.Fatal("expected a running server process")
	}
	if err := c.server.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// Give the Wait() goroutine time to mark the client dead.
	time.Sleep(300 * time.Millisecond)

	// The next call must transparently spawn a fresh server and succeed.
	if _, err := m.Definition(ctx, src, 1, 1); err != nil {
		t.Fatalf("Definition after crash: %v", err)
	}
	m.mu.Lock()
	c2 := m.clients["go"]
	m.mu.Unlock()
	if c2 == nil || c2.closed {
		t.Error("expected a fresh, live client after crash")
	}
}

func TestLSPNoServerConfigured(t *testing.T) {
	m := NewManager()
	defer m.Close()

	// Unknown extension → no spec → clear fallback error.
	ctx := context.Background()
	_, err := m.Definition(ctx, "/tmp/foo.xyz", 1, 1)
	if err == nil || !strings.Contains(err.Error(), "no language server configured") {
		t.Errorf("want 'no language server configured' error, got %v", err)
	}
}

func TestLSPBinaryMissing(t *testing.T) {
	// A command that cannot exist on PATH.
	orig := specs
	specs = []ServerSpec{{Language: "go", Command: "brocode-no-such-binary-xyz", Args: []string{"serve"}}}
	defer func() { specs = orig }()

	m := NewManager()
	defer m.Close()
	_, err := m.Definition(context.Background(), "/tmp/main.go", 1, 1)
	if err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Errorf("want 'is not installed' error, got %v", err)
	}
}

func TestLSPAvailableServers(t *testing.T) {
	m := NewManager()
	defer m.Close()

	avail := m.AvailableServers()
	// Whatever is installed, the list must never contain duplicates and the
	// active list starts empty before any tool call.
	seen := map[string]bool{}
	for _, l := range avail {
		if seen[l] {
			t.Errorf("duplicate server %q in AvailableServers", l)
		}
		seen[l] = true
	}
	if len(m.ActiveServers()) != 0 {
		t.Errorf("ActiveServers before any call = %v, want empty", m.ActiveServers())
	}
}

// TestLSPManagerCloseIdempotent ensures Close can be called repeatedly and
// never blocks.
func TestLSPManagerCloseIdempotent(t *testing.T) {
	m := NewManager()
	m.Close()
	m.Close()
	var wg sync.WaitGroup
	wg.Go(func() {
		m.Close()
	})
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked")
	}
}

// ---------------- CodeAction / Rename / Symbols / Outline ----------------

func TestLSPCodeActionAppliesEdit(t *testing.T) {
	fakeSpec(t, "go")
	m := NewManager()
	defer m.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tool.ResetChanges()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := m.CodeAction(ctx, src)
	if err != nil {
		t.Fatalf("CodeAction: %v", err)
	}
	if !strings.Contains(out, "Fix first word") {
		t.Errorf("CodeAction output = %q, want applied action title", out)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "PACKA") {
		t.Errorf("file after CodeAction = %q, want PACKA applied", data)
	}
	// The edit must be recorded for the turn's diff/undo/verification.
	changes := tool.PeekChanges()
	if len(changes) != 1 || changes[0].Path != src || changes[0].Action != "modified" {
		t.Errorf("RecordChange = %+v, want one modified change for %s", changes, src)
	}
}

func TestLSPRename(t *testing.T) {
	fakeSpec(t, "go")
	m := NewManager()
	defer m.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("func foo() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tool.ResetChanges()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := m.Rename(ctx, src, 1, 6, "bar")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if !strings.Contains(out, "Renamed \"bar\"") {
		t.Errorf("Rename output = %q, want rename summary", out)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "func bar() {}\n" {
		t.Errorf("file after Rename = %q, want %q", data, "func bar() {}\n")
	}
}

func TestLSPSymbols(t *testing.T) {
	fakeSpec(t, "go")
	m := NewManager()
	defer m.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := m.Symbols(ctx, src, "Foo")
	if err != nil {
		t.Fatalf("Symbols: %v", err)
	}
	for _, want := range []string{"Foo", "pkg →", "function", "main.go:2:1"} {
		if !strings.Contains(out, want) {
			t.Errorf("Symbols output = %q, missing %q", out, want)
		}
	}
}

func TestLSPOutline(t *testing.T) {
	fakeSpec(t, "go")
	m := NewManager()
	defer m.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := m.Outline(ctx, src)
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}
	for _, want := range []string{"function Foo", "line 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("Outline output = %q, missing %q", out, want)
		}
	}
}

// TestAutoFixToolRegistered verifies the batch auto-fix tool is wired into the
// registry (the "one shot, not per-file" companion to lsp_fix).
func TestAutoFixToolRegistered(t *testing.T) {
	r := tool.NewRegistry()
	RegisterTools(r, nil)
	if r.ToolByName("lsp_autofix") == nil {
		t.Fatal("lsp_autofix not registered")
	}
}
