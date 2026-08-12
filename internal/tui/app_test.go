package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/tokens"
)

func newTestModel() Model {
	m := New(search.New(search.SampleCorpus()), "0.1.0", "test", false)
	m.width = 120
	m.height = 50
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
		"|____/", // pixel wordmark bottom row (figlet standard)
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
	// Single-view contract (Phase 2): the user message must appear in the
	// chat body — the viewport is the one layout, the brand block is a
	// scrollable prefix, never a separate landing screen.
	v := m2.View().Content
	if !strings.Contains(v, "hello brocode") {
		t.Fatalf("sent message must be visible in the chat body:\n%s", v)
	}
	// The brand hint (fresh-start text) must disappear once the chat runs.
	if strings.Contains(v, "type a message or /help to begin") {
		t.Fatal("fresh-start hint must not show while the chat is active")
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
	got := newTestModel().renderUserMsg("hello world", 40)
	if !strings.Contains(got, "▌") || !strings.Contains(got, "hello world") {
		t.Fatalf("renderUserMsg missing bar or text, got: %q", got)
	}

	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "great"})
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "reply"})
	m.refreshChat()
	v := m.View().Content
	// Line-level check: one line must carry BOTH the bar and the user text.
	// (The status separator also contains a bar, so a bare Contains check
	// would be vacuous — this proves the bar sits on the message line.)
	found := false
	for _, ln := range strings.Split(v, "\n") {
		if strings.Contains(ln, "▌") && strings.Contains(ln, "great") {
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

func TestModelsModalShowsAllProviders(t *testing.T) {
	m := newTestModel()
	m.modelsOpen = true
	m.modelsSel = 0
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	// Models from every provider must be visible, each with its provider label.
	for _, want := range []string{
		"opencode",
		"antigravity",
		"groq",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("models modal missing %q\n---\n%s", want, v)
		}
	}
}

func TestModelsModalSearchFiltersAcrossProviders(t *testing.T) {
	m := newTestModel()
	m.modelsOpen = true
	m.modelsSel = 0

	// Type "llama" → only groq models remain.
	for _, ch := range "llama" {
		updated, _ := m.Update(runeKey(ch))
		m = updated.(Model)
	}
	if m.modelsQuery != "llama" {
		t.Fatalf("expected query 'llama', got %q", m.modelsQuery)
	}
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "llama-3.3-70b-versatile") {
		t.Fatalf("expected llama model in filtered view\n---\n%s", v)
	}
	if strings.Contains(v, "gemini-3.6-flash") {
		t.Fatalf("non-matching model leaked into filtered view\n---\n%s", v)
	}

	// Enter applies the first match (llama-3.3-70b-versatile) and switches provider to groq.
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if m2.modelsOpen {
		t.Fatal("modal should close after enter")
	}
	if m2.selectedModel != "llama-3.3-70b-versatile" {
		t.Fatalf("expected llama-3.3-70b-versatile, got %q", m2.selectedModel)
	}
	if m2.provider != "groq" {
		t.Fatalf("expected provider groq (switched by selection), got %q", m2.provider)
	}
}

func TestModelsModalSearchByProviderName(t *testing.T) {
	m := newTestModel()
	m.modelsOpen = true

	// Search by provider name — "antigravity" matches all its models.
	for _, ch := range "antigravity" {
		updated, _ := m.Update(runeKey(ch))
		m = updated.(Model)
	}
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "gemini") {
		t.Fatalf("expected antigravity models when searching by provider\n---\n%s", v)
	}
	if strings.Contains(v, "deepseek-v4-flash-free") {
		t.Fatalf("opencode model leaked into antigravity-only filter\n---\n%s", v)
	}
}

func TestModelsModalSearchNoMatchAndClear(t *testing.T) {
	m := newTestModel()
	m.modelsOpen = true

	for _, ch := range "zzzz" {
		updated, _ := m.Update(runeKey(ch))
		m = updated.(Model)
	}
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "no models match") {
		t.Fatalf("expected no-match message\n---\n%s", v)
	}

	// Esc clears the query first, then a second esc closes the modal.
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m2 := updated.(Model)
	if m2.modelsQuery != "" {
		t.Fatalf("expected query cleared on esc, got %q", m2.modelsQuery)
	}
	if !m2.modelsOpen {
		t.Fatal("modal should stay open after clearing the query")
	}
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m3 := updated.(Model)
	if m3.modelsOpen {
		t.Fatal("second esc should close the modal")
	}
}

func TestModelsModalBackspaceEditsQuery(t *testing.T) {
	m := newTestModel()
	m.modelsOpen = true
	m.modelsQuery = "gem"

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyBackspace}))
	m2 := updated.(Model)
	if m2.modelsQuery != "ge" {
		t.Fatalf("expected query 'ge' after backspace, got %q", m2.modelsQuery)
	}
}

func TestModelsModalQClosesWhenSearchEmpty(t *testing.T) {
	m := newTestModel()
	m.modelsOpen = true
	m.modelsQuery = ""

	// 'q' with an empty search box closes the modal (consistent with other modals).
	updated, _ := m.Update(runeKey('q'))
	m2 := updated.(Model)
	if m2.modelsOpen {
		t.Fatal("expected modal closed on 'q' with empty search")
	}

	// 'q' with an active query types into the search instead.
	m.modelsOpen = true
	m.modelsQuery = "ge"
	updated, _ = m.Update(runeKey('q'))
	m3 := updated.(Model)
	if !m3.modelsOpen {
		t.Fatal("modal must stay open while searching")
	}
	if m3.modelsQuery != "geq" {
		t.Fatalf("expected query 'geq', got %q", m3.modelsQuery)
	}
}

func TestModelsModalResetsQueryOnOpen(t *testing.T) {
	m := newTestModel()
	// Simulate a stale query from a previous session.
	m.modelsQuery = "gem"
	m.input.SetValue("/models")
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if !m2.modelsOpen {
		t.Fatal("expected /models modal open")
	}
	if m2.modelsQuery != "" {
		t.Fatalf("expected fresh empty query on open, got %q", m2.modelsQuery)
	}
}

func TestModelsModalNoOverflowNarrowTerminal(t *testing.T) {
	m := newTestModel()
	m.modelsOpen = true
	m.modelsSel = 0
	m.width = 44
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	for _, ln := range strings.Split(v, "\n") {
		if w := lipgloss.Width(ln); w > 44 {
			t.Fatalf("models modal row overflows %d cols on narrow terminal (got %d):\n%s", 44, w, v)
		}
	}
}

func TestPopoverTrimmedOnTinyTerminal(t *testing.T) {
	// Regression (review gate): the models popover used to grow taller than
	// the space above the fixed-bottom input, colliding with the input bar.
	// The popover must be trimmed from the top so the input stays usable.
	m := newTestModel()
	m.width, m.height = 80, 14
	m.layout()
	m.modelsOpen = true
	m.modelsSel = 0
	v := ansiStrip.ReplaceAllString(m.View().Content, "")
	lines := strings.Split(v, "\n")
	// Header + trimmed popover + input box + status must all fit in 14 rows.
	if len(lines) > 14 {
		t.Fatalf("view taller than terminal (%d lines):\n%s", len(lines), v)
	}
	// The input bar must still be present and not covered by the popover.
	if !strings.Contains(v, "❯") {
		t.Fatalf("input bar disappeared on tiny terminal:\n%s", v)
	}
	// The popover title must survive the trim (top rows are kept).
	if !strings.Contains(v, "active AI model") {
		t.Fatalf("popover title lost after trim:\n%s", v)
	}
}

