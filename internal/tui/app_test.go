package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/plumpslabs/bro-code/internal/search"
)



func newTestModel() Model {
	m := New(search.New(search.SampleCorpus()), "0.1.0", "test", false)
	m.width = 120
	m.height = 30
	m.layout()
	return m
}

func enterKey() tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})
}

func runeKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)})
}

func ctrlCKey() tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl})
}

func TestViewRendersLandingOnFreshStart(t *testing.T) {
	m := newTestModel()
	if m.started {
		t.Fatal("fresh model must not be started")
	}
	v := m.View().Content
	for _, want := range []string{
		"╚═════╝", // pixel wordmark bottom row
		"lean · efficient · transparent",
		"type a message or /help to begin",
		"❯",
		"enter send",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("landing missing %q\n---\n%s", want, v)
		}
	}
}

func TestFirstSendStartsChatAndLeavesLanding(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("hello brocode")
	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if cmd == nil {
		t.Fatal("expected a command (spinner + agent work) after send")
	}
	if !m2.started {
		t.Fatal("expected started=true after first message")
	}
	if len(m2.chat) != 1 || m2.chat[0].role != roleUser || m2.chat[0].text != "hello brocode" {
		t.Fatalf("user message not appended: %+v", m2.chat)
	}
	if m2.input.Value() != "" {
		t.Fatalf("input not cleared after send, got %q", m2.input.Value())
	}
	if !m2.agentWorking {
		t.Fatal("expected agentWorking after send")
	}
	if v := m2.View().Content; strings.Contains(v, "╚═════╝") {
		t.Fatal("landing must disappear once the chat starts")
	}
}

func TestEnterWithEmptyInputDoesNothing(t *testing.T) {
	m := newTestModel()
	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if cmd != nil {
		t.Fatal("expected no command for empty input")
	}
	if len(m2.chat) != 0 {
		t.Fatalf("chat should stay empty, got %d messages", len(m2.chat))
	}
}

func TestAgentResultStreamsToCompletion(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.streaming = true
	reply := buildReply("/diff", m.index)
	updated, _ := m.Update(agentResultMsg{reply: reply})
	m2 := updated.(Model)
	if !m2.streaming {
		t.Fatal("expected streaming to start after agentResultMsg")
	}
	if len(m2.chat) != 1 || m2.chat[0].role != roleAgent {
		t.Fatalf("agent message not appended: %+v", m2.chat)
	}
	if m2.agentWorking {
		t.Fatal("agentWorking should be false once the reply arrives")
	}

	// Pump stream ticks until the reply is fully revealed.
	revealed := ""
	for i := 0; i < 500 && m2.streaming; i++ {
		next, _ := m2.Update(streamTickMsg{})
		m2 = next.(Model)
		if len(m2.chat[0].text) < len(revealed) {
			t.Fatal("streamed text shrank — stream must be monotonic")
		}
		revealed = m2.chat[0].text
	}
	if m2.streaming {
		t.Fatal("stream never completed within 500 ticks")
	}
	if want := truncate(reply.text, maxReplyLen); m2.chat[0].text != want {
		t.Fatalf("final streamed text mismatch\nwant: %q\n got: %q", want, m2.chat[0].text)
	}
}

func TestUserMessageHasVerticalBarNoLabel(t *testing.T) {
	// Direct unit check of the renderer: the bar must lead the text.
	got := newTestModel().renderUserMsg("halo pagi", 40)
	if !strings.Contains(got, "▌") || !strings.Contains(got, "halo pagi") {
		t.Fatalf("renderUserMsg missing bar or text, got: %q", got)
	}

	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "mantap"})
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "reply"})
	m.refreshChat()
	v := m.View().Content
	// Line-level check: one line must carry BOTH the bar and the user text.
	// (The status separator also contains a bar, so a bare Contains check
	// would be vacuous — this proves the bar sits on the message line.)
	found := false
	for _, ln := range strings.Split(v, "\n") {
		if strings.Contains(ln, "▌") && strings.Contains(ln, "mantap") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a line with both the bar and the user text:\n%s", v)
	}
	// No sender labels — clean text only (user ask, Aug 2026).
	for _, no := range []string{"user:", "you:", "brocode:"} {
		if strings.Contains(v, no) {
			t.Fatalf("labels must be gone (%q), got:\n%s", no, v)
		}
	}
}

