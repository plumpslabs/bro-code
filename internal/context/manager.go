package context

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/tokens"
)

// FileChangesFormatter formats a serialized JSON list of file changes into a
// display string during RestoreSession. Wired by the tool package on startup.
var FileChangesFormatter func(payloadJSON string) string

// FileChangesRestorer restores a serialized JSON list of file changes into individual
// live DIFF: entries during RestoreSession, matching the live turn layout.
var FileChangesRestorer func(payloadJSON string) []string

// CompactionSummary follows the structured 6-heading format used for context
// compaction. The six headings (Goal, Files Touched, Decisions Made, Next
// Action, Constraints, Last Known State) give a continuing agent everything it
// needs without re-reading the dropped transcript — Next Action and Constraints
// are the two that most reduce "lost the thread" regressions after compaction.
type CompactionSummary struct {
	Goal           string   `json:"goal"`
	FilesTouched   []string `json:"files_touched"`
	DecisionsMade  []string `json:"decisions_made"`
	NextAction     string   `json:"next_action"`
	Constraints    string   `json:"constraints"`
	OpenQuestions  []string `json:"open_questions"`
	LastKnownState string   `json:"last_known_state"`
}

// Format returns the 5-heading markdown representation.
func (cs CompactionSummary) Format() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Goal\n%s\n\n", cs.Goal)

	sb.WriteString("## Files Touched\n")
	for _, f := range cs.FilesTouched {
		fmt.Fprintf(&sb, "- %s\n", f)
	}
	sb.WriteString("\n")

	sb.WriteString("## Decisions Made\n")
	for _, d := range cs.DecisionsMade {
		fmt.Fprintf(&sb, "- %s\n", d)
	}
	sb.WriteString("\n")

	fmt.Fprintf(&sb, "## Next Action\n%s\n\n", cs.NextAction)
	fmt.Fprintf(&sb, "## Constraints\n%s\n\n", cs.Constraints)

	sb.WriteString("## Open Questions / Pending Work\n")
	for _, q := range cs.OpenQuestions {
		fmt.Fprintf(&sb, "- %s\n", q)
	}
	sb.WriteString("\n")

	fmt.Fprintf(&sb, "## Last Known State\n%s\n", cs.LastKnownState)
	return sb.String()
}

// Manager maintains the live message context, token estimation, and event persistence.
type Manager struct {
	mu           sync.Mutex
	sessionID    string
	store        *store.Store
	messages     []provider.Message
	totalTokens  int
	maxWindow    int
	// systemPromptTokens is the estimated size of the per-turn system prompt
	// (repo map + warm-start memory + tool definitions). It is NOT part of the
	// conversation messages but IS sent with every request, so it must count
	// toward the context budget — otherwise the real wire request (system
	// prompt + messages) can exceed the model window even when the conversation
	// alone is under the cap. Set by the engine each turn via
	// SetSystemPromptTokens.
	systemPromptTokens int
	compactCount       int
	model              string // active model name; enables exact BPE token counting
	// Cumulative token breakdown by message kind (session-scoped, for the
	// metrics HUD). Unlike totalTokens these are NEVER reduced by compaction:
	// they count what was APPENDED this session, so user+assistant+tool can
	// exceed the live window after a compact. Tool output is counted AFTER the
	// truncate-and-pointer digest, i.e. the tokens actually re-sent each round.
	userTokens    int
	asstTokens    int
	toolTokens    int
	// lastPromptHash stores a short hash of the last user prompt so the engine
	// can detect when a new user message is completely unrelated to the ongoing
	// conversation (stale context). When detected, partial context reset is
	// triggered instead of full-clear, preserving session metadata.
	lastPromptHash string
}

// NewManager creates a context manager connected to SQLite store.
func NewManager(sessionID string, st *store.Store, maxTokens int) *Manager {
	if maxTokens <= 0 {
		maxTokens = 128000 // default context window size
	}
	return &Manager{
		sessionID: sessionID,
		store:     st,
		maxWindow: maxTokens,
	}
}

