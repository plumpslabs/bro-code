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

// brocodeIdentityPrompt anchors the model's identity when it runs inside the
// local OpenCode CLI, whose own system prompt can claim a different identity
// that BroCode cannot override (opencode run has no system-prompt flag). A
// short firm preamble in the user message tells the model who it is talking
// to. Kept minimal and brand-agnostic (it never names other tools) so it only
// shapes identity questions like "who are you?" — never task behavior.
const brocodeIdentityPrompt = `You are BroCode, a terminal coding agent for software engineering (writing, debugging, refactoring, and explaining code). The user is talking to you through BroCode. You are NOT opencode and you are not any other tool — never introduce yourself as opencode or claim to be another product. If asked who you are, or when greeting the user, say you are BroCode.`

// brocodeCapabilityNote orients the CLI model about BroCode's architecture so
// capability questions ("whose subagents?", "can you spawn subagents?", "do
// you have LSP?") are answered directly from context instead of triggering
// filesystem exploration of config directories (.opencode, ~/.config/opencode,
// agent definition files) — which the gateway's own permission system then
// rejects, wasting turns. It only shapes answers about identity/tools — never
// task behavior.
const brocodeCapabilityNote = `You are BroCode, running through a local gateway runtime. If asked about your subagents or capabilities, answer directly from this context — do NOT explore configuration directories (.opencode, ~/.config/opencode, agent files) to answer. BroCode's native engine has its own "subagent" tool (isolated sub-agents, optionally parallel) and "scout" tool (background research); in this session you can only call the gateway runtime's own tools (e.g. its task tool), so be honest about which ones you actually have.`

// askMarkerInstructions is appended to the prompt when the OpenCode CLI model
// runs with an interactive ask handler wired, so its clarification questions
// come back as a structured block that can be turned into the selection modal.
// Without a handler (headless) the model never sees it and behaves as before.
const askMarkerInstructions = `IMPORTANT — if you need the user to make a decision, choose between options, or confirm requirements before you can continue, do NOT end your message with an open question. Instead append a QUESTION BLOCK:

[Q]The question text[/Q]
[O]Option 1[/O]
[O]Option 2[/O]
[O]Option 3[/O]
[M]true[/M]

Rules:
- One [Q] block per question, immediately followed by 2-6 [O] option lines and an optional [M] line. You may ask up to 3 questions.
- [M]true[/M] means the user may select multiple options; omit it (or write [M]false[/M]) for a single choice.
- You may write your analysis BEFORE the question blocks — they will be turned into an interactive selection UI for the user.
- If you do not need clarification, answer normally and include NO question blocks.`

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
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, r.Question))
		answer := strings.Join(r.Answers, "; ")
		if answer == "" {
			answer = "(no selection)"
		}
		sb.WriteString("   Answer: " + answer + "\n")
		if r.Custom != "" {
			sb.WriteString("   Custom: " + r.Custom + "\n")
		}
	}
	return strings.TrimSpace(sb.String())
}
