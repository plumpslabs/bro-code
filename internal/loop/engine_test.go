package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	// A SPINNING model (no new files) is cut at the tool-only budget, well
	// below the 25-iteration cap; the reminder rounds inject messages without
	// calling the adapter.
	if adapter.calls > maxToolOnlyRounds {
		t.Fatalf("spinning model not cut at tool-only budget: %d completions (max %d)", adapter.calls, maxToolOnlyRounds)
	}
	if adapter.calls <= toolWarnRounds {
		t.Fatalf("spinning model cut too early (%d <= warn %d) — guard fired before any real exploration", adapter.calls, toolWarnRounds)
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
	// gets room past the first reminder (only the absolute cap stops it).
	if adapter.calls <= toolWarnRounds {
		t.Fatalf("range-reading model cut too early (%d <= warn %d): same-path range reads must count as progress", adapter.calls, toolWarnRounds)
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
	if adapter.calls <= toolWarnRounds {
		t.Fatalf("bash explorer cut too early (%d <= warn %d): distinct bash commands must count as progress", adapter.calls, toolWarnRounds)
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
	// Progressing model (new file every round) gets room past the first
	// reminder but is still bounded by the absolute cap — never burns all 25.
	if adapter.calls <= toolWarnRounds {
		t.Fatalf("progressing model cut too early (%d <= warn %d): new-file exploration must get room", adapter.calls, toolWarnRounds)
	}
	if adapter.calls > maxToolOnlyAbsolute {
		t.Fatalf("progressing model exceeded absolute cap: %d > %d", adapter.calls, maxToolOnlyAbsolute)
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
// cost-critical batching + lean-exploration rules to native-provider models
// (every round re-sends the whole conversation, so denser rounds are cheaper).
// The rules were consolidated in the prompt-slimming pass; assert the merged
// headlines.
func TestEnginePromptBatchingRule(t *testing.T) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &captureAdapter{}

	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	if _, err := engine.RunTurn(context.Background(), "hello", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	for _, want := range []string{"BATCH & STAY LEAN", "EXPLORE BEFORE ANSWERING", "ANSWER PROPORTIONATELY"} {
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

type spinningToolAdapter struct {
	calls int
}

func (s *spinningToolAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	s.calls++
	if len(req.Tools) > 0 {
		return &provider.CompletionResponse{
			ToolCalls: []provider.ToolCall{{ID: fmt.Sprintf("call_%d", s.calls), Name: "read_file", Arguments: `{"path":"a.txt"}`}},
		}, nil
	}
	return &provider.CompletionResponse{Content: "Finished synthesis"}, nil
}

func TestInteractiveTurnBudgetExtensionGate(t *testing.T) {
	tools := tool.NewRegistry()
	tools.Register(&tool.ReadFileTool{})
	ctxMgr := bcontext.NewManager("test_extension", nil, 128000)

	adapter := &spinningToolAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")
	engine.SetMaxIterations(2)

	asked := false
	engine.SetAskHandler(func(question string, options []string) (string, error) {
		asked = true
		return "Allow Once (+15 turns)", nil
	})

	_, err := engine.RunTurn(context.Background(), "do work", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !asked {
		t.Errorf("expected askHandler to be triggered when maxIterations reached")
	}
	if engine.maxIterations < 17 {
		t.Errorf("expected maxIterations to be extended to at least 17, got %d", engine.maxIterations)
	}
}

func TestRejectExtensionIsScopedPerTurn(t *testing.T) {
	tools := tool.NewRegistry()
	tools.Register(&tool.ReadFileTool{})
	ctxMgr := bcontext.NewManager("test_reject_scope", nil, 128000)

	adapter := &spinningToolAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	askCount := 0
	engine.SetAskHandler(func(question string, options []string) (string, error) {
		askCount++
		return "Reject & Synthesize Now", nil
	})

	// Turn 1: hits limit, user rejects extension. Turn 1 synthesizes and completes.
	engine.SetMaxIterations(2)
	_, err := engine.RunTurn(context.Background(), "turn 1", nil)
	if err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}
	if askCount != 1 {
		t.Fatalf("expected askCount = 1 after turn 1 reject, got %d", askCount)
	}

	// Turn 2: hits limit again. Ask handler MUST be called again!
	engine.SetMaxIterations(2)
	_, err = engine.RunTurn(context.Background(), "turn 2", nil)
	if err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}
	if askCount != 2 {
		t.Fatalf("expected askCount = 2 after turn 2 (Reject must NOT permanently lock the session), got %d", askCount)
	}
}

// fakeTool is a minimal Tool used to exercise toolsForMode trimming.
type fakeTool struct {
	name string
	desc string
}

func (f fakeTool) Name() string                                { return f.name }
func (f fakeTool) Description() string                        { return f.desc }
func (f fakeTool) Parameters() map[string]any                 { return map[string]any{} }
func (f fakeTool) Execute(ctx context.Context, a string) (string, error) { return "", nil }

// TestToolDescBudgetTrims verifies the tool-description lean budget (P5) trims
// each tool's schema in the request so more of the window is free for real task
// context. 0 budget = untouched.
func TestToolDescBudgetTrims(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(fakeTool{name: "big_tool", desc: strings.Repeat("very long description ", 50)})
	eng := NewEngine(&mockAdapter{}, reg, bcontext.NewManager("s", nil, 128000), "m")

	// No budget: full description preserved.
	full := eng.toolsForMode("BUILDER")
	var fullBig string
	for _, d := range full {
		if d.Name == "big_tool" {
			fullBig = d.Description
		}
	}
	if len(fullBig) <= 200 {
		t.Fatalf("expected full long description for big_tool, got len=%d", len(fullBig))
	}

	// Budget of 40 chars: description must be trimmed + ellipsized.
	eng.SetToolDescBudget(40)
	trimmed := eng.toolsForMode("BUILDER")
	var trimmedBig string
	for _, d := range trimmed {
		if d.Name == "big_tool" {
			trimmedBig = d.Description
		}
	}
	if len(trimmedBig) >= len(fullBig) {
		t.Fatalf("expected trimmed description to be shorter than %d, got %d: %q", len(fullBig), len(trimmedBig), trimmedBig)
	}
	if !strings.HasSuffix(trimmedBig, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", trimmedBig)
	}
}

// TestLooksLikeLSPFixTask verifies the pre-flight trigger only fires on
// diagnostic/fix-lint phrasing, not generic "build a feature" requests.
func TestLooksLikeLSPFixTask(t *testing.T) {
	yes := []string{
		"fix all the warnings in the project",
		"clean up the lint errors",
		"resolve the deprecation warnings",
		"run lsp_scan and fix diagnostics",
		"fix type errors from go vet",
	}
	for _, q := range yes {
		if !looksLikeLSPFixTask(q) {
			t.Errorf("expected LSP-fix intent for %q", q)
		}
	}
	no := []string{
		"build a login feature",
		"add a new endpoint for users",
		"explain how the cache works",
		"write tests for the parser",
	}
	for _, q := range no {
		if looksLikeLSPFixTask(q) {
			t.Errorf("did NOT expect LSP-fix intent for %q", q)
		}
	}
}

// TestReadLinesWindow verifies best-effort line-window extraction (1-indexed,
// clamped) and graceful empty result on missing files.
func TestReadLinesWindow(t *testing.T) {
	f := t.TempDir() + "/sample.txt"
	content := "zero\none\ntwo\nthree\nfour\n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readLinesWindow(f, 2, 4)
	want := "2: one\n3: two\n4: three"
	if got != want {
		t.Fatalf("readLinesWindow(2,4) = %q, want %q", got, want)
	}
	// Clamped lower bound.
	if got := readLinesWindow(f, -5, 1); got != "1: zero" {
		t.Fatalf("clamp low = %q", got)
	}
	// Missing file -> empty (never errors).
	if got := readLinesWindow("/no/such/file.txt", 1, 3); got != "" {
		t.Fatalf("missing file should return empty, got %q", got)
	}
}

// TestBuildSystemPromptIncludesPreflight verifies the pre-flight packed block is
// injected into the first prompt so diagnostics reach the model in shot 1.
func TestBuildSystemPromptIncludesPreflight(t *testing.T) {
	ctxMgr := bcontext.NewManager("sess", nil, 128000)
	eng := NewEngine(&mockAdapter{}, tool.NewRegistry(), ctxMgr, "test-model")
	eng.lspAvailable = 1
	eng.preflightBlock = "PRE-GATHERED LSP DIAGNOSTICS:\n  error 12:5  unused variable\n--- internal/foo.go:12 ---\n12: x := 1"

	prompt := eng.buildSystemPrompt("BUILDER", 1, nil)
	if !strings.Contains(prompt, "PRE-GATHERED LSP DIAGNOSTICS") {
		t.Errorf("preflight block missing from system prompt:\n%s", prompt)
	}
	// A later iteration must still include it (stable cached prefix uses the same
	// build, but guard against accidental omission on iteration>1 path).
	prompt2 := eng.buildSystemPrompt("BUILDER", 2, nil)
	if !strings.Contains(prompt2, "PRE-GATHERED LSP DIAGNOSTICS") {
		t.Errorf("preflight block missing on iteration 2")
	}
}

// TestLooksLikeImplTask verifies the plan-then-act trigger fires on clearly
// multi-step build/implement phrasing but NOT on read/question prompts, single-shot
// fixes, or small single-file edits (which run with minimal ceremony).
func TestLooksLikeImplTask(t *testing.T) {
	yes := []string{
		"implement a login endpoint",
		"build a caching layer for the API",
		"create a new user service",
		"scaffold a new Go package",
		"set up a CI pipeline",
		"introduce a rate-limiting middleware",
	}
	for _, q := range yes {
		if !looksLikeImplTask(q) {
			t.Errorf("expected impl intent for %q", q)
		}
	}
	no := []string{
		"explain how the cache works",
		"what does read_file do?",
		"show me the parser",
		"fix all the lint warnings",            // handled by pre-flight, not plan
		"add a small helper",                   // small edit: runs directly
		"refactor main.go to add a small helper", // single-file: runs directly
		"list the files in internal/loop",
		"how do I run the tests?",
	}
	for _, q := range no {
		if looksLikeImplTask(q) {
			t.Errorf("did NOT expect impl intent for %q", q)
		}
	}
}

// TestPlanModeDirectiveInSystemPrompt verifies the PLAN MODE guard text is
// injected into the first prompt when the engine is gating an implementation task.
func TestPlanModeDirectiveInSystemPrompt(t *testing.T) {
	ctxMgr := bcontext.NewManager("sess", nil, 128000)
	eng := NewEngine(&mockAdapter{}, tool.NewRegistry(), ctxMgr, "test-model")
	eng.planMode = true
	prompt := eng.buildSystemPrompt("BUILDER", 1, nil)
	if !strings.Contains(prompt, "PLAN MODE") {
		t.Errorf("expected PLAN MODE directive in system prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "ask_user") {
		t.Errorf("expected plan-mode to instruct ask_user confirmation")
	}
}

// TestLooksLikeLSPFixTaskLanguageAgnostic verifies the pre-flight trigger fires
// for Indonesian fix prompts (the user's actual phrasing), not just English.
func TestLooksLikeLSPFixTaskLanguageAgnostic(t *testing.T) {
	yes := []string{
		"perbaiki smua warning depreceted dll", // user's exact prompt (misspelled)
		"betulkan semua error di project",
		"baiki deprecated API di client.go",
		"beresin lint warnings",
		"bersihkan warnings",
		// Check/verify prompts mentioning diagnostics must also trigger pre-flight
		// (otherwise the model re-reads whole files just to verify status).
		"cek lagi apkah smua warning udh di solved?",
		"check if all errors are resolved",
		"verify the lint warnings are gone",
	}
	for _, q := range yes {
		if !looksLikeLSPFixTask(q) {
			t.Errorf("expected LSP-fix intent for %q", q)
		}
	}
	// Still must NOT fire on plain build/question prompts (no fix/check verb with
	// a diagnostic noun, or no diagnostic noun at all).
	no := []string{
		"implement a login endpoint",
		"jelaskan cara kerja cache",
		"build a caching layer",
		"why did this error happen", // question, no fix/check verb
	}
	for _, q := range no {
		if looksLikeLSPFixTask(q) {
			t.Errorf("did NOT expect LSP-fix intent for %q", q)
		}
	}
}

func TestGuardPreflightRedundant(t *testing.T) {
	e := &Engine{}
	// No pre-flight diagnostics -> reads and lsp_scan allowed.
	if e.guardPreflightRedundant("read_file", `{"path":"x.go"}`) != "" {
		t.Fatal("read must be allowed when no pre-flight block is present")
	}
	if e.guardPreflightRedundant("lsp_scan", `{}`) != "" {
		t.Fatal("lsp_scan must be allowed when no pre-flight block is present")
	}
	// Pre-flight diagnostics present, but the read target is NOT in them -> allowed.
	e.preflightActive = true
	e.preflightBlock = "PRE-GATHERED LSP DIAGNOSTICS\ninternal/lsp/client.go:10: unused"
	if got := e.guardPreflightRedundant("read_file", `{"path":"other.go"}`); got != "" {
		t.Fatalf("read of a file NOT in the packed diagnostics must be allowed, got block: %q", got)
	}
	// Reading a file whose diagnostics are already packed -> blocked (whole-file).
	if got := e.guardPreflightRedundant("read_file", `{"path":"internal/lsp/client.go"}`); got == "" {
		t.Fatal("read of a packed diagnostic file must be blocked")
	}
	// Line-range read of a packed diagnostic file -> ALSO blocked (window is packed).
	if got := e.guardPreflightRedundant("read_file", `{"path":"internal/lsp/client.go","start_line":1,"end_line":3}`); got == "" {
		t.Fatal("line-range read of a packed diagnostic file must be blocked")
	}
	// Redundant lsp_scan re-call -> blocked.
	if got := e.guardPreflightRedundant("lsp_scan", `{}`); got == "" {
		t.Fatal("redundant lsp_scan after pre-flight must be blocked")
	}
}

func TestCumulativeChangeDiff(t *testing.T) {
	tool.ResetChanges()
	p := t.TempDir() + "/f.go"
	tool.RecordChange(tool.FileChange{Path: p, Action: "modified", Old: "a\nb\n", New: "a\nc\n"})
	got := tool.CumulativeChangeDiff(p)
	if !strings.Contains(got, "+c") || !strings.Contains(got, "-b") {
		t.Fatalf("CumulativeChangeDiff = %q, want +/- markers", got)
	}
	// Unknown path -> empty.
	if tool.CumulativeChangeDiff(t.TempDir()+"/nope") != "" {
		t.Fatal("expected empty diff for unknown path")
	}
	// Repeated edits to the same path fold into ONE diff from the ORIGINAL
	// content to the LATEST content (cumulative), not a diff per edit.
	tool.ResetChanges()
	tool.RecordChange(tool.FileChange{Path: p, Action: "modified", Old: "a\nb\n", New: "a\nc\n"})
	tool.RecordChange(tool.FileChange{Path: p, Action: "modified", Old: "a\nc\n", New: "a\nd\n"})
	cum := tool.CumulativeChangeDiff(p)
	if !strings.Contains(cum, "+d") {
		t.Fatalf("cumulative diff must include the latest content, got %q", cum)
	}
	if strings.Contains(cum, "+c") {
		t.Fatalf("cumulative diff must NOT re-show intermediate content, got %q", cum)
	}
	// created → modified keeps the + view of the whole final file.
	tool.ResetChanges()
	tool.RecordChange(tool.FileChange{Path: p, Action: "created", Old: "", New: "x\n"})
	tool.RecordChange(tool.FileChange{Path: p, Action: "modified", Old: "x\n", New: "x\ny\n"})
	if got := tool.CumulativeChangeDiff(p); !strings.Contains(got, "+ x") || !strings.Contains(got, "+ y") {
		t.Fatalf("created→modified must render a whole-file add, got %q", got)
	}
	// modified → deleted renders the deleted content with - markers.
	tool.ResetChanges()
	tool.RecordChange(tool.FileChange{Path: p, Action: "modified", Old: "a\nb\n", New: "a\nb\nc\n"})
	tool.RecordChange(tool.FileChange{Path: p, Action: "deleted", Old: "a\nb\nc\n", New: ""})
	if got := tool.CumulativeChangeDiff(p); !strings.Contains(got, "- a") || !strings.Contains(got, "- c") {
		t.Fatalf("modified→deleted must render - markers, got %q", got)
	}
}