// promptHash returns a 16-char hex prefix of the SHA-256 of the prompt.
// Used for stale-context detection.
func promptHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// IsStaleContext returns true (and the old hash) if the new prompt is
// semantically unrelated to the current conversation. "Stale" = the user
// asked something completely different from the ongoing task. Detects this
// by comparing keyword overlap: if <30% of significant words in the new
// prompt appeared in the current conversation, the context is considered stale.
// The engine uses this to trigger a partial context reset (keep the pinned
// goal, drop the working trail) instead of charging ahead into a wrong
// direction.
func (m *Manager) IsStaleContext(newPrompt string) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lastPromptHash == "" {
		return false, ""
	}

	oldHash := m.lastPromptHash
	// Extract significant words from the new prompt (lenient tokenizer).
	newWords := extractSignificantWords(newPrompt)
	if len(newWords) == 0 {
		return false, ""
	}

	// Gather words from current messages.
	currWords := make(map[string]struct{}, len(newWords)*2)
	for _, msg := range m.messages {
		for w := range extractSignificantWords(msg.Content) {
			currWords[w] = struct{}{}
		}
	}

	overlap := 0
	for w := range newWords {
		if _, ok := currWords[w]; ok {
			overlap++
		}
	}

	ratio := float64(overlap) / float64(len(newWords))
	// <0.30 overlap → semantically different task → stale context.
	return ratio < 0.30, oldHash
}

// extractSignificantWords pulls lowercased tokens of length >3 from text,
// filtering common stop-words. Cheap approximate semantic overlap.
func extractSignificantWords(text string) map[string]bool {
	stop := map[string]bool{
		"this": true, "that": true, "with": true, "have": true, "from": true,
		"will": true, "want": true, "need": true, "what": true, "when": true,
		"where": true, "were": true, "they": true, "them": true,
		"does": true, "been": true, "their": true,
	}
	words := strings.Fields(strings.ToLower(text))
	out := make(map[string]bool)
	for _, w := range words {
		// strip punctuation
		w = strings.TrimFunc(w, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9')
		})
		if len(w) > 3 && !stop[w] {
			out[w] = true
		}
	}
	return out
}

// ResetStaleContext performs a PARTIAL context reset: drops the message
// history but preserves session ID, model, and token counters. After this,
// only the new prompt remains, so the next LLM call sees a clean slate
// without losing session continuity.
func (m *Manager) ResetStaleContext(newPrompt string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = []provider.Message{
		{Role: "user", Content: newPrompt},
	}
	m.totalTokens = m.estimateTokens(newPrompt)
	m.userTokens = m.totalTokens
	m.asstTokens = 0
	m.toolTokens = 0
	m.compactCount = 0
	m.lastPromptHash = promptHash(newPrompt)
}

// SetModel records the active model name so token estimation can use the
// exact BPE tokenizer for that model's encoding (falls back to the heuristic
// char-count estimator when the model is unset or the tokenizer is unavailable).
func (m *Manager) SetModel(model string) {
	m.mu.Lock()
	m.model = model
	m.mu.Unlock()
}

// SessionID returns active session ID.
func (m *Manager) SessionID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionID
}

// TotalTokens returns accumulated context tokens.
func (m *Manager) TotalTokens() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalTokens
}

// Len returns the number of messages currently in context.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.messages)
}

// MaxWindow returns context window capacity limit.
func (m *Manager) MaxWindow() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxWindow
}

// CompactCount returns how many compactions this conversation has performed
// (session-level metric, also surfaced per-turn via Engine.turnCompactions).
func (m *Manager) CompactCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.compactCount
}

// ResetCompactCount zeroes the session compaction counter.
func (m *Manager) ResetCompactCount() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.compactCount = 0
}

// SetMaxWindow updates the context window capacity. Used when the active
// model switches to one with a different declared context limit (from the
// provider config's per-model limit block). Non-positive values are ignored.
func (m *Manager) SetMaxWindow(max int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if max > 0 {
		m.maxWindow = max
	}
}


