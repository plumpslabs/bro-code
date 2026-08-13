package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ── Transient-turn guard (tool-round enrichment skip) ────────────────────

func TestIsTransientTurn(t *testing.T) {
	cases := []struct {
		q    string
		want bool
	}{
		{"Tool Execution Output:\nContent of x.go", true}, // agentic loop feeds tool results back with THIS prefix
		{"[SYSTEM TOOL RESULT]\nexit 0", true},
		{"[SYSTEM ASK RESULT]\nyes", true},
		{"gas lanjutkan", false},
		{"cek rotasi di project", false},
		{"Tool Execution Output", false}, // prefix must match exactly
	}
	for _, c := range cases {
		if got := isTransientTurn(c.q); got != c.want {
			t.Errorf("isTransientTurn(%q) = %v, want %v", c.q, got, c.want)
		}
	}
}

// TestTransientTurnCheapTreeOnly: tool-result rounds must NOT re-walk the
// project tree / rescan files — the stale prefix guard ("[SYSTEM TOOL
// RESULT]" only) missed the real queue prefix and added seconds + thousands
// of tokens per round. They DO get the CHEAP cached tree (orientation for
// free models, ~0ms since it is computed once per working dir) but never the
// heavy parts (keyword scan / file attachments / repo map).
func TestTransientTurnCheapTreeOnly(t *testing.T) {
	enriched := attachFileContext("cek rotasi", false)
	if !strings.Contains(enriched, "PROJECT WORKSPACE TREE:") {
		t.Fatalf("setup: a real user prompt must enrich, got:\n%.120s", enriched)
	}
	// A PLAIN question gets NO tree — the model drives search itself with
	// the `search` tool, exactly like opencode (which never pre-injects a
	// tree). Pre-injecting to every prompt made weak models answer from the
	// tree alone instead of researching the code.
	plain := attachFileContext("jelasin rotation feature di CRM", false)
	if strings.Contains(plain, "PROJECT WORKSPACE TREE:") {
		t.Fatalf("plain question must NOT inherit the tree (model researches itself), got:\n%.120s", plain)
	}
	// A search-intent question DOES get the tree + keyword scan.
	si := attachFileContext("cari kode rotasi di project", false)
	if !strings.Contains(si, "PROJECT WORKSPACE TREE:") {
		t.Fatalf("search-intent prompt must get the tree, got:\n%.120s", si)
	}

	// Force the tree cache warm so the transient path reads the SAME tree.
	cachedProjectTree()
	for _, q := range []string{
		"Tool Execution Output:\nContent of file.go",
		"[SYSTEM TOOL RESULT]\nexit 0",
		"[SYSTEM ASK RESULT]\nyes",
	} {
		got := attachFileContext(q, true)
		if !strings.Contains(got, "PROJECT WORKSPACE TREE:") {
			t.Errorf("transient turn must get the cheap cached tree (orientation), got:\n%.200s", got)
		}
		if !strings.Contains(got, q) {
			t.Errorf("transient turn payload must survive, got:\n%.200s", got)
		}
		// The heavy enrichment must NOT run on a tool round: no keyword-scan
		// result block, no FILE ATTACHMENT from the path scan.
		if strings.Contains(got, "FILE ATTACHMENT:") {
			t.Errorf("transient turn must skip file attachments, got:\n%.200s", got)
		}
	}
}

// ── Evidence pass helpers (AGENTIC_OVERHAUL P1) ───────────────────────────

// TestHasSearchIntent: the pre-prompt keyword scan must run ONLY when the
// user explicitly asks to find/investigate code. A plain question ("jelasin
// rotation di CRM") has NO search intent — pre-injecting BM25 top-hits there
// anchored the model on the wrong file (LineShape.tsx visual rotation instead
// of the CRM lead-rotation pipeline the user meant).
func TestHasSearchIntent(t *testing.T) {
	cases := []struct {
		q    string
		want bool
	}{
		{"jelasin rotation feat di crm ini", false}, // plain question — no pre-scan
		{"cari kode rotasi di project", true},
		{"cek file LeadRotationService.js", true}, // explicit file
		{"search where the bidding pipeline is", true},
		{"di mana letak service rotasi", true},
		{"gas lanjutkan", false},
		{"tolong cek dulu dong", true}, // "cek" = investigate intent
		{"ok thanks", false},
	}
	for _, c := range cases {
		if got := hasSearchIntent(c.q); got != c.want {
			t.Errorf("hasSearchIntent(%q) = %v, want %v", c.q, got, c.want)
		}
	}
}

