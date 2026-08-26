// Package memory implements BroCode's cross-session project memory.
//
// Every session starts cold; memory is the layer that lets a new session
// start warm. It persists durable facts about the project (architecture,
// build/test commands, decisions, gotchas) to .brocode/memory.md and makes
// them available three ways:
//
//  1. Warm start — a capped excerpt of the memory file is injected into the
//     system prompt at session start, so the agent already knows what past
//     sessions learned without grep/glob.
//  2. memory tool — the agent can recall (BM25 relevance), retain (add a
//     fact), or list the memory during a turn.
//  3. Auto-extract — on context compaction, the compaction summary's durable
//     decisions are merged into memory automatically.
//
// The file lives next to the project config (.brocode/memory.md) so it can be
// committed for the team or git-ignored for personal notes, matching the
// global/project layering used elsewhere in BroCode.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/store"
)

// maxWarmStartLines caps how much of the memory file is injected into the
// system prompt (research-backed: Claude Code auto-memory loads ~200 lines).
const maxWarmStartLines = 200

// maxWarmStartBytes caps the warm-start block size (25KB, same as Claude).
const maxWarmStartBytes = 25 * 1024

// maxRetainLines caps the memory file size; oldest entries are pruned first.
const maxRetainLines = 600

// memoryEntryTTL is how long a memory fact stays fresh before it is eligible
// for pruning by PruneStale. Facts referenced by warm-start searches within
// this window are kept; orphaned facts older than it are dropped.
const memoryEntryTTL = 30 * 24 * time.Hour // 30 days

// Store reads and writes the project memory file.
type Store struct {
	// path is the absolute path of the memory file (.brocode/memory.md).
	path string
	// facts holds the current parsed facts, keyed by section.
	facts map[string][]string
	// loaded guards load(): facts are parsed from disk once and then kept in
	// sync with the file via append-only writes, so Retain does not pay a full
	// re-read + re-parse on every call (the hot path when the agent records
	// many facts in one turn).
	loaded bool
	// mu serializes fact mutations (Retain/PruneStale) against concurrent writes.
	mu sync.Mutex
	// embedder, when set, enables hybrid retrieval: WarmStartRelevant re-ranks
	// the BM25 candidates by embedding cosine similarity (search_code's pattern).
	// Nil keeps BM25-only operation.
	embedder *search.Embedder
	// warmMaxBytes overrides the default warm-start byte cap when the engine
	// decides the active turn is already close to the window limit, a 25KB
	// memory block could itself trigger compaction).
	// Pass n <= 0 to restore the default cap.
	warmMaxBytes int
}

// SetWarmStartBudget caps the warm-start injection for the NEXT warm-start
// call (adaptive context budgeting: when the active turn is already close to
// the window limit, a 25KB memory block could itself trigger compaction).
// Pass n <= 0 to restore the default cap.
func (s *Store) SetWarmStartBudget(n int) {
	if s == nil {
		return
	}
	s.warmMaxBytes = n
}

// SetEmbedder wires an embeddings endpoint for hybrid memory retrieval. Nil
// (or a later nil) disables it — retrieval gracefully degrades to BM25-only.
func (s *Store) SetEmbedder(e *search.Embedder) {
	if s == nil {
		return
	}
	s.embedder = e
}

// NewStore opens (or creates on first write) the project memory file under
// workspaceDir/.brocode/memory.md. Returns nil if no workspace dir.
func NewStore(workspaceDir string) *Store {
	if workspaceDir == "" {
		return nil
	}
	return &Store{
		path:  filepath.Join(workspaceDir, ".brocode", "memory.md"),
		facts: map[string][]string{},
	}
}

// Path returns the memory file location (empty if store is nil).
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// load reads the memory file and parses it into sectioned facts. It tolerates
// duplicate section headers (each `## ` header appends rather than resets its
// section) so append-only writes that repeat a header stay parse-correct.
//
// Callers already holding s.mu must use loadUnlocked(); everyone else uses
// load(), which locks. Without this, concurrent warm-start reads race the
// PruneStale/Retain mutations of s.facts (reported DATA RACE under -race).
func (s *Store) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadUnlocked()
}

