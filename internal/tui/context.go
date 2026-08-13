package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/plumpslabs/bro-code/internal/agentic"
	"github.com/plumpslabs/bro-code/internal/search"
)

// ── Per-session context cache ─────────────────────────────────────────────
// Attaching the workspace tree and scanning for keyword matches on EVERY
// prompt is wasteful: the tree only changes when the repo structure changes,
// and files already attached to a recent turn are already in the model's
// context — re-reading and re-attaching them burns both latency and tokens.
// These caches are keyed by the working directory (reset automatically when
// the user starts brocode in a different project) and guarded by a mutex
// because attachFileContext runs inside the agent goroutine.

var (
	attachMu       sync.Mutex
	attachTreeDir  string
	attachTreeVal  string
	repoMapDir     string
	repoMapVal     string // AST PageRank structural map (P6) — lazy, per project
	attachSeenDir  string
	attachSeenVals map[string]int64 // path → mtime (unix nanos) at attach time
)

// cachedProjectTree returns the workspace tree, computed once per working
// directory. The tree reflects repo structure (files/dirs), not file edits,
// so a per-project cache is safe for the whole session.
func cachedProjectTree() string {
	wd, _ := os.Getwd()
	attachMu.Lock()
	defer attachMu.Unlock()
	if wd != attachTreeDir {
		attachTreeDir = wd
		attachTreeVal = getProjectTree()
	}
	return attachTreeVal
}

// sessionSeen returns the set of files already attached to a prior turn in
// this project's session, keyed by path → mtime at attach time. The set is
// bounded so a very long session cannot grow it unboundedly; once it
// overflows, the oldest entries are dropped (worst case: a file gets
// re-attached once).
func sessionSeen() map[string]int64 {
	wd, _ := os.Getwd()
	attachMu.Lock()
	defer attachMu.Unlock()
	if wd != attachSeenDir {
		attachSeenDir = wd
		attachSeenVals = make(map[string]int64)
	}
	return attachSeenVals
}

// fileSeen returns the mtime recorded for path (0 = never attached).
func fileSeen(already map[string]int64, path string) int64 {
	attachMu.Lock()
	defer attachMu.Unlock()
	return already[path]
}

// rememberAttached persists the files attached this turn (with their current
// mtime) so later turns skip re-reading content that is unchanged. A file
// that is EDITED between turns gets a new mtime and is re-attached — the
// model must never keep reasoning over a stale snapshot it can re-read.
func rememberAttached(seen map[string]bool) {
	const maxSessionSeen = 50
	attachMu.Lock()
	defer attachMu.Unlock()
	// Self-prime for the current working directory: the write must land in
	// the SAME map sessionSeen() returns for this cwd, or it would be lost
	// on the next lookup. sessionSeen() normally primes first (attachFileContext
	// reads before it writes), but remember never assumes that ordering.
	wd, _ := os.Getwd()
	if attachSeenDir != wd || attachSeenVals == nil {
		attachSeenDir = wd
		attachSeenVals = make(map[string]int64)
	}
	for f := range seen {
		if fi, err := os.Stat(f); err == nil {
			attachSeenVals[f] = fi.ModTime().UnixNano()
		}
	}
	// Bounded: once the set overflows, drop everything (the model already
	// has the most recent turns verbatim in context).
	if len(attachSeenVals) > maxSessionSeen {
		attachSeenVals = make(map[string]int64)
	}
}

// resetAttachCache clears the per-project context cache. Called on /clear:
// a fresh conversation starts with zero model context, so previously
// attached files must be attachable again — otherwise the next prompt about
// a file referenced before the clear would silently skip it.
func resetAttachCache() {
	attachMu.Lock()
	defer attachMu.Unlock()
	attachSeenVals = make(map[string]int64)
	attachSeenDir = ""
	attachTreeDir = ""
	attachTreeVal = ""
}

