package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/mcp"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/tokens"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// Simulates the real event sequence: user sends prompt, progress messages
// stream in, then the turn result arrives, and finally a late "Completed"
// progress message lands (as happens with the opencode CLI adapter whose
// stderr goroutine may outlive the turn). The user's prompt must stay visible
// and the status must settle to "Ready".
// TestAuditMemoryFootprint measures exact heap and sys memory at startup and idle.
func TestAuditMemoryFootprint(t *testing.T) {
	var m0, m1, m2 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	app := newTestApp()
	runtime.ReadMemStats(&m1)

	// Trigger BPE tokenizer
	_ = tokens.CountTokens("Test prompt with some long code content to test BPE token table allocations", "gpt-4o")
	runtime.ReadMemStats(&m2)

	t.Logf("Baseline HeapAlloc: %.2f MB, HeapSys: %.2f MB, Sys: %.2f MB",
		float64(m0.HeapAlloc)/(1024*1024), float64(m0.HeapSys)/(1024*1024), float64(m0.Sys)/(1024*1024))
	t.Logf("After App Init HeapAlloc: %.2f MB, HeapSys: %.2f MB, Sys: %.2f MB",
		float64(m1.HeapAlloc)/(1024*1024), float64(m1.HeapSys)/(1024*1024), float64(m1.Sys)/(1024*1024))
	t.Logf("After BPE Tokenizer HeapAlloc: %.2f MB, HeapSys: %.2f MB, Sys: %.2f MB",
		float64(m2.HeapAlloc)/(1024*1024), float64(m2.HeapSys)/(1024*1024), float64(m2.Sys)/(1024*1024))

	_ = app
}

func TestTurnFlowKeepsHistoryAndSettlesStatus(t *testing.T) {
	m := newTestApp()
	m.width = 140
	m.height = 40

	// 1. User sends a prompt (handleEnter equivalent).
	m.messages = append(m.messages, "YOU:\nok bro sesuaikan filter omnichannel")
	m.status = "Thinking..."

	// 2. Progress messages stream in from the engine via the Update loop.
	// These must land in the live activity slot (above the input), NOT in the
	// conversation history.
	for _, p := range []string{
		"Turn 1 reasoning...",
		"Thinking & analyzing request...",
		"grep (pattern: 'filter')",
		"read_file (OmnichannelPanel.tsx)",
	} {
		if _, err := m.Update(stepProgressMsg{info: p}); err != nil {
			t.Fatalf("step progress update failed: %v", err)
		}
	}
	if len(m.activity) != 4 {
		t.Fatalf("expected 4 activity steps, got %d: %v", len(m.activity), m.activity)
	}
	if len(m.messages) != 2 { // 1 initial banner + 1 YOU — progress must not add more
		t.Fatalf("progress polluted history: expected 2 messages, got %d", len(m.messages))
	}

	// 3. Turn result arrives (long answer).
	answer := "BROCODE:\n💭 analysis\n\n# Analisis Filter\n\n" + longAnswer(60)
	m.messages = append(m.messages, answer)
	m.status = "Ready"

	// 4. Late "Completed" progress from the adapter's stderr goroutine must be
	// ignored once the turn has settled.
	if _, err := m.Update(stepProgressMsg{info: "Completed"}); err != nil {
		t.Fatalf("late progress update failed: %v", err)
	}
	if m.status != "Ready" {
		t.Fatalf("late progress clobbered status: got %q, want Ready", m.status)
	}

	// Process the window-size message so the viewport is sized (mimics the
	// real app where Bubble Tea sends it on startup).
	if _, err := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}

	// The log must still contain the user prompt and NO progress rows.
	log := m.buildLog(m.width - 4)
	if !contains(log, "ok bro sesuaikan filter omnichannel") {
		t.Fatal("user prompt content missing from log")
	}
	if contains(log, "PROCESS:") || contains(log, "⚡ Turn 1 reasoning") {
		t.Fatal("progress steps leaked into conversation history")
	}

	// Render through the REAL View() path — this is what the user sees. A long
	// answer taller than the viewport now parks at the START of the answer
	// (not the bottom), so the reader begins at its beginning and pages down —
	// the answer no longer looks "cut off" with its start hidden above the
	// fold. The prompt stays in the log, reachable by scrolling up.
	v := m.View()
	// Glamour interleaves ANSI codes mid-word, so strip them before asserting
	// on the rendered text (same as the badge/FILES summary tests).
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	answerStart := "💭 analysis"
	if !contains(visible, answerStart) {
		t.Fatalf("start of long answer not visible after park-at-top: viewport YOffset=%d", m.logViewport.YOffset())
	}
	if contains(visible, "⚡ Completed") {
		t.Fatalf("trailing 'Completed' progress leaked into visible history")
	}
}

// TestModeCycleThreeModes verifies Shift+Tab cycles BUILDER → PLANNER →
// MINER → BUILDER and the engine mode follows.
func TestModeCycleThreeModes(t *testing.T) {
	m := newTestApp()
	if m.mode != "BUILDER" {
		t.Fatalf("expected default BUILDER, got %s", m.mode)
	}

	shiftTab := func() {
		if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}); err != nil {
			t.Fatalf("shift+tab update failed: %v", err)
		}
	}

	shiftTab()
	if m.mode != "PLANNER" || m.engine.Mode() != "PLANNER" {
		t.Errorf("expected PLANNER after 1st tab, got %s / %s", m.mode, m.engine.Mode())
	}
	shiftTab()
	if m.mode != "MINER" || m.engine.Mode() != "MINER" {
		t.Errorf("expected MINER after 2nd tab, got %s / %s", m.mode, m.engine.Mode())
	}
	shiftTab()
	if m.mode != "BUILDER" || m.engine.Mode() != "BUILDER" {
		t.Errorf("expected BUILDER after 3rd tab (wrap), got %s / %s", m.mode, m.engine.Mode())
	}
}

// TestBareTabDoesNotChangeMode guards against a regression where a plain Tab
// (no Shift) flipped the mode. Mode switching is Shift+Tab ONLY — a bare Tab
// is reserved for in-modal navigation and must be a no-op otherwise.
func TestBareTabDoesNotChangeMode(t *testing.T) {
	m := newTestApp()
	if m.mode != "BUILDER" {
		t.Fatalf("expected default BUILDER, got %s", m.mode)
	}
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyTab}); err != nil {
		t.Fatalf("tab update failed: %v", err)
	}
	if m.mode != "BUILDER" {
		t.Errorf("bare Tab must not change mode, got %s", m.mode)
	}
}

// TestActivityShownLiveDuringTurn verifies the spinner + steps are rendered in
// the status slot above the input while a turn runs, and clear after it ends.
func TestActivityShownLiveDuringTurn(t *testing.T) {
	m := newTestApp()
	if _, err := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}

	// No turn running → no activity rows in the status slot. (The footer also
	// contains "· ", so match the distinct two-space indent of activity rows.)
	v := m.View()
	if contains(v.Content, "  · ") {
		t.Fatal("activity rows shown when idle")
	}

	// Start a turn and stream a couple of steps.
	m.status = "Thinking..." // mimics handleEnter before the turn starts
	if _, err := m.Update(stepProgressMsg{info: "grep (pattern: 'filter')"}); err != nil {
		t.Fatalf("step progress update failed: %v", err)
	}
	if _, err := m.Update(stepProgressMsg{info: "read_file (OmnichannelPanel.tsx)"}); err != nil {
		t.Fatalf("step progress update failed: %v", err)
	}

	v = m.View()
	if !contains(v.Content, "grep (pattern: 'filter')") {
		t.Fatalf("live activity step not visible in status slot")
	}
	if !contains(v.Content, "read_file (OmnichannelPanel.tsx)") {
		t.Fatalf("live activity step not visible in status slot")
	}
}

