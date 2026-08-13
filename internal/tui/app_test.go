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

	"github.com/plumpslabs/bro-code/internal/agentic"
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

func TestSendShowsProviderModelRowImmediately(t *testing.T) {
	// The dim "→ provider · model" row under the user prompt must be in the
	// trace SYNCHRONOUSLY at send time (not only after the async agent
	// goroutine emits it) — the goroutine's phase line is delayed by the
	// context-enrichment walk, so without this the spinner alone would sit
	// under the prompt.
	m := newTestModel()
	m.provider = "opencode"
	m.selectedModel = "deepseek-v4-flash-free"
	m.input.SetValue("hello")
	m2, cmd := m.send()
	if cmd == nil {
		t.Fatal("expected a command after send")
	}
	if !m2.agentWorking {
		t.Fatal("expected agentWorking after send")
	}
	if len(m2.trace) == 0 || !strings.Contains(m2.trace[0], "opencode · deepseek-v4-flash-free") {
		t.Fatalf("expected provider/model row as first trace line at send time, got %v", m2.trace)
	}
	// The row must be visible in the rendered view.
	v := ansiStrip.ReplaceAllString(m2.View().Content, "")
	if !strings.Contains(v, "→ opencode · deepseek-v4-flash-free") {
		t.Fatalf("provider/model row not visible in view:\n%s", v)
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

func TestStreamingRenderIncrementalMatchesFull(t *testing.T) {
	// The incremental streaming renderer must produce byte-identical output to
	// a fresh full render at EVERY growth point — if they ever diverge, a
	// streamed reply would visibly change when re-rendered after completion.
	m := newTestModel()
	m.width = 100
	m.layout()
	w := m.chatContentWidth()

	// Realistic long reply: prose, numbered list, Go code block, bullets.
	full := strings.Join([]string{
		"Ini jawaban panjang untuk audit proyek.",
		"",
		"## Langkah-langkah",
		"1. Membuat folder docs",
		"2. Audit struktur",
		"",
		"```go",
		"func main() {",
		"\tfmt.Println(\"hello brocode\")",
		"}",
		"```",
		"",
		"- Selesai dengan kode di atas.",
		"",
		"```bash",
		"mkdir docs",
		"```",
	}, "\n")

	m.streamCache = streamCache{}
	div := m.styles.sys.Render(strings.Repeat("·", min(w, 60)))
	// Two inputs: the reply above, and the same reply with a trailing '\n' —
	// the phantom empty split element (the exact bug fixed: one missing
	// newline before the divider) must stay pinned by a regression test.
	for _, input := range []string{full, full + "\n"} {
		var sb strings.Builder
		for i := 1; i <= len(input); i++ {
			sb.WriteByte(input[i-1])
			if i%13 == 0 || i == len(input) { // checkpoint — a "stream tick"
				inc := m.renderStreamingAgent(sb.String(), w)
				want := m.renderPlain(sb.String(), w) + "\n" + div + "\n"
				if inc != want {
					t.Fatalf("streaming render diverged at %d/%d chars\n--- incremental ---\n%q\n--- full render ---\n%q", i, len(input), inc, want)
				}
			}
		}
		m.streamCache = streamCache{} // fresh cache per input
	}
}

func TestStreamingCachePopulatedOnHotPath(t *testing.T) {
	// refreshChat must feed the incremental cache while a (non-collapsible)
	// agent reply is streaming — this is the hot path the lag fix targets.
	m := newTestModel()
	m.started = true
	m.streaming = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "hello"})
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "partial line"})
	m.refreshChat()
	if m.streamCache.text == "" && m.streamCache.partialV == "" {
		t.Fatal("incremental cache must be populated by refreshChat during streaming")
	}
}

