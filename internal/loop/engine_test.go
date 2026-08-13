package loop

import (
	"context"
	"testing"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

type mockAdapter struct {
	toolCalls []provider.ToolCall
}

func (m *mockAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(m.toolCalls) > 0 {
		tc := m.toolCalls
		m.toolCalls = nil // emit once
		return &provider.CompletionResponse{
			Reasoning: "Testing tool guard",
			ToolCalls: tc,
		}, nil
	}
	return &provider.CompletionResponse{
		Content: "Done testing",
	}, nil
}

func TestPlannerModeToolGuard(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	adapter := &mockAdapter{
		toolCalls: []provider.ToolCall{
			{ID: "tc1", Name: "write_file", Arguments: `{"path":"test.txt","content":"hello"}`},
			{ID: "tc2", Name: "bash", Arguments: `{"command":"rm -rf /"}`},
		},
	}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	engine.SetMode("PLANNER")

	if engine.Mode() != "PLANNER" {
		t.Fatalf("expected mode PLANNER, got %s", engine.Mode())
	}

	_, err := engine.RunTurn(context.Background(), "test prompt", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	// Verify that guard message was injected into context for write_file and bash
	msgs := ctxMgr.Messages()
	foundWriteGuard := false
	foundBashGuard := false

	for _, msg := range msgs {
		if msg.ToolCallID == "tc1" && len(msg.Content) > 0 {
			foundWriteGuard = true
		}
		if msg.ToolCallID == "tc2" && len(msg.Content) > 0 {
			foundBashGuard = true
		}
	}

	if !foundWriteGuard {
		t.Errorf("write_file tool call was not blocked by PLANNER guard")
	}
	if !foundBashGuard {
		t.Errorf("bash tool call was not blocked by PLANNER guard")
	}
}
