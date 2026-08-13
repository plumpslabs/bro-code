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
