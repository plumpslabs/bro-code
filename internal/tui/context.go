package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// attachFileContext inspects the user query for file references or symbol keywords,
// automatically reading project tree and target files to attach rich workspace context.
func attachFileContext(q string) string {
	var ctxBuf strings.Builder
	seen := make(map[string]bool)

	// 1. Auto-attach workspace project tree structure (<5ms)
	tree := getProjectTree()
	if tree != "" {
		ctxBuf.WriteString("PROJECT WORKSPACE TREE:\n```\n")
		ctxBuf.WriteString(tree)
		ctxBuf.WriteString("\n```\n\n")
	}

	// 2. Line-range regex: matches app.go:100-200 or app.go#L50-100
	rangeRegex := regexp.MustCompile(`([a-zA-Z0-9_/-]+\.[a-zA-Z0-9_-]+)[:#][lL]?([0-9]+)(?:-([0-9]+))?`)
	for _, match := range rangeRegex.FindAllStringSubmatch(q, -1) {
		path := match[1]
		if seen[path] {
			continue
		}
		if data, err := os.ReadFile(path); err == nil {
			seen[path] = true
			lines := strings.Split(string(data), "\n")
			startLine := 1
			endLine := len(lines)
			if match[2] != "" {
				fmt.Sscanf(match[2], "%d", &startLine)
			}
			if match[3] != "" {
				fmt.Sscanf(match[3], "%d", &endLine)
			}
			if startLine < 1 {
				startLine = 1
			}
			if endLine > len(lines) {
				endLine = len(lines)
			}

			ctxBuf.WriteString(fmt.Sprintf("FILE ATTACHMENT: %s (lines %d-%d of %d)\n```\n", path, startLine, endLine, len(lines)))
			for i := startLine - 1; i < endLine && i < len(lines); i++ {
				ctxBuf.WriteString(fmt.Sprintf("%4d | %s\n", i+1, lines[i]))
			}
			ctxBuf.WriteString("```\n\n")
		}
	}

	// 3. Simple file path matches (e.g. README.md, Makefile, main.go)
	words := strings.Fields(q)
	for _, w := range words {
		clean := strings.Trim(w, "\"'`,()[]{}?")
		if clean == "" || seen[clean] || strings.Contains(clean, "..") {
			continue
		}
		if fi, err := os.Stat(clean); err == nil && !fi.IsDir() {
			if data, err := os.ReadFile(clean); err == nil {
				seen[clean] = true
				lines := strings.Split(string(data), "\n")
				maxLines := min(250, len(lines))
				ctxBuf.WriteString(fmt.Sprintf("FILE ATTACHMENT: %s (%d lines · %.1f KB)\n```\n", clean, len(lines), float64(fi.Size())/1024.0))
				for i := 0; i < maxLines; i++ {
					ctxBuf.WriteString(lines[i] + "\n")
				}
				if len(lines) > maxLines {
					ctxBuf.WriteString(fmt.Sprintf("... (%d more lines truncated for context space)\n", len(lines)-maxLines))
				}
				ctxBuf.WriteString("```\n\n")
			}
		}
	}

	// 4. Keyword search across workspace files if query contains specific terms
	if searchCtx := searchProjectFiles(q, seen); searchCtx != "" {
		ctxBuf.WriteString(searchCtx)
	}

	if ctxBuf.Len() > 0 {
		return ctxBuf.String() + "USER PROMPT:\n" + q
	}
	return q
}

// searchProjectFiles performs auto-search across workspace files for query terms.
func searchProjectFiles(q string, seen map[string]bool) string {
	var sb strings.Builder
	terms := tokenizeQuery(q)
	if len(terms) == 0 {
		return ""
	}

	count := 0
	_ = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || count >= 4 {
			return nil
		}
		if shouldIgnorePath(path) || seen[path] {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			content := string(data)
			score := calculateRelevance(content, terms)
			if score > 0 {
				seen[path] = true
				count++
				lines := strings.Split(content, "\n")
				maxL := min(120, len(lines))
				sb.WriteString(fmt.Sprintf("RELEVANT SEARCH CONTEXT (%s · relevance score %d):\n```\n", path, score))
				for i := 0; i < maxL; i++ {
					sb.WriteString(lines[i] + "\n")
				}
				if len(lines) > maxL {
					sb.WriteString(fmt.Sprintf("... (%d more lines)\n", len(lines)-maxL))
				}
				sb.WriteString("```\n\n")
			}
		}
		return nil
	})
	return sb.String()
}

