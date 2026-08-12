package tui

import (
	"fmt"
	"strings"

	"github.com/plumpslabs/bro-code/internal/diff"
	"github.com/plumpslabs/bro-code/internal/search"
)

// activityItem is one row in the tool-activity sidebar.
type activityItem struct {
	tool   string // tool name: search, diff, memory, system
	label  string // human summary, e.g. search("mcp")
	status string // running | done | error
	detail string // result detail, e.g. "3 results · 11ms"
}

// collapse is a hidden block attached to a reply (diff hunk, thinking trace).
// Hidden by default, revealed with ctrl+o — bounded rendering, Principle 1.
type collapse struct {
	summary string // one-line label shown while collapsed
	content string // full block shown while expanded
}

// mockReply is the deterministic output of the mock agent for one input.
type mockReply struct {
	text      string
	items     []activityItem
	collapse  *collapse
	subagents []subagentState
}

// buildReply turns a user input into a mock agent reply. It exercises the
// real pipeline (BM25 search + Myers diff) so the mock doubles as a smoke
// test for the core — no LLM involved yet.
func buildReply(input string, ix *search.Index) mockReply {
	input = strings.TrimSpace(input)
	switch {
	case input == "/help":
		return mockReply{text: helpText}
	case input == "/agents":
		return agentsReply()
	case input == "/info":
		return infoReply()
	case input == "/mcp":
		return mcpReply()
	case input == "/usage":
		return usageReply()
	case input == "/tools":
		return toolsReply(ix)
	case input == "/diff":
		return diffReply()
	case input == "/memory":
		return memoryReply()
	case input == "/search":
		// Bare /search is the one command that wants more input — say so
		// instead of falling into the unknown-command error.
		return mockReply{
			text: "Search what? Try: /search mcp, /search diff, /search memory.",
			items: []activityItem{{
				tool: "search", label: "search(\"\")",
				status: "error", detail: "empty query",
			}},
		}
	case strings.HasPrefix(input, "/search "):
		return searchReply(strings.TrimSpace(strings.TrimPrefix(input, "/search ")), ix)
	case strings.HasPrefix(input, "/"):
		return mockReply{
			text: "Unknown command: " + input + ".\nType /help for the command list.",
			items: []activityItem{{
				tool: "system", label: "parse command",
				status: "error", detail: "unknown command",
			}},
		}
	default:
		return searchReply(input, ix)
	}
}

// conversationalReply builds a mock reply that REFERENCES the prior
// conversation instead of treating every prompt as a fresh session — a
// follow-up like "tadi bahasa apa?" must not be answered as if the chat
// started now (the exact complaint that motivated session/context work). It
// detects follow-up phrases (Indonesian + English), recalls the last user
// question and agent answer from the transcript, answers language questions
// honestly, and falls back to the BM25 search path for genuinely new prompts.
func conversationalReply(q string, ix *search.Index, chat []chatMsg) mockReply {
	lower := strings.ToLower(q)
	if !isFollowUp(lower) {
		return searchReply(q, ix)
	}
	lastUser, lastAgent := recall(chat, q)
	return followUpReply(q, lastUser, lastAgent, ix)
}

// recall returns the last user question and agent answer from the transcript,
// skipping the current prompt (the final chat entry, appended by send()).
// Returns "" for either side when there is no prior turn to reference.
func recall(chat []chatMsg, q string) (lastUser, lastAgent string) {
	for i := len(chat) - 2; i >= 0; i-- {
		switch chat[i].role {
		case roleUser:
			if lastUser == "" && chat[i].text != q {
				lastUser = clip(chat[i].text, 80)
			}
		case roleAgent:
			if lastAgent == "" && strings.TrimSpace(chat[i].text) != "" {
				// stripAttribution removes the "⚡ …" footer from the transcript.
				lastAgent = clip(firstLine(stripAttribution(chat[i].text)), 140)
			}
		}
		if lastUser != "" && lastAgent != "" {
			return lastUser, lastAgent
		}
	}
	return lastUser, lastAgent
}

// isFollowUp reports whether the prompt reads as a conversational follow-up
// ("tadi bahasa apa?", "lanjutkan", "jelaskan lagi") rather than a fresh query.
func isFollowUp(s string) bool {
	for _, f := range []string{
		"tadi", "sebelumnya", "yang tadi", "itu tadi", "di atas", "lanjut",
		"ulangi", "jelaskan lagi", "bahasa apa", "apa bahasa", "tadi pakai",
		"kamu tadi", "tadi jawab", "tadi bilang", "masih ingat",
		"previous", "earlier", "last time", "continue", "again", "as before",
		"what language", "which language", "you said", "you mentioned",
	} {
		if strings.Contains(s, f) {
			return true
		}
	}
	return false
}