func TestEvidenceQuery(t *testing.T) {
	// No handcrafted word lists: the raw prompt's meaningful tokens become the
	// query and BM25's IDF downweights common words statistically. Only
	// tokens < 3 chars and punctuation are filtered.
	got := evidenceQuery("kak tolong cari rotasi lead bidding di project")
	for _, want := range []string{"rotasi", "bidding", "project", "tolong"} {
		if !strings.Contains(got, want) {
			t.Errorf("evidenceQuery missing %q: got %q", want, got)
		}
	}
	// Pure-noise prompts yield no query and never trigger an index scan.
	if q := evidenceQuery("ok ... ?!"); q != "" {
		t.Errorf("expected empty query for noise, got %q", q)
	}
}

func TestExplorationEvidenceBoundedAndRelevant(t *testing.T) {
	// Query a term that exists in this workspace (the BM25 index is built from
	// the test CWD = internal/tui). The block must be bounded (≤ 5 bullets) and
	// list real file paths with a snippet.
	ev := explorationEvidence("cek bm25 index file", nil)
	if ev == "" {
		t.Fatal("expected evidence for 'bm25' in this workspace, got empty")
	}
	if !strings.HasPrefix(ev, "WORKSPACE EXPLORATION") {
		t.Fatalf("expected evidence header, got %q", ev[:min(40, len(ev))])
	}
	// Count FILE bullets (they carry a "relevance" score) — symbol-map rows
	// also use "• " but have no score. Must be 1..5 files.
	fileBullets := 0
	for _, line := range strings.Split(ev, "\n") {
		if strings.Contains(line, "relevance") {
			fileBullets++
		}
	}
	if fileBullets == 0 || fileBullets > evidenceTopK {
		t.Fatalf("evidence must list 1..%d files, got %d", evidenceTopK, fileBullets)
	}
	if !strings.Contains(ev, ".go") {
		t.Fatalf("expected a Go file path in evidence: %q", ev)
	}
	// Whole block bounded (< 2 KB): the evidence pass must never flood context.
	if len(ev) > 2048 {
		t.Fatalf("evidence block too large: %d bytes", len(ev))
	}
	// File bullets carry a snippet — never an empty preview.
	for _, line := range strings.Split(ev, "\n") {
		if !strings.Contains(line, "relevance") {
			continue
		}
		if strings.Contains(line, "(no preview)") {
			t.Fatalf("file bullet must carry a matched snippet: %q", line)
		}
	}
}

func TestExplorationEvidenceEmptyForNoMatches(t *testing.T) {
	// A query that matches nothing in the workspace yields an empty block —
	// the stall recovery then skips the extra call instead of burning tokens.
	// Built dynamically so the literal never appears in this file (which the
	// BM25 index would then match!).
	noMatch := "zz" + strings.Repeat("q", 12)
	if got := explorationEvidence(noMatch, nil); got != "" {
		t.Fatalf("expected empty evidence for no matches, got %q", got)
	}
}

