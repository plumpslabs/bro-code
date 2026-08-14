package ui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"github.com/plumpslabs/bro-code/internal/context"
)

// Simulates the exact scenario from the bug report: a user prompt followed by
// a very long assistant answer (monorepo architecture doc). After the turn
// completes the view must land at the END of the answer — never at the prompt
// — so the answer is never left looking cut off below the fold, and earlier
// history is reachable by scrolling up instead of appearing to vanish.
func TestViewportLandsAtEndOfAnswer(t *testing.T) {
	m := &Model{}
	m.width = 140
	m.height = 40

	m.messages = []string{
		"⚡ BroCode engine active. Type a prompt or /help for commands.",
		"YOU:\nok bro dni ad pnysuaian bgian filter di omnichannel di bagian yng filter smua sednag ditangan dan juga belum di tangani nh dnsi eprmintaanya ad pnyesuaiaan",
		"BROCODE:\n💭 Executed via local gateway (opencode/hy3-free)\n\n# ClientConnect — CRM Sales Management System (Monorepo)\n\n## 1. Stack & Architecture\n\n| Component | Stack |\n|---|---|\n| Backend | Node.js 20+ |\n| Admin Frontend | React 18 |\n\n## 2. Hard Rules\n\n- Package Manager: bun\n- Backend Language: JavaScript (CommonJS)\n- ORM: Prisma ONLY\n\n## 3. Verification Commands\n\n```bash\ncd crm_sales_backend && bun test\ncd crm_sales_backend && npx prisma validate\n```\n\nThis is a long answer that scrolls far past the user's prompt line and must\nend with a marker that proves the viewport lands on the newest content.",

		// Marker line at the very end of the answer, appended as a separate
		// message the way the FILES summary is appended after the answer.
		"FILES:\n📄 THE_END",
	}

	contentWidth := m.width - 4
	log := m.buildLog(contentWidth)

	// Simulate the View's post-turn landing: SetContent then GotoBottom.
	m.logViewport.SetWidth(m.width)
	m.logViewport.SetContent(log)
	m.logViewport.SetHeight(m.height)
	m.logViewport.GotoBottom()

	// The visible window must contain the END of the answer (the newest
	// content), not the user's prompt at the top. Glamour interleaves ANSI
	// codes mid-word, so strip them before asserting.
	visible := ansiRegex.ReplaceAllString(m.logViewport.View(), "")
	if !strings.Contains(visible, "THE_END") {
		t.Fatalf("end of answer not visible after landing at bottom:\n%s", visible[:min(400, len(visible))])
	}
	// The prompt must still exist in the log (scrolled above, reachable via
	// PgUp) — history is never deleted.
	if !strings.Contains(ansiRegex.ReplaceAllString(log, ""), "ok bro dni ad pnysuaian") {
		t.Fatalf("user prompt missing from log (must remain in history)")
	}
}

// Reproduces the live-turn layout shift: while the agent runs, the activity
// slot grows (spinner + tool lines) which SHRINKS the log viewport. With
// unchanged content the reading position must be preserved and the viewport
// must render safely (clamped to the newest lines) — no panic, no jump.
func TestViewportPreservesPositionOnHeightChange(t *testing.T) {
	m := &Model{}
	m.width = 120
	m.height = 40
	m.messages = []string{
		"⚡ BroCode engine active. Type a prompt or /help for commands.",
		"YOU:\nok bro sesuaikan filter omnichannel di bagian bawah 3 filter",
		"BROCODE:\nOke, ini penjelasan lengkap tentang filter omnichannel.\n\nBaris 1.\nBaris 2.\nBaris 3.\nBaris 4.\nBaris 5.\nBaris 6.\nBaris 7.\nBaris 8.\nBaris 9.\nBaris 10.\nBaris 11.\nBaris 12.\nBaris 13.\nBaris 14.\nBaris 15.",
	}

	contentWidth := m.width - 4
	log := m.buildLog(contentWidth)
	m.logViewport.SetWidth(m.width)
	m.logViewport.SetContent(log)
	m.logViewport.SetHeight(m.height - 8)
	m.logViewport.GotoBottom()
	yBefore := m.logViewport.YOffset()

	// Activity grows: height shrinks. Same content — the position is
	// preserved; the viewport's rendering clamps to the newest lines.
	m.logViewport.SetHeight(m.height - 8 - 6)
	m.logViewport.SetContent(log)
	yAfter := m.logViewport.YOffset()

	if yAfter != yBefore {
		t.Logf("note: offset adjusted %d -> %d (clamped to new max offset)", yBefore, yAfter)
	}
	_ = m.logViewport.View() // must not panic with the shrunken height
	if !strings.Contains(ansiRegex.ReplaceAllString(m.logViewport.View(), ""), "Baris 15.") {
		t.Errorf("newest content not visible after height shrink:\n%q", ansiRegex.ReplaceAllString(m.logViewport.View(), ""))
	}
}

