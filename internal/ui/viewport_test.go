package ui

import (
	"strings"
	"testing"
)

// Simulates the exact scenario from the bug report: a user prompt followed by
// a very long assistant answer (monorepo architecture doc). The viewport must
// park at the user's prompt line so it never disappears from the history.
func TestViewportParksAtUserPromptAfterLongAnswer(t *testing.T) {
	m := &Model{}
	m.width = 140
	m.height = 40

	m.messages = []string{
		"⚡ BroCode engine active. Type a prompt or /help for commands.",
		"YOU:\nok bro dni ad pnysuaian bgian filter di omnichannel di bagian yng filter smua sednag ditangan dan juga belum di tangani nh dnsi eprmintaanya ad pnyesuaiaan",
		"PROCESS:\n⚡ Turn 1 reasoning...",
		"PROCESS:\n⚡ Thinking & analyzing request...",
		"BROCODE:\n💭 Executed via local gateway (opencode/hy3-free)\n\n# ClientConnect — CRM Sales Management System (Monorepo)\n\n## 1. Stack & Architecture\n\n| Component | Stack |\n|---|---|\n| Backend | Node.js 20+ |\n| Admin Frontend | React 18 |\n\n## 2. Hard Rules\n\n- Package Manager: bun\n- Backend Language: JavaScript (CommonJS)\n- ORM: Prisma ONLY\n\n## 3. Verification Commands\n\n```bash\ncd crm_sales_backend && bun test\ncd crm_sales_backend && npx prisma validate\n```\n\nThis is a long answer that scrolls far past the user's prompt line.",
	}

	contentWidth := m.width - 4
	log := m.buildLog(contentWidth)

	t.Logf("lastUserLine = %d", m.lastUserLine)
	if m.lastUserLine <= 0 {
		t.Fatalf("expected lastUserLine > 0 (parked at user prompt), got %d", m.lastUserLine)
	}

	// The line where the user prompt starts must point at the "YOU" block.
	lines := strings.Split(log, "\n")
	if m.lastUserLine >= len(lines) {
		t.Fatalf("lastUserLine %d out of bounds (log has %d lines)", m.lastUserLine, len(lines))
	}
	at := lines[m.lastUserLine]
	if !strings.Contains(at, "YOU") {
		t.Fatalf("lastUserLine %d points at %q, expected the YOU block", m.lastUserLine, at)
	}

	// The prompt must be visible in the first viewport-height window.
	window := strings.Join(lines[m.lastUserLine:min(m.lastUserLine+m.height, len(lines))], "\n")
	if !strings.Contains(window, "ok bro dni ad pnysuaian") {
		t.Fatalf("user prompt not visible in first viewport window after parking:\n%s", window[:min(400, len(window))])
	}
}

// Reproduces the live-turn bug: while the agent runs, the activity slot grows
// (spinner + 5 tool lines) which SHRINKS the log viewport. The viewport must
// re-park at the user's prompt whenever the height changes — otherwise the
// prompt silently scrolls out of view until the turn completes.
func TestViewportReParksWhenActivityShrinksViewport(t *testing.T) {
	m := &Model{}
	m.width = 120
	m.height = 40
	m.messages = []string{
		"⚡ BroCode engine active. Type a prompt or /help for commands.",
		"YOU:\nok bro sesuaikan filter omnichannel di bagian bawah 3 filter",
	}

	contentWidth := m.width - 4
	log := m.buildLog(contentWidth)
	if !m.foundUserLine {
		t.Fatal("expected foundUserLine")
	}

	// Turn starts: set up viewport at full height (no activity yet) and park.
	m.logViewport.SetContent(log)
	m.logViewport.SetHeight(m.height - 8)
	m.parkAtUserPrompt()
	yBefore := m.logViewport.YOffset()

	// Activity grows: height shrinks by 6 lines (spinner + 5 steps). Same
	// content, new height — the parking logic must move the offset so the
	// prompt stays visible.
	m.logViewport.SetHeight(m.height - 8 - 6)
	m.parkAtUserPrompt()
	yAfter := m.logViewport.YOffset()

	if yAfter != yBefore && yAfter > m.lastUserLine {
		t.Errorf("re-park moved past the prompt: yBefore=%d yAfter=%d lastUserLine=%d", yBefore, yAfter, m.lastUserLine)
	}
	if yAfter > m.lastUserLine {
		t.Errorf("prompt scrolled out of view: yAfter=%d > lastUserLine=%d", yAfter, m.lastUserLine)
	}
	// The prompt must remain within the visible window after re-parking.
	logLines := strings.Count(log, "\n") + 1
	if m.lastUserLine >= logLines {
		t.Fatalf("lastUserLine %d out of bounds (log has %d lines)", m.lastUserLine, logLines)
	}
	lines := strings.Split(log, "\n")
	visible := strings.Join(lines[m.lastUserLine:min(m.lastUserLine+m.logViewport.Height(), len(lines))], "\n")
	if !strings.Contains(visible, "ok bro sesuaikan filter") {
		t.Errorf("prompt not visible after re-park:\n%q", visible)
	}
}

// A message list without any user message (e.g. system banner only) must fall
// back to the bottom of the log rather than parking at line 0.
func TestViewportFallsBackToBottomWithoutUserMessage(t *testing.T) {
	m := &Model{}
	m.width = 100
	m.height = 30
	m.messages = []string{
		"BROCODE:\nJust an answer without a preceding user prompt.",
	}

	log := m.buildLog(m.width - 4)
	if m.lastUserLine != 0 {
		t.Fatalf("expected lastUserLine 0 when no user message, got %d", m.lastUserLine)
	}
	if !strings.Contains(log, "Just an answer") {
		t.Fatalf("log missing content: %q", log)
	}
}
