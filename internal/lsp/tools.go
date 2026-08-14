package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/plumpslabs/bro-code/internal/tool"
)

// RegisterTools registers the five LSP intelligence tools into the registry.
// The tools share one Manager (one language server process per language) and
// fail with a clear message when no server is available so the model falls
// back to grep/glob/read_file.
func RegisterTools(r *tool.Registry, m *Manager) {
	r.Register(&DefinitionTool{m: m})
	r.Register(&ReferencesTool{m: m})
	r.Register(&HoverTool{m: m})
	r.Register(&DiagnosticsTool{m: m})
	r.Register(&ScanTool{m: m})
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
