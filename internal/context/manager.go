package context

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
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

	tokens := estimateTokens(content)
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

	tokens := estimateTokens(reasoning + content)
	for _, tc := range toolCalls {
		tokens += estimateTokens(tc.Name + tc.Arguments)
	}
	m.totalTokens += tokens

	if m.store != nil {
		payload, _ := json.Marshal(msg)
		_, err := m.store.AppendEvent(m.sessionID, "assistant_msg", string(payload), tokens)
		return err
	}
	return nil
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

	tokens := estimateTokens(content)
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
	newTokens := estimateTokens(summaryText)
	for _, msg := range tail {
		newTokens += estimateTokens(msg.Content + msg.Reasoning)
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

func estimateTokens(text string) int {
	return len(text) / 4
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
