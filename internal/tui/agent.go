// agent.go — Provider execution layer: runs the active LLM provider for one
// prompt, handles SSE streaming, CLI wrappers, and the mock fallback pipeline.
package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/agentic"

	tea "charm.land/bubbletea/v2"
)

// sendPhase safely pushes a status/trace update to the TUI.
// Non-blocking if the channel is full (the run continues regardless).
func sendPhase(ch chan<- agentTraceMsg, phase, line string) {
	if ch == nil {
		return
	}
	select {
	case ch <- agentTraceMsg{phase: phase, line: line}:
	default:
	}
}

// ── Unified Phase Workflow ──────────────────────────────────────────────
// All providers follow the same phase progression for consistent UX:
//
//	thinking… → [processing… →] receiving response… → done
//
// Streaming providers (OpenCode) add intermediate phases:
//
//	thinking… → reasoning… → writing response… → done
//
// The providerWorkflow struct encapsulates the common pattern.
type providerWorkflow struct {
	ch        chan<- agentTraceMsg
	provider  string
	model     string
	startTime time.Time
}

// newWorkflow creates a workflow helper for a provider.
func newWorkflow(ch chan<- agentTraceMsg, provider, model string, startTime time.Time) *providerWorkflow {
	return &providerWorkflow{ch: ch, provider: provider, model: model, startTime: startTime}
}

// phaseThinking sends the initial thinking phase (first thing every provider does).
func (w *providerWorkflow) phaseThinking() {
	sendPhase(w.ch, "thinking…", "→ "+w.provider+" · "+w.model)
}

// phaseProcessing sends the processing/connecting phase.
func (w *providerWorkflow) phaseProcessing() {
	sendPhase(w.ch, "processing…", "→ connecting to "+w.provider+" API")
}

// phaseReceiving sends the receiving response phase.
func (w *providerWorkflow) phaseReceiving() {
	sendPhase(w.ch, "receiving response…", "→ waiting for model reply")
}

// phaseDone sends the final done phase with elapsed time.
func (w *providerWorkflow) phaseDone(elapsed time.Duration, detail string) {
	sendPhase(w.ch, "done", "→ "+detail+" · "+fmt.Sprintf("%.1fs", elapsed.Seconds()))
}

// phaseError sends an error phase.
func (w *providerWorkflow) phaseError(errMsg string) {
	sendPhase(w.ch, "error", "→ "+errMsg)
}

// appendTrace appends one dimmed process line to the in-chat log, bounded.
func appendTrace(trace []string, line string) []string {
	trace = append(trace, line)
	if len(trace) > maxTrace {
		trace = trace[len(trace)-maxTrace:]
	}
	return trace
}

// detectPhase infers agent activity phase from an output line.
// Matches both generic keywords AND opencode-specific output format.
func detectPhase(line string) string {
	lower := strings.ToLower(line)
	trimmed := strings.TrimSpace(line)

	// ── opencode-specific patterns (→Read, ✱Glob, + Thought, etc.) ──
	if strings.HasPrefix(trimmed, "→Read") || strings.HasPrefix(trimmed, "→read") {
		return "reading files…"
	}
	if strings.HasPrefix(trimmed, "✱Glob") || strings.HasPrefix(trimmed, "✱glob") {
		return "searching files…"
	}
	if strings.HasPrefix(trimmed, "+ Thought") || strings.HasPrefix(trimmed, "+ thought") {
		return "thinking…"
	}
	if strings.HasPrefix(trimmed, "→Edit") || strings.HasPrefix(trimmed, "→Write") || strings.HasPrefix(trimmed, "→edit") || strings.HasPrefix(trimmed, "→write") {
		return "writing code…"
	}
	if strings.HasPrefix(trimmed, "❯ Bash") || strings.HasPrefix(trimmed, "❯ bash") || strings.HasPrefix(trimmed, "→Bash") {
		return "running command…"
	}
	if strings.HasPrefix(trimmed, "→MCP") || strings.HasPrefix(trimmed, "→mcp") {
		return "using tool…"
	}

	// ── Generic patterns (fallback for other providers) ──
	switch {
	case strings.Contains(lower, "reading") || strings.Contains(lower, "scanning") || strings.Contains(lower, "searching"):
		return "reading files…"
	case strings.Contains(lower, "thinking") || strings.Contains(lower, "reasoning"):
		return "thinking…"
	case strings.Contains(lower, "writing") || strings.Contains(lower, "editing") || strings.Contains(lower, "creating"):
		return "writing code…"
	case strings.Contains(lower, "running") || strings.Contains(lower, "executing") || strings.Contains(lower, "command"):
		return "running command…"
	case strings.Contains(lower, "tool") || strings.Contains(lower, "function_call") || strings.Contains(lower, "tool_use"):
		return "using tool…"
	case strings.Contains(lower, "bash") || strings.Contains(lower, "shell"):
		return "running shell…"
	default:
		return ""
	}
}

// zenEndpoint is the OpenCode Zen gateway — the OpenAI-compatible chat
// completions endpoint the opencode CLI routes its free "opencode/" models
// through. Free models are served WITHOUT an API key (verified empirically
// Aug 2026: zero credentials on the machine, direct curl succeeds).
const zenEndpoint = "https://opencode.ai/zen/v1/chat/completions"

// stripAttribution removes the "⚡ provider/model · time · tokens" footer that
// brocode appends to every reply, so it never pollutes the model's context on
// later turns.
func stripAttribution(s string) string {
	if idx := strings.LastIndex(s, "\n\n  ⚡ "); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}

// attributionFooterRe extracts the "provider/model" pair from the reply footer
// brocode appends to every answer (e.g. "⚡ opencode/deepseek-v4-flash-free ·
// 3.2s · 133 tokens"). Used to detect a mid-session model switch: the chat
// history records which model actually answered each prior turn.
var attributionFooterRe = regexp.MustCompile(`⚡\s+([^/\s]+)/(\S+?)\s+·`)

// lastAttributionModel scans the chat for the MOST RECENT agent reply's
// attribution footer and returns the provider/model that answered it. Empty
// strings when no agent reply exists yet (fresh session or /clear).
func lastAttributionModel(chat []chatMsg) (string, string) {
	for i := len(chat) - 1; i >= 0; i-- {
		cm := chat[i]
		if cm.role != roleAgent {
			continue
		}
		text := cm.text
		if text == "" {
			text = cm.content
		}
		// Take the LAST footer match in the message, not the first — a model
		// reply could quote a footer in prose, and the real footer brocode
		// appends is always at the very end.
		if all := attributionFooterRe.FindAllStringSubmatch(text, -1); len(all) > 0 {
			m := all[len(all)-1]
			return m[1], m[2]
		}
	}
	return "", ""
}

// modelIdentityNote tells the active model WHO it is and — critically — when
// the user switched provider/model mid-session, that prior turns were answered
// by a DIFFERENT model. A freshly switched model inherits the whole chat
// history but has no way to know it was swapped in; without this note it may
// misread earlier turns as its own output, copy the prior model's (possibly
// wrong) tool format, or lose track of what changed. The note is injected into
// the user prompt metadata — never the system prompt — so switching models
// does not invalidate the system-prefix cache.
func modelIdentityNote(chat []chatMsg, provider, model string, window int) string {
	note := fmt.Sprintf("Active model: %s/%s", provider, model)
	if window > 0 {
		note += fmt.Sprintf(" | Context window: %s tokens", fmtTokens(window))
	}
	prevP, prevM := lastAttributionModel(chat)
	if prevP != "" && (prevP != provider || prevM != model) {
		note += fmt.Sprintf(" | NOTE: active model switched mid-session from %s/%s — earlier turns in this conversation were answered by that model; continue seamlessly with full context", prevP, prevM)
	}
	return note
}

