package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/plumpslabs/bro-code/internal/agentic"
	"github.com/plumpslabs/bro-code/internal/diff"
)

// File-writing block patterns shared by applyBuilderCodeBlocks (the end-of-
// reply sweep) and editBlockSpans (the streaming interleave). Keeping the
// regexes here guarantees the two paths agree on what counts as an edit block.
var (
	// Pattern 0: <<<<<<< SEARCH[: filename] ... ======= ... >>>>>>> REPLACE
	srEditRegex = regexp.MustCompile(`(?s)<<{3,7}\s*SEARCH:?\s*([^\n\r]*)\r?\n(.*?)\r?\n={3,7}\r?\n(.*?)\r?\n>{3,7}\s*REPLACE`)
	// Pattern 1: cat > filename << 'EOF' ... (EOF, ```, or end of string) —
	// quoted string: the pattern itself contains ``` and quotes.
	catEditRegex = regexp.MustCompile("(?s)cat\\s+>\\s+([^\\s<]+)\\s+<<\\s*['\"]?EOF['\"]?\\s*\\n(.*?)(?:\\nEOF|```|\\z)")
	// Pattern 2: ```lang:path/to/file or ```path/to/file
	blockEditRegex = regexp.MustCompile("(?s)(?:^|\\n)\\s*```[a-zA-Z0-9_-]*:([^\\s\\n]+)\\n(.*?)\\n\\s*```")
)

// editChange is one file write applied by applyBuilderCodeBlocks: the target
// file, the line count, and the FULL unified diff body (header lines removed)
// so the TUI can offer a collapsible green/red diff — the trace shows a short
// ● Edit row, ctrl+o reveals the complete change set.
type editChange struct {
	file  string
	lines int
	diff  string // full unified diff body (no ---/+++ header lines)
}

// applyBuilderCodeBlocks inspects the final agent reply in Builder mode for code blocks
// or heredocs (e.g. cat > filename << 'EOF' ... EOF) and automatically updates/writes files on disk.
// It returns the short trace logs AND the full per-file diffs (for the collapsible block).
func applyBuilderCodeBlocks(text string, userQuery string, plannerMode bool) ([]string, []editChange) {
	var logs []string
	var edits []editChange
	seen := make(map[string]bool)

	writeFile := func(filename string, content string) bool {
		filename = strings.Trim(filename, "\"'`")
		if filename == "" || seen[filename] || strings.Contains(filename, "..") {
			return false
		}
		if plannerMode && !strings.HasSuffix(filename, "brocode_plan.md") {
			logs = append(logs, fmt.Sprintf("⛔ Planner Mode: file edit for %s blocked. Press Shift+Tab to switch to Builder Mode.", filename))
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
			var fullDiff string

			if oldContent != "" {
				// Compute real unified diff using Myers diff (internal/diff)
				u := diff.Unified(filename, filename, oldContent, content)
				uLines := strings.Split(u, "\n")
				if len(uLines) > 2 {
					uLines = uLines[2:] // skip header lines
				}
				fullDiff = strings.Join(uLines, "\n")
				maxL := min(8, len(uLines))
				for i := 0; i < maxL; i++ {
					if strings.TrimSpace(uLines[i]) != "" {
						diffLines = append(diffLines, "      "+uLines[i])
					}
				}
				if len(uLines) > 8 {
					diffLines = append(diffLines, fmt.Sprintf("          … and %d more diff lines (ctrl+o for full)", len(uLines)-8))
				}
			} else {
				// New file: full diff = every line as an addition.
				var all []string
				for _, l := range lines {
					all = append(all, fmt.Sprintf("+  %s", l))
				}
				fullDiff = strings.Join(all, "\n")
				maxLines := min(5, len(lines))
				for i := 0; i < maxLines; i++ {
					diffLines = append(diffLines, fmt.Sprintf("      %4d +  %s", i+1, lines[i]))
				}
				if len(lines) > 5 {
					diffLines = append(diffLines, fmt.Sprintf("          … and %d more lines (ctrl+o for full)", len(lines)-5))
				}
			}
			logs = append(logs, fmt.Sprintf("● Edit(%s)\n  ⎿  Updated %d lines\n%s", filename, len(lines), strings.Join(diffLines, "\n")))
			edits = append(edits, editChange{file: filename, lines: len(lines), diff: fullDiff})
			return true
		}
		return false
	}

	// Pattern 0: Search & Replace blocks for precision chunk edits:
	// <<<<<<< SEARCH[: filename]
	// ... search content ...
	// =======
	// ... replacement content ...
	// >>>>>>> REPLACE
	for _, m := range srEditRegex.FindAllStringSubmatch(text, -1) {
		targetFile := strings.TrimPrefix(strings.TrimSpace(m[1]), ": ")
		targetFile = strings.Trim(targetFile, "\"'`:")
		searchStr := m[2]
		replaceStr := m[3]

		if targetFile == "" && userQuery != "" {
			// Fallback target file from user query if omitted in header
			for _, w := range strings.Fields(userQuery) {
				cleanPath := strings.Trim(w, "\"'`,()[]{}?")
				if cleanPath != "" && !strings.Contains(cleanPath, "..") && filepath.Ext(cleanPath) != "" {
					targetFile = cleanPath
					break
				}
			}
		}

		if targetFile != "" {
			if oldData, err := os.ReadFile(targetFile); err == nil {
				oldContent := string(oldData)
				if strings.Contains(oldContent, searchStr) {
					newContent := strings.Replace(oldContent, searchStr, replaceStr, 1)
					writeFile(targetFile, newContent)
				}
			}
		}
	}

	// Pattern 1: cat > filename << 'EOF' ... (EOF, ```, or end of string)
	for _, m := range catEditRegex.FindAllStringSubmatch(text, -1) {
		writeFile(m[1], strings.TrimSpace(m[2]))
	}

	// Pattern 2: ```lang:path/to/file or ```path/to/file (allowing optional leading indentation / spaces)
	for _, m := range blockEditRegex.FindAllStringSubmatch(text, -1) {
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
					if len(code) > 2 && !strings.HasPrefix(code, "cat >") && !strings.HasPrefix(code, "<<<<<<<") {
						writeFile(cleanPath, code)
						break
					}
				}
			}
		}
	}

	return logs, edits
}

