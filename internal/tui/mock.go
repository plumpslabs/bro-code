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

func agentsReply() mockReply {
	return mockReply{
		text: "Primary agent: brocode (planner — owns goal & task execution).\n\n" +
			"Spawned Subagents:\n" +
			"  • (spawn @finder) → locate codebase symbols & dependency tree [DONE]\n" +
			"  • (spawn @reviewer) → L0-L3 security & architecture review [DONE]\n" +
			"  • (spawn @debugger) → isolate runtime errors & trace hypotheses [IDLE]\n" +
			"  • (spawn @cleaner) → remove dead code & temp files [IDLE]\n\n" +
			"Subagents run in isolated context windows and report back to main agent upon completion.",
		items: []activityItem{
			{tool: "spawn", label: "(spawn @finder)", status: "spawn_ok", detail: "symbol index"},
			{tool: "spawn", label: "(spawn @reviewer)", status: "spawn_ok", detail: "L2 review pass"},
			{tool: "bash", label: "(bash go test ./...)", status: "bash_ok", detail: "47 passed"},
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
		text: "Usage & context (estimates — real settlement numbers come from the API, Principle 3):\n  • context: live in the header + right panel (used vs provider window)\n  • compaction tiers: L0 pinned (goal/constraints) · L1 verbatim tail · L2 ledger · L3 offload\n  • doctrine: avoid compaction first; aggressive compaction is the bomb (Governance Decay)\n  • idle RSS ~5MB · binary 4.5MB",
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