func systemPromptForMode(plannerMode bool) string {
	cwd, _ := os.Getwd()
	// Environment block (P5): cwd + platform + date ground the model in the
	// concrete machine, the same way Claude Code / OpenCode inject it. The
	// line is stable within a day, so the system-prefix cache is untouched
	// across prompts in the same session (P3 tracking: prefix stays stable).
	// The directive block is deliberately LEAN: opencode/Claude-style prompts
	// are short and let the model drive. Over-directing (18 numbered
	// imperatives, several of which contradicted each other — "synthesize and
	// answer now" vs "STRICTLY FORBIDDEN from prose on search-ish words") made
	// free models overthink, stall, or answer the wrong thing. The loop-level
	// safety (stall recovery, permission gate, budgets) lives in code, not
	// prompt text.
	// Lean agent prompt, modeled on the researched opencode/Claude structure:
	// provider header → environment → concise actionable rules → AGENTS.md.
	// NO "mandatory tool-call" imperatives — tool use is driven by the tool
	// schema + the harness loop (stall recovery, budgets, explore subagent),
	// exactly like Claude Code / opencode.
	basePersona := fmt.Sprintf("You are BroCode, an expert coding assistant working in `%s`.\n", cwd) +
		fmt.Sprintf("ENVIRONMENT: %s/%s | today: %s\n\n", runtime.GOOS, runtime.GOARCH, time.Now().Format("2006-01-02")) +
		"RULES (ALWAYS/NEVER):\n" +
		"- ALWAYS reason step-by-step before answering: understand the request → investigate for evidence → reason over what you found → then answer. Never answer from memory or assumption alone.\n" +
		"- ALWAYS trace actual code paths for feature questions: `search` for the terms, then `read` the matched files and follow the logic (callers → implementation → callers) before explaining how something works. Do not guess from file names alone.\n" +
		"- ALWAYS investigate before claiming or changing: use `search`/`read` for evidence; never assert project state from memory.\n" +
		"- ALWAYS answer the user directly and concisely once you have evidence — do not keep calling tools after the question is answerable.\n" +
		"- ALWAYS use the native tools (bash, search, read, write_file, edit_file, ask); never hand-write XML/code blocks for tool use. Delegate deep multi-file tracing to the `explore` subagent instead of dumping many files into context.\n" +
		"- ALWAYS make the smallest safe change (root cause first; consider blast radius; verify with build/test after changing).\n" +
		"- ALWAYS mirror the user's language; act autonomously without filler narration ('Step 1: …') or permission-asking for normal edits.\n" +
		"- NEVER run risky commands without user consent (the permission gate enforces this); if denied, adapt — do not retry.\n" +
		"- NEVER invent slash commands; suggest only real ones: /help /models /connect /search /compact /mcp /diff /tools /theme /clear /usage /memory /history.\n\n"

	// Mode block: a short agent-specific section, like opencode's plan/build
	// agent prompts. Read-only enforcement is ALSO structural (plannerMode
	// narrows toolsPayload to search/read/ask/explore), so the text is just
	// orientation, not the only guard.
	if plannerMode {
		basePersona += "MODE: PLANNER — read-only. Do not edit files or run mutating commands. Investigate, ask clarifying questions, and write the execution plan to 'brocode_plan.md' with markdown checkboxes (- [ ] Task).\n\n"
	} else {
		basePersona += "MODE: BUILDER — if 'brocode_plan.md' exists, execute its tasks in order and tick them (- [x]) only after verified completion.\n\n"
	}

	basePersona += "Tone: Crisp, deliberate, expert. You act autonomously directly in the user's workspace. Never ask the user to run commands you can run yourself."

	if data := cachedProjectDirectives(); data != "" {
		basePersona += "\n\nPROJECT DIRECTIVES (AGENTS.md):\n" + data
	}

	// AGENTS.md often references TOOLS FROM OTHER ECOSYSTEMS (kuma_context /
	// kuma_memory / matcha / mcp tools configured for opencode or Claude Code
	// on this repo). Those tools DO NOT EXIST in brocode — calling them burns
	// a round on "unsupported tool" feedback and stalls the loop. State this
	// explicitly so a weak free model that "MUST call kuma_context" per
	// AGENTS.md knows to adapt instead of retrying a phantom tool.
	basePersona += "\n\nIMPORTANT: This project's AGENTS.md may mention tools from other ecosystems (e.g. kuma_context, kuma_memory, kuma_safety, matcha, or MCP tools configured for opencode/Claude Code). Those tools are NOT available in brocode. NEVER call them — ignore those instructions and use brocode's native tools (bash, search, read) instead. If AGENTS.md says a step is optional when a tool is unavailable, treat it as unavailable and proceed normally."

	return basePersona
}

// ── AGENTS.md cache ─────────────────────────────────────────────────────
// systemPromptForMode reads AGENTS.md on EVERY message build (once per
// request). AGENTS.md is a project file that changes rarely, so caching it
// with mtime/size validation (same pattern as the config cache) turns a disk
// read per request into one stat per request — and zero I/O when unchanged.
var (
	agentsCachePath string
	agentsCacheMod  time.Time
	agentsCacheSize int64
	agentsCacheData string
)

// cachedProjectDirectives returns project directives, re-read only when any
// candidate file's mtime or size changes (stat-validated cache). The file is
// picked in opencode/Claude-discovery order: AGENTS.md first, then the
// project-brief conventions file (MATCHA_PROJECT.md — the CRM repo's domain
// brief opencode reads at startup), then CLAUDE.md / CONTEXT.md as fallbacks.
// Only the FIRST file found is used (local search stops at the first hit,
// mirroring the hierarchy). Reading MATCHA_PROJECT.md matters: it is the
// domain context that let opencode answer "rotation = CRM lead rotation"
// while brocode — which only looked for AGENTS.md (absent in that repo) —
// had zero domain grounding and answered about a tiptap line-shape component
// instead.
func cachedProjectDirectives() string {
	var candidates = []string{"AGENTS.md", "MATCHA_PROJECT.md", "CLAUDE.md", "CONTEXT.md"}
	var current string
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			current = c
			break
		}
	}
	if current == "" {
		agentsCachePath = ""
		agentsCacheData = ""
		return ""
	}
	fi, _ := os.Stat(current)
	if agentsCachePath == current && fi != nil && agentsCacheMod.Equal(fi.ModTime()) && agentsCacheSize == fi.Size() {
		return agentsCacheData
	}
	data, err := os.ReadFile(current)
	if err != nil {
		return ""
	}
	agentsCachePath = current
	agentsCacheMod = fi.ModTime()
	agentsCacheSize = fi.Size()
	agentsCacheData = string(data)
	return agentsCacheData
}

// zenMessages builds the OpenAI-style messages array from the bounded chat
// history (prior turns give the model context) plus the current prompt. The
// last chat entry is the just-appended user prompt from send(), so it is
// skipped and re-added explicitly as q. Assistant replies are sent WITHOUT
// their attribution footer. provider/model/window describe the CURRENT active
// model so the model knows its own identity and any mid-session switch.
// isTransientTurn reports whether q is an agentic-loop turn (tool result or
// ask answer) rather than a real user prompt. These turns are transient
// system events: they carry their own payload and must never trigger
// workspace enrichment (project-tree walk + 300-file keyword scan) — a
// mismatch between the prefixes used here and in send()/update.go made every
// tool round re-walk the repo, adding seconds and thousands of tokens per
// round. The agentic loop feeds results back as "Tool Execution Output:",
// the ask tool as "[SYSTEM ASK RESULT]".
func isTransientTurn(q string) bool {
	return strings.HasPrefix(q, "[SYSTEM TOOL RESULT]") ||
		strings.HasPrefix(q, "Tool Execution Output:") ||
		strings.HasPrefix(q, "[SYSTEM ASK RESULT]")
}

func zenMessages(chat []chatMsg, q, provider, model string, window int, plannerMode bool) []map[string]string {
	messages := make([]map[string]string, 0, len(chat)+2)
	messages = append(messages, map[string]string{
		"role":    "system",
		"content": systemPromptForMode(plannerMode),
	})
	for i := 0; i < len(chat)-1; i++ {
		var role, content string
		switch chat[i].role {
		case roleUser:
			role = "user"
			content = chat[i].text
			if content == "" {
				content = chat[i].content
			}
		case roleAgent:
			role = "assistant"
			content = stripAttribution(chat[i].text)
			if content == "" {
				content = chat[i].content
			}
		case roleTool:
			// Agentic-loop tool feedback is conversation data: the assistant
			// reply that follows it answers that output. Dropping these turns
			// left the model with back-to-back assistant messages and a blank
			// context gap ("what is the agent responding to?"). Sent as a
			// user turn with the raw [SYSTEM TOOL RESULT] payload — the same
			// convention the loop uses for the live tool-result prompt.
			role = "user"
			content = chat[i].content
			if content == "" {
				content = chat[i].text
			}
		case roleSystem:
			// Only the L2 compaction ledger (the folded-context summary) is
			// conversation data — UI chatter (theme changes, OAuth notices,
			// interrupt banners) must not reach the model. Dropping the ledger
			// silently erased everything compaction folded: the model saw a
			// mid-conversation gap.
			content = chat[i].text
			if content == "" {
				content = chat[i].content
			}
			if strings.Contains(content, "L2 ledger") {
				role = "system"
			}
		}
		// Never send a blank row (empty content after all fallbacks) — a
		// zero-length message would read as a broken/blank turn to the model.
		if role != "" && strings.TrimSpace(content) != "" {
			messages = append(messages, map[string]string{"role": role, "content": content})
		}
	}

	complexity, score := agentic.EvaluateComplexity(q)
	route := "FAST PATH (Minimal inspection)"
	if complexity == agentic.DeepPath {
		route = "DEEP PATH (Full plan -> impact -> implement -> review)"
	} else if complexity == agentic.NormalPath {
		route = "NORMAL PATH (Inspect -> implement -> verify)"
	}

	// Inject the routing context AND the model-identity note into the user
	// prompt. Both are per-request metadata — they belong at the end of the
	// last user message so the stable system-prompt prefix (and its cache) is
	// never touched. The old "CRITICAL RUNTIME DIRECTIVES" block was REMOVED:
	// it re-injected aggressive imperatives into every prompt ("EXPLAIN PASTED
	// TEXT FIRST", "MANDATORY TOOL EXECUTION") that duplicated — and
	// contradicted — the lean system prompt, and told weak models to act
	// before understanding, producing the wrong-topic answers the user
	// reported.
	routedPrompt := fmt.Sprintf(
		"%s\n\n[SYSTEM METADATA: Task Complexity Score: %d | Assigned Route: %s | %s. Adjust your workflow depth accordingly.]",
		q, score, route, modelIdentityNote(chat, provider, model, window))

	messages = append(messages, map[string]string{
		"role":    "user",
		"content": routedPrompt,
	})
	return messages
}