func TestCollapsibleToggledWithCtrlO(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "intro", summary: "▶ summary", content: "FULL BLOCK", collapsed: true})
	m.refreshChat()

	// Collapsed by default: summary visible, content hidden.
	v := m.View().Content
	if !strings.Contains(v, "▶ summary") || strings.Contains(v, "FULL BLOCK") {
		t.Fatalf("expected collapsed state (summary only), got:\n%s", v)
	}

	// ctrl+o expands the last collapsible block.
	updated, _ := m.Update(ctrlOKey())
	m2 := updated.(Model)
	if m2.chat[len(m2.chat)-1].collapsed {
		t.Fatal("expected block expanded after ctrl+o")
	}
	v = m2.View().Content
	if !strings.Contains(v, "FULL BLOCK") {
		t.Fatalf("expected expanded content, got:\n%s", v)
	}

	// ctrl+o again collapses it back.
	updated, _ = m2.Update(ctrlOKey())
	m3 := updated.(Model)
	if !m3.chat[len(m3.chat)-1].collapsed {
		t.Fatal("expected block collapsed after second ctrl+o")
	}
}

func TestCtrlONothingToExpand(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "plain"})
	updated, _ := m.Update(ctrlOKey())
	m2 := updated.(Model)
	if !strings.Contains(m2.status, "nothing to expand") {
		t.Fatalf("expected status hint, got %q", m2.status)
	}
}

func TestConnectModal(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/connect")
	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if cmd != nil {
		t.Fatal("expected no agent command for /connect")
	}
	if !m2.connectOpen {
		t.Fatal("expected connect modal open")
	}
	v := m2.View().Content
	for _, want := range []string{"opencode", "antigravity", "claude", "deepseek"} {
		if !strings.Contains(v, want) {
			t.Fatalf("connect modal missing %q", want)
		}
	}

	// Esc closes the modal without side effects.
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m3 := updated.(Model)
	if m3.connectOpen {
		t.Fatal("expected modal closed on esc")
	}
}

func TestConnectModalOverLanding(t *testing.T) {
	m := newTestModel() // !started — modal must render over the landing
	m.input.SetValue("/connect")
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if !m2.connectOpen {
		t.Fatal("expected modal to open from the landing")
	}
	v := m2.View().Content
	if !strings.Contains(v, "connect provider") || !strings.Contains(v, "claude") {
		t.Fatalf("expected modal content over landing, got:\n%s", v)
	}
}

func TestConnectModalNavigation(t *testing.T) {
	m := newTestModel()
	m.connectOpen = true
	m.connectSel = 0

	// Down moves to the next provider; clamping at the last one.
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m2 := updated.(Model)
	if m2.connectSel != 1 {
		t.Fatalf("expected selection 1 after down, got %d", m2.connectSel)
	}
	for i := 0; i < 10; i++ {
		updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		m2 = updated.(Model)
	}
	if m2.connectSel != len(providers)-1 {
		t.Fatalf("expected selection clamped at last provider, got %d", m2.connectSel)
	}
	// Up returns toward the top.
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m3 := updated.(Model)
	if m3.connectSel != len(providers)-2 {
		t.Fatalf("expected selection moved up, got %d", m3.connectSel)
	}
}

func TestConnectSelectsProvider(t *testing.T) {
	m := newTestModel()
	m.connectOpen = true
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: '4', Text: "4"}))
	m2 := updated.(Model)
	if m2.connectSel != 3 {
		t.Fatalf("expected deepseek selected (index 3), got %d", m2.connectSel)
	}
	updated, _ = m2.Update(enterKey())
	m3 := updated.(Model)
	if m3.connectOpen {
		t.Fatal("modal should close after enter")
	}
	if !strings.Contains(m3.status, "deepseek") {
		t.Fatalf("expected deepseek in status, got %q", m3.status)
	}
}