// editBlockSpans returns the byte spans of every file-writing block in text
// (search/replace, cat heredoc, ```lang:path fence), sorted by start position.
// Each span is exactly the slice applyBuilderCodeBlocks would consume for that
// block — the streaming interleave uses this to apply blocks AS they complete
// during reveal instead of sweeping the whole reply at the end.
func editBlockSpans(text string) [][2]int {
	var spans [][2]int
	for _, m := range srEditRegex.FindAllStringSubmatchIndex(text, -1) {
		spans = append(spans, [2]int{m[0], m[1]})
	}
	for _, m := range catEditRegex.FindAllStringSubmatchIndex(text, -1) {
		spans = append(spans, [2]int{m[0], m[1]})
	}
	for _, m := range blockEditRegex.FindAllStringSubmatchIndex(text, -1) {
		spans = append(spans, [2]int{m[0], m[1]})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	return spans
}

// toolSpan is one executable tool block in an agent reply: a fenced ```bash/
// ```sh block or a <tool_call>bash/sh/read block, with its byte span.
type toolSpan struct {
	sp   [2]int
	cmd  string // command text (bash/sh) or file path (read)
	kind string // "bash" | "read"
}

// toolBlockSpans returns every executable tool block in text, sorted by start
// position, mirroring exactly what applyAgenticToolsDeny would execute (cat >
// heredocs are builder writes, not commands — excluded).
func toolBlockSpans(text string) []toolSpan {
	var spans []toolSpan
	bashFenceRegex := regexp.MustCompile("(?s)(?:^|\\n)\\s*```(?:bash|sh)\\n(.*?)\\n\\s*```")
	for _, m := range bashFenceRegex.FindAllStringSubmatchIndex(text, -1) {
		cmd := strings.TrimSpace(text[m[2]:m[3]])
		if cmd == "" || strings.HasPrefix(cmd, "cat >") {
			continue
		}
		spans = append(spans, toolSpan{sp: [2]int{m[0], m[1]}, cmd: cmd, kind: "bash"})
	}
	for _, tc := range parseToolCallsWithSpans(text) {
		switch tc.name {
		case "bash", "sh":
			body := strings.TrimSpace(tc.body)
			if body == "" || strings.HasPrefix(body, "cat >") {
				continue
			}
			spans = append(spans, toolSpan{sp: tc.sp, cmd: body, kind: "bash"})
		case "read":
			if strings.TrimSpace(tc.body) == "" {
				continue
			}
			spans = append(spans, toolSpan{sp: tc.sp, cmd: strings.TrimSpace(tc.body), kind: "read"})
		case "ask":
			// Ask blocks are the clarify popover's payload, not a command —
			// never a tool card, never executed. The popover is their
			// execution (same rule as applyAgenticToolsDeny).
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].sp[0] < spans[j].sp[0] })
	return spans
}

// toolCall is a single parsed agentic tool request from an agent reply.
type toolCall struct {
	name string // "bash", "sh", or "read"
	body string // command text (bash/sh) or file path (read)
}

// toolCallSpan is a parsed <tool_call> plus its byte span in the ORIGINAL
// text — used by the streaming interleave to split tool blocks out of the
// prose at their exact position (a pass-2 stray-closed call's span is mapped
// back from the residual text through the segment map).
type toolCallSpan struct {
	name string
	body string
	sp   [2]int
}

