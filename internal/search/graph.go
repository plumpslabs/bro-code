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

// DetailedSymbol represents a code symbol with exact start/end lines and call information.
type DetailedSymbol struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // "func", "method", "struct", "interface", "class", "type"
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Lines     int      `json:"lines"`
	Receiver  string   `json:"receiver,omitempty"`
	Signature string   `json:"signature,omitempty"`
	Doc       string   `json:"doc,omitempty"`
	Calls     []string `json:"calls,omitempty"` // names of functions/methods called inside this symbol
}

// OutlineFile extracts the structural symbol outline of a file without reading
// the full body into LLM context. Ideal for 1,000 to 20,000+ line files.
func OutlineFile(path string) ([]DetailedSymbol, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".go" {
		return outlineGoFile(path)
	}
	if IsBinaryExt(ext) {
		return nil, nil
	}
	return outlineGenericFile(path)
}

func outlineGoFile(path string) ([]DetailedSymbol, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	fileNode, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var symbols []DetailedSymbol
	lines := strings.Split(string(data), "\n")

	for _, decl := range fileNode.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			start := fset.Position(d.Pos()).Line
			end := fset.Position(d.End()).Line
			kind := "func"
			recv := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				recv = exprString(d.Recv.List[0].Type)
			}
			doc := ""
			if d.Doc != nil {
				doc = strings.TrimSpace(d.Doc.Text())
			}

			// Extract calls within the function body
			var calls []string
			seenCalls := make(map[string]bool)
			ast.Inspect(d.Body, func(n ast.Node) bool {
				if ce, ok := n.(*ast.CallExpr); ok {
					callName := extractCallName(ce.Fun)
					if callName != "" && !seenCalls[callName] {
						seenCalls[callName] = true
						calls = append(calls, callName)
					}
				}
				return true
			})
			sort.Strings(calls)

			sig := d.Name.Name
			if start <= len(lines) {
				sig = strings.TrimSpace(lines[start-1])
				if idx := strings.Index(sig, "{"); idx > 0 {
					sig = strings.TrimSpace(sig[:idx])
				}
			}

			symbols = append(symbols, DetailedSymbol{
				Name:      d.Name.Name,
				Kind:      kind,
				StartLine: start,
				EndLine:   end,
				Lines:     end - start + 1,
				Receiver:  recv,
				Signature: sig,
				Doc:       doc,
				Calls:     calls,
			})

		case *ast.GenDecl:
			if d.Tok == token.TYPE {
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						start := fset.Position(ts.Pos()).Line
						end := fset.Position(ts.End()).Line
						kind := "type"
						if _, isStruct := ts.Type.(*ast.StructType); isStruct {
							kind = "struct"
						} else if _, isInterface := ts.Type.(*ast.InterfaceType); isInterface {
							kind = "interface"
						}
						doc := ""
						if d.Doc != nil {
							doc = strings.TrimSpace(d.Doc.Text())
						} else if ts.Doc != nil {
							doc = strings.TrimSpace(ts.Doc.Text())
						}

						symbols = append(symbols, DetailedSymbol{
							Name:      ts.Name.Name,
							Kind:      kind,
							StartLine: start,
							EndLine:   end,
							Lines:     end - start + 1,
							Signature: "type " + ts.Name.Name + " " + kind,
							Doc:       doc,
						})
					}
				}
			}
		}
	}

	return symbols, nil
}

func extractCallName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	}
	return ""
}

func outlineGenericFile(path string) ([]DetailedSymbol, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	var symbols []DetailedSymbol

	// Generic regexes for JS/TS/Python/Rust
	fnRe := regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?(?:public\s+|private\s+|protected\s+|static\s+)?(?:def|fn|function|class|interface|struct|type)\s+([a-zA-Z0-9_]+)`)
	arrowRe := regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:const|let|var)\s+([a-zA-Z0-9_]+)\s*=\s*(?:async\s*)?\(?[^=]*=>`)

	for i, line := range lines {
		lineNum := i + 1
		var name, kind string
		if m := fnRe.FindStringSubmatch(line); len(m) > 1 {
			name = m[1]
			kind = "func"
			if strings.Contains(line, "class") {
				kind = "class"
			} else if strings.Contains(line, "interface") {
				kind = "interface"
			} else if strings.Contains(line, "struct") {
				kind = "struct"
			}
		} else if m := arrowRe.FindStringSubmatch(line); len(m) > 1 {
			name = m[1]
			kind = "func"
		}

		if name != "" {
			symbols = append(symbols, DetailedSymbol{
				Name:      name,
				Kind:      kind,
				StartLine: lineNum,
				EndLine:   lineNum,
				Lines:     1,
				Signature: strings.TrimSpace(line),
			})
		}
	}
	return symbols, nil
}

