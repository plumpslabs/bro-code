package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/agentic"
	"github.com/plumpslabs/bro-code/internal/diff"
)

// applyBuilderCodeBlocks inspects the final agent reply in Builder mode for code blocks
// or heredocs (e.g. cat > filename << 'EOF' ... EOF) and automatically updates/writes files on disk.
func applyBuilderCodeBlocks(text string, userQuery string) []string {
	var logs []string
	seen := make(map[string]bool)

	writeFile := func(filename string, content string) bool {
		filename = strings.Trim(filename, "\"'`")
		if filename == "" || seen[filename] || strings.Contains(filename, "..") {
			return false
		}
		var oldContent string
		if oldData, err := os.ReadFile(filename); err == nil {
			oldContent = string(oldData)
		}

		// RISK ENGINE: Evaluate risk level and take snapshot if necessary
		risk := agentic.EvaluateFileRisk(filename)
		if risk >= agentic.L2_High {
			_ = agentic.Snapshot(filename)
			logs = append(logs, fmt.Sprintf("🛡️  Risk Engine: Auto-snapshot created for high-risk file: %s", filename))
		}

		if err := os.WriteFile(filename, []byte(content), 0644); err == nil {
			seen[filename] = true
			lines := strings.Split(strings.TrimSpace(content), "\n")
			var diffLines []string

			if oldContent != "" {
				// Compute real unified diff using Myers diff (internal/diff)
				u := diff.Unified(filename, filename, oldContent, content)
				uLines := strings.Split(u, "\n")
				if len(uLines) > 2 {
					uLines = uLines[2:] // skip header lines
				}
				maxL := min(8, len(uLines))
				for i := 0; i < maxL; i++ {
					if strings.TrimSpace(uLines[i]) != "" {
						diffLines = append(diffLines, "      "+uLines[i])
					}
				}
				if len(uLines) > 8 {
					diffLines = append(diffLines, fmt.Sprintf("          … and %d more diff lines", len(uLines)-8))
				}
			} else {
				maxLines := min(5, len(lines))
				for i := 0; i < maxLines; i++ {
					diffLines = append(diffLines, fmt.Sprintf("      %4d +  %s", i+1, lines[i]))
				}
				if len(lines) > 5 {
					diffLines = append(diffLines, fmt.Sprintf("          … and %d more lines", len(lines)-5))
				}
			}
			logs = append(logs, fmt.Sprintf("● Edit(%s)\n  ⎿  Updated %d lines\n%s", filename, len(lines), strings.Join(diffLines, "\n")))
			return true
		}
		return false
	}

	// Pattern 1: cat > filename << 'EOF' ... (EOF, ```, or end of string)
	catRegex := regexp.MustCompile("(?s)cat\\s+>\\s+([^\\s<]+)\\s+<<\\s*['\"]?EOF['\"]?\\s*\\n(.*?)(?:\\nEOF|```|\\z)")
	for _, m := range catRegex.FindAllStringSubmatch(text, -1) {
		writeFile(m[1], strings.TrimSpace(m[2]))
	}

	// Pattern 2: ```lang:path/to/file or ```path/to/file (allowing optional leading indentation / spaces)
	blockRegex := regexp.MustCompile("(?s)(?:^|\\n)\\s*```[a-zA-Z0-9_-]*:([^\\s\\n]+)\\n(.*?)\\n\\s*```")
	for _, m := range blockRegex.FindAllStringSubmatch(text, -1) {
		writeFile(m[1], strings.TrimSpace(m[2]))
	}

	// Pattern 3: Fallback if user explicitly referenced a file path in prompt (e.g. test.md or README.md)
	// and AI outputted a code block without explicit file header. Supports creating NEW files.
	if len(seen) == 0 && userQuery != "" {
		for _, w := range strings.Fields(userQuery) {
			cleanPath := strings.Trim(w, "\"'`,()[]{}?")
			if cleanPath == "" || strings.Contains(cleanPath, "..") {
				continue
			}
			ext := filepath.Ext(cleanPath)
			isValidTarget := ext != "" || strings.HasPrefix(cleanPath, ".") || strings.Contains(cleanPath, "/")
			if isValidTarget {
				codeBlockRegex := regexp.MustCompile("(?s)(?:^|\\n)\\s*```[a-zA-Z0-9_-]*\\n(.*?)\\n\\s*```")
				if match := codeBlockRegex.FindStringSubmatch(text); len(match) > 1 {
					code := strings.TrimSpace(match[1])
					if len(code) > 2 && !strings.HasPrefix(code, "cat >") {
						writeFile(cleanPath, code)
						break
					}
				}
			}
		}
	}

	return logs
}

// applyAgenticTools inspects the LLM output for tool execution requests.
// It supports both Markdown bash blocks (```bash ... ```) and XML `<tool_call>` tags.
// Returns (traceLogs, feedbackText) where feedbackText is sent back to the LLM for the next turn.
func applyAgenticTools(text string) ([]string, string) {
	var logs []string
	var feedbackSb strings.Builder
	executed := false

	// Pattern 1: Markdown bash blocks
	bashRegex := regexp.MustCompile("(?s)(?:^|\\n)\\s*```(?:bash|sh)\\n(.*?)\\n\\s*```")
	for _, m := range bashRegex.FindAllStringSubmatch(text, -1) {
		cmdStr := strings.TrimSpace(m[1])
		if cmdStr == "" || strings.HasPrefix(cmdStr, "cat >") {
			continue // Handled by builder file writer
		}
		
		logs = append(logs, fmt.Sprintf("⚙️  Running command: %s", cmdStr))
		out, err := agentic.RunCommandNative(cmdStr, agentic.ToolOptions{Timeout: 30 * time.Second})
		
		feedbackSb.WriteString(fmt.Sprintf("Result of `%s`:\n```\n%s\n```\n", cmdStr, out))
		if err != nil {
			feedbackSb.WriteString(fmt.Sprintf("Error: %v\n", err))
		}
		executed = true
	}

	// Pattern 2: XML <tool_call> tags (Poolside/Laguna format fallback)
	xmlRegex := regexp.MustCompile("(?s)<tool_call>(.*?)</tool_call>")
	for _, m := range xmlRegex.FindAllStringSubmatch(text, -1) {
		toolContent := strings.TrimSpace(m[1])
		
		idx := strings.Index(toolContent, "<")
		toolName := toolContent
		if idx != -1 {
			toolName = strings.TrimSpace(toolContent[:idx])
		}

		if toolName == "read" {
			pathRegex := regexp.MustCompile("(?s)<arg_key>path</arg_key><arg_value>(.*?)</arg_value>")
			if pathMatch := pathRegex.FindStringSubmatch(toolContent); len(pathMatch) > 1 {
				path := pathMatch[1]
				logs = append(logs, fmt.Sprintf("📖 Reading file: %s", path))
				if data, err := os.ReadFile(path); err == nil {
					feedbackSb.WriteString(fmt.Sprintf("Content of %s:\n```\n%s\n```\n", path, string(data)))
				} else {
					feedbackSb.WriteString(fmt.Sprintf("Failed to read %s: %v\n", path, err))
				}
				executed = true
			}
		} else {
			// Catch hallucinations like 'kuma_context' and force loop correction
			logs = append(logs, fmt.Sprintf("⚠️  Unsupported tool call: %s", toolName))
			feedbackSb.WriteString(fmt.Sprintf("Error: unsupported tool '%s'. BroCode does NOT support this. You must use ```bash ... ``` blocks for all commands other than 'read'.\n", toolName))
			executed = true
		}
	}

	if executed {
		return logs, feedbackSb.String()
	}
	return nil, ""
}
