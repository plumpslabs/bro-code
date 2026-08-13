package context

import (
	"strings"
	"testing"
)

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
	_ = mgr.AppendAssistantTurn("thinking...", "first answer", nil)
	before := mgr.TotalTokens()

	// Resume/replay must import without duplicating persistence side effects.
	mgr.ImportUserMessage("first prompt")
	mgr.ImportAssistantTurn("thinking...", "first answer", nil)

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