// ImpactReport details the blast radius of modifying a symbol.
type ImpactReport struct {
	Symbol      string           `json:"symbol"`
	File        string           `json:"file"`
	Kind        string           `json:"kind"`
	StartLine   int              `json:"start_line"`
	EndLine     int              `json:"end_line"`
	FanIn       int              `json:"fan_in"`       // count of callers
	FanOut      int              `json:"fan_out"`      // count of callees
	BlastRadius string           `json:"blast_radius"` // "LOW" | "MEDIUM" | "HIGH"
	Callers     []CallerLocation `json:"callers"`
	Callees     []string         `json:"callees"`
	Extraction  string           `json:"safe_extraction_order"`
}

// CallerLocation points to a call site in the workspace.
type CallerLocation struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

// AnalyzeImpact scans the workspace to compute the blast radius of a symbol.
func AnalyzeImpact(rootPath, targetSymbol, targetFile string) (*ImpactReport, error) {
	report := &ImpactReport{
		Symbol: targetSymbol,
		File:   targetFile,
	}

	// 1. If target file given, locate symbol definition
	if targetFile != "" {
		syms, _ := OutlineFile(filepath.Join(rootPath, targetFile))
		for _, s := range syms {
			if s.Name == targetSymbol {
				report.Kind = s.Kind
				report.StartLine = s.StartLine
				report.EndLine = s.EndLine
				report.Callees = s.Calls
				report.FanOut = len(s.Calls)
				break
			}
		}
	}

	// 2. Scan workspace files to find all callers (fan-in)
	callers, err := findCallSites(rootPath, targetSymbol, targetFile)
	if err == nil {
		report.Callers = callers
		report.FanIn = len(callers)
	}

	// 3. Compute blast radius score
	switch {
	case report.FanIn == 0:
		report.BlastRadius = "LOW (Internal / Dead Code Candidate — 0 external callers)"
		report.Extraction = "Step 1: Leaf extraction. Safe to move or refactor immediately with zero external impact."
	case report.FanIn <= 2:
		report.BlastRadius = "LOW (Isolated — 1-2 call sites)"
		report.Extraction = "Step 1: Extract symbol to target module. Step 2: Update 1-2 caller references."
	case report.FanIn <= 5:
		report.BlastRadius = "MEDIUM (Moderate Coupling — 3-5 call sites)"
		report.Extraction = "Step 1: Create new module. Step 2: Move symbol. Step 3: Re-export or update callers. Step 4: Run typecheck/test."
	default:
		report.BlastRadius = fmt.Sprintf("HIGH (Core Dependency — %d call sites across project)", report.FanIn)
		report.Extraction = "Step 1: Keep deprecation alias / facade in original file. Step 2: Migrate callers incrementally. Step 3: Remove alias."
	}

	return report, nil
}

func findCallSites(rootPath, symbol, defFile string) ([]CallerLocation, error) {
	var results []CallerLocation
	identRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(symbol) + `\b`)

	err := filepath.WalkDir(rootPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() {
				name := d.Name()
				if strings.HasPrefix(name, ".") || isHeavyDir(name) {
					return filepath.SkipDir
				}
			}
			return nil
		}

		rel, _ := filepath.Rel(rootPath, path)
		if IsBinaryExt(filepath.Ext(path)) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			lineNum := i + 1
			if rel == defFile && strings.Contains(line, "func "+symbol) {
				continue // Skip definition line
			}
			if identRe.MatchString(line) {
				results = append(results, CallerLocation{
					File:    rel,
					Line:    lineNum,
					Snippet: strings.TrimSpace(line),
				})
				if len(results) >= 50 {
					return nil // Cap search at 50 references
				}
			}
		}
		return nil
	})

	return results, err
}

func isHeavyDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "dist", "build", ".git", ".brocode", "out", "target":
		return true
	}
	return false
}

// ClusterGroup represents a cohesive cluster of functions that belong together.
type ClusterGroup struct {
	ID             int      `json:"cluster_id"`
	SuggestedFile  string   `json:"suggested_file"`
	PrimaryTheme   string   `json:"primary_theme"`
	Symbols        []string `json:"symbols"`
	TotalLines     int      `json:"total_lines"`
	InternalCalls  int      `json:"internal_calls"`  // high = strong cohesion
	ExternalCalls  int      `json:"external_calls"`  // low = clean boundary
}

// ClusterFileSymbols performs modularity clustering on a monolithic file.
// It groups functions that call each other into self-contained modular candidates.
func ClusterFileSymbols(path string) ([]ClusterGroup, error) {
	syms, err := OutlineFile(path)
	if err != nil {
		return nil, err
	}
	if len(syms) == 0 {
		return nil, fmt.Errorf("no symbols found in %s", path)
	}

	symMap := make(map[string]DetailedSymbol)
	for _, s := range syms {
		symMap[s.Name] = s
	}

	// Adjacency graph for connected components
	adj := make(map[string]map[string]bool)
	for _, s := range syms {
		if adj[s.Name] == nil {
			adj[s.Name] = make(map[string]bool)
		}
		for _, callee := range s.Calls {
			if _, exists := symMap[callee]; exists && callee != s.Name {
				adj[s.Name][callee] = true
				if adj[callee] == nil {
					adj[callee] = make(map[string]bool)
				}
				adj[callee][s.Name] = true
			}
		}
	}

	// BFS Connected Component detection
	visited := make(map[string]bool)
	var rawClusters [][]string

	for _, s := range syms {
		if visited[s.Name] {
			continue
		}
		var cluster []string
		queue := []string{s.Name}
		visited[s.Name] = true

		for len(queue) > 0 {
			curr := queue[0]
			queue = queue[1:]
			cluster = append(cluster, curr)

			for neighbor := range adj[curr] {
				if !visited[neighbor] {
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
		sort.Strings(cluster)
		rawClusters = append(rawClusters, cluster)
	}

	// Sort clusters by size descending
	sort.Slice(rawClusters, func(i, j int) bool {
		return len(rawClusters[i]) > len(rawClusters[j])
	})

	baseName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	dir := filepath.Dir(path)
	ext := filepath.Ext(path)

	var result []ClusterGroup
	for i, cluster := range rawClusters {
		totalLines := 0
		intCalls := 0
		extCalls := 0
		clusterSet := make(map[string]bool)
		for _, name := range cluster {
			clusterSet[name] = true
		}

		for _, name := range cluster {
			if s, ok := symMap[name]; ok {
				totalLines += s.Lines
				for _, callee := range s.Calls {
					if clusterSet[callee] {
						intCalls++
					} else {
						extCalls++
					}
				}
			}
		}

		theme := inferTheme(cluster)
		suggested := filepath.Join(dir, fmt.Sprintf("%s_%s%s", baseName, strings.ToLower(theme), ext))
		if len(rawClusters) == 1 {
			suggested = path
		}

		result = append(result, ClusterGroup{
			ID:            i + 1,
			SuggestedFile: suggested,
			PrimaryTheme:  theme,
			Symbols:       cluster,
			TotalLines:    totalLines,
			InternalCalls: intCalls,
			ExternalCalls: extCalls,
		})
	}

	return result, nil
}

func inferTheme(syms []string) string {
	if len(syms) == 0 {
		return "module"
	}
	lowerNames := make([]string, len(syms))
	for i, s := range syms {
		lowerNames[i] = strings.ToLower(s)
	}

	keywords := []string{"auth", "user", "order", "payment", "parse", "format", "util", "handle", "service", "db", "store", "event", "sync"}
	for _, kw := range keywords {
		count := 0
		for _, name := range lowerNames {
			if strings.Contains(name, kw) {
				count++
			}
		}
		if count >= (len(syms)+1)/2 {
			return kw
		}
	}

	return strings.ToLower(syms[0])
}