// parseToolCallsWithSpans is parseToolCall plus each call's byte span in text.
func parseToolCallsWithSpans(text string) []toolCallSpan {
	var calls []toolCallSpan
	idxs := xmlToolProperRegex.FindAllStringSubmatchIndex(text, -1)

	// Pass 1 (proper </tool_call> closers): spans are the full match.
	for _, m := range idxs {
		if name, body := splitToolCall(strings.TrimSpace(text[m[2]:m[3]])); name != "" {
			calls = append(calls, toolCallSpan{name: name, body: body, sp: [2]int{m[0], m[1]}})
		}
	}

	// Build the residual (text minus proper spans) with a segment→original map
	// so pass-2 spans translate back to original byte offsets.
	var segs []residSeg
	var residual strings.Builder
	cursor := 0
	for _, m := range idxs {
		segs = append(segs, residSeg{cursor, m[0]})
		residual.WriteString(text[cursor:m[0]])
		cursor = m[1]
	}
	segs = append(segs, residSeg{cursor, len(text)})
	residual.WriteString(text[cursor:])

	// Pass 2: stray-closed calls in the residual, spans mapped to the original.
	res := residual.String()
	for _, m := range xmlToolLenientRegex.FindAllStringSubmatchIndex(res, -1) {
		name, body := splitToolCall(strings.TrimSpace(res[m[2]:m[3]]))
		if name == "" {
			continue
		}
		calls = append(calls, toolCallSpan{name: name, body: body, sp: [2]int{mapResidualToOrig(segs, m[0]), mapResidualToOrig(segs, m[1])}})
	}
	return calls
}

// residSeg is a byte range of the ORIGINAL text preserved in the residual.
type residSeg struct{ o0, o1 int }

// mapResidualToOrig translates a byte offset in the pass-2 residual string
// back to its offset in the original text.
func mapResidualToOrig(segs []residSeg, pos int) int {
	for _, s := range segs {
		length := s.o1 - s.o0
		if pos <= length {
			return s.o0 + pos
		}
		pos -= length
	}
	return pos
}

// xmlToolProperRegex matches <tool_call> blocks closed with a real </tool_call>.
// It deliberately does NOT accept stray closers: a proper arg-pair call contains
// an inner </arg_value> (e.g. <arg_value>main.go</arg_value>) which must never
// be mistaken for the block's closer.
var xmlToolProperRegex = regexp.MustCompile(`(?is)<tool_call>\s*(.*?)\s*</tool_call>`)

// xmlToolLenientRegex catches calls the model closed with a stray tag instead
// of </tool_call>: </bash>, </sh>, </value>, or </arg_value>. It runs on the
// residual text AFTER proper calls are consumed, so inner arg_value closers of
// already-parsed calls can't confuse it.
var xmlToolLenientRegex = regexp.MustCompile(`(?is)<tool_call>\s*(.*?)\s*</(?:bash|sh|value|arg_value)>`)

// argPairRegex extracts <arg_key>…</arg_key><arg_value>…</arg_value> pairs.
var argPairRegex = regexp.MustCompile(`(?s)<arg_key>([^<]*)</arg_key>\s*<arg_value>(.*?)</arg_value>`)

// strayCloserRegex strips trailing stray closing tags models leave in the body
// (e.g. the `</arg_value>` in `bash\nls -la …</arg_value></tool_call>`).
var strayCloserRegex = regexp.MustCompile(`(?is)\s*</(?:arg_value|value)>+\s*$`)

// isToolName reports whether s names a supported agentic tool.
func isToolName(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bash", "sh", "read", "search", "ask":
		return true
	}
	return false
}

// ---- ask tool -------------------------------------------------------------
//
// The agent asks the user for clarification (multi-question popover) by
// emitting an `ask` tool block. Shape:
//
//	<tool_call>ask
//	<ask_question header="Auth method">Select the auth method
//	- JWT
//	- Session cookies
//	- OAuth2
//	</ask_question>
//	<ask_question header="Scope" multi="true">Which areas to cover?
//	- Admin panel
//	- Public API
//	</ask_question>
//	</tool_call>
//
// Lines after the question text starting with "- " or "* " are options;
// `multi="true"` turns them into checkboxes (pick several). Each question
// also gets a free-text custom row in the popover.

// askBlockProperRegex matches <tool_call>ask … </tool_call> spans closed
// properly.
var askBlockProperRegex = regexp.MustCompile(`(?is)<tool_call>\s*ask\s*(.*?)</tool_call>`)

// askBlockLenientRegex catches ask blocks the model closed with a stray tag.
var askBlockLenientRegex = regexp.MustCompile(`(?is)<tool_call>\s*ask\s*(.*?)</(?:ask|bash|sh|value|arg_value)>`)

// askQuestionRegex matches one <ask_question …>…</ask_question> span.
var askQuestionRegex = regexp.MustCompile(`(?is)<ask_question\b([^>]*)>(.*?)</ask_question>`)

