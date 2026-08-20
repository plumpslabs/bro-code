package store

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// KnowledgeEntry is a single node in the Smart Context Graph.
// It records what BroCode has previously analyzed about a file so it can
// avoid re-scanning unchanged content and surface learned relationships.
type KnowledgeEntry struct {
	Hash      string           `json:"hash"`      // sha1 of content (first 8 hex chars)
	Language  string           `json:"lang"`      // detected stack ("go", "ts", "python", ...)
	Tags      []string         `json:"tags"`      // extracted keywords (func names, imports)
	Neighbors []KnowledgeLink  `json:"neighbors"` // files frequently co-read (max 3)
	Symbols   []SymbolRange    `json:"symbols,omitempty"` // whole-file structural index (name→line span)
}

// KnowledgeLink is a weighted edge to another file.
type KnowledgeLink struct {
	Path   string  `json:"p"`         // e.g. "src/middleware/auth.go"
	Weight float64 `json:"w"`         // 0.0 - 1.0 co-occurrence score
}

// KnowledgeEntryRow joins the DB row with the parsed payload.
type KnowledgeEntryRow struct {
	Key      string
	Entry    KnowledgeEntry
	Weight   float64
	SeenAt   time.Time
}

const (
	// KnowledgeMaxEntries is the hard cap on the knowledge table size.
	// Older, low-weight entries are pruned when exceeded.
	KnowledgeMaxEntries = 50

	// KnowledgeMaxNeighbors caps co-read links per entry.
	KnowledgeMaxNeighbors = 3

	// KnowledgeMaxTags caps extracted keywords per entry.
	KnowledgeMaxTags = 8

	// knowledgePruneWeight is the threshold below which (combined with age >
	// 7 days) an entry is eligible for pruning.
	knowledgePruneWeight = 1.0

	// knowledgePruneAge is how long an entry must be unused before pruning.
	knowledgePruneAge = 7 * 24 * time.Hour

	// knowledgeKeyPrefix marks a file-scoped knowledge entry.
	knowledgeKeyPrefix = "file:"
)

// UpdateKnowledge stores a knowledge entry for a file. Called asynchronously
// after read_file succeeds. If the entry already exists with the same hash,
// only `weight` is incremented (reinforcement learning signal). `symbols` is
// the whole-file structural index (optional; extracted from content when nil).
func (s *Store) UpdateKnowledge(key, language string, content string, neighbors []KnowledgeLink, symbols []SymbolRange) error {
	if s == nil || key == "" || content == "" {
		return nil
	}

	hash := sha1Hash(content)
	tags := extractTags(content, language)
	if len(neighbors) > KnowledgeMaxNeighbors {
		neighbors = neighbors[:KnowledgeMaxNeighbors]
	}
	if symbols == nil {
		symbols = extractSymbols(content, language)
	}
	if len(symbols) > knowledgeMaxSymbols {
		symbols = symbols[:knowledgeMaxSymbols]
	}

	entry := KnowledgeEntry{
		Hash:      hash,
		Language:  language,
		Tags:      tags,
		Neighbors: neighbors,
		Symbols:   symbols,
	}

	val, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	// UPSERT: if existing entry has same hash, just bump weight.
	now := time.Now().UTC().Format(time.RFC3339)
	const upsert = `
		INSERT INTO knowledge (key, val, weight, created_at, last_seen)
		VALUES (?, ?, 1.5, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			val = CASE WHEN json_extract(excluded.val, '$.hash') = json_extract(knowledge.val, '$.hash')
				THEN knowledge.val
				ELSE excluded.val END,
			weight = CASE WHEN json_extract(excluded.val, '$.hash') = json_extract(knowledge.val, '$.hash')
				THEN knowledge.weight + 0.3
				ELSE 1.5 END,
			last_seen = ?
	`
	// Retry on SQLITE_BUSY: the connection is shared and serialized, but a
	// worst-case contention window still needs a bounded retry rather than a
	// hard error surfaced to the model.
	for attempt := 0; attempt < 3; attempt++ {
		_, err = s.db.Exec(upsert, key, string(val), now, now, now)
		if err == nil {
			break
		}
		if !isSQLiteBusy(err) {
			return err
		}
		sleepBackoff(attempt)
	}
	if err != nil {
		return err
	}

	// Enforce size cap (cheap: 50-row window, O(1) relative to any real DB).
	s.pruneToCap()

	return nil
}