func TestStreamCacheResetBetweenReplies(t *testing.T) {
	// A fresh agentResultMsg must reset the incremental stream cache — a stale
	// cache would prepend the previous reply's rendered lines to the new one.
	m := newTestModel()
	m.started = true

	reply := buildReply("/diff", m.index)
	updated, _ := m.Update(agentResultMsg{reply: reply})
	m2 := updated.(Model)
	// /diff replies are collapsed collapsible → the incremental path is
	// skipped by design (summary only); the cache must be empty regardless.
	if m2.streamCache.text != "" || m2.streamCache.partialV != "" {
		t.Fatalf("cache must be empty at the start of a new reply, got %+v", m2.streamCache)
	}
	// Drain to completion, then a second reply must reset any stale state.
	for i := 0; i < 500 && m2.streaming; i++ {
		next, _ := m2.Update(streamTickMsg{})
		m2 = next.(Model)
	}
	if m2.streaming {
		t.Fatal("stream never completed within 500 ticks")
	}
	updated, _ = m2.Update(agentResultMsg{reply: reply})
	m3 := updated.(Model)
	if m3.streamCache.text != "" || m3.streamCache.partialV != "" {
		t.Fatalf("stream cache must reset when a new reply starts, got %+v", m3.streamCache)
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

// seedModelsCache writes a fresh (in-TTL) model cache into the test HOME so
// DiscoverAllModels returns deterministic lists without network access.
func seedModelsCache(t *testing.T, home string, models map[string][]string) {
	t.Helper()
	dir := filepath.Join(home, ".brocode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(modelCache{Models: models, FetchedAt: time.Now()})
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeTestConfig writes a config.jsonc into the test HOME (the settings
// surface for custom providers).
func writeTestConfig(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".brocode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.jsonc"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestModelsModalShowsOnlyActiveProviders(t *testing.T) {
	// Isolate from the host machine's keys/credentials: the cache even lists
	// groq models, but with no key that provider is inactive — its models
	// must NOT show in the picker. Only opencode (free gateway) is active,
	// so only its models may appear.
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedModelsCache(t, home, map[string][]string{
		"opencode": {"big-pickle", "deepseek-v4-flash-free"},
		"groq":     {"llama-3.3-70b-versatile"},
	})

	m := newTestModel()
	m.modelsOpen = true
	m.modelsSel = 0
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "big-pickle") {
		t.Fatalf("models modal missing active provider models\n---\n%s", v)
	}
	// Phantom models for providers with no key / no credentials must not appear.
	for _, no := range []string{"llama-3.3-70b-versatile", "claude-sonnet-4-20250514", "deepseek-chat", "MiniMax-M3", "mimo-v2.5"} {
		if strings.Contains(v, no) {
			t.Fatalf("models modal leaked %q for an unconfigured provider\n---\n%s", no, v)
		}
	}
}

func TestModelsModalCustomProviderFromSettings(t *testing.T) {
	// A custom provider is only usable when it exists in settings
	// (config.jsonc). Its models come from settings — never from a hardcoded
	// list, and the legacy "custom" row must not duplicate it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedModelsCache(t, home, map[string][]string{"opencode": {"big-pickle"}})
	writeTestConfig(t, home, `{
  "provider": {
    "mygpu": {
      "name": "My GPU",
      "options": {"baseURL": "http://127.0.0.1:11434/v1"},
      "models": {
        "llama-3-8b": {"name": "Llama 3 8B", "limit": {"context": 128000}}
      }
    }
  }
}`)

	m := newTestModel()
	m.modelsOpen = true
	m.modelsSel = 0
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "llama-3-8b") || !strings.Contains(v, "mygpu") {
		t.Fatalf("expected settings-defined custom model in picker\n---\n%s", v)
	}
	// The legacy hardcoded custom model names must never appear.
	for _, no := range []string{"llama-3-70b", "deepseek-coder"} {
		if strings.Contains(v, no) {
			t.Fatalf("legacy hardcoded custom model %q leaked into picker\n---\n%s", no, v)
		}
	}
	// No duplicate rows: exactly one config-defined provider (mygpu) and one
	// legacy custom entry (still connectable, just model-less).
	counts := map[string]int{}
	for _, p := range GetProviders() {
		counts[p.name]++
	}
	if counts["mygpu"] != 1 || counts["custom"] != 1 {
		t.Fatalf("expected one mygpu and one custom row, got %v", counts)
	}
}

func TestProviderActiveGatesOnCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if providerActive("groq") {
		t.Fatal("groq must be inactive with no key configured")
	}
	if !providerActive("opencode") {
		t.Fatal("opencode is always active (free gateway)")
	}
	if providerActive("custom") {
		t.Fatal("legacy custom must be inactive when not defined in settings")
	}

	writeTestConfig(t, home, `{"provider":{"mygpu":{"name":"My GPU","options":{"baseURL":"http://x"}}}}`)
	if !providerActive("mygpu") {
		t.Fatal("config-defined custom provider must be active")
	}
}

func TestGetProvidersSkipsCollidingConfigIDs(t *testing.T) {
	// A settings entry named like a built-in provider (e.g. "groq") must not
	// produce a duplicate row in the /connect list.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestConfig(t, home, `{"provider":{"groq":{"name":"My Groq","options":{"baseURL":"http://x"}}}}`)
	counts := map[string]int{}
	for _, p := range GetProviders() {
		counts[p.name]++
	}
	if counts["groq"] != 1 {
		t.Fatalf("expected exactly one groq row despite colliding config id, got %d", counts["groq"])
	}
}

func TestModelsModalSearchFiltersAcrossProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedModelsCache(t, home, map[string][]string{
		"opencode": {"big-pickle", "deepseek-v4-flash-free"},
		"groq":     {"llama-3.3-70b-versatile"},
	})
	m := newTestModel()
	m.modelsOpen = true
	m.modelsSel = 0

	// Type "big" → only the matching opencode model remains. The groq
	// model is gone entirely — that provider is unconfigured. (The search
	// term deliberately avoids the k/j/q navigation keys.)
	for _, ch := range "big" {
		updated, _ := m.Update(runeKey(ch))
		m = updated.(Model)
	}
	if m.modelsQuery != "big" {
		t.Fatalf("expected query 'big', got %q", m.modelsQuery)
	}
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "big-pickle") {
		t.Fatalf("expected big-pickle in filtered view\n---\n%s", v)
	}
	if strings.Contains(v, "deepseek-v4-flash-free") {
		t.Fatalf("non-matching model leaked into filtered view\n---\n%s", v)
	}
	if strings.Contains(v, "llama-3.3-70b-versatile") {
		t.Fatalf("unconfigured provider model leaked into filtered view\n---\n%s", v)
	}

	// Enter applies the first match and switches provider to opencode.
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if m2.modelsOpen {
		t.Fatal("modal should close after enter")
	}
	if m2.selectedModel != "big-pickle" {
		t.Fatalf("expected big-pickle, got %q", m2.selectedModel)
	}
	if m2.provider != "opencode" {
		t.Fatalf("expected provider opencode (switched by selection), got %q", m2.provider)
	}
}

func TestModelsModalSearchByProviderName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedModelsCache(t, home, map[string][]string{
		"opencode": {"big-pickle", "deepseek-v4-flash-free"},
		"groq":     {"llama-3.3-70b-versatile"},
	})
	m := newTestModel()
	m.modelsOpen = true

	// Search by provider name — "opencode" matches all its models.
	for _, ch := range "opencode" {
		updated, _ := m.Update(runeKey(ch))
		m = updated.(Model)
	}
	v := ansiStrip.ReplaceAllString(m.renderModelsModalBox(), "")
	if !strings.Contains(v, "big-pickle") {
		t.Fatalf("expected opencode models when searching by provider\n---\n%s", v)
	}
	if strings.Contains(v, "groq") || strings.Contains(v, "llama-3.3-70b-versatile") {
		t.Fatalf("unconfigured provider leaked into the provider-name filter\n---\n%s", v)
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGY_API_KEY", "test-key") // makes antigravity active (detected)
	seedModelsCache(t, home, map[string][]string{
		"antigravity": {"gemini-2.5-flash", "gemini-2.5-pro", "gemini-2.0-flash"},
	})
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
	// Press down len(GetProviders()) more times — wraps around (circular navigation).
	for i := 0; i < len(GetProviders()); i++ {
		updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		m2 = updated.(Model)
	}
	// len(GetProviders()) providers, position 1 + len downs = wraps to 1.
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
	if m4.connectSel != len(GetProviders())-1 {
		t.Fatalf("expected circular wrap to last provider %d, got %d", len(GetProviders())-1, m4.connectSel)
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedModelsCache(t, home, map[string][]string{
		"opencode": {"big-pickle", "deepseek-v4-flash-free"},
	})
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

func TestApplySearchReplaceBlocks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "target.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc Old() {\n\tprintln(\"old\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srPayload := fmt.Sprintf("<<<<<<< SEARCH: %s\nfunc Old() {\n\tprintln(\"old\")\n}\n=======\nfunc New() {\n\tprintln(\"new\")\n}\n>>>>>>> REPLACE", file)
	logs, edits := applyBuilderCodeBlocks(srPayload, "", false)
	if len(logs) == 0 || len(edits) == 0 {
		t.Fatalf("expected edit logs and edits, got logs: %v, edits: %v", logs, edits)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "func New()") || strings.Contains(string(data), "func Old()") {
		t.Fatalf("search replace patch failed to apply, content:\n%s", string(data))
	}
}

func TestOpenCodeAutoDetection(t *testing.T) {
	// Isolate HOME so New() never reads the host machine's saved provider
	// (LastProvider in ~/.brocode/config.json) or credentials — that made
	// this test environment-dependent (it failed whenever the last saved
	// provider differed from the auto-detected opencode).
	home := t.TempDir()
	t.Setenv("HOME", home)

	detected, model := DetectOpenCode()
	m := newTestModel()
	if detected {
		if model == "" {
			t.Fatal("expected non-empty free model name when OpenCode detected")
		}
		if m.provider != "opencode" {
			t.Fatalf("expected opencode auto-selected on startup, got %q", m.provider)
		}
		if m.selectedModel == "" {
			t.Fatal("expected a model selected when opencode is detected")
		}
	} else {
		// No binary in PATH and no credentials in the isolated HOME — nothing
		// may be auto-selected, and the host config must not leak in.
		if m.provider != "" {
			t.Fatalf("expected no provider auto-selected in isolated HOME, got %q", m.provider)
		}
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

func TestSearchProjectFilesSkipsOversizeFiles(t *testing.T) {
	// Bounded-work guard: bundle-sized files must never be read for keyword
	// context (they would blow up both the scan time and the context window).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small.go"), []byte("package main\n// mcp magic here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.js"), []byte(strings.Repeat("mcp ", 70_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	got := searchProjectFiles("mcp", map[string]bool{})
	if !strings.Contains(got, "small.go") {
		t.Fatalf("expected the small matching file attached, got:\n%s", got)
	}
	if strings.Contains(got, "bundle.js") {
		t.Fatalf("oversized file must not be read/scored:\n%s", got)
	}
}

func TestSearchProjectFilesStopsAtScanBudget(t *testing.T) {
	// Bounded-work guard: the keyword scan stops after maxSearchScan files.
	// A matching file beyond the budget must never be reached (the walk
	// aborts early instead of reading the whole repository).
	dir := t.TempDir()
	const total = 305
	for i := 0; i < total; i++ {
		content := "package p\n"
		if i == total-1 {
			content = "package p\n// needle-mcp-term\n"
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.go", i)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	got := searchProjectFiles("needle-mcp-term", map[string]bool{})
	if strings.Contains(got, "f304.go") {
		t.Fatalf("file beyond the scan budget must not be read:\n%s", got)
	}
}

func TestToolBlockCommandsExtraction(t *testing.T) {
	// The pre-pass must extract bash/tool_call indicators WITHOUT executing
	// anything (pure string work) and must skip "cat >" file-writer blocks.
	reply := "Let me check the repo.\n\n```bash\nls -la\n```\n\nAlso read this:\n<tool_call>read<arg_key>path</arg_key><arg_value>main.go</arg_value></tool_call>\n\nAnd write a file:\n```bash\ncat > file.txt <<'EOF'\nhi\nEOF\n```\n"
	got := toolBlockCommands(reply)
	if len(got) != 2 {
		t.Fatalf("expected 2 indicators (bash + tool_call), got %v", got)
	}
	if !strings.Contains(got[0], "Running command: ls -la") {
		t.Fatalf("expected bash indicator, got %q", got[0])
	}
	if !strings.Contains(got[1], "Running tool: read") {
		t.Fatalf("expected tool_call indicator, got %q", got[1])
	}
	if strings.Contains(strings.Join(got, "\n"), "cat >") {
		t.Fatalf("cat > builder blocks must not be treated as commands: %v", got)
	}
	// A reply with no tool blocks yields nothing.
	if got := toolBlockCommands("just a normal answer"); len(got) != 0 {
		t.Fatalf("expected no indicators for a plain reply, got %v", got)
	}

	// Malformed tool_call closers (</arg_value> and </bash>) must still be
	// parsed — this is the exact shape poolside emitted and retried 8×.
	broken := "<tool_call>bash\nls -la backend/ && echo ok 2>/dev/null</arg_value></tool_call>"
	got = toolBlockCommands(broken)
	if len(got) != 1 || !strings.Contains(got[0], "Running command: ls -la backend/") {
		t.Fatalf("broken </arg_value> closer must yield a bash indicator, got %v", got)
	}
	brokenBashCloser := "<tool_call>bash\npwd</bash>"
	got = toolBlockCommands(brokenBashCloser)
	if len(got) != 1 || !strings.Contains(got[0], "Running command: pwd") {
		t.Fatalf("broken </bash> closer must yield a bash indicator, got %v", got)
	}
}

func TestParseToolCallShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []toolCall
	}{
		{
			name: "strict read arg pair",
			in:   "<tool_call>read<arg_key>path</arg_key><arg_value>main.go</arg_value></tool_call>",
			want: []toolCall{{name: "read", body: "main.go"}},
		},
		{
			name: "broken arg_value closer",
			in:   "<tool_call>bash\nls -la backend/ 2>/dev/null</arg_value></tool_call>",
			want: []toolCall{{name: "bash", body: "ls -la backend/ 2>/dev/null"}},
		},
		{
			name: "broken bash closer",
			in:   "<tool_call>sh\ncat main.go</sh>",
			want: []toolCall{{name: "sh", body: "cat main.go"}},
		},
		{
			name: "raw clean block",
			in:   "<tool_call>bash\nls -la\n</tool_call>",
			want: []toolCall{{name: "bash", body: "ls -la"}},
		},
		{
			name: "single-line name space body",
			in:   "<tool_call>read main.go</tool_call>",
			want: []toolCall{{name: "read", body: "main.go"}},
		},
		{
			name: "no name defaults to bash",
			in:   "<tool_call>ls -la</tool_call>",
			want: []toolCall{{name: "bash", body: "ls -la"}},
		},
		{
			name: "multiple calls",
			in:   "<tool_call>bash\necho one</tool_call>\n<tool_call>read\na.txt</tool_call>",
			want: []toolCall{{name: "bash", body: "echo one"}, {name: "read", body: "a.txt"}},
		},
		{
			name: "empty block skipped",
			in:   "<tool_call></tool_call>",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseToolCall(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("expected %d calls, got %d: %+v", len(tc.want), len(got), got)
			}
			for i := range got {
				if got[i].name != tc.want[i].name || got[i].body != tc.want[i].body {
					t.Fatalf("call %d mismatch: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCompactToolReply(t *testing.T) {
	// Prose around a fenced bash block survives; the command block is stripped
	// (its execution is told in the trace + ⚙ rows, not in the reply body).
	reply := "Let me check the repo.\n\n```bash\nls -la\n```\n\nDone."
	got, n := compactToolReply(reply)
	if n != 1 {
		t.Fatalf("expected 1 tool call counted, got %d", n)
	}
	if !strings.Contains(got, "Let me check the repo.") || !strings.Contains(got, "Done.") {
		t.Fatalf("prose must survive, got %q", got)
	}
	if strings.Contains(got, "ls -la") {
		t.Fatalf("command block must be stripped, got %q", got)
	}

	// Proper XML tool_call blocks (incl. inner arg_value closer) are stripped.
	got, n = compactToolReply("Here:\n<tool_call>read<arg_key>path</arg_key><arg_value>main.go</arg_value></tool_call>\nOk.")
	if n != 1 || strings.Contains(got, "tool_call") || !strings.Contains(got, "Ok.") {
		t.Fatalf("proper XML call must be stripped (n=%d, got %q)", n, got)
	}

	// Broken closers (the poolside loop shape) are stripped too.
	got, n = compactToolReply("Checking...\n<tool_call>bash\nls -la backend/ 2>/dev/null</arg_value></tool_call>\nResult:")
	if n != 1 || strings.Contains(got, "tool_call") || strings.Contains(got, "ls -la") || !strings.Contains(got, "Checking...") {
		t.Fatalf("broken XML call must be stripped (n=%d, got %q)", n, got)
	}

	// Stray unterminated tag fragments (a bare `<tool_call>bash<`) are cleaned.
	got, n = compactToolReply("\n<tool_call>bash<\nfind . -name '*.go'\n}")
	if n != 1 || strings.Contains(got, "<tool_call") || strings.Contains(got, "</arg_value") {
		t.Fatalf("stray fragments must be cleaned (n=%d, got %q)", n, got)
	}

	// A reply that was NOTHING but tool calls collapses to a one-line summary
	// so the agent still visibly "did work".
	got, n = compactToolReply("```bash\nls\n```\n\n```bash\ncat main.go\n```")
	if n != 2 {
		t.Fatalf("expected 2 tool calls, got %d", n)
	}
	if !strings.Contains(got, "2 tool command(s)") {
		t.Fatalf("pure-tool reply must collapse to a summary, got %q", got)
	}

	// The attribution footer must survive a pure-tool collapse — the
	// model-identity chain parses it to detect mid-session switches.
	got, n = compactToolReply("```bash\nls\n```\n\n  ⚡ opencode/deepseek-v4-flash-free · 3.2s · 133 tokens")
	if n != 1 || !strings.Contains(got, "2 tool command(s)") && !strings.Contains(got, "1 tool command(s)") {
		t.Fatalf("expected collapsed summary (n=%d, got %q)", n, got)
	}
	if !strings.Contains(got, "⚡ opencode/deepseek-v4-flash-free") {
		t.Fatalf("attribution footer must survive the collapse, got %q", got)
	}

	// Prose + footer: the footer stays at the end of the compacted text.
	got, n = compactToolReply("Checked.\n```bash\nls\n```\n\n  ⚡ opencode/deepseek-v4-flash-free · 1.0s · 10 tokens")
	if n != 1 || !strings.Contains(got, "Checked.") || !strings.Contains(got, "⚡ opencode/deepseek-v4-flash-free") {
		t.Fatalf("footer must survive prose compaction (n=%d, got %q)", n, got)
	}

	// A `cat >` heredoc is a BUILDER file-write, not an executed command — it
	// must stay visible (the user verifies what the model wrote), while a
	// sibling command fence in the same reply is stripped.
	got, n = compactToolReply("Here is the file:\n```bash\ncat > notes.md <<'EOF'\nhello\nEOF\n```\n\nAnd the check:\n```bash\ngrep hi notes.md\n```")
	if n != 1 {
		t.Fatalf("expected only the grep fence to count, got %d", n)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "cat > notes.md") {
		t.Fatalf("cat > heredoc content must survive, got %q", got)
	}
	if strings.Contains(got, "grep hi notes.md") {
		t.Fatalf("command fence must be stripped, got %q", got)
	}

	// Generic tag-like prose (`<value>`) must never be eaten by the stray-tag
	// cleanup — only tool-call-specific tag names qualify.
	got, n = compactToolReply("Use <value> as the placeholder.\n<tool_call>bash\necho hi</arg_value></tool_call>")
	if n != 1 {
		t.Fatalf("expected 1 tool call, got %d", n)
	}
	if !strings.Contains(got, "<value>") {
		t.Fatalf("generic <value> prose must survive, got %q", got)
	}

	// A plain reply is untouched and counts zero.
	plain := "just a normal answer with ```bash not a block```"
	got, n = compactToolReply(plain)
	if n != 0 || got != strings.TrimSpace(plain) {
		t.Fatalf("plain reply must pass through (n=%d, got %q)", n, got)
	}
}

func TestApplyAgenticToolsBashEndToEnd(t *testing.T) {
	// The exact broken shape from the poolside loop must now EXECUTE instead of
	// bouncing back as "unsupported tool" — and the feedback must carry the
	// real command output for the model's next turn.
	logs, feedback := applyAgenticTools("<tool_call>bash\necho brocode-tool-probe</arg_value></tool_call>", false)
	if len(logs) == 0 || !strings.Contains(logs[0], "Running command: echo brocode-tool-probe") {
		t.Fatalf("expected the broken call to execute, got logs %v", logs)
	}
	if !strings.Contains(feedback, "brocode-tool-probe") {
		t.Fatalf("expected command output in feedback, got: %q", feedback)
	}
	if strings.Contains(feedback, "unsupported tool") {
		t.Fatalf("broken bash call must not be treated as unsupported: %q", feedback)
	}

	// An unknown tool gets an actionable error naming the excerpt.
	logs, feedback = applyAgenticTools("<tool_call>kuma_context\nquery=foo</tool_call>", false)
	if len(logs) == 0 || !strings.Contains(logs[0], "Unsupported tool call: kuma_context") {
		t.Fatalf("expected unsupported marker, got logs %v", logs)
	}
	if !strings.Contains(feedback, "kuma_context") || !strings.Contains(feedback, "```bash") {
		t.Fatalf("expected actionable feedback naming the tool and the fix, got: %q", feedback)
	}
}

func TestAgentToolResultFeedsQueueAndClearsFlag(t *testing.T) {
	m := newTestModel()
	m.agentRun = 7
	m.toolRunning = true
	updated, cmd := m.Update(agentToolResultMsg{
		logs:     []string{"⚙️  Running command: ls"},
		feedback: "file1\nfile2",
		run:      7,
	})
	m2 := updated.(Model)
	if m2.toolRunning {
		t.Fatal("toolRunning must clear once the result arrives")
	}
	if cmd == nil {
		t.Fatal("expected a command (queue flush auto-sends the tool result)")
	}
	if len(m2.queue) != 0 {
		t.Fatalf("queue should be drained into the auto-send, got %v", m2.queue)
	}
	// The feedback must land in the chat as a transient roleTool message (the
	// queue flush sends it), not as a user prompt with the blue bar.
	if len(m2.chat) == 0 || m2.chat[len(m2.chat)-1].role != roleTool ||
		!strings.Contains(m2.chat[len(m2.chat)-1].content, "file1") {
		t.Fatalf("expected tool feedback sent as a roleTool message, got %+v", m2.chat)
	}
}

func TestStaleAgentToolResultDropped(t *testing.T) {
	// A result from a superseded run (user sent a new prompt meanwhile) must
	// not inject stale tool feedback into the current run.
	m := newTestModel()
	m.agentRun = 9
	m.toolRunning = true
	updated, cmd := m.Update(agentToolResultMsg{
		logs:     []string{"⚙️  Running command: ls"},
		feedback: "stale output",
		run:      5, // old run
	})
	m2 := updated.(Model)
	if cmd != nil {
		t.Fatal("stale tool result must not trigger a queue flush")
	}
	if m2.toolRunning {
		t.Fatal("toolRunning must clear even for a stale result")
	}
	if len(m2.queue) != 0 {
		t.Fatalf("stale feedback must not reach the queue, got %v", m2.queue)
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

func TestSuggestPopupOnTinyTerminalNoPanic(t *testing.T) {
	// Regression: rendering the "/" suggestion popup on a short terminal used
	// to panic (runtime error: index out of range [-1]) because the popup was
	// taller than the space above the input. It must render trimmed, never crash.
	m := newTestModel()
	m.width, m.height = 60, 12
	m.layout()
	m.input.SetValue("/")
	v := m.View().Content
	lines := strings.Split(v, "\n")
	if len(lines) > 12 {
		t.Fatalf("view taller than terminal (%d lines):\n%s", len(lines), v)
	}
	if !strings.Contains(v, "❯") {
		t.Fatalf("input bar must stay visible:\n%s", v)
	}
}

func TestMouseCommandRemoved(t *testing.T) {
	// /mouse used to toggle mouse capture off, which made the terminal
	// swallow wheel events ("scrolling scrolls the terminal, not the chat").
	// It must no longer exist as a command or a suggestion.
	if got := suggestFiltered("/mouse"); len(got) != 0 {
		t.Fatalf("/mouse must not appear in suggestions, got %+v", got)
	}
	// Sending it now routes to the normal prompt path (a regular query).
	m := newTestModel()
	m.input.SetValue("/mouse")
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if len(m2.chat) != 1 || m2.chat[0].text != "/mouse" {
		t.Fatalf("expected /mouse treated as a regular prompt, got %+v", m2.chat)
	}
	// Mouse capture must stay on regardless (nothing can toggle it anymore).
	v := m2.View()
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("mouse capture must always be on, got mode %v", v.MouseMode)
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

func TestCompactRefreshesCtxForecast(t *testing.T) {
	// Regression: after a compact, the top-right ctx readout must DROP — the
	// forecast (ctxUsed) must be recomputed from the folded transcript and the
	// stale last-response settlement (actualTokens) must be invalidated.
	m := newTestModel()
	m.window = 500
	m.chat = buildBigChat(12)
	m.refreshCtx()
	before := m.ctxUsed
	m.actualTokens = tokenUsage{total: before} // simulate a settled response

	if !m.forceCompact() {
		t.Fatal("expected forced compaction to run")
	}
	if m.ctxUsed >= before {
		t.Fatalf("ctxUsed must drop after compact: before=%d after=%d", before, m.ctxUsed)
	}
	if m.actualTokens.total != 0 {
		t.Fatalf("stale settlement must be cleared after compact, got %d", m.actualTokens.total)
	}
	// The header readout now falls back to the fresh forecast (labeled ~).
	v := ansiStrip.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(v, "ctx ~"+fmtTokens(m.ctxUsed)+" / ") {
		t.Fatalf("header must show the fresh forecast after compact:\n%s", v)
	}
}

func TestCompactTriggerConfigurable(t *testing.T) {
	// compact_trigger_pct in config.jsonc overrides the 70% default — a user
	// with a big window can compact earlier (e.g. 50%) or later.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestConfig(t, home, `{"compact_trigger_pct": 0.5}`)

	// 10 messages ≈ 390 tokens; 56% of a 700 window. Over a 50% trigger,
	// under the 70% default — so a 0.5 trigger must fire here.
	m := newTestModel()
	m.window = 700
	m.chat = buildBigChat(10)
	if pct := m.compactTriggerPct(); pct != 0.5 {
		t.Fatalf("expected configured trigger 0.5, got %v", pct)
	}
	if !m.maybeCompact() {
		t.Fatal("expected compaction at 56% with a 0.5 configured trigger")
	}
}

func TestCompactTriggerDefaultsTo70(t *testing.T) {
	// No config → the tuned 70% default applies.
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := newTestModel()
	if pct := m.compactTriggerPct(); pct != compactTriggerPct {
		t.Fatalf("expected default trigger %v, got %v", compactTriggerPct, pct)
	}
}

func TestModelWindowHonorsConfigLimit(t *testing.T) {
	// A custom provider model declaring limit.context in config.jsonc must
	// win over every heuristic — a 1M-token local model shows 1M, never the
	// 128k fallback. Case-insensitive on the model id.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestConfig(t, home, `{
  "provider": {
    "mygpu": {
      "name": "My GPU",
      "options": {"baseURL": "http://127.0.0.1:11434/v1"},
      "models": {
        "Llama-3-8B": {"name": "Llama 3 8B", "limit": {"context": 1000000}}
      }
    }
  }
}`)
	if got := modelWindowFor("mygpu", "llama-3-8b"); got != 1_000_000 {
		t.Fatalf("config limit.context must win, got %d", got)
	}
	// A model WITHOUT a declared limit falls through to the heuristics.
	if got := modelWindowFor("mygpu", "some-other-model"); got != 128_000 {
		t.Fatalf("expected 128k fallback for undeclared model, got %d", got)
	}
	// A provider that is not configured still uses heuristics (e.g. gemini).
	if got := modelWindowFor("antigravity", "gemini-3.6-flash"); got != 1_000_000 {
		t.Fatalf("expected gemini heuristic 1M, got %d", got)
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

func TestCompactSkipsContentlessMiddle(t *testing.T) {
	// A middle of only transient tool/system rows must NOT fold into a blank
	// ledger ("goal: \n\nuser requests:\n…") — compaction bails instead.
	m := newTestModel()
	m.window = 500
	m.chat = []chatMsg{
		{role: roleUser, text: "goal"},
		{role: roleTool, summary: "Tool Executed", content: "x", collapsed: true},
		{role: roleTool, summary: "Tool Executed", content: "y", collapsed: true},
		{role: roleUser, text: "tail"},
	}
	if m.forceCompact() {
		t.Fatal("must not compact a contentless middle into a blank ledger")
	}
	if m.compactCount != 0 {
		t.Fatalf("expected no compaction, got %d", m.compactCount)
	}
}

func TestChatTokensCountsToolContent(t *testing.T) {
	// roleTool rows carry their payload in content (text is empty) — the
	// forecast must count it or compaction triggers late.
	chat := []chatMsg{{role: roleTool, content: strings.Repeat("x", 400)}}
	if got := chatTokens(chat); got != 104 {
		t.Fatalf("expected tool content counted (104), got %d", got)
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
	// Real settlement replaces the forecast when the API reports tokens — and
	// the pressure is the INPUT count (what the next request will cost),
	// never input+output: output tokens don't re-enter the context window.
	m.actualTokens = tokenUsage{input: 500, output: 300, total: 800}
	panel = m.renderPanel()
	if strings.Contains(panel, "~800") || !strings.Contains(panel, "500 / 131.1k") {
		t.Fatalf("panel should prefer the settled INPUT over forecast, got:\n%s", panel)
	}
	if strings.Contains(panel, "800 / 131.1k") {
		t.Fatalf("context pressure must not include output tokens, got:\n%s", panel)
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
	old := sessionRoot
	sessionRoot = func() (string, error) { return dir, nil }
	defer func() { sessionRoot = old }()

	msgs := []chatMsg{
		{role: roleUser, text: "initial goal"},
		{role: roleSystem, text: "📋 context summary (L2 ledger)\ngoal: initial goal"},
		{role: roleUser, text: strings.Repeat("x", 400)},
	}
	if err := saveSessionTo(msgs, filepath.Join(dir, "session_test.jsonl")); err != nil {
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

func TestToolResultRoutesToRoleTool(t *testing.T) {
	// An agentic [SYSTEM TOOL RESULT] queued by the tool runner must append a
	// roleTool chat message — NOT a user message (so it never renders with the
	// blue user bar and never replays via ↑ prompt history).
	m := newTestModel()
	m.input.SetValue("[SYSTEM TOOL RESULT]\nexit 0\n---\n```bash\nls\n```")
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if len(m2.chat) != 1 || m2.chat[0].role != roleTool {
		t.Fatalf("expected a roleTool chat message, got %+v", m2.chat)
	}
	if !m2.chat[0].collapsed || m2.chat[0].summary == "" || m2.chat[0].content == "" {
		t.Fatalf("expected collapsed tool block with summary+content, got %+v", m2.chat[0])
	}
	// Tool output is transient — never added to prompt history.
	if len(m2.promptHistory) != 0 {
		t.Fatalf("tool results must not enter prompt history, got %v", m2.promptHistory)
	}
}

func TestToolResultRendersWithoutUserBar(t *testing.T) {
	// The collapsed tool row must read as a system event: a clean "⚙  Tool
	// Executed" line with a real gap after the icon, no blue ▌ bar, and a
	// ctrl+o hint. It must never look like a user message.
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleTool, summary: "Tool Executed", content: "[SYSTEM TOOL RESULT]\nexit 0", collapsed: true})
	m.refreshChat()
	v := m.View().Content

	if !strings.Contains(v, "⚙") || !strings.Contains(v, "Tool Executed") {
		t.Fatalf("expected the tool row in the view:\n%s", v)
	}
	// No line may pair the blue user bar with tool text.
	for _, ln := range strings.Split(v, "\n") {
		if strings.Contains(ln, "▌") && strings.Contains(ln, "Tool Executed") {
			t.Fatalf("tool row must not use the user bar:\n%s", v)
		}
	}
	// The renderer owns the icon: exactly one ⚙ on the tool line (a doubled
	// gear happens when the summary carries its own icon AND the renderer
	// prepends one — this regression guard keeps the row clean).
	toolLines := 0
	for _, ln := range strings.Split(v, "\n") {
		if strings.Contains(ln, "Tool Executed") {
			toolLines++
			if n := strings.Count(ln, "⚙"); n != 1 {
				t.Fatalf("tool row must render exactly one ⚙ (got %d): %q", n, ln)
			}
		}
	}
	if toolLines == 0 {
		t.Fatal("expected at least one rendered tool row line")
	}
	// Expanded reveals the tool output labeled as tool output — and the
	// internal [SYSTEM TOOL RESULT] transport prefix must be stripped.
	expanded := m.renderToolMsg(chatMsg{role: roleTool, content: "[SYSTEM TOOL RESULT]\nexit 0"}, 60)
	if !strings.Contains(expanded, "tool output") || !strings.Contains(expanded, "exit 0") {
		t.Fatalf("expanded tool row missing output, got: %q", expanded)
	}
	if strings.Contains(expanded, "[SYSTEM TOOL RESULT]") {
		t.Fatalf("expanded tool row leaks the transport prefix: %q", expanded)
	}
}

func TestSessionSaveSkipsToolRows(t *testing.T) {
	// Transient tool rows must never be persisted — a resumed session should
	// contain only real conversation turns.
	dir := t.TempDir()
	path := filepath.Join(dir, "latest.jsonl")
	msgs := []chatMsg{
		{role: roleUser, text: "hello"},
		{role: roleTool, summary: "Tool Executed", content: "[SYSTEM TOOL RESULT]\nexit 0", collapsed: true},
		{role: roleAgent, text: "hi there"},
	}
	if err := saveSessionTo(msgs, path); err != nil {
		t.Fatal(err)
	}
	got, err := loadSessionFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected tool row skipped (2 persisted), got %d: %+v", len(got), got)
	}
	if got[0].role != roleUser || got[1].role != roleAgent {
		t.Fatalf("persisted roles wrong: %+v", got)
	}
}

func TestSessionSaveSkipsBlankRows(t *testing.T) {
	// Blank turns (an agent reply interrupted before any content, stray
	// whitespace) must never be persisted — a resumed session would replay
	// them as empty, divider-only messages.
	dir := t.TempDir()
	path := filepath.Join(dir, "latest.jsonl")
	msgs := []chatMsg{
		{role: roleUser, text: "hello"},
		{role: roleAgent, text: ""}, // interrupted before any content
		{role: roleAgent, text: "real reply"},
		{role: roleUser, text: "   "},
	}
	if err := saveSessionTo(msgs, path); err != nil {
		t.Fatal(err)
	}
	got, err := loadSessionFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 non-blank rows persisted, got %d: %+v", len(got), got)
	}
	if got[0].text != "hello" || got[1].text != "real reply" {
		t.Fatalf("wrong rows persisted: %+v", got)
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
	// Point the session root at a temp dir so the test never touches the
	// real home directory.
	dir := t.TempDir()
	old := sessionRoot
	sessionRoot = func() (string, error) { return dir, nil }
	defer func() { sessionRoot = old }()

	if _, err := SaveSession([]chatMsg{{role: roleUser, text: "from before"}}); err != nil {
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
	old := sessionRoot
	sessionRoot = func() (string, error) { return dir, nil }
	defer func() { sessionRoot = old }()

	m := New(search.New(search.SampleCorpus()), "0.1.0", "test", true)
	if m.started {
		t.Fatal("expected landing when no session file exists")
	}
	if !strings.Contains(m.status, "no previous session") {
		t.Fatalf("expected no-session notice in status, got %q", m.status)
	}
}

func TestListSessionsScopedToCurrentProject(t *testing.T) {
	// /history is scoped to the project brocode is running in: running in
	// project A must NEVER list sessions saved in project B. It lists the real
	// per-project session files (~/.brocode/projects/<proj>/session_<id>.jsonl)
	// and must not show the resume fallback latest.jsonl.
	home := t.TempDir()
	t.Setenv("HOME", home)

	// The "current project": brocode's project key is the cwd basename.
	work := filepath.Join(home, "workspace", "mauproj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	content := "{\"role\":\"user\",\"text\":\"hi\"}\n{\"role\":\"agent\",\"text\":\"yo\"}\n"

	// Current project's sessions (two of them).
	cur := filepath.Join(home, ".brocode", "projects", "mauproj")
	if err := os.MkdirAll(cur, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"abc12345", "def67890"} {
		if err := os.WriteFile(filepath.Join(cur, "session_"+id+".jsonl"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Another project's session — must NOT appear in this project's history.
	other := filepath.Join(home, ".brocode", "projects", "otherproj")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "session_xyz99999.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// A stale fallback (e.g. leftover test data) must never appear in history.
	if err := os.MkdirAll(filepath.Join(home, ".brocode", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".brocode", "sessions", "latest.jsonl"), []byte("{\"role\":\"user\",\"text\":\"from before\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions := listSessions()
	if len(sessions) != 2 {
		t.Fatalf("expected only the current project's 2 sessions, got %d: %+v", len(sessions), sessions)
	}
	ids := map[string]bool{}
	for _, s := range sessions {
		ids[s.name] = true
		if strings.Contains(s.path, "otherproj") {
			t.Fatalf("session from another project leaked into history: %+v", s)
		}
		if strings.Contains(s.path, "sessions/latest") {
			t.Fatalf("fallback latest.jsonl leaked into history: %+v", s)
		}
		if s.msgCount != 2 {
			t.Fatalf("expected 2 messages counted for %s, got %d", s.path, s.msgCount)
		}
	}
	if !ids["abc12345"] || !ids["def67890"] {
		t.Fatalf("expected both current-project sessions listed, got %v", ids)
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
	msgs := zenMessages(chat, "current prompt (already appended by send)", "opencode", "deepseek-v4-flash-free", 135000, false)
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
		t.Fatalf("expected fallback total=12, got %d", tok.total)
	}
}

func TestParseZenResponseTrailingSSE(t *testing.T) {
	// Proxy gateways (9router, OpenRouter, LiteLLM) often append data: [DONE]
	// to non-streaming HTTP responses — parseZenResponse must sanitize it.
	body := `{"id":"router-c449d220d0851fea23704c14e9ff4f09","choices":[{"message":{"content":"Hello from 9router"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}data: [DONE]`
	text, _, tok, err := parseZenResponse([]byte(body))
	if err != nil {
		t.Fatalf("expected parseZenResponse to sanitize trailing data: [DONE], got error: %v", err)
	}
	if text != "Hello from 9router" {
		t.Fatalf("expected text 'Hello from 9router', got %q", text)
	}
	if tok.total != 15 {
		t.Fatalf("expected total tokens 15, got %d", tok.total)
	}
}

func TestParseZenResponseNativeToolCalls(t *testing.T) {
	// Native OpenAI function calling payload (tool_calls array)
	body := `{"id":"call-123","choices":[{"message":{"content":"Checking directory","tool_calls":[{"id":"tc1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls -la\"}"}}]}}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`
	text, _, _, err := parseZenResponse([]byte(body))
	if err != nil {
		t.Fatalf("expected parseZenResponse to handle native tool_calls, got: %v", err)
	}
	if !strings.Contains(text, "Checking directory") || !strings.Contains(text, "```bash\nls -la\n```") {
		t.Fatalf("expected native tool call converted to bash block, got: %q", text)
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
	msgs := zenMessages(chat, "second", "opencode", "deepseek-v4-flash-free", 135000, false)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (system + first + answer + prompt), got %d: %+v", len(msgs), msgs)
	}
	if got := msgs[2]["content"]; got != "the answer" {
		t.Fatalf("attribution footer must not reach the model, got %q", got)
	}
}

func TestZenMessagesIncludesCompactionLedger(t *testing.T) {
	// The L2 compaction ledger is the folded-context summary — it MUST reach
	// the model or compaction silently erases the middle of the conversation
	// (the model would see a gap: old head, then the verbatim tail). UI
	// chatter (theme changes, OAuth notices, interrupt banners) must NOT.
	chat := []chatMsg{
		{role: roleUser, text: "goal"},
		{role: roleSystem, text: "🔄 context compact — 8 messages → L2 state ledger · saved 4.2k tokens\n📋 context summary (L2 ledger)\ngoal: build docs"},
		{role: roleUser, text: "continue"},
	}
	msgs := zenMessages(chat, "continue", "opencode", "deepseek-v4-flash-free", 135000, false)
	foundLedger := false
	for _, m := range msgs {
		if m["role"] == "system" && strings.Contains(m["content"], "L2 ledger") {
			foundLedger = true
		}
	}
	if !foundLedger {
		t.Fatalf("L2 compaction ledger must reach the model:\n%+v", msgs)
	}

	// UI chatter must stay out.
	chat2 := []chatMsg{
		{role: roleUser, text: "hi"},
		{role: roleSystem, text: "theme → ocean"},
		{role: roleAgent, text: "hello"},
		{role: roleUser, text: "again"},
	}
	msgs2 := zenMessages(chat2, "again", "opencode", "deepseek-v4-flash-free", 135000, false)
	for _, m := range msgs2 {
		if m["role"] == "system" && strings.Contains(m["content"], "theme → ocean") {
			t.Fatalf("UI chatter must not reach the model:\n%+v", msgs2)
		}
	}
}

func TestZenMessagesIncludesToolResults(t *testing.T) {
	// Agentic-loop tool feedback is conversation data — the assistant reply
	// after it answers that output. Dropping the turn left back-to-back
	// assistant messages and a blank context gap.
	chat := []chatMsg{
		{role: roleUser, text: "audit project"},
		{role: roleAgent, text: "plan with tool blocks"},
		{role: roleTool, summary: "Tool Executed", content: "[SYSTEM TOOL RESULT]\nls output", collapsed: true},
		{role: roleAgent, text: "done"},
		{role: roleUser, text: "continue"},
	}
	msgs := zenMessages(chat, "continue", "opencode", "deepseek-v4-flash-free", 135000, false)
	// The tool payload must appear EXACTLY once — as a prior user turn. It
	// must not be duplicated as the current prompt q (the last chat entry is
	// skipped) nor dropped (back-to-back assistant turns with a blank gap).
	count := 0
	for _, m := range msgs {
		if strings.Contains(m["content"], "ls output") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("tool result must reach the model exactly once, got %d:\n%+v", count, msgs)
	}
}

func TestZenMessagesSkipsEmptyRows(t *testing.T) {
	// Blank turns must never reach the model as zero-length messages.
	chat := []chatMsg{
		{role: roleUser, text: "   "},
		{role: roleAgent, text: ""},
		{role: roleUser, text: "real prompt"},
	}
	msgs := zenMessages(chat, "real prompt", "opencode", "deepseek-v4-flash-free", 135000, false)
	if len(msgs) != 2 { // system directive + the prompt
		t.Fatalf("expected system + prompt only, got %d: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if strings.TrimSpace(m["content"]) == "" {
			t.Fatalf("blank message reached the model: %+v", msgs)
		}
	}
}

func TestZenMessagesModelIdentityNote(t *testing.T) {
	// The active model must be told WHO it is (provider/model + context
	// window) inside the user-prompt metadata — never in the system prompt,
	// so switching models does not invalidate the system-prefix cache.
	chat := []chatMsg{
		{role: roleUser, text: "build docs"},
		{role: roleAgent, text: "on it\n\n  ⚡ opencode/deepseek-v4-flash-free · 3.2s · 133 tokens"},
		{role: roleUser, text: "continue"},
	}
	msgs := zenMessages(chat, "continue", "opencode", "deepseek-v4-flash-free", 135000, false)
	if len(msgs) != 4 { // system + user + assistant + prompt
		t.Fatalf("expected 4 messages, got %d: %+v", len(msgs), msgs)
	}
	last := msgs[len(msgs)-1]["content"]
	if !strings.Contains(last, "Active model: opencode/deepseek-v4-flash-free") {
		t.Fatalf("model identity missing from metadata: %q", last)
	}
	if !strings.Contains(last, "Context window: "+fmtTokens(135000)+" tokens") {
		t.Fatalf("context window missing from metadata: %q", last)
	}
	// Same model as the prior turn → NO switch note.
	if strings.Contains(last, "switched mid-session") {
		t.Fatalf("no switch expected when the model is unchanged: %q", last)
	}
	// The system prompt itself must stay model-agnostic (cache stability).
	sys := msgs[0]["content"]
	if strings.Contains(sys, "Active model:") {
		t.Fatalf("model identity must not leak into the system prompt:\n%s", sys)
	}
}

func TestZenMessagesMidSessionModelSwitch(t *testing.T) {
	// THE regression this feature exists for: the user starts with
	// opencode/deepseek, switches to poolside mid-session via /models, and
	// sends the next prompt. The new model inherits the whole history but
	// must be TOLD it was swapped in — otherwise it may copy the prior
	// model's tool format or misread earlier turns as its own output.
	chat := []chatMsg{
		{role: roleUser, text: "audit the project"},
		{role: roleAgent, text: "found these issues\n\n  ⚡ opencode/deepseek-v4-flash-free · 24.0s · 6.7k tokens"},
		{role: roleUser, text: "gas"},
	}
	msgs := zenMessages(chat, "gas", "poolside", "poolside/laguna-s-2.1", 135000, false)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %+v", len(msgs), msgs)
	}
	last := msgs[len(msgs)-1]["content"]
	if !strings.Contains(last, "Active model: poolside/poolside/laguna-s-2.1") {
		t.Fatalf("new model identity missing: %q", last)
	}
	if !strings.Contains(last, "switched mid-session from opencode/deepseek-v4-flash-free") {
		t.Fatalf("switch note must name the prior model: %q", last)
	}
	if !strings.Contains(last, "continue seamlessly") {
		t.Fatalf("switch note must tell the model to continue: %q", last)
	}

	// A fresh session (no prior agent reply) has no switch note, only identity.
	fresh := []chatMsg{{role: roleUser, text: "hello"}}
	msgs = zenMessages(fresh, "hello", "groq", "llama-3.3-70b-versatile", 131072, false)
	last = msgs[len(msgs)-1]["content"]
	if !strings.Contains(last, "Active model: groq/llama-3.3-70b-versatile") {
		t.Fatalf("fresh session must still carry identity: %q", last)
	}
	if strings.Contains(last, "switched mid-session") {
		t.Fatalf("fresh session must not claim a switch: %q", last)
	}
}

func TestLastAttributionModelParsing(t *testing.T) {
	// lastAttributionModel reads the provider/model from the MOST RECENT agent
	// reply's footer — the source of truth for mid-session switch detection.
	chat := []chatMsg{
		{role: roleUser, text: "q1"},
		{role: roleAgent, text: "a1\n\n  ⚡ opencode/deepseek-v4-flash-free · 1.0s · 10 tokens"},
		{role: roleUser, text: "q2"},
		{role: roleAgent, text: "a2\n\n  ⚡ poolside/poolside/laguna-s-2.1 · 2.0s · 20 tokens"},
		{role: roleUser, text: "q3"},
	}
	p, m := lastAttributionModel(chat)
	if p != "poolside" || m != "poolside/laguna-s-2.1" {
		t.Fatalf("expected the LAST agent's model, got %q/%q", p, m)
	}
	// No agent reply yet → empty (fresh session).
	if p, m := lastAttributionModel([]chatMsg{{role: roleUser, text: "hi"}}); p != "" || m != "" {
		t.Fatalf("expected empty for a session with no agent reply, got %q/%q", p, m)
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

func TestAgentAskOpensPopover(t *testing.T) {
	m := newTestModel()
	m.started = true // the popover renders in the chat body, not the landing
	m.agentWorking = true
	updated, _ := m.Update(agentAskMsg{
		title: "agent needs your input",
		questions: []askQuestion{
			{header: "Auth method", question: "Which one?", options: []string{"JWT", "Session cookies"}},
			{header: "Scope", question: "Which areas?", options: []string{"Admin", "API"}, multiSelect: true},
		},
	})
	m2 := updated.(Model)
	if !m2.askOpen {
		t.Fatal("expected askOpen")
	}
	if m2.askKind != askClarify || len(m2.askQuestions) != 2 {
		t.Fatalf("popover state wrong: kind=%v questions=%d", m2.askKind, len(m2.askQuestions))
	}
	// The popover renders inline (both questions, incl. the checkbox label).
	v := ansiStrip.ReplaceAllString(m2.View().Content, "")
	for _, want := range []string{"Auth method", "JWT", "Scope", "[multiple]"} {
		if !strings.Contains(v, want) {
			t.Fatalf("popover missing %q:\n%s", want, v)
		}
	}
}

func TestStaleAskFromInterruptedRunDropped(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true // a NEW run is in progress
	m.agentRun = 2
	// A question left in the buffer by the interrupted run 1 must not open
	// the ask popover during run 2.
	updated, _ := m.Update(agentAskMsg{questions: []askQuestion{{question: "phantom?", options: []string{"A"}}}, run: 1})
	m2 := updated.(Model)
	if m2.askOpen {
		t.Fatal("stale question must not open the ask UI")
	}
}

func TestAskSubmitSendsSerializedAnswers(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true
	m.answerCh = make(chan string, 1)
	m.openAsk("", []askQuestion{
		{header: "Auth", question: "Which?", options: []string{"JWT", "Cookies"}},
		{header: "Scope", question: "Which areas?", options: []string{"Admin", "API"}, multiSelect: true},
	}, "")

	// items: JWT(0) Cookies(1) custom(2) Admin(3) API(4) custom(5)
	m.toggleAskFocus()                         // radio: JWT
	m.askFocus = m.moveAskFocus(m.askFocus, 1) // Cookies
	m.toggleAskFocus()                         // radio: Cookies
	m.askFocus = m.moveAskFocus(m.askFocus, 2) // Admin (skips Q1 custom row)
	m.toggleAskFocus()                         // checkbox: Admin on
	m.askFocus = m.moveAskFocus(m.askFocus, 1) // API
	m.toggleAskFocus()                         // checkbox: API on

	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if m2.askOpen {
		t.Fatal("expected popover closed after enter")
	}
	select {
	case ans := <-m.answerCh:
		if !strings.Contains(ans, "Auth: Cookies") || !strings.Contains(ans, "Scope: Admin, API") {
			t.Fatalf("unexpected serialized answers: %q", ans)
		}
	default:
		t.Fatal("answers not sent to the agent")
	}
}

func TestAskCustomAnswerFlow(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true
	m.answerCh = make(chan string, 1)
	m.openAsk("", []askQuestion{{header: "H", question: "Q?", options: []string{"A", "B"}}}, "")

	// Focus the custom row (items: A(0) B(1) custom(2)) and open the editor.
	m.askFocus = 2
	m.toggleAskFocus()
	if !m.askCustomOpen {
		t.Fatal("expected custom editor open")
	}
	m.input.SetValue("jawaban bebas saya")
	updated, _ := m.Update(enterKey()) // saves the custom answer
	m2 := updated.(Model)
	if m2.askCustomOpen {
		t.Fatal("expected custom editor closed after enter")
	}
	if m2.askCustom[0] != "jawaban bebas saya" {
		t.Fatalf("custom answer not saved: %q", m2.askCustom[0])
	}
	updated, _ = m2.Update(enterKey()) // submits the form
	m3 := updated.(Model)
	if m3.askOpen {
		t.Fatal("expected popover closed after submit")
	}
	select {
	case ans := <-m.answerCh:
		if !strings.Contains(ans, "custom: jawaban bebas saya") {
			t.Fatalf("expected custom answer serialized, got %q", ans)
		}
	default:
		t.Fatal("answers not sent")
	}
}

func TestAskEscCancelsAndSignalsAbort(t *testing.T) {
	m := newTestModel()
	m.agentWorking = true
	m.answerCh = make(chan string, 1)
	m.openAsk("", []askQuestion{{question: "Q?", options: []string{"A"}}}, "")

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m2 := updated.(Model)
	if m2.askOpen {
		t.Fatal("expected popover closed on esc")
	}
	if !m2.agentAborted || m2.agentWorking {
		t.Fatal("expected run marked aborted and stopped")
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

func TestAskArrowsNavigateItems(t *testing.T) {
	m := newTestModel()
	m.openAsk("", []askQuestion{{question: "Q?", options: []string{"A", "B", "C"}}}, "")
	// items: A(0) B(1) C(2) custom(3)

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m2 := updated.(Model)
	if m2.askFocus != 1 {
		t.Fatalf("expected focus 1 after down, got %d", m2.askFocus)
	}
	updated, _ = m2.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m3 := updated.(Model)
	if m3.askFocus != 2 {
		t.Fatalf("expected focus 2, got %d", m3.askFocus)
	}
	updated, _ = m3.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m4 := updated.(Model)
	if m4.askFocus != 1 {
		t.Fatalf("expected focus 1 after up, got %d", m4.askFocus)
	}
	// Down past the last item wraps to the first.
	for i := 0; i < 4; i++ {
		updated, _ = m4.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		m4 = updated.(Model)
	}
	if m4.askFocus != 1 {
		t.Fatalf("expected wrap-around to 1, got %d", m4.askFocus)
	}
}

func TestMockAgentRunAsksAndContinues(t *testing.T) {
	m := newTestModel()
	m.provider = "" // mock fallback — no network
	traceCh := make(chan agentTraceMsg, 64)
	askCh := make(chan agentAskMsg, 1)
	answerCh := make(chan string, 1)
	cancel := func() {}
	cmd := m.agentWorkCmd("hello", traceCh, askCh, answerCh, &cancel, 9)

	resultCh := make(chan tea.Msg, 1)
	go func() { resultCh <- cmd() }()

	// The scripted run pauses with MULTIPLE questions for the user.
	q, ok := <-askCh
	if !ok {
		t.Fatal("expected an ask from the mock agent")
	}
	if len(q.questions) < 2 {
		t.Fatalf("expected multiple questions, got %d: %+v", len(q.questions), q.questions)
	}

	// Trace lines were streamed before the questions.
	if len(traceCh) == 0 {
		t.Fatal("expected trace lines streamed during the run")
	}

	answerCh <- "1. How to proceed: Show answer summary\n2. Extras: Trace log"
	msg := <-resultCh
	res, ok := msg.(agentResultMsg)
	if !ok {
		t.Fatalf("expected agentResultMsg, got %T", msg)
	}
	if res.run != 9 {
		t.Fatalf("expected run id 9, got %d", res.run)
	}
	if !strings.Contains(res.reply.text, "Show answer summary") {
		t.Fatalf("final reply must reference the chosen answer, got:\n%s", res.reply.text)
	}
}

func TestMockAgentRunAbortedOnCancel(t *testing.T) {
	m := newTestModel()
	m.provider = ""
	traceCh := make(chan agentTraceMsg, 64)
	askCh := make(chan agentAskMsg, 1)
	answerCh := make(chan string, 1)
	cancel := func() {}
	cmd := m.agentWorkCmd("hello", traceCh, askCh, answerCh, &cancel, 5)

	resultCh := make(chan tea.Msg, 1)
	go func() { resultCh <- cmd() }()

	q, ok := <-askCh
	if !ok || len(q.questions) == 0 {
		t.Fatalf("expected an ask first, got %+v", q)
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

func chatRoles(chat []chatMsg) []string {
	names := map[role]string{roleSystem: "system", roleUser: "user", roleAgent: "agent", roleTool: "tool"}
	out := make([]string, 0, len(chat))
	for _, cm := range chat {
		if n, ok := names[cm.role]; ok {
			out = append(out, n)
		} else {
			out = append(out, fmt.Sprintf("role(%d)", cm.role))
		}
	}
	return out
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

func TestReadToolFeedbackCapped(t *testing.T) {
	// The read tool feeds file content straight back to the model — a large
	// file must be capped like command output, not dumped into the context.
	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("data ", 20000)), 0o644); err != nil { // ~100KB, past the 40k-char cap
		t.Fatal(err)
	}
	logs, feedback := applyAgenticTools("<tool_call>read\n"+big+"</tool_call>", false)
	if len(logs) == 0 || !strings.Contains(logs[0], "Reading file") {
		t.Fatalf("expected read log, got %v", logs)
	}
	if !strings.Contains(feedback, "[output truncated") {
		t.Fatalf("large read must be capped with a marker, feedback len=%d", len(feedback))
	}
	if strings.Count(feedback, "data ") > 8000 {
		t.Fatalf("read content not capped, ~%d tokens of file dumped", strings.Count(feedback, "data "))
	}
}

func TestIsTrivialFollowup(t *testing.T) {
	// Short conversational continuations are NOT worth a 300-file scan.
	trivial := []string{"lanjut", "gas", "yes", "ok", "lanjutkan", "gas aja", "sip", "mantap"}
	for _, q := range trivial {
		if !isTrivialFollowup(q) {
			t.Fatalf("expected %q to be a trivial follow-up", q)
		}
	}
	// File-ish references mean real intent — keep the scan.
	nontrivial := []string{"fix bug di app.go", "cek router/ 3", "lihat internal/tui/agent.go", "buatin main.go", "jalankan ./test.sh", "perbaiki auth_flow"}
	for _, q := range nontrivial {
		if isTrivialFollowup(q) {
			t.Fatalf("expected %q to keep the keyword scan", q)
		}
	}
	// Short CODE-INTENT queries without file tokens also keep the scan
	// ("fix compile" needs context just as much as a path reference).
	intent := []string{"fix compile", "add test", "debug error", "refactor ini", "cek bug"}
	for _, q := range intent {
		if isTrivialFollowup(q) {
			t.Fatalf("expected %q to keep the scan (code intent verb)", q)
		}
	}
	// A long prompt is never trivial even without file tokens.
	if isTrivialFollowup("tolong jelaskan secara detail bagaimana cara kerja compactor di brocode ini") {
		t.Fatal("long prompt must never be treated as trivial")
	}
}

func TestResetAttachCacheClearsSeenFiles(t *testing.T) {
	// A file attached before /clear must be attachable again after the reset
	// — the fresh conversation has zero model context.
	dir := t.TempDir()
	f := filepath.Join(dir, "main.go")
	if err := os.WriteFile(f, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	resetAttachCache()
	rememberAttached(map[string]bool{f: true})
	if seenAt := fileSeen(sessionSeen(), f); seenAt == 0 {
		t.Fatal("expected file to be remembered after attach")
	}
	resetAttachCache()
	if seenAt := fileSeen(sessionSeen(), f); seenAt != 0 {
		t.Fatalf("expected reset to clear the file, got mtime %d", seenAt)
	}
}

func TestSeenFileReattachesAfterEdit(t *testing.T) {
	// The mtime-aware dedup must NOT block re-attachment of a file edited
	// since its last attach — the model cannot keep a stale snapshot.
	dir := t.TempDir()
	f := filepath.Join(dir, "app.go")
	if err := os.WriteFile(f, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	resetAttachCache()
	rememberAttached(map[string]bool{f: true})
	first := fileSeen(sessionSeen(), f)
	if first == 0 {
		t.Fatal("expected file recorded after attach")
	}

	// Edited file → new mtime → must NOT match the recorded one.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(f, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(f)
	if fileSeen(sessionSeen(), f) == fi.ModTime().UnixNano() {
		t.Fatal("edited file must not match its old recorded mtime")
	}
}

func TestStreamRevealChunkAdaptive(t *testing.T) {
	// A fully-received long reply must reveal in ~streamRevealSecs seconds,
	// not at a glacial fixed pace: 10k chars ÷ (20fps × 2s) = 250/tick.
	if got := streamRevealChunk(10000); got < 200 || got > 300 {
		t.Fatalf("expected ~250 chars/tick for a 10k reply, got %d", got)
	}
	// Short replies keep the smooth minimum pace (12 chars/tick).
	if got := streamRevealChunk(50); got != streamChunk {
		t.Fatalf("expected min streamChunk for a short reply, got %d", got)
	}
	// Never dump more than 2k chars in a single frame.
	if got := streamRevealChunk(1_000_000); got > 2000 {
		t.Fatalf("reveal chunk must be capped per frame, got %d", got)
	}
	// Empty buffer → minimum pace (defensive).
	if got := streamRevealChunk(0); got != streamChunk {
		t.Fatalf("expected min pace for empty buffer, got %d", got)
	}
}

func TestModelWindowForGroqFallback(t *testing.T) {
	// Unknown Groq models must fall back to the modern 128k window — the old
	// 8k made auto-compaction fire at ~5.7k tokens, far too aggressive for
	// 128k-class Groq models.
	if got := modelWindowFor("groq", "unknown-model"); got != 128_000 {
		t.Fatalf("expected groq fallback 128k, got %d", got)
	}
	// Known model families still win over the provider fallback.
	if got := modelWindowFor("groq", "llama-3.3-70b-versatile"); got != 128_000 {
		t.Fatalf("expected llama 128k, got %d", got)
	}
}

func TestContextPressureUsesWorseOfForecastAndReported(t *testing.T) {
	// contextPressure drives the compaction trigger and the ctx readout: the
	// WORSE of the local forecast and the provider's last reported input.
	// The 4-char/token forecast underestimates dense code/tool output, so a
	// session can sit at 150k real input in a 135k window while the forecast
	// still reads under the 70% trigger — the API number is settlement.
	m := newTestModel()
	m.window = 1000
	m.chat = buildBigChat(6)
	m.refreshCtx()
	forecast := m.ctxUsed

	// No settlement yet → pure forecast.
	if got := m.contextPressure(); got != forecast {
		t.Fatalf("expected pure forecast %d, got %d", forecast, got)
	}

	// Reported input HIGHER than the forecast (dense tool output) → the API
	// number is settlement and must drive the trigger.
	m.actualTokens = tokenUsage{input: forecast * 2}
	if got := m.contextPressure(); got != forecast*2 {
		t.Fatalf("expected reported input %d to win, got %d", forecast*2, got)
	}

	// Reported input LOWER than the forecast → the forecast wins (pressure
	// never dips below what the transcript visibly occupies).
	m.actualTokens = tokenUsage{input: 10}
	if got := m.contextPressure(); got != forecast {
		t.Fatalf("expected forecast %d to win over a tiny reported input, got %d", forecast, got)
	}
}

func TestCompactNoticeCarriesActualPressure(t *testing.T) {
	// The ledger notice must state the ACTUAL pressure that crossed the
	// threshold — the API-reported input when available, not just the local
	// forecast — so a session that really sat past its window shows
	// "(was 150k / 135k)" instead of a misleading optimistic forecast.
	m := newTestModel()
	m.window = 500
	m.chat = buildBigChat(12)
	m.refreshCtx()
	settled := m.ctxUsed + 1000 // API reported way past the window
	m.actualTokens = tokenUsage{input: settled}

	if !m.forceCompact() {
		t.Fatal("expected forced compaction to run")
	}
	wantWas := fmt.Sprintf("(was %s / %s)", fmtTokens(settled), fmtTokens(500))
	found := false
	for _, cm := range m.chat {
		if cm.role == roleSystem && strings.Contains(cm.text, "context compact") {
			found = true
			if !strings.Contains(cm.text, wantWas) {
				t.Fatalf("notice must carry the actual pressure %q, got:\n%s", wantWas, cm.text)
			}
		}
	}
	if !found {
		t.Fatal("expected a ledger notice message after compaction")
	}
}

func TestCompactCommandRunsVisibleProcess(t *testing.T) {
	// /compact must be a VISIBLE process (compacting flag → spinner + status)
	// that resolves through compactRunMsg — not a zero-frame blink.
	m := newTestModel()
	m.input.SetValue("/compact")
	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if cmd == nil {
		t.Fatal("expected a command (spinner + delayed compact)")
	}
	if !m2.compacting {
		t.Fatal("expected compacting=true so the spinner shows")
	}
	if !strings.Contains(m2.status, "compacting") {
		t.Fatalf("expected compacting status, got %q", m2.status)
	}

	// The delayed compactRunMsg applies the folding and clears the flag.
	m2.window = 500
	m2.chat = buildBigChat(12)
	updated, _ = m2.Update(compactRunMsg{})
	m3 := updated.(Model)
	if m3.compacting {
		t.Fatal("compacting flag must clear once the process resolves")
	}
	if m3.compactCount != 1 {
		t.Fatalf("expected compaction applied on compactRunMsg, got %d", m3.compactCount)
	}
}

func TestCompactRunMsgSkipsCleanContext(t *testing.T) {
	// When there is nothing worth folding (too few messages for even a forced
	// compact), compactRunMsg reports the headroom instead of fabricating a
	// fold. Note: /compact deliberately FORCES folding on a big-enough
	// transcript — the clean-context branch only fires when forceCompact
	// bails (below the forced minimum of 3 messages).
	m := newTestModel()
	m.window = 1_000_000
	m.chat = buildBigChat(2) // below the forced minimum
	m.compacting = true
	updated, _ := m.Update(compactRunMsg{})
	m2 := updated.(Model)
	if m2.compacting {
		t.Fatal("compacting flag must clear")
	}
	if m2.compactCount != 0 {
		t.Fatalf("no compaction may run on a clean context, got %d", m2.compactCount)
	}
	if !strings.Contains(m2.status, "no compaction needed") {
		t.Fatalf("expected a clean-context status, got %q", m2.status)
	}
}

func TestSendPreservesCompactionTraceOnUserMessage(t *testing.T) {
	// Regression: send() resets m.trace right after the agent work starts —
	// the compaction's "scanning → folding → reclaimed" process lines were
	// being wiped silently, so a compaction looked like it never happened.
	// They must be preserved on the user message that triggered the fold.
	m := newTestModel()
	m.window = 500
	m.chat = buildBigChat(12) // over the 70% trigger
	m.input.SetValue("gas")
	m2, cmd := m.send()
	if cmd == nil {
		t.Fatal("expected a command after send")
	}
	if m2.compactCount != 1 {
		t.Fatalf("expected auto-compaction to run on the over-trigger transcript, got %d", m2.compactCount)
	}
	found := false
	for _, cm := range m2.chat {
		if cm.role == roleUser && strings.Contains(cm.text, "gas") {
			joined := strings.Join(cm.trace, "\n")
			if !strings.Contains(joined, "Compaction →") {
				t.Fatalf("compaction process trace must survive on the user message, got %q", joined)
			}
			if !strings.Contains(joined, "Reclaimed") {
				t.Fatalf("reclaimed line must survive, got %q", joined)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected the triggering user message in the folded transcript")
	}
	// The live trace itself was reset for the new agent run — but the compact
	// log must not be double-shown there (it lives on the user message).
	if strings.Contains(strings.Join(m2.trace, "\n"), "Compaction →") {
		t.Fatal("compaction trace must not leak into the fresh live trace")
	}
}

func TestRenderCompactBlockDivider(t *testing.T) {
	// The L2 ledger renders as a centered ✂ Compaction divider block — the
	// same visual language Claude Code uses — never a plain "system:" line.
	m := newTestModel()
	cm := chatMsg{role: roleSystem, text: "🔄 context compact — 8 messages → L2 state ledger · saved 4.2k tokens (was 150.0k / 135.0k)\n📋 context summary (L2 ledger)\ngoal: build docs\n\nuser requests:\n  • build docs"}
	got := ansiStrip.ReplaceAllString(m.renderCompactBlock(cm, 60), "")
	if !strings.Contains(got, "✂ Compaction") {
		t.Fatalf("expected the ✂ divider marker, got:\n%s", got)
	}
	if !strings.Contains(got, "8 messages → L2 state ledger") {
		t.Fatalf("expected the process summary under the divider, got:\n%s", got)
	}
	if !strings.Contains(got, "goal: build docs") {
		t.Fatalf("expected the ledger body, got:\n%s", got)
	}
	if strings.Contains(got, "🔄 context compact — ") {
		t.Fatalf("the raw notice prefix must be cleaned, got:\n%s", got)
	}
	// The full render path must route the ledger into the divider block too.
	m.chat = appendChat(m.chat, cm)
	m.refreshChat()
	v := ansiStrip.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(v, "✂ Compaction") || strings.Contains(v, "system: 🔄 context compact") {
		t.Fatalf("renderChatMsg must route the ledger to the divider block:\n%s", v)
	}

	// Narrow terminal: no line may overflow the content width. The divider
	// must stay within w-2 (it is drawn inside the content margin); body and
	// summary lines are indented 4 and wrapped at w-4, so they may reach w.
	for _, w := range []int{20, 30, 40} {
		got := ansiStrip.ReplaceAllString(m.renderCompactBlock(cm, w), "")
		lines := strings.Split(got, "\n")
		if wd := lipgloss.Width(ansiStrip.ReplaceAllString(lines[0], "")); wd > w-2 {
			t.Fatalf("divider overflows at w=%d: %d cells (limit %d):\n%s", w, wd, w-2, got)
		}
		for _, ln := range lines[1:] {
			if wd := lipgloss.Width(ln); wd > w {
				t.Fatalf("compact block line overflows at w=%d: %d cells (limit %d):\n%s", w, wd, w, got)
			}
		}
	}
}

func TestSendClosesSupersededAnswerCh(t *testing.T) {
	// Regression: a mock worker blocked on `<-answerCh` (its question was
	// dropped as stale after an ESC) used to leak forever when a new send()
	// replaced the channels — nothing ever answered the old channel. send()
	// must CLOSE the superseded channel so the receive returns "" and the
	// worker exits (its tagged result is then dropped by the run guard).
	m := newTestModel()
	old := make(chan string, 1)
	m.answerCh = old
	m.input.SetValue("gas")
	m2, _ := m.send()
	if m2.answerCh == nil || m2.answerCh == old {
		t.Fatal("expected a fresh answerCh after send")
	}
	select {
	case _, ok := <-old:
		if ok {
			t.Fatal("old answerCh must be closed — a blocked worker would leak")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old answerCh was not closed — blocked worker would leak forever")
	}
	// The fresh channel must still accept the UI's answer (esc/submit).
	m2.answerCh <- "ok"
}

func TestCachedModelEntriesNeverFetches(t *testing.T) {
	// With an absent/stale cache, cachedModelEntries must return ONLY local
	// data (config providers + static opencode) — zero network. The /models
	// picker renders this immediately while modelsRefreshCmd fetches live
	// lists in the background.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestConfig(t, home, `{"provider":{"mygpu":{"name":"My GPU","options":{"baseURL":"http://x"},"models":{"llama-3-8b":{"name":"L","limit":{"context":128000}}}}}}`)

	got := cachedModelEntries()
	if len(got["mygpu"]) != 1 || got["mygpu"][0] != "llama-3-8b" {
		t.Fatalf("expected config provider models from settings, got %v", got)
	}
	if _, ok := got["opencode"]; !ok {
		t.Fatal("expected the static opencode fallback so the picker is never empty")
	}
	if !modelsCacheStale() {
		t.Fatal("absent cache must be reported stale (drives the async refresh)")
	}
}

func TestModelsOpenWithStaleCacheRendersWithoutNetwork(t *testing.T) {
	// The bug this pins: opening /models with a stale 24h cache used to fetch
	// every provider API SYNCHRONOUSLY inside the render path — freezing the
	// UI for up to tens of seconds. It must now open instantly (network-free
	// render) and schedule the refresh in the background.
	home := t.TempDir()
	t.Setenv("HOME", home) // no models_cache.json → stale
	m := newTestModel()
	m.input.SetValue("/models")
	updated, cmd := m.Update(enterKey())
	m2 := updated.(Model)
	if !m2.modelsOpen {
		t.Fatal("expected /models modal open")
	}
	if cmd == nil {
		t.Fatal("expected an async refresh command when the cache is stale")
	}
	// The picker renders static models immediately — no freeze, no network.
	v := ansiStrip.ReplaceAllString(m2.renderModelsModalBox(), "")
	if !strings.Contains(v, "deepseek-v4-flash-free") {
		t.Fatalf("expected static opencode models without network, got:\n%s", v)
	}
}

func TestPromptHistoryBounded(t *testing.T) {
	// ↑-navigation history must be capped: a marathon session used to keep
	// every prompt ever sent in memory (the chat is bounded at maxHistory,
	// but promptHistory grew forever). Beyond the cap the OLDEST drop off.
	m := newTestModel()
	for i := 0; i < maxPromptHistory+50; i++ {
		m.promptHistory = appendPromptHistory(m.promptHistory, fmt.Sprintf("prompt %d", i))
	}
	if len(m.promptHistory) != maxPromptHistory {
		t.Fatalf("expected %d entries, got %d", maxPromptHistory, len(m.promptHistory))
	}
	// The 50 oldest prompts dropped; the newest maxPromptHistory survive.
	if m.promptHistory[0] != "prompt 50" {
		t.Fatalf("expected the oldest prompts to drop, got %q", m.promptHistory[0])
	}
	if last := m.promptHistory[len(m.promptHistory)-1]; last != fmt.Sprintf("prompt %d", maxPromptHistory+49) {
		t.Fatalf("expected the newest prompt to survive, got %q", last)
	}
}

func TestSuggestSubagentCacheTTL(t *testing.T) {
	// loadDiscoveredSubagents used to walk both agent dirs + read every .md
	// on EVERY keystroke while the @ popup was up. The result is now cached
	// for a short TTL — the walk happens once, and a file change only shows
	// after the TTL (documented staleness window).
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	if err := os.MkdirAll(".brocode/agents", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".brocode/agents/planner.md", []byte("# Planner Agent\nRole: plans things\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	subagentCache = nil // reset the package-level cache for determinism

	got := loadDiscoveredSubagents()
	found := false
	for _, it := range got {
		if it.cmd == "@planner" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected @planner discovered from .brocode/agents, got %+v", got)
	}

	// Within the TTL the cache is warm: deleting the file must NOT change the
	// result (the popup is ephemeral — the next keystroke 10s later re-walks).
	if err := os.Remove(".brocode/agents/planner.md"); err != nil {
		t.Fatal(err)
	}
	got2 := loadDiscoveredSubagents()
	if len(got2) != len(got) {
		t.Fatalf("cache must stay warm within TTL: before=%d after=%d", len(got), len(got2))
	}
	// Reset so the warm cache cannot leak this temp-home agent into later tests.
	subagentCache = nil
}

func TestSaveSessionAccumulatesUniqueFiles(t *testing.T) {
	// Each SaveSession call must create a NEW session file — never overwrite
	// the previous one — so /history can list every past conversation.
	dir := t.TempDir()
	old := sessionRoot
	sessionRoot = func() (string, error) { return dir, nil }
	defer func() { sessionRoot = old }()

	for i := 0; i < 3; i++ {
		if _, err := SaveSession([]chatMsg{{role: roleUser, text: fmt.Sprintf("msg %d", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	files := sessionFilesIn(dir)
	if len(files) != 3 {
		t.Fatalf("expected 3 accumulated session files, got %d", len(files))
	}
	names := map[string]bool{}
	for _, f := range files {
		if names[f.name] {
			t.Fatalf("duplicate session file name %s — a save overwrote a previous session", f.name)
		}
		names[f.name] = true
	}
}

func TestSaveSessionPrunesOldest(t *testing.T) {
	// Retention cap: after maxSessionFiles+5 saves only the 20 newest remain
	// and the oldest 5 are gone. Explicit mtimes make the ordering
	// deterministic regardless of filesystem timestamp resolution.
	dir := t.TempDir()
	old := sessionRoot
	sessionRoot = func() (string, error) { return dir, nil }
	defer func() { sessionRoot = old }()

	const total = maxSessionFiles + 5
	// All saved files get strictly increasing PAST mtimes so the ordering is
	// deterministic and the just-written file is always the newest at prune
	// time (a now-based scheme would give the future mtimes that let a
	// just-written file sort as "oldest" and get pruned right after writing).
	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < total; i++ {
		p, err := SaveSession([]chatMsg{{role: roleUser, text: fmt.Sprintf("msg %d", i)}})
		if err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	files := sessionFilesIn(dir)
	if len(files) != maxSessionFiles {
		t.Fatalf("expected %d files after pruning, got %d", maxSessionFiles, len(files))
	}
	seen := map[string]bool{}
	for _, f := range files {
		msgs, err := loadSessionFrom(f.path)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("failed loading %s: %v", f.path, err)
		}
		seen[msgs[0].text] = true
	}
	for i := 0; i < 5; i++ {
		if seen[fmt.Sprintf("msg %d", i)] {
			t.Fatalf("old session msg %d should have been pruned", i)
		}
	}
	for i := 5; i < total; i++ {
		if !seen[fmt.Sprintf("msg %d", i)] {
			t.Fatalf("recent session msg %d missing after prune", i)
		}
	}
}

func TestLoadSessionPicksNewest(t *testing.T) {
	// -c resume must continue the MOST RECENT conversation, not an older one.
	dir := t.TempDir()
	old := sessionRoot
	sessionRoot = func() (string, error) { return dir, nil }
	defer func() { sessionRoot = old }()

	p, err := SaveSession([]chatMsg{{role: roleUser, text: "older session"}})
	if err != nil {
		t.Fatal(err)
	}
	// Force a clearly older mtime so ordering cannot tie on clock resolution.
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveSession([]chatMsg{{role: roleUser, text: "newer session"}}); err != nil {
		t.Fatal(err)
	}

	msgs, err := LoadSession()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].text != "newer session" {
		t.Fatalf("expected newest session to resume, got %+v", msgs)
	}
}

// ---- interactive ask popover + native permission gate ----------------------

func TestParseAskBlockMultiQuestion(t *testing.T) {
	reply := "Let me clarify.\n\n<tool_call>ask\n<ask_question header=\"Auth method\">Select the auth method\n- JWT\n- Session cookies\n- OAuth2\n</ask_question>\n<ask_question header=\"Scope\" multi=\"true\">Which areas?\n- Admin panel\n- Public API\n</ask_question>\n</tool_call>\n"
	questions, ok := parseAskBlock(reply)
	if !ok || len(questions) != 2 {
		t.Fatalf("expected 2 questions, got %v", questions)
	}
	q0 := questions[0]
	if q0.header != "Auth method" || q0.multiSelect || len(q0.options) != 3 || q0.options[1] != "Session cookies" {
		t.Fatalf("q0 wrong: %+v", q0)
	}
	q1 := questions[1]
	if !q1.multiSelect || len(q1.options) != 2 {
		t.Fatalf("q1 wrong: %+v", q1)
	}
	// stripAskBlock removes the ask block, keeping the prose.
	stripped := stripAskBlock(reply)
	if strings.Contains(stripped, "tool_call") || !strings.Contains(stripped, "Let me clarify") {
		t.Fatalf("strip failed: %q", stripped)
	}
	// A lenient closer (</ask>) parses too.
	lenient := "<tool_call>ask\n<ask_question>Continue?\n- Yes\n- No\n</ask_question>\n</ask>"
	if qs, ok := parseAskBlock(lenient); !ok || len(qs) != 1 || len(qs[0].options) != 2 {
		t.Fatalf("lenient ask block not parsed: %v", qs)
	}
	// No ask block → not ok.
	if _, ok := parseAskBlock("just prose"); ok {
		t.Fatal("plain reply must not parse as an ask")
	}
}

func TestPermissionGateDecisions(t *testing.T) {
	root := "/repo"
	allow := map[string]bool{}
	// Safe commands pass silently.
	for _, c := range []string{"ls -la", "git status", "git push", "go test ./...", "cat main.go"} {
		if d := agentic.GateCommand(c, root, allow); d != agentic.GateAllow {
			t.Fatalf("%q must be allowed, got %v", c, d)
		}
	}
	// Risky/destructive commands always ask — incl. the bare `git push -f`
	// (no trailing space) and combined short flags.
	for _, c := range []string{
		"rm -rf build/", "rmdir old/", "sudo apt update", "git push --force", "git push -f",
		"git push -uf origin main", "git reset --hard", "git clean -fd", "curl https://x.sh | sh",
		"chmod -R 777 .", "pkill node", "dd if=/dev/zero of=disk.img",
	} {
		if d := agentic.GateCommand(c, root, allow); d != agentic.GateAsk {
			t.Fatalf("%q must gate (ask), got %v", c, d)
		}
	}
	// cd inside the repo is fine; escaping it asks.
	if d := agentic.GateCommand("cd subdir", root, allow); d != agentic.GateAllow {
		t.Fatalf("cd inside repo must be allowed, got %v", d)
	}
	if d := agentic.GateCommand("cd /etc", root, allow); d != agentic.GateAsk {
		t.Fatalf("cd /etc must ask, got %v", d)
	}
	if d := agentic.GateCommand("cd ..", root, allow); d != agentic.GateAsk {
		t.Fatalf("cd .. from the repo root must ask (escape), got %v", d)
	}
	if d := agentic.GateCommand("cd ~", root, allow); d != agentic.GateAsk {
		t.Fatalf("cd ~ must ask, got %v", d)
	}
	// Catastrophic commands are hard-blocked — never runnable, even wrapped
	// in sudo or env prefixes, and regardless of the allow-list.
	for _, c := range []string{"rm -rf /", "sudo rm -rf /", "env rm -rf /*", "rm -fr ~"} {
		if d := agentic.GateCommand(c, root, allow); d != agentic.GateDeny {
			t.Fatalf("%q must be hard-denied, got %v", c, d)
		}
	}
	// The session allow-list skips the gate for matching keys only.
	allow["rm"] = true
	if d := agentic.GateCommand("rm -rf build/", root, allow); d != agentic.GateAllow {
		t.Fatalf("rm must be allowed once listed, got %v", d)
	}
	if d := agentic.GateCommand("sudo apt update", root, allow); d != agentic.GateAsk {
		t.Fatalf("sudo must still gate, got %v", d)
	}
	if d := agentic.GateCommand("rm -rf /", root, allow); d != agentic.GateDeny {
		t.Fatalf("rm -rf / stays hard-denied even when allowed, got %v", d)
	}
}

func TestPermissionFlowDeny(t *testing.T) {
	m := newTestModel()
	m.started = true
	replyText := "```bash\nrm -rf build/\n```\n```bash\nls -la\n```"
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: replyText})
	m.agentRun = 3
	m.openPermission([]string{"rm -rf build/"}, nil, replyText)
	m.askRadio[0] = 2 // deny

	m2, cmd := m.submitPermission()
	if m2.askOpen {
		t.Fatal("permission popover must close on submit")
	}
	if cmd == nil {
		t.Fatal("expected a tool-run command")
	}
	if m2.allowList["rm"] {
		t.Fatal("deny must not seed the allow-list")
	}
	// The execution skips the denied command and reports it; the safe one
	// still runs (applyAgenticToolsDeny directly — never run rm for real;
	// launchToolRun compacts the shared chat slice, so use the captured
	// replyText, not m.chat[0].text).
	logs, feedback := applyAgenticToolsDeny(replyText, map[string]bool{"rm": true}, false, nil)
	if !strings.Contains(feedback, "User denied") || !strings.Contains(feedback, "rm -rf build/") {
		t.Fatalf("expected deny feedback, got: %q", feedback)
	}
	if !strings.Contains(feedback, "ls -la") {
		t.Fatalf("safe command must still run, got: %q", feedback)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "⛔ User denied: rm -rf build/") {
		t.Fatalf("expected a deny log line, got %v", logs)
	}
}

func TestPermissionFlowAlwaysAllowSeedsList(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "```bash\nrm -rf build/\n```"})
	m.agentRun = 3
	m.openPermission([]string{"rm -rf build/"}, nil, m.chat[0].text)
	m.askRadio[0] = 1 // always allow

	m2, _ := m.submitPermission()
	if !m2.allowList["rm"] {
		t.Fatal("always allow must seed the session allow-list")
	}
	// A later reply with the same risky command no longer gates.
	gated, hard := m2.gatedCommands("```bash\nrm -rf build/\n```")
	if len(gated)+len(hard) != 0 {
		t.Fatalf("allowed command must not gate again: gated=%v hard=%v", gated, hard)
	}
	// …but a DIFFERENT risky command still gates.
	gated, _ = m2.gatedCommands("```bash\nsudo rm -rf build/\n```")
	if len(gated) != 1 {
		t.Fatalf("sudo rm must still gate: %v", gated)
	}
}

func TestPermissionEscDenies(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "```bash\nrm -rf build/\n```"})
	m.agentRun = 3
	m.openPermission([]string{"rm -rf build/"}, nil, m.chat[0].text)

	updated, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m2 := updated.(Model)
	if m2.askOpen {
		t.Fatal("permission popover must close on esc")
	}
	if m2.agentAborted {
		t.Fatal("permission esc = deny, not abort")
	}
	if cmd == nil {
		t.Fatal("expected the safe-commands run after deny")
	}
}

func TestInputHiddenWhileAskOpen(t *testing.T) {
	m := newTestModel()
	m.openAsk("", []askQuestion{{question: "Q?", options: []string{"A"}}}, "")
	v := ansiStrip.ReplaceAllString(m.View().Content, "")
	// The ❯ chat input must NOT render while the popover is up — the
	// popover replaces it.
	if strings.Contains(v, "❯") {
		t.Fatalf("chat input must be hidden while the ask popover is open:\n%s", v)
	}
	if !strings.Contains(v, "agent needs your input") {
		t.Fatalf("expected the ask hint in the bottom bar:\n%s", v)
	}
}

func TestAskToolPathQueuesAnswers(t *testing.T) {
	m := newTestModel()
	m.started = true
	m.agentRun = 5
	reply := "Let me ask.\n\n<tool_call>ask\n<ask_question header=\"Go\">Continue?\n- Yes\n- No\n</ask_question>\n</tool_call>\n"
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: reply})
	questions, _ := parseAskBlock(reply)
	m.openAsk("💬 agent needs your input", questions, stripAskBlock(reply))

	// Select "Yes" (focus 0) and submit — no tools in the reply, so the
	// answers are queued straight to the model as a transient system row.
	m.toggleAskFocus()
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if m2.askOpen {
		t.Fatal("popover must close on submit")
	}
	if len(m2.chat) == 0 || m2.chat[len(m2.chat)-1].role != roleTool {
		t.Fatalf("expected ask result sent as a transient tool row, got %+v", m2.chat)
	}
	last := m2.chat[len(m2.chat)-1]
	if !strings.Contains(last.content, "Go: Yes") {
		t.Fatalf("expected serialized answer in the sent row, got %q", last.content)
	}
	if last.summary != "User Answered" {
		t.Fatalf("expected 'User Answered' summary, got %q", last.summary)
	}
}

func TestAskBlockSkipsToolIndicators(t *testing.T) {
	reply := "<tool_call>ask\n<ask_question>Continue?\n- Yes\n- No\n</ask_question>\n</tool_call>\n\n```bash\nls\n```"
	// The ask block must not emit a "Running tool: ask" indicator; the bash
	// fence still does.
	ind := toolBlockCommands(reply)
	for _, line := range ind {
		if strings.Contains(line, "Running tool: ask") {
			t.Fatalf("ask block must not produce a tool indicator: %v", ind)
		}
	}
	if len(ind) != 1 || !strings.Contains(ind[0], "ls") {
		t.Fatalf("expected only the bash indicator, got %v", ind)
	}
}

func TestExtractToolEcho(t *testing.T) {
	// Weak models (poolside etc.) repeat the [SYSTEM TOOL RESULT] payload
	// verbatim in their reply text. extractToolEcho must peel those echoed
	// blocks off the prose so they can fold into the dim collapsible block.
	text := "Terima kasih. Saya lihat strukturnya:\n\n[SYSTEM TOOL RESULT]:\n  ```js\n  const { DataTypes } = require('sequelize');\n  module.exports = X;\n  ```\n\nSaya lanjut mengecek model lain.\n"
	prose, echo, n := extractToolEcho(text)
	if n != 1 {
		t.Fatalf("expected 1 echoed block, got %d", n)
	}
	if !strings.Contains(prose, "Terima kasih") || !strings.Contains(prose, "Saya lanjut") {
		t.Fatalf("prose must survive, got %q", prose)
	}
	if strings.Contains(prose, "SYSTEM TOOL RESULT") || strings.Contains(prose, "DataTypes") {
		t.Fatalf("echoed block leaked into prose: %q", prose)
	}
	if !strings.Contains(echo, "SYSTEM TOOL RESULT") || !strings.Contains(echo, "DataTypes") {
		t.Fatalf("echoed block missing, got %q", echo)
	}

	// An indented payload without a fence is still an echo.
	prose, echo, n = extractToolEcho("Hasil:\n[SYSTEM TOOL RESULT]\n  exit 0\n  file1\n")
	if n != 1 || !strings.Contains(echo, "file1") {
		t.Fatalf("indented payload must be echoed (n=%d, echo=%q)", n, echo)
	}

	// A bare marker with no payload stays prose (not an echo).
	prose, echo, n = extractToolEcho("Saya menerima [SYSTEM TOOL RESULT] dan akan memprosesnya.\n")
	if n != 0 || !strings.Contains(prose, "SYSTEM TOOL RESULT") {
		t.Fatalf("bare marker must stay prose (n=%d, prose=%q)", n, prose)
	}

	// Multiple echoed blocks are all extracted.
	text = "A\n[SYSTEM TOOL RESULT]:\n  ```\n  one\n  ```\nB\n[SYSTEM TOOL RESULT]:\n  ```\n  two\n  ```\nC\n"
	prose, echo, n = extractToolEcho(text)
	if n != 2 || !strings.Contains(echo, "one") || !strings.Contains(echo, "two") {
		t.Fatalf("both echoes must be extracted (n=%d, echo=%q)", n, echo)
	}
	if !strings.Contains(prose, "A") || !strings.Contains(prose, "B") || !strings.Contains(prose, "C") {
		t.Fatalf("interleaved prose must survive, got %q", prose)
	}

	// The attribution footer ("⚡ provider/model · time · tokens") sits right
	// after the echoed block — it is indented like echo payload but is
	// brocode's own metadata (the model-identity chain parses it from the
	// reply text). It must stay in prose, never fold into the echo.
	text = "Terima kasih.\n[SYSTEM TOOL RESULT]:\n  ```js\n  const d = 1;\n  ```\n\n  ⚡ poolside/poolside/laguna-s-2.1 · 16.6s · 3.3k tokens"
	prose, echo, n = extractToolEcho(text)
	if n != 1 {
		t.Fatalf("expected 1 echo with a footer present, got %d", n)
	}
	if !strings.Contains(prose, "⚡ poolside/poolside/laguna-s-2.1") {
		t.Fatalf("attribution footer must survive in prose, got %q", prose)
	}
	if strings.Contains(echo, "⚡ poolside") {
		t.Fatalf("footer must not fold into the echo block, got %q", echo)
	}

	// Plain prose without any marker is untouched.
	prose, echo, n = extractToolEcho("just a normal answer")
	if n != 0 || prose != "just a normal answer" || echo != "" {
		t.Fatalf("plain prose must pass through (n=%d, prose=%q)", n, prose)
	}
}

func TestReplyEchoFoldsIntoCollapsibleBlock(t *testing.T) {
	// A finished reply whose text echoes the tool payload must swap its
	// DISPLAY text for the prose and fold the echo into a collapsed block —
	// the reply never sits in the transcript as a white wall of output.
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "cek project"})
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: ""})
	full := "Terima kasih atas hasilnya.\n[SYSTEM TOOL RESULT]:\n  ```js\n  const x = 1;\n  ```\nSelesai."
	display, folded := m.finalizeReplyDisplay(full)
	if !folded {
		t.Fatal("echo must be folded")
	}
	cm := m.chat[len(m.chat)-1]
	if !cm.collapsed || !cm.collapsible() {
		t.Fatalf("expected collapsed collapsible block, got %+v", cm)
	}
	if !strings.Contains(cm.summary, "echoed tool output") {
		t.Fatalf("expected echo summary, got %q", cm.summary)
	}
	if !strings.Contains(cm.content, "const x = 1") {
		t.Fatalf("echo content missing, got %q", cm.content)
	}
	if display != "Terima kasih atas hasilnya.\nSelesai." && !strings.Contains(display, "Selesai.") {
		t.Fatalf("display text must be prose-only, got %q", display)
	}
	if strings.Contains(display, "const x = 1") || strings.Contains(display, "SYSTEM TOOL RESULT") {
		t.Fatalf("echo leaked into display text, got %q", display)
	}

	// A second call (ask/permission re-enters via launchToolRun) must not
	// double-merge the same echo into the block content.
	display2, folded2 := m.finalizeReplyDisplay(full)
	if !folded2 {
		t.Fatal("second fold must still report folded")
	}
	if strings.Count(m.chat[len(m.chat)-1].content, "const x = 1") != 1 {
		t.Fatalf("echo must not be double-merged, got %q", m.chat[len(m.chat)-1].content)
	}
	_ = display2

	// When the reply ALSO carries a thinking-trace collapse (non-echo block),
	// the echo merges into its content exactly once — and the footer stays in
	// the display text.
	m3 := newTestModel()
	m3.started = true
	m3.chat = appendChat(m3.chat, chatMsg{role: roleAgent, text: "", summary: "▶ thinking trace", content: "trace body", collapsed: true})
	full2 := "Hasil:\n[SYSTEM TOOL RESULT]:\n  ```\n  out2\n  ```\n\n  ⚡ poolside/poolside/laguna-s-2.1 · 16.6s · 3.3k tokens"
	d3, folded3 := m3.finalizeReplyDisplay(full2)
	if !folded3 {
		t.Fatal("echo must be folded onto a trace block")
	}
	if strings.Count(m3.chat[len(m3.chat)-1].content, "out2") != 1 {
		t.Fatalf("echo must merge into the trace exactly once, got %q", m3.chat[len(m3.chat)-1].content)
	}
	if !strings.Contains(m3.chat[len(m3.chat)-1].content, "trace body") {
		t.Fatalf("existing trace content must survive the merge, got %q", m3.chat[len(m3.chat)-1].content)
	}
	// Re-entry must not double-merge onto the trace.
	_, _ = m3.finalizeReplyDisplay(full2)
	if strings.Count(m3.chat[len(m3.chat)-1].content, "out2") != 1 {
		t.Fatalf("echo must not double-merge onto a trace, got %q", m3.chat[len(m3.chat)-1].content)
	}
	// The footer survives in the display prose (review gate: model-identity
	// chain parses it from the reply text).
	if !strings.Contains(d3, "⚡ poolside/poolside/laguna-s-2.1") {
		t.Fatalf("attribution footer must survive in display, got %q", d3)
	}
	if strings.Contains(d3, "out2") {
		t.Fatalf("echo content must not leak into display, got %q", d3)
	}

	// A reply with no echo is untouched (folded=false, display=full text).
	m2 := newTestModel()
	m2.started = true
	m2.chat = appendChat(m2.chat, chatMsg{role: roleAgent, text: ""})
	plain := "just an answer"
	d, f := m2.finalizeReplyDisplay(plain)
	if f || d != plain {
		t.Fatalf("plain reply must pass through (folded=%v, display=%q)", f, d)
	}
}

func TestEchoBlockRendersDimNotWhite(t *testing.T) {
	// Collapsed: the summary line renders as a dim ▸ row (same as every
	// collapsible) — never a full white block.
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "prose", summary: "⚙ 2 echoed tool output(s) · ctrl+o to expand", content: "[SYSTEM TOOL RESULT]:\n  ```\n  const payloadD = 1;\n  ```", collapsed: true})
	m.refreshChat()
	v := m.View().Content
	if !strings.Contains(v, "echoed tool output") {
		t.Fatalf("collapsed echo summary missing:\n%s", v)
	}
	if strings.Contains(v, "payloadD") {
		t.Fatalf("collapsed echo content must be hidden:\n%s", v)
	}

	// Expanded via ctrl+o: content renders labeled "tool output" (dim) and
	// the internal [SYSTEM TOOL RESULT] transport prefix is stripped.
	updated, _ := m.Update(ctrlOKey())
	m2 := updated.(Model)
	v = m2.View().Content
	if !strings.Contains(v, "tool output") || !strings.Contains(v, "payloadD") {
		t.Fatalf("expanded echo must render dim tool output:\n%s", v)
	}
	if strings.Contains(v, "[SYSTEM TOOL RESULT]") {
		t.Fatalf("transport prefix leaked into expanded echo:\n%s", v)
	}

	// isEchoContent catches both TOOL and ASK payloads.
	if !isEchoContent("[SYSTEM TOOL RESULT]:\nx") || !isEchoContent("[SYSTEM ASK RESULT]\nx") {
		t.Fatal("isEchoContent must recognize both transport prefixes")
	}
	if isEchoContent("thinking trace") {
		t.Fatal("isEchoContent must reject non-echo content")
	}

	// stripToolResultPrefix removes both prefixes.
	if got := stripToolResultPrefix("[SYSTEM TOOL RESULT]\nexit 0"); got != "exit 0" {
		t.Fatalf("tool prefix not stripped, got %q", got)
	}
	if got := stripToolResultPrefix("[SYSTEM ASK RESULT]\nGo: Yes"); got != "Go: Yes" {
		t.Fatalf("ask prefix not stripped, got %q", got)
	}
}

func TestNoToolReplyCompactsEchoInStream(t *testing.T) {
	// End-to-end: a reply with NO tool blocks but an echoed tool payload must
	// complete streaming with prose-only display + a collapsed echo block.
	m := newTestModel()
	m.started = true
	m.streaming = true
	reply := mockReply{text: "Terima kasih.\n[SYSTEM TOOL RESULT]:\n  ```js\n  const d = 1;\n  ```\nSelesai."}
	updated, _ := m.Update(agentResultMsg{reply: reply, run: m.agentRun})
	m2 := updated.(Model)
	for i := 0; i < 500 && m2.streaming; i++ {
		next, _ := m2.Update(streamTickMsg{})
		m2 = next.(Model)
	}
	if m2.streaming {
		t.Fatal("stream never completed")
	}
	last := m2.chat[len(m2.chat)-1]
	if !last.collapsed || !strings.Contains(last.summary, "echoed tool output") {
		t.Fatalf("expected folded echo block after stream, got %+v", last)
	}
	if strings.Contains(last.text, "const d = 1") || strings.Contains(last.text, "SYSTEM TOOL RESULT") {
		t.Fatalf("echo must not remain in the reply text, got %q", last.text)
	}
	if !strings.Contains(last.text, "Terima kasih.") || !strings.Contains(last.text, "Selesai.") {
		t.Fatalf("prose must remain in the reply, got %q", last.text)
	}
}

func TestApplyBuilderCodeBlocksReturnsFullDiffs(t *testing.T) {
	// applyBuilderCodeBlocks must return BOTH the short trace log AND the
	// FULL per-file unified diff body (header lines removed) so the TUI can
	// show a collapsible green/red change set instead of a truncated preview.
	dir := t.TempDir()
	f := filepath.Join(dir, "main.go")
	if err := os.WriteFile(f, []byte("package main\n\nfunc main() {\n\tprintln(\"old\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	// Pattern 2: ```go:main.go block (existing file → real unified diff).
	code := "package main\n\nfunc main() {\n\tprintln(\"new\")\n\tprintln(\"extra line\")\n}\n"
	logs, edits := applyBuilderCodeBlocks("```go:main.go\n"+code+"```", "", false)
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d (logs=%v)", len(edits), logs)
	}
	e := edits[0]
	if e.file != "main.go" {
		t.Fatalf("expected main.go, got %q", e.file)
	}
	if e.lines != 6 {
		t.Fatalf("expected 6 lines, got %d", e.lines)
	}
	// Full diff body: contains the added and removed lines.
	if !strings.Contains(e.diff, "+\tprintln(\"new\")") || !strings.Contains(e.diff, "+\tprintln(\"extra line\")") {
		t.Fatalf("full diff missing additions, got %q", e.diff)
	}
	if !strings.Contains(e.diff, "-\tprintln(\"old\")") {
		t.Fatalf("full diff missing deletion, got %q", e.diff)
	}
	// No ---/+++ header lines in the body.
	if strings.Contains(e.diff, "--- ") || strings.Contains(e.diff, "+++ ") {
		t.Fatalf("diff body must not carry header lines, got %q", e.diff)
	}
	// The trace log stays short (bounded preview with the ctrl+o hint). Note
	// the Risk Engine may prepend its own snapshot line for high-risk files,
	// so search for the edit entry rather than assuming it sits at index 0.
	foundEdit := false
	for _, l := range logs {
		if strings.Contains(l, "● Edit(main.go)") {
			foundEdit = true
		}
	}
	if !foundEdit {
		t.Fatalf("expected an edit trace log, got %v", logs)
	}

	// New file → full diff is every line as an addition.
	logs, edits = applyBuilderCodeBlocks("```go:newfile.go\npackage newfile\n\nfunc New() {}\n```", "", false)
	if len(edits) != 1 {
		t.Fatalf("expected 1 new-file edit, got %d", len(edits))
	}
	if !strings.Contains(edits[0].diff, "+  package newfile") || !strings.Contains(edits[0].diff, "+  func New() {}") {
		t.Fatalf("new-file full diff must list every line as addition, got %q", edits[0].diff)
	}
}

func TestEditBlockSpansFindsAllKinds(t *testing.T) {
	// The streaming interleave must recognize every file-writing block kind:
	// ```lang:path fence, SEARCH/REPLACE block, and cat heredoc — in order.
	text := strings.Join([]string{
		"intro",
		"",
		"```js:a.js",
		"const x = 1",
		"```",
		"",
		"mid",
		"",
		"<<<<<<< SEARCH: b.go",
		"old",
		"=======",
		"new",
		">>>>>>> REPLACE",
		"",
		"cat > c.txt << 'EOF'",
		"content",
		"EOF",
	}, "\n")
	spans := editBlockSpans(text)
	if len(spans) != 3 {
		t.Fatalf("expected 3 edit block spans, got %d: %v", len(spans), spans)
	}
	for i := 1; i < len(spans); i++ {
		if spans[i][0] <= spans[i-1][0] {
			t.Fatalf("spans must be sorted by start, got %v", spans)
		}
	}
	for _, sp := range spans {
		if sp[0] < 0 || sp[1] > len(text) || sp[1] <= sp[0] {
			t.Fatalf("invalid span %v in text of %d bytes", sp, len(text))
		}
	}
	// Each span must be a self-contained block applyBuilderCodeBlocks accepts.
	if logs, edits := applyBuilderCodeBlocks(strings.Join([]string{
		"```js:a.js",
		"const x = 1",
		"```",
	}, "\n"), "", false); len(logs) != 1 || len(edits) != 1 {
		t.Fatalf("fence span must apply standalone, logs=%v edits=%v", logs, edits)
	}
}

func TestStripEditSpansRemovesApplied(t *testing.T) {
	text := "A\n```go:x.go\ncode\n```\nB"
	spans := editBlockSpans(text)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %v", spans)
	}
	clean := stripEditSpans(text, spans)
	if clean != "A\nB" {
		t.Fatalf("stripped text must join the surrounding prose, got %q", clean)
	}
	// No spans → unchanged.
	if stripEditSpans(text, nil) != text {
		t.Fatal("nil spans must return the text unchanged")
	}
}

func TestAppendEditCardsStructuredRows(t *testing.T) {
	// Residual edits (applied by the end-of-reply sweep) append as structured
	// roleTool rows AFTER the reply — never folded on top of the agent text.
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "done"})
	edits := []editChange{
		{file: "a.go", lines: 10, diff: "+\tnew line\n-\told line\n  \tcontext"},
	}
	m.appendEditCards(edits)
	if len(m.chat) != 2 || m.chat[1].role != roleTool {
		t.Fatalf("edit card must append as roleTool row, got %+v", m.chat)
	}
	cm := m.chat[1]
	if !cm.collapsed || !strings.Contains(cm.summary, "Edit(a.go)") {
		t.Fatalf("edit card summary wrong: %+v", cm)
	}
	if !strings.Contains(cm.content, "✎ a.go · 10 lines") || !strings.Contains(cm.content, "new line") {
		t.Fatalf("edit card content wrong: %q", cm.content)
	}
}

func TestInterleavedEditStreamsInline(t *testing.T) {
	// A reply containing a ```lang:path block must apply the file DURING
	// streaming and split the message into [prose, ✎ edit card, tail] — the
	// edit appears inline where the model emitted it, not as a summary folded
	// on top after the reply finishes.
	work := t.TempDir()
	t.Chdir(work)
	if err := os.MkdirAll("src", 0o755); err != nil {
		t.Fatal(err)
	}

	m := newTestModel()
	m.started = true
	file := "src/main.go"
	replyText := strings.Join([]string{
		"Saya akan perbaiki:",
		"",
		"```go:" + file,
		"package main",
		"",
		"func main() {}",
		"```",
		"",
		"Selesai.",
	}, "\n")

	updated, _ := m.Update(agentResultMsg{reply: mockReply{text: replyText}, run: m.agentRun})
	m2 := updated.(Model)
	for i := 0; i < 500 && m2.streaming; i++ {
		next, _ := m2.Update(streamTickMsg{})
		m2 = next.(Model)
	}
	if m2.streaming {
		t.Fatal("stream never completed within 500 ticks")
	}

	// The file must be written — interleaved, i.e. before the stream ended.
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("file must be written during streaming: %v", err)
	}

	foundProse, foundCard, foundTail := false, false, false
	for _, cm := range m2.chat {
		switch {
		case cm.role == roleAgent && strings.Contains(cm.text, "Saya akan perbaiki"):
			foundProse = true
			if strings.Contains(cm.text, "package main") {
				t.Fatalf("raw edit block must be split out of the prose, got %q", cm.text)
			}
		case cm.role == roleTool && strings.Contains(cm.summary, "Edit("+file+")"):
			foundCard = true
			if !cm.collapsed || cm.content == "" {
				t.Fatalf("edit card must be collapsed with diff content, got %+v", cm)
			}
		case cm.role == roleAgent && strings.Contains(cm.text, "Selesai."):
			foundTail = true
		}
	}
	if !foundProse || !foundCard || !foundTail {
		t.Fatalf("expected interleaved [prose, edit card, tail], got chat roles: %v", chatRoles(m2.chat))
	}
}

func TestInterleavedToolStreamsInlineSafe(t *testing.T) {
	// A reply carrying a ```bash block must split it into an inline ⚙ card at
	// the position the model emitted it (prose → ⚙ card → tail), and the
	// safe command must launch EARLY (agentResultMsg) — real-time execution,
	// not a batch that pops in after the reply finishes.
	work := t.TempDir()
	t.Chdir(work)

	m := newTestModel()
	m.started = true
	replyText := strings.Join([]string{
		"Cek dulu strukturnya:",
		"",
		"```bash",
		"ls -la",
		"```",
		"",
		"Selesai.",
	}, "\n")

	updated, _ := m.Update(agentResultMsg{reply: mockReply{text: replyText}, run: m.agentRun})
	m2 := updated.(Model)
	if !m2.toolLaunched {
		t.Fatal("safe command must launch early at agentResultMsg")
	}
	if !m2.toolRunning {
		t.Fatal("toolRunning must be set by the early launch")
	}
	if m2.pendingPermissionText != "" {
		t.Fatalf("no gated commands → pendingPermissionText must be empty, got %q", m2.pendingPermissionText)
	}

	// Reveal the full reply: the tool block must split into a card INLINE.
	for i := 0; i < 500 && m2.streaming; i++ {
		next, _ := m2.Update(streamTickMsg{})
		m2 = next.(Model)
	}
	if m2.streaming {
		t.Fatal("stream never completed within 500 ticks")
	}

	foundProse, foundCard, foundTail := false, false, false
	for _, cm := range m2.chat {
		switch {
		case cm.role == roleAgent && strings.Contains(cm.text, "Cek dulu strukturnya"):
			foundProse = true
			if strings.Contains(cm.text, "ls -la") {
				t.Fatalf("raw tool block must be split out of the prose, got %q", cm.text)
			}
		case cm.role == roleTool && strings.Contains(cm.summary, "bash") && strings.Contains(cm.summary, "ls -la"):
			foundCard = true
			if !cm.collapsed || cm.content == "" {
				t.Fatalf("tool card must be collapsed with command content, got %+v", cm)
			}
			if strings.Contains(cm.summary, "awaiting approval") {
				t.Fatalf("safe command must not wait for approval, got %q", cm.summary)
			}
		case cm.role == roleAgent && strings.Contains(cm.text, "Selesai."):
			foundTail = true
		}
	}
	if !foundProse || !foundCard || !foundTail {
		t.Fatalf("expected interleaved [prose, tool card, tail], got chat roles: %v", chatRoles(m2.chat))
	}
}

func TestEarlyLaunchCmdIsReturnedNotDropped(t *testing.T) {
	// REGRESSION: agentResultMsg used to call launchEarlyTools() and DISCARD
	// the returned tea.Cmd. The early-launch cmd IS the tool execution
	// (runAgenticToolsCmdDeny in the background) — dropping it left
	// toolRunning=true forever: the "⚙ executing tool commands…" spinner
	// spun with no agentToolResultMsg ever arriving (the end-of-reply path
	// skips launchToolRun because toolLaunched is already set). The returned
	// cmd must actually execute the tool and deliver a result message.
	work := t.TempDir()
	t.Chdir(work)
	if err := os.WriteFile("hello.txt", []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestModel()
	m.started = true
	replyText := strings.Join([]string{
		"Baca dulu:",
		"",
		"<tool_call>read",
		"<arg_key>path</arg_key><arg_value>hello.txt</arg_value>",
		"</tool_call>",
	}, "\n")

	updated, cmd := m.Update(agentResultMsg{reply: mockReply{text: replyText}, run: m.agentRun})
	m2 := updated.(Model)
	if !m2.toolLaunched {
		t.Fatal("safe tool must launch early at agentResultMsg")
	}
	if !m2.toolRunning {
		t.Fatal("toolRunning must be set by the early launch")
	}
	if cmd == nil {
		t.Fatal("the early-launch cmd must be RETURNED — dropping it leaves the tool spinner stuck forever")
	}

	// Executing the returned cmd must produce the tool result (not just the
	// stream tick) — this is what feeds the agent loop back. tea.Batch wraps
	// the sub-commands in a BatchMsg, so unwrap it and run each one.
	// Executing the returned cmd must produce the tool result (not just the
	// stream tick) — this is what feeds the agent loop back. tea.Batch nests:
	// cmd() → BatchMsg[streamTick, earlyCmd], earlyCmd() → BatchMsg[spinner,
	// toolRun]. Unwrap recursively.
	var toolResult, streamTick bool
	var walk func(tea.Cmd, int)
	walk = func(c tea.Cmd, depth int) {
		if c == nil || depth > 4 {
			return
		}
		msg := c()
		if msg == nil {
			return
		}
		switch m := msg.(type) {
		case tea.BatchMsg:
			for _, sub := range m {
				walk(sub, depth+1)
			}
		case agentToolResultMsg:
			toolResult = true
		case streamTickMsg:
			streamTick = true
		}
	}
	walk(cmd, 0)
	if !toolResult {
		t.Fatal("returned cmd must eventually produce agentToolResultMsg — the tool never executed")
	}
	if !streamTick {
		t.Fatal("expected the stream tick to also be batched")
	}
}

func TestInterleavedToolGatedWaitsForPermission(t *testing.T) {
	// A reply mixing a gated command and a safe one: the safe command launches
	// early, the gated one is EXCLUDED from the early launch and waits for the
	// permission popover — its card shows "awaiting approval" until the user
	// decides, then updates to allowed/denied.
	work := t.TempDir()
	t.Chdir(work)

	m := newTestModel()
	m.started = true
	replyText := "```bash\nrm -rf build/\n```\n\nLanjut.\n```bash\nls -la\n```"

	updated, _ := m.Update(agentResultMsg{reply: mockReply{text: replyText}, run: m.agentRun})
	m2 := updated.(Model)
	if !m2.toolLaunched {
		t.Fatal("safe subset must still launch early when gated commands exist")
	}
	if m2.pendingPermissionText == "" {
		t.Fatal("gated command must be stashed for the permission popover")
	}
	if !strings.Contains(m2.pendingPermissionText, "rm -rf build/") {
		t.Fatalf("pending permission text must contain the gated command, got %q", m2.pendingPermissionText)
	}
	if strings.Contains(m2.pendingPermissionText, "ls -la") {
		t.Fatalf("safe command must be stripped from the gated round, got %q", m2.pendingPermissionText)
	}

	// Reveal fully — the gated card appears inline marked "awaiting approval".
	for i := 0; i < 500 && m2.streaming; i++ {
		next, _ := m2.Update(streamTickMsg{})
		m2 = next.(Model)
	}
	if m2.streaming {
		t.Fatal("stream never completed within 500 ticks")
	}
	if !m2.askOpen {
		t.Fatal("gated command must open the permission popover at end of reply")
	}
	foundAwaiting := false
	for _, cm := range m2.chat {
		if cm.role == roleTool && strings.Contains(cm.summary, "rm -rf build/") && strings.Contains(cm.summary, "awaiting approval") {
			foundAwaiting = true
		}
	}
	if !foundAwaiting {
		t.Fatal("gated command must have an inline card awaiting approval")
	}

	// Allow once → the card flips to allowed and the gated command runs.
	m2.askRadio[0] = 0 // allow once
	m3, cmd := m2.submitPermission()
	if cmd == nil {
		t.Fatal("expected a tool-run command after approval")
	}
	if m3.allowList["rm"] {
		t.Fatal("allow once must not seed the allow-list")
	}
	foundAllowed := false
	for _, cm := range m3.chat {
		if cm.role == roleTool && strings.Contains(cm.summary, "rm -rf build/") && strings.Contains(cm.summary, "allowed ✓") {
			foundAllowed = true
		}
	}
	if !foundAllowed {
		t.Fatal("gated card must update to allowed after approval")
	}
}

func TestAskBlockSkipsEarlyLaunchAndCards(t *testing.T) {
	// A reply carrying an ask block must NOT launch any tool early and must
	// NOT card the ask XML as a ⚙ card — the clarify popover IS its
	// execution, and it opens at the end of the reply (existing flow).
	m := newTestModel()
	m.started = true
	replyText := strings.Join([]string{
		"Sebelum lanjut:",
		"",
		"<tool_call>ask",
		"<ask_question>",
		"Pilih mode:",
		"- Cepat",
		"- Detail",
		"</ask_question>",
		"</tool_call>",
		"",
		"```bash",
		"ls -la",
		"```",
	}, "\n")

	updated, _ := m.Update(agentResultMsg{reply: mockReply{text: replyText}, run: m.agentRun})
	m2 := updated.(Model)
	if m2.toolLaunched {
		t.Fatal("ask block must skip the early launch")
	}
	// Reveal fully: the ask XML must not become a tool card.
	for i := 0; i < 500 && m2.streaming; i++ {
		next, _ := m2.Update(streamTickMsg{})
		m2 = next.(Model)
	}
	if m2.streaming {
		t.Fatal("stream never completed within 500 ticks")
	}
	if !m2.askOpen {
		t.Fatal("ask block must open the clarify popover at end of reply")
	}
	for _, cm := range m2.chat {
		if cm.role == roleTool {
			if strings.Contains(cm.summary, "ask") || strings.Contains(cm.content, "ask_question") {
				t.Fatalf("the ask XML must never become a tool card, got %+v", cm)
			}
			// The bash command's card is legit — but nothing launched early,
			// so it must read as queued, not running.
			if !strings.Contains(cm.summary, "queued") {
				t.Fatalf("bash card without early launch must read as queued, got %q", cm.summary)
			}
		}
	}
}

func TestEditCardRendersWithDiffColor(t *testing.T) {
	// Collapsed edit card: ✎ icon + summary + ctrl+o hint, diff hidden.
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleTool, summary: "Edit(a.go) · updated 6 lines", content: "✎ a.go · 6 lines\n+ new\n- old\n  ctx", collapsed: true})
	m.refreshChat()
	v := m.View().Content
	if !strings.Contains(v, "✎") || !strings.Contains(v, "Edit(a.go)") || !strings.Contains(v, "ctrl+o to view") {
		t.Fatalf("collapsed edit card chrome missing:\n%s", v)
	}
	if strings.Contains(v, "+ new") {
		t.Fatalf("collapsed edit card must hide the diff:\n%s", v)
	}

	// Expanded via ctrl+o: the diff renders with + / - markers colored.
	updated, _ := m.Update(ctrlOKey())
	m2 := updated.(Model)
	v = m2.View().Content
	for _, want := range []string{"✎ a.go", "+ new", "- old", "ctx"} {
		if !strings.Contains(v, want) {
			t.Fatalf("expanded edit card missing %q:\n%s", want, v)
		}
	}
}

func TestEditDiffRendersGreenRed(t *testing.T) {
	// Collapsed: only the dim ▸ summary row shows — the diff stays hidden.
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "", summary: "✎ 1 file(s) edited · ctrl+o to expand", content: "✎ main.go · 6 lines\n+ new\n- old\n  ctx", collapsed: true})
	m.refreshChat()
	v := m.View().Content
	if !strings.Contains(v, "file(s) edited") {
		t.Fatalf("collapsed edit summary missing:\n%s", v)
	}
	if strings.Contains(v, "new") || strings.Contains(v, "old") {
		t.Fatalf("collapsed diff content must be hidden:\n%s", v)
	}

	// Expanded via ctrl+o: the diff renders with + green / - red markers
	// (the renderer keeps the + / - markers with their diff spacing).
	updated, _ := m.Update(ctrlOKey())
	m2 := updated.(Model)
	v = m2.View().Content
	for _, want := range []string{"✎ main.go", "+ new", "- old", "ctx"} {
		if !strings.Contains(v, want) {
			t.Fatalf("expanded edit diff missing %q:\n%s", want, v)
		}
	}

	// isDiffContent recognizes the ✎ header; renderDiffContent keeps markers.
	if !isDiffContent("✎ a.go · 2 lines\n+\tx") {
		t.Fatal("isDiffContent must recognize edit blocks")
	}
	if isDiffContent("thinking trace") || isDiffContent("[SYSTEM TOOL RESULT]\nx") {
		t.Fatal("isDiffContent must reject non-edit content")
	}
	r := ansiStrip.ReplaceAllString(m2.renderDiffContent("✎ a.go · 1 lines\n+ added\n- removed", 60), "")
	if !strings.Contains(r, "+ added") || !strings.Contains(r, "- removed") || !strings.Contains(r, "✎ a.go") {
		t.Fatalf("renderDiffContent lost markers, got %q", r)
	}
}

func TestAskPopoverRendersBorderedCard(t *testing.T) {
	// The ask/permission popover must render as a thin-bordered floating card
	// (popoverBox chrome), not a bare text blob that blends into the chat.
	m := newTestModel()
	m.started = true
	m.openAsk("💬 agent needs your input", []askQuestion{
		{header: "Auth method", question: "Which one?", options: []string{"JWT", "Session cookies"}},
	}, "")
	m.refreshChat()
	v := ansiStrip.ReplaceAllString(m.View().Content, "")
	for _, want := range []string{"┌", "│", "└", "Auth method", "JWT", "agent needs your input"} {
		if !strings.Contains(v, want) {
			t.Fatalf("ask popover missing border/card element %q:\n%s", want, v)
		}
	}
}

func TestMouseWheelScrollsChatWhileAskOpen(t *testing.T) {
	// While the ask popover is up (not typing a custom answer) the wheel
	// scrolls the chat history behind it — the user may want to re-read
	// context before deciding. Keyboard (↑↓/space/enter) still navigates.
	m := newTestModel()
	m.started = true
	m.askOpen = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: strings.Repeat("filler line\n", 60)})
	m.refreshChat()
	m.viewport.GotoTop()

	updated, _ := m.Update(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelDown}))
	m2 := updated.(Model)
	if m2.viewport.YOffset() <= 0 {
		t.Fatalf("expected wheel to scroll the chat behind the popover, offset=%d", m2.viewport.YOffset())
	}
}

func TestMouseWheelIgnoredWhileTypingCustomAnswer(t *testing.T) {
	// While the user is typing a custom answer (askCustomOpen), the wheel is
	// ignored so it can never move the cursor inside the input.
	m := newTestModel()
	m.started = true
	m.askOpen = true
	m.askCustomOpen = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: strings.Repeat("filler line\n", 60)})
	m.refreshChat()
	m.viewport.GotoTop()

	_, _ = m.Update(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelDown}))
	if m.viewport.YOffset() != 0 {
		t.Fatalf("expected no scroll while typing a custom answer, offset=%d", m.viewport.YOffset())
	}
}