func TestDiffReplyIsCollapsible(t *testing.T) {
	reply := buildReply("/diff", newTestModel().index)
	if reply.collapse == nil {
		t.Fatal("expected /diff reply to be collapsible")
	}
	if !strings.Contains(reply.collapse.summary, "diff") {
		t.Fatalf("expected diff summary, got %q", reply.collapse.summary)
	}
}

func TestSearchReplyHasThinkingCollapse(t *testing.T) {
	reply := buildReply("mcp", newTestModel().index)
	if reply.collapse == nil {
		t.Fatal("expected search reply to carry a collapsed thinking trace")
	}
	if !strings.Contains(reply.collapse.content, "BM25") {
		t.Fatalf("expected BM25 in thinking trace, got %q", reply.collapse.content)
	}
}

func ctrlOKey() tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModCtrl})
}

func TestSearchCommandRepliesWithResults(t *testing.T) {
	m := newTestModel()
	reply := buildReply("/search mcp", m.index)
	if !strings.Contains(reply.text, "MCP client") {
		t.Fatalf("expected MCP client in reply, got:\n%s", reply.text)
	}
	if len(reply.items) != 1 || reply.items[0].tool != "search" {
		t.Fatalf("expected one search activity item, got %+v", reply.items)
	}
}

func TestPlainQueryUsesSearchPipeline(t *testing.T) {
	m := newTestModel()
	reply := buildReply("diff", m.index)
	if !strings.Contains(reply.text, "match") {
		t.Fatalf("expected search reply, got:\n%s", reply.text)
	}
}

func TestSystemCommands(t *testing.T) {
	m := newTestModel()

	agents := buildReply("/agents", m.index)
	if !strings.Contains(agents.text, "Primary agent") || !strings.Contains(agents.text, "finder") {
		t.Fatalf("expected agents reply, got:\n%s", agents.text)
	}
	mcp := buildReply("/mcp", m.index)
	if !strings.Contains(mcp.text, "MCP servers") || mcp.items[0].tool != "mcp" {
		t.Fatalf("expected mcp reply, got:\n%s", mcp.text)
	}
	usage := buildReply("/usage", m.index)
	if !strings.Contains(usage.text, "compaction") || !strings.Contains(usage.text, "L0") {
		t.Fatalf("expected usage reply, got:\n%s", usage.text)
	}
}

func TestUnknownCommandIsError(t *testing.T) {
	m := newTestModel()
	reply := buildReply("/nope", m.index)
	if !strings.Contains(reply.text, "Unknown command") {
		t.Fatalf("expected unknown-command error, got:\n%s", reply.text)
	}
	if len(reply.items) != 1 || reply.items[0].status != "error" {
		t.Fatalf("expected an error activity item, got %+v", reply.items)
	}
}

func TestNoResultsEmptyState(t *testing.T) {
	m := newTestModel()
	reply := buildReply("zzzz", m.index)
	if !strings.Contains(reply.text, "No tools or skills matched") {
		t.Fatalf("expected empty-state reply, got:\n%s", reply.text)
	}
}

func TestLogoBlockUniformWidth(t *testing.T) {
	// Precise centering requires the wordmark to be a rectangle: every line
	// must share one display width (Lip Gloss: trailing spaces skew Place).
	lines := strings.Split(padBlock(logoBlock), "\n")
	first := lipgloss.Width(lines[0])
	if first == 0 {
		t.Fatal("logo must not be empty")
	}
	for i, ln := range lines[1:] {
		if got := lipgloss.Width(ln); got != first {
			t.Fatalf("logo line %d width %d != %d — block is not uniform", i+2, got, first)
		}
	}
}