// codeIntentWords are short action words that signal a real coding request
// even without a file-ish token — "fix compile", "add test", "refactor this"
// should keep the keyword scan. Purely conversational continuations ("lanjut",
// "gas", "yes", "ok") contain none of them and are skipped.
var codeIntentWords = []string{
	"fix", "add", "edit", "create", "update", "delete", "remove", "refactor",
	"implement", "debug", "test", "run", "build", "compile", "error", "bug",
	"explain", "audit", "review", "perbaiki", "buat", "ubah", "cek", "tambah",
}

// isTrivialFollowup reports whether q is a short conversational continuation
// ("lanjut", "gas", "yes", "ok") that cannot benefit from a workspace keyword
// scan — a 300-file walk for a 3-word continuation is pure latency with zero
// added context. File-ish tokens (a path, extension, or slash) or a code-intent
// verb (fix/add/debug…) mean real intent and always keep the scan.
func isTrivialFollowup(q string) bool {
	s := strings.TrimSpace(q)
	if len([]rune(s)) > 24 {
		return false
	}
	lower := strings.ToLower(s)
	for _, w := range strings.Fields(s) {
		if strings.Contains(w, ".") || strings.Contains(w, "/") || strings.Contains(w, "_") || strings.Contains(w, "-") {
			return false
		}
	}
	for _, v := range codeIntentWords {
		if strings.Contains(lower, v) {
			return false
		}
	}
	return true
}

// attachTransientContext gives a tool-result/ask round the cheap parts of
// workspace context: the CACHED project tree only (computed once per working
// dir — zero re-walk). The heavy parts (keyword scan of up to 300 files,
// file attachments, repo map) stay skipped so a tool round still costs ~0ms
// and a few hundred tokens of orientation, not seconds + thousands of
// tokens. Free models benefit most: they "forget" structure between rounds,
// and re-reading the tree keeps them grounded in the workspace while they
// reason over tool output.
func attachTransientContext(q string) string {
	tree := cachedProjectTree()
	if tree == "" {
		return q
	}
	return "PROJECT WORKSPACE TREE:\n```\n" + tree + "\n```\n\n" + q
}