// QueryKnowledge returns up to `limit` knowledge entries whose key or tags
// relate to the prompt. Uses a simple keyword overlap heuristic (no BM25
// dependency). Results are ordered by weight descending.
func (s *Store) QueryKnowledge(prompt string) ([]KnowledgeEntryRow, error) {
	if s == nil || strings.TrimSpace(prompt) == "" {
		return nil, nil
	}

	keywords := strings.Fields(strings.ToLower(prompt))
	if len(keywords) == 0 {
		return nil, nil
	}

	// Fetch top-weighted entries (the "hot" knowledge). Then filter by
	// keyword overlap in-memory — 50 entries is small enough for linear scan.
	rows, err := s.db.Query(`
		SELECT key, val, weight, last_seen FROM knowledge
		ORDER BY weight DESC, last_seen DESC
		LIMIT ?
	`, KnowledgeMaxEntries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []KnowledgeEntryRow
	for rows.Next() {
		var key, val string
		var weight float64
		var seenAt string
		if err := rows.Scan(&key, &val, &weight, &seenAt); err != nil {
			continue
		}
		var entry KnowledgeEntry
		if err := json.Unmarshal([]byte(val), &entry); err != nil {
			continue
		}
		// Simple tag-based relevance scoring.
		relevance := 0.0
		for _, tag := range entry.Tags {
			for _, kw := range keywords {
				if strings.Contains(strings.ToLower(tag), kw) {
					relevance += 1.0
					break
				}
			}
		}
		// Boost on symbol-name matches: this is what makes a query like "login
		// handler" resolve to handleLogin(L42-88) inside a 5000-line file,
		// even though the model never read the whole file.
		for _, sym := range entry.Symbols {
			for _, kw := range keywords {
				if strings.Contains(strings.ToLower(sym.Name), kw) {
					relevance += 1.5
					break
				}
			}
		}
		// Boost if the key path itself contains the keyword.
		for _, kw := range keywords {
			if strings.Contains(strings.ToLower(key), kw) {
				relevance += 2.0
			}
		}
		if relevance > 0 {
			out = append(out, KnowledgeEntryRow{
				Key:    key,
				Entry:  entry,
				Weight: weight + relevance, // boost
				SeenAt: parseTime(seenAt),
			})
		}
		// Always include top 3 highest-weight entries regardless of tag match.
		if len(out) >= 3 {
			break
		}
	}

	// Sort by combined weight+relevance (already roughly ordered, but ensure).
	if len(out) > 1 {
		sortKnowledge(out)
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out, nil
}

// InvalidateKnowledge removes a knowledge entry. Called synchronously on
// edit_file / write_file / delete_file to prevent serving stale hashes.
// Retries up to 3 times with exponential backoff to handle SQLite write
// contention from concurrent async knowledge updates.
func (s *Store) InvalidateKnowledge(key string) error {
	if s == nil || key == "" {
		return nil
	}
	// Retry loop for SQLite busy errors during async update contention.
	for attempt := 0; attempt < 3; attempt++ {
		_, err := s.db.Exec("DELETE FROM knowledge WHERE key = ?", key)
		if err == nil {
			return nil
		}
		if !isSQLiteBusy(err) {
			return err
		}
		sleepBackoff(attempt)
	}
	// Final attempt without retry.
	_, err := s.db.Exec("DELETE FROM knowledge WHERE key = ?", key)
	return err
}

// isSQLiteBusy checks if the error is a SQLITE_BUSY contention error.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "database is locked") ||
		strings.Contains(err.Error(), "SQLITE_BUSY")
}

// sleepBackoff sleeps with exponential backoff: attempt 0 → 10ms, 1 → 50ms, 2 → 250ms.
func sleepBackoff(attempt int) {
	times := []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 250 * time.Millisecond}
	if attempt < len(times) {
		time.Sleep(times[attempt])
	}
}

// PruneKnowledge removes entries with weight < knowledgePruneWeight and age
// > knowledgePrunAge. Returns the number of entries pruned.
func (s *Store) PruneKnowledge() (int, error) {
	if s == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-knowledgePruneAge).UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`
		DELETE FROM knowledge
		WHERE weight < ? AND last_seen < ?
	`, knowledgePruneWeight, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// pruneToCap enforces the hard entry cap. Called after every insert.
func (s *Store) pruneToCap() {
	if s == nil {
		return
	}
	_, _ = s.db.Exec(`
		DELETE FROM knowledge
		WHERE key NOT IN (
			SELECT key FROM knowledge ORDER BY weight DESC, last_seen DESC LIMIT ?
		)
	`, KnowledgeMaxEntries)
}

// sha1Hash returns the first 8 hex chars of SHA-1(content).
func sha1Hash(content string) string {
	h := sha1.Sum([]byte(content))
	return hex.EncodeToString(h[:4]) // 8 hex chars = fast compare
}