// TestQuitCancelsRunningTurn verifies that quitting (ctrl+c) cancels the
// in-flight turn and sets the quitting flag, so the turn goroutine stops
// sending to the exiting program (no goroutine leak on quit mid-turn).
func TestQuitCancelsRunningTurn(t *testing.T) {
	m := newTestApp()
	m.status = "Thinking..."
	canceled := false
	m.cancelTurn = func() { canceled = true }

	// The second return is the tea.Cmd — tea.Quit on quit, which is expected.
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("expected a quit cmd after ctrl+c")
	}
	if !m.quitting {
		t.Fatal("expected quitting flag after ctrl+c")
	}
	if !canceled {
		t.Fatal("expected in-flight turn to be canceled on quit")
	}
}

// TestInterruptedTurnIsNotAnError verifies that a user interrupt (ESC) does
// not surface the resulting "context canceled" error as an ERROR row.
func TestInterruptedTurnIsNotAnError(t *testing.T) {
	m := newTestApp()

	// User presses ESC while a turn is running.
	m.status = "Thinking..."
	m.turnRunning = true
	m.cancelTurn = func() {}
	// KeyPressMsg is an alias for Key; KeyEscape renders as "esc".
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}); err != nil {
		t.Fatalf("esc key update failed: %v", err)
	}
	if m.turnRunning {
		t.Fatal("expected turnRunning=false after ESC")
	}

	// The in-flight turn returns "context canceled".
	if _, err := m.Update(turnResultMsg{err: fmt.Errorf("http request failed: context canceled")}); err != nil {
		t.Fatalf("turn result update failed: %v", err)
	}

	// No ERROR row may be added, and status must be Ready.
	for _, msg := range m.messages {
		if strings.HasPrefix(msg, "ERROR:") {
			t.Fatalf("interrupt surfaced as error: %q", msg)
		}
	}
	if m.status != "Ready" {
		t.Fatalf("expected Ready after interrupt, got %q", m.status)
	}
}

// TestInterruptedPartialAnswerKept verifies that when a turn is interrupted
// (ESC) after the model already streamed some text, that partial text stays in
// the conversation history — labeled as partial — instead of vanishing. This
// keeps the chat connected: an entry the user saw appear must not disappear.
func TestInterruptedPartialAnswerKept(t *testing.T) {
	m := newTestApp()

	// The answer starts streaming.
	m.streaming = true
	m.pendingStream = "Starting to answer... and cut off mid sentence"
	m.status = "Thinking..."
	m.cancelTurn = func() {}

	// User presses ESC.
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}); err != nil {
		t.Fatalf("esc key update failed: %v", err)
	}

	// The in-flight turn returns "context canceled".
	if _, err := m.Update(turnResultMsg{err: fmt.Errorf("http request failed: context canceled")}); err != nil {
		t.Fatalf("turn result update failed: %v", err)
	}

	found := false
	for _, msg := range m.messages {
		if strings.Contains(msg, "Starting to answer... and cut off") {
			found = true
			if !strings.Contains(msg, "interrupted") {
				t.Fatalf("partial answer kept but not labeled as partial: %q", msg)
			}
		}
	}
	if !found {
		t.Fatal("interrupted partial answer was dropped from history")
	}
	if m.pendingStream != "" {
		t.Fatal("pendingStream not cleared after turn result")
	}
}

// TestInterruptThenEnterRunsImmediately verifies that when a turn is interrupted
// by ESC, typing a prompt and pressing ENTER immediately runs the new turn
// instead of queuing it. It also verifies that late-arriving results from the
// cancelled turn do NOT clobber the new turn.
func TestInterruptThenEnterRunsImmediately(t *testing.T) {
	m := newTestApp()

	// 1. Start first turn
	m.promptInput.SetValue("first question")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.turnRunning {
		t.Fatal("expected turnRunning after first prompt")
	}
	firstGen := m.turnGen

	// 2. User presses ESC to interrupt
	m.cancelTurn = func() {}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.turnRunning {
		t.Fatal("expected turnRunning=false immediately after ESC")
	}
	if m.status != "Ready" {
		t.Fatalf("expected status Ready after ESC, got %q", m.status)
	}

	// 3. User types second prompt and presses Enter
	m.promptInput.SetValue("second question")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.turnRunning {
		t.Fatal("expected second prompt to start running immediately, not queue")
	}
	if len(m.pendingQueue) != 0 {
		t.Fatalf("expected empty queue, but got %v", m.pendingQueue)
	}

	// 4. Stale turnResultMsg from first cancelled turn arrives
	m.Update(turnResultMsg{err: fmt.Errorf("context canceled"), gen: firstGen})
	// It must NOT stop the second turn!
	if !m.turnRunning {
		t.Fatal("stale turnResultMsg clobbered the second turn's turnRunning state")
	}

	// 5. Second turn completes
	m.Update(turnResultMsg{content: "second answer", gen: m.turnGen})
	if m.turnRunning {
		t.Fatal("expected turnRunning=false after second turn completed")
	}
}

// TestEmptyAnswerSurfaced verifies that a turn completing with an empty model
// response never leaves the UI stuck on "Thinking...": a notice is appended
// and status settles to Ready.
func TestEmptyAnswerSurfaced(t *testing.T) {
	m := newTestApp()
	m.status = "Thinking..."
	m.streaming = true
	m.pendingStream = ""

	if _, err := m.Update(turnResultMsg{content: ""}); err != nil {
		t.Fatalf("empty turn result update failed: %v", err)
	}
	found := false
	for _, msg := range m.messages {
		if strings.Contains(msg, "Empty Response") || strings.Contains(msg, "empty response") || strings.Contains(msg, "Model Returned Empty") {
			found = true
		}
	}
	if !found {
		t.Fatal("empty response was not surfaced in history")
	}
	if m.status != "Ready" {
		t.Fatalf("status = %q, want Ready", m.status)
	}
	if m.streaming {
		t.Fatal("streaming must be cleared after an empty turn")
	}
}

// TestTurnQueueOneAtATime verifies a prompt sent while a turn is in flight is
// queued (never run concurrently — that clobbers engine state and caused the
// nil-handler panic) and auto-sends when the current turn finishes.
func TestTurnQueueOneAtATime(t *testing.T) {
	m := newTestApp()

	// First prompt starts a turn.
	m.promptInput.SetValue("first")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.turnRunning {
		t.Fatal("expected turnRunning after first send")
	}

	// Second prompt while the turn is in flight must be queued, not run.
	m.promptInput.SetValue("second")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.pendingQueue) != 1 || m.pendingQueue[0].Text != "second" {
		t.Fatalf("expected 'second' queued, got %v", m.pendingQueue)
	}
	if m.status != "Queued..." {
		t.Fatalf("expected Queued status, got %q", m.status)
	}

	// Turn finishes → the queued prompt auto-starts (one at a time). The
	// returned Cmd is the batch that runs the next turn — non-nil by design.
	m.Update(turnResultMsg{content: "answer", err: nil, mode: "BUILDER"})
	if len(m.pendingQueue) != 0 {
		t.Fatalf("queue should be empty after drain, got %v", m.pendingQueue)
	}
	if !m.turnRunning {
		t.Fatal("expected next turn running after queue drain")
	}
	last := m.messages[len(m.messages)-1]
	if !strings.HasPrefix(last, "YOU:\nsecond") {
		t.Fatalf("expected queued prompt auto-sent, last message %q", last)
	}
}