func TestExtractToolEchoBareOutputMarker(t *testing.T) {
	// Weak models also echo tool results with a bare "Output:" label instead
	// of the full [SYSTEM TOOL RESULT] marker — that must fold too.
	text := "Sekarang saya cek isi docs.\nOutput:\n  ```\n  # File di docs\n  backend/docs/api.md\n  ```\n\nSekarang saya cek validasi."
	prose, echo, n := extractToolEcho(text)
	if n != 1 {
		t.Fatalf("expected 1 echoed block, got %d (prose=%q echo=%q)", n, prose, echo)
	}
	if !strings.Contains(echo, "backend/docs/api.md") {
		t.Fatalf("payload must fold into the echo block, got %q", echo)
	}
	if strings.Contains(prose, "backend/docs/api.md") || !strings.Contains(prose, "Sekarang saya cek validasi") {
		t.Fatalf("payload must leave prose, got %q", prose)
	}

	// A bare "Output:" with only prose after it (no indented/fenced payload)
	// is NOT an echo — it must stay prose.
	text2 := "Output:\nThe fix is applied."
	prose2, echo2, n2 := extractToolEcho(text2)
	if n2 != 0 || echo2 != "" || prose2 != "Output:\nThe fix is applied." {
		t.Fatalf("marker with no payload must stay prose (n=%d prose=%q echo=%q)", n2, prose2, echo2)
	}
}

