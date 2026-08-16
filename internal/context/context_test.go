package context

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
)

func TestAppendSystemNotePersistsAndRestores(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	if err := st.CreateSession("sess-note", dir); err != nil {
		t.Fatalf("create session: %v", err)
	}
	mgr := NewManager("sess-note", st, 128000)

	if err := mgr.AppendSystemNote("📖 Commands:\n/help — show commands"); err != nil {
		t.Fatalf("AppendSystemNote: %v", err)
	}

	events, err := st.GetSessionEvents("sess-note")
	if err != nil {
		t.Fatalf("GetSessionEvents: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == "system_msg" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a system_msg event to be persisted, got: %+v", events)
	}

	display := RestoreSession(mgr, events)
	joined := strings.Join(display, "\n")
	if !strings.Contains(joined, "📖 Commands:") {
		t.Errorf("expected system note rendered in restored display, got: %v", joined)
	}
}

func TestTruncateToolOutput(t *testing.T) {
	short := "hello world"
	if TruncateToolOutput(short, 100) != short {
		t.Errorf("expected short output unchanged")
	}

	var sb strings.Builder
	for i := 1; i <= 60; i++ {
		if i > 1 {
			sb.WriteString("\n")
		}
		sb.WriteString("line item long content line item long content")
	}

	truncated := TruncateToolOutput(sb.String(), 200)
	if !strings.Contains(truncated, "[showing top 40/") {
		t.Errorf("expected truncation notice in output, got: %s", truncated)
	}
}

func TestContextManagerImportDoesNotDoubleCount(t *testing.T) {
	mgr := NewManager("test-session", nil, 1000)

	// Append like a normal turn.
	_ = mgr.AppendUserMessage("first prompt")
	_ = mgr.AppendAssistantTurn("BUILDER", "test-model", "thinking...", "first answer", nil)
	before := mgr.TotalTokens()

	// Resume/replay must import without duplicating persistence side effects.
	mgr.ImportUserMessage("first prompt")
	mgr.ImportAssistantTurn("BUILDER", "test-model", "thinking...", "first answer", nil)

	msgs := mgr.Messages()
	if len(msgs) != 4 {
		t.Errorf("expected 4 messages after import, got %d", len(msgs))
	}
	if msgs[2].Role != "user" || msgs[2].Content != "first prompt" {
		t.Errorf("expected imported user message at index 2, got %+v", msgs[2])
	}
	if msgs[3].Role != "assistant" || msgs[3].Content != "first answer" {
		t.Errorf("expected imported assistant message at index 3, got %+v", msgs[3])
	}

	// Importing counts tokens but the store is nil here; the key property is
	// that total tokens grew (so the context window is respected on resume).
	if mgr.TotalTokens() <= before {
		t.Errorf("expected token count to grow after import, before=%d after=%d", before, mgr.TotalTokens())
	}
}

// eventPayload marshals a provider.Message the same way AppendUserMessage /
// AppendAssistantTurn persist events, so the test exercises the real format
// that resume sees in the database.
func eventPayload(msg provider.Message) string {
	b, _ := json.Marshal(msg)
	return string(b)
}

func TestRestoreSessionRendersToolCallsNotRawJSON(t *testing.T) {
	mgr := NewManager("s", nil, 128000)
	events := []store.Event{
		{Type: "user_msg", PayloadJSON: eventPayload(provider.Message{Role: "user", Content: "halo"})},
		{Type: "assistant_msg", PayloadJSON: eventPayload(provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "grep", Arguments: `{"pattern": "inbox"}`},
			{ID: "c2", Name: "read_file", Arguments: `{"path": "a.js"}`},
		}})},
		{Type: "tool_result", PayloadJSON: eventPayload(provider.Message{Role: "user", ToolCallID: "c1", Content: "line 1: inbox\n"})},
		{Type: "assistant_msg", PayloadJSON: eventPayload(provider.Message{Role: "assistant", Content: "ini jawabannya"})},
	}

	display := RestoreSession(mgr, events)

	// The tool-call-only turn must NOT leak raw JSON into the history.
	var sawRawJSON bool
	for _, line := range display {
		if strings.Contains(line, `{"role":"assistant"`) || strings.Contains(line, `"tool_calls"`) {
			sawRawJSON = true
		}
	}
	if sawRawJSON {
		t.Fatalf("raw JSON leaked into restored history: %v", display)
	}

	// The final answer is present in display, tool-only turns are kept for model context.
	joined := strings.Join(display, "\n")
	if !strings.Contains(joined, "ini jawabannya") {
		t.Errorf("expected final answer in history, got: %v", display)
	}

	// Tool result must be re-paired with its call in the message list.
	msgs := mgr.Messages()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 restored messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 2 {
		t.Errorf("assistant tool-call turn not restored with calls: %+v", msgs[1])
	}
	if msgs[2].Role != "user" || msgs[2].ToolCallID != "c1" || !strings.Contains(msgs[2].Content, "inbox") {
		t.Errorf("tool result not re-paired with call c1: %+v", msgs[2])
	}
}