// attrValue extracts a quoted attribute (attr="…") from a tag's attribute
// string, or "" when absent.
func attrValue(attrs, name string) string {
	re := regexp.MustCompile(`(?i)` + name + `\s*=\s*["']([^"']*)["']`)
	if m := re.FindStringSubmatch(attrs); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// parseAskBlock extracts an ask request from an agent reply. Returns the
// questions (and whether any were found). The popover title is decided by the
// caller.
func parseAskBlock(text string) ([]askQuestion, bool) {
	var blocks []string
	for _, m := range askBlockProperRegex.FindAllStringSubmatch(text, -1) {
		blocks = append(blocks, m[1])
	}
	if len(blocks) == 0 {
		for _, m := range askBlockLenientRegex.FindAllStringSubmatch(text, -1) {
			blocks = append(blocks, m[1])
		}
	}
	var questions []askQuestion
	for _, b := range blocks {
		for _, qm := range askQuestionRegex.FindAllStringSubmatch(b, -1) {
			attrs, body := qm[1], qm[2]
			header := attrValue(attrs, "header")
			multi := strings.Contains(attrs, `multi="true"`) || strings.Contains(attrs, `multi='true'`)
			var qText []string
			var opts []string
			for _, raw := range strings.Split(strings.TrimSpace(body), "\n") {
				ln := strings.TrimSpace(raw)
				if ln == "" {
					continue
				}
				if strings.HasPrefix(ln, "- ") || strings.HasPrefix(ln, "* ") {
					opts = append(opts, strings.TrimSpace(ln[2:]))
				} else {
					qText = append(qText, ln)
				}
			}
			if len(qText) == 0 && len(opts) == 0 {
				continue // empty question block — ignore
			}
			questions = append(questions, askQuestion{
				header:      header,
				question:    strings.Join(qText, " "),
				options:     opts,
				multiSelect: multi,
			})
		}
	}
	return questions, len(questions) > 0
}

// stripAskBlock removes ask tool blocks from a reply so the remaining tool
// pipeline (and the display) never sees them again — the popover IS their
// execution.
func stripAskBlock(text string) string {
	t := askBlockProperRegex.ReplaceAllString(text, "")
	if t != text {
		return t
	}
	return askBlockLenientRegex.ReplaceAllString(t, "")
}

// allToolCommands returns every bash command string in an agent reply
// (fenced ```bash/```sh blocks + <tool_call>bash/sh), skipping cat > builder
// blocks and ask blocks. Used by the permission gate — the execution path
// (applyAgenticTools) re-parses independently.
func allToolCommands(text string) []string {
	var cmds []string
	bashRegex := regexp.MustCompile("(?s)(?:^|\\n)\\s*```(?:bash|sh)\\n(.*?)\\n\\s*```")
	for _, m := range bashRegex.FindAllStringSubmatch(text, -1) {
		cmdStr := strings.TrimSpace(m[1])
		if cmdStr == "" || strings.HasPrefix(cmdStr, "cat >") {
			continue
		}
		cmds = append(cmds, cmdStr)
	}
	for _, tc := range parseToolCall(text) {
		if tc.name != "bash" && tc.name != "sh" {
			continue
		}
		cmdStr := strings.TrimSpace(tc.body)
		if cmdStr == "" || strings.HasPrefix(cmdStr, "cat >") {
			continue
		}
		cmds = append(cmds, cmdStr)
	}
	return cmds
}

// toolSetSignature normalizes a reply's raw command set into a comparison key
// for the tool-loop repetition guard. Multi-command replies compare as a WHOLE
// (same set, same order) — a model that adds a command or reorders them is
// trying something new (progress), while re-emitting the identical set is the
// classic stuck loop (re-running the same failing command expecting a
// different result). The NUL separator is safe: shell commands never contain
// it.
func toolSetSignature(cmds []string) string {
	if len(cmds) == 0 {
		return ""
	}
	return strings.Join(cmds, "\x00")
}

// splitToolCall splits a <tool_call> body into a tool name and its payload,
// tolerating every malformed shape models actually emit:
//
//	read<arg_key>path</arg_key><arg_value>main.go</arg_value>   → read, main.go
//	bash\nls -la … (stray </arg_value> already stripped)         → bash, ls -la
//	read\nmain.go                                               → read, main.go
//	ls -la (no name at all)                                     → bash, ls -la
func splitToolCall(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}

	// Shape A: XML-ish arg_key/arg_value pair — the name is everything before
	// the first <arg_key>, the payload is the arg_value content.
	if m := argPairRegex.FindStringSubmatch(raw); len(m) == 3 {
		name := raw[:strings.Index(raw, "<arg_key")]
		return strings.TrimSpace(name), strings.TrimSpace(m[2])
	}

	// Shape B: first line is the tool name, the rest is the payload (after
	// cleaning any stray closer that leaked in from the outer match).
	first, rest, hasRest := strings.Cut(raw, "\n")
	first = strings.TrimSpace(first)
	if isToolName(first) {
		return strings.ToLower(first), strayCloserRegex.ReplaceAllString(strings.TrimSpace(rest), "")
	}
	if !hasRest {
		// Single-line "bash ls -la" or "read main.go".
		if name, body, ok := strings.Cut(first, " "); ok && isToolName(name) {
			return strings.ToLower(name), strings.TrimSpace(body)
		}
		// Unknown first token WITH arguments ("ls -la") — a bare command;
		// defensively run it as bash. A lone token ("kuma_context") is a
		// (possibly hallucinated) tool name — surface it as unsupported.
		if strings.Contains(first, " ") {
			return "bash", raw
		}
		return first, ""
	}

	// Multi-line with an unknown first token: the first line names the tool.
	return first, strings.TrimSpace(rest)
}