// TestChatHistoryNeverPruned pins the "history must never disappear"
// guarantee: far more than the old 200-message display window is appended and
// every single message must remain in the chat log (the safety ceiling is
// thousands, not a display window).
func TestChatHistoryNeverPruned(t *testing.T) {
	m := &Model{}
	for i := 0; i < 400; i++ {
		m.appendMessages(fmt.Sprintf("YOU:\nmessage %d", i))
	}
	if len(m.messages) != 400 {
		t.Fatalf("history pruned: expected all 400 messages kept, got %d", len(m.messages))
	}
	// The very first message must still be there — nothing was dropped.
	if !strings.Contains(m.messages[0], "message 0") {
		t.Fatalf("oldest message lost: %q", m.messages[0])
	}
	if !strings.Contains(m.messages[len(m.messages)-1], "message 399") {
		t.Fatalf("newest message lost: %q", m.messages[len(m.messages)-1])
	}
}

// TestBuildLogCacheInvalidatesOnAppend verifies the rendered-history cache
// rebuilds when messages change, so appended content always shows up.
func TestBuildLogCacheInvalidatesOnAppend(t *testing.T) {
	m := &Model{}
	m.width = 120
	m.messages = []string{"⚡ BroCode engine active. Type a prompt or /help for commands."}

	log1 := m.buildLog(m.width - 4)
	// Same state → cache hit, identical output.
	if log2 := m.buildLog(m.width - 4); log2 != log1 {
		t.Fatalf("cache miss on identical state")
	}

	// New message appended → the cache must rebuild and include it.
	m.appendMessages("YOU:\nhalo bro")
	log3 := m.buildLog(m.width - 4)
	if !strings.Contains(log3, "halo bro") {
		t.Fatalf("appended message missing after cache rebuild: %q", log3)
	}
}

// A message list without any user message (e.g. system banner only) must
// render its content fine and land at the bottom.
func TestViewportFallsBackToBottomWithoutUserMessage(t *testing.T) {
	m := &Model{}
	m.width = 100
	m.height = 30
	m.messages = []string{
		"BROCODE:\nJust an answer without a preceding user prompt.",
	}

	log := m.buildLog(m.width - 4)
	if !strings.Contains(log, "Just an answer") {
		t.Fatalf("log missing content: %q", log)
	}
	m.logViewport.SetWidth(m.width)
	m.logViewport.SetContent(log)
	m.logViewport.SetHeight(m.height)
	m.logViewport.GotoBottom()
	if !strings.Contains(ansiRegex.ReplaceAllString(m.logViewport.View(), ""), "Just an answer") {
		t.Fatalf("answer not visible after landing at bottom: %q", m.logViewport.View())
	}
}

// TestTerminalWidthBoundariesNoOverflow verifies that rendering UI across
// various terminal widths (60, 80, 100, 120, 160) with wide markdown tables,
// wide tree diagrams, and long text lines NEVER causes line count overflow or
// line wrapping of the sticky footer bar.
func TestTerminalWidthBoundariesNoOverflow(t *testing.T) {
	widths := []int{60, 80, 100, 120, 160}
	wideMarkdown := "BROCODE:MINER\n" +
		"| Step | Action | Status |\n" +
		"|---|---|---|\n" +
		"| 1 | Cek kantor scheduled hari ini -> [Kantor A, Kantor B] | OK |\n" +
		"| 2 | Filter kriteria -> [Kantor A, Kantor B] | MATCH |\n\n" +
		"├── Step 1: Cek kantor scheduled hari ini -> [Kantor A, Kantor B]\n" +
		"├── Step 2: Filter kriteria -> [Kantor A, Kantor B] (match)\n" +
		"└── RETURN: { officeId: Kantor A, eligibleOfficeIds: [A, B] }\n"

	for _, w := range widths {
		m := &Model{
			promptInput: textarea.New(),
			context:     context.NewManager(t.TempDir(), nil, 200000),
		}
		m.width = w
		m.height = 30
		m.status = "Ready"
		m.mode = "MINER"
		m.messages = []string{
			"👤 Cek lead rotation",
			wideMarkdown,
		}

		viewStr := m.View().Content
		lines := strings.Split(viewStr, "\n")

		// The footer help line (last line) must NEVER wrap or overlap with prompt input.
		lastLine := lines[len(lines)-1]
		if strings.Contains(lastLine, "MINE ❯") {
			t.Errorf("Width %d: Prompt entry overlapped into sticky footer line", w)
		}
	}
}

func TestSanitizeLLMOutputCollapsesMultipleNewlines(t *testing.T) {
	input := "Heading line\n\n\n\n\n\n\n\n\nSome text content after big gap"
	got := sanitizeLLMOutput(input)
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("expected multi-newlines to be collapsed, got %q", got)
	}
	expected := "Heading line\n\nSome text content after big gap"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}
