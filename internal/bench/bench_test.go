package bench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumpslabs/bro-code/internal/provider"
)

// fakeAdapter answers after a configurable number of tool rounds.
type fakeAdapter struct {
	mu        sync.Mutex
	callCount int
	// toolRounds is how many tool-only rounds to emit before answering.
	toolRounds int
}

func (f *fakeAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	f.mu.Lock()
	f.callCount++
	round := f.callCount
	f.mu.Unlock()

	if round <= f.toolRounds {
		// Use a safe command so the gate passes.
		return &provider.CompletionResponse{
			ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "bash", Arguments: `{"command":"echo bench-step"}`},
			},
		}, nil
	}
	return &provider.CompletionResponse{Content: "task complete"}, nil
}

func (f *fakeAdapter) StreamComplete(ctx context.Context, req provider.CompletionRequest, onDelta func(string)) (*provider.CompletionResponse, error) {
	return f.Complete(ctx, req)
}

func TestBenchRunAndVerify(t *testing.T) {
	f := &fakeAdapter{toolRounds: 1}
	r := &Runner{Adapter: f, Model: "m", Timeout: 30 * time.Second}

	// The verify script checks the sandbox file the setup created.
	cases := []Case{
		{
			ID:     "write-answer",
			Prompt: "create file result.txt containing done",
			Setup:  "echo started > state.txt",
			Verify: "test -f state.txt && grep -q started state.txt",
		},
	}
	results := r.Run(context.Background(), cases)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	res := results[0]
	if !res.Pass {
		t.Fatalf("expected pass, got error %q", res.Error)
	}
	if res.Iterations != 2 { // 1 tool round + 1 answer
		t.Errorf("iterations = %d, want 2", res.Iterations)
	}
	if res.Tokens <= 0 {
		t.Errorf("tokens = %d, want > 0", res.Tokens)
	}
}

func TestBenchVerifyFails(t *testing.T) {
	f := &fakeAdapter{}
	r := &Runner{Adapter: f, Model: "m", Timeout: 30 * time.Second}
	cases := []Case{
		{ID: "fails", Prompt: "do nothing", Verify: "exit 1"},
	}
	results := r.Run(context.Background(), cases)
	if results[0].Pass {
		t.Error("expected FAIL when verify exits non-zero")
	}
	if !strings.Contains(results[0].Error, "verification failed") {
		t.Errorf("error = %q, want verification failed message", results[0].Error)
	}
}

func TestBenchSandboxIsolation(t *testing.T) {
	// Two cases must not see each other's files.
	root := t.TempDir()
	f := &fakeAdapter{}
	r := &Runner{Adapter: f, Model: "m", SandboxRoot: root, Timeout: 30 * time.Second}
	cases := []Case{
		{ID: "a", Prompt: "one", Setup: "touch file_a.txt", Verify: "test -f file_a.txt && test ! -f file_b.txt"},
		{ID: "b", Prompt: "two", Setup: "touch file_b.txt", Verify: "test -f file_b.txt && test ! -f file_a.txt"},
	}
	results := r.Run(context.Background(), cases)
	for _, res := range results {
		if !res.Pass {
			t.Errorf("case %s: %q", res.ID, res.Error)
		}
	}
}

func TestSummarizeAndRender(t *testing.T) {
	rep := Summarize([]Result{
		{ID: "b", Pass: true, Duration: time.Second, Tokens: 100},
		{ID: "a", Pass: false, Error: "boom\nline2", Duration: 2 * time.Second, Tokens: 50},
	})
	if rep.Total != 2 || rep.Passed != 1 || rep.Failed != 1 {
		t.Errorf("summary counts wrong: %+v", rep)
	}
	if rep.PassRate != 50 {
		t.Errorf("pass rate = %.1f, want 50", rep.PassRate)
	}
	if rep.MeanTokens != 75 {
		t.Errorf("mean tokens = %d, want 75", rep.MeanTokens)
	}
	// Per-case must be sorted by ID (a before b).
	if rep.PerCase[0].ID != "a" || rep.PerCase[1].ID != "b" {
		t.Errorf("per-case not sorted: %+v", rep.PerCase)
	}
	rendered := RenderReport(rep)
	if !strings.Contains(rendered, "50.0%") {
		t.Errorf("render missing pass rate: %q", rendered)
	}
}

func TestLoadCasesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cases.json")
	os.WriteFile(p, []byte(`[
		{"id":"one","prompt":"p1","verify":"true"},
		{"id":"two","prompt":"p2","verify":"true"}
	]`), 0o644)
	cases, err := LoadCases(p)
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("cases = %d, want 2", len(cases))
	}

	// Single-case file also works.
	os.WriteFile(p, []byte(`{"id":"solo","prompt":"p","verify":"true"}`), 0o644)
	cases, err = LoadCases(p)
	if err != nil {
		t.Fatalf("LoadCases single: %v", err)
	}
	if len(cases) != 1 || cases[0].ID != "solo" {
		t.Fatalf("single case parse wrong: %+v", cases)
	}
}