// parseToolCall extracts every <tool_call> block from an agent reply, returning
// the parsed name/body pairs. It is lenient on purpose: models frequently emit
// malformed tags (stray </arg_value>, </value>, or a </bash> closer), and a
// dropped call used to make the model retry the same broken request forever.
// Proper </tool_call>-closed calls are consumed first so an inner </arg_value>
// (legitimate arg-pair content) is never taken for the block's closer; the
// residual text is then scanned for stray-closed calls.
func parseToolCall(text string) []toolCall {
	var calls []toolCall

	// Pass 1: proper closers. Carve their spans out of the text as we go so
	// Pass 2 never re-parses their inner tags.
	idxs := xmlToolProperRegex.FindAllStringSubmatchIndex(text, -1)
	var residual strings.Builder
	cursor := 0
	for _, m := range idxs {
		residual.WriteString(text[cursor:m[0]])
		cursor = m[1]
		if name, body := splitToolCall(strings.TrimSpace(text[m[2]:m[3]])); name != "" {
			calls = append(calls, toolCall{name: name, body: body})
		}
	}
	residual.WriteString(text[cursor:])

	// Pass 2: stray-closed calls in the residual.
	for _, m := range xmlToolLenientRegex.FindAllStringSubmatch(residual.String(), -1) {
		if name, body := splitToolCall(strings.TrimSpace(m[1])); name != "" {
			calls = append(calls, toolCall{name: name, body: body})
		}
	}
	return calls
}

// toolBlockCommands returns one-line indicator strings for every bash/tool_call
// block in an agent reply WITHOUT executing them. It is the cheap pre-pass that
// lets the TUI show "⚙️ Running command: …" immediately, while applyAgenticTools
// actually executes the commands in a background goroutine. The bash indicator
// lines match applyAgenticTools' own logs exactly, so the renderer dedupes them
// when the execution finishes.
func toolBlockCommands(text string) []string {
	var cmds []string
	bashRegex := regexp.MustCompile("(?s)(?:^|\\n)\\s*```(?:bash|sh)\\n(.*?)\\n\\s*```")
	for _, m := range bashRegex.FindAllStringSubmatch(text, -1) {
		cmdStr := strings.TrimSpace(m[1])
		if cmdStr == "" || strings.HasPrefix(cmdStr, "cat >") {
			continue // cat > blocks are handled by the builder file writer
		}
		cmds = append(cmds, "⚙️  Running command: "+cmdStr)
	}
	for _, tc := range parseToolCall(text) {
		switch tc.name {
		case "bash", "sh":
			if tc.body == "" || strings.HasPrefix(tc.body, "cat >") {
				continue
			}
			cmds = append(cmds, "⚙️  Running command: "+tc.body)
		case "ask":
			// The ask block is handled by the interactive popover — not a
			// background tool execution — so it gets no indicator here.
		default:
			cmds = append(cmds, "⚙️  Running tool: "+tc.name)
		}
	}
	return cmds
}

