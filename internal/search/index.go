package search

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// IndexedSymbol is one symbol occurrence: where a name is defined in the repo.
type IndexedSymbol struct {
	Name string // symbol name
	Kind string // func / method / struct / class / interface / ...
	File string // absolute path
	Line int    // 1-based line
}

// GlobalIndex is a codebase-wide symbol + reference index built ONCE per
// session and reused across turns. Unlike per-call LSP queries it answers
// repo-wide questions instantly and for free: where a symbol is defined, which
// files reference it, and which files import a given module — no server
// spawn, no full-file reads into context.
type GlobalIndex struct {
	byName  map[string][]IndexedSymbol
	files   []string
	imports map[string]map[string]bool // file -> referenced module names (last path segments)
}

// BuildGlobalIndex walks root (skipping heavy/vendor dirs) and indexes every
// supported source file's symbols and import references.
func BuildGlobalIndex(root string) *GlobalIndex {
	g := &GlobalIndex{
		byName:  map[string][]IndexedSymbol{},
		imports: map[string]map[string]bool{},
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && isHeavyDirName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".py", ".rs",
			".c", ".cpp", ".cc", ".h", ".hpp", ".java", ".kt", ".rb", ".php", ".swift", ".cs":
		default:
			return nil
		}
		// Never index sensitive files (.env, credentials, keys) — their
		// contents must not leak into code_locate results.
		if isSensitiveName(d.Name()) {
			return nil
		}
		syms, _ := ExtractSymbols(path)
		for _, s := range syms {
			g.byName[s.Name] = append(g.byName[s.Name], IndexedSymbol{Name: s.Name, Kind: s.Kind, File: path, Line: s.Line})
		}
		if refs := extractImportNames(path); len(refs) > 0 {
			g.imports[path] = refs
		}
		g.files = append(g.files, path)
		return nil
	})
	sort.Strings(g.files)
	return g
}

// Lookup returns every indexed occurrence of a symbol name (definitions and
// declarations), sorted by file then line.
func (g *GlobalIndex) Lookup(name string) []IndexedSymbol {
	out := append([]IndexedSymbol(nil), g.byName[name]...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// Referencers returns the files that reference the symbol name (single quick
// pass over the indexed file list, capped), excluding its definition files.
func (g *GlobalIndex) Referencers(name string) []string {
	if name == "" {
		return nil
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(name) + `\b`)
	if err != nil {
		return nil
	}
	defFiles := map[string]bool{}
	for _, s := range g.byName[name] {
		defFiles[s.File] = true
	}
	var out []string
	for _, f := range g.files {
		if defFiles[f] {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		body := string(data)
		if len(body) > 200_000 {
			body = body[:200_000]
		}
		if re.MatchString(body) {
			out = append(out, filepath.ToSlash(f))
			if len(out) >= 15 {
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Importers returns the files whose import specifiers reference the given file
// (matched by the module name's last path segment, e.g. ConversationService.js
// is imported by files that `import ... from '.../ConversationService'`).
func (g *GlobalIndex) Importers(file string) []string {
	base := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	// Also match with extension (some imports include it).
	names := []string{base, filepath.Base(file)}
	var out []string
	for f, refs := range g.imports {
		if f == file {
			continue
		}
		for _, n := range names {
			if refs[n] {
				out = append(out, filepath.ToSlash(f))
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// FormatLookup renders a compact human-readable code_locate report.
func (g *GlobalIndex) FormatLookup(name string) string {
	defs := g.Lookup(name)
	if len(defs) == 0 {
		// Not a declared symbol — still show referencing files (it may be
		// defined dynamically or in an unsupported file).
		refs := g.Referencers(name)
		if len(refs) == 0 {
			return fmt.Sprintf("Symbol %q not found anywhere in the indexed codebase.", name)
		}
		return fmt.Sprintf("Symbol %q is not declared as a known symbol, but is referenced by (%d):\n  %s", name, len(refs), strings.Join(refs, "\n  "))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Symbol %q:\n", name))
	for _, s := range defs {
		sb.WriteString(fmt.Sprintf("  • %s:%d [%s]\n", filepath.ToSlash(s.File), s.Line, s.Kind))
	}
	if refs := g.Referencers(name); len(refs) > 0 {
		sb.WriteString(fmt.Sprintf("Referenced by (%d): %s\n", len(refs), strings.Join(refs, ", ")))
	}
	var importers []string
	for _, s := range defs {
		for _, im := range g.Importers(s.File) {
			importers = append(importers, im)
		}
	}
	importers = uniqueSorted(importers)
	if len(importers) > 0 {
		sb.WriteString(fmt.Sprintf("Imported by (%d): %s\n", len(importers), strings.Join(importers, ", ")))
	}
	return strings.TrimSpace(sb.String())
}

// FileCount returns the number of indexed files (used by the tool's output
// header and /lsp-style status).
func (g *GlobalIndex) FileCount() int { return len(g.files) }

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

var (
	// goImportRe matches Go import specifiers: `import "x/y"`, `import _ "x/y"`,
	// and `"x/y"` lines inside an import block.
	goImportRe = regexp.MustCompile(`(?m)^\s*(?:import\s+)?(?:[a-zA-Z0-9_\.]+\s+)?"([a-zA-Z0-9_\-./]+)"`)
	// jsImportRe matches JS/TS module specifiers: `from 'x'`, `require('x')`,
	// `import('x')`, and `import x from 'x'`.
	jsImportRe = regexp.MustCompile(`(?m)(?:from\s*|require\(\s*|import\(\s*|import\s+[^'"]*from\s*)['"]([^'"]+)['"]`)
	// pyImportRe matches Python imports: `from x.y import ...` and `import x.y`.
	pyImportRe = regexp.MustCompile(`(?m)^\s*(?:from\s+([\w.]+)\s+import|import\s+([\w.,\s]+))`)
)

// extractImportNames returns the set of module names (last path segments) a
// file references in its import statements — the lightweight call graph.
func extractImportNames(path string) map[string]bool {
	ext := strings.ToLower(filepath.Ext(path))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := string(data)
	if len(text) > 200_000 {
		text = text[:200_000]
	}

	refs := map[string]bool{}
	switch ext {
	case ".go":
		for _, m := range goImportRe.FindAllStringSubmatch(text, -1) {
			if seg := lastSegment(m[1]); seg != "" && seg != "C" {
				refs[seg] = true
			}
		}
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs":
		for _, m := range jsImportRe.FindAllStringSubmatch(text, -1) {
			spec := m[1]
			// Strip query/hash, take last path segment.
			if i := strings.IndexAny(spec, "?#"); i >= 0 {
				spec = spec[:i]
			}
			if seg := lastSegment(spec); seg != "" && seg != "node" {
				refs[seg] = true
			}
		}
	case ".py":
		for _, m := range pyImportRe.FindAllStringSubmatch(text, -1) {
			mod := m[1]
			if mod == "" {
				mod = m[2]
			}
			for _, part := range strings.Split(mod, ",") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				if seg := lastSegment(part); seg != "" {
					refs[seg] = true
				}
			}
		}
	}
	return refs
}

// lastSegment returns the final path segment of a module path.
func lastSegment(p string) string {
	p = strings.Trim(p, `"'`)
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
