package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/plumpslabs/bro-code/internal/tool"
)

// RegisterTools registers the LSP intelligence tools into the registry. The
// tools share one Manager (one language server process per language) and fail
// with a clear message when no server is available so the model falls back to
// grep/glob/read_file.
func RegisterTools(r *tool.Registry, m *Manager) {
	r.Register(&DefinitionTool{m: m})
	r.Register(&ReferencesTool{m: m})
	r.Register(&HoverTool{m: m})
	r.Register(&DiagnosticsTool{m: m})
	r.Register(&ScanTool{m: m})
	r.Register(&FixTool{m: m})
	r.Register(&AutoFixTool{m: m})
	r.Register(&RenameTool{m: m})
	r.Register(&SymbolsTool{m: m})
	r.Register(&OutlineTool{m: m})
}

// posArgs is the shared argument shape for position-based tools.
type posArgs struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

func paramsWithPos() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "File path containing the symbol"},
			"line": map[string]any{"type": "integer", "description": "1-based line of the symbol"},
			"col":  map[string]any{"type": "integer", "description": "1-based column of the symbol"},
		},
		"required": []string{"path", "line", "col"},
	}
}

func parsePos(argsJSON string) (posArgs, error) {
	var args posArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return args, err
	}
	if args.Path == "" {
		return args, fmt.Errorf("path is required")
	}
	if args.Line < 1 {
		args.Line = 1
	}
	if args.Col < 1 {
		args.Col = 1
	}
	return args, nil
}

// DefinitionTool — jump to where a symbol is defined.
type DefinitionTool struct{ m *Manager }

func (t *DefinitionTool) Name() string { return "lsp_definition" }
func (t *DefinitionTool) Description() string {
	return "Jump to the definition of a symbol (function, type, variable) using the language server. Returns the file:line:col location(s). Use when you need to find where something is actually defined, not just mentioned."
}
func (t *DefinitionTool) Parameters() map[string]any { return paramsWithPos() }
func (t *DefinitionTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	args, err := parsePos(argsJSON)
	if err != nil {
		return "", err
	}
	return t.m.Definition(ctx, args.Path, args.Line, args.Col)
}

// ReferencesTool — find every place a symbol is used.
type ReferencesTool struct{ m *Manager }

func (t *ReferencesTool) Name() string { return "lsp_references" }
func (t *ReferencesTool) Description() string {
	return "Find all references to a symbol across the project using the language server. Returns up to 40 file:line:col locations. Use only when grep cannot tell real references from string matches — grep is cheaper for broad searches."
}
func (t *ReferencesTool) Parameters() map[string]any { return paramsWithPos() }
func (t *ReferencesTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	args, err := parsePos(argsJSON)
	if err != nil {
		return "", err
	}
	return t.m.References(ctx, args.Path, args.Line, args.Col)
}

// HoverTool — documentation and type info for a symbol.
type HoverTool struct{ m *Manager }

func (t *HoverTool) Name() string { return "lsp_hover" }
func (t *HoverTool) Description() string {
	return "Get hover documentation, signature and type information for a symbol using the language server (output truncated to 1500 chars). Use to understand what a function expects and returns — prefer reading the definition for full context."
}
func (t *HoverTool) Parameters() map[string]any { return paramsWithPos() }
func (t *HoverTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	args, err := parsePos(argsJSON)
	if err != nil {
		return "", err
	}
	return t.m.Hover(ctx, args.Path, args.Line, args.Col)
}

// DiagnosticsTool — compiler/linter errors for a file.
type DiagnosticsTool struct{ m *Manager }

func (t *DiagnosticsTool) Name() string { return "lsp_diagnostics" }
func (t *DiagnosticsTool) Description() string {
	return "Get compiler and linter diagnostics (errors, warnings, deprecated usages) for a specific file using the language server (up to 30 per file). Use on edited files when the project's own typecheck CLI is not conclusive."
}
func (t *DiagnosticsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "File path to check"},
		},
		"required": []string{"path"},
	}
}
func (t *DiagnosticsTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	return t.m.Diagnostics(ctx, args.Path)
}

// ScanTool — project-wide health check: errors, warnings, deprecated APIs.
type ScanTool struct{ m *Manager }

func (t *ScanTool) Name() string { return "lsp_scan" }
func (t *ScanTool) Description() string {
	return "Scan the whole project for type errors, warnings and deprecated API usages using language servers (no build needed; scans at most 60 files). Returns a per-file report plus error/warning/deprecated counts. EXPENSIVE (starts language servers, ~3s, verbose output) — call at most ONCE per task: at the start to see what is already broken before editing, or after large edits to verify nothing regressed. Prefer the project's own typecheck/lint CLI (go build, tsc --noEmit, bun test) when available."
}
func (t *ScanTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Optional directory to scan (default: current working directory)"},
		},
	}
}
func (t *ScanTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	root := args.Path
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return t.m.ScanDiagnostics(ctx, abs)
}

// FixTool — apply the language server's auto-fixable code actions to a file.
type FixTool struct{ m *Manager }

