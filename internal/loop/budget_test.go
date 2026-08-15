package loop

import (
	"context"
	"math"
	"strings"
	"testing"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// budgetAdapter reports a large prompt-token usage on every completion so the
// engine's cost accounting accumulates real dollars against "gpt-4o" prices.
// Main-loop requests (Tools non-nil) return a tool call; synth requests
// (Tools nil) answer with content.
type budgetAdapter struct {
	total int
}

func (a *budgetAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	a.total++
	u := provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 0, TotalTokens: 1_000_000}
	if len(req.Tools) == 0 {
		return &provider.CompletionResponse{Content: "SYNTHESIZED", Usage: u}, nil
	}
	return &provider.CompletionResponse{
		ToolCalls: []provider.ToolCall{{ID: "t", Name: "list_dir", Arguments: `{"path":"."}`}},
		Usage:     u,
	}, nil
}

// TestCostBudgetStopsTurnGracefully: with gpt-4o pricing ($2.50/M input) and a
// $5 budget, the turn is cut off after two tool completions and answered by a
// bounded synthesis completion — no raw error, cost counter reflects spend.
func TestCostBudgetStopsTurnGracefully(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &budgetAdapter{}
	e := NewEngine(adapter, tools, ctxMgr, "gpt-4o")
	e.SetBudgetUSD(5.0)

	res, err := e.RunTurn(context.Background(), "do some work", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !strings.Contains(res, "SYNTHESIZED") {
		t.Fatalf("expected graceful synthesis answer, got %q", res)
	}
	if !strings.Contains(res, "Batas Biaya") {
		t.Fatalf("expected budget-abort marker in answer, got %q", res)
	}
	// 2 tool completions + 1 synth completion, each $2.50.
	if got, want := e.CostUSD(), 7.5; math.Abs(got-want) > 1e-3 {
		t.Fatalf("expected cost %.2f, got %.2f (completions=%d)", want, got, adapter.total)
	}
}

// TestNoBudgetStillRunsFullTurn: without a budget the same adapter runs until
// it exhausts the tool-only budget (never blocking on cost).
func TestNoBudgetStillRunsFullTurn(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &budgetAdapter{}
	e := NewEngine(adapter, tools, ctxMgr, "gpt-4o")

	_, err := e.RunTurn(context.Background(), "do some work", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if adapter.total <= 2 {
		t.Fatalf("expected more than 2 completions without a budget, got %d", adapter.total)
	}
}
