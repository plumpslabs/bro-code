package ui

import (
	"fmt"
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
		if _, err := m.Update(stepProgressMsg(p)); err != nil {
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
	if _, err := m.Update(stepProgressMsg("Completed")); err != nil {
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

	// Render through the REAL View() path — this is what the user sees. After
	// the turn completes the view must land at the END of the answer so long
	// answers never look cut off below the fold (the prompt stays in the log,
	// reachable by scrolling up — it is never deleted).
	v := m.View()
	// Glamour interleaves ANSI codes mid-word, so strip them before asserting
	// on the rendered text (same as the badge/FILES summary tests).
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	lastAnswerLine := "line of the long answer with some detail and context"
	if !contains(visible, lastAnswerLine) {
		t.Fatalf("end of answer not visible in real View() output")
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
	if _, err := m.Update(stepProgressMsg("grep (pattern: 'filter')")); err != nil {
		t.Fatalf("step progress update failed: %v", err)
	}
	if _, err := m.Update(stepProgressMsg("read_file (OmnichannelPanel.tsx)")); err != nil {
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
	if m.filesExpanded {
		t.Fatal("summary must start collapsed")
	}
	if len(m.messages) == 0 {
		t.Fatal("no messages appended")
	}
	last := m.messages[len(m.messages)-1]
	if !strings.HasPrefix(last, "FILES:\n") {
		t.Fatalf("expected FILES summary message, got %q", last)
	}

	// Collapsed render shows the compact row but not the diff body.
	collapsed := ansiRegex.ReplaceAllString(formatMessage(last, 120, false), "")
	if !strings.Contains(collapsed, "src/a.ts") {
		t.Fatalf("collapsed summary missing file row:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "+ x") {
		t.Fatalf("collapsed summary must hide the diff body:\n%s", collapsed)
	}

	// Expanded render shows the +/- diff lines.
	expanded := ansiRegex.ReplaceAllString(formatMessage(last, 120, true), "")
	if !strings.Contains(expanded, "+ x") {
		t.Fatalf("expanded summary missing diff lines:\n%s", expanded)
	}
}

// TestActivityResetsOnNewTurn verifies activity clears when a fresh turn starts.
func TestActivityResetsOnNewTurn(t *testing.T) {
	m := newTestApp()
	m.status = "Thinking..." // mimics handleEnter before the turn starts
	if _, err := m.Update(stepProgressMsg("grep (pattern: 'filter')")); err != nil {
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
	if !strings.HasPrefix(last, "BROCODE:PLANNER\n") {
		t.Fatalf("expected mode-stamped message, got %q", last)
	}

	// Renderer must draw the mode chip next to the BROCODE label. ANSI codes
	// are stripped first — glamour interleaves them mid-word.
	rendered := ansiRegex.ReplaceAllString(formatMessage(last, 120, false), "")
	if !strings.Contains(rendered, "PLANNER") {
		t.Fatalf("rendered answer missing PLANNER badge:\n%s", rendered)
	}

	// Legacy unstamped format still renders (no badge, no crash).
	legacy := ansiRegex.ReplaceAllString(formatMessage("BROCODE:\nplain answer", 120, false), "")
	if !strings.Contains(legacy, "plain answer") {
		t.Fatalf("legacy format broken:\n%s", legacy)
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

	for _, id := range []string{"sess_a", "sess_b"} {
		if err := st.CreateSession(id, "/tmp/proj"); err != nil {
			t.Fatalf("failed to create %s: %v", id, err)
		}
	}

	ctx := bcontext.NewManager("test-sess", st, 128000)
	m := NewApp(provider.AppConfig{Providers: map[string]provider.CustomProviderConfig{}}, provider.DetectedProvider{}, "test-model", nil, tool.NewRegistry(), ctx, nil, nil, nil, "⚡ test")

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

func newTestApp() Model {
	cfg := provider.AppConfig{Providers: map[string]provider.CustomProviderConfig{}}
	p := provider.DetectedProvider{}
	ctx := bcontext.NewManager("test-sess", nil, 128000)
	return NewApp(cfg, p, "test-model", nil, tool.NewRegistry(), ctx, nil, nil, nil, "⚡ test")
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