func TestCompactScrollsToDivider(t *testing.T) {
	// After /compact resolves, the viewport must land on the ✂ Compaction
	// divider so the fold is SEEN — not happening silently out of view above
	// the tail (the "ter-compact di atas" complaint).
	m := newTestModel()
	m.window = 500
	m.chat = buildBigChat(12)
	m.refreshChat()
	m.viewport.GotoTop()

	m.input.SetValue("/compact")
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if !m2.compacting {
		t.Fatal("expected the visible compaction process to start")
	}

	updated, _ = m2.Update(compactRunMsg{})
	m3 := updated.(Model)
	if m3.follow {
		t.Fatal("expected follow=false after scroll-to-compaction")
	}
	v := ansiStrip.ReplaceAllString(m3.View().Content, "")
	if !strings.Contains(v, "✂ Compaction") {
		t.Fatalf("expected the ✂ Compaction divider visible in the viewport after /compact:\n%s", v)
	}
}

func TestReplyAfterCompactRefollowsViewport(t *testing.T) {
	// After a manual /compact the viewport is pinned to show the ✂ divider.
	// When the agent's next reply streams in, the conversation continues
	// BELOW that divider — the viewport must re-follow the new reply so it
	// doesn't stay stranded at the top ("compaction di atas, lanjutannya
	// mana?" complaint).
	m := newTestModel()
	m.started = true
	m.follow = true
	m.window = 500
	m.chat = buildBigChat(12)
	m.refreshChat()
	m.viewport.GotoTop()

	// /compact pins the view to the divider (follow=false).
	m.input.SetValue("/compact")
	updated, _ := m.Update(enterKey())
	m2 := updated.(Model)
	if !m2.compacting || !m2.resumeFollowOnReply {
		t.Fatal("manual /compact must arm the resume-follow flag")
	}
	updated, _ = m2.Update(compactRunMsg{})
	m3 := updated.(Model)
	if m3.follow {
		t.Fatal("compaction itself must pin the view (follow=false)")
	}

	// The agent's continuation reply arrives — the viewport re-follows.
	updated, _ = m3.Update(agentResultMsg{reply: mockReply{text: "Lanjutan percakapan setelah compact."}, run: m3.agentRun})
	m4 := updated.(Model)
	if !m4.follow {
		t.Fatal("reply after manual compaction must re-enable follow")
	}
	if m4.resumeFollowOnReply {
		t.Fatal("resume-follow flag must be consumed by the first reply")
	}
}