func (t *FixTool) Name() string { return "lsp_fix" }
func (t *FixTool) Description() string {
	return "Apply auto-fixable code actions from the language server to a file (auto-import, organize imports, quick-fix rewrites). The server's preferred action is applied and the edits are written to disk — the project's own checks still verify the result. Use on files whose diagnostics the server can fix automatically; call again if more actions remain."
}
func (t *FixTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "File path to apply quick-fixes to"},
		},
		"required": []string{"path"},
	}
}
func (t *FixTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	return t.m.CodeAction(ctx, args.Path)
}

// AutoFixTool — apply all auto-fixable quick-fixes across the project in one shot.
type AutoFixTool struct{ m *Manager }

func (t *AutoFixTool) Name() string { return "lsp_autofix" }
func (t *AutoFixTool) Description() string {
	return "Apply ALL auto-fixable language-server quick-fixes (imports, organize-imports, trivial rewrites) across the project — or one file — in a SINGLE call, instead of invoking lsp_fix per file. Use at the END of a 'fix warnings/lint' task after your edits, or to batch-clear fixable diagnostics repo-wide. This is a repo-wide MUTATING action; the project's own checks still verify the result. Pass target \"all\" (default) or a specific file path."
}
func (t *AutoFixTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{"type": "string", "description": "Scope of the fix: \"all\" (default) to fix every file with diagnostics, or a specific file path to fix just that file"},
		},
	}
}
func (t *AutoFixTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if args.Target != "" && args.Target != "all" {
		abs, err := filepath.Abs(args.Target)
		if err != nil {
			return "", err
		}
		c, err := t.m.clientFor(ctx, abs)
		if err != nil {
			return "", err
		}
		text, err := textAt(abs)
		if err != nil {
			return "", err
		}
		if err := c.ensureOpen(ctx, abs, text); err != nil {
			return "", err
		}
		if err := t.m.waitForDiagnostics(ctx, diagSettle); err != nil {
			return "", err
		}
		applied, summary, ferr := t.m.autoFixFile(ctx, c, abs, text)
		if ferr != nil {
			return "", ferr
		}
		if applied == 0 {
			return "✅ No auto-fixable diagnostics in " + args.Target, nil
		}
		return fmt.Sprintf("🔧 %s: %d fix(es) applied\n%s", args.Target, applied, summary), nil
	}
	abs, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	return t.m.AutoFixAll(ctx, abs)
}

// RenameTool — semantic rename across the project.
type RenameTool struct{ m *Manager }

func (t *RenameTool) Name() string { return "lsp_rename" }
func (t *RenameTool) Description() string {
	return "Rename a symbol across the whole project using the language server's semantic rename (updates every definition and reference — unlike sed/grep, it understands the code). Edits are written to disk and the project's checks then verify nothing broke. Position must be ON the symbol name. Use when a rename must be consistent project-wide."
}
func (t *RenameTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "File path containing the symbol"},
			"line":    map[string]any{"type": "integer", "description": "1-based line of the symbol name"},
			"col":     map[string]any{"type": "integer", "description": "1-based column of the symbol name"},
			"newName": map[string]any{"type": "string", "description": "New name for the symbol"},
		},
		"required": []string{"path", "line", "col", "newName"},
	}
}
func (t *RenameTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	args, err := parsePos(argsJSON)
	if err != nil {
		return "", err
	}
	var full struct {
		posArgs
		NewName string `json:"newName"`
	}
	full.posArgs = args
	if err := json.Unmarshal([]byte(argsJSON), &full); err != nil {
		return "", err
	}
	if full.NewName == "" {
		return "", fmt.Errorf("newName is required")
	}
	return t.m.Rename(ctx, args.Path, args.Line, args.Col, full.NewName)
}

// SymbolsTool — find symbols by name, no cursor needed.
type SymbolsTool struct{ m *Manager }

func (t *SymbolsTool) Name() string { return "lsp_symbols" }
func (t *SymbolsTool) Description() string {
	return "Search the whole workspace for symbols (functions, types, methods) by NAME using the language server — no cursor position needed (unlike lsp_definition). Returns name, kind, container and file:line for up to 40 matches. Use to locate a symbol by name when grep matches too much or misses semantic boundaries."
}
func (t *SymbolsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "description": "Any file of the target language (anchors which language server to ask)"},
			"query": map[string]any{"type": "string", "description": "Symbol name (or substring) to search for"},
		},
		"required": []string{"path", "query"},
	}
}
func (t *SymbolsTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path  string `json:"path"`
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if args.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	return t.m.Symbols(ctx, args.Path, args.Query)
}

// OutlineTool — hierarchical symbol tree of a file.
type OutlineTool struct{ m *Manager }

func (t *OutlineTool) Name() string { return "lsp_outline" }
func (t *OutlineTool) Description() string {
	return "Get the hierarchical symbol outline of a file (functions, types, methods, fields with their line numbers) using the language server. A quick semantic map of a file's shape — use before editing a file you haven't seen to know where things live."
}
func (t *OutlineTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "File path to outline"},
		},
		"required": []string{"path"},
	}
}
func (t *OutlineTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	return t.m.Outline(ctx, args.Path)
}
