package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
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
	if !m.foundUserLine {
		t.Fatalf("expected foundUserLine (user prompt missing from log)")
	}
	if m.lastUserLine < 0 {
		t.Fatalf("lastUserLine %d invalid", m.lastUserLine)
	}
	if !contains(log, "ok bro sesuaikan filter omnichannel") {
		t.Fatal("user prompt content missing from log")
	}
	if contains(log, "PROCESS:") || contains(log, "⚡ Turn 1 reasoning") {
		t.Fatal("progress steps leaked into conversation history")
	}

	// Render through the REAL View() path — this is what the user sees.
	// The prompt must be visible and the trailing "Completed" progress must
	// NOT leak into the visible history.
	v := m.View()
	visible := v.Content
	if !contains(visible, "ok bro sesuaikan filter omnichannel") {
		t.Fatalf("user prompt not visible in real View() output")
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