// extractTags pulls meaningful keywords from source content: function names,
// import paths, string literals, variable declarations. Conservative and
// language-aware.
func extractTags(content, language string) []string {
	content = strings.ToLower(content)
	tags := make(map[string]struct{})

	// Pattern: func name( ... ) in Go/TS/JS
	reFunc := regexp.MustCompile(`func(?:tion)?\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
	for _, m := range reFunc.FindAllStringSubmatch(content, 10) {
		if len(m) > 1 && len(m[1]) > 3 {
			tags[m[1]] = struct{}{}
		}
	}

	// Pattern: import "path" or require("path")
	reImport := regexp.MustCompile(`(?:import|require)\s*\(?\s*["']([^"']+)["']`)
	for _, m := range reImport.FindAllStringSubmatch(content, 10) {
		if len(m) > 1 {
			// Extract the leaf component as a tag.
			parts := strings.Split(m[1], "/")
			leaf := parts[len(parts)-1]
			if len(leaf) > 2 {
				tags[leaf] = struct{}{}
			}
		}
	}

	// Pattern: const/var name =
	reVar := regexp.MustCompile(`(?:const|var|let)\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
	for _, m := range reVar.FindAllStringSubmatch(content, 10) {
		if len(m) > 1 && len(m[1]) > 4 {
			tags[m[1]] = struct{}{}
		}
	}

	// Pattern: string literals (quoted, 10-40 chars)
	reString := regexp.MustCompile(`"([^"]{10,40})"`)
	for _, m := range reString.FindAllStringSubmatch(content, 5) {
		if len(m) > 1 && !strings.HasPrefix(m[1], "/") {
			tags[m[1]] = struct{}{}
		}
	}

	out := make([]string, 0, len(tags))
	for t := range tags {
		out = append(out, t)
		if len(out) >= KnowledgeMaxTags {
			break
		}
	}
	return out
}

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func sortKnowledge(entries []KnowledgeEntryRow) {
	// Simple insertion sort by Weight (entries slice is ≤3).
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Weight > entries[j-1].Weight; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// FormatKnowledgeHints renders knowledge entries into a compact system-prompt
// block for injection by the engine. For each relevant file it also lists the
// matched symbols with their line spans, so the model knows exactly where to
// jump (read_file(start_line/end_line)) instead of re-reading the whole file.
// `query` (the current prompt) is used to prioritize the symbols whose names
// match — so a query like "omega handler" surfaces omega(L4953-4953) first,
// even inside a 5000-line file whose first symbols are unrelated.
func FormatKnowledgeHints(entries []KnowledgeEntryRow, query string) string {
	if len(entries) == 0 {
		return ""
	}
	kws := strings.Fields(strings.ToLower(query))
	rankSym := func(s SymbolRange) bool {
		if len(kws) == 0 {
			return false
		}
		for _, kw := range kws {
			if strings.Contains(strings.ToLower(s.Name), kw) {
				return true
			}
		}
		return false
	}
	var sb strings.Builder
	sb.WriteString("🧠 **SMART CONTEXT** — Previously analyzed files relevant to your request:\n")
	for _, e := range entries {
		path := strings.TrimPrefix(e.Key, knowledgeKeyPrefix)
		sb.WriteString(fmt.Sprintf("- `%s` (%s, %d symbols, last seen: %s, weight %.1f)\n",
			path, e.Entry.Language, len(e.Entry.Symbols), e.SeenAt.Format("2006-01-02"), e.Weight))
		// Surface up to 6 symbol anchors, matched symbols first (then a
		// structural sample), so the relevant region is always visible.
		matched := e.Entry.Symbols[:0:0]
		rest := e.Entry.Symbols[:0:0]
		for _, sym := range e.Entry.Symbols {
			if rankSym(sym) {
				matched = append(matched, sym)
			} else {
				rest = append(rest, sym)
			}
		}
		shown := 0
		for _, sym := range append(matched, rest...) {
			sb.WriteString(fmt.Sprintf("    • %s `%s` (L%d-%d)\n", sym.Kind, sym.Name, sym.Start, sym.End))
			shown++
			if shown >= 6 {
				if len(e.Entry.Symbols) > 6 {
					sb.WriteString(fmt.Sprintf("    • … +%d more symbols (use code_locate or read_file range)\n", len(e.Entry.Symbols)-6))
				}
				break
			}
		}
	}
	sb.WriteString("→ Jump straight to the relevant symbol with read_file(start_line/end_line); skip re-reading unchanged files.\n")
	return sb.String()
}