// compactToolReply strips agentic tool-call blocks (fenced ```bash/```sh and
// <tool_call> XML) from a finished agent reply's display text. Those blocks
// were already executed — their indicators live in the trace and their results
// in ⚙ Tool Executed rows — so re-showing the raw command text made tool-heavy
// replies render as a giant wall of text (the poolside loop regression). Prose
// is preserved verbatim; a reply that contained nothing but tool calls
// collapses to a one-line summary so the agent still visibly did work.
func compactToolReply(text string) (string, int) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", 0
	}

	// The attribution footer is conversation metadata (the model-identity
	// chain parses it to detect switches) — pull it out FIRST so the strip
	// passes operate on the body only and can never double-append it.
	footer := attributionFooter(text)
	if footer != "" {
		text = strings.TrimSpace(strings.TrimSuffix(text, footer))
		if text == "" {
			return footer, 0 // footer-only reply — nothing to strip
		}
	}

	// Pass 1: properly-closed <tool_call>…</tool_call> spans. The lazy match
	// stops only at </tool_call>, so an inner arg_value closer can never
	// truncate the span (the parseToolCall two-pass lesson, applied to
	// display).
	proper := regexp.MustCompile(`(?is)<tool_call>.*?</tool_call>`)
	// Pass 2: stray-closed <tool_call>…</(bash|sh|value|arg_value|ask)> on the
	// residual text (models frequently emit these).
	lenient := regexp.MustCompile(`(?is)<tool_call>.*?</(?:bash|sh|value|arg_value|ask)>`)
	// Pass 3: fenced ```bash/```sh command blocks. A `cat >` heredoc is a
	// BUILDER file-write (applyBuilderCodeBlocks territory), not an executed
	// command — it must stay visible so the user can verify what the model
	// wrote (matching toolBlockCommands, which skips cat > blocks).
	fence := regexp.MustCompile("(?s)(?:^|\\n)\\s*```(?:bash|sh)\\n(.*?)\\n\\s*```")
	// Pass 4: an opener with no closer at all (```<tool_call>bash<```) — the
	// nastiest model output. Its line is garbage; eat the whole line.
	unterminated := regexp.MustCompile(`(?im)^[ \t]*<tool_call>\b[^\n]*$`)
	// Final pass: stray tool-tag fragments (a dangling `</arg_value>` with no
	// block around it). Only tool-call-specific tag names are stripped —
	// generic names like `<value>` or `<bash>` can legitimately appear in
	// prose (e.g. docs about XML/HTML) and must never be eaten.
	strayTag := regexp.MustCompile(`(?i)</?(?:tool_call|arg_key|arg_value|ask_question)\b[^>]*>`)

	// Count each pass against the residual it actually stripped from, so a
	// span can never be double-counted.
	count := len(proper.FindAllString(text, -1))
	residual := proper.ReplaceAllString(text, "")
	count += len(lenient.FindAllString(residual, -1))
	residual = lenient.ReplaceAllString(residual, "")

	// Fence pass keeps cat > heredocs: rebuild the reply from the non-fence
	// spans, and count only the command fences that were actually removed.
	fenceIdxs := fence.FindAllStringSubmatchIndex(residual, -1)
	var kept strings.Builder
	cur := 0
	fenceCount := 0
	for _, m := range fenceIdxs {
		if strings.HasPrefix(strings.TrimSpace(residual[m[2]:m[3]]), "cat >") {
			kept.WriteString(residual[cur:m[1]])
			cur = m[1]
			continue
		}
		kept.WriteString(residual[cur:m[0]])
		cur = m[1]
		fenceCount++
	}
	kept.WriteString(residual[cur:])
	count += fenceCount
	residual = kept.String()

	count += len(unterminated.FindAllString(residual, -1))
	cleaned := unterminated.ReplaceAllString(residual, "")
	cleaned = strayTag.ReplaceAllString(cleaned, "")

	cleaned = strings.TrimSpace(cleaned)
	// Collapse runs of 3+ blank lines (a stripped block leaves several) to a
	// single paragraph separator.
	for strings.Contains(cleaned, "\n\n\n\n") {
		cleaned = strings.ReplaceAll(cleaned, "\n\n\n\n", "\n\n")
	}
	if cleaned == "" && count > 0 {
		// Pure-tool reply: collapse to a one-line summary but keep the footer
		// so the turn that answered stays attributable to its model.
		return fmt.Sprintf("⚙️  %d tool command(s) executed — see trace above%s", count, footer), count
	}
	if footer != "" {
		cleaned = strings.TrimRight(cleaned, "\n") + footer
	}
	return cleaned, count
}

// toolEchoMarkerRegex matches a line that opens an echoed tool-result block:
// weak models repeat the exact "[SYSTEM TOOL RESULT]:" / "[SYSTEM ASK RESULT]:"
// payload brocode fed them back in their reply text, or a bare "Output:" /
// "Tool output:" label followed by the payload. The optional trailing colon
// is tolerated (both forms appear in the wild). A bare label only folds when
// a real payload follows (indented/fenced lines) — a lone "Output:" with
// prose after it stays prose.
var toolEchoMarkerRegex = regexp.MustCompile(`(?i)^\s*(?:\[SYSTEM (?:TOOL|ASK) RESULT\]|(?:tool|command|shell) output|output)\s*:?\s*$`)

// extractToolEcho splits a finished agent reply into prose and any tool-result
// blocks the model echoed verbatim. Echoed blocks are pure transcript noise
// (the payload already lives in the ⚙ Tool Executed rows and the next prompt's
// context) — they render as a dim collapsible row, never as white prose.
//
// A block is: the marker line + everything after it that is blank, indented
// (2+ spaces — echoed payloads are indented under the marker), or inside a
// fenced block. The first unindented non-blank non-fence line ends the block
// (that is prose). A lone marker with no payload is left as prose.
func extractToolEcho(text string) (prose string, echo string, count int) {
	lines := strings.Split(text, "\n")
	var proseL, echoL []string
	i := 0
	for i < len(lines) {
		ln := lines[i]
		if !toolEchoMarkerRegex.MatchString(ln) {
			proseL = append(proseL, ln)
			i++
			continue
		}
		// Marker found — collect the echoed payload that follows.
		block := []string{ln}
		i++
		inFence := false
		hadContent := false
		for i < len(lines) {
			cur := lines[i]
			trim := strings.TrimSpace(cur)
			if strings.HasPrefix(trim, "```") {
				inFence = !inFence
				block = append(block, cur)
				hadContent = true
				i++
				continue
			}
			// An attribution footer line ("⚡ provider/model · time · tokens") is
			// indented like echo payload but is brocode's OWN metadata — the
			// model-identity chain parses it from the reply text to detect
			// mid-session switches. It must stay in prose, never fold into the
			// echoed block (the echo often sits directly above the footer).
			if strings.HasPrefix(trim, "⚡") && strings.Contains(trim, " · ") {
				break
			}
			if inFence || trim == "" || strings.HasPrefix(cur, " ") || strings.HasPrefix(cur, "\t") {
				if trim != "" {
					hadContent = true
				}
				block = append(block, cur)
				i++
				continue
			}
			break // prose resumes — the echoed block ends here
		}
		if hadContent {
			echoL = append(echoL, block...)
			count++
		} else {
			proseL = append(proseL, block...)
		}
	}
	return strings.TrimRight(strings.Join(proseL, "\n"), " \n"), strings.TrimRight(strings.Join(echoL, "\n"), " \n"), count
}

