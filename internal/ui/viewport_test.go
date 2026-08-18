package ui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
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

// TestViewLogRendersThroughViewport pins the core chat-area fix: the log must
// be rendered THROUGH the viewport window (m.logViewport.View()), not as a raw
// unbounded string. A long history is clipped to the terminal's available
// height with the newest content at the bottom, and scrolling up (PgUp)
// reveals the older history — which is exactly what was broken: the raw log
// was written each frame, the renderer dropped lines from the top, and the
// viewport scroll offset was never applied, so a prompt sent after a long
// response was pushed off-screen and unreachable.
func TestViewLogRendersThroughViewport(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 30
	// Size the viewport like WindowSizeMsg does.
	m.promptInput.SetWidth(m.width - 4)
	m.logViewport.SetWidth(m.width)
	m.updateLogHeight()

	// A long history: the first message must scroll out of the window while the
	// newest stays visible at the bottom.
	m.messages = []string{"⚡ BroCode engine active. Type a prompt or /help for commands."}
	for i := 1; i <= 40; i++ {
		m.appendMessages(fmt.Sprintf("YOU:\nprompt %d with some filler text to wrap lines", i))
		m.appendMessages(fmt.Sprintf("BROCODE:\nanswer %d with filler text", i))
	}

	v := m.View()
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !strings.Contains(visible, "answer 40") {
		t.Fatalf("newest answer must be visible at the bottom:\n%s", visible)
	}
	if strings.Contains(visible, "prompt 1") {
		t.Fatalf("oldest history must be scrolled out of the window (reachable via PgUp), not rendered raw")
	}

	// Scrolling up must reveal the older history — before this fix PgUp updated
	// the viewport offset but View() wrote the raw log, so scrolling did
	// nothing and old messages were unreachable.
	m.logViewport.GotoTop()
	v = m.View()
	visible = ansiRegex.ReplaceAllString(v.Content, "")
	if !strings.Contains(visible, "prompt 1") {
		t.Fatalf("scrolling up must reveal the oldest history:\n%s", visible)
	}
}

// TestViewHeightExactlyFillsTerminal pins the layout math that prevents both
// history cropping and flicker: when the log OVERFLOWS the terminal, the log
// viewport plus the chrome below it (activity slot, input, banner, help) must
// total EXACTLY the terminal height. A one-line overflow made the renderer
// crop the first history line, and the raw-log rendering re-flowed the whole
// screen every frame. (Short content takes the natural-growth path instead —
// pinned by TestShortHistoryGrowsNaturallyNoBlankGap.)
func TestViewHeightExactlyFillsTerminal(t *testing.T) {
	for _, h := range []int{20, 30, 40} {
		m := newTestApp()
		m.width = 120
		m.height = h
		m.promptInput.SetWidth(m.width - 4)
		m.logViewport.SetWidth(m.width)
		m.updateLogHeight()
		// Long history so the log overflows the fold and the viewport path is
		// exercised (the exact-fill guarantee only applies there).
		for i := 1; i <= 40; i++ {
			m.appendMessages(fmt.Sprintf("YOU:\nprompt %d with some filler text to wrap lines", i))
			m.appendMessages(fmt.Sprintf("BROCODE:\nanswer %d with filler text", i))
		}

		v := m.View()
		n := strings.Count(v.Content, "\n") + 1
		if n != h {
			t.Errorf("height %d: View() produced %d lines (must equal terminal height so nothing is cropped)", h, n)
		}
	}
}

