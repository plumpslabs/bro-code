package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// EditSymbolTool enables AST-addressable code editing: targeting functions,
// methods, structs, and classes directly by symbol name without relying on
// string search or guessing line numbers.
type EditSymbolTool struct{}

func (t *EditSymbolTool) Name() string { return "edit_symbol" }
func (t *EditSymbolTool) Description() string {
	return "AST-addressable symbol editor: replace or modify a function, method, struct, class, or interface by its symbol name directly. Eliminates string-matching ambiguity and automatically validates AST syntax and scope before writing to disk."
}

func (t *EditSymbolTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Target file path",
			},
			"symbol": map[string]any{
				"type":        "string",
				"description": "Exact symbol or method name to locate (e.g. 'ProcessItem', 'User.GetID', 'handleSlashCommand', 'processExpiredBroadcasts', 'Calculator')",
			},
			"code": map[string]any{
				"type":        "string",
				"description": "The replacement or new implementation code",
			},
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"replace_all", "replace_body", "insert_before", "insert_after"},
				"description": "Edit action: 'replace_all' (default: replaces the entire symbol declaration), 'replace_body' (replaces only the inner body of the function/method), 'insert_before', 'insert_after'",
			},
		},
		"required": []string{"path", "symbol", "code"},
	}
}

func (t *EditSymbolTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path   string `json:"path"`
		Symbol string `json:"symbol"`
		Code   string `json:"code"`
		Action string `json:"action"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	args.Path = resolvePath(args.Path)
	if strings.TrimSpace(args.Symbol) == "" {
		return "", fmt.Errorf("edit_symbol requires a non-empty 'symbol' name")
	}
	if args.Action == "" {
		args.Action = "replace_all"
	}

	// Native guard: never edit sensitive files or heavy vendor directories
	if err := GuardFile(args.Path); err != nil {
		return "", err
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}
	oldContent := string(data)
	if strings.HasPrefix(oldContent, "\xef\xbb\xbf") {
		oldContent = oldContent[3:]
	}

	hasCRLF := strings.Contains(oldContent, "\r\n")
	normalizedOld := oldContent
	if hasCRLF {
		normalizedOld = strings.ReplaceAll(oldContent, "\r\n", "\n")
	}

	ext := strings.ToLower(filepath.Ext(args.Path))
	var newContent string
	var editDesc string

	if ext == ".go" {
		res, desc, err := applyGoSymbolEdit(normalizedOld, args.Symbol, args.Code, args.Action)
		if err != nil {
			return "", fmt.Errorf("Go AST edit failed for %s in %s: %w", args.Symbol, args.Path, err)
		}
		newContent = res
		editDesc = desc
	} else {
		res, desc, err := applyGenericSymbolEdit(normalizedOld, args.Symbol, args.Code, args.Action, ext)
		if err != nil {
			return "", fmt.Errorf("Symbol edit failed for %s in %s: %w", args.Symbol, args.Path, err)
		}
		newContent = res
		editDesc = desc
	}

	if hasCRLF {
		newContent = strings.ReplaceAll(newContent, "\r\n", "\n")
		newContent = strings.ReplaceAll(newContent, "\n", "\r\n")
	}

	if newContent == oldContent {
		return fmt.Sprintf("No change made to %s (replacement content identical to current)", args.Path), nil
	}

	// Snapshot for one-turn rollback
	_ = Snapshot(args.Path)

	// Invalidate tool cache
	toolResultCache.InvalidatePath(args.Path)

	if err := os.WriteFile(args.Path, []byte(newContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to write %s: %w", args.Path, err)
	}

	RecordChange(FileChange{
		Path:   args.Path,
		Action: "modified",
		Old:    oldContent,
		New:    newContent,
	})

	return fmt.Sprintf("✅ Successfully edited symbol %q in %s (%s)", args.Symbol, args.Path, editDesc), nil
}

// ─── GO AST ENGINE ──────────────────────────────────────────────────────────

func applyGoSymbolEdit(content, symbol, replacement, action string) (string, string, error) {
	fset := token.NewFileSet()
	fileNode, err := parser.ParseFile(fset, "", content, parser.ParseComments)
	if err != nil {
		return "", "", fmt.Errorf("initial file has syntax error: %w", err)
	}

	targetRecv, targetName := parseSymbolTarget(symbol)

	type declSpan struct {
		startPos token.Pos
		endPos   token.Pos
		bodyL    token.Pos
		bodyR    token.Pos
		isFunc   bool
		name     string
		recv     string
	}

	var candidates []declSpan
	var available []string

	for _, decl := range fileNode.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			recvName := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recvName = formatTypeExpr(d.Recv.List[0].Type)
			}
			symKey := d.Name.Name
			if recvName != "" {
				symKey = recvName + "." + d.Name.Name
			}
			available = append(available, symKey)

			matches := false
			if targetRecv != "" {
				cleanRecv := strings.TrimPrefix(recvName, "*")
				cleanTarget := strings.TrimPrefix(targetRecv, "*")
				if strings.EqualFold(cleanRecv, cleanTarget) && d.Name.Name == targetName {
					matches = true
				}
			} else {
				if d.Name.Name == targetName {
					matches = true
				}
			}

			if matches {
				startPos := d.Pos()
				if d.Doc != nil && d.Doc.Pos().IsValid() {
					startPos = d.Doc.Pos()
				}
				span := declSpan{
					startPos: startPos,
					endPos:   d.End(),
					isFunc:   true,
					name:     d.Name.Name,
					recv:     recvName,
				}
				if d.Body != nil {
					span.bodyL = d.Body.Lbrace
					span.bodyR = d.Body.Rbrace
				}
				candidates = append(candidates, span)
			}

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					available = append(available, ts.Name.Name)
					if ts.Name.Name == targetName {
						startPos := d.Pos()
						if d.Doc != nil && d.Doc.Pos().IsValid() {
							startPos = d.Doc.Pos()
						}
						candidates = append(candidates, declSpan{
							startPos: startPos,
							endPos:   d.End(),
							isFunc:   false,
							name:     ts.Name.Name,
						})
					}
				}
			}
		}
	}

	if len(candidates) == 0 {
		return "", "", fmt.Errorf("symbol %q not found in file AST. Available symbols: %s", symbol, strings.Join(available, ", "))
	}
	if len(candidates) > 1 {
		var list []string
		for _, c := range candidates {
			pos := fset.Position(c.startPos)
			if c.recv != "" {
				list = append(list, fmt.Sprintf("(%s).%s at line %d", c.recv, c.name, pos.Line))
			} else {
				list = append(list, fmt.Sprintf("%s at line %d", c.name, pos.Line))
			}
		}
		return "", "", fmt.Errorf("symbol %q is ambiguous (matched multiple declarations): %s. Specify receiver like 'Receiver.Method'", symbol, strings.Join(list, ", "))
	}

	target := candidates[0]
	var newContent string

	srcBytes := []byte(content)
	startOffset := fset.Position(target.startPos).Offset
	endOffset := fset.Position(target.endPos).Offset

	if startOffset < 0 || endOffset > len(srcBytes) || startOffset > endOffset {
		return "", "", fmt.Errorf("invalid AST offset range [%d:%d]", startOffset, endOffset)
	}

	switch action {
	case "replace_all":
		var buf bytes.Buffer
		buf.Write(srcBytes[:startOffset])
		buf.WriteString(replacement)
		buf.Write(srcBytes[endOffset:])
		newContent = buf.String()

	case "replace_body":
		if !target.isFunc || target.bodyL == token.NoPos || target.bodyR == token.NoPos {
			return "", "", fmt.Errorf("action 'replace_body' is only supported for functions/methods with a body block")
		}
		bStart := fset.Position(target.bodyL).Offset + 1
		bEnd := fset.Position(target.bodyR).Offset
		if bStart < 0 || bEnd > len(srcBytes) || bStart > bEnd {
			return "", "", fmt.Errorf("invalid body brace offset range [%d:%d]", bStart, bEnd)
		}
		cleanRep := strings.TrimSpace(replacement)
		var buf bytes.Buffer
		buf.Write(srcBytes[:bStart])
		buf.WriteString("\n\t" + cleanRep + "\n")
		buf.Write(srcBytes[bEnd:])
		newContent = buf.String()

	case "insert_before":
		var buf bytes.Buffer
		buf.Write(srcBytes[:startOffset])
		buf.WriteString(replacement)
		buf.WriteString("\n\n")
		buf.Write(srcBytes[startOffset:])
		newContent = buf.String()

	case "insert_after":
		var buf bytes.Buffer
		buf.Write(srcBytes[:endOffset])
		buf.WriteString("\n\n")
		buf.WriteString(replacement)
		buf.Write(srcBytes[endOffset:])
		newContent = buf.String()

	default:
		return "", "", fmt.Errorf("unknown action %q (supported: replace_all, replace_body, insert_before, insert_after)", action)
	}

	// ── Pre-Write AST Validation Gate ─────────────────────────────────────────
	valFset := token.NewFileSet()
	valNode, err := parser.ParseFile(valFset, "", newContent, parser.ParseComments)
	if err != nil {
		return "", "", fmt.Errorf("AST syntax validation failed on resulting code: %w\nEdit was rejected to prevent corrupting file", err)
	}

	// Duplicate check: verify no 2 methods/functions share identical signature/receiver
	seenFuncs := make(map[string]int)
	for _, decl := range valNode.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			key := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				key = formatTypeExpr(fn.Recv.List[0].Type) + "." + key
			}
			seenFuncs[key]++
			if seenFuncs[key] > 1 {
				return "", "", fmt.Errorf("duplicate declaration of symbol %q detected after edit; edit rejected", key)
			}
		}
	}

	// Auto-format Go code cleanly
	var formatted bytes.Buffer
	if err := format.Node(&formatted, valFset, valNode); err == nil && formatted.Len() > 0 {
		newContent = formatted.String()
	}

	return newContent, fmt.Sprintf("action: %s, Go AST verified", action), nil
}

func parseSymbolTarget(s string) (recv, name string) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "(")
	s = strings.TrimPrefix(s, "*")
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		recv = strings.Trim(s[:idx], "()*")
		name = s[idx+1:]
		return recv, name
	}
	return "", s
}

func formatTypeExpr(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return formatTypeExpr(t.X)
	case *ast.SelectorExpr:
		return formatTypeExpr(t.X) + "." + t.Sel.Name
	default:
		return fmt.Sprintf("%v", expr)
	}
}

// ─── GENERIC STRUCTURAL SYMBOL ENGINE (JS/TS/PY/ETC.) ───────────────────────

func applyGenericSymbolEdit(content, symbol, replacement, action, ext string) (string, string, error) {
	cleanSym := strings.TrimSpace(symbol)
	if idx := strings.LastIndex(cleanSym, "."); idx >= 0 {
		cleanSym = cleanSym[idx+1:]
	}

	lines := strings.Split(content, "\n")
	type blockMatch struct {
		startLine int
		endLine   int
		indent    string
	}

	var matches []blockMatch

	if ext == ".py" {
		// Python indentation-based block locator
		pyDeclRe := regexp.MustCompile(fmt.Sprintf(`(?m)^(\s*)(def|class)\s+%s\b`, regexp.QuoteMeta(cleanSym)))
		for i, line := range lines {
			if loc := pyDeclRe.FindStringSubmatchIndex(line); loc != nil {
				baseIndent := leadingWhitespace(line)
				endLine := i + 1
				for j := i + 1; j < len(lines); j++ {
					curLine := lines[j]
					if strings.TrimSpace(curLine) == "" {
						endLine = j + 1
						continue
					}
					curIndent := leadingWhitespace(curLine)
					if len(curIndent) <= len(baseIndent) {
						break
					}
					endLine = j + 1
				}
				matches = append(matches, blockMatch{
					startLine: i,
					endLine:   endLine,
					indent:    baseIndent,
				})
			}
		}
	} else {
		// C-style / JS / TS / Rust / PHP / Java brace-based block locator
		declPatterns := []string{
			fmt.Sprintf(`(?i)\b(?:async\s+)?(?:function\s+)?%s\s*\(`, regexp.QuoteMeta(cleanSym)),
			fmt.Sprintf(`(?i)\b(?:const|let|var)\s+%s\s*=\s*`, regexp.QuoteMeta(cleanSym)),
			fmt.Sprintf(`(?i)\b%s\s*:\s*(?:async\s+)?function\b`, regexp.QuoteMeta(cleanSym)),
			fmt.Sprintf(`(?i)\b%s\s*\([^)]*\)\s*\{`, regexp.QuoteMeta(cleanSym)),
			fmt.Sprintf(`(?i)\b(?:class|struct|interface|trait|enum)\s+%s\b`, regexp.QuoteMeta(cleanSym)),
		}

		for i, line := range lines {
			matched := false
			for _, pat := range declPatterns {
				if ok, _ := regexp.MatchString(pat, line); ok {
					matched = true
					break
				}
			}
			if matched {
				fullRemaining := strings.Join(lines[i:], "\n")
				braceStart := strings.Index(fullRemaining, "{")
				if braceStart >= 0 {
					closingIdx := findClosingBrace(fullRemaining, braceStart)
					if closingIdx > 0 {
						sub := fullRemaining[:closingIdx+1]
						subLines := strings.Count(sub, "\n")
						matches = append(matches, blockMatch{
							startLine: i,
							endLine:   i + subLines + 1,
							indent:    leadingWhitespace(line),
						})
					}
				}
			}
		}
	}

	if len(matches) == 0 {
		return "", "", fmt.Errorf("symbol %q not found via structural locator", symbol)
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("symbol %q matched %d locations in %s; use more specific context or disambiguate", symbol, len(matches), ext)
	}

	m := matches[0]
	rLines := strings.Split(strings.TrimRight(replacement, "\n"), "\n")
	var newLines []string

	switch action {
	case "replace_all":
		newLines = append(newLines, lines[:m.startLine]...)
		newLines = append(newLines, rLines...)
		if m.endLine < len(lines) {
			newLines = append(newLines, lines[m.endLine:]...)
		}
	case "insert_before":
		newLines = append(newLines, lines[:m.startLine]...)
		newLines = append(newLines, rLines...)
		newLines = append(newLines, lines[m.startLine:]...)
	case "insert_after":
		newLines = append(newLines, lines[:m.endLine]...)
		newLines = append(newLines, rLines...)
		if m.endLine < len(lines) {
			newLines = append(newLines, lines[m.endLine:]...)
		}
	default:
		return "", "", fmt.Errorf("unknown action %q", action)
	}

	result := strings.Join(newLines, "\n")

	// ── Pre-Write Structural Validation Gate ──────────────────────────────────
	if ext != ".py" {
		if err := validateBrackets(result); err != nil {
			return "", "", fmt.Errorf("pre-write structural validation failed: %w\nEdit rejected to protect file integrity", err)
		}
	}

	return result, fmt.Sprintf("action: %s, structural balance verified", action), nil
}

// findClosingBrace scans s starting at startPos (which must be '{') and finds
// the index of the matching closing '}'. Accurately ignores comments and strings.
func findClosingBrace(s string, startPos int) int {
	runes := []rune(s)
	if startPos < 0 || startPos >= len(runes) || runes[startPos] != '{' {
		return -1
	}
	depth := 0
	inStr := rune(0)
	inLineComment := false
	inBlockComment := false

	for i := startPos; i < len(runes); i++ {
		r := runes[i]
		var prev rune
		if i > 0 {
			prev = runes[i-1]
		}
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		if inLineComment {
			if r == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if prev == '*' && r == '/' {
				inBlockComment = false
			}
			continue
		}
		if inStr != 0 {
			if r == inStr && prev != '\\' {
				inStr = 0
			}
			continue
		}

		// Comment starts
		if r == '/' && next == '/' {
			inLineComment = true
			i++
			continue
		}
		if r == '/' && next == '*' {
			inBlockComment = true
			i++
			continue
		}

		// String starts
		if r == '"' || r == '\'' || r == '`' {
			inStr = r
			continue
		}

		if r == '{' {
			depth++
		} else if r == '}' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// validateBrackets lexically checks that all braces, brackets, and parenthesis
// are properly balanced, ignoring comments and string literals.
func validateBrackets(s string) error {
	runes := []rune(s)
	var stack []rune
	inStr := rune(0)
	inLineComment := false
	inBlockComment := false
	lineNum := 1

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		var prev rune
		if i > 0 {
			prev = runes[i-1]
		}
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		if r == '\n' {
			lineNum++
			inLineComment = false
			continue
		}

		if inLineComment {
			continue
		}
		if inBlockComment {
			if prev == '*' && r == '/' {
				inBlockComment = false
			}
			continue
		}
		if inStr != 0 {
			if r == inStr && prev != '\\' {
				inStr = 0
			}
			continue
		}

		// Comment starts
		if r == '/' && next == '/' {
			inLineComment = true
			i++
			continue
		}
		if r == '/' && next == '*' {
			inBlockComment = true
			i++
			continue
		}

		// String starts
		if r == '"' || r == '\'' || r == '`' {
			inStr = r
			continue
		}

		switch r {
		case '{', '(', '[':
			stack = append(stack, r)
		case '}':
			if len(stack) == 0 || stack[len(stack)-1] != '{' {
				return fmt.Errorf("unmatched '}' near line %d", lineNum)
			}
			stack = stack[:len(stack)-1]
		case ')':
			if len(stack) == 0 || stack[len(stack)-1] != '(' {
				return fmt.Errorf("unmatched ')' near line %d", lineNum)
			}
			stack = stack[:len(stack)-1]
		case ']':
			if len(stack) == 0 || stack[len(stack)-1] != '[' {
				return fmt.Errorf("unmatched ']' near line %d", lineNum)
			}
			stack = stack[:len(stack)-1]
		}
	}

	if inBlockComment {
		return fmt.Errorf("unclosed block comment '/*' at end of file")
	}
	if inStr != 0 {
		return fmt.Errorf("unclosed string literal %c at end of file", inStr)
	}
	if len(stack) > 0 {
		return fmt.Errorf("unclosed %q remaining at end of file", string(stack[len(stack)-1]))
	}
	return nil
}
