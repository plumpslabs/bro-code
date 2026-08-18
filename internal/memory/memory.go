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

// Store reads and writes the project memory file.
type Store struct {
	// path is the absolute path of the memory file (.brocode/memory.md).
	path string
	// facts holds the current parsed facts, keyed by section.
	facts map[string][]string
	// embedder, when set, enables hybrid retrieval: WarmStartRelevant re-ranks
	// the BM25 candidates by embedding cosine similarity (search_code's pattern).
	// Nil keeps BM25-only operation.
	embedder *search.Embedder
	// warmMaxBytes overrides the default warm-start byte cap when the engine
	// decides the remaining window cannot afford the full 25KB block (adaptive
	// context budgeting). 0 keeps the default maxWarmStartBytes.
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

// load reads the memory file and parses it into sectioned facts.
func (s *Store) load() {
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
			s.facts[section] = []string{}
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

// WarmStart returns a capped excerpt of memory for system-prompt injection.
// Returns "" when the store is empty or nil.
func (s *Store) WarmStart() string {
	if s == nil {
		return ""
	}
	s.load()
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
	if len(s.facts) == 0 {
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

	s.load()
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
	}
	if err := s.Save(); err != nil {
		return false, err
	}
	return true, nil
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
		if ok, err := s.Retain("Notes", "Out-of-scope: "+f); err == nil && ok {
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
		if ok, err := s.Retain("Notes", "Session goal: "+goal); err == nil && ok {
			changed = true
		}
	}
	for _, d := range decisions {
		if d == "" || strings.Contains(d, "preserve memory window") {
			continue
		}
		if ok, err := s.Retain("Decisions", d); err == nil && ok {
			changed = true
		}
	}
	if state != "" && state != "Context compacted successfully" {
		if ok, err := s.Retain("Notes", "Last known state: "+state); err == nil && ok {
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

	changed := false
	if lastGoal != "" {
		// Truncate very long prompts to keep the memory file lean.
		if len(lastGoal) > 200 {
			lastGoal = lastGoal[:200] + "…"
		}
		if ok, err := s.Retain("Session Notes", "Goal: "+lastGoal); err == nil && ok {
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
		if ok, err := s.Retain("Session Notes", "Touched files: "+strings.Join(paths, ", ")); err == nil && ok {
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
