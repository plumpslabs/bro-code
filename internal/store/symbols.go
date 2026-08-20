package store

import (
	"regexp"
	"strings"
)

// SymbolRange is a structural anchor: a named code unit and the line span it
// occupies in the source file. It gives the Smart Context Graph POSITION
// awareness across an entire file — even when only part of the file was ever
// read into the model's context. This is what lets BroCode answer "where is X
// in that 5000-line file?" without force-cutting or re-reading everything: the
// graph knows every symbol's line range, so recall can point straight at it
// (coarse-to-fine: file → symbol → line span). No embeddings, no vector stack.
type SymbolRange struct {
	Name  string `json:"n"` // symbol name (func/method/class/struct…)
	Kind  string `json:"k"` // func | method | class | struct | interface | def | enum | trait
	Start int    `json:"s"` // 1-based start line
	End   int    `json:"e"` // 1-based end line (inclusive)
}

// knowledgeMaxSymbols caps how many symbol anchors we keep per file. A 5000-line
// file may define hundreds of units; we keep the first N (ordered by appearance)
// so the DB row stays small while still covering the whole file's structure.
const knowledgeMaxSymbols = 80

// extractSymbols builds a whole-file structural index (symbol → line range)
// using language-aware regexes. It returns AST-boundary-aligned spans: each
// symbol's End is the line before the next definition (or EOF). This mirrors
// the cAST / RepoCoder finding that chunking on syntactic boundaries — not
// arbitrary N lines — preserves structure and avoids losing context at cuts.
//
// It runs on the FULL file content (the read_file tool passes it even when only
// a span/shrinkwrap was shown to the model), so positions cover every line.
func extractSymbols(content, language string) []SymbolRange {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	defRe := symbolPatterns(language)
	if defRe == nil {
		return nil
	}

	type hit struct {
		line int
		name string
		kind string
	}
	var hits []hit
	for i, ln := range lines {
		for _, re := range defRe {
			m := re.re.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			name := ""
			if len(m) > 1 {
				name = m[1]
			}
			if name == "" || len(name) < 2 {
				continue
			}
			hits = append(hits, hit{line: i + 1, name: name, kind: re.kind})
			if len(hits) >= knowledgeMaxSymbols {
				break
			}
		}
		if len(hits) >= knowledgeMaxSymbols {
			break
		}
	}
	if len(hits) == 0 {
		return nil
	}

	out := make([]SymbolRange, 0, len(hits))
	for i, h := range hits {
		end := len(lines)
		if i+1 < len(hits) {
			end = hits[i+1].line - 1
		}
		if end < h.line {
			end = h.line
		}
		out = append(out, SymbolRange{Name: h.name, Kind: h.kind, Start: h.line, End: end})
	}
	return out
}

// symbolPattern pairs a regex (first capture group = symbol name) with a kind.
type symbolPattern struct {
	re   *regexp.Regexp
	kind string
}

// symbolPatterns returns the definition-detecting regexes for a language.
// Anchored to line starts so we catch top-level and method definitions without
// matching every identifier usage.
func symbolPatterns(language string) []symbolPattern {
	switch language {
	case "go":
		return []symbolPattern{
			{regexp.MustCompile(`^func \([^)]*\)\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), "method"},
			{regexp.MustCompile(`^func\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), "func"},
			{regexp.MustCompile(`^type\s+([A-Za-z_][A-Za-z0-9_]*)\s+struct`), "struct"},
			{regexp.MustCompile(`^type\s+([A-Za-z_][A-Za-z0-9_]*)\s+interface`), "interface"},
		}
	case "ts", "js":
		return []symbolPattern{
			{regexp.MustCompile(`^function\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), "func"},
			{regexp.MustCompile(`^class\s+([A-Za-z_][A-Za-z0-9_]*)`), "class"},
			{regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(?:function|\(|async)`), "func"},
			{regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\([^)]*\)\s*\{`), "method"},
		}
	case "python":
		return []symbolPattern{
			{regexp.MustCompile(`^def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), "def"},
			{regexp.MustCompile(`^class\s+([A-Za-z_][A-Za-z0-9_]*)`), "class"},
		}
	case "rust":
		return []symbolPattern{
			{regexp.MustCompile(`^fn\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), "func"},
			{regexp.MustCompile(`^(?:pub\s+)?struct\s+([A-Za-z_][A-Za-z0-9_]*)`), "struct"},
			{regexp.MustCompile(`^impl[<\(]?\s*([A-Za-z_][A-Za-z0-9_]*)`), "impl"},
			{regexp.MustCompile(`^(?:pub\s+)?trait\s+([A-Za-z_][A-Za-z0-9_]*)`), "trait"},
			{regexp.MustCompile(`^(?:pub\s+)?enum\s+([A-Za-z_][A-Za-z0-9_]*)`), "enum"},
		}
	case "java":
		return []symbolPattern{
			{regexp.MustCompile(`^(?:public|private|protected|static|final|\s)*class\s+([A-Za-z_][A-Za-z0-9_]*)`), "class"},
			{regexp.MustCompile(`^(?:public|private|protected|static|final|\s)*interface\s+([A-Za-z_][A-Za-z0-9_]*)`), "interface"},
			{regexp.MustCompile(`^\s*(?:public|private|protected|static|final|\s)+[\w<>\[\],\s]+\s+([A-Za-z_][A-Za-z0-9_]*)\s*\([^;]*\)\s*\{`), "method"},
		}
	case "ruby":
		return []symbolPattern{
			{regexp.MustCompile(`^def\s+([A-Za-z_][A-Za-z0-9_?!]*)`), "def"},
			{regexp.MustCompile(`^class\s+([A-Za-z_][A-Za-z0-9_]*)`), "class"},
			{regexp.MustCompile(`^module\s+([A-Za-z_][A-Za-z0-9_]*)`), "module"},
		}
	case "cpp", "c":
		return []symbolPattern{
			{regexp.MustCompile(`^(?:struct|class|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`), "struct"},
			{regexp.MustCompile(`^[\w:<>\*&,\s]+\s+([A-Za-z_][A-Za-z0-9_]*)\s*\([^;]*\)\s*\{?`), "func"},
		}
	case "php":
		return []symbolPattern{
			{regexp.MustCompile(`^function\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), "func"},
			{regexp.MustCompile(`^(?:abstract\s+)?class\s+([A-Za-z_][A-Za-z0-9_]*)`), "class"},
			{regexp.MustCompile(`^interface\s+([A-Za-z_][A-Za-z0-9_]*)`), "interface"},
		}
	case "sql":
		return []symbolPattern{
			{regexp.MustCompile(`(?i)^CREATE\s+(?:OR\s+REPLACE\s+)?(?:TABLE|VIEW|FUNCTION|PROCEDURE)\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`), "def"},
		}
	default:
		// Generic fallback: any indented/anchored `name(` definition.
		return []symbolPattern{
			{regexp.MustCompile(`^(?:func|def|function|fn)\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`), "func"},
			{regexp.MustCompile(`^(?:class|struct|interface|trait|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`), "class"},
		}
	}
}