// attachFileContext inspects the user query for file references or symbol keywords,
// automatically reading project tree and target files to attach rich workspace context.
// plannerMode widens the auto-context to ALWAYS include the evidence pass (P1 of
// AGENTIC_OVERHAUL): a planner that will not call tools must still get evidence.
func attachFileContext(q string, plannerMode bool) string {
	// Transient agentic-loop turns (tool results / ask answers) carry their
	// own payload — enrich them with the CHEAP cached tree only (never the
	// heavy re-walk / 300-file rescan). This guard lives HERE (not only in
	// agentWorkCmd) so a tool-result round can never re-walk the project
	// tree + rescan up to 300 files: the mismatch that made every tool round
	// pay seconds + thousands of tokens was exactly a caller-side prefix
	// check that missed the queue's "Tool Execution Output:" prefix.
	if isTransientTurn(q) {
		return attachTransientContext(q)
	}
	var ctxBuf strings.Builder
	seen := make(map[string]bool)

	// 1. Auto-attach workspace project tree structure — cached per working
	// directory (structure changes rarely; rebuilding it per prompt is
	// wasted I/O on every Enter).
	//
	// GATED on search intent (or planner mode, which won't call tools): a
	// plain question ("jelasin rotation feature") must NOT inherit the tree —
	// the model drives search itself with the `search` tool, exactly like
	// opencode (which never pre-injects a tree). Pre-injecting the tree to
	// EVERY prompt made weak free models answer from the tree alone instead
	// of researching the actual code — the root of the LineShape-vs-lead-
	// rotation class of wrong answers.
	if hasSearchIntent(q) || plannerMode {
		tree := cachedProjectTree()
		if tree != "" {
			ctxBuf.WriteString("PROJECT WORKSPACE TREE:\n```\n")
			ctxBuf.WriteString(tree)
			ctxBuf.WriteString("\n```\n\n")
		}
	}

	// 1b. Repository map (P6): the AST PageRank structural map — "find better,
	// not read more" (AGENT_LOOP §5.2). Only for depth-promised prompts
	// (planner mode / DeepPath), computed lazily once per project.
	if shouldAttachRepoMap(q, plannerMode) {
		if rm := cachedRepoMap(); rm != "" {
			ctxBuf.WriteString(rm + "\n")
		}
	}

	// 2. Line-range regex: matches app.go:100-200 or app.go#L50-100
	rangeRegex := regexp.MustCompile(`([a-zA-Z0-9_/-]+\.[a-zA-Z0-9_-]+)[:#][lL]?([0-9]+)(?:-([0-9]+))?`)
	for _, match := range rangeRegex.FindAllStringSubmatch(q, -1) {
		path := match[1]
		if seen[path] {
			continue
		}
		if data, err := os.ReadFile(path); err == nil {
			seen[path] = true
			lines := strings.Split(string(data), "\n")
			startLine := 1
			endLine := len(lines)
			if match[2] != "" {
				fmt.Sscanf(match[2], "%d", &startLine)
			}
			if match[3] != "" {
				fmt.Sscanf(match[3], "%d", &endLine)
			}
			if startLine < 1 {
				startLine = 1
			}
			if endLine > len(lines) {
				endLine = len(lines)
			}

			ctxBuf.WriteString(fmt.Sprintf("FILE ATTACHMENT: %s (lines %d-%d of %d)\n```\n", path, startLine, endLine, len(lines)))
			for i := startLine - 1; i < endLine && i < len(lines); i++ {
				ctxBuf.WriteString(fmt.Sprintf("%4d | %s\n", i+1, lines[i]))
			}
			ctxBuf.WriteString("```\n\n")
		}
	}

	// 3. Simple file path matches (e.g. README.md, Makefile, main.go). Files
	// already attached to a prior turn are skipped ONLY when their content is
	// unchanged — their text is already in the model's context, so re-reading
	// and re-attaching it every prompt would duplicate tokens. A file edited
	// since its last attach (mtime changed) is re-attached so the model never
	// reasons over a stale snapshot.
	already := sessionSeen()
	words := strings.Fields(q)
	for _, w := range words {
		clean := strings.Trim(w, "\"'`,()[]{}?")
		if clean == "" || seen[clean] || strings.Contains(clean, "..") {
			continue
		}
		if fi, err := os.Stat(clean); err == nil && !fi.IsDir() {
			if seenAt := fileSeen(already, clean); seenAt == fi.ModTime().UnixNano() {
				continue // unchanged since last attach — already in context
			}
			if data, err := os.ReadFile(clean); err == nil {
				seen[clean] = true
				lines := strings.Split(string(data), "\n")
				maxLines := min(120, len(lines))
				ctxBuf.WriteString(fmt.Sprintf("FILE ATTACHMENT: %s (%d lines · %.1f KB)\n```\n", clean, len(lines), float64(fi.Size())/1024.0))
				for i := 0; i < maxLines; i++ {
					ctxBuf.WriteString(lines[i] + "\n")
				}
				if len(lines) > maxLines {
					ctxBuf.WriteString(fmt.Sprintf("... (%d more lines truncated; use `read %s` for specific range)\n", len(lines)-maxLines, clean))
				}
				ctxBuf.WriteString("```\n\n")
			}
		}
	}

	// 4. Keyword search across workspace files if query contains specific
	// terms. Skipped for trivial conversational follow-ups ("lanjut", "gas",
	// "yes"): scanning up to 300 files for a 3-word continuation is pure
	// latency with zero added context.
	//
	// Narrowed: the scan now runs ONLY when the user explicitly asked to
	// find/investigate code ("cari", "cek", "search", "di mana", …) or named
	// a file. For a plain question ("jelasin rotation di CRM") it is SKIPPED:
	// pre-injecting speculative matches anchored the model on the WRONG file
	// (observed: a "rotation" match in LineShape.tsx — a visual editor line
	// node — hijacked the answer away from the CRM lead-rotation pipeline the
	// user meant). The model must drive search with intent (opencode pattern:
	// no context injection, model searches), not inherit the harness's guess.
	if !isTrivialFollowup(q) && hasSearchIntent(q) {
		if searchCtx := searchProjectFiles(q, seen); searchCtx != "" {
			ctxBuf.WriteString(searchCtx)
		}
	}

	// 5. Evidence pass (AGENTIC_OVERHAUL P1 — search-first guarantee).
	// NARROWED to planner mode only: brocode pre-injects a compact BM25-ranked
	// file list for planning so a planner that will not call tools still gets
	// evidence. For ordinary prompts it is SKIPPED — the model drives search
	// itself (stall recovery in agent.go still covers reasoning-only replies
	// with evidence when a weak model refuses to call tools). Pre-injecting
	// BM25 top-hits on every NORMAL/DEEP prompt biased the model toward the
	// first match (LineShape.tsx above) instead of answering from context.
	if plannerMode {
		if ev := explorationEvidence(q, seen); ev != "" {
			ctxBuf.WriteString(ev)
		}
	}

	if ctxBuf.Len() > 0 {
		var attachedList []string
		for f := range seen {
			attachedList = append(attachedList, f)
		}
		if symSummary := search.FormatSymbolSummary(attachedList); symSummary != "" {
			ctxBuf.WriteString(symSummary)
		}
		rememberAttached(seen)
		return ctxBuf.String() + "USER PROMPT:\n" + q
	}
	return q
}