func TestAntigravityModelsModal(t *testing.T) {
	m := newTestModel()
	m.provider = "antigravity"
	m.input.SetValue("/models")
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if !m2.modelsOpen {
		t.Fatal("expected /models modal open")
	}

	// Type "gemini-2" to search — filters to antigravity models specifically.
	for _, ch := range "gemini-2" {
		updated, _ = m2.Update(runeKey(ch))
		m2 = updated.(Model)
	}
	if m2.modelsQuery != "gemini-2" {
		t.Fatalf("expected query 'gemini-2', got %q", m2.modelsQuery)
	}

	// Enter selects the first match (gemini-2.5-flash) and switches provider.
	updated, _ = m2.Update(enterKey())
	m3 := updated.(Model)
	if !strings.Contains(m3.selectedModel, "gemini-2") {
		t.Fatalf("expected a gemini-2 model, got %q", m3.selectedModel)
	}
	if m3.provider != "antigravity" {
		t.Fatalf("expected provider antigravity, got %q", m3.provider)
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

	// Down moves to the next provider; wraps to first at the bottom (circular).
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m2 := updated.(Model)
	if m2.connectSel != 1 {
		t.Fatalf("expected selection 1 after down, got %d", m2.connectSel)
	}
	// Press down len(defaultProviders) more times — wraps around (circular navigation).
	for i := 0; i < len(defaultProviders); i++ {
		updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		m2 = updated.(Model)
	}
	// len(defaultProviders) providers, position 1 + len downs = wraps to 1.
	if m2.connectSel != 1 {
		t.Fatalf("expected circular wrap to 1, got %d", m2.connectSel)
	}
	// Up from 1 goes to 0.
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m3 := updated.(Model)
	if m3.connectSel != 0 {
		t.Fatalf("expected selection 0 after up, got %d", m3.connectSel)
	}
	// Up from 0 wraps to last provider.
	updated, _ = m3.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m4 := updated.(Model)
	if m4.connectSel != len(defaultProviders)-1 {
		t.Fatalf("expected circular wrap to last provider %d, got %d", len(defaultProviders)-1, m4.connectSel)
	}
}

func TestConnectSelectsProvider(t *testing.T) {
	m := newTestModel()
	m.connectOpen = true
	// Select poolside (index 4) — api key provider → opens key modal
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: '5', Text: "5"}))
	m2 := updated.(Model)
	if m2.connectSel != 4 {
		t.Fatalf("expected poolside selected (index 4), got %d", m2.connectSel)
	}
	updated, _ = m2.Update(enterKey())
	m3 := updated.(Model)
	if m3.connectOpen {
		t.Fatal("connect modal should close")
	}
	if !m3.apikeyOpen {
		t.Fatal("apikey modal should open for api-key provider")
	}
	if m3.apikeyTarget != "poolside" {
		t.Fatalf("expected apikeyTarget 'poolside', got %q", m3.apikeyTarget)
	}
}

func TestPasteGoesToMainInputNormally(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("type")
	updated, cmd := m.Update(tea.PasteMsg{Content: "-me-more"})
	m2 := updated.(Model)
	if cmd != nil {
		t.Fatal("paste must not return a command")
	}
	if m2.input.Value() != "type-me-more" {
		t.Fatalf("expected pasted text in main input, got %q", m2.input.Value())
	}
}

func TestPasteGoesToAPIKeyModalWhenOpen(t *testing.T) {
	m := newTestModel()
	m.apikeyOpen = true
	m.apikeyTarget = "groq"

	updated, cmd := m.Update(tea.PasteMsg{Content: "gsk_abc123def456"})
	m2 := updated.(Model)
	if cmd != nil {
		t.Fatal("paste must not return a command")
	}
	// The key must land in the API key input — NOT the main chat input.
	if m2.apikeyInput.Value() != "gsk_abc123def456" {
		t.Fatalf("expected pasted key in apikeyInput, got %q", m2.apikeyInput.Value())
	}
	if m2.input.Value() != "" {
		t.Fatalf("main input must stay empty while the key modal is open, got %q", m2.input.Value())
	}
}

func TestPasteNormalizesNewlines(t *testing.T) {
	m := newTestModel()
	updated, _ := m.Update(tea.PasteMsg{Content: "multi\r\nline\nkey"})
	m2 := updated.(Model)
	if m2.input.Value() != "multi line key" {
		t.Fatalf("expected newlines normalized to spaces, got %q", m2.input.Value())
	}
}

func TestLargePasteCompaction(t *testing.T) {
	m := newTestModel()
	hugePaste := strings.Repeat("log line text here\n", 10)
	updated, _ := m.Update(tea.PasteMsg{Content: hugePaste})
	m2 := updated.(Model)
	if !strings.Contains(m2.input.Value(), "[Pasted Snippet:") {
		t.Fatalf("expected large paste to be compacted into placeholder badge, got %q", m2.input.Value())
	}
	if m2.pastedText != hugePaste {
		t.Fatalf("expected full pasted content stored in m.pastedText")
	}
}

func TestEmptyPasteDoesNothing(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("keep")
	updated, _ := m.Update(tea.PasteMsg{Content: "\n\r\n"})
	m2 := updated.(Model)
	if m2.input.Value() != "keep" {
		t.Fatalf("expected input untouched, got %q", m2.input.Value())
	}
}

func TestRenderInlineMarkdownStripsMarkers(t *testing.T) {
	m := newTestModel()
	got := ansiStrip.ReplaceAllString(m.renderInlineMarkdown("**Pakai:** `./bin/brocode` dan teks biasa"), "")
	if strings.Contains(got, "**") {
		t.Fatalf("bold markers must be stripped, got %q", got)
	}
	if strings.Contains(got, "`") {
		t.Fatalf("code backticks must be stripped, got %q", got)
	}
	if !strings.Contains(got, "Pakai:") || !strings.Contains(got, "./bin/brocode") {
		t.Fatalf("inner text must survive styling, got %q", got)
	}
}

func TestRenderPlainStripsMarkdownMarkers(t *testing.T) {
	m := newTestModel()
	text := "**Pakai:** `./bin/brocode` (TUI) atau `-c` (resume).\n\n**Dev:** `make test`."
	got := ansiStrip.ReplaceAllString(m.renderPlain(text, 60), "")
	if strings.Contains(got, "**") {
		t.Fatalf("bold markers must not appear in rendered agent text, got:\n%s", got)
	}
	if strings.Contains(got, "`") {
		t.Fatalf("backticks must not appear in rendered agent text, got:\n%s", got)
	}
	if !strings.Contains(got, "Pakai:") || !strings.Contains(got, "./bin/brocode") || !strings.Contains(got, "make test") {
		t.Fatalf("plain content must survive, got:\n%s", got)
	}
}

func TestRenderPlainLongBoldWrapsCleanly(t *testing.T) {
	m := newTestModel()
	// A bold span longer than the width must wrap WITHOUT leaking asterisks.
	text := "**" + strings.Repeat("boldword ", 10) + "**"
	got := ansiStrip.ReplaceAllString(m.renderPlain(text, 30), "")
	if strings.Contains(got, "**") || strings.Contains(got, "*") {
		t.Fatalf("no stray asterisks allowed after wrap, got:\n%s", got)
	}
	for _, ln := range strings.Split(got, "\n") {
		if w := lipgloss.Width(ln); w > 28 {
			t.Fatalf("wrapped line overflows: %d > 28 (%q)", w, ln)
		}
	}
}

func TestRenderMarkdownBlockLevelStripsMarkers(t *testing.T) {
	m := newTestModel()
	// Headings: # prefix gone, inner **bold** and `code` markers gone too.
	got := ansiStrip.ReplaceAllString(m.renderMarkdown("## Dev: `make test` **race**"), "")
	for _, no := range []string{"##", "**", "`"} {
		if strings.Contains(got, no) {
			t.Fatalf("heading leaks marker %q: %q", no, got)
		}
	}
	if !strings.Contains(got, "Dev:") || !strings.Contains(got, "make test") || !strings.Contains(got, "race") {
		t.Fatalf("heading content lost: %q", got)
	}

	// Blockquote: > prefix gone, inner markers styled not literal.
	got = ansiStrip.ReplaceAllString(m.renderMarkdown("> **Important** `note`"), "")
	for _, no := range []string{">", "**", "`"} {
		if strings.Contains(got, no) {
			t.Fatalf("blockquote leaks marker %q: %q", no, got)
		}
	}
	if !strings.Contains(got, "Important") || !strings.Contains(got, "note") {
		t.Fatalf("blockquote content lost: %q", got)
	}
}

func TestRenderInlineMarkdownBoldWithNestedCode(t *testing.T) {
	m := newTestModel()
	got := ansiStrip.ReplaceAllString(m.renderInlineMarkdown("**Install with `make build`**"), "")
	if strings.Contains(got, "**") || strings.Contains(got, "`") {
		t.Fatalf("nested code inside bold leaks markers: %q", got)
	}
	if !strings.Contains(got, "make build") {
		t.Fatalf("nested code content lost: %q", got)
	}
}