// TestFileConfirmBarFlow verifies the input-bar file-action confirm: the bar
// replaces the input, ENTER submits the chosen option, and the broker receives
// the decision (allow / always / discard).
func TestFileConfirmBarFlow(t *testing.T) {
	m := newTestApp()

	// A pending create_file confirm opens the bar.
	m.Update(fileConfirmMsg{id: "f1", kind: "create_file", path: "src/new.ts"})
	if !m.showFileConfirm {
		t.Fatal("expected file confirm bar to open")
	}
	if m.fileConfirmKind != "create_file" || m.fileConfirmPath != "src/new.ts" {
		t.Fatalf("unexpected confirm state: %+v", m.fileConfirm)
	}

	// Default selection is Allow once — ENTER answers with Allow=true.
	got := make(chan tool.FileActionDecision, 1)
	m.fileConfirm.pending["f1"] = got
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case d := <-got:
		if !d.Allow || d.Always {
			t.Fatalf("expected Allow once, got %+v", d)
		}
	default:
		t.Fatal("broker did not receive the decision")
	}
	if m.showFileConfirm {
		t.Fatal("confirm bar must close after submit")
	}

	// Always allow via the '2' key.
	m.Update(fileConfirmMsg{id: "f2", kind: "delete_file", path: "src/old.ts"})
	got2 := make(chan tool.FileActionDecision, 1)
	m.fileConfirm.pending["f2"] = got2
	m.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case d := <-got2:
		if !d.Allow || !d.Always {
			t.Fatalf("expected Always allow, got %+v", d)
		}
	default:
		t.Fatal("broker did not receive the always decision")
	}

	// Discard via ESC.
	m.Update(fileConfirmMsg{id: "f3", kind: "delete_file", path: "src/other.ts"})
	got3 := make(chan tool.FileActionDecision, 1)
	m.fileConfirm.pending["f3"] = got3
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	select {
	case d := <-got3:
		if d.Allow {
			t.Fatalf("expected discard (Allow=false), got %+v", d)
		}
	default:
		t.Fatal("broker did not receive the discard decision")
	}
}