// hasSearchIntent reports whether the user EXPLICITLY asked to find or
// investigate code — the only case where brocode pre-scans the workspace for
// keyword matches (step 4 above). Plain questions ("jelasin rotation di CRM")
// have no search intent: the model must drive search with its own context
// rather than inherit the harness's BM25 guess, which demonstrably anchored
// answers on the wrong file (LineShape.tsx visual-rotation instead of the CRM
// lead-rotation pipeline). Both Indonesian and English intents, plus an
// explicit file-ish token (path/extension/slash) which implies "look at this".
func hasSearchIntent(q string) bool {
	s := strings.ToLower(q)
	for _, w := range []string{
		"cari", "temukan", "cek", "riset", "tracing", "trace", "telusuri",
		"search", "find", "investigate", "explore", "look at", "look for",
		"di mana", "where is", "grep", "locate", "tunjukin", "tunjukkan",
	} {
		if strings.Contains(s, w) {
			return true
		}
	}
	for _, w := range strings.Fields(s) {
		if strings.Contains(w, ".") || strings.Contains(w, "/") {
			return true
		}
	}
	return false
}

// shouldAttachRepoMap reports whether the AST PageRank repo map should be
// attached for this prompt: planner mode (analysis-first) or a task the router
// scores DeepPath (≥ 7). Tool-result turns skip it — they carry their own
// payload. The map is expensive to build on a big repo, so it is lazy (P2) and
// cached per project.
func shouldAttachRepoMap(q string, plannerMode bool) bool {
	// Tool-result turns never re-attach the map (they carry their own payload)
	// — checked before planner mode so plan mode can't double-attach.
	if isTransientTurn(q) {
		return false
	}
	if plannerMode {
		return true
	}
	complexity, _ := agentic.EvaluateComplexity(q)
	return complexity == agentic.DeepPath
}

// cachedRepoMap builds the AST PageRank repo map once per working directory
// (structure changes rarely; the build walks + parses every source file once).
// Lazy: never computed unless shouldAttachRepoMap says a depth-promised prompt
// needs it.
func cachedRepoMap() string {
	wd, _ := os.Getwd()
	attachMu.Lock()
	defer attachMu.Unlock()
	if wd != repoMapDir {
		repoMapDir = wd
		repoMapVal = search.BuildRepoMap(wd, 0)
	}
	return repoMapVal
}

