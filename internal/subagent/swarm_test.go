package subagent

import (
	"context"
	"strings"
	"sync"
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

// recordingSwarmAdapter is mockSwarmAdapter plus per-role model capture, so the
// per-role model routing (CheapModel) can be verified deterministically.
type recordingSwarmAdapter struct {
	mu    sync.Mutex
	model string
	arch  string
	build string
	audit string
}

func (r *recordingSwarmAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[len(req.Messages)-1].Content
	}
	r.mu.Lock()
	r.model = req.Model
	switch {
	case strings.Contains(prompt, "You are the AUDITOR"):
		r.audit = req.Model
	case strings.Contains(prompt, "You are the BUILDER"):
		r.build = req.Model
	case strings.Contains(prompt, "You are the ARCHITECT"):
		r.arch = req.Model
	}
	r.mu.Unlock()

	content := "OK"
	if strings.Contains(prompt, "You are the AUDITOR") {
		content = "Audit: PASSED all security checks."
	} else if strings.Contains(prompt, "You are the BUILDER") {
		content = "Implemented refactor in auth.go"
	} else if strings.Contains(prompt, "You are the ARCHITECT") {
		content = "Blueprint: Refactor auth middleware in auth.go"
	}
	return &provider.CompletionResponse{Content: content}, nil
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

// TestSwarmPerRoleModelRouting verifies the ARCHITECT runs on the strong
// Model while BUILDER and AUDITOR are routed to CheapModel when set — so
// mechanical roles never burn flagship pricing for mechanical work.
func TestSwarmPerRoleModelRouting(t *testing.T) {
	adapter := &recordingSwarmAdapter{}
	runner := &Runner{
		Adapter:       adapter,
		Tools:         tool.NewRegistry(),
		Model:         "strong-model",
		CheapModel:    "cheap-model",
		ContextWindow: 32000,
	}

	_, err := runner.ExecuteSwarm(context.Background(), SwarmTask{
		Goal:    "Refactor authentication layer",
		Context: "Focus on session token expiration",
	}, nil)
	if err != nil {
		t.Fatalf("ExecuteSwarm failed: %v", err)
	}

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.arch != "strong-model" {
		t.Errorf("ARCHITECT model = %q, want strong-model", adapter.arch)
	}
	if adapter.build != "cheap-model" {
		t.Errorf("BUILDER model = %q, want cheap-model", adapter.build)
	}
	if adapter.audit != "cheap-model" {
		t.Errorf("AUDITOR model = %q, want cheap-model", adapter.audit)
	}
}

// TestSwarmFallsBackToSingleModel proves CheapModel="" keeps every role on the
// same Model (backward-compatible default).
func TestSwarmFallsBackToSingleModel(t *testing.T) {
	adapter := &recordingSwarmAdapter{}
	runner := &Runner{
		Adapter:       adapter,
		Tools:         tool.NewRegistry(),
		Model:         "strong-model",
		ContextWindow: 32000,
	}

	_, err := runner.ExecuteSwarm(context.Background(), SwarmTask{
		Goal: "Refactor authentication layer",
	}, nil)
	if err != nil {
		t.Fatalf("ExecuteSwarm failed: %v", err)
	}

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.arch != "strong-model" || adapter.build != "strong-model" || adapter.audit != "strong-model" {
		t.Errorf("without CheapModel all roles should use Model: arch=%q build=%q audit=%q",
			adapter.arch, adapter.build, adapter.audit)
	}
}

// TestSwarmMetricsAttribution verifies per-phase token/cost accounting: each
// role's RunMetrics is populated from the engine turn usage and SwarmResult
// aggregates them into TotalTokens/TotalCost.
func TestSwarmMetricsAttribution(t *testing.T) {
	adapter := &usageSwarmAdapter{}
	runner := &Runner{
		Adapter:       adapter,
		Tools:         tool.NewRegistry(),
		Model:         "gpt-4o",
		ContextWindow: 32000,
	}

	res, err := runner.ExecuteSwarm(context.Background(), SwarmTask{
		Goal: "Refactor authentication layer",
	}, nil)
	if err != nil {
		t.Fatalf("ExecuteSwarm failed: %v", err)
	}

	if res.TotalTokens != 3*1500 {
		t.Errorf("TotalTokens = %d, want %d (3 phases × 1500)", res.TotalTokens, 3*1500)
	}
	if res.Architect.Tokens != 1500 || res.Builder.Tokens != 1500 || res.Auditor.Tokens != 1500 {
		t.Errorf("per-phase tokens wrong: arch=%d build=%d audit=%d",
			res.Architect.Tokens, res.Builder.Tokens, res.Auditor.Tokens)
	}
	// Estimated cost is strictly positive once usage is attributed.
	if res.TotalCost <= 0 {
		t.Errorf("TotalCost = %.4f, want > 0", res.TotalCost)
	}
}

// usageSwarmAdapter is mockSwarmAdapter plus per-role model capture, so the
// per-role model routing (CheapModel) can be verified deterministically.
type usageSwarmAdapter struct{}

func (u *usageSwarmAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	content := "OK"
	content = "Audit: PASSED all security checks."
	if len(req.Messages) > 0 {
		if strings.Contains(req.Messages[len(req.Messages)-1].Content, "You are the BUILDER") {
			content = "Implemented refactor in auth.go"
		} else if strings.Contains(req.Messages[len(req.Messages)-1].Content, "You are the ARCHITECT") {
			content = "Blueprint: Refactor auth middleware in auth.go"
		}
	}
	return &provider.CompletionResponse{
		Content: content,
		Usage:   provider.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
	}, nil
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