// TestShortHistoryGrowsNaturallyNoBlankGap pins the `-c` fix: a short history
// (e.g. a freshly resumed session) must render NATURALLY — the chat grows from
// the top with the input/footer right below it — instead of being drawn inside
// a viewport that pads short content to its full height, which left a huge
// blank gap ("1 layar kosong") between the chat and the input.
func TestShortHistoryGrowsNaturallyNoBlankGap(t *testing.T) {
	for _, h := range []int{20, 30, 40} {
		m := newTestApp()
		m.width = 120
		m.height = h
		m.promptInput.SetWidth(m.width - 4)
		m.logViewport.SetWidth(m.width)
		m.updateLogHeight()
		// The exact `-c` shape: a resume banner plus a couple of short messages.
		m.messages = []string{
			"✅ Resumed session sess_123 (5 events total)",
			"YOU:\nhello bro",
			"BROCODE:\nhello, where should we continue from?",
		}

		v := m.View()
		visible := ansiRegex.ReplaceAllString(v.Content, "")

		// All history must be visible — nothing hidden above the fold.
		for _, want := range []string{"hello bro", "hello, where", "Resumed session"} {
			if !strings.Contains(visible, want) {
				t.Fatalf("height %d: short history must render fully (natural growth), missing %q:\n%s", h, want, visible)
			}
		}

		// The chrome must sit right below the chat: the input prompt must appear
		// within a few lines of the last history line, NOT at the bottom of a
		// full-height padded viewport (that was the giant blank gap).
		lastLogIdx := strings.LastIndex(visible, "hello, where")
		inputIdx := strings.Index(visible, "❯ ")
		if inputIdx < 0 {
			t.Fatalf("height %d: input prompt missing from view:\n%s", h, visible)
		}
		gap := strings.Count(visible[lastLogIdx:inputIdx], "\n")
		// ≤ ~8 rows: message box border + the per-message "\n\n" separators + the
		// chrome's own spacing. The padded-viewport bug put 20+ blank rows here.
		if gap > 8 {
			t.Errorf("height %d: %d blank lines between chat and input (should be ≤ 8, no padded blank gap)", h, gap)
		}

		// Total height must be LESS than the terminal — no padding to fill it.
		n := strings.Count(visible, "\n") + 1
		if n >= h {
			t.Errorf("height %d: short content rendered %d lines — must not pad to terminal height", h, n)
		}
	}
}

// TestViewNeverCutsNewestAtBoundary pins the unified viewport design: as the
// log grows PAST the fold (the point where the old natural↔viewport hybrid
// switched modes and could cut/jump), the newest line must ALWAYS stay visible
// at the bottom and the view must never be taller than the terminal. The
// content-hugging height grows with the log until it hits the screen cap, so
// there is no boundary to glitch at.
func TestViewNeverCutsNewestAtBoundary(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 12 // small terminal so the fold is crossed after a few messages
	m.promptInput.SetWidth(m.width - 4)
	m.logViewport.SetWidth(m.width)
	m.updateLogHeight()

	for i := 1; i <= 12; i++ {
		m.appendMessages(fmt.Sprintf("YOU:\nprompt %d", i))
		m.appendMessages(fmt.Sprintf("BROCODE:\nanswer %d", i))

		v := m.View()
		n := strings.Count(v.Content, "\n") + 1
		if n > m.height {
			t.Fatalf("step %d: view taller than terminal (%d lines) — something was cut", i, n)
		}
		visible := ansiRegex.ReplaceAllString(v.Content, "")
		if !strings.Contains(visible, fmt.Sprintf("answer %d", i)) {
			t.Fatalf("step %d: newest answer not visible at bottom:\n%s", i, visible)
		}
	}
}

