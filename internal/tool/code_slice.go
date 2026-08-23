package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/plumpslabs/bro-code/internal/search"
)

// CodeSliceTool extracts an AST-level dependency slice (definition body + inbound callers + outbound callees).
// This provides deep, surgical context without polluting the prompt with thousands of lines of unrelated code.
type CodeSliceTool struct {
	Index *search.GlobalIndex
}

func (t *CodeSliceTool) Name() string { return "code_slice" }
func (t *CodeSliceTool) Description() string {
	return "Extract an exact AST dependency slice for a symbol across the codebase: (1) definition body, (2) inbound callers (who calls it), and (3) outbound dependencies (callees and types it invokes). Eliminates context bloat and hallucination by delivering the exact execution chain instead of 3000-line raw files."
}

func (t *CodeSliceTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Symbol or method name to slice (e.g. 'ProcessPayment', 'OrderController.checkout', 'SaveChannelToDb', 'User')",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Optional file path where the symbol is defined. If omitted, BroCode auto-locates it using the codebase index.",
			},
		},
		"required": []string{"name"},
	}
}

func (t *CodeSliceTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	name := strings.TrimSpace(args.Name)
	if name == "" {
		return "", fmt.Errorf("code_slice requires a symbol name")
	}

	targetPath := resolvePath(args.Path)
	defLine := 0

	// Auto-locate symbol if path is not provided
	if targetPath == "" && t.Index != nil {
		matches := t.Index.Lookup(name)
		if len(matches) == 0 {
			// Try looking up base symbol if method (e.g. "User.GetID" -> "GetID")
			if idx := strings.LastIndex(name, "."); idx != -1 {
				matches = t.Index.Lookup(name[idx+1:])
			}
		}
		if len(matches) > 0 {
			targetPath = matches[0].File
			defLine = matches[0].Line
		}
	}

	if targetPath == "" {
		return "", fmt.Errorf("could not locate symbol %q in codebase. Provide 'path' explicitly or check spelling", name)
	}

	if err := GuardFile(targetPath); err != nil {
		return "", err
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		return "", err
	}

	content := string(data)
	ext := strings.ToLower(filepath.Ext(targetPath))

	// 1. Extract Symbol Snippet
	symbolSnippet, startL, endL := extractSymbolBodySnippet(content, name, ext, defLine)
	if symbolSnippet == "" {
		// Fallback: 30-line window around defLine
		symbolSnippet, startL, endL = extractWindowSnippet(content, defLine, 35)
	}

	relPath, err := filepath.Rel(".", targetPath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		relPath = targetPath
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### 🔪 AST Dependency Slice: `%s` (%s:%d-%d)\n\n", name, filepath.ToSlash(relPath), startL, endL))
	sb.WriteString(fmt.Sprintf("```%s\n%s\n```\n\n", formatLang(ext), symbolSnippet))

	// 2. Extract Inbound Callers (who references/calls this symbol)
	if t.Index != nil {
		referencerFiles := t.Index.Referencers(name)
		if len(referencerFiles) > 0 {
			sb.WriteString("#### 📥 Inbound Callers (References in codebase):\n")
			callerCount := 0
			for _, rf := range referencerFiles {
				if rf == targetPath {
					continue
				}
				snippets := extractCallingLines(rf, name, 3)
				for _, snip := range snippets {
					relF, _ := filepath.Rel(".", rf)
					if relF == "" {
						relF = rf
					}
					sb.WriteString(fmt.Sprintf("• `%s:%d` ➔ `%s`\n", filepath.ToSlash(relF), snip.Line, snip.Code))
					callerCount++
					if callerCount >= 6 {
						break
					}
				}
				if callerCount >= 6 {
					break
				}
			}
			if callerCount == 0 {
				sb.WriteString("• (No direct external callers found in indexed files)\n")
			}
			sb.WriteString("\n")
		}
	}

	// 3. Extract Outbound Dependencies (callees/types referenced inside this symbol)
	if t.Index != nil {
		deps := extractOutboundDependencies(symbolSnippet, name, t.Index)
		if len(deps) > 0 {
			sb.WriteString("#### 📤 Outbound Dependencies (Invoked symbols & types):\n")
			for _, d := range deps {
				relF, _ := filepath.Rel(".", d.File)
				if relF == "" {
					relF = d.File
				}
				sb.WriteString(fmt.Sprintf("• `%s` (%s) ➔ `%s:%d`\n", d.Name, d.Kind, filepath.ToSlash(relF), d.Line))
			}
			sb.WriteString("\n")
		}
	}

	return CapOutput(strings.TrimSpace(sb.String())), nil
}

type callSnippet struct {
	Line int
	Code string
}

func extractCallingLines(filePath, symbol string, limit int) []callSnippet {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(symbol) + `\b`)
	if err != nil {
		return nil
	}

	var out []callSnippet
	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		if re.MatchString(l) {
			clean := strings.TrimSpace(l)
			if len(clean) > 120 {
				clean = clean[:120] + "..."
			}
			out = append(out, callSnippet{Line: i + 1, Code: clean})
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func extractSymbolBodySnippet(content, symbol, ext string, hintLine int) (string, int, int) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return "", 0, 0
	}

	cleanSym := symbol
	if idx := strings.LastIndex(symbol, "."); idx != -1 {
		cleanSym = symbol[idx+1:]
	}

	// Regex search for function/method/class/struct definition
	pattern := fmt.Sprintf(`(?m)^\s*(?:(?:export\s+|public\s+|private\s+|protected\s+|async\s+|static\s+|func\s+|def\s+|class\s+|struct\s+|type\s+)*)(?:func\s+)?(?:\([^\)]+\)\s+)?%s\b`, regexp.QuoteMeta(cleanSym))
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", 0, 0
	}

	targetLine := -1
	if hintLine > 0 && hintLine <= len(lines) {
		targetLine = hintLine - 1
	} else {
		for i, l := range lines {
			if re.MatchString(l) {
				targetLine = i
				break
			}
		}
	}

	if targetLine == -1 {
		return "", 0, 0
	}

	// Capture block using bracket balancing or indent heuristic
	start := targetLine
	end := start
	openBraces := 0
	foundOpen := false

	for i := start; i < len(lines) && i < start+150; i++ {
		line := lines[i]
		openBraces += strings.Count(line, "{")
		openBraces -= strings.Count(line, "}")
		if strings.Contains(line, "{") {
			foundOpen = true
		}
		end = i
		if foundOpen && openBraces <= 0 {
			break
		}
	}

	if !foundOpen {
		// Python / indentation based
		baseIndent := len(lines[start]) - len(strings.TrimLeft(lines[start], " \t"))
		for i := start + 1; i < len(lines) && i < start+100; i++ {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "" {
				continue
			}
			curIndent := len(lines[i]) - len(strings.TrimLeft(lines[i], " \t"))
			if curIndent <= baseIndent && !strings.HasPrefix(trimmed, "#") {
				end = i - 1
				break
			}
			end = i
		}
	}

	if end < start {
		end = start
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}

	return strings.Join(lines[start:end+1], "\n"), start + 1, end + 1
}

func extractWindowSnippet(content string, line, span int) (string, int, int) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return "", 0, 0
	}
	if line <= 0 {
		line = 1
	}
	start := line - 1
	if start >= len(lines) {
		start = len(lines) - 1
	}
	end := start + span
	if end >= len(lines) {
		end = len(lines) - 1
	}
	return strings.Join(lines[start:end+1], "\n"), start + 1, end + 1
}

func extractOutboundDependencies(snippet, targetSym string, idx *search.GlobalIndex) []search.IndexedSymbol {
	if idx == nil || snippet == "" {
		return nil
	}

	// Find identifier words in snippet
	idRe := regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]{3,}\b`)
	words := idRe.FindAllString(snippet, -1)

	seen := map[string]bool{targetSym: true}
	var out []search.IndexedSymbol

	for _, w := range words {
		if seen[w] {
			continue
		}
		seen[w] = true
		matches := idx.Lookup(w)
		if len(matches) > 0 {
			out = append(out, matches[0])
			if len(out) >= 6 {
				break
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})

	return out
}

func formatLang(ext string) string {
	switch ext {
	case ".go":
		return "go"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".json":
		return "json"
	default:
		return ""
	}
}