func TestRestoreSessionEngineReminderAndCap(t *testing.T) {
	// Tiny window: only the newest events that fit ~80% survive.
	mgr := NewManager("s", nil, 100)
	var events []store.Event
	for range 30 {
		events = append(events, store.Event{Type: "user_msg", PayloadJSON: eventPayload(provider.Message{Role: "user", Content: strings.Repeat("a", 200)})})
	}

	display := RestoreSession(mgr, events)
	joined := strings.Join(display, "\n")
	if !strings.Contains(joined, "older events omitted") {
		t.Errorf("expected omitted-events note when context window is full, got: %v", joined)
	}
	if len(mgr.Messages()) >= 30 {
		t.Errorf("expected some events dropped to fit window, restored %d", len(mgr.Messages()))
	}

	// Engine-injected reminders restore for the model but are hidden from display.
	mgr2 := NewManager("s2", nil, 128000)
	events2 := []store.Event{
		{Type: "user_msg", PayloadJSON: eventPayload(provider.Message{Role: "user", Content: "⚠️ You have been calling tools for many rounds without answering. STOP calling tools"})},
	}
	display2 := RestoreSession(mgr2, events2)
	if len(display2) != 0 {
		t.Errorf("engine reminder should be hidden from display, got: %v", display2)
	}
	if len(mgr2.Messages()) != 1 || mgr2.Messages()[0].Content == "" {
		t.Errorf("reminder must still be restored to model context: %+v", mgr2.Messages())
	}
}

func TestSetMaxWindow(t *testing.T) {
	mgr := NewManager("s", nil, 128000)
	if mgr.MaxWindow() != 128000 {
		t.Fatalf("expected default window 128000, got %d", mgr.MaxWindow())
	}
	mgr.SetMaxWindow(1048576)
	if mgr.MaxWindow() != 1048576 {
		t.Errorf("expected updated window 1048576, got %d", mgr.MaxWindow())
	}
	// Non-positive values are ignored.
	mgr.SetMaxWindow(0)
	if mgr.MaxWindow() != 1048576 {
		t.Errorf("expected window unchanged after SetMaxWindow(0), got %d", mgr.MaxWindow())
	}
}

func TestContextManagerAppendAndCompaction(t *testing.T) {
	mgr := NewManager("test-session", nil, 100)

	if err := mgr.AppendUserMessage("build a feature"); err != nil {
		t.Fatalf("failed to append user msg: %v", err)
	}

	msgs := mgr.Messages()
	if len(msgs) != 1 || msgs[0].Content != "build a feature" {
		t.Errorf("unexpected messages list: %+v", msgs)
	}

	summary := CompactionSummary{
		Goal:           "Build a feature",
		FilesTouched:   []string{"main.go"},
		DecisionsMade:  []string{"Use SQLite"},
		OpenQuestions:  []string{"None"},
		LastKnownState: "Pass",
	}

	if err := mgr.Compact(summary); err != nil {
		t.Fatalf("failed to compact context: %v", err)
	}

	compactedMsgs := mgr.Messages()
	if len(compactedMsgs) < 1 || compactedMsgs[0].Role != "system" {
		t.Errorf("expected compacted system summary message as first element")
	}
}

// TestRestoreSessionStampsModeAndModel verifies that an assistant turn
// persisted with Mode/Model metadata restores with a "BROCODE:MODE:MODEL\n"
// display prefix (so the UI renders the original badge), while messages saved
// without metadata keep the legacy unstamped form.
func TestRestoreSessionStampsModeAndModel(t *testing.T) {
	mgr := NewManager("s", nil, 128000)
	events := []store.Event{
		{Type: "user_msg", PayloadJSON: eventPayload(provider.Message{Role: "user", Content: "pahami arsitektur"})},
		{Type: "assistant_msg", PayloadJSON: eventPayload(provider.Message{Role: "assistant", Content: "ini rencana", Mode: "PLANNER", Model: "poolside/laguna-s-2.1"})},
		{Type: "assistant_msg", PayloadJSON: eventPayload(provider.Message{Role: "assistant", Content: "jawaban lama tanpa stamp"})},
	}

	display := RestoreSession(mgr, events)

	if !strings.Contains(display[1], "BROCODE:PLANNER:poolside/laguna-s-2.1\n") {
		t.Errorf("expected mode+model stamped display, got: %q", display[1])
	}
	if !strings.HasPrefix(display[2], "BROCODE:\n") {
		t.Errorf("expected legacy unstamped display, got: %q", display[2])
	}

	// The metadata must also survive into the restored model context so a
	// re-serialized request keeps the fields.
	msgs := mgr.Messages()
	if msgs[1].Mode != "PLANNER" || msgs[1].Model != "poolside/laguna-s-2.1" {
		t.Errorf("expected mode/model restored into context, got %+v", msgs[1])
	}
}

// TestAppendAssistantTurnPersistsModeModel verifies AppendAssistantTurn stamps
// the turn with its mode and model and that the stored payload round-trips.
func TestAppendAssistantTurnPersistsModeModel(t *testing.T) {
	mgr := NewManager("s", nil, 128000)
	if err := mgr.AppendAssistantTurn("MINER", "poolside/x", "why", "jawaban", nil); err != nil {
		t.Fatalf("AppendAssistantTurn failed: %v", err)
	}
	msgs := mgr.Messages()
	if len(msgs) != 1 || msgs[0].Mode != "MINER" || msgs[0].Model != "poolside/x" {
		t.Errorf("expected mode/model on appended turn, got %+v", msgs)
	}

	// Round-trip through the same payload format resume reads from disk.
	var decoded provider.Message
	if err := json.Unmarshal([]byte(eventPayload(msgs[0])), &decoded); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}
	if decoded.Mode != "MINER" || decoded.Model != "poolside/x" {
		t.Errorf("mode/model lost in serialization round-trip: %+v", decoded)
	}
}