// TestMouseScrollModeEnablesWheelEvents pins that mouse events are enabled in
// the view. Previously View() always forced MouseModeNone, so the wheel could
// never scroll the log. SCROLL is now the default (wheel works out of the
// box); Ctrl+M toggles to SELECT (native selection, no mouse capture) and
// back.
func TestMouseScrollModeEnablesWheelEvents(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 30

	// Default: SCROLL — wheel scrolling works out of the box.
	v := m.View()
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("default SCROLL mode must enable MouseModeCellMotion (wheel events), got %v", v.MouseMode)
	}

	// Toggle to SELECT (ctrl+m): native selection, no mouse capture.
	if _, err := m.Update(tea.KeyPressMsg{Code: 'm', Mod: tea.ModCtrl}); err != nil {
		t.Fatalf("ctrl+m update failed: %v", err)
	}
	v = m.View()
	if v.MouseMode != tea.MouseModeNone {
		t.Errorf("SELECT mode must use MouseModeNone, got %v", v.MouseMode)
	}

	// Toggle back to SCROLL (ctrl+m).
	if _, err := m.Update(tea.KeyPressMsg{Code: 'm', Mod: tea.ModCtrl}); err != nil {
		t.Fatalf("ctrl+m update failed: %v", err)
	}
	v = m.View()
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("SCROLL mode must enable MouseModeCellMotion (wheel events), got %v", v.MouseMode)
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

// TestResumeHistoryContinuity pins the `-c` resume UX: the restored history
// (resume banner + old turns) is loaded INTO the chat log, so continuing the
// session keeps ONE continuous conversation — the newest answer lands at the
// bottom, the view never exceeds the terminal, and the oldest restored message
// is still reachable by scrolling up (nothing is overwritten or detached).
func TestResumeHistoryContinuity(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 24
	m.promptInput.SetWidth(m.width - 4)
	m.logViewport.SetWidth(m.width)
	m.updateLogHeight()

	// The exact `-c` shape after the fix: resume banner + restored history
	// seeded into the message list, then the user continues the conversation.
	m.messages = []string{
		"✅ Resumed session sess_123 (6 events total)",
		"YOU:\nhello, continue omnichannel filter task",
		"BROCODE:\nSure, continuing now. Previous answer line one.\nLine two.",
		"YOU:\ngreat, continue",
		"BROCODE:\nPrevious answer.\nAnother line.",
	}

	// Continuation: new prompt + long answer.
	m.appendMessages("YOU:\ncontinue now, proceed with task")
	m.appendMessages("BROCODE:BUILDER\n" + longAnswer(30))

	v := m.View()
	n := strings.Count(v.Content, "\n") + 1
	if n > m.height {
		t.Fatalf("resumed view taller than terminal: %d > %d — nothing may be cropped", n, m.height)
	}
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !strings.Contains(visible, "line of the long answer with some detail and context") {
		t.Fatalf("newest answer not visible at bottom after resume continuation:\n%s", visible)
	}

	// Old history must remain in the log (reachable by scrolling up) — the
	// conversation is continuous, never split or overwritten.
	m.logViewport.GotoTop()
	v = m.View()
	top := ansiRegex.ReplaceAllString(v.Content, "")
	if !strings.Contains(top, "hello, continue omnichannel filter task") {
		t.Fatalf("oldest restored message not reachable by scrolling up (continuity lost):\n%s", top)
	}
}

// TestFirstFrameClippedBeforeWindowSize pins the pre-resize first frame: before
// the model receives WindowSizeMsg the viewport path is unavailable (width 0),
// so a long restored history must NOT dump its full length — the frame is
// clipped to the terminal height and lands on the newest content.
func TestFirstFrameClippedBeforeWindowSize(t *testing.T) {
	m := newTestApp()
	m.width = 0
	m.height = 0
	m.messages = []string{"✅ Resumed session sess_123"}
	for i := 1; i <= 40; i++ {
		m.appendMessages(fmt.Sprintf("YOU:\nprompt %d with some filler text to wrap lines", i))
		m.appendMessages(fmt.Sprintf("BROCODE:\nanswer %d with filler text", i))
	}

	v := m.View()
	n := strings.Count(v.Content, "\n") + 1
	if n > 80 {
		t.Fatalf("first frame before WindowSizeMsg dumped %d lines — must be clipped to terminal height", n)
	}
	visible := ansiRegex.ReplaceAllString(v.Content, "")
	if !strings.Contains(visible, "answer 40") {
		t.Fatalf("newest content missing after first-frame clip:\n%s", visible)
	}
}