// Store returns connected SQLite store.
func (m *Manager) Store() *store.Store {
	return m.store
}

// AppendUserMessage adds a user message to the context and store.
func (m *Manager) AppendUserMessage(content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	msg := provider.Message{
		Role:    "user",
		Content: content,
	}
	m.messages = append(m.messages, msg)

	tokens := m.estimateTokens(content)
	m.totalTokens += tokens
	m.userTokens += tokens

	// Record prompt hash for stale-context detection.
	m.lastPromptHash = promptHash(content)

	if m.store != nil {
		payload, _ := json.Marshal(msg)
		_, err := m.store.AppendEvent(m.sessionID, "user_msg", string(payload), tokens)
		return err
	}
	return nil
}

// AppendAssistantTurn adds assistant reasoning, answer content, and tool calls.
// mode and model stamp the turn's origin (engine mode + active model) so a
// persisted session can restore each answer with its original badge/label.
func (m *Manager) AppendAssistantTurn(mode, model, reasoning, content string, toolCalls []provider.ToolCall) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	msg := provider.Message{
		Role:      "assistant",
		Content:   content,
		Reasoning: reasoning,
		ToolCalls: toolCalls,
		Mode:      mode,
		Model:     model,
	}
	m.messages = append(m.messages, msg)

	tokens := m.estimateTokens(reasoning + content)
	for _, tc := range toolCalls {
		tokens += m.estimateTokens(tc.Name + tc.Arguments)
	}
	m.totalTokens += tokens
	m.asstTokens += tokens

	if m.store != nil {
		payload, _ := json.Marshal(msg)
		_, err := m.store.AppendEvent(m.sessionID, "assistant_msg", string(payload), tokens)
		return err
	}
	return nil
}

// AppendSystemNote records a UI/informational message (slash-command output
// such as /help or /diagnose) to the session store so it survives a -c resume.
// These are not part of the LLM conversation — only the visible chat history —
// which is why the engine's AppendUserMessage/AppendAssistantTurn do not cover
// them and they previously vanished when a session was reloaded.
func (m *Manager) AppendSystemNote(content string) error {
	if m.store == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	payload, _ := json.Marshal(provider.Message{Role: "system", Content: content})
	_, err := m.store.AppendEvent(m.sessionID, "system_msg", string(payload), 0)
	return err
}

// AppendFileDiff records a live per-file diff at the exact moment an edit lands,
// preserving the true chronological flow in the session history so -c resume
// never dumps diffs at the bottom of the conversation.
func (m *Manager) AppendFileDiff(path, diff string) error {
	if m.store == nil || path == "" || diff == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	payload, _ := json.Marshal(map[string]string{
		"path": path,
		"diff": diff,
	})
	_, err := m.store.AppendEvent(m.sessionID, "file_diff", string(payload), 0)
	return err
}

// ImportUserMessage restores a user message into memory (tokens counted)
// WITHOUT re-persisting it to the store. Used when replaying a session's
// events so resuming never duplicates history.
func (m *Manager) ImportUserMessage(content string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, provider.Message{Role: "user", Content: content})
	t := m.estimateTokens(content)
	m.userTokens += t
	m.totalTokens += t
}

// ImportAssistantTurn restores an assistant turn into memory (tokens counted)
// WITHOUT re-persisting it to the store. See ImportUserMessage. mode and model
// carry the turn's original engine mode and model so the restored UI log can
// render the correct badge.
func (m *Manager) ImportAssistantTurn(mode, model, reasoning, content string, toolCalls []provider.ToolCall) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, provider.Message{Role: "assistant", Content: content, Reasoning: reasoning, ToolCalls: toolCalls, Mode: mode, Model: model})
	t := m.estimateTokens(reasoning + content)
	m.asstTokens += t
	m.totalTokens += t
}

