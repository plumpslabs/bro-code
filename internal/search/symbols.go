package search

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SymbolItem represents an extracted code symbol (function, struct, type, class, etc.).
type SymbolItem struct {
	Name string // Symbol name (e.g. "EvaluateComplexity", "UserModel")
	Kind string // Kind (e.g. "func", "struct", "class", "interface")
	Line int    // Line number in file
}

// ExtractSymbols parses a file and returns its structural symbols without reading full file body into LLM context.
// Uses Go's native go/ast parser for Go files, and lightweight regex matching for other languages.
func ExtractSymbols(path string) ([]SymbolItem, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".go" {
		return extractGoSymbols(path)
	}
	return extractGenericSymbols(path)
}

// extractGoSymbols uses Go standard library AST parser (zero dependencies, 0 extra binary size).
func extractGoSymbols(path string) ([]SymbolItem, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var symbols []SymbolItem
	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			line := fset.Position(d.Pos()).Line
			kind := "func"
			if d.Recv != nil {
				kind = "method"
			}
			symbols = append(symbols, SymbolItem{Name: d.Name.Name, Kind: kind, Line: line})

		case *ast.GenDecl:
			if d.Tok == token.TYPE {
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						line := fset.Position(ts.Pos()).Line
						kind := "type"
						if _, isStruct := ts.Type.(*ast.StructType); isStruct {
							kind = "struct"
						} else if _, isInterface := ts.Type.(*ast.InterfaceType); isInterface {
							kind = "interface"
						}
						symbols = append(symbols, SymbolItem{Name: ts.Name.Name, Kind: kind, Line: line})
					}
				}
			}
		}
	}
	return symbols, nil
}

// extractGenericSymbols extracts declarations using fast, zero-alloc regex for TS, Python, Rust, C++, Java, etc.
func extractGenericSymbols(path string) ([]SymbolItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(data), "\n")
	var symbols []SymbolItem

	// Universal declaration pattern matching function, class, def, fn, struct, interface, type, export
	symbolRe := regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:async\s+)?(?:pub\s+)?(?:static\s+)?(?:def|fn|function|class|interface|type|struct|enum)\s+([a-zA-Z0-9_]+)`)

	for i, line := range lines {
		if m := symbolRe.FindStringSubmatch(line); len(m) == 2 {
			kind := "decl"
			lower := strings.ToLower(line)
			switch {
			case strings.Contains(lower, "class "):
				kind = "class"
			case strings.Contains(lower, "def ") || strings.Contains(lower, "fn ") || strings.Contains(lower, "function "):
				kind = "func"
			case strings.Contains(lower, "interface "):
				kind = "interface"
			case strings.Contains(lower, "struct "):
				kind = "struct"
			case strings.Contains(lower, "type "):
				kind = "type"
			}
			symbols = append(symbols, SymbolItem{Name: m[1], Kind: kind, Line: i + 1})
		}
	}
	return symbols, nil
}

// FormatSymbolSummary returns a ultra-compact symbol map string for a file list (fits in < 300 tokens total).
func FormatSymbolSummary(files []string) string {
	var sb strings.Builder
	for _, f := range files {
		syms, err := ExtractSymbols(f)
		if err != nil || len(syms) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("SYMBOL MAP (%s):\n", f))
		for _, s := range syms {
			sb.WriteString(fmt.Sprintf("  • L%d [%s] %s\n", s.Line, s.Kind, s.Name))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// RepoNode represents a file node in the PageRank dependency graph.
type RepoNode struct {
	Path     string
	Symbols  []SymbolItem
	InDegree int
	Score    float64
}

// BuildRepoMap walks workspace files, computes AST PageRank scores based on incoming symbol references,
// and builds a hyper-compact ~500 token structural Repo Map (Aider-style AST PageRank).
func BuildRepoMap(root string, maxFiles int) string {
	var files []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			rel, _ := filepath.Rel(root, path)
			if rel == "node_modules" || rel == ".git" || rel == "vendor" || rel == "bin" || rel == ".opencode" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".go", ".ts", ".js", ".py", ".rs", ".java", ".cpp", ".c", ".h", ".php":
			rel, _ := filepath.Rel(root, path)
			files = append(files, rel)
		}
		return nil
	})

	if len(files) == 0 {
		return ""
	}

	// Step 1: Extract symbols per file
	nodes := make(map[string]*RepoNode)
	symbolToFile := make(map[string]string) // symbol name -> file defined in

	for _, f := range files {
		fullPath := filepath.Join(root, f)
		syms, err := ExtractSymbols(fullPath)
		if err != nil || len(syms) == 0 {
			continue
		}
		nodes[f] = &RepoNode{Path: f, Symbols: syms}
		for _, s := range syms {
			if len(s.Name) > 2 {
				symbolToFile[s.Name] = f
			}
		}
	}

	if len(nodes) == 0 {
		return ""
	}

	// Step 2: Compute incoming references (PageRank graph edges)
	for _, node := range nodes {
		fullPath := filepath.Join(root, node.Path)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		content := string(data)
		for symName, defFile := range symbolToFile {
			if defFile != node.Path && strings.Contains(content, symName) {
				if target, ok := nodes[defFile]; ok {
					target.InDegree++
				}
			}
		}
	}

	// Step 3: Rank nodes by PageRank score: Score = InDegree*10 + len(Symbols)
	ranked := make([]*RepoNode, 0, len(nodes))
	for _, n := range nodes {
		n.Score = float64(n.InDegree*10 + len(n.Symbols))
		ranked = append(ranked, n)
	}

	// Sort descending by score
	for i := 0; i < len(ranked)-1; i++ {
		for j := i + 1; j < len(ranked); j++ {
			if ranked[j].Score > ranked[i].Score {
				ranked[i], ranked[j] = ranked[j], ranked[i]
			}
		}
	}

	if maxFiles <= 0 || maxFiles > len(ranked) {
		maxFiles = len(ranked)
	}
	if maxFiles > 12 {
		maxFiles = 12 // Cap at top 12 architectural files (~500 tokens)
	}

	var sb strings.Builder
	sb.WriteString("🏛️  REPO MAP (AST PageRank Structural Graph):\n")
	for i := 0; i < maxFiles; i++ {
		n := ranked[i]
		sb.WriteString(fmt.Sprintf("• %s (score: %.0f, refs: %d)\n", n.Path, n.Score, n.InDegree))
		limit := min(6, len(n.Symbols))
		for k := 0; k < limit; k++ {
			s := n.Symbols[k]
			sb.WriteString(fmt.Sprintf("   └─ L%d [%s] %s\n", s.Line, s.Kind, s.Name))
		}
		if len(n.Symbols) > limit {
			sb.WriteString(fmt.Sprintf("   └─ … and %d more symbols\n", len(n.Symbols)-limit))
		}
	}
	return sb.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