// TestTurnResultAppendsFileSummary verifies file changes surface as live
// per-edit DIFF entries during the turn (engine onChange → fileDiffMsg) and
// that the turn-end does NOT append a batched FILES summary. The DIFF entry is
// collapsed by default (compact (+N −M)) and reveals the +/- diff when
// filesExpanded is toggled with ctrl+f.
func TestTurnResultAppendsFileSummary(t *testing.T) {
	m := newTestApp()
	tool.ResetChanges()
	defer tool.ResetChanges()

	// Record a change the way write_file would, then drive the live stream the
	// engine emits for it (onChange → SetOnChange → fileDiffMsg).
	tool.RecordChange(tool.FileChange{Path: "src/a.ts", Action: "created", New: "x\ny\n"})
	diff := tool.CumulativeChangeDiff("src/a.ts")
	m.Update(fileDiffMsg{path: "src/a.ts", diff: diff})

	// A live DIFF entry must be present.
	var di int = -1
	for i, msg := range m.messages {
		if strings.HasPrefix(msg, "DIFF:\n") {
			di = i
		}
	}
	if di < 0 {
		t.Fatal("expected a live DIFF entry, got none")
	}

	// Settling the turn must NOT append a batched FILES summary.
	m.Update(turnResultMsg{content: "done", err: nil, mode: "BUILDER"})
	for _, msg := range m.messages {
		if strings.HasPrefix(msg, "FILES:\n") {
			t.Fatalf("turn-end must not append a FILES summary, got %q", msg)
		}
	}

	// Collapsed render hides the diff body but shows the path and (+N −M) stat.
	collapsed := ansiRegex.ReplaceAllString(formatMessage(m.messages[di], 120, false), "")
	if !strings.Contains(collapsed, "src/a.ts") {
		t.Fatalf("collapsed entry missing file path:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "(+") {
		t.Fatalf("collapsed entry missing (+N −M) stat:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "+ x") {
		t.Fatalf("collapsed entry must hide the diff body:\n%s", collapsed)
	}

	// Expanded render (ctrl+f) reveals the diff body.
	expanded := ansiRegex.ReplaceAllString(formatMessage(m.messages[di], 120, true), "")
	if !strings.Contains(expanded, "+ x") {
		t.Fatalf("expanded entry missing diff lines:\n%s", expanded)
	}
}

// TestActivityResetsOnNewTurn verifies activity clears when a fresh turn starts.
func TestActivityResetsOnNewTurn(t *testing.T) {
	m := newTestApp()
	m.status = "Thinking..." // mimics handleEnter before the turn starts
	if _, err := m.Update(stepProgressMsg{info: "grep (pattern: 'filter')"}); err != nil {
		t.Fatalf("step progress update failed: %v", err)
	}
	if len(m.activity) != 1 {
		t.Fatalf("expected 1 activity step, got %d", len(m.activity))
	}
	// A new user prompt resets the activity list (handleEnter path).
	m.activity = nil
	if len(m.activity) != 0 {
		t.Fatalf("activity not reset on new turn")
	}
}

// TestTurnResultModeBadge verifies each assistant answer is stamped with the
// engine mode the turn ran under, and the renderer draws the mode badge.
func TestTurnResultModeBadge(t *testing.T) {
	m := newTestApp()
	m.mode = "PLANNER"

	// Complete a turn while in PLANNER mode; the answer must be stamped.
	if _, err := m.Update(turnResultMsg{content: "analysis", err: nil, mode: m.mode}); err != nil {
		t.Fatalf("turn result update failed: %v", err)
	}
	if len(m.messages) == 0 {
		t.Fatal("no messages appended")
	}
	last := m.messages[len(m.messages)-1]
	if !strings.HasPrefix(last, "BROCODE:PLANNER:test-model\n") {
		t.Fatalf("expected mode+model-stamped message, got %q", last)
	}

	// Renderer must draw the mode chip next to the BROCODE label, with the
	// model shown dimmed after it. ANSI codes are stripped first — glamour
	// interleaves them mid-word.
	rendered := ansiRegex.ReplaceAllString(formatMessage(last, 120, false), "")
	if !strings.Contains(rendered, "PLANNER") {
		t.Fatalf("rendered answer missing PLANNER badge:\n%s", rendered)
	}
	if !strings.Contains(rendered, "test-model") {
		t.Fatalf("rendered answer missing model label:\n%s", rendered)
	}

	// Legacy unstamped format still renders (no badge, no crash).
	legacy := ansiRegex.ReplaceAllString(formatMessage("BROCODE:\nplain answer", 120, false), "")
	if !strings.Contains(legacy, "plain answer") {
		t.Fatalf("legacy format broken:\n%s", legacy)
	}

	// Mode-only (no model) legacy stamp still parses and renders the badge.
	modeOnly := ansiRegex.ReplaceAllString(formatMessage("BROCODE:PLANNER\nplain", 120, false), "")
	if !strings.Contains(modeOnly, "PLANNER") || !strings.Contains(modeOnly, "plain") {
		t.Fatalf("mode-only stamp broken:\n%s", modeOnly)
	}
}

// newTestApp builds a fully-initialized app model (real textarea/input
// widgets) without starting the Bubble Tea program, so View() can be rendered.
// TestConnectBuiltInAPIKeyTyping verifies keystrokes reach the API-key
// textinput at step 1 for a built-in provider (regression: routing used to
// send them to the unfocused custom-name input, so typing did nothing).
func TestConnectBuiltInAPIKeyTyping(t *testing.T) {
	m := newTestApp()

	// Open /connect; the first built-ins (opencode, deepseek) — pick a KEYED
	// provider (deepseek, index 1) so the wizard lands on the API-key step:
	// keyless providers (opencode/Ollama) skip it and save directly.
	_, _ = m.handleSlashCommand("/connect")
	if !m.showConnect || m.connectStep != 0 {
		t.Fatalf("expected connect wizard at step 0, got step=%d show=%v", m.connectStep, m.showConnect)
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	// ENTER → step 1 (built-in provider, API key).
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); err != nil {
		t.Fatalf("enter failed: %v", err)
	}
	if m.connectStep != 1 || m.connectCustom {
		t.Fatalf("expected built-in step 1 (API key), got step=%d custom=%v", m.connectStep, m.connectCustom)
	}

	// Type characters — they must land in the API-key input.
	_, _ = m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	_, _ = m.Update(tea.KeyPressMsg{Code: 'k', Text: "k"})
	if got := m.connectTextInput.Value(); got != "sk" {
		t.Errorf("typing at built-in API key step = %q, want %q", got, "sk")
	}
}

// TestConnectKeylessProviderSkipsKeyStep proves keyless built-in providers
// (opencode, Ollama) save immediately instead of asking for an API key that
// does not exist.
func TestConnectKeylessProviderSkipsKeyStep(t *testing.T) {
	m := newTestApp()

	_, _ = m.handleSlashCommand("/connect")
	if m.connectStep != 0 || !m.showConnect {
		t.Fatalf("expected wizard step 0, got step=%d show=%v", m.connectStep, m.showConnect)
	}

	// First entry is opencode (APIKeyEnvVar "") — ENTER must save directly.
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); err != nil {
		t.Fatalf("enter failed: %v", err)
	}
	if m.showConnect {
		t.Fatal("expected keyless provider to skip the API-key step and close the wizard")
	}
}

// TestSessionsDeleteWithConfirm verifies the sessions-modal delete flow: d
// arms a confirmation (never deletes instantly), n cancels it, y executes, and
// D arms a delete-all that also survives its own confirm.
func TestSessionsDeleteWithConfirm(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := store.NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	cwd, _ := os.Getwd()
	for _, id := range []string{"sess_a", "sess_b"} {
		if err := st.CreateSession(id, cwd); err != nil {
			t.Fatalf("failed to create %s: %v", id, err)
		}
	}

	ctx := bcontext.NewManager("test-sess", st, 128000)
	m := NewApp(provider.AppConfig{Providers: map[string]provider.CustomProviderConfig{}}, provider.DetectedProvider{}, "test-model", nil, tool.NewRegistry(), ctx, nil, nil, nil, 0, nil, nil, "⚡ test")

	_, _ = m.handleSlashCommand("/sessions")
	if !m.showSessions || len(m.sessionList) != 2 {
		t.Fatalf("expected sessions modal with 2 sessions, got show=%v list=%d", m.showSessions, len(m.sessionList))
	}

	// d arms a confirm — nothing may be deleted yet.
	_, _ = m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if m.sessionsConfirmID == "" {
		t.Fatal("expected confirm pending after pressing d")
	}
	if list, _ := st.ListSessions(); len(list) != 2 {
		t.Fatalf("d must not delete before confirm: %d sessions left", len(list))
	}

	// n cancels the confirm; the session survives.
	_, _ = m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.sessionsConfirmID != "" {
		t.Fatal("expected confirm cancelled after n")
	}
	if list, _ := st.ListSessions(); len(list) != 2 {
		t.Fatalf("n must not delete: %d sessions left", len(list))
	}

	// d again + y executes the single delete.
	_, _ = m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	_, _ = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if list, _ := st.ListSessions(); len(list) != 1 {
		t.Fatalf("expected 1 session after confirmed delete, got %d", len(list))
	}
	if len(m.messages) == 0 || !strings.Contains(m.messages[len(m.messages)-1], "Deleted session") {
		t.Errorf("expected delete confirmation message in history")
	}

	// D arms delete-all; y wipes everything except the active session row,
	// which is recreated so the events FK stays valid.
	_, _ = m.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	if m.sessionsConfirmID != "ALL" {
		t.Fatalf("expected delete-all confirm, got %q", m.sessionsConfirmID)
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	list, _ := st.ListSessions()
	if len(list) != 1 || list[0].ID != "test-sess" {
		t.Fatalf("expected only recreated active session, got %+v", list)
	}
}

// TestMouseModeDefaultsToScroll verifies the wheel-scroll mouse mode is the
// default (so the mouse wheel works out of the box and long answers never feel
// "stuck").
func TestMouseModeDefaultsToScroll(t *testing.T) {
	m := newTestApp()
	if m.mouseMode != "SCROLL" {
		t.Fatalf("expected SCROLL mouse mode by default, got %q", m.mouseMode)
	}
}

// TestViewportParksAtTopOfLongAnswer verifies a long assistant answer parks
// the viewport at the START of the answer (not the bottom), so its beginning
// is readable immediately and pages down — the complaint that long answers
// looked "cut off" until Ctrl+P.
func TestViewportParksAtTopOfLongAnswer(t *testing.T) {
	m := newTestApp()
	m.width = 100
	m.height = 30
	m.messages = append(m.messages, "YOU:\nok")
	m.messages = append(m.messages, "BROCODE:\n💭 analysis\n\n# Analisis Filter\n\n"+longAnswer(80))
	if _, err := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}

	v := m.View()
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !contains(visible, "💭 analysis") {
		t.Fatalf("start of long answer not parked at top; viewport YOffset=%d", m.logViewport.YOffset())
	}
	if contains(visible, "last line of the long answer") {
		t.Fatal("long answer parked at the bottom instead of the top (end visible, start hidden)")
	}
	if m.logViewport.AtBottom() {
		t.Fatal("viewport must not be at the bottom when parking a long answer at its start")
	}
}

// TestViewportParksAtBottomForShortAnswer verifies short answers still land at
// the bottom (the whole answer visible, nothing to scroll).
func TestViewportParksAtBottomForShortAnswer(t *testing.T) {
	m := newTestApp()
	m.width = 100
	m.height = 30
	m.messages = append(m.messages, "YOU:\nok")
	m.messages = append(m.messages, "BROCODE:\nshort answer")
	if _, err := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}

	v := m.View()
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !contains(visible, "short answer") {
		t.Fatalf("short answer not visible; viewport YOffset=%d", m.logViewport.YOffset())
	}
	if !m.logViewport.AtBottom() {
		t.Fatal("short answer must land at the bottom of the viewport")
	}
}

// TestViewportParksAtTopAnswerBehindFilesSummary verifies that when a turn
// ends with a FILES change summary, the viewport still parks at the START of
// the answer behind it (not the summary, not the bottom).
func TestViewportParksAtTopAnswerBehindFilesSummary(t *testing.T) {
	m := newTestApp()
	m.width = 100
	m.height = 30
	m.messages = append(m.messages, "YOU:\nok")
	m.messages = append(m.messages, "BROCODE:\n💭 analysis\n\n"+longAnswer(80))
	m.messages = append(m.messages, "FILES:\n  modified: src/a.ts"+tool.FileChangesSep+"+1 -1")
	if _, err := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}

	v := m.View()
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !contains(visible, "💭 analysis") {
		t.Fatalf("answer start not parked at top behind FILES summary; YOffset=%d", m.logViewport.YOffset())
	}
	if m.logViewport.AtBottom() {
		t.Fatal("viewport must not be at the bottom when parking at the answer behind a FILES summary")
	}
}

