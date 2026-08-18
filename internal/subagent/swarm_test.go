package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

type mockSwarmAdapter struct{}

func (m *mockSwarmAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[len(req.Messages)-1].Content
	}

	content := "OK"
	if strings.Contains(prompt, "You are the AUDITOR") {
		content = "Audit: PASSED all security checks."
	} else if strings.Contains(prompt, "You are the BUILDER") {
		content = "Implemented refactor in auth.go"
	} else if strings.Contains(prompt, "You are the ARCHITECT") {
		content = "Blueprint: Refactor auth middleware in auth.go"
	}

	return &provider.CompletionResponse{
		Content: content,
	}, nil
}

func TestSwarmPipelineExecution(t *testing.T) {
	tools := tool.NewRegistry()
	runner := &Runner{
		Adapter:       &mockSwarmAdapter{},
		Tools:         tools,
		Model:         "test-model",
		ContextWindow: 32000,
	}

	task := SwarmTask{
		Goal:    "Refactor authentication layer",
		Context: "Focus on session token expiration",
	}

	var updates []string
	onUpdate := func(st loop.LoopState, info string) {
		updates = append(updates, info)
	}

	res, err := runner.ExecuteSwarm(context.Background(), task, onUpdate)
	if err != nil {
		t.Fatalf("ExecuteSwarm failed: %v", err)
	}

	if !res.Success {
		t.Errorf("expected success to be true, got %v", res.Success)
	}
	if !strings.Contains(res.ArchitectSpec, "Blueprint") {
		t.Errorf("expected ArchitectSpec to contain Blueprint, got %q", res.ArchitectSpec)
	}
	if !strings.Contains(res.BuilderOutput, "Implemented") {
		t.Errorf("expected BuilderOutput to contain Implemented, got %q", res.BuilderOutput)
	}
	if !strings.Contains(res.AuditorVerdict, "PASSED") {
		t.Errorf("expected AuditorVerdict to contain PASSED, got %q", res.AuditorVerdict)
	}
	if len(updates) == 0 {
		t.Error("expected onUpdate progress messages")
	}
}

func TestSwarmToolExecution(t *testing.T) {
	tools := tool.NewRegistry()
	runner := &Runner{
		Adapter:       &mockSwarmAdapter{},
		Tools:         tools,
		Model:         "test-model",
		ContextWindow: 32000,
	}

	swTool := &SwarmTool{Runner: runner}
	out, err := swTool.Execute(context.Background(), `{"goal":"Optimize DB queries"}`)
	if err != nil {
		t.Fatalf("SwarmTool.Execute failed: %v", err)
	}
	if !strings.Contains(out, "Swarm Pipeline Finished") {
		t.Errorf("expected header, got %q", out)
	}
}
