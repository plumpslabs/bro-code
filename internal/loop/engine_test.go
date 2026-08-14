package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

type mockAdapter struct {
	toolCalls []provider.ToolCall
}

type failAdapter struct {
	err error
}

func (m *failAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, m.err
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

// lateProgressAdapter is a ProgressingAdapter that fires the progress callback
// from its own goroutine AFTER CompleteWithProgress returns — simulating the
// opencode CLI's stderr goroutine, which can keep streaming after the turn
// wraps up (and RunTurn has already reset the engine's progress handler to
// nil via its deferred cleanup).
type lateProgressAdapter struct {
	calls int
}

func (m *lateProgressAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return &provider.CompletionResponse{Content: "done"}, nil
}

func (m *lateProgressAdapter) CompleteWithProgress(ctx context.Context, req provider.CompletionRequest, onProgress func(string)) (*provider.CompletionResponse, error) {
	m.calls++
	go func() {
		// Fire well after RunTurn returns so the engine's progressHandler
		// field has already been reset to nil. Before the snapshot fix this
		// dereferenced the nil field and panicked.
		time.Sleep(10 * time.Millisecond)
		onProgress("late progress")
	}()
	return &provider.CompletionResponse{Content: "done"}, nil
}

// TestEngineLateProgressNoPanic guards the nil-progress-handler panic (the
// crash the user hit when spamming prompts while a turn was in flight): the
// adapter's own goroutine keeps streaming progress after the turn finished and
// the engine reset its handler. completeWith must hold a snapshot so the late
// callback is a safe no-op, and the engine must remain usable afterwards.
func TestEngineLateProgressNoPanic(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &lateProgressAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	res, err := engine.RunTurn(context.Background(), "hi", func(state LoopState, info string) {})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if res != "done" {
		t.Fatalf("expected answer 'done', got %q", res)
	}

	// Give the late goroutine time to fire. Before the snapshot fix this
	// panics inside the goroutine and crashes the test binary.
	time.Sleep(40 * time.Millisecond)

	// The engine must stay usable for subsequent turns (the handler is nil
	// now, so the plain path is taken — no goroutine, no panic).
	if _, err := engine.RunTurn(context.Background(), "again", nil); err != nil {
		t.Fatalf("second RunTurn failed: %v", err)
	}
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

func TestMinerModeToolGuard(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	adapter := &mockAdapter{
		toolCalls: []provider.ToolCall{
			{ID: "mw1", Name: "write_file", Arguments: `{"path":"test.txt","content":"hello"}`},
			{ID: "mb1", Name: "bash", Arguments: `{"command":"git log --oneline -5"}`},
			{ID: "mm1", Name: "memory", Arguments: `{"action":"retain","section":"Architecture","fact":"service -> repo -> DB"}`},
		},
	}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	engine.SetMode("MINER")
	if engine.Mode() != "MINER" {
		t.Fatalf("expected mode MINER, got %s", engine.Mode())
	}

	_, err := engine.RunTurn(context.Background(), "learn the codebase", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	msgs := ctxMgr.Messages()
	foundWriteGuard, foundBashOK, foundMemoryOK := false, false, false
	for _, msg := range msgs {
		switch msg.ToolCallID {
		case "mw1":
			if strings.Contains(msg.Content, "MINER GUARD") {
				foundWriteGuard = true
			}
		case "mb1":
			// Read-only bash is ALLOWED in MINER mode (no guard message; the
			// real bash tool ran and produced output or an error).
			foundBashOK = !strings.Contains(msg.Content, "MINER GUARD")
		case "mm1":
			// memory retain is the whole point of MINER — must not be blocked.
			foundMemoryOK = !strings.Contains(msg.Content, "MINER GUARD")
		}
	}
	if !foundWriteGuard {
		t.Error("write_file must be blocked in MINER mode")
	}
	if !foundBashOK {
		t.Error("read-only bash must NOT be blocked in MINER mode")
	}
	if !foundMemoryOK {
		t.Error("memory retain must NOT be blocked in MINER mode")
	}
}

func TestUsageRecorderCalledAtTurnEnd(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	adapter := &mockAdapter{
		toolCalls: []provider.ToolCall{
			{ID: "ur1", Name: "read_file", Arguments: `{"path":"internal/app/handler.go"}`},
			{ID: "ur2", Name: "edit_file", Arguments: `{"path":"internal/app/handler.go"}`},
		},
	}

	var recorded []string
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	engine.SetUsageRecorder(func(paths []string) { recorded = paths })

	if _, err := engine.RunTurn(context.Background(), "touch a file", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if len(recorded) == 0 {
		t.Fatal("usage recorder must receive touched files at turn end")
	}
	joined := strings.Join(recorded, ",")
	if !strings.Contains(joined, "internal/app/handler.go") {
		t.Errorf("expected handler.go in recorded paths, got %v", recorded)
	}
}

func TestAskUserToolFlow(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	var asked []tool.AskQuestion
	askTool := tools.Lookup("ask_user").(*tool.AskUserTool)
	askTool.Ask = func(_ context.Context, qs []tool.AskQuestion) ([]tool.AskResult, error) {
		asked = qs
		return []tool.AskResult{{Question: qs[0].Question, Answers: []string{"PostgreSQL"}}}, nil
	}

	adapter := &mockAdapter{
		toolCalls: []provider.ToolCall{
			{ID: "tc_ask", Name: "ask_user", Arguments: `{"questions":[{"question":"Which database?","options":["SQLite","PostgreSQL"]}]}`},
		},
	}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	res, err := engine.RunTurn(context.Background(), "set up a database", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if res != "Done testing" {
		t.Errorf("unexpected final answer: %s", res)
	}
	if len(asked) != 1 || asked[0].Question != "Which database?" {
		t.Errorf("ask handler not called with expected questions: %+v", asked)
	}

	// The answers must land in the context so the model sees them on the next loop iteration.
	found := false
	for _, msg := range ctxMgr.Messages() {
		if strings.Contains(msg.Content, "PostgreSQL") {
			found = true
		}
	}
	if !found {
		t.Error("expected user answer to be present in context after ask_user")
	}
}

func TestPermissionGateFlow(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	// Approve gated commands once when asked.
	tools.SetUserAskHandler(func(_ context.Context, qs []tool.AskQuestion) ([]tool.AskResult, error) {
		return []tool.AskResult{{Question: qs[0].Question, Answers: []string{"✅ Allow once"}}}, nil
	})

	adapter := &mockAdapter{
		toolCalls: []provider.ToolCall{
			{ID: "tc_rm", Name: "bash", Arguments: `{"command":"rm -rf /"}`},
			{ID: "tc_chmod", Name: "bash", Arguments: `{"command":"chmod +x fake.sh"}`},
		},
	}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	if _, err := engine.RunTurn(context.Background(), "test", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	denied := false
	run := false
	for _, msg := range ctxMgr.Messages() {
		if msg.ToolCallID == "tc_rm" && strings.Contains(msg.Content, "PERMISSION DENIED") {
			denied = true
		}
		if msg.ToolCallID == "tc_chmod" && strings.Contains(msg.Content, "Command") {
			run = true
		}
	}
	if !denied {
		t.Error("rm -rf / should have been hard-denied by the permission gate")
	}
	if !run {
		t.Error("approved gated command should have executed")
	}
}

func TestEngineFallbackOnPrimaryFailure(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	engine := NewEngine(&failAdapter{err: fmt.Errorf("provider down")}, tools, ctxMgr, "primary-model")
	engine.AddFallback(Fallback{Adapter: &mockAdapter{}, Model: "fallback-model"})

	res, err := engine.RunTurn(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if res != "Done testing" {
		t.Errorf("expected fallback model answer, got %q", res)
	}
}

func TestLastFallbackModelReported(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	engine := NewEngine(&failAdapter{err: fmt.Errorf("provider down")}, tools, ctxMgr, "primary-model")
	engine.AddFallback(Fallback{Adapter: &mockAdapter{}, Model: "fallback-model"})

	if _, err := engine.RunTurn(context.Background(), "hello", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if engine.LastFallbackModel() != "fallback-model" {
		t.Errorf("expected lastFallback=fallback-model, got %q", engine.LastFallbackModel())
	}
	// The primary failure reason must be recorded so the UI can tell the user
	// WHY the fallback happened (duration/queue limit, invalid model, ...).
	if !strings.Contains(engine.LastFallbackReason(), "provider down") {
		t.Errorf("expected fallback reason to mention primary error, got %q", engine.LastFallbackReason())
	}

	// A turn served by the primary provider resets the marker.
	engine2 := NewEngine(&mockAdapter{}, tools, bcontext.NewManager("s2", nil, 128000), "primary-model")
	if _, err := engine2.RunTurn(context.Background(), "hello", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if engine2.LastFallbackModel() != "" {
		t.Errorf("expected no fallback marker for primary-served turn, got %q", engine2.LastFallbackModel())
	}
	if engine2.LastFallbackReason() != "" {
		t.Errorf("expected no fallback reason for primary-served turn, got %q", engine2.LastFallbackReason())
	}
}

// repeatingAdapter always returns the SAME tool call, simulating a model that
// is stuck in a loop (re-running grep on the same file forever).
type repeatingAdapter struct {
	calls int
}

func (m *repeatingAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	m.calls++
	return &provider.CompletionResponse{
		Reasoning: "spinning",
		ToolCalls: []provider.ToolCall{
			{ID: "tc", Name: "grep", Arguments: `{"pattern":"filter"}`},
		},
	}, nil
}

// TestEngineLoopGuard ensures a model that repeats the exact same tool call is
// stopped: the loop must terminate with an answer (or error) instead of
// spinning through every iteration.
func TestEngineLoopGuard(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	adapter := &repeatingAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	engine.maxIterations = 25

	res, err := engine.RunTurn(context.Background(), "explore the filter code", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	// The loop guard should have stopped the repetition long before hitting
	// the iteration cap (maxIterations=25).
	if adapter.calls >= 15 {
		t.Fatalf("loop guard did not stop repetition: %d completions ran", adapter.calls)
	}

	// The turn must abort with a clear message instead of spinning forever.
	if !strings.Contains(res, "Turn aborted") {
		t.Errorf("expected loop-guard abort message, got %q", res)
	}
	if engine.State() != StateBlocked {
		t.Errorf("expected StateBlocked after loop guard, got %v", engine.State())
	}
}

// toolOnlyAdapter always returns tool calls (never an answer), simulating a
// model that explores forever without producing a final response. It re-greps
// the SAME path every round (no new file ever discovered) — the spinning
// case the tool budget must cut.
type toolOnlyAdapter struct {
	calls int
}

func (m *toolOnlyAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Tools) == 0 {
		return &provider.CompletionResponse{
			Content: "Turn aborted: What the agent was last working on: exploring",
		}, nil
	}
	m.calls++
	return &provider.CompletionResponse{
		Reasoning: "exploring",
		ToolCalls: []provider.ToolCall{
			{ID: "tc", Name: "grep", Arguments: fmt.Sprintf(`{"path":".","pattern":"filter%d"}`, m.calls)},
		},
	}, nil
}

// progressingToolAdapter also never answers, but discovers a NEW file every
// round — the legitimate deep-exploration case that deserves room to think.
type progressingToolAdapter struct {
	calls int
}

func (m *progressingToolAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Tools) == 0 {
		return &provider.CompletionResponse{
			Content: "Turn aborted: What the agent was last working on: exploring",
		}, nil
	}
	m.calls++
	return &provider.CompletionResponse{
		Reasoning: "exploring",
		ToolCalls: []provider.ToolCall{
			{ID: "tc", Name: "read_file", Arguments: fmt.Sprintf(`{"path":"file-%d.go"}`, m.calls)},
		},
	}, nil
}

// TestEngineToolBudget ensures a model that calls tools but never writes an
// answer is stopped at the tool-only budget instead of burning all 25
// iterations.
func TestEngineToolBudget(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	adapter := &toolOnlyAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	res, err := engine.RunTurn(context.Background(), "explore", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	// A SPINNING model (no new files) is cut at maxToolOnlyRounds; the
	// reminder rounds inject messages without calling the adapter.
	if adapter.calls != maxToolOnlyAbsolute {
		t.Fatalf("spinning model cut off at %d completions, want %d", adapter.calls, maxToolOnlyAbsolute)
	}
	if !strings.Contains(res, "Turn aborted") && !strings.Contains(res, "Tool Limit Tercapai") {
		t.Errorf("expected tool-budget graceful summary message, got %q", res)
	}
	// The abort must say WHAT the agent was stuck on (its last reasoning),
	// so the user can rephrase instead of staring at a file dump.
	if !strings.Contains(res, "What the agent was last working on: exploring") {
		t.Errorf("expected abort to include the agent's last reasoning, got %q", res)
	}
	if engine.State() != StateDone && engine.State() != StateBlocked {
		t.Errorf("expected terminal state, got %v", engine.State())
	}
	// The FIRST reminder must fire early (at toolWarnRounds, well before the
	// abort) and name the round/file counts — the rabbit-hole cut that saves
	// tokens instead of letting the loop burn to the abort.
	msgs := ctxMgr.Messages()
	found := false
	for _, msg := range msgs {
		if strings.Contains(msg.Content, "already examined") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected early 'already examined' reminder in context before abort")
	}
}

// rangeReadingAdapter never answers but reads DIFFERENT line ranges of the
// SAME file every round — the exact "I need lines 60-100" pattern the user
// kept hitting, where the model was methodically covering a large file and
// the budget wrongly treated same-path reads as spinning.
type rangeReadingAdapter struct {
	calls int
}

func (m *rangeReadingAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Tools) == 0 {
		return &provider.CompletionResponse{
			Content: "Turn aborted: range reading complete",
		}, nil
	}
	m.calls++
	return &provider.CompletionResponse{
		Reasoning: "exploring",
		ToolCalls: []provider.ToolCall{
			{ID: "tc", Name: "read_file", Arguments: fmt.Sprintf(`{"path":"big.js","start_line":%d,"end_line":%d}`, (m.calls-1)*50+1, m.calls*50)},
		},
	}, nil
}

// TestEngineToolBudgetRangeReadsAreProgress proves that reading DIFFERENT
// ranges of the SAME large file counts as genuine progress: the model must
// NOT be cut at maxToolOnlyRounds (the old behaviour that aborted the
// "I need lines 60-100" case) — it gets room up to the absolute cap.
func TestEngineToolBudgetRangeReadsAreProgress(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	adapter := &rangeReadingAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	_, err := engine.RunTurn(context.Background(), "explore big file", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	// Range reads of the same file must NOT be treated as spinning: the model
	// survives past maxToolOnlyRounds (only the absolute cap stops it).
	if adapter.calls <= maxToolOnlyRounds {
		t.Fatalf("range-reading model cut at %d calls (<= maxToolOnlyRounds %d): same-path range reads must count as progress", adapter.calls, maxToolOnlyRounds)
	}
	if adapter.calls > maxToolOnlyAbsolute {
		t.Fatalf("range-reading model exceeded absolute cap: %d > %d", adapter.calls, maxToolOnlyAbsolute)
	}
}

// bashExplorerAdapter never answers but runs a DIFFERENT bash command every
// round — the legitimate bash-based exploration pattern (git status, ls,
// grep -rn) that the prompts encourage. Previously only "find" commands were
// credited as progress, so bash explorers were miscounted as spinning and
// aborted early.
type bashExplorerAdapter struct {
	calls int
}

func (m *bashExplorerAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Tools) == 0 {
		return &provider.CompletionResponse{
			Content: "Turn aborted: bash exploration complete",
		}, nil
	}
	m.calls++
	return &provider.CompletionResponse{
		Reasoning: "exploring",
		ToolCalls: []provider.ToolCall{
			{ID: "tc", Name: "bash", Arguments: fmt.Sprintf(`{"command":"echo step-%d"}`, m.calls)},
		},
	}, nil
}

// TestEngineToolBudgetBashExplorationIsProgress proves that running DIFFERENT
// bash commands counts as genuine progress (not spinning): the model must
// survive past maxToolOnlyRounds, only the absolute cap stops it.
func TestEngineToolBudgetBashExplorationIsProgress(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	adapter := &bashExplorerAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	_, err := engine.RunTurn(context.Background(), "explore repo state", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if adapter.calls <= maxToolOnlyRounds {
		t.Fatalf("bash explorer cut at %d calls (<= maxToolOnlyRounds %d): distinct bash commands must count as progress", adapter.calls, maxToolOnlyRounds)
	}
	if adapter.calls > maxToolOnlyAbsolute {
		t.Fatalf("bash explorer exceeded absolute cap: %d > %d", adapter.calls, maxToolOnlyAbsolute)
	}
}

// TestEngineToolBudgetProgressing proves the adaptive budget: a model that
// keeps discovering NEW files (genuine deep exploration) is NOT cut at
// maxToolOnlyRounds — it gets room until the absolute cap, so an agent that
// is still gathering context is never forced to answer mid-thought.
func TestEngineToolBudgetProgressing(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	adapter := &progressingToolAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	res, err := engine.RunTurn(context.Background(), "explore", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	// Progressing model runs past maxToolOnlyRounds up to the absolute cap.
	if adapter.calls != maxToolOnlyAbsolute {
		t.Fatalf("progressing model cut off at %d completions, want absolute cap %d", adapter.calls, maxToolOnlyAbsolute)
	}
	if !strings.Contains(res, "Turn aborted") && !strings.Contains(res, "Tool Limit Tercapai") {
		t.Errorf("expected summary message, got %q", res)
	}
}

// TestEngineToolBudgetResetsAfterAnswer ensures a model that explores for a
// few rounds then answers is NOT cut off.
func TestEngineToolBudgetResetsAfterAnswer(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	// mockAdapter emits toolCalls once, then answers — well under the budget.
	adapter := &mockAdapter{
		toolCalls: []provider.ToolCall{
			{ID: "tc1", Name: "grep", Arguments: `{"pattern":"filter"}`},
		},
	}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	res, err := engine.RunTurn(context.Background(), "explore", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if res != "Done testing" {
		t.Errorf("expected normal answer, got %q", res)
	}
	if engine.State() != StateDone {
		t.Errorf("expected StateDone, got %v", engine.State())
	}
}

// captureAdapter records the system prompt of each request so tests can
// assert what was injected.
type captureAdapter struct {
	sysPrompt string
}

func (c *captureAdapter) Complete(_ context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		c.sysPrompt = req.Messages[0].Content
	}
	return &provider.CompletionResponse{Content: "Done"}, nil
}

func TestSkillsInjectedIntoSystemPrompt(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &captureAdapter{}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	engine.SetSkills("- go-build: Build and test Go projects\n- team-rule: Team convention")

	if _, err := engine.RunTurn(context.Background(), "hello", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !strings.Contains(adapter.sysPrompt, "AVAILABLE SKILLS:") {
		t.Error("expected AVAILABLE SKILLS block in system prompt")
	}
	if !strings.Contains(adapter.sysPrompt, "go-build: Build and test Go projects") {
		t.Error("expected skill listing in system prompt")
	}
	// Skills must never leak the opencode-specific dir into the prompt.
	if strings.Contains(adapter.sysPrompt, ".opencode") {
		t.Error("system prompt must not reference .opencode")
	}
}

// TestEnginePromptBatchingRule proves the BUILDER system prompt carries the
// cost-critical batching + consultant-posture rules to native-provider models
// (every round re-sends the whole conversation, so denser rounds are cheaper).
func TestEnginePromptBatchingRule(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &captureAdapter{}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	if _, err := engine.RunTurn(context.Background(), "hello", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	for _, want := range []string{"BATCH YOUR TOOL CALLS", "SENIOR CONSULTANT POSTURE"} {
		if !strings.Contains(adapter.sysPrompt, want) {
			t.Errorf("expected %q in BUILDER system prompt", want)
		}
	}
}

func TestMemoryWarmStartInjected(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &captureAdapter{}

	memDir := t.TempDir()
	mem := memory.NewStore(memDir)
	mem.Retain("Decisions", "Filter omnichannel: Semua abaikan status + PIC")

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	engine.SetMemoryStore(mem)

	if _, err := engine.RunTurn(context.Background(), "hello", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !strings.Contains(adapter.sysPrompt, "PROJECT MEMORY") {
		t.Error("expected PROJECT MEMORY block in system prompt")
	}
	if !strings.Contains(adapter.sysPrompt, "Filter omnichannel") {
		t.Error("expected warm-start memory content in system prompt")
	}
}

func TestNoSkillsNoBlock(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &captureAdapter{}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	engine.SetSkills("")

	if _, err := engine.RunTurn(context.Background(), "hello", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if strings.Contains(adapter.sysPrompt, "AVAILABLE SKILLS:") {
		t.Error("expected no AVAILABLE SKILLS block when skills are empty")
	}
}

func TestEngineNoFallbackFails(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)

	engine := NewEngine(&failAdapter{err: fmt.Errorf("provider down")}, tools, ctxMgr, "primary-model")
	if _, err := engine.RunTurn(context.Background(), "hello", nil); err == nil {
		t.Errorf("expected failure when primary fails and no fallback exists")
	}
}

// prefixCaptureAdapter records every completion request so the test can prove
// the system prompt and tool definitions are byte-identical across loop rounds
// — the precondition for prompt caching (Anthropic cache_control breakpoints
// and OpenAI's automatic prefix caching both key off an unchanged prefix).
type prefixCaptureAdapter struct {
	sysPrompts []string
	toolsJSON  []string
	calls      int
}

func (m *prefixCaptureAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	m.calls++
	// Marshal tools deterministically so the byte comparison is meaningful.
	tj, _ := json.Marshal(req.Tools)
	m.sysPrompts = append(m.sysPrompts, req.Messages[0].Content)
	m.toolsJSON = append(m.toolsJSON, string(tj))

	if m.calls == 1 {
		return &provider.CompletionResponse{
			Reasoning: "exploring",
			ToolCalls: []provider.ToolCall{
				{ID: "tc1", Name: "read_file", Arguments: `{"path":"a.go"}`},
				{ID: "tc2", Name: "grep", Arguments: `{"pattern":"foo"}`},
			},
		}, nil
	}
	return &provider.CompletionResponse{Content: "done", Reasoning: "finished"}, nil
}

// TestEngineStablePrefixAcrossRounds proves the exact precondition for prompt
// caching: across the tool round and the final answer round of ONE turn, the
// system prompt and tool definitions sent to the provider are byte-identical.
// Only the growing message history (after the cached prefix) changes. If this
// ever breaks (e.g. a timestamp, random counter, or mutation sneaks into the
// system prompt mid-turn), every cache hit is lost and each round pays full
// price again.
func TestEngineStablePrefixAcrossRounds(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &prefixCaptureAdapter{}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	if _, err := engine.RunTurn(context.Background(), "explore the codebase", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if adapter.calls < 2 {
		t.Fatalf("expected at least 2 completions (tool round + answer), got %d", adapter.calls)
	}
	baseSys := adapter.sysPrompts[0]
	baseTools := adapter.toolsJSON[0]
	for i := 1; i < len(adapter.sysPrompts); i++ {
		if adapter.sysPrompts[i] != baseSys {
			t.Errorf("system prompt changed between round 1 and %d — cache prefix broken", i+1)
		}
		if adapter.toolsJSON[i] != baseTools {
			t.Errorf("tool definitions changed between round 1 and %d — cache prefix broken", i+1)
		}
	}
}

// TestEngineStablePrefixAcrossTurns proves the same prefix stability ACROSS
// turns in one session: turn 2 must re-send the same system prompt and tools
// as turn 1 (the messages differ, but the cacheable prefix must not).
func TestEngineStablePrefixAcrossTurns(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &prefixCaptureAdapter{}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	if _, err := engine.RunTurn(context.Background(), "first question", nil); err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}
	if _, err := engine.RunTurn(context.Background(), "second question", nil); err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}
	if len(adapter.sysPrompts) < 2 {
		t.Fatalf("expected system prompts from both turns, got %d", len(adapter.sysPrompts))
	}
	if adapter.sysPrompts[0] != adapter.sysPrompts[len(adapter.sysPrompts)-1] {
		t.Error("system prompt must be byte-identical across turns in a session")
	}
	if adapter.toolsJSON[0] != adapter.toolsJSON[len(adapter.toolsJSON)-1] {
		t.Error("tool definitions must be byte-identical across turns in a session")
	}
}