// TestPagerModeCtrlPEnterExit verifies ctrl+p opens the in-TUI pager over the
// last assistant answer and q/Esc/Ctrl+P exit it, restoring the normal chat.
func TestPagerModeCtrlPEnterExit(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36
	m.messages = append(m.messages, "BROCODE:\n"+longAnswer(5))
	if _, err := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}

	// ctrl+p enters the pager.
	if _, err := m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}); err != nil {
		t.Fatalf("ctrl+p update failed: %v", err)
	}
	if !m.pagerActive {
		t.Fatal("expected pager active after ctrl+p")
	}
	if !contains(m.pagerContent, "LATEST ANSWER") {
		t.Fatal("pager header missing from pager content")
	}

	// The pager renders the answer through the real View() path.
	v := m.View()
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !contains(visible, "line of the long answer") {
		t.Fatal("answer not rendered inside the pager")
	}

	// q exits back to the normal chat view.
	if _, err := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); err != nil {
		t.Fatalf("q update failed: %v", err)
	}
	if m.pagerActive {
		t.Fatal("expected pager closed after q")
	}
	if _, err := m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}); err != nil {
		t.Fatalf("re-enter ctrl+p failed: %v", err)
	}
	if !m.pagerActive {
		t.Fatal("expected pager re-entered after second ctrl+p")
	}
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); err != nil {
		t.Fatalf("esc update failed: %v", err)
	}
	if m.pagerActive {
		t.Fatal("expected pager closed after esc")
	}
}

// TestPagerScrollKeysDoNotCrash sanity-checks pager navigation keys page the
// viewport instead of leaking into input history handling.
func TestPagerScrollKeysDoNotCrash(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36
	m.messages = append(m.messages, "BROCODE:\n"+longAnswer(200))
	if _, err := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}
	if _, err := m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}); err != nil {
		t.Fatalf("ctrl+p failed: %v", err)
	}
	for _, k := range []tea.KeyPressMsg{
		{Code: tea.KeyPgDown}, {Code: tea.KeyPgUp},
		{Code: tea.KeyHome}, {Code: tea.KeyEnd},
		{Code: 'u', Mod: tea.ModCtrl}, {Code: 'd', Mod: tea.ModCtrl},
	} {
		if _, err := m.Update(k); err != nil {
			t.Fatalf("pager key %v failed: %v", k, err)
		}
	}
	if !m.pagerActive {
		t.Fatal("pager unexpectedly closed during navigation")
	}
}

// TestQueueDoesNotPolluteHistory verifies a prompt sent while a turn runs is
// queued WITHOUT appending a notification row to the conversation history —
// the queue lives in the activity slot above the input instead.
func TestQueueDoesNotPolluteHistory(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36
	before := len(m.messages)
	m.turnRunning = true
	m.promptInput.SetValue("prompt kedua saat turn jalan")
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); err != nil {
		t.Fatalf("enter failed: %v", err)
	}
	if len(m.pendingQueue) != 1 {
		t.Fatalf("expected 1 queued prompt, got %d", len(m.pendingQueue))
	}
	if m.pendingQueue[0].Text != "prompt kedua saat turn jalan" {
		t.Fatalf("queued prompt mismatch: %q", m.pendingQueue[0].Text)
	}
	if len(m.messages) != before {
		t.Fatalf("queue must not add history rows: before=%d after=%d", before, len(m.messages))
	}
	for _, msg := range m.messages {
		if contains(msg, "queued") || contains(msg, "Previous turn") {
			t.Fatalf("queue notification leaked into history: %q", msg)
		}
	}
}

// TestQueueBlockRendersAboveInput verifies queued prompts render live in the
// chrome above the input (not in the chat log) with a count, mode badge, and one row each.
func TestQueueBlockRendersAboveInput(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36
	m.turnRunning = true
	m.pendingQueue = []QueuedPrompt{
		{Text: "prompt satu", Mode: "BUILDER"},
		{Text: "prompt dua panjang dengan banyak kata sehingga harus dirapikan ke satu baris preview", Mode: "PLANNER"},
	}
	m.status = "Queued..."
	if _, err := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}
	v := m.View()
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !contains(visible, "PROMPT QUEUE (2)") {
		t.Fatal("queue block header missing from View()")
	}
	if !contains(visible, "prompt satu") {
		t.Fatal("first queued row missing from View()")
	}
	if !contains(visible, "BUILD") || !contains(visible, "PLAN") {
		t.Fatal("mode badges missing from queued prompt rows in View()")
	}
	// Long prompt flattened to one line (no newline in the row).
	if contains(visible, "banyak kata sehingga harus\n") {
		t.Fatal("queued prompt preview must be a single line")
	}
}

// TestQueueManageCtrlKAndAltK verifies Ctrl+K and Alt+K enter queue management,
// e/d/m edit, delete, or change mode, K/J reorder, and Esc exits.
func TestQueueManageCtrlKAndAltK(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36
	m.turnRunning = true
	m.pendingQueue = []QueuedPrompt{
		{Text: "satu", Mode: "BUILDER"},
		{Text: "dua", Mode: "PLANNER"},
		{Text: "tiga", Mode: "MINER"},
	}

	ctrlK := func() {
		if _, err := m.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}); err != nil {
			t.Fatalf("ctrl+k failed: %v", err)
		}
	}
	ctrlK()
	if !m.queueMode {
		t.Fatal("ctrl+k did not enter queue mode")
	}
	if m.queueSel != 0 {
		t.Fatalf("expected selection at 0, got %d", m.queueSel)
	}

	// m cycles mode of the selected prompt (BUILDER -> PLANNER -> MINER)
	if _, err := m.Update(tea.KeyPressMsg{Code: 'm', Text: "m"}); err != nil {
		t.Fatalf("m key failed: %v", err)
	}
	if m.pendingQueue[0].Mode != "PLANNER" {
		t.Fatalf("expected mode cycled to PLANNER, got %q", m.pendingQueue[0].Mode)
	}

	// J moves selected prompt DOWN (swapping satu and dua)
	if _, err := m.Update(tea.KeyPressMsg{Code: 'J', Text: "J"}); err != nil {
		t.Fatalf("J key failed: %v", err)
	}
	if m.pendingQueue[0].Text != "dua" || m.pendingQueue[1].Text != "satu" {
		t.Fatalf("J failed to swap queue items: %v", m.pendingQueue)
	}
	if m.queueSel != 1 {
		t.Fatalf("expected selection at 1 after moving down, got %d", m.queueSel)
	}

	// K moves selected prompt UP (swapping back)
	if _, err := m.Update(tea.KeyPressMsg{Code: 'K', Text: "K"}); err != nil {
		t.Fatalf("K key failed: %v", err)
	}
	if m.pendingQueue[0].Text != "satu" || m.pendingQueue[1].Text != "dua" {
		t.Fatalf("K failed to swap queue items: %v", m.pendingQueue)
	}

	// ↓ moves selection to 1 ("dua")
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyDown}); err != nil {
		t.Fatalf("down failed: %v", err)
	}
	if m.queueSel != 1 {
		t.Fatalf("expected selection at 1 after down, got %d", m.queueSel)
	}

	// d deletes the selected prompt ("dua").
	if _, err := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"}); err != nil {
		t.Fatalf("d failed: %v", err)
	}
	if len(m.pendingQueue) != 2 || m.pendingQueue[0].Text != "satu" || m.pendingQueue[1].Text != "tiga" {
		t.Fatalf("d must delete the selected item, got %v", m.pendingQueue)
	}
	if !m.queueMode {
		t.Fatal("queue mode must stay active while items remain")
	}

	// e loads the selected prompt into the input and removes it from the queue.
	m.queueSel = 0
	if _, err := m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"}); err != nil {
		t.Fatalf("e failed: %v", err)
	}
	if len(m.pendingQueue) != 1 || m.pendingQueue[0].Text != "tiga" {
		t.Fatalf("e must remove the edited item, got %v", m.pendingQueue)
	}
	if m.promptInput.Value() != "satu" {
		t.Fatalf("e must load the prompt into the input, got %q", m.promptInput.Value())
	}
	if m.queueMode {
		t.Fatal("e must exit queue mode so the prompt can be edited")
	}

	// Ctrl+K again, then Esc exits.
	ctrlK()
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); err != nil {
		t.Fatalf("esc failed: %v", err)
	}
	if m.queueMode {
		t.Fatal("esc did not exit queue mode")
	}

	// Deleting the last item auto-exits queue mode.
	ctrlK()
	if _, err := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"}); err != nil {
		t.Fatalf("d failed: %v", err)
	}
	if len(m.pendingQueue) != 0 {
		t.Fatalf("expected empty queue, got %v", m.pendingQueue)
	}
	if m.queueMode {
		t.Fatal("queue mode must auto-exit when the queue empties")
	}
}

