package context

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/tokens"
)

// CompactionSummary follows Section 3.1 5-heading structured format.
type CompactionSummary struct {
	Goal           string   `json:"goal"`
	FilesTouched   []string `json:"files_touched"`
	DecisionsMade  []string `json:"decisions_made"`
	OpenQuestions  []string `json:"open_questions"`
	LastKnownState string   `json:"last_known_state"`
}

// Format returns the 5-heading markdown representation.
func (cs CompactionSummary) Format() string {
	var sb strings.Builder
	sb.WriteString("## Goal\n" + cs.Goal + "\n\n")

	sb.WriteString("## Files Touched\n")
	for _, f := range cs.FilesTouched {
		sb.WriteString("- " + f + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Decisions Made\n")
	for _, d := range cs.DecisionsMade {
		sb.WriteString("- " + d + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Open Questions / Pending Work\n")
	for _, q := range cs.OpenQuestions {
		sb.WriteString("- " + q + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Last Known State\n" + cs.LastKnownState + "\n")
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
	compactCount int
	model        string // active model name; enables exact BPE token counting
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

// MaxWindow returns context window capacity limit.
func (m *Manager) MaxWindow() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxWindow
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

	if m.store != nil {
		payload, _ := json.Marshal(msg)
		_, err := m.store.AppendEvent(m.sessionID, "assistant_msg", string(payload), tokens)
		return err
	}
	return nil
}

// ImportUserMessage restores a user message into memory (tokens counted)
// WITHOUT re-persisting it to the store. Used when replaying a session's
// events so resuming never duplicates history.
func (m *Manager) ImportUserMessage(content string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, provider.Message{Role: "user", Content: content})
	m.totalTokens += m.estimateTokens(content)
}

// ImportAssistantTurn restores an assistant turn into memory (tokens counted)
// WITHOUT re-persisting it to the store. See ImportUserMessage. mode and model
// carry the turn's original engine mode and model so the restored UI log can
// render the correct badge.
func (m *Manager) ImportAssistantTurn(mode, model, reasoning, content string, toolCalls []provider.ToolCall) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, provider.Message{Role: "assistant", Content: content, Reasoning: reasoning, ToolCalls: toolCalls, Mode: mode, Model: model})
	m.totalTokens += m.estimateTokens(reasoning + content)
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
	m.totalTokens += m.estimateTokens(content)
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

	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == "user" && m.messages[i].ToolCallID == "" {
			return m.messages[i].Content
		}
	}
	return ""
}

// NeedsCompaction checks if token usage exceeds 85% threshold.
func (m *Manager) NeedsCompaction() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalTokens > int(float64(m.maxWindow)*0.85)
}

// Compact performs structured summary compaction.
func (m *Manager) Compact(summary CompactionSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	summaryText := summary.Format()
	systemSummaryMsg := provider.Message{
		Role:    "system",
		Content: "CONTEXT COMPACTED SUMMARY:\n" + summaryText,
	}

	// Keep last 4 messages (active subgoal / recent tool context)
	keepCount := 4
	if len(m.messages) < keepCount {
		keepCount = len(m.messages)
	}
	tail := m.messages[len(m.messages)-keepCount:]

	m.messages = append([]provider.Message{systemSummaryMsg}, tail...)
	m.compactCount++

	// Recalculate tokens
	newTokens := m.estimateTokens(summaryText)
	for _, msg := range tail {
		newTokens += m.estimateTokens(msg.Content + msg.Reasoning)
	}
	m.totalTokens = newTokens

	if m.store != nil {
		payload, _ := json.Marshal(summary)
		_, err := m.store.AppendEvent(m.sessionID, "compaction_summary", string(payload), newTokens)
		return err
	}

	return nil
}

// TruncateToolOutput applies Section 3.1 Prevention Strategy.
func TruncateToolOutput(content string, maxChars int) string {
	if len(content) <= maxChars {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 50 {
		head := strings.Join(lines[:40], "\n")
		return fmt.Sprintf("%s\n\n[showing top 40/%d lines, refine query or ask for specific section]", head, len(lines))
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
	return tokens.EstimateTokens(text)
}

// EstimateTokens approximates LLM token counts cheaply and deterministically
// (weighted per line: prose ≈4 chars/token, code ≈3.5, CJK ≈1.2). Kept here
// as a thin wrapper over the leaf tokens package so existing external callers
// and the compaction thresholds stay unchanged.
func EstimateTokens(text string) int {
	return tokens.EstimateTokens(text)
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