func TestRenderPlainWrapsLongAgentText(t *testing.T) {
	m := newTestModel()
	long := "Ini kalimat yang sangat panjang sekali sehingga pasti melebihi lebar kolom chat dan harus dibungkus ke beberapa baris supaya tidak tembus ke panel status di sebelah kanan."
	got := ansiStrip.ReplaceAllString(m.renderPlain(long, 40), "")
	max := 0
	for _, ln := range strings.Split(got, "\n") {
		if w := lipgloss.Width(ln); w > max {
			max = w
		}
	}
	// wrapW = w-2 = 38; each wrapped line must fit inside it.
	if max > 38 {
		t.Fatalf("agent line overflows: max width %d > 38\n%s", max, got)
	}
	if len(strings.Split(got, "\n")) < 2 {
		t.Fatalf("expected the long line to wrap into multiple lines:\n%s", got)
	}
}

func TestRenderPlainWrapsLongUnbrokenToken(t *testing.T) {
	m := newTestModel()
	longURL := "https://accounts.google.com/o/oauth2/v2/auth?response_type=code&client_id=1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com&redirect_uri=http://127.0.0.1:51121/oauth-callback"
	got := ansiStrip.ReplaceAllString(m.renderPlain(longURL, 40), "")
	max := 0
	for _, ln := range strings.Split(got, "\n") {
		if w := lipgloss.Width(ln); w > max {
			max = w
		}
	}
	if max > 38 {
		t.Fatalf("long token still overflows: max width %d > 38\n%s", max, got)
	}
}

func TestRenderPlainClipsLongCodeLines(t *testing.T) {
	m := newTestModel()
	longCode := "```\n" + strings.Repeat("x", 120) + "\n```"
	got := ansiStrip.ReplaceAllString(m.renderPlain(longCode, 40), "")
	max := 0
	for _, ln := range strings.Split(got, "\n") {
		if w := lipgloss.Width(ln); w > max {
			max = w
		}
	}
	if max > 38 {
		t.Fatalf("code line overflows: max width %d > 38\n%s", max, got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("expected clipped code line to carry an ellipsis:\n%s", got)
	}
}

func TestAPIKeyModalBoundsInput(t *testing.T) {
	m := newTestModel()
	m.apikeyOpen = true
	m.apikeyTarget = "groq"
	m.apikeyInput.SetValue("gsk_" + strings.Repeat("a", 80))
	v := ansiStrip.ReplaceAllString(m.renderAPIKeyModalBox(), "")
	max := 0
	for _, ln := range strings.Split(v, "\n") {
		if w := lipgloss.Width(ln); w > max {
			max = w
		}
	}
	// Modal is min(56, width-4)=56 wide incl. borders → content ≤ 56.
	if max > 56 {
		t.Fatalf("api key modal overflows: max width %d > 56\n%s", max, v)
	}
	// The masked dots must scroll — never render one dot per key char.
	// Key is 84 chars; a bounded input renders far fewer dots.
	dots := strings.Count(v, "•")
	if dots >= 84 {
		t.Fatalf("expected bounded masked input (scrolling), got %d dots", dots)
	}
	if dots == 0 {
		t.Fatal("expected some masked dots to render")
	}
}

func TestModelsModal(t *testing.T) {
	m := newTestModel()
	m.modelsOpen = true
	m.modelsSel = 0
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m2 := updated.(Model)
	if m2.modelsSel != 1 {
		t.Fatalf("expected modelsSel 1 after down, got %d", m2.modelsSel)
	}
	updated, _ = m2.Update(enterKey())
	m3 := updated.(Model)
	if m3.modelsOpen {
		t.Fatal("modal should close after enter")
	}
	// The selected model should be from opencode provider (dynamic or static)
	if m3.provider != "opencode" {
		t.Fatalf("expected provider opencode, got %q", m3.provider)
	}
	if m3.selectedModel == "" {
		t.Fatal("expected a non-empty model name")
	}
}

func TestCopyShortcut(t *testing.T) {
	m := newTestModel()
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "hello world"})
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: 'y', Mod: tea.ModCtrl}))
	m2 := updated.(Model)
	if !strings.Contains(m2.status, "copied") {
		t.Fatalf("expected copy status notice, got %q", m2.status)
	}
}

func TestOpenCodeAutoDetection(t *testing.T) {
	detected, model := DetectOpenCode()
	if detected {
		if model == "" {
			t.Fatal("expected non-empty free model name when OpenCode detected")
		}
	}
	m := newTestModel()
	if detected && m.provider != "opencode" {
		t.Fatalf("expected opencode auto-selected on startup, got %q", m.provider)
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
	if !strings.Contains(agents.text, "Primary agent") || !strings.Contains(agents.text, "Fast Path") {
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
	logoLines := strings.Split(logoArt, "\n")
	// The logo is a 44-col rectangle; rows that start with a space (figlet
	// "standard" row 0) naturally show their first glyph one col right of the
	// pad — measure the rectangle's left edge, not the first non-space cell.
	matched := 0
	for _, wantLn := range logoLines {
		ownLead := len(wantLn) - len(strings.TrimLeft(wantLn, " "))
		for _, ln := range strings.Split(v, "\n") {
			if strings.TrimSpace(ln) == strings.TrimSpace(wantLn) {
				left := len(ln) - len(strings.TrimLeft(ln, " "))
				if left-ownLead != want {
					t.Fatalf("logo row left pad %d != want %d (centering drift): %q", left-ownLead, want, ln)
				}
				matched++
				break
			}
		}
	}
	if matched != len(logoLines) {
		t.Fatalf("expected %d centered logo rows, matched %d", len(logoLines), matched)
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
	if v := m2.View().Content; !strings.Contains(v, "|____/") {
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
	for _, want := range []string{"theme", "default", "mono", "ocean"} {
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

	// 'q' key does NOT quit (quit is ctrl+c only).
	_, cmd = m.Update(runeKey('q'))
	if expectsQuit(cmd) {
		t.Fatal("'q' must not quit the app")
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
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: strings.Repeat("filler line\n", 60)})
	m.refreshChat()

	// pgdown scrolls the viewport.
	m.viewport.GotoTop()
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgDown}))
	m2 := updated.(Model)
	if m2.viewport.YOffset() <= 0 {
		t.Fatalf("expected pgdown scroll, offset=%d", m2.viewport.YOffset())
	}
}

func TestMouseWheelScrollsChatViewport(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.follow = false
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: strings.Repeat("filler line\n", 60)})
	m.refreshChat()

	// Wheel down scrolls the viewport deeper into the chat.
	m.viewport.GotoTop()
	updated, _ := m.Update(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelDown}))
	m2 := updated.(Model)
	if m2.viewport.YOffset() <= 0 {
		t.Fatalf("expected wheel-down scroll, offset=%d", m2.viewport.YOffset())
	}

	// Wheel up scrolls back toward the top.
	updated, _ = m2.Update(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelUp}))
	m3 := updated.(Model)
	if m3.viewport.YOffset() >= m2.viewport.YOffset() {
		t.Fatalf("expected wheel-up to reduce offset: before=%d after=%d", m2.viewport.YOffset(), m3.viewport.YOffset())
	}
}

func TestMouseWheelDoesNotTouchInput(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.input.SetValue("typed text")
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: strings.Repeat("filler line\n", 60)})
	m.refreshChat()

	// Wheel gestures must never alter the input form content.
	before := m.input.Value()
	_, _ = m.Update(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelDown}))
	if m.input.Value() != before {
		t.Fatalf("wheel scrolled the form: input changed from %q to %q", before, m.input.Value())
	}
}

func TestMouseWheelIgnoredWhileModalOpen(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.modelsOpen = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: strings.Repeat("filler line\n", 60)})
	m.refreshChat()
	m.viewport.GotoTop()

	// Wheel must not scroll the hidden viewport behind a modal.
	_, _ = m.Update(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelDown}))
	if m.viewport.YOffset() != 0 {
		t.Fatalf("expected no scroll while modal open, offset=%d", m.viewport.YOffset())
	}
}