// TestQueueDrainStartsNextTurn verifies a completed turn drains the queue into
// the next startTurn and clamps the queue selection.
func TestQueueDrainStartsNextTurn(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36
	m.pendingQueue = []QueuedPrompt{
		{Text: "q1", Mode: "BUILDER"},
		{Text: "q2", Mode: "PLANNER"},
	}
	m.queueSel = 1
	before := len(m.messages)

	// The returned Cmd is the next turn's runner (not an error) — startTurn
	// fires the drained prompt, so Update returns a real tea.Cmd here.
	m.Update(turnResultMsg{content: "answer"})
	if len(m.pendingQueue) != 1 || m.pendingQueue[0].Text != "q2" {
		t.Fatalf("queue must drain the first item, got %v", m.pendingQueue)
	}
	if m.queueSel != 0 {
		t.Fatalf("queue selection must clamp after drain, got %d", m.queueSel)
	}
	if len(m.messages) != before+2 { // 1 BROCODE answer + 1 YOU (drained prompt)
		t.Fatalf("expected answer + drained prompt appended, got %d -> %d messages", before, len(m.messages))
	}
	if !contains(m.messages[len(m.messages)-2], "answer") {
		t.Fatalf("drained turn answer missing from history")
	}
	if !contains(m.messages[len(m.messages)-1], "q1") {
		t.Fatalf("drained prompt must enter history as a YOU row")
	}
}

func newTestApp() Model {
	cfg := provider.AppConfig{Providers: map[string]provider.CustomProviderConfig{}}
	p := provider.DetectedProvider{}
	ctx := bcontext.NewManager("test-sess", nil, 128000)
	return NewApp(cfg, p, "test-model", nil, tool.NewRegistry(), ctx, nil, nil, nil, 0, nil, nil, "⚡ test")
}

func longAnswer(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += "line of the long answer with some detail and context\n"
	}
	return s
}