// ImportToolResult restores a tool result into memory (tokens counted)
// WITHOUT re-persisting it to the store. Used when replaying a session's
// events so resuming never duplicates history. Keeps the assistant tool_calls
// → tool result pairing intact for providers that require it.
func (m *Manager) ImportToolResult(toolCallID, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, provider.Message{
		Role:       "user",
		Content:    content,
		ToolCallID: toolCallID,
	})
	t := m.estimateTokens(content)
	m.toolTokens += t
	m.totalTokens += t
}

// maxToolResultContextChars caps how much of each tool result is kept in the
// persistent context. The model still receives the full result (up to the
// tool layer's CapOutput, ~40k) in the round it runs the tool — this smaller
// recap is what gets RE-SENT on every later loop iteration. Keeping it lean
// directly cuts the per-round token burn of multi-round agentic turns (a
// 20-round exploration re-sends each recap 19 times).
const maxToolResultContextChars = 2500

// AppendToolResult adds a tool execution result to the context.
func (m *Manager) AppendToolResult(toolCallID, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Prevention: limit raw tool output length to prevent context stuff bloating
	content = TruncateToolOutput(content, maxToolResultContextChars)

	msg := provider.Message{
		Role:       "user",
		Content:    content,
		ToolCallID: toolCallID,
	}
	m.messages = append(m.messages, msg)

	tokens := m.estimateTokens(content)
	m.totalTokens += tokens
	m.toolTokens += tokens

	if m.store != nil {
		payload, _ := json.Marshal(msg)
		_, err := m.store.AppendEvent(m.sessionID, "tool_result", string(payload), tokens)
		return err
	}
	return nil
}

// Messages returns a copy of current active messages for the LLM call.
func (m *Manager) Messages() []provider.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	cp := make([]provider.Message, len(m.messages))
	copy(cp, m.messages)
	return cp
}

// LastUserPrompt returns the content of the most recent user prompt in context.
func (m *Manager) LastUserPrompt() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range slices.Backward(m.messages) {
		if m.messages[i].Role == "user" && m.messages[i].ToolCallID == "" {
			return m.messages[i].Content
		}
	}
	return ""
}

// SetSystemPromptTokens records the estimated size of the per-turn system
// prompt so the context budget accounts for it (see systemPromptTokens).
func (m *Manager) SetSystemPromptTokens(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.systemPromptTokens = n
}

// TotalContextTokens is the true on-wire token cost of a request: the
// conversation messages plus the system prompt that accompanies every call.
func (m *Manager) TotalContextTokens() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalTokens + m.systemPromptTokens
}

// defaultCompactionRatio is the fraction of the context window at which
// compaction kicks in. 0.60 (not 0.85) on purpose: research on context
// engineering shows quality and reliability degrade well before the window is
// "full" (the "context rot" effect), and compacting earlier keeps each turn's
// working set small and high-signal. The hard fitMessages() guard still protects
// against any true overflow regardless of this ratio. The ratio is adaptive: the
// learn package nudges it over sessions (see SetCompactionRatio) to keep context
// utilization in the high-signal band instead of a fixed guess.
const defaultCompactionRatio = 0.60

// compactionRatio is the live, adaptive trigger threshold. It starts at
// defaultCompactionRatio and is overwritten by SetCompactionRatio when the
// self-improving learner has tuned it from observed utilization.
var compactionRatio = defaultCompactionRatio

// SetCompactionRatio overrides the adaptive compaction trigger (0 < r <= 0.95).
// The learn package calls this each turn so the threshold converges to the
// project's actual usage pattern over time.
func SetCompactionRatio(r float64) {
	if r <= 0 {
		r = defaultCompactionRatio
	}
	if r > 0.95 {
		r = 0.95
	}
	compactionRatio = r
}

// TokenBreakdown returns the cumulative tokens appended this session by kind:
// user messages, assistant turns (reasoning + content + tool-call schemas), and
// tool output digests. Cumulative by design — not reduced by compaction — so it
// answers "where did this session's tokens go?" (the metrics question), not
// "how full is the window now?" (TotalTokens).
func (m *Manager) TokenBreakdown() (user, assistant, tool int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.userTokens, m.asstTokens, m.toolTokens
}

