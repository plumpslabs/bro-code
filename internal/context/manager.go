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

	tokens := EstimateTokens(content)
	m.totalTokens += tokens

	if m.store != nil {
		payload, _ := json.Marshal(msg)
		_, err := m.store.AppendEvent(m.sessionID, "user_msg", string(payload), tokens)
		return err
	}
	return nil
}

// AppendAssistantTurn adds assistant reasoning, answer content, and tool calls.
func (m *Manager) AppendAssistantTurn(reasoning, content string, toolCalls []provider.ToolCall) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	msg := provider.Message{
		Role:      "assistant",
		Content:   content,
		Reasoning: reasoning,
		ToolCalls: toolCalls,
	}
	m.messages = append(m.messages, msg)

	tokens := EstimateTokens(reasoning + content)
	for _, tc := range toolCalls {
		tokens += EstimateTokens(tc.Name + tc.Arguments)
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
	m.totalTokens += EstimateTokens(content)
}

// ImportAssistantTurn restores an assistant turn into memory (tokens counted)
// WITHOUT re-persisting it to the store. See ImportUserMessage.
func (m *Manager) ImportAssistantTurn(reasoning, content string, toolCalls []provider.ToolCall) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, provider.Message{Role: "assistant", Content: content, Reasoning: reasoning, ToolCalls: toolCalls})
	m.totalTokens += EstimateTokens(reasoning + content)
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
	m.totalTokens += EstimateTokens(content)
}

// AppendToolResult adds a tool execution result to the context.
func (m *Manager) AppendToolResult(toolCallID, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Prevention: limit raw tool output length to prevent context stuff bloating
	content = TruncateToolOutput(content, 4000)

	msg := provider.Message{
		Role:       "user",
		Content:    content,
		ToolCallID: toolCallID,
	}
	m.messages = append(m.messages, msg)

	tokens := EstimateTokens(content)
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
	newTokens := EstimateTokens(summaryText)
	for _, msg := range tail {
		newTokens += EstimateTokens(msg.Content + msg.Reasoning)
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

// estimateTokens approximates LLM token counts more accurately than a flat
// len/4. Rough per-token character densities: English prose ≈4 chars/token,
// EstimateTokens approximates LLM token counts cheaply and deterministically
// (weighted per line: prose ≈4 chars/token, code ≈3.5, CJK ≈1.2). Kept here
// as a thin wrapper over the leaf tokens package so existing callers and the
// compaction thresholds stay unchanged.
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
	for _, ev := range events {
		if m.TotalTokens() > int(float64(m.MaxWindow())*0.8) && restored > 0 {
			skipped = len(events) - restored
			break
		}

		// Parse the payload so assistant turns keep their real structure
		// instead of being rendered as raw JSON — a tool-call-only turn has
		// empty Content, and the ExtractEventContent fallback would dump the
		// whole payload into the history and the LLM context.
		var msg provider.Message
		_ = json.Unmarshal([]byte(ev.PayloadJSON), &msg)

		switch ev.Type {
		case "user_msg":
			text := msg.Content
			if text == "" {
				text = ExtractEventContent(ev.PayloadJSON)
			}
			m.ImportUserMessage(text)
			if isEngineReminder(text) {
				display = append(display, "⚙️ "+text)
			} else {
				display = append(display, "YOU:\n"+text)
			}
		case "assistant_msg":
			m.ImportAssistantTurn(msg.Reasoning, msg.Content, msg.ToolCalls)
			if strings.TrimSpace(msg.Content) != "" {
				display = append(display, "BROCODE:\n"+msg.Content)
			} else if len(msg.ToolCalls) > 0 {
				display = append(display, "BROCODE: 🔧 "+toolCallSummary(msg.ToolCalls))
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
	if skipped > 0 {
		display = append(display, fmt.Sprintf("💾 Restored the %d most recent events; %d older events omitted to stay within the context window.", restored, skipped))
	}
	return display
}

// isEngineReminder reports whether a persisted user_msg was injected by the
// engine (loop guard, tool budget, verification failure) rather than typed by
// the user. Such messages must be restored for the model's context but should
// not be displayed as if the user had said them.
func isEngineReminder(text string) bool {
	for _, prefix := range []string{
		"⚠️ You have been calling tools",
		"⚠️ [LOOP GUARD]",
		"Level 1 verification check failed:",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// toolCallSummary renders a compact, human-readable list of tool calls for
// resumed history instead of dumping the raw arguments JSON.
func toolCallSummary(calls []provider.ToolCall) string {
	names := make([]string, 0, len(calls))
	for _, tc := range calls {
		names = append(names, tc.Name)
	}
	return strings.Join(names, " → ")
}
