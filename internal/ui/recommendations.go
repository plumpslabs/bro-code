package ui

import (
	"fmt"
	"regexp"
	"strings"
)

// QuickRecommendation represents an actionable follow-up suggestion proposed by the AI.
type QuickRecommendation struct {
	Index   int    `json:"index"` // 1, 2, 3...
	Title   string `json:"title"` // e.g. "Lanjutkan test auth"
	Prompt  string `json:"prompt"` // e.g. "Lanjutkan test auth dengan beberapa validator"
	Clicked bool   `json:"clicked"`
}

var (
	// Matches markdown checklist format: - [ ] **Title** — Prompt or - [ ] **Title**: Prompt
	recChecklistRe = regexp.MustCompile(`(?m)^[-*]\s*\[\s*\]\s*\*\*([^*]+)\*\*\s*[—–\-:]\s*(.+)$`)
	// Matches numbered format: 1. **Title** — Prompt or 1. [Title] — Prompt
	recNumberedRe = regexp.MustCompile(`(?m)^\d+\.\s*(?:\*\*|\[)([^\]*]+)(?:\*\*|\])\s*[—–\-:]\s*(.+)$`)
	// Matches bullet format: - **Title** — Prompt
	recBulletRe = regexp.MustCompile(`(?m)^[-*]\s*\*\*([^*]+)\*\*\s*[—–\-:]\s*(.+)$`)
)

// ExtractRecommendations parses senior recommendations from the end of an assistant's response.
func ExtractRecommendations(content string) []QuickRecommendation {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	// Look for recommendations heading
	headings := []string{
		"### 💡 Senior Recommendations",
		"### Senior Recommendations",
		"### 💡 Senior Next Actions",
		"### Senior Next Actions",
		"### 💡 Next Actions",
		"### 💡 Next Steps",
		"## 💡 Senior Recommendations",
	}

	startIdx := -1
	for _, h := range headings {
		if idx := strings.LastIndex(content, h); idx != -1 {
			startIdx = idx + len(h)
			break
		}
	}

	var targetText string
	if startIdx != -1 {
		targetText = content[startIdx:]
	} else {
		// If no explicit heading, don't parse arbitrary bullets to avoid false positives
		return nil
	}

	var recs []QuickRecommendation
	idx := 1

	// 1. Try checklist regex
	matches := recChecklistRe.FindAllStringSubmatch(targetText, 5)
	if len(matches) == 0 {
		matches = recNumberedRe.FindAllStringSubmatch(targetText, 5)
	}
	if len(matches) == 0 {
		matches = recBulletRe.FindAllStringSubmatch(targetText, 5)
	}

	for _, m := range matches {
		if len(m) >= 3 {
			title := strings.TrimSpace(m[1])
			prompt := strings.TrimSpace(m[2])
			if title != "" && prompt != "" {
				recs = append(recs, QuickRecommendation{
					Index:   idx,
					Title:   title,
					Prompt:  prompt,
					Clicked: false,
				})
				idx++
				if len(recs) >= 3 {
					break
				}
			}
		}
	}

	return recs
}

// RenderRecommendationsBar formats the quick recommendations into an interactive TUI card.
func RenderRecommendationsBar(recs []QuickRecommendation, width int) string {
	if len(recs) == 0 {
		return ""
	}

	var lines []string
	hasActive := false
	for _, r := range recs {
		if r.Clicked {
			lines = append(lines, fmt.Sprintf("  ~~[%d] %s~~ (✓ Queued/Executed)", r.Index, r.Title))
		} else {
			hasActive = true
			lines = append(lines, fmt.Sprintf("  \x1b[36m[%d]\x1b[0m \x1b[1m%s\x1b[0m ── \x1b[90m\"%s\"\x1b[0m", r.Index, r.Title, truncatePrompt(r.Prompt)))
		}
	}

	if len(lines) == 0 {
		return ""
	}

	header := "💡 Senior Next Actions (Type [1]-[3] or click to execute/queue):"
	if !hasActive {
		header = "💡 Senior Next Actions (All executed):"
	}

	var sb strings.Builder
	sb.WriteString("\x1b[90m╭─ " + header + " ─\x1b[0m\n")
	for _, l := range lines {
		sb.WriteString(l + "\n")
	}
	sb.WriteString("\x1b[90m╰───────────────────────────────────────────────────────────────────\x1b[0m")
	return sb.String()
}
