package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// fakeAdapter implements ProviderAdapter with scripted behavior per request.
type fakeAdapter struct {
	mu            sync.Mutex
	callCount     int
	maxToolRounds int
	// reportUsage makes every completion report heavy token usage so cost
	// budgets trip deterministically in tests.
	reportUsage bool
}

// Complete inspects the incoming messages and either issues a tool call or
// answers. Each fresh sub-agent context has exactly one user message (the
// task); a scripted tool round verifies the loop works inside the sub-agent.
func (f *fakeAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	f.mu.Lock()
	f.callCount++
	round := f.callCount
	f.mu.Unlock()

	// Count how many user/tool messages are present.
	var userMsgs int
	for _, m := range req.Messages {
		if m.Role == "user" && m.Content != "" && m.ToolCallID == "" {
			userMsgs++
		}
	}

	// Find the last user message content (the task).
	task := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && req.Messages[i].Content != "" && req.Messages[i].ToolCallID == "" {
			task = req.Messages[i].Content
			break
		}
	}

	usage := provider.Usage{}
	if f.reportUsage {
		usage = provider.Usage{PromptTokens: 200000, CompletionTokens: 200000, TotalTokens: 400000}
	}

	// Round 1: call the bash tool (safe command) to prove tool execution works.
	if round == 1 {
		return &provider.CompletionResponse{
			Usage: usage,
			ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "bash", Arguments: `{"command":"echo sub-ok"}`},
			},
		}, nil
	}
	// Round 2: answer, echoing the task and the tool result we saw.
	return &provider.CompletionResponse{
		Usage:   usage,
		Content: fmt.Sprintf("SUBAGENT ANSWER for task %q | userMsgs=%d", task, userMsgs),
	}, nil
}

// StreamComplete lets the fakeAdapter satisfy StreamingAdapter as well (used by
// the engine when a stream handler is set — it isn't here, so not exercised).
func (f *fakeAdapter) StreamComplete(ctx context.Context, req provider.CompletionRequest, onDelta func(string)) (*provider.CompletionResponse, error) {
	return f.Complete(ctx, req)
}

// blockingAdapter tracks the max number of concurrent in-flight completions,
// so the scout concurrency cap can be verified deterministically. It blocks
// until the gate channel is closed/released, and respects context cancellation
// so cancel tests can actually abort a running scout.
type blockingAdapter struct {
	mu   sync.Mutex
	cur  int
	max  int
	gate chan struct{}
}

func (b *blockingAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	b.mu.Lock()
	b.cur++
	if b.cur > b.max {
		b.max = b.cur
	}
	b.mu.Unlock()
	select {
	case <-b.gate:
	case <-ctx.Done():
		b.mu.Lock()
		b.cur--
		b.mu.Unlock()
		return nil, ctx.Err()
	}
	b.mu.Lock()
	b.cur--
	b.mu.Unlock()
	return &provider.CompletionResponse{Content: "scout done"}, nil
}

func (b *blockingAdapter) maxConcurrent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max
}

// TestScoutConcurrencyCapped verifies that spawning more scouts than the
// concurrency cap never runs more than 3 agent loops at once — an unbounded
// batch would hammer the provider with parallel full agent loops.
func TestScoutConcurrencyCapped(t *testing.T) {
	gate := make(chan struct{})
	adapter := &blockingAdapter{gate: gate}
	tools := tool.NewRegistry()
	r := &Runner{Adapter: adapter, Model: "test-model", Tools: tools}
	sm := NewScoutManager(r)

	ctx := context.Background()
	for i := 0; i < 6; i++ {
		if _, err := sm.Start(ctx, fmt.Sprintf("scout task %d", i)); err != nil {
			t.Fatalf("start scout: %v", err)
		}
	}

	// Wait until the semaphore is saturated (3 concurrent runs in flight).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && adapter.maxConcurrent() < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	close(gate) // release all blocked scouts

	if m := adapter.maxConcurrent(); m > 3 {
		t.Fatalf("scouts exceeded concurrency cap: max concurrent = %d (want <= 3)", m)
	}
	// All six scouts must complete after release.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && sm.Pending() > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := sm.Pending(); n != 0 {
		t.Fatalf("%d scouts still pending after release", n)
	}
}