// loadUnlocked parses the memory file into s.facts. Callers MUST hold s.mu.
func (s *Store) loadUnlocked() {
	s.facts = map[string][]string{}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var section string
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "## "); ok {
			section = after
			if _, ok := s.facts[section]; !ok {
				s.facts[section] = []string{}
			}
			continue
		}
		if section != "" && strings.HasPrefix(trimmed, "- ") {
			s.facts[section] = append(s.facts[section], strings.TrimPrefix(trimmed, "- "))
		}
	}
}

// Save writes the facts back to the file, pruning to the size cap.
func (s *Store) Save() error {
	if s == nil || s.path == "" {
		return nil
	}
	// Deterministic section order (Architecture, Build & Test, Decisions,
	// Gotchas, ... alphabetically) so diffs are stable.
	sections := make([]string, 0, len(s.facts))
	for k := range s.facts {
		sections = append(sections, k)
	}
	sort.Strings(sections)

	var sb strings.Builder
	sb.WriteString("# Memory — project knowledge learned across sessions\n")
	sb.WriteString("# Auto-maintained by BroCode. Edit freely; entries persist for warm starts.\n\n")
	lines := 2
	for _, sec := range sections {
		items := s.facts[sec]
		if len(items) == 0 {
			continue
		}
		if lines >= maxRetainLines {
			break
		}
		sb.WriteString("## ");sb.WriteString(sec);sb.WriteString("\n")
		lines++
		for _, f := range items {
			if lines >= maxRetainLines {
				break
			}
			sb.WriteString("- ");sb.WriteString(f);sb.WriteString("\n")
			lines++
		}
		sb.WriteString("\n")
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, []byte(sb.String()), 0o644)
}

// PruneStale drops memory facts older than memoryEntryTTL to keep the memory
// file focused on the current project state. It is safe to call at session
// start — the cost is one file read + one BM25-free pass. Returns the number
// of facts pruned. Facts with embedded timestamps (ISO-8601 prefix) are
// evaluated by date; facts without timestamps are retained (conservative:
// never lose a fact we can't date).
func (s *Store) PruneStale() int {
	if s == nil || s.path == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// loaded may already be true if Retain ran this session; reuse the cached
	// facts so pruning stays consistent with appended writes.
	if !s.loaded {
		s.loadUnlocked()
		s.loaded = true
	}
	pruned := 0
	cutoff := time.Now().Add(-memoryEntryTTL)
	for sec, items := range s.facts {
		if sec == "" || sec == "Architecture" || sec == "Build & Test" {
			// Never auto-prune structural/architecture facts — they define
			// the project even if not recently referenced.
			continue
		}
		var kept []string
		for _, f := range items {
			ts := extractFactTimestamp(f)
			if ts != nil && ts.Before(cutoff) {
				pruned++
				continue
			}
			kept = append(kept, f)
		}
		s.facts[sec] = kept
	}
	if pruned > 0 {
		_ = s.Save()
	}
	return pruned
}

// extractFactTimestamp looks for an ISO-8601 date prefix (YYYY-MM-DD) or a
// trailing "[YYYY-MM-DD]" marker in a fact string. Returns the parsed time
// or nil if no timestamp is found.
func extractFactTimestamp(fact string) *time.Time {
	// Try leading "YYYY-MM-DD" prefix.
	if len(fact) >= 10 {
		if t, err := time.Parse("2006-01-02", fact[:10]); err == nil {
			return &t
		}
	}
	// Try trailing "[YYYY-MM-DD]" suffix.
	if idx := strings.LastIndex(fact, "["); idx >= 0 {
		suffix := fact[idx+1:]
		if len(suffix) >= 11 && suffix[10] == ']' {
			if t, err := time.Parse("2006-01-02", suffix[:10]); err == nil {
				return &t
			}
		}
	}
	return nil
}