func TestCenterBlockIsSymmetric(t *testing.T) {
	// Even leftover: padding must be identical on both sides.
	out := centerBlock("123456", 12)
	left := len(out) - len(strings.TrimLeft(out, " "))
	right := len(out) - len(strings.TrimRight(out, " "))
	if left != right {
		t.Fatalf("centering asymmetric: left=%d right=%d (line=%q)", left, right, out)
	}
	if len(out) != 12 {
		t.Fatalf("centered line must fill the width exactly, got %d", len(out))
	}

	// Odd leftover: the extra column goes to the right (standard convention).
	out = centerBlock("12345", 12) // content 5, leftover 7
	left = len(out) - len(strings.TrimLeft(out, " "))
	right = len(out) - len(strings.TrimRight(out, " "))
	if left != 3 || right != 4 {
		t.Fatalf("odd leftover must go right: left=%d right=%d (line=%q)", left, right, out)
	}
}

func TestLandingLogoExactlyCentered(t *testing.T) {
	// Regression guard for the double-centering drift: every logo row must
	// start at exactly (width - logoWidth)/2. If Place ever re-centers the
	// block a second time, the left pad grows and this test fails.
	m := newTestModel()
	m.width, m.height = 80, 30
	m.layout()
	v := ansiStrip.ReplaceAllString(m.View().Content, "")
	want := (80 - logoWidth) / 2
	var lefts []int
	for _, ln := range strings.Split(v, "\n") {
		if strings.Contains(ln, "██") || strings.Contains(ln, "╚═") {
			left := len(ln) - len(strings.TrimLeft(ln, " "))
			lefts = append(lefts, left)
			if left != want {
				t.Fatalf("logo row left pad %d != want %d (centering drift)", left, want)
			}
		}
	}
	if len(lefts) != 6 {
		t.Fatalf("expected 6 logo rows, got %d", len(lefts))
	}
}

func TestClearCommandReturnsToLanding(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "keep me?"})
	m.input.SetValue("/clear")
	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if cmd != nil {
		t.Fatal("expected no command for /clear")
	}
	if m2.started {
		t.Fatal("expected started=false after /clear")
	}
	if len(m2.chat) != 0 {
		t.Fatalf("chat not cleared: %+v", m2.chat)
	}
	if v := m2.View().Content; !strings.Contains(v, "╚═════╝") {
		t.Fatal("expected landing after /clear")
	}
}

func TestThemeCommandOpensPicker(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/theme")
	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if cmd != nil {
		t.Fatal("expected no agent command for /theme")
	}
	if !m2.themeOpen {
		t.Fatal("expected theme picker modal open")
	}
	if m2.themeName != "default" {
		t.Fatalf("theme must not change until applied, got %q", m2.themeName)
	}
	v := m2.View().Content
	for _, want := range []string{"select theme", "default", "mono", "ocean"} {
		if !strings.Contains(v, want) {
			t.Fatalf("theme picker missing %q\n---\n%s", want, v)
		}
	}
	if m2.started {
		t.Fatal("opening the picker must not start a conversation")
	}
}

func TestHelpOnLandingStartsChat(t *testing.T) {
	m := newTestModel() // not started
	_, cmd := m.Update(runeKey('?'))
	if cmd == nil {
		t.Fatal("expected help command on '?'")
	}
	updated, _ := m.Update(runeKey('?'))
	m2 := updated.(Model)
	if !m2.started {
		t.Fatal("expected '?' on the landing to start the chat (help must be visible)")
	}
	if !m2.agentWorking {
		t.Fatal("expected agentWorking after '?'")
	}
}

func TestThemePickerNavigatesAndApplies(t *testing.T) {
	m := newTestModel()
	m.themeOpen = true
	m.themeSel = 0 // default selected

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m2 := updated.(Model)
	if m2.themeSel != 1 {
		t.Fatalf("expected selection 1 after down, got %d", m2.themeSel)
	}
	updated, _ = m2.Update(enterKey())
	m3 := updated.(Model)
	if m3.themeOpen {
		t.Fatal("picker should close after enter")
	}
	if m3.themeName != "mono" {
		t.Fatalf("expected mono applied, got %q", m3.themeName)
	}
}