// followUpReply answers a conversational follow-up by pointing back at the
// recalled prior turns — the session-memory demo. Falls back to a local index
// search for the actual substance so the reply stays useful.
func followUpReply(q, lastUser, lastAgent string, ix *search.Index) mockReply {
	var sb strings.Builder
	sb.WriteString("🧠 **Session resumed** — context from previous turns is preserved:\n\n")
	if lastUser != "" {
		sb.WriteString("  • Previous query: " + lastUser + "\n")
	}
	if lastAgent != "" {
		sb.WriteString("  • Previous response: " + lastAgent + "\n")
	}
	lower := strings.ToLower(q)
	if strings.Contains(lower, "bahasa") || strings.Contains(lower, "language") {
		sb.WriteString("\nLanguage context: **" + languageOf(lastUser, lastAgent) + "** — conversation history is maintained.\n")
	}
	sb.WriteString("\nFor your current request, from local index:\n")
	results := ix.Search(q, 3)
	if len(results) > 0 {
		for _, r := range results {
			sb.WriteString(fmt.Sprintf("  • %s — %s\n", r.Title, r.ID))
		}
	} else {
		sb.WriteString("  (no matching entries found — try another keyword or /search)\n")
	}
	return mockReply{
		text: strings.TrimRight(sb.String(), "\n"),
		items: []activityItem{{
			tool: "memory", label: "recall(session)",
			status: "done", detail: "prior turns referenced",
		}},
		collapse: thinkingTrace(ix),
	}
}

// languageOf guesses the dominant language of the conversation — Indonesian
// vs English — by counting function words. An honest heuristic for the mock;
// the real detector comes with the LLM layer.
func languageOf(texts ...string) string {
	id := 0
	en := 0
	idWords := []string{" yang ", " di ", " ke ", " dari ", " untuk ", " saya ", " kamu ", " ini ", " itu ", " apa ", " dengan ", " pada ", " tidak ", " akan ", " adalah ", " sudah ", " bisa ", " jangan ", " cara "}
	enWords := []string{" the ", " and ", " is ", " are ", " of ", " to ", " in ", " for ", " with ", " this ", " that ", " what ", " how ", " you ", " i ", " it "}
	for _, t := range texts {
		lower := " " + strings.ToLower(t) + " "
		for _, w := range idWords {
			if strings.Contains(lower, w) {
				id++
			}
		}
		for _, w := range enWords {
			if strings.Contains(lower, w) {
				en++
			}
		}
	}
	switch {
	case id > en:
		return "Indonesia"
	case en > id:
		return "English"
	default:
		return "campuran (mixed)"
	}
}