// WarmStart returns a capped excerpt of memory for system-prompt injection.
// Returns "" when the store is empty or nil.
func (s *Store) WarmStart() string {
	if s == nil {
		return ""
	}
	s.load()

	s.mu.Lock()
	// Deterministic section order: WarmStart feeds the system prompt, which is
	// part of the stable prefix that prompt caching keys off. Map iteration
	// would randomize section order per call, silently invalidating the cache
	// every round. Sections are sorted; items keep their stored order.
	sections := make([]string, 0, len(s.facts))
	for sec := range s.facts {
		sections = append(sections, sec)
	}
	sort.Strings(sections)

	var sb strings.Builder
	for _, sec := range sections {
		items := s.facts[sec]
		if len(items) == 0 {
			continue
		}
		sb.WriteString("## ");sb.WriteString(sec);sb.WriteString("\n")
		for _, f := range items {
			sb.WriteString("- ");sb.WriteString(f);sb.WriteString("\n")
		}
	}
	s.mu.Unlock()

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return ""
	}
	// Cap by lines then bytes. The byte cap is adaptive: SetWarmStartBudget
	// (the engine's remaining-window calculation) can shrink the default 25KB.
	maxBytes := maxWarmStartBytes
	if s.warmMaxBytes > 0 && s.warmMaxBytes < maxBytes {
		maxBytes = s.warmMaxBytes
	}
	lines := strings.Split(out, "\n")
	if len(lines) > maxWarmStartLines {
		lines = lines[:maxWarmStartLines]
		out = strings.Join(lines, "\n") + "\n… (truncated)"
	}
	if len(out) > maxBytes {
		out = out[:maxBytes] + "\n… (truncated)"
	}
	return out
}

// WarmStartRelevant returns a query-filtered dynamic slice of memory facts.
// When query is non-empty, it selects the top relevant facts matching the active
// task using BM25 relevance scoring, saving 70-90% of token overhead.
func (s *Store) WarmStartRelevant(query string) string {
	if s == nil {
		return ""
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return s.WarmStart()
	}
	s.load()

	s.mu.Lock()
	if len(s.facts) == 0 {
		s.mu.Unlock()
		return ""
	}

	var docs []search.Document
	for sec, items := range s.facts {
		for _, f := range items {
			docs = append(docs, search.Document{
				ID:    sec,
				Title: sec,
				Body:  f,
			})
		}
	}
	s.mu.Unlock()
	if len(docs) == 0 {
		return ""
	}
	idx := search.NewBM25(docs)
	results := idx.Search(query, 8)
	if len(results) == 0 {
		return s.WarmStart()
	}
	// Hybrid retrieval: when an embeddings endpoint is wired, re-rank the BM25
	// candidates by semantic similarity (the top-8 cap keeps the embedding
	// batch bounded). Any embedding failure falls back to BM25 order untouched.
	if s.embedder != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		results = search.ReRankDocs(ctx, query, results, s.embedder, 8)
		cancel()
	}

	var sb strings.Builder
	seen := map[string]bool{}
	for _, res := range results {
		fact := strings.TrimSpace(res.Doc.Body)
		if seen[fact] {
			continue
		}
		seen[fact] = true
		fmt.Fprintf(&sb, "- [%s] %s\n", res.Doc.Title, fact)
	}
	return strings.TrimSpace(sb.String())
}

// MemoryIndex returns a compact table of contents for all memory sections.
// This is the "L1 pointer file" from Claude Code's 3-layer memory architecture:
// a lightweight index that's always loaded into context, telling the agent
// WHAT knowledge exists and WHERE to look. After compaction, the agent still
// knows the project's structure without re-reading everything.
//
// Example output:
//   📚 Memory Index (use memory tool to recall details):
//   • Architecture (5 facts): auth flow, DB schema, API routes
//   • Conventions (3 facts): naming, error handling, testing
//   • Gotchas (2 facts): race condition in cache, N+1 query
func (s *Store) MemoryIndex() string {
	if s == nil {
		return ""
	}
	s.load()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.facts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("📚 Memory Index (use memory tool to recall details):\n")
	for sec, items := range s.facts {
		if len(items) == 0 {
			continue
		}
		// Show first 3 items as preview
		preview := make([]string, 0, 3)
		for i, f := range items {
			if i >= 3 {
				preview = append(preview, "...")
				break
			}
			// Truncate long facts for preview
			short := f
			if len(short) > 60 {
				short = short[:57] + "..."
			}
			preview = append(preview, short)
		}
		fmt.Fprintf(&sb, "• %s (%d facts): %s\n", sec, len(items), strings.Join(preview, ", "))
	}
	return sb.String()
}