// toolsPayload returns the native OpenAI JSON Tools schema for function
// calling. plannerMode narrows the surface to the read-only core (search,
// read, ask) — the model physically cannot emit write/edit/bash tool calls in
// plan mode (P3 enforcement at the tool level, on top of the existing
// text-block gates in builder.go).
func toolsPayload(plannerMode bool) []map[string]interface{} {
	all := []map[string]interface{}{
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "bash",
				"description": "Execute a shell command in the workspace.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{
							"type":        "string",
							"description": "The shell command to execute.",
						},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "search",
				"description": "Search workspace files instantly using pre-indexed BM25 relevance search.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "The search query keywords.",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "read",
				"description": "Read file content from the workspace.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "File path to read.",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "write_file",
				"description": "Create or completely overwrite a file.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "File path to write.",
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The full content to write into the file.",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "edit_file",
				"description": "Search and replace a specific block of text in an existing file. Use this to modify files safely.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "File path to edit.",
						},
						"search": map[string]interface{}{
							"type":        "string",
							"description": "The exact existing code block to replace.",
						},
						"replace": map[string]interface{}{
							"type":        "string",
							"description": "The new code block that will replace the search block.",
						},
					},
					"required": []string{"path", "search", "replace"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "ask",
				"description": "Ask the user a clarifying question before proceeding.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"question": map[string]interface{}{
							"type":        "string",
							"description": "The question to ask the user.",
						},
						"options": map[string]interface{}{
							"type": "array",
							"items": map[string]interface{}{
								"type": "string",
							},
							"description": "List of options for the user to select from.",
						},
					},
					"required": []string{"question", "options"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "explore",
				"description": "Delegate focused read-only codebase research to a subagent. Use for deep multi-file investigation (e.g. 'map the rotation pipeline'): the subagent searches and reads many files and returns a condensed report with file:line findings. Keeps the main context lean — only the report comes back.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"question": map[string]interface{}{
							"type":        "string",
							"description": "The focused research question for the subagent.",
						},
					},
					"required": []string{"question"},
				},
			},
		},
	}
	if !plannerMode {
		return all
	}
	// Plan mode: keep the read-only core — search, read, ask + explore
	// (delegated research is read-only by construction). The model physically
	// cannot emit write/edit/bash tool calls (P3 enforcement at the tool
	// level, on top of the existing text-block gates in builder.go).
	out := make([]map[string]interface{}, 0, 4)
	for _, t := range all {
		fn, _ := t["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		if name == "search" || name == "read" || name == "ask" || name == "explore" {
			out = append(out, t)
		}
	}
	return out
}

// parseZenResponse extracts the answer text, the optional reasoning trace and
// the real token usage from a Zen chat-completions response (OpenAI shape
// plus reasoning_content). It sanitizes trailing SSE markers (e.g. `data: [DONE]`)
// that proxy gateways like 9router/OpenRouter append to non-streaming HTTP responses.
// zenToolCall is one native tool call emitted by the model (OpenAI shape).
// Shared by the non-streaming parser (parseZenResponse) and the SSE path
// (zenChatReply) so both produce identical executable blocks (P2).
type zenToolCall struct {
	ID        string
	Name      string
	Arguments string // raw JSON arguments string, parsed per-tool below
}

// toolCallsToBlocks renders native tool calls as the executable text blocks
// the tool runner understands (```bash fences, <tool_call> XML, cat >
// heredocs, SEARCH/REPLACE hunks). Any leading content text is preserved
// above the blocks. Unknown tool names are skipped rather than echoed raw
// (the runner would reject them with a confusing error).
func toolCallsToBlocks(text string, tcs []zenToolCall) string {
	var sb strings.Builder
	if text != "" {
		sb.WriteString(text + "\n\n")
	}
	for _, tc := range tcs {
		name := strings.ToLower(tc.Name)
		var args map[string]interface{}
		_ = json.Unmarshal([]byte(tc.Arguments), &args)

		switch name {
		case "bash", "sh":
			if cmd, ok := args["command"].(string); ok && strings.TrimSpace(cmd) != "" {
				sb.WriteString(fmt.Sprintf("```bash\n%s\n```\n", strings.TrimSpace(cmd)))
			}
		case "search":
			if query, ok := args["query"].(string); ok && strings.TrimSpace(query) != "" {
				sb.WriteString(fmt.Sprintf("<tool_call>search\n%s\n</tool_call>\n", strings.TrimSpace(query)))
			}
		case "read":
			if path, ok := args["path"].(string); ok && strings.TrimSpace(path) != "" {
				sb.WriteString(fmt.Sprintf("<tool_call>read\n%s\n</tool_call>\n", strings.TrimSpace(path)))
			}
		case "write_file":
			if path, ok := args["path"].(string); ok {
				if content, ok := args["content"].(string); ok {
					// Use builder.go's cat block format for file writing
					sb.WriteString(fmt.Sprintf("cat > %s << 'EOF'\n%s\nEOF\n", strings.TrimSpace(path), content))
				}
			}
		case "edit_file":
			if path, ok := args["path"].(string); ok {
				if search, ok := args["search"].(string); ok {
					if replace, ok := args["replace"].(string); ok {
						// Use builder.go's SEARCH/REPLACE format
						sb.WriteString(fmt.Sprintf("<<<<<<< SEARCH: %s\n%s\n=======\n%s\n>>>>>>> REPLACE\n", strings.TrimSpace(path), search, replace))
					}
				}
			}
		case "ask":
			if question, ok := args["question"].(string); ok {
				if optionsList, ok := args["options"].([]interface{}); ok {
					sb.WriteString(fmt.Sprintf("<tool_call>ask\n<ask_question>%s\n", strings.TrimSpace(question)))
					for _, opt := range optionsList {
						if optStr, ok := opt.(string); ok {
							sb.WriteString(fmt.Sprintf("- %s\n", strings.TrimSpace(optStr)))
						}
					}
					sb.WriteString("</ask_question>\n</tool_call>\n")
				}
			}
		case "explore":
			if question, ok := args["question"].(string); ok && strings.TrimSpace(question) != "" {
				sb.WriteString(fmt.Sprintf("<tool_call>explore\n%s\n</tool_call>\n", strings.TrimSpace(question)))
			}
		}
	}
	return sb.String()
}