func TestThemePickerNumberKeyAndCancel(t *testing.T) {
	m := newTestModel()
	m.themeOpen = true
	m.themeSel = 0

	// Number key 3 → ocean (sorted names: default, mono, ocean).
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: '3', Text: "3"}))
	m2 := updated.(Model)
	if m2.themeSel != 2 {
		t.Fatalf("expected selection 2 via number key, got %d", m2.themeSel)
	}

	// Esc cancels without applying.
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m3 := updated.(Model)
	if m3.themeOpen {
		t.Fatal("expected picker closed on esc")
	}
	if m3.themeName != "default" {
		t.Fatalf("cancel must not change the theme, got %q", m3.themeName)
	}
}

func TestThemeSetAndReject(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/theme ocean")
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if m2.themeName != "ocean" {
		t.Fatalf("expected ocean theme, got %q", m2.themeName)
	}

	m2.input.SetValue("/theme nope")
	updated, _ = m2.Update(enterKey())
	m3 := updated.(Model)
	if m3.themeName != "ocean" {
		t.Fatalf("theme must stay ocean on unknown name, got %q", m3.themeName)
	}
	if !strings.Contains(m3.status, "unknown theme") {
		t.Fatalf("expected unknown-theme notice in status, got %q", m3.status)
	}
}

func TestSuggestionsVisibleWhileTypingSlash(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/the")
	if !m.suggestVisible() {
		t.Fatal("expected suggestions visible for /the")
	}
	v := m.View().Content
	if !strings.Contains(v, "/theme") {
		t.Fatalf("expected /theme in suggestion popup\n---\n%s", v)
	}

	// Empty value → no popup.
	m.input.SetValue("")
	if m.suggestVisible() {
		t.Fatal("suggestions must not show without a leading slash")
	}
}

func TestSuggestionsNavigateAndAcceptOnEnter(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/di")
	m.suggestSel = 0

	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if cmd == nil {
		t.Fatal("expected agent command after accepting /diff")
	}
	if !m2.started {
		t.Fatal("expected conversation started after accept")
	}
	if len(m2.chat) != 1 || m2.chat[0].text != "/diff" {
		t.Fatalf("expected /diff sent, got %+v", m2.chat)
	}
}

func TestSuggestionsAcceptThemeOpensPicker(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/them")
	m.suggestSel = 0

	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if cmd != nil {
		t.Fatal("expected no agent command — /theme routes to the picker")
	}
	if !m2.themeOpen {
		t.Fatal("expected theme picker after accepting /theme")
	}
	if m2.started {
		t.Fatal("picking a theme must not start a conversation")
	}
}

func TestSuggestionsDismissOnEsc(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/the")
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m2 := updated.(Model)
	if m2.suggestVisible() {
		t.Fatal("expected suggestions dismissed on esc")
	}
	// Typing again re-enables the popup.
	updated, _ = m2.Update(runeKey('m'))
	m3 := updated.(Model)
	if !m3.suggestVisible() {
		t.Fatal("expected suggestions back after typing")
	}
}

func TestQuitKeys(t *testing.T) {
	m := newTestModel()

	_, cmd := m.Update(ctrlCKey())
	if !expectsQuit(cmd) {
		t.Fatal("expected Quit for ctrl+c")
	}

	// 'q' with empty input must quit.
	_, cmd = m.Update(runeKey('q'))
	if !expectsQuit(cmd) {
		t.Fatal("expected Quit for 'q' with empty input")
	}

	// 'q' with non-empty input is a query character, NOT quit.
	m.input.SetValue("query")
	_, cmd = m.Update(runeKey('q'))
	if expectsQuit(cmd) {
		t.Fatal("'q' must not quit while typing")
	}
}