// Recall searches memory facts by BM25 relevance to query, returning the top
// matches formatted for the model. Returns a friendly "no memory" note when
// empty.
func (s *Store) Recall(query string, limit int) string {
	if s == nil {
		return "ℹ️ No project memory store."
	}
	s.load()
	var docs []search.Document
	for sec, items := range s.facts {
		for _, f := range items {
			docs = append(docs, search.Document{
				ID:    sec,
				Title: sec,
				Body:  f,
			})
		}
	}
	if len(docs) == 0 {
		return "ℹ️ Project memory is empty. Use memory retain to store facts as you learn them."
	}
	if limit <= 0 || limit > 10 {
		limit = 5
	}
	idx := search.NewBM25(docs)
	results := idx.Search(query, limit)

	// FormatResults renders each fact as "[section] body" (ID = section,
	// body = the fact) with a relevance score and snippet.
	return "PROJECT MEMORY MATCHES:\n" + search.FormatResults(results, query) +
		"\n\nUse these as verified prior knowledge; you may still read files to confirm details."
}

// Retain adds a fact to a section, deduplicating near-identical entries.
// It returns true when a new fact was added. Sections default to "Notes".
func (s *Store) Retain(section, fact string) (bool, error) {
	if s == nil || s.path == "" {
		return false, nil
	}
	section = strings.TrimSpace(section)
	if section == "" {
		section = "Notes"
	}
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return false, nil
	}
	// Anti-feedback-loop: never store our own memory prompt block or the
	// engine's placeholder compaction text.
	if strings.Contains(fact, "PROJECT MEMORY MATCHES") ||
		strings.Contains(fact, "Auto-maintained by BroCode") ||
		strings.Contains(fact, "preserve memory window") ||
		strings.Contains(fact, "Context compacted successfully") {
		return false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Lazy load: parse from disk once, then keep s.facts in sync with the file
	// via append-only writes. Avoids a full re-read + re-parse on every Retain
	// (the hot path when the agent records many facts in one turn).
	if !s.loaded {
		s.loadUnlocked()
		s.loaded = true
	}
	norm := normalize(fact)
	for _, existing := range s.facts[section] {
		if normalize(existing) == norm {
			return false, nil // exact duplicate
		}
		if len(norm) > 40 && len(existing) >= 40 && strings.Contains(norm, existing[:40]) {
			return false, nil // near-duplicate prefix
		}
	}

	s.facts[section] = append(s.facts[section], fact)
	// Prune: cap total lines by trimming the longest section's oldest entries.
	if totalLines(s.facts) > maxRetainLines {
		trimFacts(s.facts, maxRetainLines)
		if err := s.Save(); err != nil {
			return false, err
		}
		return true, nil
	}
	// Common path: append-only write (no full file rewrite).
	if err := s.appendFact(section, fact); err != nil {
		return false, err
	}
	return true, nil
}