func TestToolRepeatDetectionStopsLoop(t *testing.T) {
	// A model stuck in a loop re-emits the IDENTICAL command set instead of
	// reacting to the previous output. The guard must block the 3rd identical
	// set BEFORE it runs again — the loop dies in 3 rounds, not 20.
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "fix the build"})
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "checking…"})
	reply := "Let me check:\n```bash\nls -la\n```\n"

	// Run 1: new command set → executes, previous set recorded.
	m2, cmd1 := m.launchToolRun(reply, nil)
	if cmd1 == nil {
		t.Fatal("run 1 must execute")
	}
	if m2.toolPrevCmds == "" || m2.toolRepeat != 0 {
		t.Fatalf("run 1 must record the set without repeats (prev=%q repeat=%d)", m2.toolPrevCmds, m2.toolRepeat)
	}

	// Run 2: identical set → executes (first repeat allowed — a legitimate
	// double-check), counter bumps.
	m3, cmd2 := m2.launchToolRun(reply, nil)
	if cmd2 == nil {
		t.Fatal("run 2 (first repeat) must still execute")
	}
	if m3.toolRepeat != 1 {
		t.Fatalf("expected repeat=1 after the 2nd identical run, got %d", m3.toolRepeat)
	}

	// Run 3: identical set → BLOCKED before execution (nil cmd), loop stops.
	m4, cmd3 := m3.launchToolRun(reply, nil)
	if cmd3 != nil {
		t.Fatal("run 3 (2nd repeat) must NOT execute")
	}
	if m4.toolRunning {
		t.Fatal("tool loop must be stopped, not running")
	}
	if !strings.Contains(m4.status, "repeated") {
		t.Fatalf("expected a repeated-command status, got %q", m4.status)
	}
	if !strings.Contains(strings.Join(m4.trace, "\n"), "Tool loop stopped") {
		t.Fatalf("expected a stop trace line, got %v", m4.trace)
	}
}

