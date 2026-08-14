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
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			section = strings.TrimPrefix(trimmed, "## ")
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
		sb.WriteString("## " + sec + "\n")
		lines++
		for _, f := range items {
			if lines >= maxRetainLines {
				break
			}
			sb.WriteString("- " + f + "\n")
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
	var sb strings.Builder
	for sec, items := range s.facts {
		if len(items) == 0 {
			continue
		}
		sb.WriteString("## " + sec + "\n")
		for _, f := range items {
			sb.WriteString("- " + f + "\n")
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return ""
	}
	// Cap by lines then bytes.
	lines := strings.Split(out, "\n")
	if len(lines) > maxWarmStartLines {
		lines = lines[:maxWarmStartLines]
		out = strings.Join(lines, "\n") + "\n… (truncated)"
	}
	if len(out) > maxWarmStartBytes {
		out = out[:maxWarmStartBytes] + "\n… (truncated)"
	}
	return out
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
		sb.WriteString("## " + sec + "\n")
		for _, f := range s.facts[sec] {
			sb.WriteString("- " + f + "\n")
		}
	}
	if !any {
		return "ℹ️ Project memory is empty."
	}
	return strings.TrimSpace(sb.String())
}

// CaptureMinerFindings persists what a MINER turn actually examined plus the
// model's own synthesized summary into project memory. It runs automatically
// at the end of every MINER turn, so a MINER run leaves durable knowledge
// even when the model never called the memory retain tool (its only other
// path). Deterministic — no extra LLM call; only facts the turn touched are
// recorded (examined files + the answer the model produced from them).
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
		if ok, err := s.Retain("Session Notes", "MINER explored: "+strings.Join(list, ", ")); err == nil && ok {
			changed = true
		}
	}
	ans := strings.TrimSpace(answer)
	// Only persist substantial answers (a greeting or empty reply is noise).
	if len(ans) >= 40 {
		if len(ans) > 600 {
			ans = ans[:600] + "…"
		}
		if ok, err := s.Retain("Notes", "MINER findings: "+ans); err == nil && ok {
			changed = true
		}
	}
	if changed {
		return s.Save()
	}
	return nil
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