// appendFact writes a single fact to the memory file without rewriting the
// whole file. It always emits the section header alongside the bullet so the
// line parses under the correct section regardless of where it lands in the
// file (load() tolerates duplicate section headers). A freshly created file
// gets the standard header comment first.
func (s *Store) appendFact(section, fact string) error {
	needHeader := false
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		needHeader = true
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if needHeader {
		if _, err := f.WriteString(
			"# Memory — project knowledge learned across sessions\n" +
				"# Auto-maintained by BroCode. Edit freely; entries persist for warm starts.\n\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString("## " + section + "\n- " + fact + "\n")
	return err
}

// retainWithTimestamp adds a fact tagged with the current date for TTL tracking.
// Used by sections that benefit from auto-pruning (Notes, Gotchas, Session Notes).
func (s *Store) retainWithTimestamp(section, fact string) (bool, error) {
	if s == nil || s.path == "" {
		return false, nil
	}
	section = strings.TrimSpace(section)
	if section == "" {
		section = "Notes"
	}
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return false, nil
	}
	// Don't double-tag if already has a timestamp.
	var tagged string
	if extractFactTimestamp(fact) == nil {
		tagged = time.Now().Format("2006-01-02") + " " + fact
	} else {
		tagged = fact
	}
	return s.Retain(section, tagged)
}

// List returns all memory facts formatted for the model (used by /memory).
func (s *Store) List() string {
	if s == nil {
		return "ℹ️ No project memory store."
	}
	s.load()
	var sb strings.Builder
	secs := make([]string, 0, len(s.facts))
	for k := range s.facts {
		secs = append(secs, k)
	}
	sort.Strings(secs)
	any := false
	for _, sec := range secs {
		if len(s.facts[sec]) == 0 {
			continue
		}
		any = true
		sb.WriteString("## ");sb.WriteString(sec);sb.WriteString("\n")
		for _, f := range s.facts[sec] {
			sb.WriteString("- ");sb.WriteString(f);sb.WriteString("\n")
		}
	}
	if !any {
		return "ℹ️ Project memory is empty."
	}
	return strings.TrimSpace(sb.String())
}

// CaptureMinerFindings persists what a MINER turn actually examined plus the
// model's own synthesized summary into project memory. It parses markdown headers
// (## Architecture, ## Build & Test, ## Decisions, ## Gotchas) and bullet points,
// storing structured facts into their respective sections in memory.md.
func (s *Store) CaptureMinerFindings(answer string, files []string) error {
	if s == nil {
		return nil
	}
	changed := false
	if len(files) > 0 {
		list := files
		if len(list) > 12 {
			list = list[:12] // keep the memory file lean
		}
		if ok, err := s.Retain("Architecture", "MINER explored: "+strings.Join(list, ", ")); err == nil && ok {
			changed = true
		}
	}
	ans := strings.TrimSpace(answer)
	// Only persist substantial answers (a greeting or empty reply is noise).
	if len(ans) >= 40 {
		lines := strings.Split(ans, "\n")
		currentSec := "Notes"
		parsedAny := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if after, found := strings.CutPrefix(trimmed, "## "); found {
				currentSec = after
				continue
			}
			
			var fact string
			if after, found := strings.CutPrefix(trimmed, "- "); found {
				fact = after
			} else if after, found := strings.CutPrefix(trimmed, "* "); found {
				fact = after
			}

			if len(fact) >= 15 {
				if ok, err := s.Retain(currentSec, fact); err == nil && ok {
					changed = true
					parsedAny = true
				}
			}
		}
		// Fallback for plain un-sectioned text
		if !parsedAny {
			if len(ans) > 600 {
				ans = ans[:600] + "…"
			}
			if ok, err := s.Retain("Notes", "MINER findings: "+ans); err == nil && ok {
				changed = true
			}
		}
	}
	if changed {
		return s.Save()
	}
	return nil
}

// SkillGotchas returns the distilled repair lessons recorded for a skill
// (## Skill Notes entries prefixed "<skill>: "), oldest first. The engine uses
// it to decide when a skill has accumulated enough real gotchas (≥2) to
// warrant proposing a patch to its SKILL.md.
func (s *Store) SkillGotchas(skill string) []string {
	if s == nil || skill == "" {
		return nil
	}
	s.load() // ensure cross-session facts are in memory before scanning
	prefix := skill + ": "
	var out []string
	for _, f := range s.facts["Skill Notes"] {
		if after, ok := strings.CutPrefix(f, prefix); ok && strings.TrimSpace(after) != "" {
			out = append(out, after)
		}
	}
	return out
}

// CaptureOutOfScopeFindings persists the "### OUT-OF-SCOPE FINDINGS" section a
// BUILDER turn's answer ends with (rule b13: capture off-task issues instead of
// chasing them mid-task) into project memory, so a follow-up task can pick them
// up instead of losing them to the chat history. Returns how many facts were
// retained (0 when the answer has no such section or nothing new).
func (s *Store) CaptureOutOfScopeFindings(answer string) int {
	if s == nil || strings.TrimSpace(answer) == "" {
		return 0
	}
	lines := strings.Split(answer, "\n")
	inSection := false
	var facts []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		// Header matches "### OUT-OF-SCOPE FINDINGS" (also bare "## " and
		// case variations); a later '#' header ends the section.
		if strings.HasPrefix(lower, "#") {
			if strings.Contains(lower, "out-of-scope") || strings.Contains(lower, "out of scope") {
				inSection = true
			} else {
				inSection = false
			}
			continue
		}
		if !inSection {
			continue
		}
		fact := strings.TrimPrefix(strings.TrimPrefix(trimmed, "- "), "* ")
		if len(fact) < 15 {
			continue
		}
		facts = append(facts, fact)
	}
	if len(facts) > 5 {
		facts = facts[:5] // keep the memory file lean
	}
	changed := 0
	for _, f := range facts {
		if ok, err := s.retainWithTimestamp("Notes", "Out-of-scope: "+f); err == nil && ok {
			changed++
		}
	}
	if changed > 0 {
		_ = s.Save()
	}
	return changed
}

