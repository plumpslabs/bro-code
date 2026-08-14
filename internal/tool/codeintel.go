package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plumpslabs/bro-code/internal/search"
)

// CodeSymbolsTool returns a compact structural map (functions, structs,
// classes, methods + line numbers) of one or more files — the agent sees a
// file's shape without reading its whole body into context.
type CodeSymbolsTool struct{}

func (t *CodeSymbolsTool) Name() string { return "code_symbols" }
func (t *CodeSymbolsTool) Description() string {
	return "Return a compact map of symbols (functions, methods, structs, classes, interfaces, enums) with their line numbers for one or more files. Use to understand a file's structure quickly without reading the whole file — then read_file the exact lines you need."
}
func (t *CodeSymbolsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"paths": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "File paths to extract symbols from (1-5 files)",
			},
		},
		"required": []string{"paths"},
	}
}
func (t *CodeSymbolsTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if len(args.Paths) == 0 {
		return "", fmt.Errorf("code_symbols requires at least one path")
	}
	if len(args.Paths) > 5 {
		args.Paths = args.Paths[:5]
	}

	// Resolve relative to cwd and verify existence.
	resolved := make([]string, 0, len(args.Paths))
	for _, p := range args.Paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		if st, err := os.Stat(abs); err != nil || st.IsDir() {
			continue // skip missing or directory paths
		}
		resolved = append(resolved, abs)
	}
	if len(resolved) == 0 {
		return "No readable files found. Check the paths and try again.", nil
	}

	out := search.FormatSymbolSummary(resolved)
	if strings.TrimSpace(out) == "" {
		return "No symbols found in the given files (unsupported language or empty files).", nil
	}
	return CapOutput(out), nil
}

// CodeLocateTool answers repo-wide "where is X and who uses it" questions in
// one call using the persistent session index (symbols + reference graph) — no
// LSP spawn and no full-file reads. This is the repo-map equivalent that lets
// the model navigate precisely before reading anything.
type CodeLocateTool struct {
	Index *search.GlobalIndex
}

func (t *CodeLocateTool) Name() string { return "code_locate" }
func (t *CodeLocateTool) Description() string {
	return "Locate a symbol across the whole codebase in one call: where it is defined (file:line, kind) AND which files reference or import it. Use BEFORE reading files to navigate precisely — e.g. code_locate(name: \"ConversationService\") returns its class/method locations plus every file that uses it. Indexed once per session, instant and free (no LSP server needed)."
}
func (t *CodeLocateTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "Symbol name to locate (class, function, method, struct, variable)"},
		},
		"required": []string{"name"},
	}
}
func (t *CodeLocateTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if t.Index == nil {
		return "", fmt.Errorf("code_locate index is not built (codebase index unavailable)")
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return "", fmt.Errorf("code_locate requires a symbol name")
	}
	return CapOutput(t.Index.FormatLookup(name)), nil
}

// SearchCodeTool performs a relevance search over the codebase — ranks files
// against a natural-language query. BM25 first; when an embedding endpoint is
// wired (SetEmbedder), the top candidates are re-ranked by vector cosine
// similarity for true semantic matching.
type SearchCodeTool struct {
	embedder *search.Embedder
}

// SetEmbedder wires an OpenAI-compatible embeddings endpoint so search_code
// re-ranks BM25 hits semantically (with a persistent per-file cache). Nil
// keeps the tool BM25-only.
func (t *SearchCodeTool) SetEmbedder(e *search.Embedder) { t.embedder = e }

func (t *SearchCodeTool) Name() string { return "search_code" }
func (t *SearchCodeTool) Description() string {
	return "Semantic code search: rank files by relevance to a query. BM25 over file contents, re-ranked with embeddings when available. Unlike grep it matches meaning, not exact strings — use for 'where is X handled', 'which file does auth', vague concepts, or when grep returns nothing useful. Returns top file paths with a snippet around the match."
}
func (t *SearchCodeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Natural-language query or keywords to search for"},
			"path":  map[string]any{"type": "string", "description": "Directory to search (default: current directory)"},
			"limit": map[string]any{"type": "integer", "description": "Max results (default 5, max 10)"},
		},
		"required": []string{"query"},
	}
}
func (t *SearchCodeTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Query string `json:"query"`
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", fmt.Errorf("search_code requires a query")
	}
	if args.Path == "" {
		args.Path = "."
	}
	if args.Limit <= 0 {
		args.Limit = 5
	} else if args.Limit > 10 {
		args.Limit = 10
	}

	docs, err := search.IndexDir(args.Path)
	if err != nil {
		return "", err
	}
	idx := search.NewBM25(docs)
	results := idx.Search(args.Query, 15)
	if t.embedder != nil {
		results = search.ReRank(ctx, args.Path, args.Query, results, t.embedder, args.Limit)
	}
	return CapOutput(search.FormatResults(results, args.Query)), nil
}

// SortSymbolLines is a convenience for tests: returns symbols sorted by line.
func SortSymbolLines(syms []search.SymbolItem) []search.SymbolItem {
	sorted := append([]search.SymbolItem(nil), syms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Line < sorted[j].Line })
	return sorted
}