// expectsQuit reports whether running cmd produces a tea.QuitMsg.
func expectsQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestScrollKeysAlwaysWorkWithoutFocus(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.follow = false
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: strings.Repeat("filler line\n", 40)})
	m.refreshChat()

	// No focus machine exists — the input is always focused. Arrow keys
	// must still scroll the chat viewport.
	m.viewport.GotoTop()
	updated, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m2 := updated.(Model)
	if cmd != nil {
		t.Fatal("scrolling must not produce a command")
	}
	if m2.viewport.YOffset() <= 0 {
		t.Fatalf("expected scroll down from top, offset=%d", m2.viewport.YOffset())
	}

	// Mouse wheel scrolls too.
	m2.viewport.GotoTop()
	updated, _ = m2.Update(tea.MouseWheelMsg(tea.Mouse{X: 0, Y: 0, Button: tea.MouseWheelDown}))
	m3 := updated.(Model)
	if m3.viewport.YOffset() <= 0 {
		t.Fatalf("expected wheel scroll down, offset=%d", m3.viewport.YOffset())
	}
}

func TestPanelHiddenOnNarrowTerminals(t *testing.T) {
	m := newTestModel()
	m.width = 80
	m.layout()
	if m.showPanel {
		t.Fatal("status panel should be hidden on 80-col terminals")
	}
	m.width = 120
	m.layout()
	if !m.showPanel {
		t.Fatal("status panel should be visible on 120-col terminals")
	}
}

func TestPanelShowsStatusSections(t *testing.T) {
	m := newTestModel() // 120 cols → panel visible
	m.started = true
	m.activity = prependActivity(nil, activityItem{tool: "search", label: "search(\"mcp\")", status: "done", detail: "3 results · 11ms"})
	v := m.View().Content
	for _, want := range []string{"status", "context", "git", "branch", "mcp", "filter", "agents", "activity", "ctx", "search"} {
		if !strings.Contains(v, want) {
			t.Fatalf("status panel missing %q\n---\n%s", want, v)
		}
	}
}

func TestGitBranchDetection(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/feature-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	st := gitInfo()
	if st.branch != "feature-x" {
		t.Fatalf("expected branch feature-x, got %q", st.branch)
	}
	// Temp dirs live outside $HOME, so cwdShort falls back to the raw path —
	// it must still be non-empty and resolve to the actual cwd.
	if st.path == "" || st.path == "—" {
		t.Fatalf("expected a real cwd path, got %q", st.path)
	}
	if wd, _ := os.Getwd(); !strings.Contains(st.path, filepath.Base(wd)) {
		t.Fatalf("expected cwd %q inside path %q", wd, st.path)
	}
}

func TestTokenEstimateAndFormat(t *testing.T) {
	chat := []chatMsg{{role: roleUser, text: strings.Repeat("x", 400)}}
	if got := tokenEstimate(chat); got != 100 {
		t.Fatalf("expected 100 tokens for 400 chars, got %d", got)
	}
	if fmtTokens(137) != "137" || fmtTokens(1500) != "1.5k" || fmtTokens(2_000_000) != "2.0M" {
		t.Fatalf("fmtTokens misformats: %s %s %s", fmtTokens(137), fmtTokens(1500), fmtTokens(2_000_000))
	}
}

func TestConnectSetsProviderAndWindow(t *testing.T) {
	m := newTestModel()
	m.connectOpen = true
	m.connectSel = 0

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: '4', Text: "4"}))
	m2 := updated.(Model)
	updated, _ = m2.Update(enterKey())
	m3 := updated.(Model)
	if m3.provider != "deepseek" || m3.window != 131072 {
		t.Fatalf("expected deepseek/131072, got %q/%d", m3.provider, m3.window)
	}
	if !strings.Contains(m3.status, "deepseek") {
		t.Fatalf("expected deepseek in status, got %q", m3.status)
	}
}

func TestHeaderShowsCtx(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.provider = "claude"
	m.window = 200_000
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: strings.Repeat("x", 400)}) // ~100 tokens
	v := m.View().Content
	if !strings.Contains(v, "ctx 100 / 200.0k") {
		t.Fatalf("expected ctx estimate in header, got:\n%s", v)
	}
}

func TestSessionRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "latest.jsonl")
	msgs := []chatMsg{
		{role: roleUser, text: "hello"},
		{role: roleAgent, text: "hi there"},
		{role: roleSystem, text: "note"},
	}
	if err := saveSessionTo(msgs, path); err != nil {
		t.Fatal(err)
	}
	got, err := loadSessionFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(got))
	}
	for i, want := range msgs {
		if got[i].role != want.role || got[i].text != want.text {
			t.Fatalf("msg %d mismatch: got %+v, want %+v", i, got[i], want)
		}
	}
}