func searchReply(q string, ix *search.Index) mockReply {
	results := ix.Search(q, 5)
	items := []activityItem{{
		tool: "search", label: "search(" + quote(q) + ")",
		status: "done", detail: fmt.Sprintf("%d results · %dms", len(results), 3+len(q)%13),
	}}
	if len(results) == 0 {
		return mockReply{
			text:     "No tools or skills matched " + quote(q) + " in the local index.\n\nTry: mcp, diff, edit, memory, search, testing, performance.",
			items:    items,
			collapse: thinkingTrace(ix),
		}
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d match(es) for %s:\n", len(results), quote(q)))
	for _, r := range results {
		sb.WriteString(fmt.Sprintf("  • %s — %s\n", r.Title, r.ID))
	}
	sb.WriteString("\nAsk me to use one — e.g. /diff to see a Myers diff in action.")
	return mockReply{text: strings.TrimRight(sb.String(), "\n"), items: items, collapse: thinkingTrace(ix)}
}

// thinkingTrace is the mock agent's reasoning line — collapsed by default so
// the UI never shows noise, expanded with ctrl+o. Real traces come with the
// LLM layer; this exercises the same collapsible mechanism.
func thinkingTrace(ix *search.Index) *collapse {
	return &collapse{
		summary: "thinking — ctrl+o to expand",
		content: fmt.Sprintf("plan → search index (%d tools) → tokenize → BM25 rank → top-k → format reply", ix.Len()),
	}
}

func diffReply() mockReply {
	return mockReply{
		text: "Here is a sample Myers diff (git-style unified hunk) of main.go — green = added, red = removed. Myers is O(ND), the algorithm git uses, far cheaper than Levenshtein O(n×m).",
		items: []activityItem{{
			tool: "diff", label: "diff(main.go)",
			status: "done", detail: "1 hunk · Myers O(ND)",
		}},
		collapse: &collapse{
			summary: "diff · main.go — 1 hunk · ctrl+o to expand",
			content: indent(diff.Sample()),
		},
	}
}

func memoryReply() mockReply {
	return mockReply{
		text: "Session memory (planned, Principle 5):\n" +
			"  • store: SQLite via modernc.org/sqlite (CGO-free)\n" +
			"  • shape: JSONL, TTL-based retention\n" +
			"  • budget: a few rows per turn — never a hot path\n" +
			"  • offload: compaction tiers (L0–L3) keep this bounded",
		items: []activityItem{{
			tool: "memory", label: "memory(session)",
			status: "done", detail: "TTL + retention",
		}},
	}
}

func infoReply() mockReply {
	return mockReply{
		text: "📊 **Session Info**\n\n" +
			"• Provider: opencode (zen gateway)\n" +
			"• Model: deepseek-v4-flash-free\n" +
			"• Context window: ~131k tokens\n" +
			"• Session: brocode (abc12345)\n" +
			"• Git: main @ /path/to/project\n" +
			"• MCP: filter armed (lazy)\n" +
			"• Sub-agents: idle (0 active)\n" +
			"• Activities: 3 recent",
		items: []activityItem{
			{tool: "system", label: "session info", status: "done", detail: "full status"},
		},
	}
}

func agentsReply() mockReply {
	return mockReply{
		text: "Primary agent: brocode (owns goal & task execution).\n\n" +
			"Available Native Capabilities:\n" +
			"  • Fast Path → Quick direct edits\n" +
			"  • Deep Path → Complex analysis & Risk L0-L3 Engine\n\n" +
			"Subtasks run natively in background contexts.",
		items: []activityItem{
			{tool: "spawn", label: "(spawn @finder)", status: "spawn_ok", detail: "symbol index"},
			{tool: "spawn", label: "(spawn @reviewer)", status: "spawn_ok", detail: "L2 review pass"},
			{tool: "bash", label: "(bash go test ./...)", status: "bash_ok", detail: "51 passed"},
		},
		subagents: []subagentState{
			{name: "finder", task: "locate symbols", status: "done"},
			{name: "reviewer", task: "L2 review pass", status: "done"},
		},
	}
}

func mcpReply() mockReply {
	return mockReply{
		text: "MCP servers (mock — none connected yet):\n  • filter layer armed: catalog → inspect → execute (Principle 2)\n  • tools/list will be cached per TTL; schemas injected only on call\n  • connected servers will appear here when the MCP layer lands",
		items: []activityItem{{
			tool: "mcp", label: "mcp.status",
			status: "done", detail: "0 connected · filter armed",
		}},
	}
}

func usageReply() mockReply {
	return mockReply{
		text: "Usage & context (forecast marked ~ — real settlement numbers come from the API, Principle 3):\n  • context: live in the header + right panel (used vs provider window, percent color-coded)\n  • compaction tiers: L0 pinned (goal/constraints) · L1 verbatim tail · L2 ledger · L3 offload\n  • trigger: auto-compact at 70% of window — preventive, never wait for the cliff\n  • session memory: follow-ups recall prior turns; -c resume is compaction-safe\n  • idle RSS ~5MB · binary 8.7MB",
		items: []activityItem{{
			tool: "usage", label: "usage()",
			status: "done", detail: "est. tokens · 5MB RSS",
		}},
	}
}

func toolsReply(ix *search.Index) mockReply {
	var sb strings.Builder
	sb.WriteString("Tools & skills in the local index:\n")
	for _, d := range ix.Docs() {
		sb.WriteString(fmt.Sprintf("  • %s — %s\n", d.Title, d.ID))
	}
	return mockReply{
		text: strings.TrimRight(sb.String(), "\n"),
		items: []activityItem{{
			tool: "system", label: "list tools",
			status: "done", detail: fmt.Sprintf("%d available", ix.Len()),
		}},
	}
}

const helpText = `Commands:
  /connect         connect an LLM provider (UI)
  /search <query>  search tools & skills (BM25)
  /diff            Myers diff demo (hunk collapsed — ctrl+o to expand)
  /agents          primary agent + lazy subagents
  /mcp             MCP server status (filter layer)
  /usage           usage + compaction tiers
  /memory          session memory plan
  /tools           list indexed tools & skills
  /theme           open the theme picker (or /theme <name> to set)
  /clear           start fresh (back to the landing)
  /help            this help
  /quit            quit

Keys:
  enter      send · accept suggestion
  ↑↓ / wheel scroll the chat (no focus needed) · pgup/pgdown page
  tab        accept suggestion (while the popup is open)
  ctrl+o     expand/collapse the last block (diff hunk, thinking trace)
  ?          this help
  q/ctrl+c   quit (q only when the input is empty)

While the agent works, its live steps (grep/read/bash/subagent) stream
into the chat as a dimmed process log. When the agent asks you a question,
↑↓ chooses an option, type a custom answer, enter submits, esc cancels.

The right panel is a live status dashboard: model + context window + token
usage (estimate until the provider layer lands), git branch + path, MCP
servers, and sub-agents. Typing a command shows a suggestion popup that
filters as you type — try "/" now. Start a new conversation with brocode;
resume your last one with brocode -c. Any other input runs the BM25 search
pipeline — the same code path the real agent will use.`

func quote(s string) string { return "\"" + s + "\"" }

// indent prefixes every line of s with two spaces so multi-line output
// (diffs, lists) reads cleanly inside a chat message.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n")
}