// It natively parses OpenAI tool_calls JSON payloads.
func parseZenResponse(body []byte) (text, reasoning string, tok tokenUsage, err error) {
	s := strings.TrimSpace(string(body))
	if idx := strings.Index(s, "data: [DONE]"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if first := strings.IndexByte(s, '{'); first >= 0 {
		if last := strings.LastIndexByte(s, '}'); last > first {
			s = s[first : last+1]
		}
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(s), &resp); err != nil {
		return "", "", tokenUsage{}, err
	}
	if len(resp.Choices) > 0 {
		msg := resp.Choices[0].Message
		text = msg.Content
		reasoning = strings.TrimSpace(msg.ReasoningContent)

		// Convert native OpenAI tool_calls into clean execution blocks (shared
		// helper — the SSE path produces the same blocks, P2).
		if len(msg.ToolCalls) > 0 {
			tcs := make([]zenToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				tcs = append(tcs, zenToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			text = toolCallsToBlocks(text, tcs)
		}
	}

	// Handle thinking models (like DeepSeek / Poolside) that output raw <think> ... </think> tags
	if strings.Contains(text, "<think>") {
		if start := strings.Index(text, "<think>"); start >= 0 {
			if end := strings.Index(text, "</think>"); end > start {
				th := strings.TrimSpace(text[start+7 : end])
				if reasoning == "" {
					reasoning = th
				} else {
					reasoning = reasoning + "\n" + th
				}
				text = strings.TrimSpace(text[:start] + text[end+8:])
			}
		}
	}
	tok.input = resp.Usage.PromptTokens
	tok.output = resp.Usage.CompletionTokens
	tok.total = resp.Usage.TotalTokens
	if tok.total == 0 {
		tok.total = tok.input + tok.output // some endpoints omit total_tokens
	}
	return text, reasoning, tok, nil
}

// zenChatReply performs the native OpenCode Zen chat-completions call using
// SSE streaming (`stream: true`) for real-time token delivery. The user sees
// the response being written live instead of a dead "waiting…" spinner.
// Tokens arrive via traceCh; the full response is assembled for the final
// agentResultMsg. cancel is installed as the context cancel so ESC can abort.
func zenChatReply(m Model, q, model, endpoint string, traceCh chan<- agentTraceMsg, startTime time.Time, cancel *func(), run int) tea.Msg {
	label := "opencode/" + model // display name — the wire ID is bare

	// Use unified workflow for initial phases
	wf := newWorkflow(traceCh, "opencode", model, startTime)
	wf.phaseThinking() // → thinking… · opencode/model
	reqBody, err := json.Marshal(map[string]interface{}{
		"model":       model,
		"messages":    zenMessages(m.chat, q, "opencode", model, m.window, m.plannerMode),
		"tools":       toolsPayload(m.plannerMode),
		"temperature": 0.7,
		"max_tokens":  4096,
		"stream":      true,
	})
	if err != nil {
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("opencode", "", err),
			items: []activityItem{{tool: "opencode", label: "zen request build failed", status: "error", detail: err.Error()}},
		}, run: run}
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	*cancel = ctxCancel // ESC interrupt aborts the request

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(reqBody)))
	if err != nil {
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("opencode", "", err),
			items: []activityItem{{tool: "opencode", label: "zen request failed", status: "error", detail: err.Error()}},
		}, run: run}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("opencode", "", err),
			items: []activityItem{{tool: "opencode", label: "zen request failed", status: "error", detail: err.Error()}},
		}, run: run}
	}
	defer resp.Body.Close()

	// Non-streaming fallback: if the gateway doesn't return SSE (wrong
	// content-type or error status), fall back to reading the full body.
	ct := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(ct, "text/event-stream")
	if resp.StatusCode != http.StatusOK || !isSSE {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return agentResultMsg{reply: mockReply{
				text:  parseCLIError("opencode", string(body), nil),
				items: []activityItem{{tool: "opencode", label: fmt.Sprintf("zen HTTP %d", resp.StatusCode), status: "error", detail: clip(string(body), 100)}},
			}, run: run}
		}
		// Non-SSE 200 — parse as regular JSON response
		text, reasoning, tok, parseErr := parseZenResponse(body)
		if parseErr != nil || text == "" {
			errDetail := "empty response"
			if parseErr != nil {
				errDetail = parseErr.Error()
			}
			return agentResultMsg{reply: mockReply{
				text:  parseCLIError("opencode", string(body), parseErr),
				items: []activityItem{{tool: "opencode", label: "zen parse error", status: "error", detail: errDetail}},
			}, run: run}
		}
		elapsed := time.Since(startTime)
		var col *collapse
		if reasoning != "" {
			col = &collapse{
				summary: "thinking trace (" + fmt.Sprintf("%d chars", len(reasoning)) + ")",
				content: reasoning,
			}
		}
		text += fmt.Sprintf("\n\n  ⚡ %s · %.1fs · %s tokens", label, elapsed.Seconds(), fmtTokens(tok.total))
		return agentResultMsg{
			reply: mockReply{
				text: text, collapse: col,
				items: []activityItem{{tool: "opencode", label: "zen " + label, status: "ok",
					detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), tok.total)}},
			},
			tokens: tok, run: run,
		}
	}

	// ── SSE streaming ──────────────────────────────────────────────────
	sendPhase(traceCh, "reasoning…", "")

	var fullContent strings.Builder
	var reasoning strings.Builder
	var tok tokenUsage
	phase := "reasoning"
	chunkCount := 0
	// Native tool-call fragments accumulated per index across SSE deltas (P2).
	var streamToolCalls []struct {
		ID   string
		Name string
		Args strings.Builder
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		// SSE protocol: lines starting with "data: " carry the payload
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		// Parse the SSE chunk (OpenAI streaming delta format). delta.tool_calls
		// ARE parsed here (P2): the gateway streams native tool calls as
		// fragments — {index, id, function:{name, arguments}} — where name
		// appears once and arguments arrive in pieces that must be
		// concatenated per index.
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Role             string `json:"role"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}

		// Extract token usage if present (some providers send it in the last chunk)
		if chunk.Usage != nil {
			tok.input = chunk.Usage.PromptTokens
			tok.output = chunk.Usage.CompletionTokens
			tok.total = chunk.Usage.TotalTokens
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		// Reasoning/thinking tokens
		if delta.ReasoningContent != "" {
			reasoning.WriteString(delta.ReasoningContent)
			if phase != "reasoning" {
				phase = "reasoning"
				sendPhase(traceCh, "reasoning…", "")
			}
			continue
		}

		// Content tokens — transition from reasoning to writing
		if delta.Content != "" {
			if phase == "reasoning" && fullContent.Len() == 0 {
				phase = "writing"
				sendPhase(traceCh, "writing response…", "")
			}
			fullContent.WriteString(delta.Content)
			chunkCount++
		}

		// Native tool call fragments — accumulate by index (P2).
		for _, tc := range delta.ToolCalls {
			for len(streamToolCalls) <= tc.Index {
				streamToolCalls = append(streamToolCalls, struct {
					ID   string
					Name string
					Args strings.Builder
				}{})
			}
			if tc.ID != "" {
				streamToolCalls[tc.Index].ID = tc.ID
			}
			if tc.Function.Name != "" {
				streamToolCalls[tc.Index].Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				streamToolCalls[tc.Index].Args.WriteString(tc.Function.Arguments)
			}
		}

		// Check for finish
		if chunk.Choices[0].FinishReason != nil {
			break
		}
	}

	elapsed := time.Since(startTime)
	text := strings.TrimSpace(fullContent.String())
	reasoningText := strings.TrimSpace(reasoning.String())

	// Native tool calls streamed over SSE are converted to executable blocks
	// HERE — no non-streaming retry needed (P2). Blocks for content-less tool
	// replies become the whole reply text and feed the existing tool runner.
	if len(streamToolCalls) > 0 {
		tcs := make([]zenToolCall, 0, len(streamToolCalls))
		for _, stc := range streamToolCalls {
			if stc.Name == "" {
				continue // fragment with no name — skip, never emit empty blocks
			}
			tcs = append(tcs, zenToolCall{ID: stc.ID, Name: stc.Name, Arguments: stc.Args.String()})
		}
		if len(tcs) > 0 {
			text = toolCallsToBlocks(text, tcs)
		}
	}

	// A stream that delivered ONLY reasoning (no content, no tool call) is
	// NOT a finished answer: the model stalled promising to search. Retry
	// non-streaming BEFORE promoting the trace to the reply text — the retry
	// may still return real content (AGENTIC_OVERHAUL D2).
	if text == "" {
		// Non-streaming fallback retry: stream gave 0 content tokens → retry once synchronously with stream: false
		fallbackBody, _ := json.Marshal(map[string]interface{}{
			"model":       model,
			"messages":    zenMessages(m.chat, q, "opencode", model, m.window, m.plannerMode),
			"tools":       toolsPayload(m.plannerMode),
			"temperature": 0.7,
			"max_tokens":  4096,
			"stream":      false,
		})
		fbReq, fbErr := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(fallbackBody)))
		if fbErr == nil {
			fbReq.Header.Set("Content-Type", "application/json")
			if fbResp, fbDoErr := client.Do(fbReq); fbDoErr == nil {
				defer fbResp.Body.Close()
				if fbData, readErr := io.ReadAll(fbResp.Body); readErr == nil {
					fbText, fbReason, fbTok, pErr := parseZenResponse(fbData)
					if pErr == nil && fbText != "" {
						text = fbText
						if fbReason != "" {
							reasoningText = fbReason
						}
						if fbTok.total > 0 {
							tok = fbTok
						}
					}
				}
			}
		}
	}

	// Reasoning-only promotion — AFTER the retry, so a thinking trace is never
	// presented as the answer while a tool call or real content is recoverable.
	if text == "" && reasoningText != "" {
		text = reasoningText

		// Stall recovery (AGENTIC_OVERHAUL P0): the reply is STILL
		// reasoning-only — no content, no tool call — so the turn would
		// dead-end on a thinking trace. Re-prompt ONCE with a [SYSTEM TOOL
		// RESULT] the model cannot ignore. The trigger is the OBSERVED stall
		// itself (no prompt classifier — a reasoning-only reply is always a
		// failed turn, whatever the language), and it only costs a call when
		// the evidence pass actually matches files. Bounded: this is the
		// third and final model call per user prompt (stream + retry +
		// recovery). rawQ strips the workspace-context prefix
		// attachFileContext added, so the evidence pass sees the real prompt.
		rawQ := stripEnrichedPrompt(q)
		// Tool/ask-result turns carry their own payload and must never trigger
		// the evidence re-prompt (the model already holds the tool output; a
		// second evidence injection would be a redundant full model call).
		if !isTransientTurn(rawQ) {
			if ev := explorationEvidence(rawQ, nil); ev != "" {
				msgs := zenMessages(m.chat, q, "opencode", model, m.window, m.plannerMode)
				msgs = append(msgs, map[string]string{
					"role":    "user",
					"content": "[SYSTEM TOOL RESULT]\n" + ev + "\nYour previous reply contained only reasoning with no tool call and no answer. Use this evidence to answer now, or continue investigating with the `search` / `read` tools.",
				})
				if body, err := json.Marshal(map[string]interface{}{
					"model":       model,
					"messages":    msgs,
					"tools":       toolsPayload(m.plannerMode),
					"temperature": 0.7,
					"max_tokens":  4096,
					"stream":      false,
				}); err == nil {
					if recReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(body))); err == nil {
						recReq.Header.Set("Content-Type", "application/json")
						if recResp, err := client.Do(recReq); err == nil {
							recData, recErr := io.ReadAll(recResp.Body)
							recResp.Body.Close()
							if recErr == nil {
								if rText, _, rTok, pErr := parseZenResponse(recData); pErr == nil && rText != "" {
									text = rText
									if rTok.total > 0 {
										tok = rTok
									}
								}
							}
						}
					}
				}
			}
		}
	}

	if text == "" {
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("opencode", "empty streaming response from zen gateway", nil),
			items: []activityItem{{tool: "opencode", label: "zen empty response", status: "error", detail: "no content"}},
		}, run: run}
	}

	if tok.total == 0 {
		tok.total = tok.input + tok.output
	}

	var col *collapse
	if reasoningText != "" {
		col = &collapse{
			summary: "thinking trace (" + fmt.Sprintf("%d chars", len(reasoningText)) + ")",
			content: reasoningText,
		}
	}
	text += fmt.Sprintf("\n\n  ⚡ %s · %.1fs · %s tokens", label, elapsed.Seconds(), fmtTokens(tok.total))

	sendPhase(traceCh, "done", fmt.Sprintf("→ %d chunks · %.1fs", chunkCount, elapsed.Seconds()))

	return agentResultMsg{
		reply: mockReply{
			text:     text,
			collapse: col,
			items: []activityItem{{
				tool: "opencode", label: "zen " + label, status: "ok",
				detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), tok.total),
			}},
		},
		tokens: tok,
		run:    run,
	}
}

// agentWorkCmd executes the active provider for one prompt. OpenCode runs
// NATIVELY — brocode calls the OpenCode Zen gateway (OpenAI-compatible, free
// models need no API key) directly over HTTP instead of wrapping the opencode
// CLI. Antigravity still shells out to agy (its API is undocumented), and
// api-key providers (groq) go native over HTTP. cancel is a pointer so the
// in-flight request can be aborted (context cancel / subprocess kill) when
// the user presses ESC. run tags the result so stale/interrupted runs are
// dropped in Update.
func (m Model) agentWorkCmd(q string, traceCh chan<- agentTraceMsg, askCh chan<- agentAskMsg, answerCh chan string, cancel *func(), run int) tea.Cmd {
	selectedMod := m.selectedModel
	if selectedMod == "" {
		selectedMod = openCodeFreeModels[0]
	}
	startTime := time.Now()

	return func() tea.Msg {
		defer close(traceCh) // Always close so waitForTrace stops
		defer close(askCh)   // …and waitForAsk

		// Workspace context enrichment (project tree + file attachments +
		// keyword search) runs HERE in the background goroutine — never in
		// the UI update loop. send() used to attach it synchronously, which
		// froze the whole TUI (Enter AND typing) on every prompt while the
		// project was walked and every file was read. Tool-result turns skip
		// enrichment: they are transient system events that don't need (and
		// shouldn't burn tokens on) workspace context. The guard must match
		// the EXACT prefixes send() uses for those transient turns — the
		// agentic loop feeds results back as "Tool Execution Output:" (not
		// "[SYSTEM TOOL RESULT]"), and a mismatch here made every tool round
		// re-walk the tree + rescan up to 300 files, bloating per-round
		// latency and tokens (observed: ~2.2s + ~6k tokens per read round).
		if isTransientTurn(q) {
			// Tool-result turns: strip the transport prefix, then attach the
			// CHEAP cached tree (orientation only — free models forget
			// structure between rounds). No heavy re-walk / 300-file scan.
			q = strings.TrimPrefix(q, "Tool Execution Output:\n")
			q = attachTransientContext(q)
		} else {
			q = attachFileContext(q, m.plannerMode)
		}

		// OpenCode provider: native HTTP to the Zen gateway — no CLI wrapper.
		if m.provider == "opencode" {
			return zenChatReply(m, q, selectedMod, zenEndpoint, traceCh, startTime, cancel, run)
		}

		// Attempt real antigravity execution via agy CLI runner if installed and antigravity provider active
		// Follows the unified provider workflow: thinking → [processing → receiving → done]
		if m.provider == "antigravity" {
			agyMod := m.selectedModel
			if agyMod == "" {
				agyMod = "gemini-3.6-flash"
			}
			agyBin := "agy"
			if _, err := exec.LookPath("agy"); err != nil {
				// If agy is not in PATH, check ~/.local/bin/agy directly
				home, _ := os.UserHomeDir()
				localAgy := filepath.Join(home, ".local", "bin", "agy")
				if _, errStat := os.Stat(localAgy); errStat == nil {
					agyBin = localAgy
				} else {
					// Auto-install agy CLI package via curl installer
					installCmd := exec.Command("sh", "-c", "curl -fsSL https://antigravity.google/install.sh | sh")
					_ = installCmd.Run()
					if _, errStat2 := os.Stat(localAgy); errStat2 == nil {
						agyBin = localAgy
					}
				}
			}

			// Use unified workflow for initial phase
			wf := newWorkflow(traceCh, "antigravity", agyMod, startTime)
			wf.phaseThinking() // → thinking… · antigravity/model

			// Use context with timeout to prevent hanging
			ctx, ctxCancel := context.WithTimeout(context.Background(), 600*time.Second)
			defer ctxCancel()
			cmd := exec.CommandContext(ctx, agyBin, "run", "--model", agyMod, q)
			agyStdoutPipe, agyErr := cmd.StdoutPipe()
			if agyErr == nil && cmd.Start() == nil {
				*cancel = func() { _ = cmd.Process.Kill() } // ESC interrupt
				var agyOutput strings.Builder
				scanner := bufio.NewScanner(agyStdoutPipe)
				agyLineCount := 0
				// Track last phase to avoid spamming same phase
				lastPhase := ""
				for scanner.Scan() {
					line := scanner.Text()
					agyOutput.WriteString(line + "\n")

					if phase := detectPhase(line); phase != "" {
						if phase != lastPhase {
							// Push the phase AND the real tool line into the chat log.
							sendPhase(traceCh, phase, strings.TrimSpace(line))
							lastPhase = phase
						}
					} else if agyLineCount == 0 {
						sendPhase(traceCh, "generating response…", "→ waiting for first token")
					}
					agyLineCount++
				}
				cmdErr := cmd.Wait()
				elapsed := time.Since(startTime)
				respText := strings.TrimSpace(agyOutput.String())

				// Handle timeout or execution errors
				if cmdErr != nil {
					if ctx.Err() == context.DeadlineExceeded {
						wf.phaseError("timeout after 600s")
						return agentResultMsg{reply: mockReply{
							text:  fmt.Sprintf("⏱️ **Antigravity Timeout**\n\nCommand timed out after 600 seconds.\nThe model may be slow or the request too complex.\n\nPartial output (%d chars):\n%s", len(respText), clip(respText, 200)),
							items: []activityItem{{tool: "antigravity", label: "agy run timeout", status: "error", detail: "600s timeout"}},
						}, run: run}
					}
					// Other execution errors
					if respText == "" {
						wf.phaseError(cmdErr.Error())
						return agentResultMsg{reply: mockReply{
							text:  parseCLIError("antigravity", "", cmdErr),
							items: []activityItem{{tool: "antigravity", label: "agy run failed", status: "error", detail: cmdErr.Error()}},
						}, run: run}
					}
				}

				if len(respText) > 0 {
					attr := fmt.Sprintf("\n\n  ⚡ antigravity/%s · %.1fs", agyMod, elapsed.Seconds())
					respText += attr

					// Phase done: success
					wf.phaseDone(elapsed, fmt.Sprintf("%d chars", len(respText)))

					reply := mockReply{
						text: respText,
						items: []activityItem{
							{tool: "antigravity", label: fmt.Sprintf("agy run --model %s", agyMod), status: "ok", detail: fmt.Sprintf("%.1fs response", elapsed.Seconds())},
						},
					}
					return agentResultMsg{reply: reply, run: run}
				}
				// Empty response with no error - likely agy CLI issue
				if respText == "" {
					wf.phaseError("empty response from agy CLI")
					return agentResultMsg{reply: mockReply{
						text:  "❌ **Antigravity Empty Response**\n\nThe agy CLI returned no output.\nThis could mean:\n• agy is not properly installed or configured\n• The model is unavailable\n• Network connection issue\n\nTry: `agy auth` to re-authenticate, or switch provider with `/connect`",
						items: []activityItem{{tool: "antigravity", label: "agy empty response", status: "error", detail: "no output"}},
					}, run: run}
				}
			} else {
				// Fallback: blocking CombinedOutput if StdoutPipe fails
				out, err := cmd.CombinedOutput()
				elapsed := time.Since(startTime)
				respText := strings.TrimSpace(string(out))

				// Handle timeout
				if ctx.Err() == context.DeadlineExceeded {
					wf.phaseError("timeout after 600s")
					return agentResultMsg{reply: mockReply{
						text:  fmt.Sprintf("⏱️ **Antigravity Timeout**\n\nCommand timed out after 600 seconds.\nPartial output:\n%s", clip(respText, 200)),
						items: []activityItem{{tool: "antigravity", label: "agy run timeout", status: "error", detail: "600s timeout"}},
					}, run: run}
				}

				if err == nil && len(respText) > 0 {
					attr := fmt.Sprintf("\n\n  ⚡ antigravity/%s · %.1fs", agyMod, elapsed.Seconds())
					respText += attr

					// Phase done: success (fallback path)
					wf.phaseDone(elapsed, fmt.Sprintf("%d chars", len(respText)))

					reply := mockReply{
						text: respText,
						items: []activityItem{
							{tool: "antigravity", label: fmt.Sprintf("agy run --model %s", agyMod), status: "ok", detail: fmt.Sprintf("%.1fs response", elapsed.Seconds())},
						},
					}
					return agentResultMsg{reply: reply, run: run}
				}
				errStr := parseCLIError("antigravity", respText, err)
				reply := mockReply{
					text: errStr,
					items: []activityItem{
						{tool: "antigravity", label: fmt.Sprintf("agy run --model %s", agyMod), status: "error", detail: "provider error"},
					},
				}
				return agentResultMsg{reply: reply, run: run}
			}
		}

		// Freebuff provider: native API with saved credentials
		if m.provider == "freebuff" {
			return freebuffNativeAgentRun(m, q, traceCh, askCh, answerCh, startTime, cancel, run)
		}

		// Codebuff provider: native API with saved credentials
		if m.provider == "codebuff" {
			return codebuffNativeAgentRun(m, q, traceCh, askCh, answerCh, startTime, cancel, run)
		}

		// Groq provider: OpenAI-compatible chat completions
		// Follows the unified provider workflow: thinking → processing → receiving → done
		if m.provider == "groq" {
			apiKey := loadAPIKey("groq")
			if apiKey == "" {
				wf := newWorkflow(traceCh, "groq", "unknown", startTime)
				wf.phaseError("API key required")
				reply := mockReply{
					text:  "🔑 **Groq API Key Required**\n\nOpen `/connect` → select Groq → paste API key from console.groq.com",
					items: []activityItem{{tool: "groq", label: "groq api key missing", status: "error", detail: "no key saved"}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			groqModel := m.selectedModel
			if groqModel == "" {
				groqModel = groqModels[0]
			}

			wf := newWorkflow(traceCh, "groq", groqModel, startTime)

			// Phase 1: thinking
			wf.phaseThinking()

			// Build OpenAI-compatible request body. The messages come from
			// zenMessages(m.chat, q) — the SAME history builder the other
			// OpenAI-compatible providers use. Sending only the current query
			// made every groq turn context-free ("gas" after a long plan was
			// answered as if it were a first message).
			reqBody := map[string]interface{}{
				"model":       groqModel,
				"messages":    zenMessages(m.chat, q, "groq", groqModel, m.window, m.plannerMode),
				"tools":       toolsPayload(m.plannerMode),
				"temperature": 0.7,
				"max_tokens":  4096,
			}
			reqJSON, _ := json.Marshal(reqBody)

			// Phase 2: processing
			wf.phaseProcessing()

			req, err := http.NewRequest("POST", "https://api.groq.com/openai/v1/chat/completions", strings.NewReader(string(reqJSON)))
			if err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("groq", "", err),
					items: []activityItem{{tool: "groq", label: "groq request build failed", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiKey)

			client := &http.Client{Timeout: 600 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("groq", "", err),
					items: []activityItem{{tool: "groq", label: "groq request failed", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				wf.phaseError(fmt.Sprintf("HTTP %d", resp.StatusCode))
				reply := mockReply{
					text:  parseCLIError("groq", string(respBody), nil),
					items: []activityItem{{tool: "groq", label: fmt.Sprintf("groq HTTP %d", resp.StatusCode), status: "error", detail: string(respBody[:min(100, len(respBody))])}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			// Phase 3: receiving response
			wf.phaseReceiving()

			// Parse OpenAI-compatible response
			var groqResp struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(respBody, &groqResp); err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("groq", string(respBody), err),
					items: []activityItem{{tool: "groq", label: "groq parse error", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			elapsed := time.Since(startTime)
			respText := ""
			if len(groqResp.Choices) > 0 {
				respText = groqResp.Choices[0].Message.Content
			}

			tok := tokenUsage{
				input:  groqResp.Usage.PromptTokens,
				output: groqResp.Usage.CompletionTokens,
				total:  groqResp.Usage.TotalTokens,
			}

			if respText != "" {
				attr := fmt.Sprintf("\n\n  ⚡ groq/%s · %.1fs · %s tokens", groqModel, elapsed.Seconds(), fmtTokens(groqResp.Usage.TotalTokens))
				respText += attr

				// Phase 4: done
				wf.phaseDone(elapsed, fmt.Sprintf("%d tokens", tok.total))

				reply := mockReply{
					text: respText,
					items: []activityItem{{
						tool: "groq", label: fmt.Sprintf("groq %s", groqModel), status: "ok",
						detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), groqResp.Usage.TotalTokens),
					}},
				}
				return agentResultMsg{reply: reply, tokens: tok, run: run}
			}
		}

		// Poolside provider: OpenAI-compatible chat completions
		// Follows the unified provider workflow: thinking → processing → receiving → done
		if m.provider == "poolside" {
			apiKey := loadAPIKey("poolside")
			if apiKey == "" {
				wf := newWorkflow(traceCh, "poolside", "unknown", startTime)
				wf.phaseError("API key required")
				reply := mockReply{
					text:  "🔑 **Poolside API Key Required**\n\nOpen `/connect` → select Poolside → paste API key from inference.poolside.ai",
					items: []activityItem{{tool: "poolside", label: "poolside api key missing", status: "error", detail: "no key saved"}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			poolModel := m.selectedModel
			if poolModel == "" {
				poolModel = poolsideModels[0]
			}

			wf := newWorkflow(traceCh, "poolside", poolModel, startTime)
			wf.phaseThinking()

			reqBody := map[string]interface{}{
				"model":       poolModel,
				"messages":    zenMessages(m.chat, q, "poolside", poolModel, m.window, m.plannerMode),
				"tools":       toolsPayload(m.plannerMode),
				"temperature": 0.7,
				"max_tokens":  4096,
			}
			reqJSON, _ := json.Marshal(reqBody)

			wf.phaseProcessing()

			req, err := http.NewRequest("POST", "https://inference.poolside.ai/v1/chat/completions", strings.NewReader(string(reqJSON)))
			if err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("poolside", "", err),
					items: []activityItem{{tool: "poolside", label: "poolside request build failed", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiKey)

			client := &http.Client{Timeout: 600 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("poolside", "", err),
					items: []activityItem{{tool: "poolside", label: "poolside request failed", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				wf.phaseError(fmt.Sprintf("HTTP %d", resp.StatusCode))
				reply := mockReply{
					text:  parseCLIError("poolside", string(respBody), nil),
					items: []activityItem{{tool: "poolside", label: fmt.Sprintf("poolside HTTP %d", resp.StatusCode), status: "error", detail: string(respBody[:min(100, len(respBody))])}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			wf.phaseReceiving()

			var poolResp struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(respBody, &poolResp); err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("poolside", string(respBody), err),
					items: []activityItem{{tool: "poolside", label: "poolside parse error", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			elapsed := time.Since(startTime)
			respText := ""
			if len(poolResp.Choices) > 0 {
				respText = poolResp.Choices[0].Message.Content
			}

			tok := tokenUsage{
				input:  poolResp.Usage.PromptTokens,
				output: poolResp.Usage.CompletionTokens,
				total:  poolResp.Usage.TotalTokens,
			}

			if respText != "" {
				attr := fmt.Sprintf("\n\n  ⚡ poolside/%s · %.1fs · %s tokens", poolModel, elapsed.Seconds(), fmtTokens(poolResp.Usage.TotalTokens))
				respText += attr

				wf.phaseDone(elapsed, fmt.Sprintf("%d tokens", tok.total))

				reply := mockReply{
					text: respText,
					items: []activityItem{{
						tool: "poolside", label: fmt.Sprintf("poolside %s", poolModel), status: "ok",
						detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), poolResp.Usage.TotalTokens),
					}},
				}
				return agentResultMsg{reply: reply, tokens: tok, run: run}
			}
		}
		// Dynamic Custom Providers from config.jsonc
		cfg := LoadAppConfig()
		cfgProvider, isCustom := cfg.Provider[m.provider]
		if isCustom || m.provider == "custom" {
			var endpoint, apiKey string
			headers := make(map[string]string)

			if isCustom {
				endpoint = cfgProvider.Options.BaseURL
				apiKey = cfgProvider.Options.APIKey
				if cfgProvider.Options.Headers != nil {
					headers = cfgProvider.Options.Headers
				}
			} else {
				// Legacy custom fallback
				keyData := loadAPIKey("custom")
				parts := strings.Split(keyData, "|")
				endpoint = "http://localhost:11434/v1/chat/completions"
				if len(parts) >= 1 && parts[0] != "" {
					endpoint = parts[0]
				}
				if len(parts) >= 2 {
					apiKey = parts[1]
				}
			}

			if !strings.HasSuffix(endpoint, "/chat/completions") {
				endpoint = strings.TrimRight(endpoint, "/") + "/chat/completions"
			}

			customModel := m.selectedModel
			if customModel == "" {
				customModel = "default"
			}

			wf := newWorkflow(traceCh, m.provider, customModel, startTime)
			wf.phaseThinking()
			reqBody := map[string]interface{}{
				"model":       customModel,
				"messages":    zenMessages(m.chat, q, m.provider, customModel, m.window, m.plannerMode),
				"tools":       toolsPayload(m.plannerMode),
				"temperature": 0.7,
				"max_tokens":  8192,
			}
			reqJSON, _ := json.Marshal(reqBody)
			wf.phaseProcessing()

			req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(reqJSON)))
			if err != nil {
				wf.phaseError(err.Error())
				return agentResultMsg{reply: mockReply{
					text:  parseCLIError(m.provider, "", err),
					items: []activityItem{{tool: m.provider, label: "request build failed", status: "error", detail: err.Error()}},
				}, run: run}
			}
			req.Header.Set("Content-Type", "application/json")
			if apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			}
			for k, v := range headers {
				req.Header.Set(k, v)
			}

			client := &http.Client{Timeout: 600 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				wf.phaseError(err.Error())
				return agentResultMsg{reply: mockReply{
					text:  parseCLIError("custom", "", err),
					items: []activityItem{{tool: "custom", label: "request failed", status: "error", detail: err.Error()}},
				}, run: run}
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				wf.phaseError(fmt.Sprintf("HTTP %d", resp.StatusCode))
				return agentResultMsg{reply: mockReply{
					text:  parseCLIError("custom", string(respBody), nil),
					items: []activityItem{{tool: "custom", label: fmt.Sprintf("HTTP %d", resp.StatusCode), status: "error", detail: string(respBody[:min(100, len(respBody))])}},
				}, run: run}
			}

			wf.phaseReceiving()
			text, reasoning, tok, err := parseZenResponse(respBody)
			if err != nil {
				wf.phaseError(err.Error())
				return agentResultMsg{reply: mockReply{
					text:  parseCLIError(m.provider, string(respBody), err),
					items: []activityItem{{tool: m.provider, label: "parse error", status: "error", detail: err.Error()}},
				}, run: run}
			}

			elapsed := time.Since(startTime)
			if text != "" {
				var col *collapse
				if reasoning != "" {
					col = &collapse{
						summary: "thinking trace (" + fmt.Sprintf("%d chars", len(reasoning)) + ")",
						content: reasoning,
					}
				}
				attr := fmt.Sprintf("\n\n  ⚡ %s/%s · %.1fs · %s tokens", m.provider, customModel, elapsed.Seconds(), fmtTokens(tok.total))
				text += attr
				wf.phaseDone(elapsed, fmt.Sprintf("%d tokens", tok.total))
				reply := mockReply{
					text:     text,
					collapse: col,
					items: []activityItem{{
						tool: m.provider, label: fmt.Sprintf("%s %s", m.provider, customModel), status: "ok",
						detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), tok.total),
					}},
				}
				return agentResultMsg{reply: reply, tokens: tok, run: run}
			}
		}

		// Fallback mock pipeline — a scripted interactive agent run that shows
		// the full UX: live tool entries in the chat log, a subagent spawn,
		// and a question back to the user (↑↓ choose / type custom). The
		// answer continues the run and shapes the final reply.
		return mockAgentRun(m, q, traceCh, askCh, answerCh, startTime, run)
	}
}

// mockAgentRun simulates a multi-step coding-agent turn so the TUI's live
// process log and interactive question UX are demonstrable without a real
// tool-calling model. It streams dimmed tool entries, spawns a mock subagent,
// asks the user how to continue, then finishes with a tailored reply.
func mockAgentRun(m Model, q string, traceCh chan<- agentTraceMsg, askCh chan<- agentAskMsg, answerCh <-chan string, startTime time.Time, run int) tea.Msg {
	const tick = agentLatency / 5

	steps := []struct {
		phase, line string
	}{
		{"thinking…", "→ analyze prompt: " + clip(q, 48)},
		{"reading files…", "→ read internal/tui/app.go"},
		{"searching files…", "✱ glob **/*.go"},
		{"searching files…", "→ grep \"agentWorkCmd\" internal/tui/"},
		{"running command…", "❯ bash go test ./internal/tui/"},
	}
	for _, s := range steps {
		sendPhase(traceCh, s.phase, s.line)
		time.Sleep(tick)
	}

	// Spawn a mock subagent — visible in the right panel agents section.
	sendPhase(traceCh, "spawning subagent…", "(spawn @finder) → locate symbols & dependency tree")
	time.Sleep(tick)

	// Ask the user — the run pauses until they answer or cancel. The popover
	// supports MULTIPLE questions (radio + checkbox) in one go.
	askCh <- agentAskMsg{
		title: "agent needs your input",
		questions: []askQuestion{
			{
				header:   "How to proceed",
				question: "Task completed. How to proceed?",
				options: []string{
					"Show answer summary",
					"Show change diff",
					"Run tests & report results",
					"Jelaskan detail implementasi",
				},
			},
			{
				header:      "Extras",
				question:    "What else to include? (pick any)",
				options:     []string{"Trace log", "Token usage", "Next-step suggestions"},
				multiSelect: true,
			},
		},
		run: run,
	}
	answer := <-answerCh
	if answer == "" {
		// Cancelled by the user (esc) — the run is aborted and its late
		// result will be dropped by the run/abort guard in Update.
		return agentResultMsg{reply: mockReply{
			text:  "interrupted — no answer submitted",
			items: []activityItem{{tool: "system", label: "question cancelled", status: "error", detail: "esc"}},
		}, run: run}
	}

	sendPhase(traceCh, "processing answer…", "→ answer diterima: "+clip(answer, 48))
	time.Sleep(tick)

	// Slash commands keep their curated replies; free-form prompts go through
	// the session-memory path (recalls prior turns — no more "sesi baru").
	var reply mockReply
	if strings.HasPrefix(q, "/") {
		reply = buildReply(q, m.index)
	} else {
		reply = conversationalReply(q, m.index, m.chat)
	}
	reply.text = "Your choice: " + answer + "\n\n" + reply.text
	reply.items = prependActivity(reply.items,
		activityItem{tool: "search", label: "grep agentWorkCmd", status: "done", detail: "3 hits"},
		activityItem{tool: "bash", label: "go test ./internal/tui/", status: "done", detail: "all pass"},
	)
	reply.subagents = []subagentState{{name: "finder", task: "locate symbols", status: "done"}}

	elapsed := time.Since(startTime)
	reply.text += fmt.Sprintf("\n\n  ⚡ mock/pipeline · %.1fs", elapsed.Seconds())
	reply.items = append(reply.items, activityItem{tool: "system", label: "mock pipeline", status: "ok", detail: fmt.Sprintf("%.1fs", elapsed.Seconds())})
	return agentResultMsg{reply: reply, run: run}
}

// freebuffNativeAgentRun executes Freebuff via native API with saved credentials.
// Follows the unified provider workflow: thinking → processing → receiving → done.
func freebuffNativeAgentRun(m Model, q string, traceCh chan<- agentTraceMsg, askCh chan<- agentAskMsg, answerCh <-chan string, startTime time.Time, cancel *func(), run int) tea.Msg {
	selectedMod := m.selectedModel
	if selectedMod == "" {
		selectedMod = freebuffNativeModels[0]
	}

	wf := newWorkflow(traceCh, "freebuff", selectedMod, startTime)

	// Phase 1: thinking
	wf.phaseThinking()

	// Load credentials
	authToken := loadManicodeCredentials()
	if authToken == "" {
		return agentResultMsg{reply: mockReply{
			text:  "No Freebuff credentials found.\n\nPlease install and login first:\n  npm i -g freebuff\n  freebuff",
			items: []activityItem{{tool: "freebuff", label: "credentials missing", status: "error", detail: "no auth token"}},
		}, run: run}
	}

	// Phase 2: processing
	wf.phaseProcessing()

	// Execute native API call
	response, err := freebuffNativeChat(authToken, selectedMod, q)
	if err != nil {
		wf.phaseError(err.Error())
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("freebuff", err.Error(), nil),
			items: []activityItem{{tool: "freebuff", label: "freebuff API failed", status: "error", detail: err.Error()}},
		}, run: run}
	}

	elapsed := time.Since(startTime)
	respText := strings.TrimSpace(response)

	if respText == "" {
		wf.phaseError("empty response")
		return agentResultMsg{reply: mockReply{
			text:  "Freebuff returned an empty response.",
			items: []activityItem{{tool: "freebuff", label: "empty response", status: "error", detail: "no output"}},
		}, run: run}
	}

	// Phase 3: receiving response
	wf.phaseReceiving()

	attr := fmt.Sprintf("\n\n  ⚡ freebuff/%s · %.1fs", selectedMod, elapsed.Seconds())
	respText += attr

	// Phase 4: done
	wf.phaseDone(elapsed, fmt.Sprintf("%d chars response", len(respText)))

	reply := mockReply{
		text: respText,
		items: []activityItem{{
			tool: "freebuff", label: fmt.Sprintf("freebuff %s", selectedMod), status: "ok",
			detail: fmt.Sprintf("%.1fs response", elapsed.Seconds()),
		}},
	}
	return agentResultMsg{reply: reply, run: run}
}

// codebuffNativeAgentRun executes Codebuff via native API with saved credentials.
// Follows the unified provider workflow: thinking → processing → receiving → done.
func codebuffNativeAgentRun(m Model, q string, traceCh chan<- agentTraceMsg, askCh chan<- agentAskMsg, answerCh <-chan string, startTime time.Time, cancel *func(), run int) tea.Msg {
	selectedMod := m.selectedModel
	if selectedMod == "" {
		selectedMod = codebuffNativeModels[0]
	}

	wf := newWorkflow(traceCh, "codebuff", selectedMod, startTime)

	// Phase 1: thinking
	wf.phaseThinking()

	// Load credentials (same as freebuff)
	authToken := loadManicodeCredentials()
	if authToken == "" {
		return agentResultMsg{reply: mockReply{
			text:  "No Codebuff credentials found.\n\nPlease install and login first:\n  npm i -g codebuff\n  codebuff",
			items: []activityItem{{tool: "codebuff", label: "credentials missing", status: "error", detail: "no auth token"}},
		}, run: run}
	}

	// Phase 2: processing
	wf.phaseProcessing()

	// Execute native API call (same backend as freebuff)
	response, err := freebuffNativeChat(authToken, selectedMod, q)
	if err != nil {
		wf.phaseError(err.Error())
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("codebuff", err.Error(), nil),
			items: []activityItem{{tool: "codebuff", label: "codebuff API failed", status: "error", detail: err.Error()}},
		}, run: run}
	}

	elapsed := time.Since(startTime)
	respText := strings.TrimSpace(response)

	if respText == "" {
		wf.phaseError("empty response")
		return agentResultMsg{reply: mockReply{
			text:  "Codebuff returned an empty response.",
			items: []activityItem{{tool: "codebuff", label: "empty response", status: "error", detail: "no output"}},
		}, run: run}
	}

	// Phase 3: receiving response
	wf.phaseReceiving()

	attr := fmt.Sprintf("\n\n  ⚡ codebuff/%s · %.1fs", selectedMod, elapsed.Seconds())
	respText += attr

	// Phase 4: done
	wf.phaseDone(elapsed, fmt.Sprintf("%d chars response", len(respText)))

	reply := mockReply{
		text: respText,
		items: []activityItem{{
			tool: "codebuff", label: fmt.Sprintf("codebuff %s", selectedMod), status: "ok",
			detail: fmt.Sprintf("%.1fs response", elapsed.Seconds()),
		}},
	}
	return agentResultMsg{reply: reply, run: run}
}

// parseCLIError categorizes raw CLI stderr into clear, actionable user messages
// for rate limits, auth failures, quota limits, and network errors.
// Handles provider-specific errors for Freebuff, Codebuff, OpenCode, etc.
func parseCLIError(providerName, rawErr string, execErr error) string {
	lower := strings.ToLower(rawErr)

	// Provider-specific error messages
	switch {
	case providerName == "freebuff":
		if strings.Contains(lower, "session") && strings.Contains(lower, "limit") {
			return fmt.Sprintf("⚠️ [Freebuff Session Limit]\n\nFreebuff free tier is limited to 6 sessions/hour.\n👉 Solution: Wait ~10 min for session reset, or switch provider via /connect")
		}
		if strings.Contains(lower, "not found") || strings.Contains(lower, "command not found") {
			return fmt.Sprintf("❌ [Freebuff Not Installed]\n\nFreebuff CLI not found.\n👉 Solution: npm install -g freebuff && freebuff")
		}
		if strings.Contains(lower, "session_model_mismatch") {
			return fmt.Sprintf("⚠️ [Freebuff Model Mismatch]\n\nModel unavailable for this session.\n👉 Solution: Auto-fallback to available model")
		}
		if strings.Contains(lower, "unauthorized") || strings.Contains(lower, "401") {
			return fmt.Sprintf("🔑 [Freebuff Auth Error]\n\nInvalid or expired credentials.\n👉 Solution: Run freebuff to re-login, then /connect again")
		}
		if strings.Contains(lower, "rate limit") || strings.Contains(lower, "429") {
			return fmt.Sprintf("⚠️ [Freebuff Rate Limit]\n\nToo many requests.\n👉 Solution: Wait a few minutes before retrying")
		}
	case providerName == "codebuff":
		if strings.Contains(lower, "rate limit") || strings.Contains(lower, "429") {
			return fmt.Sprintf("⚠️ [Codebuff Rate Limit]\n\nServer is rate-limiting requests.\n👉 Solution: Wait a few minutes or use another model via `/models`.")
		}
		if strings.Contains(lower, "session limit") || strings.Contains(lower, "5 sessions") {
			return fmt.Sprintf("⚠️ [Codebuff Session Limit]\n\nFree tier is limited to 5 sessions/hour.\n👉 Solution: Wait for the next session or use another provider via `/connect`.")
		}
	case providerName == "opencode":
		if strings.Contains(lower, "free limit") || strings.Contains(lower, "quota") {
			return fmt.Sprintf("⚠️ [OpenCode Free Limit]\n\nFree tier limit reached.\n👉 Solution: Use another model via `/models` or wait for reset.")
		}
	}

	// Generic error patterns
	switch {
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "429") || strings.Contains(lower, "too many requests"):
		return fmt.Sprintf("⚠️ [%s Rate Limit Exceeded]\n\nServer is experiencing high queue or call limit reached.\n👉 Solution: Use another model via `/models` (e.g., `mimo-v2.5-free` or `ling-3.0-tiny-free`).", providerName)
	case strings.Contains(lower, "unauthorized") || strings.Contains(lower, "401") || strings.Contains(lower, "auth") || strings.Contains(lower, "login") || strings.Contains(lower, "key"):
		return fmt.Sprintf("🔑 [%s Authentication Required]\n\nProvider authentication is invalid or token has expired.\n👉 Solution: Run `/connect` in brocode to refresh your login session.", providerName)
	case strings.Contains(lower, "quota") || strings.Contains(lower, "exceeded") || strings.Contains(lower, "insufficient"):
		return fmt.Sprintf("💳 [%s Quota Exceeded]\n\nProvider daily/monthly quota limit has been reached.\n👉 Solution: Switch to another provider in `/connect` or choose a free model in `/models`.", providerName)
	case strings.Contains(lower, "connect") || strings.Contains(lower, "network") || strings.Contains(lower, "timeout") || strings.Contains(lower, "offline"):
		return fmt.Sprintf("🌐 [%s Network Error]\n\nFailed to connect to provider server. Check your internet connection.\n👉 Solution: Ensure stable internet connection and try again.", providerName)
	default:
		if rawErr != "" {
			return fmt.Sprintf("❌ [%s Execution Error]\n\n%s", providerName, rawErr)
		}
		if execErr != nil {
			return fmt.Sprintf("❌ [%s Execution Error]\n\n%v", providerName, execErr)
		}
		return fmt.Sprintf("❌ [%s Error]\n\nAn unknown error occurred while contacting the provider.", providerName)
	}
}

// waitForTrace returns a tea.Cmd that relays the next status/trace update from
// the agent goroutine. When the channel closes (run finished) it returns nil.
func (m Model) waitForTrace() tea.Cmd {
	ch := m.traceCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		t, ok := <-ch
		if !ok {
			return nil // channel closed, agent finished
		}
		return agentTraceMsg{phase: t.phase, line: t.line}
	}
}

// waitForAsk relays a pending agent ask (one or more questions). The goroutine
// closes askCh when the run finishes, which returns nil here and stops the
// relay.
func (m Model) waitForAsk() tea.Cmd {
	ch := m.askCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		q, ok := <-ch
		if !ok {
			return nil // run finished without asking
		}
		return agentAskMsg{title: q.title, questions: q.questions, run: q.run}
	}
}