// tokenizeQuery splits a query into lowercase terms for relevance matching.
func tokenizeQuery(q string) []string {
	var terms []string
	for _, w := range strings.Fields(strings.ToLower(q)) {
		w = strings.Trim(w, "\"'`,()[]{}?")
		if len(w) > 2 {
			terms = append(terms, w)
		}
	}
	return terms
}

// shouldIgnorePath returns true for common non-source directories.
func shouldIgnorePath(path string) bool {
	ignore := []string{".git", "node_modules", "vendor", ".brocode", ".agents", "bin", "dist"}
	for _, ig := range ignore {
		if strings.Contains(path, "/"+ig+"/") || strings.HasPrefix(path, ig+"/") {
			return true
		}
	}
	return false
}

// calculateRelevance scores content against query terms using simple keyword matching.
func calculateRelevance(content string, terms []string) int {
	lower := strings.ToLower(content)
	score := 0
	for _, t := range terms {
		score += strings.Count(lower, t)
	}
	return score
} // getProjectTree returns a concise workspace directory tree.
func getProjectTree() string {
	var sb strings.Builder
	count := 0
	_ = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || count > 35 {
			return nil
		}
		if shouldIgnorePath(path) {
			if info.IsDir() && path != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if path == "." {
			return nil
		}
		depth := strings.Count(path, string(filepath.Separator))
		indent := strings.Repeat("  ", depth)
		if info.IsDir() {
			sb.WriteString(fmt.Sprintf("%s📁 %s/\n", indent, filepath.Base(path)))
		} else {
			sb.WriteString(fmt.Sprintf("%s📄 %s\n", indent, filepath.Base(path)))
		}
		count++
		return nil
	})
	return sb.String()
}

// extractDynamicSubagents inspects user query AND agent response text for genuine subagent delegations.
// Subagents are ONLY spawned when explicitly requested by user prompt or delegated by agent.
func extractDynamicSubagents(userQuery, text, currentProvider, currentModel string) ([]subagentState, []string) {
	var subagents []subagentState
	var traceLogs []string
	seen := make(map[string]bool)

	// Check if user explicitly invoked a subagent (@planner, @auditor, etc.)
	subReg := regexp.MustCompile(`(?i)@([a-zA-Z0-9_-]{3,30})`)
	userMatches := subReg.FindAllStringSubmatch(userQuery, -1)

	// Check if agent explicitly declared delegation ("delegating to @...", "spawning @...")
	delegReg := regexp.MustCompile(`(?i)(?:delegating to|spawning|delegate to|executing)\s+@([a-zA-Z0-9_-]{3,30})`)
	agentMatches := delegReg.FindAllStringSubmatch(text, -1)

	allMatches := append(userMatches, agentMatches...)
	for _, m := range allMatches {
		rawName := strings.ToLower(m[1])
		if rawName == "" || seen[rawName] || rawName == "brocode" || rawName == "here" || rawName == "everyone" {
			continue
		}
		name := rawName
		if !strings.HasPrefix(name, "matcha-") && (name == "planner" || name == "finder" || name == "auditor" || name == "reviewer" || name == "cleaner" || name == "debugger") {
			name = "matcha-" + name
		}

		prov := currentProvider
		mod := currentModel
		if strings.Contains(name, "reviewer") || strings.Contains(name, "auditor") {
			prov = "antigravity"
			mod = "gemini-3.6-flash"
		} else if strings.Contains(name, "finder") || strings.Contains(name, "debugger") {
			prov = "opencode"
			mod = "deepseek-v4-flash-free"
		}
		seen[rawName] = true
		subagents = append(subagents, subagentState{
			name:     name,
			task:     "subagent task",
			provider: prov,
			model:    mod,
			status:   "done",
		})
		traceLogs = append(traceLogs, fmt.Sprintf("● spawn(@%s) → subagent task\n  ⎿  Delegated to %s (%s)", name, mod, prov))
	}
	return subagents, traceLogs
}

// saveMatchaPlan automatically saves/persists engineering plans into .agents/plan/current.md
func saveMatchaPlan(text string) string {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "plan") && !strings.Contains(lower, "roadmap") && !strings.Contains(lower, "step 1") && !strings.Contains(lower, "checkpoint") {
		return ""
	}
	_ = os.MkdirAll(".agents/plan", 0o755)
	planPath := ".agents/plan/current.md"
	if err := os.WriteFile(planPath, []byte(text), 0o644); err == nil {
		return planPath
	}
	return ""
}