// NeedsCompaction checks if token usage exceeds compactionRatio of the window.
// The budget includes the system prompt, so compaction triggers before the real
// request (system prompt + messages) can overflow the model's context window.
func (m *Manager) NeedsCompaction() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Inline the total rather than calling TotalContextTokens(): that helper
	// re-locks m.mu and this method already holds it (non-reentrant mutex).
	total := m.totalTokens + m.systemPromptTokens
	return total > int(float64(m.maxWindow)*compactionRatio)
}

// pinnedGoalMaxChars caps how much of the first user message is pinned through
// compaction. The GOAL/instruction is the pinned core, not a giant pasted error
// or log dump — so only the leading instruction-bearing part is kept verbatim.
const pinnedGoalMaxChars = 1500

// Compact performs structured summary compaction.
//
// Goal-pinning: the FIRST user message carries the turn's goal and constraints.
// Summarizing it away is the documented compaction failure (governance decay —
// constraint loss jumps from 0% to 30-59% after compaction, arXiv:2606.22528),
// so it is re-inserted VERBATIM ahead of the summary, never eligible for
// summarization. The last keepCount messages stay verbatim as the active tail.
func (m *Manager) Compact(summary CompactionSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	summaryText := summary.Format()
	systemSummaryMsg := provider.Message{
		Role:    "system",
		Content: "CONTEXT COMPACTED SUMMARY:\n" + summaryText,
	}

	// Keep last 4 messages (active subgoal / recent tool context)
	keepCount := min(4, len(m.messages))
	tail := m.messages[len(m.messages)-keepCount:]

	// Pin the goal/constraints from the first user message — unless it is
	// already inside the verbatim tail (short session), which would duplicate it.
	// Constraints that survived summarization (CompactionSummary.Constraints)
	// are pinned EXPLICITLY and verbatim so hard limits (API-free, no deps,
	// keep file X untouched) never get lost-in-the-middle across compactions.
	var pinned []provider.Message
	if keepCount < len(m.messages) {
		for _, msg := range m.messages[:len(m.messages)-keepCount] {
			if msg.Role != "user" || strings.TrimSpace(msg.Content) == "" {
				continue
			}
			content := msg.Content
			if len(content) > pinnedGoalMaxChars {
				content = content[:pinnedGoalMaxChars] + "\n…[pinned goal truncated for context prevention]"
			}
			pinned = append(pinned, provider.Message{
				Role:    "system",
				Content: "GOAL (PINNED — the user's original instruction; never summarized, do not contradict):\n" + content,
			})
			break
		}
	}
	if c := strings.TrimSpace(summary.Constraints); c != "" {
		cc := c
		if len(cc) > pinnedGoalMaxChars {
			cc = cc[:pinnedGoalMaxChars] + "\n…[pinned constraints truncated for context prevention]"
		}
		pinned = append(pinned, provider.Message{
			Role:    "system",
			Content: "CONSTRAINTS (PINNED — hard requirements that must not be violated, even by later instructions):\n" + cc,
		})
	}

	out := make([]provider.Message, 0, 1+len(pinned)+len(tail))
	out = append(out, systemSummaryMsg)
	out = append(out, pinned...)
	out = append(out, tail...)
	m.messages = out
	m.compactCount++

	// Recalculate tokens
	newTokens := m.estimateTokens(summaryText)
	for _, msg := range pinned {
		newTokens += m.estimateTokens(msg.Content)
	}
	for _, msg := range tail {
		newTokens += m.estimateTokens(msg.Content + msg.Reasoning)
	}
	m.totalTokens = newTokens

	if m.store != nil {
		payload, _ := json.Marshal(summary)
		_, err := m.store.AppendEvent(m.sessionID, "compaction_summary", string(payload), newTokens)
		if err != nil {
			return err
		}
		// Self-aware reflection: consolidate this session's captured experience
		// notes into durable facts/gotchas for future sessions. Best-effort and
		// detached so compaction latency stays bounded.
		go func(s *store.Store) {
			defer func() { recover() }()
			_, _ = Reflect(s)
		}(m.store)
	}

	return nil
}