func TestArrowKeysNavigateHistory(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.promptHistory = []string{"hello", "world"}

	// ctrl+p from empty input → shows last history item
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: 'p', Mod: tea.ModCtrl}))
	m2 := updated.(Model)
	if m2.input.Value() != "world" {
		t.Fatalf("expected 'world', got %q", m2.input.Value())
	}
	if m2.historyIdx != 1 {
		t.Fatalf("expected historyIdx=1, got %d", m2.historyIdx)
	}

	// Up again (already in history) → shows older item
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m3 := updated.(Model)
	if m3.input.Value() != "hello" {
		t.Fatalf("expected 'hello', got %q", m3.input.Value())
	}

	// Down → goes back to newer item
	updated, _ = m3.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m4 := updated.(Model)
	if m4.input.Value() != "world" {
		t.Fatalf("expected 'world', got %q", m4.input.Value())
	}

	// Down again → back to draft
	updated, _ = m4.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m5 := updated.(Model)
	if m5.input.Value() != "" {
		t.Fatalf("expected empty draft, got %q", m5.input.Value())
	}
	if m5.historyIdx != -1 {
		t.Fatalf("expected historyIdx=-1, got %d", m5.historyIdx)
	}
}

func TestPanelHiddenOnNarrowTerminals(t *testing.T) {
	m := newTestModel()
	m.width = 80
	m.layout()
	if m.showPanel {
		t.Fatal("status panel should be hidden (panel disabled for clean layout)")
	}
	m.width = 120
	m.layout()
	if m.showPanel {
		t.Fatal("status panel should be hidden (panel disabled for clean layout)")
	}
}

func TestPanelShowsStatusSections(t *testing.T) {
	// Panel is disabled for clean chat-only layout
	// Use /info command instead for status information
	m := newTestModel()
	m.started = true
	m.activity = prependActivity(nil, activityItem{tool: "search", label: "search(\"mcp\")", status: "done", detail: "3 results · 11ms"})
	v := m.View().Content
	// Verify chat-only layout works without panel
	if strings.Contains(v, " status ") {
		t.Fatal("panel should be disabled for clean layout")
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
	// tokens.Estimate: ~4 Latin chars per token, ~1 token per CJK ideograph.
	if got := tokens.Estimate(strings.Repeat("x", 400)); got != 100 {
		t.Fatalf("expected 100 tokens for 400 latin chars, got %d", got)
	}
	if got := tokens.Estimate(strings.Repeat("漢", 200)); got != 200 {
		t.Fatalf("expected 200 tokens for 200 CJK ideographs, got %d", got)
	}
	if got := tokens.Estimate(""); got != 0 {
		t.Fatalf("expected 0 for empty string, got %d", got)
	}
	// chatTokens adds ~4 tokens of role overhead per message.
	chat := []chatMsg{{role: roleUser, text: strings.Repeat("x", 400)}}
	if got := chatTokens(chat); got != 104 {
		t.Fatalf("expected 104 with role overhead, got %d", got)
	}
	if fmtTokens(137) != "137" || fmtTokens(1500) != "1.5k" || fmtTokens(2_000_000) != "2.0M" {
		t.Fatalf("fmtTokens misformats: %s %s %s", fmtTokens(137), fmtTokens(1500), fmtTokens(2_000_000))
	}
}

// buildBigChat builds a chat long enough to trip the 70% compaction trigger
// on a small window.
func buildBigChat(n int) []chatMsg {
	chat := make([]chatMsg, 0, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			chat = append(chat, chatMsg{role: roleUser, text: "pertanyaan " + strings.Repeat("x", 60) + fmt.Sprintf(" #%d", i)})
		} else {
			chat = append(chat, chatMsg{role: roleAgent, text: "jawaban " + strings.Repeat("y", 200) + fmt.Sprintf(" #%d", i)})
		}
	}
	return chat
}

func TestMaybeCompactTiers(t *testing.T) {
	m := newTestModel()
	m.window = 500
	m.chat = buildBigChat(12) // 6 user + 6 agent, ~468 tokens — over 70% of 500
	if !m.maybeCompact() {
		t.Fatal("expected compaction to run on an over-trigger transcript")
	}
	if m.compactCount != 1 {
		t.Fatalf("expected compactCount=1, got %d", m.compactCount)
	}
	// L0 goal pinned: the first user message survives verbatim.
	if m.chat[0].role != roleUser || !strings.Contains(m.chat[0].text, "#0") {
		t.Fatalf("L0 goal message must stay verbatim, got %q", m.chat[0].text)
	}
	// The middle became a visible L2 ledger (system role).
	foundLedger := false
	for _, cm := range m.chat {
		if cm.role == roleSystem && strings.Contains(cm.text, "context summary") {
			foundLedger = true
		}
	}
	if !foundLedger {
		t.Fatal("expected a visible L2 ledger message after compaction")
	}
	// Recent tail messages stay verbatim.
	if last := m.chat[len(m.chat)-1]; last.role != roleAgent {
		t.Fatalf("expected the last message to survive, got role %d", last.role)
	}
}

func TestMaybeCompactSkipsSmallSessions(t *testing.T) {
	m := newTestModel()
	m.window = 1000
	m.chat = buildBigChat(4) // below compactMinMsgs (8)
	if m.maybeCompact() {
		t.Fatal("must never compact a tiny session")
	}
	if m.compactCount != 0 {
		t.Fatalf("expected compactCount=0, got %d", m.compactCount)
	}
}

func TestMaybeCompactSkipsUnderTrigger(t *testing.T) {
	m := newTestModel()
	m.window = 1_000_000
	m.chat = buildBigChat(12)
	if m.maybeCompact() {
		t.Fatal("must not compact when usage is far under the trigger")
	}
}

func TestHeaderAndPanelUseCtxForecast(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.window = 131_072
	m.chat = []chatMsg{{role: roleUser, text: strings.Repeat("x", 400)}}
	m.refreshCtx()
	if m.ctxUsed != 104 {
		t.Fatalf("expected ctxUsed=104, got %d", m.ctxUsed)
	}
	// Header shows the calibrated forecast with the "~" label.
	hdr := m.renderHeader()
	if !strings.Contains(hdr, "~104") {
		t.Fatalf("header should show the ~ forecast, got:\n%s", hdr)
	}
	// Panel shows the forecast + percent.
	m.layout()
	m.width = 120
	m.layout()
	panel := m.renderPanel()
	if !strings.Contains(panel, "~104") || !strings.Contains(panel, "0.1%") {
		t.Fatalf("panel should show forecast + percent, got:\n%s", panel)
	}
	// Real settlement replaces the forecast when the API reports tokens.
	m.actualTokens = tokenUsage{input: 500, output: 300, total: 800}
	panel = m.renderPanel()
	if strings.Contains(panel, "~800") || !strings.Contains(panel, "800 / 131.1k") {
		t.Fatalf("panel should prefer settlement over forecast, got:\n%s", panel)
	}
}

func TestPanelCompactedBadge(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.window = 131_072
	m.compactCount = 2
	m.chat = []chatMsg{{role: roleUser, text: "halo"}}
	m.refreshCtx()
	m.layout()
	m.width = 120
	m.layout()
	panel := m.renderPanel()
	if !strings.Contains(panel, "2x compact") {
		t.Fatalf("panel should show the compaction badge, got:\n%s", panel)
	}
}

func TestResumeRecomputesCtx(t *testing.T) {
	// A saved session containing a compacted ledger must count toward the
	// window on -c resume — refreshCtx runs at load, not lazily.
	dir := t.TempDir()
	old := sessionPath
	sessionPath = func() (string, error) { return dir + "/latest.jsonl", nil }
	defer func() { sessionPath = old }()

	msgs := []chatMsg{
		{role: roleUser, text: "initial goal"},
		{role: roleSystem, text: "📋 context summary (L2 ledger)\ngoal: initial goal"},
		{role: roleUser, text: strings.Repeat("x", 400)},
	}
	if err := saveSessionTo(msgs, dir+"/latest.jsonl"); err != nil {
		t.Fatal(err)
	}

	m := New(nil, "v-test", "", true)
	if len(m.chat) != 3 {
		t.Fatalf("expected 3 resumed messages, got %d", len(m.chat))
	}
	if m.ctxUsed == 0 {
		t.Fatal("resume must recompute ctxUsed from the loaded transcript")
	}
	want := chatTokens(msgs)
	if m.ctxUsed != want {
		t.Fatalf("expected ctxUsed=%d after resume, got %d", want, m.ctxUsed)
	}
}

