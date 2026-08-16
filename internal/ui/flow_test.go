package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// Simulates the real event sequence: user sends prompt, progress messages
// stream in, then the turn result arrives, and finally a late "Completed"
// progress message lands (as happens with the opencode CLI adapter whose
// stderr goroutine may outlive the turn). The user's prompt must stay visible
// and the status must settle to "Ready".
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
	m.cancelTurn = func() {}
	// KeyPressMsg is an alias for Key; KeyEscape renders as "esc".
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}); err != nil {
		t.Fatalf("esc key update failed: %v", err)
	}
	if !m.interrupted {
		t.Fatal("expected interrupted flag after ESC")
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
	m.pendingStream = "Mulai menjawab... dan terpotong di tengah kalimat"
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
		if strings.Contains(msg, "Mulai menjawab... dan terpotong") {
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
		if strings.Contains(msg, "empty response") {
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
	if len(m.pendingQueue) != 1 || m.pendingQueue[0] != "second" {
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

// TestTurnResultAppendsFileSummary verifies the FILES change summary is
// appended after a turn that touched files, with the compact block rendered by
// default and the diff revealed when filesExpanded is toggled.
func TestTurnResultAppendsFileSummary(t *testing.T) {
	m := newTestApp()
	tool.ResetChanges()
	defer tool.ResetChanges()

	// Record a change the way write_file would.
	tool.RecordChange(tool.FileChange{Path: "src/a.ts", Action: "created", New: "x\ny\n"})

	m.Update(turnResultMsg{content: "done", err: nil, mode: "BUILDER"})
	// The diff is shown by default (no ctrl+f needed); ctrl+f collapses it.
	if !m.filesExpanded {
		t.Fatal("summary must start expanded (diff visible without ctrl+f)")
	}
	if len(m.messages) == 0 {
		t.Fatal("no messages appended")
	}
	last := m.messages[len(m.messages)-1]
	if !strings.HasPrefix(last, "FILES:\n") {
		t.Fatalf("expected FILES summary message, got %q", last)
	}

	// Default (expanded) render shows the +/- diff lines.
	expanded := ansiRegex.ReplaceAllString(formatMessage(last, 120, true), "")
	if !strings.Contains(expanded, "src/a.ts") {
		t.Fatalf("summary missing file row:\n%s", expanded)
	}
	if !strings.Contains(expanded, "+ x") {
		t.Fatalf("expanded summary missing diff lines:\n%s", expanded)
	}

	// Collapsed render (ctrl+f) hides the diff body but keeps the row.
	collapsed := ansiRegex.ReplaceAllString(formatMessage(last, 120, false), "")
	if strings.Contains(collapsed, "+ x") {
		t.Fatalf("collapsed summary must hide the diff body:\n%s", collapsed)
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
	// keyless providers (opencode/FreeBuff/Ollama) skip it and save directly.
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
// (opencode, FreeBuff, Ollama) save immediately instead of asking for an API
// key that does not exist — the confusion the user hit with FreeBuff.
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
	m := NewApp(provider.AppConfig{Providers: map[string]provider.CustomProviderConfig{}}, provider.DetectedProvider{}, "test-model", nil, tool.NewRegistry(), ctx, nil, nil, nil, 0, nil, "⚡ test")

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
	m.messages = append(m.messages, "BROCODE:\njawaban singkat")
	if _, err := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}

	v := m.View()
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !contains(visible, "jawaban singkat") {
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
	if !contains(m.pagerContent, "JAWABAN TERAKHIR") {
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
	if m.pendingQueue[0] != "prompt kedua saat turn jalan" {
		t.Fatalf("queued prompt mismatch: %q", m.pendingQueue[0])
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
// chrome above the input (not in the chat log) with a count and one row each.
func TestQueueBlockRendersAboveInput(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36
	m.turnRunning = true
	m.pendingQueue = []string{"prompt satu", "prompt dua panjang dengan banyak kata sehingga harus dirapikan ke satu baris preview"}
	m.status = "Queued..."
	if _, err := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36}); err != nil {
		t.Fatalf("window size update failed: %v", err)
	}
	v := m.View()
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !contains(visible, "ANTRIAN (2)") {
		t.Fatal("queue block header missing from View()")
	}
	if !contains(visible, "prompt satu") {
		t.Fatal("first queued row missing from View()")
	}
	// Long prompt flattened to one line (no newline in the row).
	if contains(visible, "banyak kata sehingga harus\n") {
		t.Fatal("queued prompt preview must be a single line")
	}
}

// TestQueueManageAltK verifies Alt+K enters queue management and e/d edit or
// delete the selected queued prompt, Esc/Alt+K exit.
func TestQueueManageAltK(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 36
	m.turnRunning = true
	m.pendingQueue = []string{"satu", "dua", "tiga"}

	altK := func() {
		if _, err := m.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModAlt}); err != nil {
			t.Fatalf("alt+k failed: %v", err)
		}
	}
	altK()
	if !m.queueMode {
		t.Fatal("alt+k did not enter queue mode")
	}
	if m.queueSel != 0 {
		t.Fatalf("expected selection at 0, got %d", m.queueSel)
	}

	// ↓ moves the selection.
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
	if len(m.pendingQueue) != 2 || m.pendingQueue[0] != "satu" || m.pendingQueue[1] != "tiga" {
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
	if len(m.pendingQueue) != 1 || m.pendingQueue[0] != "tiga" {
		t.Fatalf("e must remove the edited item, got %v", m.pendingQueue)
	}
	if m.promptInput.Value() != "satu" {
		t.Fatalf("e must load the prompt into the input, got %q", m.promptInput.Value())
	}
	if m.queueMode {
		t.Fatal("e must exit queue mode so the prompt can be edited")
	}

	// Alt+K again, then Esc exits.
	altK()
	if _, err := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); err != nil {
		t.Fatalf("esc failed: %v", err)
	}
	if m.queueMode {
		t.Fatal("esc did not exit queue mode")
	}

	// Deleting the last item auto-exits queue mode.
	altK()
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
	m.pendingQueue = []string{"q1", "q2"}
	m.queueSel = 1
	before := len(m.messages)

	// The returned Cmd is the next turn's runner (not an error) — startTurn
	// fires the drained prompt, so Update returns a real tea.Cmd here.
	m.Update(turnResultMsg{content: "jawaban"})
	if len(m.pendingQueue) != 1 || m.pendingQueue[0] != "q2" {
		t.Fatalf("queue must drain the first item, got %v", m.pendingQueue)
	}
	if m.queueSel != 0 {
		t.Fatalf("queue selection must clamp after drain, got %d", m.queueSel)
	}
	if len(m.messages) != before+2 { // 1 BROCODE answer + 1 YOU (drained prompt)
		t.Fatalf("expected answer + drained prompt appended, got %d -> %d messages", before, len(m.messages))
	}
	if !contains(m.messages[len(m.messages)-2], "jawaban") {
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
	return NewApp(cfg, p, "test-model", nil, tool.NewRegistry(), ctx, nil, nil, nil, 0, nil, "⚡ test")
}

func longAnswer(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += "line of the long answer with some detail and context\n"
	}
	return s
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
	tmp := NewApp(provider.AppConfig{}, provider.DetectedProvider{}, "test-model", nil, tool.NewRegistry(), ctx, nil, nil, nil, 0, seeded, "⚡ test")
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