// TestMCPModalFlow verifies the /mcp interactive modal: it opens (not just a
// text note), lists servers with status, the a-wizard adds a server to
// .mcp.json (merging), and d+y removes it with an explicit confirm.
func TestMCPModalFlow(t *testing.T) {
	t.Setenv("BROCODE_NO_OPENCODE", "1")
	tmp := t.TempDir() // .mcp.json lands in a temp dir, not the repo
	if err := os.WriteFile(filepath.Join(tmp, ".mcp.json"), []byte(`{"mcpServers": {"github": {"command": "npx", "args": ["-y", "pkg"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestApp()

	// Empty-state: a manager with no servers shows the add hint (built before
	// the chdir below so NewApp still runs inside the git repo).
	m2 := newTestApp()
	m2.mcpMgr = mcp.NewManager()
	m2.showMCP = true
	if v := m2.renderMCPModal(); !strings.Contains(v, "No MCP servers configured") {
		t.Fatalf("empty state missing:\n%s", v)
	}

	t.Chdir(tmp)
	m.mcpMgr = mcp.NewManager()
	// Never spawn real subprocesses in tests: every server fails fast at the
	// client factory, so reloads record errors instead of hanging on stdio.
	m.mcpMgr.SetClientFactory(func(cfg mcp.ServerConfig) (mcp.Client, error) {
		return nil, errors.New("no spawn in tests")
	})
	m.mcpMgr.LoadDefaults()

	// /mcp opens the modal.
	_, _ = m.handleSlashCommand("/mcp")
	if !m.showMCP {
		t.Fatal("expected /mcp to open the modal")
	}
	view := m.renderMCPModal()
	if !strings.Contains(view, "github") {
		t.Fatalf("modal should list the server:\n%s", view)
	}


	// 'a' starts the add wizard at the transport step.
	_, _ = m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if !m.mcpAddActive || m.mcpAddStep != 0 {
		t.Fatal("expected add wizard at transport step")
	}
	// ENTER keeps stdio → name step; type a name.
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.mcpAddStep != 1 {
		t.Fatal("expected name step")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	_, _ = m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	if got := m.mcpAddName.Value(); got != "fs" {
		t.Fatalf("name input = %q, want fs", got)
	}
	// ENTER → command step; type a command.
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.mcpAddStep != 2 {
		t.Fatal("expected command step")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	_, _ = m.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	_, _ = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if got := m.mcpAddCmd.Value(); got != "npx" {
		t.Fatalf("command input = %q, want npx", got)
	}
	// ENTER saves: .mcp.json written, wizard closed.
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.mcpAddActive {
		t.Fatal("wizard must close after save")
	}
	data, err := os.ReadFile(mcp.ProjectMCPPath())
	if err != nil {
		t.Fatalf(".mcp.json not written: %v", err)
	}
	if !strings.Contains(string(data), "\"fs\"") || !strings.Contains(string(data), "\"github\"") {
		t.Fatalf("new server must merge into .mcp.json, preserving github:\n%s", data)
	}

	// After the reload the manager reflects both file servers.
	names := m.mcpNames()
	if len(names) != 2 || names[0] != "fs" || names[1] != "github" {
		t.Fatalf("manager after save = %v, want [fs github]", names)
	}

	// 'd' arms the delete confirm; 'n' cancels; 'd' + 'y' removes from file.
	_, _ = m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if m.mcpConfirm != "fs" {
		t.Fatalf("expected delete confirm for fs, got %q", m.mcpConfirm)
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.mcpConfirm != "" {
		t.Fatal("n must cancel the delete confirm")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	_, _ = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	data, err = os.ReadFile(mcp.ProjectMCPPath())
	if err != nil {
		t.Fatalf(".mcp.json missing after delete: %v", err)
	}
	if strings.Contains(string(data), "\"fs\"") || !strings.Contains(string(data), "\"github\"") {
		t.Fatalf("delete must remove only the target server:\n%s", data)
	}
}

// TestLiveDiffUpsertPerPath verifies live per-edit DIFF entries grow one entry
// per file: a second diff for the same path replaces the first (the engine
// sends cumulative diffs), while a different path appends a fresh entry — so
// repeated edits don't flood the conversation history.
func TestLiveDiffUpsertPerPath(t *testing.T) {
	m := newTestApp()
	countDiffs := func() int {
		n := 0
		for _, msg := range m.messages {
			if strings.HasPrefix(msg, "DIFF:\n") {
				n++
			}
		}
		return n
	}

	// Two different files -> two entries.
	for _, d := range []fileDiffMsg{
		{path: "src/a.ts", diff: "+1 -1"},
		{path: "src/b.ts", diff: "+2 -2"},
	} {
		if _, err := m.Update(d); err != nil {
			t.Fatalf("diff update failed: %v", err)
		}
	}
	if n := countDiffs(); n != 2 {
		t.Fatalf("expected 2 DIFF entries after two new files, got %d: %v", n, m.messages)
	}

	// Same file again -> replaces, does not append.
	if _, err := m.Update(fileDiffMsg{path: "src/a.ts", diff: "+3 -3"}); err != nil {
		t.Fatalf("upsert diff update failed: %v", err)
	}
	if n := countDiffs(); n != 2 {
		t.Fatalf("repeated edit to the same file must not add a message, got %d", n)
	}
	entry := func(path string) string {
		for _, msg := range m.messages {
			if strings.HasPrefix(msg, "DIFF:\n"+path+"\n") {
				return msg
			}
		}
		return ""
	}
	if e := entry("src/a.ts"); !strings.Contains(e, "+3 -3") {
		t.Fatalf("a.ts entry not replaced with the cumulative diff: %q", e)
	}
	if e := entry("src/b.ts"); !strings.Contains(e, "+2 -2") {
		t.Fatalf("b.ts entry must be untouched: %q", e)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// TestNewAppSeedsPromptHistory verifies that prompts passed in from a resumed
// session are loaded into the up/down prompt-history (so ArrowUp recalls them),
// not just the live chat log. This is what makes `brocode -c` recall previous
// prompts even before anything is typed this run.
func TestNewAppSeedsPromptHistory(t *testing.T) {
	ctx := bcontext.NewManager("test-sess", nil, 128000)
	seeded := []string{"first prompt", "second prompt"}
	tmp := NewApp(provider.AppConfig{}, provider.DetectedProvider{}, "test-model", nil, tool.NewRegistry(), ctx, nil, nil, nil, 0, seeded, nil, "⚡ test")
	m := &tmp
	if len(m.promptHistory) != 2 {
		t.Fatalf("promptHistory should be seeded with 2 prompts, got %d", len(m.promptHistory))
	}
	if m.historyIdx != 2 {
		t.Fatalf("historyIdx should point past the end (2), got %d", m.historyIdx)
	}
	// ArrowUp should recall the most recent seeded prompt.
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyUp}); err != nil {
		t.Fatalf("ArrowUp update failed: %v", err)
	}
	if m.promptInput.Value() != "second prompt" {
		t.Fatalf("ArrowUp should recall last prompt, got %q", m.promptInput.Value())
	}
}

// TestMultiRoundStreamingWithLiveDiffEmission verifies the complete multi-round
// live turn flow: streaming thoughts -> tool execution -> live DIFF badge in chat
// -> next thought round -> Ctrl+F unified diff expansion.
func TestMultiRoundStreamingWithLiveDiffEmission(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 40
	m.mode = "BUILDER"
	m.activeModel = "poolside/laguna-s-2.1"

	// 1. User prompt
	m.appendMessages("YOU:\nAdd Power method to Calculator")
	m.turnRunning = true
	m.status = "Thinking..."

	// 2. Round 1: Model streams first response
	m.streaming = true
	m.pendingStream = "Saya akan menambahkan method Power pada Calculator."

	// Transition to tool execution: stream is committed to history
	_, _ = m.Update(stepProgressMsg{state: 1, info: "📝 edit_file calculator.go"}) // StateActing = 1
	if m.pendingStream != "" {
		t.Fatalf("pendingStream should be cleared after StateActing transition")
	}

	// 3. Engine emits live DIFF through onUpdate channel
	diff := "@@ -1,3 +1,8 @@\n package playground\n \n+// Power returns base^exponent\n+func (c *Calculator) Power(base, exp float64) float64 {\n+    return math.Pow(base, exp)\n+}\n"
	_, _ = m.Update(stepProgressMsg{state: 1, info: "DIFF:\nplayground/calculator.go\n" + diff})

	// Verify DIFF message is present in history
	hasDiff := false
	for _, msg := range m.messages {
		if strings.HasPrefix(msg, "DIFF:\nplayground/calculator.go") {
			hasDiff = true
			break
		}
	}
	if !hasDiff {
		t.Fatalf("expected DIFF:playground/calculator.go in m.messages, got: %v", m.messages)
	}

	// 4. Render log in compact mode (default)
	logCompact := m.buildLog(110)
	if !strings.Contains(logCompact, "DIFF") || !strings.Contains(logCompact, "calculator.go") {
		t.Fatalf("rendered log should contain DIFF badge: %s", logCompact)
	}

	// 5. User presses Ctrl+F to expand diff
	_, _ = m.Update(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	if !m.filesExpanded {
		t.Fatalf("filesExpanded should be true after Ctrl+F")
	}
	logExpanded := m.buildLog(110)
	if !strings.Contains(logExpanded, "+func (c *Calculator) Power") {
		t.Fatalf("expanded log should show full diff line: %s", logExpanded)
	}

	// 6. Round 2: Model creates a new test file
	m.streaming = true
	m.pendingStream = "Sekarang saya akan membuat file unit test baru."
	_, _ = m.Update(stepProgressMsg{state: 1, info: "✍️ write_file calculator_test.go"})
	newFileDiff := "+package playground\n+\n+import \"testing\"\n+\n+func TestPower(t *testing.T) {}\n"
	_, _ = m.Update(stepProgressMsg{state: 1, info: "DIFF:\nplayground/calculator_test.go\n" + newFileDiff})

	// 7. Verify CREATE badge rendered for newly created file
	m.filesExpanded = false
	logWithCreate := m.buildLog(110)
	if !strings.Contains(logWithCreate, "CREATE") || !strings.Contains(logWithCreate, "calculator_test.go") {
		t.Fatalf("log should render CREATE badge for new file: %s", logWithCreate)
	}

	// 8. Turn finishes cleanly
	_, _ = m.Update(turnResultMsg{content: "Implementasi selesai dan unit test berhasil dibuat.", mode: "BUILDER"})
	if m.status != "Ready" {
		t.Fatalf("expected status Ready after turn result, got %s", m.status)
	}
}

// TestQueuePerPromptModeSwitching verifies that user can switch modes mid-turn
// via Shift+Tab and queue subsequent prompts with their dedicated target mode.
func TestQueuePerPromptModeSwitching(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36

	// Turn 1 starts under BUILDER
	m.mode = "BUILDER"
	m.promptInput.SetValue("turn 1 in builder")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.turnRunning {
		t.Fatal("turn 1 should be running")
	}

	// While Turn 1 is running, user hits Shift+Tab to switch mode to PLANNER
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.mode != "PLANNER" {
		t.Fatalf("expected mode switched to PLANNER while turn running, got %s", m.mode)
	}

	// User types prompt 2 and hits Enter -> queued with PLANNER mode
	m.promptInput.SetValue("turn 2 in planner")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.pendingQueue) != 1 {
		t.Fatalf("expected 1 item queued, got %d", len(m.pendingQueue))
	}
	if m.pendingQueue[0].Text != "turn 2 in planner" || m.pendingQueue[0].Mode != "PLANNER" {
		t.Fatalf("expected prompt 2 queued with PLANNER mode, got %+v", m.pendingQueue[0])
	}

	// User hits Shift+Tab again -> switches to MINER
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.mode != "MINER" {
		t.Fatalf("expected mode switched to MINER, got %s", m.mode)
	}

	// User types prompt 3 and hits Enter -> queued with MINER mode
	m.promptInput.SetValue("turn 3 in miner")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.pendingQueue) != 2 {
		t.Fatalf("expected 2 items queued, got %d", len(m.pendingQueue))
	}
	if m.pendingQueue[1].Text != "turn 3 in miner" || m.pendingQueue[1].Mode != "MINER" {
		t.Fatalf("expected prompt 3 queued with MINER mode, got %+v", m.pendingQueue[1])
	}

	// Turn 1 finishes -> Turn 2 auto starts in PLANNER mode
	m.Update(turnResultMsg{content: "done turn 1", mode: "BUILDER"})
	if m.mode != "PLANNER" {
		t.Fatalf("expected active mode updated to PLANNER on turn 2 drain, got %s", m.mode)
	}
	if len(m.pendingQueue) != 1 {
		t.Fatalf("expected 1 item left in queue, got %d", len(m.pendingQueue))
	}

	// Turn 2 finishes -> Turn 3 auto starts in MINER mode
	m.Update(turnResultMsg{content: "done turn 2", mode: "PLANNER"})
	if m.mode != "MINER" {
		t.Fatalf("expected active mode updated to MINER on turn 3 drain, got %s", m.mode)
	}
	if len(m.pendingQueue) != 0 {
		t.Fatalf("expected queue completely drained, got %d items", len(m.pendingQueue))
	}
}

// TestTurnModeIsolationDuringMidTurnShiftTab verifies that switching mode mid-turn
// does NOT alter the mode badge of intermediate reasoning steps or the final response
// of the currently executing turn.
func TestTurnModeIsolationDuringMidTurnShiftTab(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36

	// Turn starts under BUILDER
	m.mode = "BUILDER"
	m.promptInput.SetValue("perbaiki bug kontras warna")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.turnRunning {
		t.Fatal("turn should be running")
	}
	if m.turnMode != "BUILDER" {
		t.Fatalf("expected turnMode to be BUILDER, got %s", m.turnMode)
	}

	// Iteration 1 streams some reasoning and executes a tool
	m.streaming = true
	m.pendingStream = "Saya akan memeriksa file MessageBubble.tsx"
	m.Update(stepProgressMsg{state: loop.StateActing, info: "📖 read_file MessageBubble.tsx"})

	// Verify iteration 1 assistant block was stamped with BUILDER
	foundBuilder := false
	for _, msg := range m.messages {
		if strings.HasPrefix(msg, "BROCODE:BUILDER:") {
			foundBuilder = true
			break
		}
	}
	if !foundBuilder {
		t.Fatalf("expected iteration 1 to be stamped with BROCODE:BUILDER, messages: %v", m.messages)
	}

	// Mid-turn: User hits Shift+Tab twice to switch mode to MINER for their NEXT prompt
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.mode != "MINER" {
		t.Fatalf("expected pending UI mode to be MINER, got %s", m.mode)
	}
	// But in-flight turnMode MUST STILL BE BUILDER!
	if m.turnMode != "BUILDER" {
		t.Fatalf("in-flight turnMode must remain BUILDER, got %s", m.turnMode)
	}

	// Iteration 2 streams more reasoning and executes another tool
	m.streaming = true
	m.pendingStream = "Sekarang saya akan mengedit warnanya"
	m.Update(stepProgressMsg{state: loop.StateActing, info: "✍️ edit_file MessageBubble.tsx"})

	// Check messages: Iteration 2 must STILL be stamped with BUILDER, NEVER MINER!
	for _, msg := range m.messages {
		if strings.HasPrefix(msg, "BROCODE:MINER:") {
			t.Fatalf("CRITICAL BUG: mid-turn Shift+Tab leaked into in-flight turn step: %v", msg)
		}
	}

	// Turn completes
	m.Update(turnResultMsg{content: "Semua warna berhasil diperbaiki.", mode: m.turnMode})
	for _, msg := range m.messages {
		if strings.HasPrefix(msg, "BROCODE:MINER:") {
			t.Fatalf("CRITICAL BUG: final turnResultMsg got stamped with MINER: %v", msg)
		}
	}
}

func TestEphemeralAskSlashCommand(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 40

	// 1. Empty /ask prints usage
	m.handleSlashCommand("/ask")
	lastMsg := m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "ask") {
		t.Fatalf("expected usage message for empty /ask, got: %s", lastMsg)
	}

	// 2. Receiving ephemeralAskResultMsg displays note and resets status to Ready
	askResult := "ASK:\nWhere is webhook?\n---\nIt is in `services/webhook.js`"
	m.Update(ephemeralAskResultMsg(askResult))

	if m.status != "Ready" {
		t.Errorf("expected status 'Ready', got %q", m.status)
	}
	found := false
	for _, msg := range m.messages {
		if strings.Contains(msg, "Where is webhook?") && strings.Contains(msg, "services/webhook.js") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ephemeral ask answer in UI message log, got messages: %v", m.messages)
	}
}

func TestClearInputShortcuts(t *testing.T) {
	m := newTestApp()

	// 1. Ctrl+U clears the input bar
	m.promptInput.SetValue("some long prompt typed by user")
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if m.promptInput.Value() != "" {
		t.Fatalf("Ctrl+U should clear promptInput, got %q", m.promptInput.Value())
	}

	// 2. Esc clears the input bar when not running a turn
	m.promptInput.SetValue("another prompt draft")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.promptInput.Value() != "" {
		t.Fatalf("Esc should clear promptInput, got %q", m.promptInput.Value())
	}

	// 3. Alt+Backspace / Ctrl+Backspace clears the input bar
	m.promptInput.SetValue("windows / mac shortcut test")
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModAlt})
	if m.promptInput.Value() != "" {
		t.Fatalf("Alt+Backspace should clear promptInput, got %q", m.promptInput.Value())
	}
}

func TestNormalizeEmojiSpacing(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"⚠️ Same-command loop detected", "⚠️ Same-command loop detected"},
		{"⚠️  Final warning", "⚠️ Final warning"},
		{"🧠 Turn 10/25 reasoning...", "🧠 Turn 10/25 reasoning..."},
		{"🧠  Turn 23/25", "🧠 Turn 23/25"},
		{"🧠 Thinking & analyzing request...", "🧠 Thinking & analyzing request..."},
		{"⏳ poolside/laguna-s-2", "⏳ poolside/laguna-s-2"},
		{"📖  read_file src/app.ts", "📖 read_file src/app.ts"},
		{"📖 read_file src/app.ts", "📖 read_file src/app.ts"},
		{"🔧  edit_file src/main.go", "🔧 edit_file src/main.go"},
		{"⚙️  bash npm test", "⚙️ bash npm test"},
		{"Plain text with no emoji", "Plain text with no emoji"},
		{"", ""},
	}
	for _, tc := range tests {
		got := normalizeEmojiSpacing(tc.input)
		if got != tc.want {
			t.Errorf("normalizeEmojiSpacing(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestTodosLiveUpdateInTUI(t *testing.T) {
	m := newTestApp()
	m.width = 100
	m.height = 30

	// 1. Initial TODOs step progress
	todo1 := "TODOS:\n✓  Step 1 Done\n⏳ Step 2 Working\n○  Step 3 Waiting"
	m.Update(stepProgressMsg{state: loop.StateActing, info: todo1})

	if len(m.messages) == 0 {
		t.Fatal("expected message added")
	}
	if !strings.Contains(m.messages[len(m.messages)-1], "TODOS:\n") {
		t.Fatalf("expected last message to be TODOS, got: %s", m.messages[len(m.messages)-1])
	}

	// 2. Updated TODOs in the same turn should update in-place (no duplicate card)
	todo2 := "TODOS:\n✓  Step 1 Done\n✓  Step 2 Done\n⏳ Step 3 Working"
	msgCountBefore := len(m.messages)
	m.Update(stepProgressMsg{state: loop.StateActing, info: todo2})

	if len(m.messages) != msgCountBefore {
		t.Fatalf("expected in-place update without extra bubble, got count: %d, want: %d", len(m.messages), msgCountBefore)
	}

	rendered := m.View().Content
	if !strings.Contains(rendered, "TODOs") {
		t.Fatalf("expected rendered TUI to show TODOs card, got: %s", rendered)
	}
}