// attributionFooter extracts the trailing "⚡ provider/model · time · tokens"
// line brocode appends to every reply, or "" when absent. Kept separate from
// stripAttribution (which REMOVES it) so display compaction can re-attach it.
func attributionFooter(s string) string {
	idx := strings.LastIndex(s, "\n\n  ⚡ ")
	if idx < 0 {
		return ""
	}
	footer := strings.TrimSpace(s[idx:])
	if footer == "" {
		return ""
	}
	return "\n\n  " + footer
}

// applyAgenticTools inspects the LLM output for tool execution requests.
// It supports both Markdown bash blocks (```bash ... ```) and XML `<tool_call>` tags.
// Returns (traceLogs, feedbackText) where feedbackText is sent back to the LLM for the next turn.
func applyAgenticTools(text string, plannerMode bool) ([]string, string) {
	return applyAgenticToolsDeny(text, nil, plannerMode, nil)
}

type pendingToolTask struct {
	log string
	run func() string
}

// applyAgenticToolsDeny is applyAgenticTools with a permission deny-list: any
// command whose agentic.AllowKey is in deny is logged as "user denied" and
// fed back to the model instead of executed (the native permission gate's
// "deny" decision). Tool calls are executed with Parallel Fan-Out Goroutines.
func applyAgenticToolsDeny(text string, deny map[string]bool, plannerMode bool, exp *exploreConfig) ([]string, string) {
	var tasks []pendingToolTask
	var logs []string

	skipDenied := func(cmdStr string) bool {
		if deny == nil {
			return false
		}
		key := agentic.AllowKey(cmdStr)
		if key == "" || !deny[key] {
			return false
		}
		tasks = append(tasks, pendingToolTask{
			log: "⛔ User denied: " + cmdStr,
			run: func() string {
				return "User denied command `" + cmdStr + "`. Do not run it again without asking.\n"
			},
		})
		return true
	}

	// Pattern 1: Markdown bash blocks
	bashRegex := regexp.MustCompile("(?s)(?:^|\\n)\\s*```(?:bash|sh)\\n(.*?)\\n\\s*```")
	for _, m := range bashRegex.FindAllStringSubmatch(text, -1) {
		cmdStr := strings.TrimSpace(m[1])
		if cmdStr == "" || strings.HasPrefix(cmdStr, "cat >") {
			continue // Handled by builder file writer
		}
		if skipDenied(cmdStr) {
			continue
		}

		cmdTarget := cmdStr
		if plannerMode {
			tasks = append(tasks, pendingToolTask{
				log: fmt.Sprintf("⛔ Planner Mode: command '%s' blocked", clip(cmdTarget, 30)),
				run: func() string {
					return fmt.Sprintf("Planner Mode Active: shell command `%s` is disabled. Press Shift+Tab to switch to Builder Mode to run commands.\n", cmdTarget)
				},
			})
			continue
		}
		tasks = append(tasks, pendingToolTask{
			log: fmt.Sprintf("⚙️  Running command: %s", cmdTarget),
			run: func() string {
				out, err := agentic.RunCommandNative(cmdTarget, agentic.ToolOptions{Timeout: 30 * time.Second})
				var sb strings.Builder
				sb.WriteString(fmt.Sprintf("Result of `%s`:\n```\n%s\n```\n", cmdTarget, out))
				if err != nil {
					sb.WriteString(fmt.Sprintf("Error: %v\n", err))
				}
				return sb.String()
			},
		})
	}

	// Pattern 2: XML <tool_call> tags
	for _, tc := range parseToolCall(text) {
		switch tc.name {
		case "bash", "sh":
			cmdStr := strings.TrimSpace(tc.body)
			if cmdStr == "" || strings.HasPrefix(cmdStr, "cat >") {
				continue
			}
			if skipDenied(cmdStr) {
				continue
			}
			cmdTarget := cmdStr
			if plannerMode {
				tasks = append(tasks, pendingToolTask{
					log: fmt.Sprintf("⛔ Planner Mode: command '%s' blocked", clip(cmdTarget, 30)),
					run: func() string {
						return fmt.Sprintf("Planner Mode Active: shell command `%s` is disabled. Press Shift+Tab to switch to Builder Mode to run commands.\n", cmdTarget)
					},
				})
				continue
			}
			tasks = append(tasks, pendingToolTask{
				log: fmt.Sprintf("⚙️  Running command: %s", cmdTarget),
				run: func() string {
					out, err := agentic.RunCommandNative(cmdTarget, agentic.ToolOptions{Timeout: 30 * time.Second})
					var sb strings.Builder
					sb.WriteString(fmt.Sprintf("Result of `%s`:\n```\n%s\n```\n", cmdTarget, out))
					if err != nil {
						sb.WriteString(fmt.Sprintf("Error: %v\n", err))
					}
					return sb.String()
				},
			})
		case "read":
			path := strings.TrimSpace(tc.body)
			if path == "" {
				continue
			}
			filePath := path
			tasks = append(tasks, pendingToolTask{
				log: fmt.Sprintf("📖 Reading file: %s", filePath),
				run: func() string {
					if data, err := os.ReadFile(filePath); err == nil {
						return fmt.Sprintf("Content of %s:\n```\n%s\n```\n", filePath, agentic.CapOutput(string(data)))
					} else {
						return fmt.Sprintf("Failed to read %s: %v\n", filePath, err)
					}
				},
			})
		case "search":
			q := strings.TrimSpace(tc.body)
			queryStr := q
			tasks = append(tasks, pendingToolTask{
				log: fmt.Sprintf("🔎  Workspace search: %s", queryStr),
				run: func() string {
					if queryStr == "" {
						return "Search tool: empty query — pass the terms after the tool name.\n"
					}
					results := fileSearch(queryStr, fileSearchTopK)
					if len(results) == 0 {
						return fmt.Sprintf("Workspace search `%s`: no matching files in the index.\n", queryStr)
					}
					var sb strings.Builder
					sb.WriteString(fmt.Sprintf("Workspace search `%s` — top %d file match(es):\n", queryStr, len(results)))
					for _, r := range results {
						sn := r.Snippet
						if sn == "" {
							sn = "(no preview)"
						}
						sb.WriteString(fmt.Sprintf("  • %s (relevance %.1f) — %s\n", r.ID, r.Score, sn))
					}
					sb.WriteString("Read a match with `cat` before editing it.\n")
					return sb.String()
				},
			})
		case "ask":
			// Handled by parseAskBlock elsewhere, ignore here to prevent false error
			continue
		case "explore":
			// Delegated read-only research (P8): spawn the nested explore loop
			// in the background; its condensed report is fed back to the main
			// agent as a [SYSTEM TOOL RESULT] it then reasons over.
			q := strings.TrimSpace(tc.body)
			if q == "" {
				continue
			}
			exp := exp // capture per-task (parameter is stable, but be explicit)
			tasks = append(tasks, pendingToolTask{
				log: fmt.Sprintf("● explore → researching: %s", clip(q, 48)),
				run: func() string {
					report, calls, err := runExploreLoop(exp, q)
					var sb strings.Builder
					if calls != 1 {
						sb.WriteString(fmt.Sprintf("Explore subagent report (%d rounds):\n%s\n", calls, report))
					} else {
						sb.WriteString(fmt.Sprintf("Explore subagent report (%d round):\n%s\n", calls, report))
					}
					if err != nil {
						sb.WriteString(fmt.Sprintf("\nExplore error: %v\n", err))
					}
					sb.WriteString("\nSynthesize this report into your answer. Continue investigating with `search`/`read` only if the report left gaps.\n")
					return sb.String()
				},
			})
		default:
			badName := tc.name
			badBody := tc.body
			tasks = append(tasks, pendingToolTask{
				log: fmt.Sprintf("⚠️  Unsupported tool call: %s", badName),
				run: func() string {
					return fmt.Sprintf(
						"Error: unsupported tool '%s'. BroCode only supports: bash/sh (shell commands), read (files), and search (workspace files). "+
							"Do NOT wrap commands in XML — output a ```bash\n<command>\n``` block instead. "+
							"Received: %s\n",
						badName, clip(badBody, 120))
				},
			})
		}
	}

	if len(tasks) == 0 {
		return nil, ""
	}

	// PARALLEL FAN-OUT GOROUTINE EXECUTION
	// Parallelize read/search/bash tool execution across goroutines, preserving order
	feedbacks := make([]string, len(tasks))
	var wg sync.WaitGroup

	for i, task := range tasks {
		logs = append(logs, task.log)
		wg.Add(1)
		go func(idx int, t pendingToolTask) {
			defer wg.Done()
			feedbacks[idx] = t.run()
		}(i, task)
	}

	wg.Wait()

	var feedbackSb strings.Builder
	for _, fb := range feedbacks {
		feedbackSb.WriteString(fb)
	}

	return logs, feedbackSb.String()
}
