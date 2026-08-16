package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// AskQuestion is a single interactive multiple-choice question presented to the
// user. It lives in provider (not tool) so the OpenCode CLI adapter can present
// clarification questions through the same interactive modal as the ask_user
// tool — the tool package aliases these types to keep one source of truth
// without an import cycle.
type AskQuestion struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Multi    bool     `json:"multi"`
}

// AskResult is the user's answer to one question.
type AskResult struct {
	Question string   `json:"question"`
	Answers  []string `json:"answers"`
	Custom   string   `json:"custom,omitempty"`
}

// AskUserHandler presents interactive questions to the user and blocks until
// they answer. Wired by the TUI; nil means headless (no modal possible).
type AskUserHandler func(ctx context.Context, questions []AskQuestion) ([]AskResult, error)

var (
	askQRe = regexp.MustCompile(`(?s)\[Q\](.*?)\[/Q\]`)
	askORe = regexp.MustCompile(`(?s)\[O\](.*?)\[/O\]`)
	askMRe = regexp.MustCompile(`(?s)\[M\](.*?)\[/M\]`)
)

// ParseAskBlocks extracts structured question blocks ([Q]...[/Q] with [O]
// options and an optional [M] multi flag) from model output. It returns the
// parsed questions plus the text with all marker blocks removed (the model's
// own analysis around the questions is preserved). When no question blocks are
// present it returns nil and the original text unchanged. ANSI escapes are
// stripped first so colored CLI output still parses.
func ParseAskBlocks(text string) ([]AskQuestion, string) {
	text = ansiEscapeRe.ReplaceAllString(text, "")
	qIdx := askQRe.FindAllStringSubmatchIndex(text, -1)
	if len(qIdx) == 0 {
		return nil, text
	}

	var questions []AskQuestion
	for i, m := range qIdx {
		qEnd := m[1]
		qText := strings.TrimSpace(text[m[2]:m[3]])
		if qText == "" {
			continue
		}
		// Options belong to the question that precedes them: the span runs from
		// this block's end to the next [Q] (or the end of the text).
		spanEnd := len(text)
		if i+1 < len(qIdx) {
			spanEnd = qIdx[i+1][0]
		}
		span := text[qEnd:spanEnd]

		var opts []string
		for _, om := range askORe.FindAllStringSubmatchIndex(span, -1) {
			if opt := strings.TrimSpace(span[om[2]:om[3]]); opt != "" {
				opts = append(opts, opt)
			}
		}

		multi := false
		if mm := askMRe.FindStringSubmatch(span); mm != nil {
			v := strings.ToLower(strings.TrimSpace(mm[1]))
			multi = v == "true" || v == "multi" || v == "yes" || v == "1"
		}

		questions = append(questions, AskQuestion{
			Question: qText,
			Options:  opts,
			Multi:    multi,
		})
	}
	if len(questions) == 0 {
		return nil, text
	}

	// Remove all marker blocks, leaving the surrounding analysis intact.
	cleaned := askQRe.ReplaceAllString(text, "")
	cleaned = askORe.ReplaceAllString(cleaned, "")
	cleaned = askMRe.ReplaceAllString(cleaned, "")
	cleaned = removeEmptyFences(cleaned)
	return questions, strings.TrimSpace(cleaned)
}

// removeEmptyFences drops ``` fence pairs left empty after marker removal
// (the model sometimes wraps the question block in a code fence). Real code
// blocks — a fence line followed by non-fence content — are left untouched.
func removeEmptyFences(s string) string {
	isFence := func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "```") }
	lines := strings.Split(s, "\n")
	drop := make([]bool, len(lines))
	for i := 0; i < len(lines); i++ {
		if drop[i] || !isFence(lines[i]) {
			continue
		}
		// Find the next non-blank line; if it is also a fence, both were the
		// emptied block's open/close and are dropped together.
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		if j < len(lines) && isFence(lines[j]) {
			drop[i] = true
			drop[j] = true
			i = j
		}
	}
	var out []string
	for i, l := range lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// formatAskResults renders the user's answers back into a continuation prompt
// so the (stateless) CLI run can pick up where the clarification left off.
func formatAskResults(results []AskResult) string {
	var sb strings.Builder
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, r.Question)
		answer := strings.Join(r.Answers, "; ")
		if answer == "" {
			answer = "(no selection)"
		}
		sb.WriteString("   Answer: ");sb.WriteString(answer);sb.WriteString("\n")
		if r.Custom != "" {
			sb.WriteString("   Custom: ");sb.WriteString(r.Custom);sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String())
}