func TestToolRepeatGuardResetsOnNewCommands(t *testing.T) {
	// A CHANGED command set is progress — adding a command or reordering must
	// reset the repetition counter, so a model that actually tries something
	// new is never blocked.
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "fix the build"})
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "checking…"})
	replyA := "```bash\nls -la\n```\n"
	replyB := "```bash\nls -la && echo NEW\n```\n"

	m2, _ := m.launchToolRun(replyA, nil)
	m3, _ := m2.launchToolRun(replyA, nil) // identical → repeat=1
	if m3.toolRepeat != 1 {
		t.Fatalf("setup: expected repeat=1, got %d", m3.toolRepeat)
	}
	m4, cmd := m3.launchToolRun(replyB, nil)
	if cmd == nil {
		t.Fatal("a changed command set must execute")
	}
	if m4.toolRepeat != 0 {
		t.Fatalf("repeat must reset on a new command set, got %d", m4.toolRepeat)
	}
}

func TestToolRepeatGuardResetOnUserTurn(t *testing.T) {
	// A real user prompt is progress: it must clear the repetition guard so
	// the agent gets a fresh budget instead of inheriting the stuck state.
	m := newTestModel()
	m.started = true
	m.toolPrevCmds = "ls -la"
	m.toolRepeat = 2 // simulated stuck state

	m.input.SetValue("try something else")
	m2, _ := m.send()
	if m2.toolPrevCmds != "" || m2.toolRepeat != 0 {
		t.Fatalf("user turn must reset the guard (prev=%q repeat=%d)", m2.toolPrevCmds, m2.toolRepeat)
	}
}