// MergeCompaction persists durable facts from a compaction summary into the
// Decisions/Gotchas sections automatically (auto-extract on context loss).
func (s *Store) MergeCompaction(goal string, decisions []string, state string) error {
	if s == nil {
		return nil
	}
	changed := false
	if goal != "" && goal != "Continue active conversation" {
		if ok, err := s.retainWithTimestamp("Notes", "Session goal: "+goal); err == nil && ok {
			changed = true
		}
	}
	for _, d := range decisions {
		if d == "" || strings.Contains(d, "preserve memory window") {
			continue
		}
		if ok, err := s.retainWithTimestamp("Decisions", d); err == nil && ok {
			changed = true
		}
	}
	if state != "" && state != "Context compacted successfully" {
		if ok, err := s.retainWithTimestamp("Notes", "Last known state: "+state); err == nil && ok {
			changed = true
		}
	}
	if changed {
		return s.Save()
	}
	return nil
}

// CaptureGotcha records a project-specific trap, gotcha, or failure pattern
// so the agent never repeats the same mistake in future sessions.
func (s *Store) CaptureGotcha(contextHint, gotcha string) error {
	if s == nil {
		return nil
	}
	gotcha = strings.TrimSpace(gotcha)
	if len(gotcha) < 10 {
		return nil
	}
	entry := gotcha
	if contextHint != "" {
		entry = fmt.Sprintf("[%s] %s", contextHint, gotcha)
	}
	if ok, err := s.Retain("Gotchas", entry); err == nil && ok {
		return s.Save()
	}
	return nil
}

// CaptureSession extracts durable facts from a finished session's events
// WITHOUT calling the LLM (deterministic, non-blocking — safe to run on quit):
//   - the last real user prompt becomes a session goal note
//   - files the agent wrote/edited are recorded under a session note
//
// This complements auto-extract-on-compaction: short sessions that never hit
// the compaction threshold still leave a trace in project memory.
func (s *Store) CaptureSession(sessionID string, events []store.Event) error {
	if s == nil {
		return nil
	}
	var lastGoal string
	edited := map[string]bool{}
	changed := false

	for _, ev := range events {
		var msg provider.Message
		if json.Unmarshal([]byte(ev.PayloadJSON), &msg) != nil {
			continue
		}
		switch ev.Type {
		case "user_msg":
			if c := strings.TrimSpace(msg.Content); c != "" && !strings.HasPrefix(c, "⚠️") && !strings.HasPrefix(c, "📖") {
				lastGoal = c
			}
		case "assistant_msg":
			for _, tc := range msg.ToolCalls {
				if tc.Name == "write_file" || tc.Name == "edit_file" {
					var args struct {
						Path string `json:"path"`
					}
					if json.Unmarshal([]byte(tc.Arguments), &args) == nil && args.Path != "" {
						edited[args.Path] = true
					}
				}
			}
		}
	}

	if lastGoal != "" {
		// Truncate very long prompts to keep the memory file lean.
		if len(lastGoal) > 200 {
			lastGoal = lastGoal[:200] + "…"
		}
		if ok, err := s.retainWithTimestamp("Session Notes", "Goal: "+lastGoal); err == nil && ok {
			changed = true
		}
	}
	if len(edited) > 0 {
		paths := make([]string, 0, len(edited))
		for p := range edited {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		if len(paths) > 8 {
			paths = paths[:8]
		}
		if ok, err := s.retainWithTimestamp("Session Notes", "Touched files: "+strings.Join(paths, ", ")); err == nil && ok {
			changed = true
		}
	}
	if changed {
		return s.Save()
	}
	return nil
}

func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func totalLines(facts map[string][]string) int {
	n := 0
	for _, items := range facts {
		n += len(items)
	}
	return n
}

// trimFacts removes oldest entries from the largest section until under cap.
func trimFacts(facts map[string][]string, cap int) {
	for totalLines(facts) > cap {
		var biggest string
		max := -1
		for sec, items := range facts {
			if len(items) > max {
				max = len(items)
				biggest = sec
			}
		}
		if biggest == "" || len(facts[biggest]) == 0 {
			return
		}
		facts[biggest] = facts[biggest][1:]
	}
}