func TestConversationalMemory(t *testing.T) {
	ix := search.New(search.SampleCorpus())
	chat := []chatMsg{
		{role: roleUser, text: "how BM25 works"},
		{role: roleAgent, text: "BM25 scores relevance via TF-IDF.\n\n  ⚡ mock/pipeline · 0.4s"},
		{role: roleUser, text: "previous question?"}, // the current prompt
	}
	r := conversationalReply("previous question?", ix, chat)
	if !strings.Contains(r.text, "Session resumed") {
		t.Fatalf("follow-up should reference the session, got:\n%s", r.text)
	}
	if !strings.Contains(r.text, "how BM25 works") {
		t.Fatalf("follow-up should recall the prior user question, got:\n%s", r.text)
	}
	// The attribution footer must be stripped from the recalled answer.
	if strings.Contains(r.text, "⚡") {
		t.Fatalf("recalled answer must not leak the attribution footer:\n%s", r.text)
	}

	// A genuinely new prompt falls through to the normal search path.
	r2 := conversationalReply("mcp", ix, chat)
	if strings.Contains(r2.text, "Session resumed") {
		t.Fatalf("a fresh query must not be treated as a follow-up:\n%s", r2.text)
	}
}

func TestRecallSkipsCurrentPrompt(t *testing.T) {
	chat := []chatMsg{
		{role: roleUser, text: "pertanyaan lama"},
		{role: roleAgent, text: "jawaban lama"},
		{role: roleUser, text: "pertanyaan baru"},
	}
	u, a := recall(chat, "pertanyaan baru")
	if u != "pertanyaan lama" || a != "jawaban lama" {
		t.Fatalf("recall must skip the current prompt, got %q / %q", u, a)
	}
}

func TestCtxColorScale(t *testing.T) {
	// ctxColor takes a 0–100 percentage: muted under the 70% trigger, sand
	// between trigger and 80%, red past 80%. A regression for the off-by-100
	// bug where 0.8% usage rendered red (every real session would show red).
	m := newTestModel()
	sample := "used"
	// lipgloss.Style is not comparable — compare the rendered ANSI instead.
	// Red (Error) and sand (Accent) must differ, and 50% must NOT be red.
	red := m.ctxColor(90).Render(sample)
	sand := m.ctxColor(75).Render(sample)
	muted := m.ctxColor(50).Render(sample)
	if red == muted || sand == muted {
		t.Fatal("color bands must differ from muted")
	}
	if red == sand {
		t.Fatal("90% (red) must differ from 75% (sand)")
	}
	if m.ctxColor(0.5).Render(sample) == red {
		t.Fatal("0.5% must NOT render red (off-by-100 regression)")
	}
}

func TestLanguageOf(t *testing.T) {
	if got := languageOf("yang ini dari mana untuk apa"); got != "Indonesia" {
		t.Fatalf("expected Indonesia, got %s", got)
	}
	if got := languageOf("the and of this is that"); got != "English" {
		t.Fatalf("expected English, got %s", got)
	}
}

func TestConnectSetsProviderAndWindow(t *testing.T) {
	m := newTestModel()
	m.connectOpen = true
	m.connectSel = 0

	// Select opencode (index 0) — auto-detect provider, connects directly
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: '1', Text: "1"}))
	m2 := updated.(Model)
	updated, _ = m2.Update(enterKey())
	m3 := updated.(Model)
	if m3.provider != "opencode" {
		t.Fatalf("expected opencode provider, got %q", m3.provider)
	}
	if m3.connectOpen {
		t.Fatal("modal should close after enter")
	}
}

func TestHeaderShowsCtx(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.provider = "claude"
	m.window = 200_000
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: strings.Repeat("x", 400)}) // ~104 tokens
	m.refreshCtx()
	v := m.View().Content
	if !strings.Contains(v, "ctx ~104 / 200.0k") {
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

func TestZenMessagesHistoryAndPrompt(t *testing.T) {
	chat := []chatMsg{
		{role: roleUser, text: "old question"},
		{role: roleAgent, text: "old answer"},
		{role: roleUser, text: "current prompt (already appended by send)"},
	}
	msgs := zenMessages(chat, "current prompt (already appended by send)")
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (system + history + prompt), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("expected first message to be system directive, got %+v", msgs[0])
	}
	if msgs[1]["role"] != "user" || msgs[1]["content"] != "old question" {
		t.Fatalf("expected first user turn, got %+v", msgs[1])
	}
	if msgs[2]["role"] != "assistant" || msgs[2]["content"] != "old answer" {
		t.Fatalf("expected assistant turn, got %+v", msgs[2])
	}
	if msgs[3]["role"] != "user" || !strings.HasPrefix(msgs[3]["content"], "current prompt (already appended by send)") {
		t.Fatalf("expected final user prompt, got %+v", msgs[3])
	}
	// The last chat entry (the just-appended current prompt) must not be
	// duplicated as a prior turn.
	for _, msg := range msgs {
		if strings.HasPrefix(msg["content"], "current prompt") && msg["role"] == "assistant" {
			t.Fatalf("current prompt leaked into a prior turn: %+v", msgs)
		}
	}
}

func TestParseZenResponse(t *testing.T) {
	body := `{
		"choices":[{"message":{
			"content":"hello from zen",
			"reasoning_content":"step by step…"
		}}],
		"usage":{"prompt_tokens":88,"completion_tokens":20,"total_tokens":108}
	}`
	text, reasoning, tok, err := parseZenResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello from zen" {
		t.Fatalf("expected content, got %q", text)
	}
	if reasoning != "step by step…" {
		t.Fatalf("expected reasoning_content, got %q", reasoning)
	}
	if tok.input != 88 || tok.output != 20 || tok.total != 108 {
		t.Fatalf("usage not parsed: %+v", tok)
	}
}

func TestParseZenResponseMissingTotalTokens(t *testing.T) {
	body := `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`
	_, _, tok, err := parseZenResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if tok.total != 12 {
		t.Fatalf("expected total to fall back to input+output (12), got %d", tok.total)
	}
}