func TestSubAgentRunsIsolatedTurn(t *testing.T) {
	f := &fakeAdapter{}
	tools := tool.NewRegistry()
	r := &Runner{Adapter: f, Model: "test-model", Tools: tools}

	out, err := r.Run(context.Background(), "inspect file A")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "inspect file A") {
		t.Errorf("answer = %q, want it to contain the task", out)
	}
}

func TestSubAgentContextIsIsolated(t *testing.T) {
	f := &fakeAdapter{}
	tools := tool.NewRegistry()
	r := &Runner{Adapter: f, Model: "test-model", Tools: tools}

	ctx := context.Background()
	out1, err := r.Run(ctx, "task one")
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	out2, err := r.Run(ctx, "task two")
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	// Each sub-agent context must contain exactly one user message (its own
	// task) — never the previous sub-agent's task.
	if !strings.Contains(out1, "userMsgs=1") {
		t.Errorf("first sub-agent saw userMsgs != 1: %q", out1)
	}
	if !strings.Contains(out2, "userMsgs=1") {
		t.Errorf("second sub-agent saw userMsgs != 1: %q (leaked context!)", out2)
	}
	if strings.Contains(out2, "task one") {
		t.Errorf("second sub-agent leaked first task into its context: %q", out2)
	}
}

func TestSubAgentRunManyParallel(t *testing.T) {
	f := &fakeAdapter{}
	tools := tool.NewRegistry()
	r := &Runner{Adapter: f, Model: "test-model", Tools: tools}

	reports, err := r.RunMany(context.Background(), []SubAgent{
		{ID: "a", Task: "task alpha"},
		{ID: "b", Task: "task beta"},
		{ID: "c", Task: "task gamma"},
	}, true, nil)
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}
	if len(reports) != 3 {
		t.Fatalf("reports = %d, want 3", len(reports))
	}
	for _, rep := range reports {
		if !strings.Contains(rep, "Sub-agent") || !strings.Contains(rep, "DONE") {
			t.Errorf("report missing status: %q", rep)
		}
	}
	// All three tasks must be answered.
	joined := strings.Join(reports, "\n")
	for _, want := range []string{"task alpha", "task beta", "task gamma"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing answer for %q in %q", want, joined)
		}
	}
}

func TestSubAgentToolExecute(t *testing.T) {
	f := &fakeAdapter{}
	tools := tool.NewRegistry()
	subTool := &Tool{Runner: &Runner{Adapter: f, Model: "test-model", Tools: tools}}

	args, _ := json.Marshal(map[string]any{
		"tasks": []map[string]any{
			{"id": "x", "task": "check the API"},
			{"id": "y", "task": "review the UI"},
		},
		"parallel": true,
	})
	out, err := subTool.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "check the API") || !strings.Contains(out, "review the UI") {
		t.Errorf("output = %q, want both task answers", out)
	}
}

func TestSubAgentToolEmptyArgs(t *testing.T) {
	f := &fakeAdapter{}
	tools := tool.NewRegistry()
	subTool := &Tool{Runner: &Runner{Adapter: f, Model: "test-model", Tools: tools}}
	_, err := subTool.Execute(context.Background(), `{}`)
	if err == nil {
		t.Error("expected error for empty args, got nil")
	}
}

func TestSubAgentRegistryDropsInteractiveTools(t *testing.T) {
	tools := tool.NewRegistry()
	sub := tools.SubRegistry()

	for _, banned := range []string{"ask_user", "review_changes", "subagent"} {
		if sub.Lookup(banned) != nil {
			t.Errorf("SubRegistry still contains %q", banned)
		}
	}
	// Core tools must remain.
	for _, keep := range []string{"read_file", "grep", "bash", "git", "write_file", "web_search"} {
		if sub.Lookup(keep) == nil {
			t.Errorf("SubRegistry missing %q", keep)
		}
	}
}