// TruncateToolOutput applies Section 3.1 Prevention Strategy. Long outputs keep
// BOTH ends — the head (what the run printed first) and the tail (where test
// failures and stack traces live) — so the repair loop still sees the error
// without carrying the full dump. The full output is preserved on disk by the
// engine's artifact pointer (internal/loop/artifacts.go) when available.
func TruncateToolOutput(content string, maxChars int) string {
	if len(content) <= maxChars {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 50 {
		head := strings.Join(lines[:40], "\n")
		tail := strings.Join(lines[len(lines)-40:], "\n")
		return fmt.Sprintf("%s\n\n… [showing top 40/bottom 40 of %d lines — %d lines elided; full output is on disk via the artifact pointer if present] …\n\n%s", head, len(lines), len(lines)-80, tail)
	}
	return content[:maxChars] + "\n\n[output truncated for context prevention]"
}

// estimateTokens (method) counts tokens for the active model using the exact
// BPE tokenizer when a model is set (Manager.SetModel) and the tokenizer is
// available; otherwise it falls back to the heuristic char-count estimator.
// This keeps context-budget and compaction thresholds accurate for the model
// actually serving the turn. Callers must hold m.mu (it reads m.model directly
// and must not re-lock the non-reentrant mutex).
func (m *Manager) estimateTokens(text string) int {
	if m.model != "" {
		if n := tokens.CountTokens(text, m.model); n > 0 {
			return n
		}
	}
	return tokens.CountTokensDefault(text)
}

// EstimateTokens calculates BPE token counts for text.
func EstimateTokens(text string) int {
	return tokens.CountTokensDefault(text)
}

// ExtractEventContent extracts clean human-readable content from event JSON payload string.
func ExtractEventContent(payloadJSON string) string {
	var msg provider.Message
	if json.Unmarshal([]byte(payloadJSON), &msg) == nil && msg.Content != "" {
		return msg.Content
	}
	var rawStr string
	if json.Unmarshal([]byte(payloadJSON), &rawStr) == nil && rawStr != "" {
		return rawStr
	}
	return payloadJSON
}

// RestoreSession replays a session's stored events into memory WITHOUT
// re-persisting them (so resume / /sessions switch never duplicates history).
// It restores only the newest events that fit ~80% of the context window and
// returns human-readable display lines for the UI log.
//
// Assistant turns keep their real structure (reasoning/content/tool_calls):
// tool-call-only turns render as a compact summary instead of raw JSON, and
// tool results are re-paired with their calls so providers that require the
// tool_calls → result pairing don't break. Engine-injected reminders (loop
// guard, tool budget, verification failures) are restored for the model but
// displayed as ⚙️ system notes, not as if the user had typed them.
func RestoreSession(m *Manager, events []store.Event) []string {
	var display []string
	skipped := 0
	restored := 0
	var pendingTools []string

	flushPendingTools := func() {
		if len(pendingTools) > 0 {
			display = append(display, compactToolSummary(pendingTools))
			pendingTools = nil
		}
	}

	for _, ev := range events {
		if m.TotalTokens() > int(float64(m.MaxWindow())*0.8) && restored > 0 {
			skipped = len(events) - restored
			break
		}

		var msg provider.Message
		_ = json.Unmarshal([]byte(ev.PayloadJSON), &msg)

		switch ev.Type {
		case "user_msg":
			text := msg.Content
			if text == "" {
				text = ExtractEventContent(ev.PayloadJSON)
			}
			m.ImportUserMessage(text)
			if !isEngineReminder(text) {
				flushPendingTools()
				display = append(display, "YOU:\n"+text)
			}
		case "assistant_msg":
			m.ImportAssistantTurn(msg.Mode, msg.Model, msg.Reasoning, msg.Content, msg.ToolCalls)
			for _, tc := range msg.ToolCalls {
				pendingTools = append(pendingTools, tc.Name)
			}
			if strings.TrimSpace(msg.Content) != "" {
				flushPendingTools()
				// Stamp the original mode/model ("BROCODE:PLANNER:poolside/x\n")
				// so the restored answer renders with its true badge. Fall back
				// to the legacy unstamped form for messages saved without them.
				stamp := "BROCODE:\n" + msg.Content
				if msg.Mode != "" {
					stamp = "BROCODE:" + msg.Mode
					if msg.Model != "" {
						stamp += ":" + msg.Model
					}
					stamp += "\n" + msg.Content
				}
				display = append(display, stamp)
			}
		case "tool_result":
			text := msg.Content
			if text == "" {
				text = ExtractEventContent(ev.PayloadJSON)
			}
			m.ImportToolResult(msg.ToolCallID, text)
		case "system_msg":
			text := msg.Content
			if text == "" {
				text = ExtractEventContent(ev.PayloadJSON)
			}
			display = append(display, text)
		case "file_diff":
			flushPendingTools()
			var p struct {
				Path string `json:"path"`
				Diff string `json:"diff"`
			}
			if json.Unmarshal([]byte(ev.PayloadJSON), &p) == nil && p.Path != "" {
				prefix := "DIFF:\n" + p.Path + "\n"
				replaced := false
				for i := len(display) - 1; i >= 0; i-- {
					if strings.HasPrefix(display[i], "YOU:\n") {
						break
					}
					if strings.HasPrefix(display[i], prefix) {
						display[i] = prefix + p.Diff
						replaced = true
						break
					}
				}
				if !replaced {
					display = append(display, prefix+p.Diff)
				}
			}
		case "file_changes":
			// If file_diff events already restored individual per-file diffs in place,
			// skip legacy bulk file_changes to avoid duplicate diff display.
			hasInlineDiff := false
			for _, d := range display {
				if strings.HasPrefix(d, "DIFF:\n") {
					hasInlineDiff = true
					break
				}
			}
			if !hasInlineDiff {
				flushPendingTools()
				if FileChangesRestorer != nil {
					display = append(display, FileChangesRestorer(ev.PayloadJSON)...)
				} else if FileChangesFormatter != nil {
					if formatted := FileChangesFormatter(ev.PayloadJSON); formatted != "" {
						display = append(display, formatted)
					}
				}
			}
		}
		restored++
	}
	flushPendingTools()
	if skipped > 0 {
		display = append(display, fmt.Sprintf("💾 Restored the %d most recent events; %d older events omitted to stay within the context window.", restored, skipped))
	}
	return display
}

// isEngineReminder reports whether a persisted user_msg was injected by the
// engine (loop guard, tool budget, verification failure, synthesis prompt)
// rather than typed by the user. Such messages must be restored for the model's
// context but should not be displayed as if the user had said them.
func isEngineReminder(text string) bool {
	if strings.Contains(text, "⚠️") || strings.Contains(text, "[LOOP GUARD]") || strings.Contains(text, "verification check failed") {
		return true
	}
	return false
}

// IsEngineReminder is the exported form of isEngineReminder, used by callers
// outside this package (e.g. the CLI) that need to filter out engine-injected
// user_msg events when reconstructing user-facing history.
func IsEngineReminder(text string) bool { return isEngineReminder(text) }

// compactToolSummary renders a single compact 1-line process summary for tool calls.
func compactToolSummary(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	counts := make(map[string]int)
	var order []string
	for _, name := range tools {
		if counts[name] == 0 {
			order = append(order, name)
		}
		counts[name]++
	}
	var parts []string
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%s (x%d)", name, counts[name]))
	}
	return fmt.Sprintf("PROCESS:\n⚙️ Process (%d tools executed): %s", len(tools), strings.Join(parts, " · "))
}