func TestFileSearchIndexBounded(t *testing.T) {
	// The native search index covers the workspace, bounded: ignored dirs and
	// oversized files are never indexed, real source files are.
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	if err := os.WriteFile("auth.go", []byte("package auth\n\n// handles the refresh flow\nfunc Refresh() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("db.go", []byte("package db\n\nvar conn string // database connection\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("node_modules/pkg", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("node_modules/pkg/index.js", []byte("refresh token logic here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("big.log", []byte(strings.Repeat("refresh ", 30<<10)), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := cachedFileIndex()
	if ix == nil {
		t.Fatal("expected a workspace index")
	}
	res := ix.Search("refresh", 5)
	if len(res) == 0 {
		t.Fatal("expected a match for 'refresh'")
	}
	if res[0].ID != "auth.go" {
		t.Fatalf("expected auth.go first, got %+v", res[0])
	}
	// The ignored dir and the oversized file must never surface.
	for _, r := range ix.Search("refresh", 10) {
		if r.ID == "node_modules/pkg/index.js" || r.ID == "big.log" {
			t.Fatalf("ignored/oversized file must not be indexed: %+v", r)
		}
	}
}

// TestFileSearchMonorepoBalanceAndPriority is the regression test for the
// mis-answer root cause: in a monorepo, an alphabetical walk filled the
// bounded index with tooling/config/frontend files before crm_sales_backend's
// src/services/lead-rotation/ was reached — so "rotation" found nothing real
// and the agent answered about an unrelated component. The index must (a)
// skip tooling dirs, (b) balance per top-level dir, (c) index core src/
// before tests/scripts, and (d) rank a path match above a body match.
func TestFileSearchMonorepoBalanceAndPriority(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	// Tooling dirs that must never eat the budget.
	for _, td := range []string{".agents", ".kuma", ".opencode", "node_modules"} {
		if err := os.MkdirAll(td, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(td+"/tooling-file.js", []byte("rotation rotation rotation rotation"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A big frontend that would starve the backend under a plain walk.
	if err := os.MkdirAll("crm-react-vite-tailwind-modern/src/components/tiptap", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("crm-react-vite-tailwind-modern/src/components/tiptap/LineShape.tsx",
		[]byte("rotation attr rotation visual rotation transform rotate deg rotation"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The REAL rotation implementation (backend service + its path token).
	if err := os.MkdirAll("crm_sales_backend/src/services/lead-rotation", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("crm_sales_backend/src/services/lead-rotation/LeadRotationService.js",
		[]byte("const prisma = require; class LeadRotationService { importSchedule csv }"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := cachedFileIndex()
	if ix == nil {
		t.Fatal("expected a workspace index")
	}
	// Tooling files must not be indexed.
	for _, d := range ix.Docs() {
		for _, td := range []string{".agents/", ".kuma/", ".opencode/", "node_modules/"} {
			if strings.Contains(d.ID, td) {
				t.Fatalf("tooling dir must not be indexed: %s", d.ID)
			}
		}
	}
	// The core service must be indexed and must OUTRANK the body-only match.
	res := ix.Search("rotation", 5)
	if len(res) == 0 {
		t.Fatal("expected results for 'rotation'")
	}
	if res[0].ID != "crm_sales_backend/src/services/lead-rotation/LeadRotationService.js" {
		t.Fatalf("expected LeadRotationService.js first (path match), got %+v", res)
	}
}

func TestApplyAgenticToolsSearchTool(t *testing.T) {
	// The agent's <tool_call>search…</tool_call> runs the native bounded index
	// (no bash, no grep round-trip) and feeds the matching file paths back.
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	if err := os.WriteFile("middleware.go", []byte("package mw\n\n// the middleware chain\nfunc Chain() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reply := "Let me find the auth middleware:\n<tool_call>search\nmiddleware</tool_call>\n"
	logs, feedback := applyAgenticToolsDeny(reply, nil, false, nil)
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, "\n"), "Workspace search: middleware") {
		t.Fatalf("expected a search trace log, got %v", logs)
	}
	if !strings.Contains(feedback, "middleware.go") {
		t.Fatalf("expected middleware.go in feedback, got:\n%s", feedback)
	}
	// The snippet (first substantive line after the package decl) is included
	// so the model can pick a file without reading it first.
	if !strings.Contains(feedback, "the middleware chain") {
		t.Fatalf("expected the file snippet in feedback, got:\n%s", feedback)
	}
	if strings.Contains(feedback, "Result of `") {
		t.Fatalf("search must not execute bash, got:\n%s", feedback)
	}

	// An empty query gets an actionable error, not a silent no-op.
	_, feedback2 := applyAgenticToolsDeny("<tool_call>search\n  </tool_call>", nil, false, nil)
	if !strings.Contains(feedback2, "empty query") {
		t.Fatalf("expected an empty-query error, got %q", feedback2)
	}
}

func TestFileSearchRefreshesAfterEdit(t *testing.T) {
	// The index refreshes incrementally (mtime-driven): an edited file is
	// re-indexed, a deleted file disappears, and a brand-new file appears —
	// without a full rebuild (unchanged files are never re-read).
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	if err := os.WriteFile("app.go", []byte("package app\n\n// the login flow\nfunc Login() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("gone.go", []byte("package gone\n\n// temporary file\nfunc Temp() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if ix := cachedFileIndex(); ix == nil {
		t.Fatal("expected an index")
	} else if res := ix.Search("login", 5); len(res) == 0 {
		t.Fatal("setup: expected 'login' to match app.go")
	}

	// Edit one file, delete another, add a brand-new one.
	if err := os.WriteFile("app.go", []byte("package app\n\n// the signup flow\nfunc Signup() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove("gone.go"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("new.go", []byte("package new\n\n// the payment flow\nfunc Pay() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Expire the cooldown so the next lookup actually refreshes.
	fileIndexChecked = time.Time{}
	ix := cachedFileIndex()

	if res := ix.Search("login", 5); len(res) != 0 {
		t.Fatalf("old content must not match after the edit, got %+v", res)
	}
	if res := ix.Search("signup", 5); len(res) != 1 || res[0].ID != "app.go" {
		t.Fatalf("edited content must be searchable, got %+v", res)
	}
	if res := ix.Search("payment", 5); len(res) != 1 || res[0].ID != "new.go" {
		t.Fatalf("a brand-new file must be indexed, got %+v", res)
	}
	if res := ix.Search("temporary", 5); len(res) != 0 {
		t.Fatalf("a deleted file must not match, got %+v", res)
	}
}

func TestDragSelectPinsViewport(t *testing.T) {
	// Pressing the mouse to start a drag must pin the viewport (follow=false),
	// and a refresh while dragging must NOT auto-scroll to the bottom — a
	// freshly-streamed reply would otherwise scroll under the selection and the
	// highlight would cover DIFFERENT text than the user pointed at (the "only
	// part of the line got selected" complaint on just-finished replies).
	m := newTestModel()
	m.started = true
	m.follow = true
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: strings.Repeat("line of content here\n", 60)})
	m.refreshChat()
	m.viewport.SetYOffset(5)
	before := m.viewport.YOffset()

	updated, _ := m.Update(mouseClick(2, headerHeight+2))
	m2 := updated.(Model)
	if m2.follow {
		t.Fatal("drag start must pin the viewport (follow=false)")
	}
	m2.refreshChat()
	if m2.viewport.YOffset() != before {
		t.Fatalf("viewport must stay pinned during drag, moved %d -> %d", before, m2.viewport.YOffset())
	}
}

func TestDragSelectAutoScrollsPastBottom(t *testing.T) {
	// Dragging past the viewport edge must auto-scroll and extend the
	// selection beyond the originally visible window — a long drag that stops
	// at the edge otherwise truncates the copied text silently.
	m := newTestModel()
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: strings.Repeat("long line content here\n", 60)})
	m.refreshChat()

	updated, _ := m.Update(mouseClick(2, headerHeight+2))
	m2 := updated.(Model)
	y0 := m2.dragSel.y0

	maxRow := headerHeight + m.viewport.Height() + 10
	updated, _ = m2.Update(tea.MouseMotionMsg(tea.Mouse{X: 40, Y: maxRow}))
	m3 := updated.(Model)
	if m3.dragSel.y1 <= y0 {
		t.Fatal("auto-scroll must extend the selection beyond the drag start")
	}
	if want := m3.viewport.YOffset() + m3.viewport.Height() - 1; m3.dragSel.y1 != want {
		t.Fatalf("selection line must track the scrolled bottom, got y1=%d want=%d (offset=%d)", m3.dragSel.y1, want, m3.viewport.YOffset())
	}
}

func TestTableRendersAligned(t *testing.T) {
	// A markdown table must render as ONE column-aligned grid: the pipe
	// separators line up vertically across the header and every data row.
	// (The old renderer drew each row independently → ragged columns.)
	m := newTestModel()
	m.started = true
	md := "| Aspect | BroCode | Claude Code |\n|--------|---------|-------------|\n| Size | 10MB | 150MB |\n| longcell | 1 | 2 |"
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: md})
	m.refreshChat()
	v := m.View().Content

	var pipeCols [][]int
	for _, ln := range strings.Split(v, "\n") {
		plain := ansiStrip.ReplaceAllString(ln, "")
		if !strings.Contains(plain, "│") {
			continue
		}
		var pos []int
		for i, r := range plain {
			if r == '│' {
				pos = append(pos, i)
			}
		}
		if len(pos) >= 3 {
			pipeCols = append(pipeCols, pos)
		}
	}
	if len(pipeCols) < 3 {
		t.Fatalf("expected at least 3 aligned table rows, got %d:\n%s", len(pipeCols), v)
	}
	for i := 1; i < len(pipeCols); i++ {
		if pipeCols[i][0] != pipeCols[0][0] || pipeCols[i][1] != pipeCols[0][1] || pipeCols[i][2] != pipeCols[0][2] {
			t.Fatalf("table pipes must align across rows, row0=%v row%d=%v:\n%s", pipeCols[0], i, pipeCols[i], v)
		}
	}
}

func TestTableStreamingIncrementalMatchesFull(t *testing.T) {
	// The streaming table render (buffered rows flushed as a block) must agree
	// with the final full render once the whole reply has streamed — the
	// final view is the one that persists, and it must be the aligned grid.
	// (Mid-stream states may differ while rows are buffered for alignment;
	// non-table replies stay byte-identical — covered by
	// TestStreamingRenderIncrementalMatchesFull.)
	m := newTestModel()
	m.width = 100
	m.layout()
	w := m.chatContentWidth()
	md := "before\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\nafter"
	div := m.styles.sys.Render(strings.Repeat("·", min(w, 60)))
	m.streamCache = streamCache{}

	// Feed the whole reply in one shot — the cache starts fresh, exactly like
	// a reply that streamed and then re-rendered at completion.
	inc := m.renderStreamingAgent(md, w)
	want := m.renderPlain(md, w) + "\n" + div + "\n"
	if inc != want {
		t.Fatalf("streaming render must match full render at completion\n--- incremental ---\n%q\n--- full render ---\n%q", inc, want)
	}
	// The aligned grid must actually be present (pipes line up) — not the
	// old ragged per-row output.
	plain := ansiStrip.ReplaceAllString(inc, "")
	if !strings.Contains(plain, "│") || !strings.Contains(plain, "A│") {
		t.Fatalf("aligned table grid missing from streaming output:\n%q", inc)
	}
}