func TestLoadSessionMissingFile(t *testing.T) {
	got, err := loadSessionFrom(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing file, got %+v", got)
	}
}

func TestLoadSessionSkipsCorruptLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latest.jsonl")
	content := "{\"role\":\"user\",\"text\":\"good\"}\nnot-json\n{\"role\":\"agent\",\"text\":\"also good\"}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadSessionFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 valid messages (corrupt line skipped), got %+v", got)
	}
}

func TestResumeLoadsSession(t *testing.T) {
	// Point the session path at a temp dir so the test never touches the
	// real home directory.
	dir := t.TempDir()
	old := sessionPath
	sessionPath = func() (string, error) {
		return filepath.Join(dir, "latest.jsonl"), nil
	}
	defer func() { sessionPath = old }()

	if err := SaveSession([]chatMsg{{role: roleUser, text: "from before"}}); err != nil {
		t.Fatal(err)
	}
	m := New(search.New(search.SampleCorpus()), "0.1.0", "test", true)
	if !m.started {
		t.Fatal("expected started=true when a session is resumed")
	}
	if len(m.chat) == 0 || m.chat[0].text != "from before" {
		t.Fatalf("expected resumed message, got %+v", m.chat)
	}
}

func TestResumeWithoutSessionStaysOnLanding(t *testing.T) {
	dir := t.TempDir()
	old := sessionPath
	sessionPath = func() (string, error) {
		return filepath.Join(dir, "latest.jsonl"), nil
	}
	defer func() { sessionPath = old }()

	m := New(search.New(search.SampleCorpus()), "0.1.0", "test", true)
	if m.started {
		t.Fatal("expected landing when no session file exists")
	}
	if !strings.Contains(m.status, "no previous session") {
		t.Fatalf("expected no-session notice in status, got %q", m.status)
	}
}

func TestActivityBounded(t *testing.T) {
	items := make([]activityItem, 0, maxActivity+10)
	for i := 0; i < maxActivity+10; i++ {
		items = append(items, activityItem{tool: "t", label: "x", status: "done"})
	}
	got := prependActivity(nil, items...)
	if len(got) > maxActivity {
		t.Fatalf("activity not bounded: %d > %d", len(got), maxActivity)
	}
}

func TestChatBounded(t *testing.T) {
	m := newTestModel()
	for i := 0; i < maxHistory+10; i++ {
		m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "x"})
	}
	if len(m.chat) > maxHistory {
		t.Fatalf("chat not bounded: %d > %d", len(m.chat), maxHistory)
	}
}

func TestPromptQueueingAndManagement(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true // simulate busy agent

	// Submit input while agent is busy -> should queue
	m.input.SetValue("first queued prompt")
	updated, _ := m.send()
	m = updated

	if len(m.queue) != 1 {
		t.Fatalf("expected queue len 1, got %d", len(m.queue))
	}
	if m.queue[0] != "first queued prompt" {
		t.Fatalf("expected 'first queued prompt', got %q", m.queue[0])
	}

	m.input.SetValue("second queued prompt")
	updated, _ = m.send()
	m = updated

	if len(m.queue) != 2 {
		t.Fatalf("expected queue len 2, got %d", len(m.queue))
	}

	// Open /queue manager
	m.input.SetValue("/queue")
	updated, _ = m.send()
	m = updated

	if !m.queueOpen {
		t.Fatal("expected queue modal open")
	}

	// Test merge command ('m')
	m.queueSel = 1
	up, _ := m.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	m = up.(Model)

	if len(m.queue) != 1 {
		t.Fatalf("expected merged queue len 1, got %d", len(m.queue))
	}
	wantMerged := "first queued prompt second queued prompt"
	if m.queue[0] != wantMerged {
		t.Fatalf("expected %q, got %q", wantMerged, m.queue[0])
	}
}