func TestStripEnrichedPrompt(t *testing.T) {
	if got := stripEnrichedPrompt("USER PROMPT:\nhalo"); got != "halo" {
		t.Fatalf("expected 'halo', got %q", got)
	}
	enriched := "PROJECT WORKSPACE TREE:\n```\nx\n```\n\nUSER PROMPT:\ncek rotasi"
	if got := stripEnrichedPrompt(enriched); got != "cek rotasi" {
		t.Fatalf("expected stripped prompt, got %q", got)
	}
	// A literal "USER PROMPT:" inside an attached file must not truncate early
	// — LastIndex keeps the final marker.
	tricky := "FILE ATTACHMENT: a.txt\nUSER PROMPT:\nno\n\nUSER PROMPT:\nfinal"
	if got := stripEnrichedPrompt(tricky); got != "final" {
		t.Fatalf("expected final marker to win, got %q", got)
	}
	if got := stripEnrichedPrompt("no marker"); got != "no marker" {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ── Stall recovery (AGENTIC_OVERHAUL P0) ──────────────────────────────────
// Simulates the transcript dead-end over HTTP: the stream delivers reasoning
// only, the non-streaming retry ALSO returns reasoning only, and the stall
// recovery then injects the evidence pass and re-prompts. The server counts
// calls so the bounded (≤ 3) recovery is provable.

func zenStallServer(t *testing.T, recoveryAnswer string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Stream   bool                `json:"stream"`
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		if body.Stream {
			// SSE: reasoning only, no content, no tool call — the transcript stall.
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"I must emit native search calls\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		last := ""
		if n := len(body.Messages); n > 0 {
			last = body.Messages[n-1]["content"]
		}
		if strings.Contains(last, "[SYSTEM TOOL RESULT]") {
			// Stall recovery call: return a real answer.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, recoveryAnswer)
			return
		}
		// Non-streaming retry: still stalls with reasoning only.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"reasoning_content":"still just thinking","content":""}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	return srv, &calls
}

func TestZenChatReplyStallRecoveryForInvestigationPrompt(t *testing.T) {
	srv, calls := zenStallServer(t, "Jawaban berbasis evidence")
	defer srv.Close()

	m := newTestModel()
	m.plannerMode = true // forces the evidence pass even for short queries
	m.window = 16000
	ch := make(chan agentTraceMsg, 32)
	cancel := func() {}
	res := zenChatReply(m, "cek bm25", "test-model", srv.URL, ch, time.Now(), &cancel, 1)
	got := res.(agentResultMsg).reply.text

	if !strings.Contains(got, "Jawaban berbasis evidence") {
		t.Fatalf("stall recovery must return the recovery answer, got %q", got)
	}
	if *calls != 3 {
		t.Fatalf("expected exactly 3 model calls (stream + retry + recovery), got %d", *calls)
	}
}

func TestZenChatReplyNoRecoveryForCasualPrompt(t *testing.T) {
	srv, calls := zenStallServer(t, "jawaban")
	defer srv.Close()

	m := newTestModel()
	m.plannerMode = false
	m.window = 16000
	// Query built dynamically so it matches nothing in the indexed workspace
	// ("halo" would match this very test file!) — recovery then has no
	// evidence and must skip the extra call.
	noMatch := "zz" + strings.Repeat("q", 12)
	ch := make(chan agentTraceMsg, 32)
	cancel := func() {}
	res := zenChatReply(m, noMatch, "test-model", srv.URL, ch, time.Now(), &cancel, 1)
	got := res.(agentResultMsg).reply.text

	// Recovery is attempted for ANY reasoning-only reply, but with no matching
	// evidence for "halo" the extra call is skipped: stream + retry only
	// (bounded at 2, no evidence burn), and the thinking trace remains.
	if *calls != 2 {
		t.Fatalf("expected 2 calls when evidence is empty, got %d", *calls)
	}
	if !strings.Contains(got, "I must emit native search calls") {
		t.Fatalf("expected the thinking trace to remain the reply, got %q", got)
	}
}

// ── P2: native tool_calls ────────────────────────────────────────────────

func TestToolCallsToBlocks(t *testing.T) {
	tcs := []zenToolCall{
		{Name: "search", Arguments: `{"query": "rotasi"}`},
		{Name: "read", Arguments: `{"path": "main.go"}`},
		{Name: "bash", Arguments: `{"command": "go test ./..."}`},
		{Name: "write_file", Arguments: `{"path": "x.go", "content": "package x"}`},
		{Name: "edit_file", Arguments: `{"path": "y.go", "search": "a", "replace": "b"}`},
		{Name: "ask", Arguments: `{"question": "q?", "options": ["a", "b"]}`},
		{Name: "unknown_tool", Arguments: `{"x": 1}`},
	}
	got := toolCallsToBlocks("intro", tcs)
	for _, want := range []string{
		"intro",
		"<tool_call>search\nrotasi\n</tool_call>",
		"<tool_call>read\nmain.go\n</tool_call>",
		"```bash\ngo test ./...\n```",
		"cat > x.go << 'EOF'",
		"<<<<<<< SEARCH: y.go",
		"<ask_question>q?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("toolCallsToBlocks missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unknown_tool") {
		t.Errorf("unknown tool must be skipped, got %q", got)
	}
}

func TestZenChatReplyStreamsNativeToolCalls(t *testing.T) {
	// The gateway streams a native search tool call as SSE fragments (index +
	// id, then arguments pieces). P2 must accumulate them into an executable
	// <tool_call> block WITHOUT a non-streaming retry.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			t.Error("expected only the streaming call — no non-streaming retry")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"search\",\"arguments\":\"\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"query\\\":\\\"rotasi\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	m := newTestModel()
	m.window = 16000
	ch := make(chan agentTraceMsg, 32)
	cancel := func() {}
	res := zenChatReply(m, "cek rotasi", "test-model", srv.URL, ch, time.Now(), &cancel, 1)
	got := res.(agentResultMsg).reply.text

	if calls != 1 {
		t.Fatalf("expected exactly 1 (streaming) call, got %d", calls)
	}
	if !strings.Contains(got, "<tool_call>search") || !strings.Contains(got, "rotasi") {
		t.Fatalf("streamed tool call must become an executable block, got %q", got)
	}
}

// ── P3: planner tool surface + repo map gating ────────────────────────────

func toolNames(payload []map[string]interface{}) []string {
	var names []string
	for _, t := range payload {
		fn, _ := t["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		names = append(names, name)
	}
	return names
}

func TestToolsPayloadPlannerNarrowsSurface(t *testing.T) {
	all := toolNames(toolsPayload(false))
	for _, want := range []string{"bash", "search", "read", "write_file", "edit_file", "ask"} {
		if !contains(all, want) {
			t.Errorf("builder surface missing %q: %v", want, all)
		}
	}
	planner := toolNames(toolsPayload(true))
	for _, drop := range []string{"bash", "write_file", "edit_file"} {
		if contains(planner, drop) {
			t.Errorf("planner surface must drop %q: %v", drop, planner)
		}
	}
	for _, keep := range []string{"search", "read", "ask"} {
		if !contains(planner, keep) {
			t.Errorf("planner surface missing %q: %v", keep, planner)
		}
	}
}

func TestShouldAttachRepoMap(t *testing.T) {
	cases := []struct {
		q           string
		plannerMode bool
		want        bool
	}{
		{"halo", false, false},
		{"halo", true, true},                                // planner always
		{"migrate security auth architecture", false, true}, // DeepPath router score
		{"[SYSTEM TOOL RESULT]\nx", true, false},            // tool turns never
	}
	for _, c := range cases {
		if got := shouldAttachRepoMap(c.q, c.plannerMode); got != c.want {
			t.Errorf("shouldAttachRepoMap(%q, %v) = %v, want %v", c.q, c.plannerMode, got, c.want)
		}
	}
}

// ── P5: environment block ─────────────────────────────────────────────────

func TestSystemPromptEnvironmentBlock(t *testing.T) {
	p := systemPromptForMode(false)
	if !strings.Contains(p, "ENVIRONMENT:") || !strings.Contains(p, runtime.GOOS) {
		t.Fatalf("system prompt must carry the environment block (GOOS=%s), got:\n%s", runtime.GOOS, p[:300])
	}
}

func TestSystemPromptGuardsPhantomEcosystemTools(t *testing.T) {
	// The CRM repo's AGENTS.md DEMANDS kuma_context("MUST call at session
	// start") — a tool that exists in opencode/Claude Code (via MCP) but NOT
	// in brocode. The system prompt must explicitly warn the model that
	// ecosystem tools are unavailable so a weak free model adapts instead of
	// burning rounds on "unsupported tool" retries (the kuma_kuma_context
	// wasted round in the rotation transcript).
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("AGENTS.md", []byte("MUST call kuma_context({action: \"init\"}) at session start."), 0o644); err != nil {
		t.Fatal(err)
	}
	cachedProjectDirectives() // warm the cache in this cwd
	p := systemPromptForMode(false)
	for _, want := range []string{"kuma_context", "NOT available in brocode", "NEVER call them", "use brocode's native tools"} {
		if !strings.Contains(p, want) {
			t.Fatalf("system prompt must warn about phantom ecosystem tools, missing %q", want)
		}
	}
	// The warning must reach the path that reads the brief from a different
	// cwd too (the cache is keyed by dir — a fresh project must still get it).
	dir2 := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "AGENTS.md"), []byte("kuma_context init"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	if err := os.Chdir(dir2); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)
	cachedProjectDirectives()
	p2 := systemPromptForMode(false)
	if !strings.Contains(p2, "NOT available in brocode") {
		t.Fatal("phantom-tool guard must be present regardless of cwd")
	}
}

// TestCachedProjectDirectivesFallsBackToBrief: when AGENTS.md is absent (the
// CRM repo's case — opencode answered correctly there precisely because it
// read MATCHA_PROJECT.md), the cache must fall back to the project brief so
// the model gets domain grounding ("CRM with lead rotation") instead of
// answering about an unrelated component. The cache is global state, so back
// up and restore it around the test.
func TestCachedProjectDirectivesFallsBackToBrief(t *testing.T) {
	origPath, origMod, origSize, origData := agentsCachePath, agentsCacheMod, agentsCacheSize, agentsCacheData
	defer func() {
		agentsCachePath, agentsCacheMod, agentsCacheSize, agentsCacheData = origPath, origMod, origSize, origData
	}()
	agentsCachePath, agentsCacheData = "", ""

	// Make a temp cwd with only MATCHA_PROJECT.md (no AGENTS.md) and switch to it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MATCHA_PROJECT.md"), []byte("# CRM brief\nLead rotation pipeline"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	got := cachedProjectDirectives()
	if !strings.Contains(got, "Lead rotation pipeline") {
		t.Fatalf("expected MATCHA_PROJECT.md content as fallback, got %q", got)
	}
}