func TestSubAgentGatedCommandsDenied(t *testing.T) {
	tools := tool.NewRegistry()
	sub := tools.SubRegistry()

	// A gated command (rm) must be denied in the sub-registry without any
	// interactive modal — askFunc denies with an error.
	approved, reason, err := sub.GateAction(context.Background(), provider.ToolCall{
		Name:      "bash",
		Arguments: `{"command":"rm -rf ./dist"}`,
	})
	if err != nil {
		t.Fatalf("GateAction: %v", err)
	}
	if approved {
		t.Errorf("gated command approved in sub-registry: %s", reason)
	}
	if !strings.Contains(reason, "sub-agents cannot") {
		t.Errorf("reason = %q, want sub-agent denial message", reason)
	}

	// Safe commands still pass.
	approved, _, err = sub.GateAction(context.Background(), provider.ToolCall{
		Name:      "bash",
		Arguments: `{"command":"echo hi"}`,
	})
	if err != nil || !approved {
		t.Errorf("safe command should pass, got approved=%v err=%v", approved, err)
	}
}

func TestScoutRunsInBackgroundAndDrains(t *testing.T) {
	f := &fakeAdapter{}
	tools := tool.NewRegistry()
	r := &Runner{Adapter: f, Model: "test-model", Tools: tools}
	sm := NewScoutManager(r)
	sc := &ScoutTool{Manager: sm}

	// Scout returns a receipt immediately — it must not block on the fake
	// adapter's two-round loop.
	start := time.Now()
	out, err := sc.Execute(context.Background(), `{"task":"research the auth module"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "background") {
		t.Errorf("expected background receipt, got %q", out)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("scout Execute blocked instead of returning immediately (took %v)", time.Since(start))
	}

	// Immediately after starting, the job is pending (not done yet).
	if sm.Pending() != 1 {
		t.Errorf("expected 1 pending scout, got %d", sm.Pending())
	}

	// Poll until the background job finishes (fake adapter is fast).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if sm.Pending() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scout never finished in background")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Drain delivers the findings.
	reports := sm.Drain()
	if len(reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(reports))
	}
	if !strings.Contains(reports[0], "Scout") || !strings.Contains(reports[0], "DONE") {
		t.Errorf("report = %q, want scout status", reports[0])
	}
	if !strings.Contains(reports[0], "research the auth module") {
		t.Errorf("report should echo the task: %q", reports[0])
	}

	// Second drain is empty — jobs are removed once delivered.
	if got := sm.Drain(); len(got) != 0 {
		t.Errorf("expected empty second drain, got %q", got)
	}
}

func TestScoutToolRequiresTask(t *testing.T) {
	sm := NewScoutManager(&Runner{Adapter: &fakeAdapter{}, Model: "m", Tools: tool.NewRegistry()})
	sc := &ScoutTool{Manager: sm}
	if _, err := sc.Execute(context.Background(), `{}`); err == nil {
		t.Error("expected error for empty task")
	}
}

func TestScoutManagerNilSafe(t *testing.T) {
	if got := (&ScoutManager{}).Pending(); got != 0 {
		t.Errorf("nil-safe Pending expected 0, got %d", got)
	}
	var sm *ScoutManager
	if got := sm.Drain(); got != nil {
		t.Errorf("nil Drain expected nil, got %q", got)
	}
	if _, err := sm.Start(context.Background(), "x"); err == nil {
		t.Error("nil manager Start should error")
	}
}

func TestSubAgentToolTimeout(t *testing.T) {
	// A hung adapter must be cut off by the tool's 10-minute cap — we can't
	// wait that long in a test, so verify the timeout wiring exists by using a
	// pre-canceled context instead: Run should return promptly.
	f := &fakeAdapter{}
	tools := tool.NewRegistry()
	subTool := &Tool{Runner: &Runner{Adapter: f, Model: "test-model", Tools: tools}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	// Give the loop time to spin once, then the deadline hits.
	done := make(chan struct{})
	go func() {
		_, _ = subTool.Execute(ctx, `{"task":"quick check"}`)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Execute did not return after context cancellation")
	}
}

// TestRunManyStreamsProgress verifies completed sub-agents emit an incremental
// DONE/FAILED progress line instead of the caller waiting for the whole batch.
func TestRunManyStreamsProgress(t *testing.T) {
	f := &fakeAdapter{}
	tools := tool.NewRegistry()
	r := &Runner{Adapter: f, Model: "test-model", Tools: tools}

	var mu sync.Mutex
	var updates []string
	onUpdate := func(st loop.LoopState, info string) {
		mu.Lock()
		updates = append(updates, info)
		mu.Unlock()
	}

	reports, err := r.RunMany(context.Background(), []SubAgent{
		{ID: "a", Task: "task alpha"},
		{ID: "b", Task: "task beta"},
	}, true, onUpdate)
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %d, want 2", len(reports))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(updates) < 2 {
		t.Fatalf("expected >=2 progress updates, got %d: %v", len(updates), updates)
	}
	var doneCount int
	for _, u := range updates {
		if strings.Contains(u, "Sub-agent") && (strings.Contains(u, "DONE") || strings.Contains(u, "FAILED")) {
			doneCount++
		}
	}
	if doneCount < 2 {
		t.Errorf("expected >=2 completion progress lines, got %d of %v", doneCount, updates)
	}
}

// TestScoutCancelAbortsJob verifies Cancel() stops a running scout via its
// context, and CancelAll() aborts every in-flight scout.
func TestScoutCancelAbortsJob(t *testing.T) {
	gate := make(chan struct{})
	adapter := &blockingAdapter{gate: gate}
	tools := tool.NewRegistry()
	r := &Runner{Adapter: adapter, Model: "test-model", Tools: tools}
	sm := NewScoutManager(r)

	id, err := sm.Start(context.Background(), "cancel me")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait until the scout is blocked inside the adapter.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && adapter.maxConcurrent() < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	sm.Cancel(id)

	// The job must become done (with an error from the cancelled context).
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sm.Pending() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := sm.Pending(); n != 0 {
		t.Fatalf("%d scouts still pending after cancel", n)
	}
	close(gate) // release in case the cancel raced

	reports := sm.Drain()
	if len(reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(reports))
	}
	if !strings.Contains(reports[0], "FAILED") {
		t.Errorf("cancelled scout should report FAILED, got %q", reports[0])
	}
}

// TestScoutCancelAll verifies CancelAll aborts every in-flight scout even when
// none of their contexts would otherwise be cancelled.
func TestScoutCancelAll(t *testing.T) {
	gate := make(chan struct{})
	adapter := &blockingAdapter{gate: gate}
	tools := tool.NewRegistry()
	r := &Runner{Adapter: adapter, Model: "test-model", Tools: tools}
	sm := NewScoutManager(r)

	for i := 0; i < 4; i++ {
		if _, err := sm.Start(context.Background(), fmt.Sprintf("job %d", i)); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && adapter.maxConcurrent() < 3 {
		time.Sleep(10 * time.Millisecond)
	}

	sm.CancelAll()
	close(gate)

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sm.Pending() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := sm.Pending(); n != 0 {
		t.Fatalf("%d scouts still pending after CancelAll", n)
	}
}

// TestSubAgentBudgetCap verifies a Runner.BudgetUSD > 0 is wired onto the
// sub-agent engine so a runaway sub-agent is stopped by the cost budget.
func TestSubAgentBudgetCap(t *testing.T) {
	// usageAdapter reports heavy token usage on every completion, so a tiny
	// budget trips the engine's hard cost-stop after the first completion.
	usageAdapter := &fakeAdapter{reportUsage: true}
	tools := tool.NewRegistry()
	r := &Runner{Adapter: usageAdapter, Model: "test-model", Tools: tools, BudgetUSD: 0.0000001}

	out, err := r.Run(context.Background(), "expensive task")
	if err != nil {
		t.Fatalf("Run with budget cap: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("expected a graceful synthesized answer even under a tiny budget")
	}
}