// stripEnrichedPrompt recovers the ORIGINAL user prompt from the context-
// enriched prompt attachFileContext builds (tree + attachments + evidence
// prepended, "USER PROMPT:\n" marker appended last). LastIndex is used so a
// literal "USER PROMPT:" inside an attached file never truncates early — the
// marker attachFileContext appends is always the final occurrence.
func stripEnrichedPrompt(q string) string {
	const marker = "USER PROMPT:\n"
	if idx := strings.LastIndex(q, marker); idx >= 0 {
		return q[idx+len(marker):]
	}
	return q
}

// searchProjectFiles performs auto-search across workspace files for query terms.
// Bounded work (anti-lag): ignored directories (.git, node_modules, …) are
// skipped entirely with SkipDir, the walk stops after maxSearchScanFiles files,
// and bundle-sized files (>256 KB) are never read — only files that could
// realistically carry the short query terms are opened.
func searchProjectFiles(q string, seen map[string]bool) string {
	var sb strings.Builder
	terms := tokenizeQuery(q)
	if len(terms) == 0 {
		return ""
	}
	// Files already attached to a prior turn are skipped by the scan too —
	// their content is already in the model's context; re-reading them on
	// every prompt is wasted I/O and duplicated tokens. An edited file (new
	// mtime) is rescanned so the model never reasons over a stale snapshot.
	already := sessionSeen()

	const (
		maxSearchMatches = 2         // at most 2 relevant files attached
		maxSearchScan    = 300       // stop walking after this many files
		maxSearchSize    = 256 << 10 // skip bundles/minified (>256 KB)
	)

	scanned := 0
	count := 0
	_ = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if shouldIgnorePath(path) {
			if info.IsDir() && path != "." {
				return filepath.SkipDir // never descend into .git/node_modules/vendor…
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if scanned >= maxSearchScan {
			return filepath.SkipAll
		}
		scanned++
		if count >= maxSearchMatches || info.Size() > maxSearchSize || seen[path] {
			return nil
		}
		if seenAt := fileSeen(already, path); seenAt == info.ModTime().UnixNano() {
			return nil // unchanged since last attach — already in context
		}
		if data, err := os.ReadFile(path); err == nil {
			content := string(data)
			score := calculateRelevance(content, terms)
			if score > 0 {
				seen[path] = true
				count++
				lines := strings.Split(content, "\n")
				maxL := min(60, len(lines))
				sb.WriteString(fmt.Sprintf("RELEVANT SEARCH CONTEXT (%s · relevance score %d):\n```\n", path, score))
				for i := 0; i < maxL; i++ {
					sb.WriteString(lines[i] + "\n")
				}
				if len(lines) > maxL {
					sb.WriteString(fmt.Sprintf("... (%d more lines)\n", len(lines)-maxL))
				}
				sb.WriteString("```\n\n")
			}
		}
		return nil
	})
	return sb.String()
}

// tokenizeQuery splits a query into lowercase terms for relevance matching.
func tokenizeQuery(q string) []string {
	var terms []string
	for _, w := range strings.Fields(strings.ToLower(q)) {
		w = strings.Trim(w, "\"'`,()[]{}?")
		if len(w) > 2 {
			terms = append(terms, w)
		}
	}
	return terms
}

// shouldIgnorePath returns true for common non-source directories across all ecosystems.
func shouldIgnorePath(path string) bool {
	ignore := []string{
		".git", "node_modules", "vendor", ".brocode", ".agents", ".kuma", ".opencode",
		".matcha", "bin", "dist",
		".venv", "venv", "__pycache__", ".next", ".nuxt", ".svelte-kit", "target",
		"build", "out", ".gradle", ".idea", ".vscode", "coverage", ".turbo",
	}
	for _, ig := range ignore {
		if strings.Contains(path, "/"+ig+"/") || strings.HasPrefix(path, ig+"/") || path == ig {
			return true
		}
	}
	return false
}

// calculateRelevance scores content against query terms using simple keyword matching.
func calculateRelevance(content string, terms []string) int {
	lower := strings.ToLower(content)
	score := 0
	for _, t := range terms {
		score += strings.Count(lower, t)
	}
	return score
}

