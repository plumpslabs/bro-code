// Package search provides zero-dependency code intelligence: symbol
// extraction (function/struct/class maps per file) and BM25 relevance search
// over file contents. Hand-rolled and dependency-free on purpose — these give
// the agent a structural map of the codebase without reading every file into
// context, and a relevance-ranked search that beats plain regex grep.
package search

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SymbolItem represents an extracted code symbol (function, struct, type, class, etc.).
type SymbolItem struct {
	Name string // Symbol name (e.g. "EvaluateComplexity", "UserModel")
	Kind string // Kind (e.g. "func", "method", "struct", "class", "interface")
	Line int    // Line number in file
}

// ExtractSymbols parses a file and returns its structural symbols without
// reading the full file body into LLM context. Uses Go's native go/ast parser
// for Go files, and lightweight regex matching for other languages.
func ExtractSymbols(path string) ([]SymbolItem, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return extractGoSymbols(path)
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".py", ".rs", ".c", ".cpp", ".cc", ".h", ".hpp",
		".java", ".kt", ".rb", ".php", ".swift", ".cs", ".vue", ".svelte", ".astro", ".mdx",
		".prisma", ".graphql", ".gql", ".proto", ".sql", ".dart", ".scala", ".zig", ".lua", ".ex", ".exs":
		return extractGenericSymbols(path)
	}
	return nil, nil
}

// extractGoSymbols uses Go standard library AST parser (zero dependencies).
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

// genericDeclRe matches common declaration forms across languages.
var genericDeclRe = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?(?:pub\s+)?(?:static\s+)?(?:const\s+)?(?:def|fn|function|class|interface|struct|enum|trait|impl|type)\s+([a-zA-Z0-9_]+)`)

// extractGenericSymbols extracts declarations using fast regex for TS, Python,
// Rust, C++, Java, JS, Vue, Svelte, Astro, GraphQL, Prisma, SQL, etc.
var (
	arrowFnRe     = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:default\s+)?const\s+([a-zA-Z0-9_]+)\s*=\s*(?:async\s*)?\(?`)
	cjsExportRe   = regexp.MustCompile(`(?m)^\s*module\.exports\s*=\s*([a-zA-Z0-9_]+)`)
	classMethodRe = regexp.MustCompile(`(?m)^\s{2,}(?:async\s+)?(?:static\s+)?(?:get\s+|set\s+)?([a-zA-Z0-9_]+)\s*\([^)]*\)\s*\{`)
	prismaModelRe = regexp.MustCompile(`(?m)^\s*(?:model|enum)\s+([a-zA-Z0-9_]+)`)
	graphqlTypeRe = regexp.MustCompile(`(?m)^\s*(?:type|input|interface|enum|union|schema)\s+([a-zA-Z0-9_]+)`)
	protoMsgRe    = regexp.MustCompile(`(?m)^\s*(?:message|service|rpc|enum)\s+([a-zA-Z0-9_]+)`)
	sqlTableRe    = regexp.MustCompile(`(?im)^\s*CREATE\s+(?:TABLE|VIEW|FUNCTION|PROCEDURE)\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z0-9_\.]+)`)
)

func extractGenericSymbols(path string) ([]SymbolItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(data), "\n")
	var symbols []SymbolItem

	seen := map[string]bool{}
	add := func(name, kind string, line int) {
		name = strings.Trim(name, `"';`)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		symbols = append(symbols, SymbolItem{Name: name, Kind: kind, Line: line})
	}

	for i, line := range lines {
		ln := i + 1
		if m := genericDeclRe.FindStringSubmatch(line); len(m) == 2 {
			lower := strings.ToLower(line)
			kind := "decl"
			switch {
			case strings.Contains(lower, "class "):
				kind = "class"
			case strings.Contains(lower, "interface "):
				kind = "interface"
			case strings.Contains(lower, "struct "):
				kind = "struct"
			case strings.Contains(lower, "enum "):
				kind = "enum"
			case strings.Contains(lower, "trait "), strings.Contains(lower, "impl "):
				kind = "trait"
			case strings.Contains(lower, "def ") || strings.Contains(lower, "fn ") || strings.Contains(lower, "function "):
				kind = "func"
			case strings.Contains(lower, "type "):
				kind = "type"
			}
			add(m[1], kind, ln)
		}
		if m := arrowFnRe.FindStringSubmatch(line); len(m) == 2 {
			add(m[1], "func", ln)
		}
		if m := cjsExportRe.FindStringSubmatch(line); len(m) == 2 {
			add(m[1], "func", ln)
		}
		if m := prismaModelRe.FindStringSubmatch(line); len(m) == 2 {
			add(m[1], "model", ln)
		}
		if m := graphqlTypeRe.FindStringSubmatch(line); len(m) == 2 {
			add(m[1], "type", ln)
		}
		if m := protoMsgRe.FindStringSubmatch(line); len(m) == 2 {
			add(m[1], "proto", ln)
		}
		if m := sqlTableRe.FindStringSubmatch(line); len(m) == 2 {
			add(m[1], "table", ln)
		}
		// Indented method/property definitions inside classes (JS/TS/Vue/Svelte).
		if !strings.Contains(line, "function") && !strings.Contains(line, "=>") {
			if m := classMethodRe.FindStringSubmatch(line); len(m) == 2 {
				add(m[1], "method", ln)
			}
		}
	}
	return symbols, nil
}

// FormatSymbolSummary returns a compact symbol map string for a file list —
// the agent's "structure preview" of a file without reading its body.
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
	return strings.TrimSpace(sb.String())
}

// SortedSymbols returns symbols ordered by line.
func SortedSymbols(syms []SymbolItem) []SymbolItem {
	out := append([]SymbolItem(nil), syms...)
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}