func TestZenChatReplyNativeHTTP(t *testing.T) {
	var gotModel string
	var gotMsgs []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string              `json:"model"`
			Messages []map[string]string `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		gotMsgs = body.Messages
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"native reply","reasoning_content":"think trace"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer srv.Close()

	m := newTestModel()
	m.provider = "opencode"
	// send() appends the current prompt as the LAST chat entry before the
	// agent starts — zenMessages must skip it and re-add it explicitly.
	m.chat = []chatMsg{
		{role: roleUser, text: "prior turn"},
		{role: roleUser, text: "current"},
	}
	cancel := func() {}
	traceCh := make(chan agentTraceMsg, 8)

	msg := zenChatReply(m, "current", "deepseek-v4-flash-free", srv.URL, traceCh, time.Now(), &cancel, 7)

	// The request must use the BARE model ID — the gateway rejects the
	// "opencode/" prefix on direct calls.
	if gotModel != "deepseek-v4-flash-free" {
		if res, ok := msg.(agentResultMsg); ok {
			t.Logf("zenChatReply error path: %q", res.reply.text)
		}
		t.Fatalf("model must be bare (no opencode/ prefix), got %q", gotModel)
	}
	if len(gotMsgs) != 3 || gotMsgs[1]["content"] != "prior turn" || !strings.HasPrefix(gotMsgs[2]["content"], "current") {
		t.Fatalf("unexpected messages sent: %+v", gotMsgs)
	}

	res, ok := msg.(agentResultMsg)
	if !ok {
		t.Fatalf("expected agentResultMsg, got %T", msg)
	}
	if res.run != 7 {
		t.Fatalf("result must carry the run id, got %d", res.run)
	}
	if !strings.Contains(res.reply.text, "native reply") {
		t.Fatalf("reply text missing content: %q", res.reply.text)
	}
	if res.reply.collapse == nil || !strings.Contains(res.reply.collapse.content, "think trace") {
		t.Fatalf("reasoning_content must become a collapsible thinking trace, got %+v", res.reply.collapse)
	}
	if res.tokens.total != 15 || res.tokens.input != 10 || res.tokens.output != 5 {
		t.Fatalf("tokens not parsed: %+v", res.tokens)
	}
}

func TestAgentResultAppliesRealTokens(t *testing.T) {
	m := newTestModel()
	m.agentRun = 3
	updated, _ := m.Update(agentResultMsg{
		reply:  mockReply{text: "hi"},
		tokens: tokenUsage{input: 10, output: 5, total: 15},
		run:    3,
	})
	m2 := updated.(Model)
	if m2.actualTokens.total != 15 || m2.actualTokens.input != 10 || m2.actualTokens.output != 5 {
		t.Fatalf("real tokens not applied to panel state: %+v", m2.actualTokens)
	}
}

func TestZenMessagesStripsAttributionFooter(t *testing.T) {
	chat := []chatMsg{
		{role: roleUser, text: "first"},
		{role: roleAgent, text: "the answer\n\n  ⚡ opencode/deepseek-v4-flash-free · 3.2s · 133 tokens"},
		{role: roleUser, text: "second"},
	}
	msgs := zenMessages(chat, "second")
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (system + first + answer + prompt), got %d: %+v", len(msgs), msgs)
	}
	if got := msgs[2]["content"]; got != "the answer" {
		t.Fatalf("attribution footer must not reach the model, got %q", got)
	}
}

func TestESCDuringAgentWorkLeavesPhaseChOpen(t *testing.T) {
	m := newTestModel()
	m.provider = "" // force the mock fallback — no network in tests
	m.input.SetValue("hello")
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if !m2.agentWorking {
		t.Fatal("agent should be working")
	}

	// ESC aborts the run but must NOT close traceCh itself — the agent
	// goroutine owns the single close; a second close would panic.
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m3 := updated.(Model)
	if m3.agentWorking {
		t.Fatal("expected interrupted")
	}
	if !m3.agentAborted {
		t.Fatal("expected aborted flag set")
	}
	if m3.traceCh == nil {
		t.Fatal("ESC must leave traceCh open — goroutine owns the close")
	}
}

func TestAbortedAgentResultDropped(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true
	m.agentAborted = true
	m.agentRun = 1
	updated, _ := m.Update(agentResultMsg{reply: mockReply{text: "late reply"}, run: 1})
	m2 := updated.(Model)
	if len(m2.chat) != 0 {
		t.Fatalf("interrupted run's late reply must be dropped, got %+v", m2.chat)
	}
	if m2.agentAborted {
		t.Fatal("aborted flag must reset after dropping the stale result")
	}
}

func TestStaleAgentResultDropped(t *testing.T) {
	m := newTestModel()
	m.agentRun = 2
	updated, _ := m.Update(agentResultMsg{reply: mockReply{text: "stale"}, run: 1})
	m2 := updated.(Model)
	if len(m2.chat) != 0 {
		t.Fatalf("superseded run's result must be dropped, got %+v", m2.chat)
	}
	if m2.agentWorking {
		t.Fatal("dropping a stale result must not flip agent state")
	}
}

func TestAgentTraceAppendsToProcessLog(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true
	updated, _ := m.Update(agentTraceMsg{phase: "reading files…", line: "→ read internal/tui/app.go"})
	m2 := updated.(Model)
	if m2.agentPhase != "reading files…" {
		t.Fatalf("expected phase updated, got %q", m2.agentPhase)
	}
	if m2.agentStep != 1 {
		t.Fatalf("expected step 1, got %d", m2.agentStep)
	}
	if len(m2.trace) != 1 || m2.trace[0] != "→ read internal/tui/app.go" {
		t.Fatalf("trace line not appended: %+v", m2.trace)
	}

	// A line-only update (empty phase) must not bump the step counter.
	updated, _ = m2.Update(agentTraceMsg{line: "✱ glob **/*.go"})
	m3 := updated.(Model)
	if m3.agentStep != 1 {
		t.Fatalf("line-only update must not bump step, got %d", m3.agentStep)
	}
	if len(m3.trace) != 2 {
		t.Fatalf("expected 2 trace lines, got %+v", m3.trace)
	}
}

func TestTraceBounded(t *testing.T) {
	trace := []string{}
	for i := 0; i < maxTrace+10; i++ {
		trace = appendTrace(trace, fmt.Sprintf("line %d", i))
	}
	if len(trace) != maxTrace {
		t.Fatalf("trace not bounded: %d > %d", len(trace), maxTrace)
	}
	// The oldest entries were dropped — the newest remain.
	if trace[0] != "line 10" || trace[len(trace)-1] != fmt.Sprintf("line %d", maxTrace+9) {
		t.Fatalf("trace bounds wrong: first=%q last=%q", trace[0], trace[len(trace)-1])
	}
}

func TestAgentQuestionOpensAskUI(t *testing.T) {
	m := newTestModel()
	m.started = true // the question box renders in the chat body, not the landing
	m.agentWorking = true
	updated, _ := m.Update(agentQuestionMsg{prompt: "lanjut?", options: []string{"A", "B"}})
	m2 := updated.(Model)
	if !m2.askOpen {
		t.Fatal("expected askOpen")
	}
	if m2.askPrompt != "lanjut?" || len(m2.askOptions) != 2 || m2.askSel != 0 {
		t.Fatalf("question state wrong: %+v", m2)
	}
	// The question renders inline in the chat area.
	v := m2.View().Content
	if !strings.Contains(v, "agent question") || !strings.Contains(v, "lanjut?") {
		t.Fatalf("question box missing from view:\n%s", v)
	}
}

func TestStaleQuestionFromInterruptedRunDropped(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true // a NEW run is in progress
	m.agentRun = 2
	// A question left in the buffer by the interrupted run 1 must not open
	// the ask UI during run 2.
	updated, _ := m.Update(agentQuestionMsg{prompt: "phantom?", options: []string{"A"}, run: 1})
	m2 := updated.(Model)
	if m2.askOpen {
		t.Fatal("stale question must not open the ask UI")
	}
}

func TestAnswerQuestionSubmitsSelectedOption(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true
	m.askOpen = true
	m.askPrompt = "continue?"
	m.askOptions = []string{"Show summary", "Run tests"}
	m.askSel = 1
	m.answerCh = make(chan string, 1)

	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if m2.askOpen {
		t.Fatal("expected question closed after enter")
	}
	select {
	case ans := <-m.answerCh:
		if ans != "Run tests" {
			t.Fatalf("expected selected option as answer, got %q", ans)
		}
	default:
		t.Fatal("answer not sent to the agent")
	}
}

func TestAnswerQuestionCustomText(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true
	m.askOpen = true
	m.askOptions = []string{"A", "B"}
	m.answerCh = make(chan string, 1)
	m.input.SetValue("jawaban bebas saya")

	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if m2.askOpen {
		t.Fatal("expected question closed after enter")
	}
	select {
	case ans := <-m.answerCh:
		if ans != "jawaban bebas saya" {
			t.Fatalf("expected custom typed answer, got %q", ans)
		}
	default:
		t.Fatal("custom answer not sent")
	}
}

func TestQuestionEscCancelsAndSignalsAbort(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true
	m.askOpen = true
	m.askOptions = []string{"A", "B"}
	m.answerCh = make(chan string, 1)

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m2 := updated.(Model)
	if m2.askOpen {
		t.Fatal("expected question closed on esc")
	}
	if !m2.agentAborted {
		t.Fatal("expected run marked aborted")
	}
	if m2.agentWorking {
		t.Fatal("expected agent not working after cancel")
	}
	select {
	case ans := <-m.answerCh:
		if ans != "" {
			t.Fatalf("expected empty abort signal, got %q", ans)
		}
	default:
		t.Fatal("abort signal not sent to the agent")
	}
}

func TestQuestionArrowsNavigateOptions(t *testing.T) {
	m := newTestModel()
	m.askOpen = true
	m.askOptions = []string{"A", "B", "C"}

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m2 := updated.(Model)
	if m2.askSel != 1 {
		t.Fatalf("expected selection 1 after down, got %d", m2.askSel)
	}
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m3 := updated.(Model)
	if m3.askSel != 2 {
		t.Fatalf("expected selection 2, got %d", m3.askSel)
	}
	updated, _ = m3.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m4 := updated.(Model)
	if m4.askSel != 1 {
		t.Fatalf("expected selection 1 after up, got %d", m4.askSel)
	}
}

func TestMockAgentRunAsksAndContinues(t *testing.T) {
	m := newTestModel()
	m.provider = "" // mock fallback — no network
	traceCh := make(chan agentTraceMsg, 64)
	askCh := make(chan agentQuestionMsg, 1)
	answerCh := make(chan string, 1)
	cancel := func() {}
	cmd := m.agentWorkCmd("hello", traceCh, askCh, answerCh, &cancel, 9)

	resultCh := make(chan tea.Msg, 1)
	go func() { resultCh <- cmd() }()

	// The scripted run pauses with a question for the user.
	q, ok := <-askCh
	if !ok {
		t.Fatal("expected a question from the mock agent")
	}
	if len(q.options) == 0 {
		t.Fatal("expected options in the question")
	}

	// Trace lines were streamed before the question.
	if len(traceCh) == 0 {
		t.Fatal("expected trace lines streamed during the run")
	}

	answerCh <- q.options[0]
	msg := <-resultCh
	res, ok := msg.(agentResultMsg)
	if !ok {
		t.Fatalf("expected agentResultMsg, got %T", msg)
	}
	if res.run != 9 {
		t.Fatalf("expected run id 9, got %d", res.run)
	}
	if !strings.Contains(res.reply.text, q.options[0]) {
		t.Fatalf("final reply must reference the chosen option, got:\n%s", res.reply.text)
	}
}

func TestMockAgentRunAbortedOnCancel(t *testing.T) {
	m := newTestModel()
	m.provider = ""
	traceCh := make(chan agentTraceMsg, 64)
	askCh := make(chan agentQuestionMsg, 1)
	answerCh := make(chan string, 1)
	cancel := func() {}
	cmd := m.agentWorkCmd("hello", traceCh, askCh, answerCh, &cancel, 5)

	resultCh := make(chan tea.Msg, 1)
	go func() { resultCh <- cmd() }()

	q, ok := <-askCh
	if !ok || len(q.options) == 0 {
		t.Fatalf("expected a question first, got %+v", q)
	}

	answerCh <- "" // user cancelled with esc
	msg := <-resultCh
	res, ok := msg.(agentResultMsg)
	if !ok {
		t.Fatalf("expected agentResultMsg, got %T", msg)
	}
	if !strings.Contains(res.reply.text, "interrupted") {
		t.Fatalf("expected interrupted reply, got:\n%s", res.reply.text)
	}
}

// zenModelsPayload is the exact response shape of GET /zen/v1/models.
func zenModelsPayload(ids ...string) string {
	data := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]interface{}{"id": id, "object": "model"})
	}
	b, _ := json.Marshal(map[string]interface{}{"object": "list", "data": data})
	return string(b)
}

func TestFetchZenModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Paid + free mixed, unsorted, with a duplicate — exactly what the
		// real gateway returns.
		fmt.Fprint(w, zenModelsPayload(
			"claude-fable-5", // paid — must be filtered out
			"big-pickle",     // free, no "-free" suffix
			"deepseek-v4-flash-free",
			"mimo-v2.5-free",
			"big-pickle", // duplicate — deduped
			"ling-3.0-tiny-free",
			"opencode-fancy-pro", // paid — filtered
		))
	}))
	defer srv.Close()

	models, err := fetchZenModels(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"big-pickle", "deepseek-v4-flash-free", "ling-3.0-tiny-free", "mimo-v2.5-free"}
	if fmt.Sprintf("%v", models) != fmt.Sprintf("%v", want) {
		t.Fatalf("expected sorted+deduped free models %v, got %v", want, models)
	}
}

func TestFetchZenModelsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := fetchZenModels(srv.URL); err == nil {
		t.Fatal("expected error on HTTP 500")
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer srv2.Close()
	if _, err := fetchZenModels(srv2.URL); err == nil {
		t.Fatal("expected error on invalid JSON")
	}

	// A successful fetch with ZERO free models (gateway naming change) must
	// be an error — otherwise the picker would show a lying "0 live" state
	// while silently listing static models.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, zenModelsPayload("claude-fable-5", "opencode-fancy-pro")) // paid only
	}))
	defer srv3.Close()
	if _, err := fetchZenModels(srv3.URL); err == nil {
		t.Fatal("expected error when the gateway returns no free models")
	}

	// A model with "free" INSIDE its id (not a -free suffix) is paid and
	// must be filtered out — the suffix check guards against leaks.
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, zenModelsPayload("opencode-freeloader-pro", "big-pickle"))
	}))
	defer srv4.Close()
	models, err := fetchZenModels(srv4.URL)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%v", models) != "[big-pickle]" {
		t.Fatalf("expected only big-pickle, got %v", models)
	}
}

func TestModelsOpenKicksFetchWhenStale(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/models")
	// Fresh cache (zero time is stale) — first open must kick a fetch.
	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if !m2.modelsOpen {
		t.Fatal("expected /models modal open")
	}
	if !m2.zenModelsLoading {
		t.Fatal("expected zenModelsLoading on stale cache open")
	}
	if cmd == nil {
		t.Fatal("expected a fetch cmd on stale cache open")
	}

	// A freshly fetched cache skips the network call.
	m3 := newTestModel()
	m3.zenModels = []string{"deepseek-v4-flash-free"}
	m3.zenModelsFetched = time.Now()
	m3.input.SetValue("/models")
	updated2, cmd2 := m3.Update(enterKey())
	m4 := updated2.(Model)
	if m4.zenModelsLoading {
		t.Fatal("fresh cache must not refetch")
	}
	if cmd2 != nil {
		t.Fatal("fresh cache must not return a fetch cmd")
	}
}

func TestZenModelsMsgUpdatesPicker(t *testing.T) {
	m := newTestModel()
	m.zenModelsLoading = true
	m.modelsOpen = true
	updated, _ := m.Update(zenModelsMsg{models: []string{"big-pickle", "mimo-v2.5-free"}})
	m2 := updated.(Model)
	if m2.zenModelsLoading {
		t.Fatal("zenModelsMsg must clear the loading flag")
	}
	if len(m2.zenModels) != 2 {
		t.Fatalf("expected 2 cached models, got %d", len(m2.zenModels))
	}
	if m2.zenModelsFetched.IsZero() {
		t.Fatal("zenModelsMsg must stamp the fetch time")
	}
	// Picker now lists the live models under the opencode provider.
	r := m2.allModelEntries()
	found := false
	for _, e := range r {
		if e.provider == "opencode" && e.model == "big-pickle" {
			found = true
		}
	}
	if !found {
		t.Fatalf("live models missing from picker entries: %+v", r)
	}

	// Error message clears loading and keeps the static fallback usable.
	m3 := newTestModel()
	m3.zenModelsLoading = true
	m3.modelsOpen = true
	updated3, _ := m3.Update(zenModelsErrMsg{err: fmt.Errorf("network down")})
	m4 := updated3.(Model)
	if m4.zenModelsLoading {
		t.Fatal("zenModelsErrMsg must clear the loading flag")
	}
	if len(m4.allModelEntries()) == 0 {
		t.Fatal("picker must still list static models after a failed fetch")
	}
}

func TestModelsModalLiveSourceNote(t *testing.T) {
	m := newTestModel()
	m.modelsOpen = true

	// Loading state shows the spinner note.
	m.zenModelsLoading = true
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "fetching live free models") {
		t.Fatalf("expected loading note in modal:\n%s", v)
	}

	// Live state shows the count note.
	m.zenModelsLoading = false
	m.zenModels = []string{"big-pickle", "mimo-v2.5-free"}
	v = ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "live: 2 free models") {
		t.Fatalf("expected live note in modal:\n%s", v)
	}

	// Fallback state (never fetched) is transparent about the static list.
	m.zenModels = nil
	m.zenModelsFetched = time.Time{}
	v = ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "static list") {
		t.Fatalf("expected static fallback note in modal:\n%s", v)
	}
}

func ctrlEKey() tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: 'e', Mod: tea.ModCtrl})
}

func TestLongPromptCollapsesToBadge(t *testing.T) {
	m := newTestModel()
	long := strings.Repeat("x", 250)
	m.input.SetValue(long)
	if !m.longPromptOpen() {
		t.Fatal("expected longPromptOpen for >200 chars")
	}
	v := ansiStrip.ReplaceAllString(m.renderInput(), "")
	if !strings.Contains(v, "[Long prompt:") {
		t.Fatalf("expected long-prompt badge in input, got:\n%s", v)
	}
	if strings.Contains(v, strings.Repeat("x", 50)) {
		t.Fatalf("full prompt text must be hidden behind the badge, got:\n%s", v)
	}
	// The full text must still be in the input — only the display collapses.
	if m.input.Value() != long {
		t.Fatalf("input value must stay intact, got %d chars", len(m.input.Value()))
	}
	// Short prompts never collapse.
	m.input.SetValue("short")
	if m.longPromptOpen() {
		t.Fatal("short prompt must not collapse")
	}
}

func TestCtrlETogglesPromptEditModal(t *testing.T) {
	m := newTestModel()
	m.input.SetValue(strings.Repeat("y", 250))

	// ctrl+e opens the preview/edit modal.
	updated, _ := m.Update(ctrlEKey())
	m2 := updated.(Model)
	if !m2.promptEditOpen {
		t.Fatal("expected prompt edit modal open after ctrl+e")
	}
	v := m2.View().Content
	if !strings.Contains(v, "prompt preview") {
		t.Fatalf("expected preview modal in view, got:\n%s", v)
	}

	// esc closes it; the input is untouched.
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m3 := updated.(Model)
	if m3.promptEditOpen {
		t.Fatal("expected modal closed on esc")
	}
	if m3.input.Value() != m2.input.Value() {
		t.Fatal("esc must not alter the input")
	}
}

func TestCtrlEIgnoredForShortPrompt(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("tiny")
	updated, _ := m.Update(ctrlEKey())
	m2 := updated.(Model)
	if m2.promptEditOpen {
		t.Fatal("short prompts must not open the edit modal")
	}
	if !strings.Contains(m2.status, "only for long prompts") {
		t.Fatalf("expected a status hint, got %q", m2.status)
	}
}

func TestPromptEditModalEnterSends(t *testing.T) {
	m := newTestModel()
	m.input.SetValue(strings.Repeat("z", 250))
	m.promptEditOpen = true

	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if cmd == nil {
		t.Fatal("expected send command after enter in the edit modal")
	}
	if m2.promptEditOpen {
		t.Fatal("modal must close when the prompt is sent")
	}
	if len(m2.chat) != 1 || m2.chat[0].text != strings.Repeat("z", 250) {
		t.Fatalf("full long prompt must be sent unchanged, got %+v", m2.chat)
	}
}

func mouseClick(x, y int) tea.MouseClickMsg {
	return tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: tea.MouseLeft})
}

func TestDragSelectHighlightsAndCopies(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "hello drag select world"})
	m.refreshChat()

	// The chat message sits below the brand block (content line 8 in this
	// layout). Map that content line back to a terminal row: the viewport
	// starts at headerHeight, so row = headerHeight + (line - YOffset).
	line := m.viewport.YOffset()
	for i, ln := range strings.Split(m.viewport.GetContent(), "\n") {
		if strings.Contains(ln, "hello drag select world") {
			line = i
			break
		}
	}
	row := headerHeight + (line - m.viewport.YOffset())

	// Press the left button on the message row, column 2.
	updated, _ := m.Update(mouseClick(2, row))
	m2 := updated.(Model)
	if !m2.dragSel.active {
		t.Fatal("expected drag-select to start on left press inside the viewport")
	}

	// Drag to column 20 on the same row.
	updated, _ = m2.Update(tea.MouseMotionMsg(tea.Mouse{X: 20, Y: row}))
	m3 := updated.(Model)
	if !m3.dragSel.active {
		t.Fatal("drag must stay active during motion")
	}
	if m3.dragSel.x1 != 20 {
		t.Fatalf("expected drag point x=20, got %d", m3.dragSel.x1)
	}

	// The highlight must render in the VIEWPORT CONTENT while dragging (the
	// input cursor below also uses reverse video, so assert on the viewport).
	if !strings.Contains(m3.viewport.GetContent(), "\x1b[7m") {
		t.Fatalf("expected reverse-video highlight while dragging:\n%s", m3.viewport.GetContent())
	}

	// Release copies the selected rectangle and clears the highlight.
	updated, _ = m3.Update(tea.MouseReleaseMsg(tea.Mouse{X: 20, Y: row}))
	m4 := updated.(Model)
	if m4.dragSel.active {
		t.Fatal("drag must end on release")
	}
	if !strings.Contains(m4.status, "copied selection") {
		t.Fatalf("expected copied status, got %q", m4.status)
	}
	if strings.Contains(m4.viewport.GetContent(), "\x1b[7m") {
		t.Fatal("highlight must clear after release")
	}
}

func TestPlainClickCopiesNothing(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "just a click"})
	m.refreshChat()

	// Find a real CHARACTER to click on (not a space) — a plain click on any
	// cell, even text, must never copy a single char.
	line, col := m.viewport.YOffset(), 0
	for i, ln := range strings.Split(m.viewport.GetContent(), "\n") {
		for j, r := range ln {
			if r != ' ' && r != '\t' && r != '▌' {
				line, col = i, j
				break
			}
		}
		if col > 0 {
			break
		}
	}
	row := headerHeight + (line - m.viewport.YOffset())

	// Press + release at the same point = zero motion → no copy.
	updated, _ := m.Update(mouseClick(col, row))
	m2 := updated.(Model)
	updated, _ = m2.Update(tea.MouseReleaseMsg(tea.Mouse{X: col, Y: row}))
	m3 := updated.(Model)
	if m3.dragSel.active {
		t.Fatal("drag must be inactive after release")
	}
	if strings.Contains(m3.status, "copied") {
		t.Fatalf("a plain click on text must not copy a single char, got status %q", m3.status)
	}
	// The status must be untouched by the no-op release.
	if m3.status != m2.status {
		t.Fatalf("status must be preserved by a plain click, got %q → %q", m2.status, m3.status)
	}
}

func TestDragSelectIgnoredWhileModalOpen(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.modelsOpen = true
	updated, _ := m.Update(mouseClick(5, headerHeight))
	m2 := updated.(Model)
	if m2.dragSel.active {
		t.Fatal("drag-select must not start while a modal is open")
	}
}

func TestDragSelectOutsideViewportIgnored(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "content"})
	m.refreshChat()

	// Press in the header row (y=0) — not the viewport.
	updated, _ := m.Update(mouseClick(5, 0))
	m2 := updated.(Model)
	if m2.dragSel.active {
		t.Fatal("press on the header must not start a drag")
	}
}

func TestSelectedTextExtractsLinear(t *testing.T) {
	m := newTestModel()
	m.dragSel = dragSelection{active: true, x0: 0, y0: 0, x1: 4, y1: 1}
	m.viewport.SetContent("line one\nline two\nline three")
	got := m.selectedText()
	if got != "line one\nline" {
		t.Fatalf("expected linear selection, got %q", got)
	}

	// Multi-column drag on one row picks only the dragged span.
	m.dragSel = dragSelection{active: true, x0: 5, y0: 2, x1: 7, y1: 2}
	got = m.selectedText()
	if got != "thr" {
		t.Fatalf("expected 'thr' from columns 5-7 of line 3, got %q", got)
	}
}

func TestHighlightRangePreservesANSI(t *testing.T) {
	// The reverse codes must wrap the selected columns without clobbering the
	// surrounding style codes (color survives via reverse-off, not a reset).
	styled := "\x1b[32mhello world\x1b[0m"
	got := highlightRange(styled, 6, 10)
	if !strings.Contains(got, "\x1b[7m") {
		t.Fatalf("expected reverse-on inside the line, got %q", got)
	}
	plain := ansiStrip.ReplaceAllString(got, "")
	if plain != "hello world" {
		t.Fatalf("visible text must survive, got %q", plain)
	}
	// The reverse-off (27m) must land BEFORE the original trailing reset — the
	// reset belongs to the tail, never inside the highlighted span.
	revOff := strings.Index(got, "\x1b[27m")
	reset := strings.Index(got, "\x1b[0m")
	if revOff < 0 {
		t.Fatalf("expected reverse-off, got %q", got)
	}
	if reset >= 0 && revOff > reset {
		t.Fatalf("reverse-off must precede the trailing reset, got %q", got)
	}
	// Out-of-range selection returns the line unchanged.
	if out := highlightRange("short", 10, 20); out != "short" {
		t.Fatalf("out-of-range selection must not change the line, got %q", out)
	}
}