// ── Evidence pass (AGENTIC_OVERHAUL P1 — search-first guarantee) ──────────
// The default loop is reactive: "User → fast context scan → LLM → tool call → …".
// But when the model will not cooperate with tool calls (weak free models stall
// and reply with reasoning only), the scan must still deliver evidence.
//
// Design rule: NO prompt-keyword classifier. Enterprise agents (Claude Code
// "grep in a loop", opencode explore subagent) let the MODEL drive search
// with fast tools; the harness's job is to keep the loop alive. brocode only
// pre-injects evidence where the mode/router already promised depth (plan
// mode, NORMAL/DEEP tasks) or in the stall-recovery path — the trigger there
// is an OBSERVED failure (reasoning-only reply), not a heuristic. Ranking and
// stopword suppression are left to BM25's IDF, which is statistical and
// language-agnostic — no handcrafted word lists.

// evidenceQuery derives the BM25 query from the raw prompt. BM25's IDF
// downweights common words statistically, so no stopword list is needed; the
// only guard is requiring meaningful tokens (≥ 3 chars) so pure-noise prompts
// ("ok", "...", punctuation) never trigger an index scan.
func evidenceQuery(q string) string {
	var toks []string
	for _, w := range strings.Fields(strings.ToLower(q)) {
		w = strings.Trim(w, "\"'`,()[]{}?:;.!")
		if len([]rune(w)) >= 3 {
			toks = append(toks, w)
		}
	}
	return strings.Join(toks, " ")
}

// explorationEvidence runs the cached BM25 workspace index for q and returns a
// compact evidence block: top-ranked file paths, a term-matched snippet per
// file, and a symbol map. Bounded by construction (≤ evidenceTopK files, ≤
// evidenceSnippetMax runes per snippet, symbol map caps itself) so the
// evidence never floods the context (P1). seen skips files whose content was
// already attached this turn.
const (
	evidenceTopK        = 5
	evidenceSnippetMax  = 120
	evidenceScanLines   = 100
	evidenceSymbolFiles = 2 // symbol map is capped tighter than the file list (token discipline)
)

func explorationEvidence(q string, seen map[string]bool) string {
	query := evidenceQuery(q)
	if query == "" {
		return ""
	}
	results := fileSearch(query, evidenceTopK)
	if len(results) == 0 {
		return ""
	}

	const evidenceMaxBytes = 2048 // hard cap on the whole block (P1 token discipline)
	var sb strings.Builder
	sb.WriteString("WORKSPACE EXPLORATION (auto-run evidence pass, read-only):\n")
	terms := strings.Fields(query)
	var paths []string
	for _, r := range results {
		if seen != nil && seen[r.ID] {
			continue
		}
		paths = append(paths, r.ID)
		sb.WriteString(fmt.Sprintf("  • %s (relevance %.1f) — %s\n", r.ID, r.Score, matchedSnippet(r.ID, terms, r.Snippet)))
	}
	if len(paths) == 0 {
		return ""
	}
	footer := "\nRead any file above with the `read` tool before editing. If a path matches the request, open it — do not answer from memory.\n"
	// Symbol map capped to the TOP-ranked files only — the map is dense (one
	// line per symbol). It is DROPPED (never truncated mid-list) when it would
	// push the block past the cap: the file list is the contract, symbols are
	// garnish.
	if sym := search.FormatSymbolSummary(paths[:min(len(paths), evidenceSymbolFiles)]); sym != "" {
		if sb.Len()+len(sym)+len(footer) <= evidenceMaxBytes {
			sb.WriteString("\n" + sym)
		}
	}
	sb.WriteString(footer)
	if sb.Len() > evidenceMaxBytes { // final guard — never exceed the cap
		return string([]rune(sb.String())[:evidenceMaxBytes]) + "\n… (evidence truncated)\n"
	}
	return sb.String()
}

// matchedSnippet returns a short, term-anchored preview of a file: the first
// line within the first evidenceScanLines that contains a query term
// (case-insensitive), clipped to evidenceSnippetMax runes; falls back to the
// index snippet (first non-empty line) when no line matches.
func matchedSnippet(path string, terms []string, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return clipEvidence(fallback)
	}
	lines := strings.Split(string(data), "\n")
	upper := lines
	if len(upper) > evidenceScanLines {
		upper = upper[:evidenceScanLines]
	}
	for _, ln := range upper {
		l := strings.ToLower(ln)
		for _, t := range terms {
			if strings.Contains(l, t) {
				return clipEvidence(strings.TrimSpace(ln))
			}
		}
	}
	return clipEvidence(fallback)
}

// clipEvidence bounds a snippet to evidenceSnippetMax runes with an ellipsis.
func clipEvidence(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no preview)"
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= evidenceSnippetMax {
		return string(r)
	}
	return string(r[:evidenceSnippetMax]) + "…"
} // getProjectTree returns a concise workspace directory tree. Bounded work:
// once treeMaxEntries entries have been collected the walk stops entirely
// (SkipAll) — it never keeps descending through the rest of a huge repo just
// to produce nothing more.
func getProjectTree() string {
	const treeMaxEntries = 35
	var sb strings.Builder
	count := 0
	_ = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if count > treeMaxEntries {
			return filepath.SkipAll
		}
		if shouldIgnorePath(path) {
			if info.IsDir() && path != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if path == "." {
			return nil
		}
		depth := strings.Count(path, string(filepath.Separator))
		indent := strings.Repeat("  ", depth)
		if info.IsDir() {
			sb.WriteString(fmt.Sprintf("%s📁 %s/\n", indent, filepath.Base(path)))
		} else {
			sb.WriteString(fmt.Sprintf("%s📄 %s\n", indent, filepath.Base(path)))
		}
		count++
		return nil
	})
	return sb.String()
}

// extractDynamicSubagents inspects user query AND agent response text for
// @name mentions (explicit role references). These are PROMPT CONTEXT, not
// spawned workers: brocode runs a single agent loop, so an @mention steers
// the same model via the prompt text — it never launches a separate process
// or model. The panel records the mention as done and the trace says exactly
// that; the old "spawn → delegated to model X" traces were fictional (the
// named model was never called) and were removed.
func extractDynamicSubagents(userQuery, text string) ([]subagentState, []string) {
	var subagents []subagentState
	var traceLogs []string
	seen := make(map[string]bool)

	// Check if user explicitly invoked a subagent (@planner, @auditor, etc.)
	subReg := regexp.MustCompile(`(?i)@([a-zA-Z0-9_-]{3,30})`)
	userMatches := subReg.FindAllStringSubmatch(userQuery, -1)

	// Check if agent explicitly declared delegation ("delegating to @...", "spawning @...")
	delegReg := regexp.MustCompile(`(?i)(?:delegating to|spawning|delegate to|executing)\s+@([a-zA-Z0-9_-]{3,30})`)
	agentMatches := delegReg.FindAllStringSubmatch(text, -1)

	allMatches := append(userMatches, agentMatches...)
	for _, m := range allMatches {
		rawName := strings.ToLower(m[1])
		if rawName == "" || seen[rawName] || rawName == "brocode" || rawName == "here" || rawName == "everyone" {
			continue
		}
		seen[rawName] = true
		subagents = append(subagents, subagentState{
			name:   rawName,
			task:   "agent context",
			status: "done",
		})
		traceLogs = append(traceLogs, fmt.Sprintf("● @%s → agent context (single agent loop)", rawName))
	}
	return subagents, traceLogs
}

// savePlan automatically saves/persists engineering plans into .agents/plan/current.md
func savePlan(text string) string {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "plan") && !strings.Contains(lower, "roadmap") && !strings.Contains(lower, "step 1") && !strings.Contains(lower, "checkpoint") {
		return ""
	}
	_ = os.MkdirAll(".agents/plan", 0o755)
	planPath := ".agents/plan/current.md"
	if err := os.WriteFile(planPath, []byte(text), 0o644); err == nil {
		return planPath
	}
	return ""
}
