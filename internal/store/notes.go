package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// NoteKind classifies a durable, searchable note captured from agent activity.
// The taxonomy follows the retain→recall→reflect discipline: raw experiences
// are distilled (by the reflection pass) into facts/beliefs/decisions/gotchas
// that are cheaper to retrieve and higher-signal for future sessions.
type NoteKind string

const (
	// NoteExperience is a raw, provenance-tagged record of an agent action
	// (what tool, what target, what outcome). High-volume, low-abstraction.
	NoteExperience NoteKind = "experience"
	// NoteHotfile marks a file the agent touches repeatedly this session.
	NoteHotfile NoteKind = "hotfile"
	// NoteFact is a distilled, durable insight (from reflection).
	NoteFact NoteKind = "fact"
	// NoteBelief is an evolving conclusion carrying a confidence score.
	NoteBelief NoteKind = "belief"
	// NoteDecision records an architectural/product decision.
	NoteDecision NoteKind = "decision"
	// NoteGotcha records a trap/pitfall learned the hard way.
	NoteGotcha NoteKind = "gotcha"
)

// noteMaxEntries caps the notes table so it cannot grow without bound. Older,
// low-weight notes are pruned when exceeded.
const noteMaxEntries = 200

// notePruneWeight / notePruneAge mirror the knowledge store's decay policy.
const (
	notePruneWeight = 1.0
	notePruneAge    = 7 * 24 * time.Hour
)

// Note is a single self-documenting record in the unified context store.
type Note struct {
	ID         int64     `json:"id"`
	Kind       NoteKind  `json:"kind"`
	Subject    string    `json:"subject"`    // file path / query / topic
	Content    string    `json:"content"`    // outcome or distilled insight
	Tags       []string  `json:"tags"`       // keywords for retrieval
	Provenance string    `json:"provenance"` // "tool=... target=... outcome=..."
	Weight     float64   `json:"weight"`
	Confidence float64   `json:"confidence"` // 0..1, meaningful for beliefs/facts
	CreatedAt  time.Time `json:"created_at"`
	LastSeen   time.Time `json:"last_seen"`
}

// RecordNote stores (or reinforces) a note. Repeated identical (kind, subject)
// pairs bump weight and refresh last_seen instead of bloating the table.
func (s *Store) RecordNote(kind NoteKind, subject, content, provenance string, tags []string) error {
	if s == nil || subject == "" || content == "" {
		return nil
	}
	if len(tags) > KnowledgeMaxTags {
		tags = tags[:KnowledgeMaxTags]
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	// Retry loop for SQLite busy errors during concurrent async writes.
	for attempt := 0; attempt < 3; attempt++ {
		_, err = s.db.Exec(`
			INSERT INTO notes (kind, subject, content, tags, provenance, weight, confidence, created_at, last_seen)
			VALUES (?, ?, ?, ?, ?, 1.0, 1.0, ?, ?)
			ON CONFLICT(kind, subject) DO UPDATE SET
				content = excluded.content,
				tags = excluded.tags,
				provenance = excluded.provenance,
				weight = MIN(weight + 0.2, 5.0),
				last_seen = excluded.last_seen
		`, string(kind), subject, content, string(tagsJSON), provenance, now, now)
		if err == nil {
			s.pruneToCapNotes()
			return nil
		}
		if !isSQLiteBusy(err) {
			return err
		}
		sleepBackoff(attempt)
	}
	_, err = s.db.Exec(`
		INSERT INTO notes (kind, subject, content, tags, provenance, weight, confidence, created_at, last_seen)
		VALUES (?, ?, ?, ?, ?, 1.0, 1.0, ?, ?)
		ON CONFLICT(kind, subject) DO UPDATE SET
			content = excluded.content,
			tags = excluded.tags,
			provenance = excluded.provenance,
			weight = MIN(weight + 0.2, 5.0),
			last_seen = excluded.last_seen
	`, string(kind), subject, content, string(tagsJSON), provenance, now, now)
	return err
}

// RecallNotes searches the notes store by keyword overlap, optionally filtered
// by kind. Returns up to `limit` notes ordered by combined weight+relevance.
// This is the agent-facing "context_recall" query: active self-retrieval.
func (s *Store) RecallNotes(query string, kinds []NoteKind, limit int) ([]Note, error) {
	if s == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	keywords := strings.Fields(strings.ToLower(query))
	if len(keywords) == 0 {
		return nil, nil
	}

	sqlStr := `SELECT id, kind, subject, content, tags, provenance, weight, confidence, created_at, last_seen FROM notes`
	args := []any{}
	if len(kinds) > 0 {
		ph := strings.Repeat("?,", len(kinds))
		ph = ph[:len(ph)-1]
		sqlStr += ` WHERE kind IN (` + ph + `)`
		for _, k := range kinds {
			args = append(args, string(k))
		}
	}
	sqlStr += ` ORDER BY weight DESC, last_seen DESC LIMIT ?`
	args = append(args, noteMaxEntries)

	rows, err := s.db.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		n, ok := scanNote(rows)
		if !ok {
			continue
		}
		relevance := scoreNote(n, keywords)
		if relevance <= 0 {
			if len(out) >= 3 && len(out) >= limit {
				break
			}
			continue
		}
		n.Weight += relevance
		out = append(out, n)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// QueryNotesForPrompt returns high-signal distilled notes (facts/beliefs/
// decisions/gotchas) relevant to a prompt, for warm-start injection. Raw
// experiences are excluded — they are consolidation fuel, not prompt content.
func (s *Store) QueryNotesForPrompt(query string, limit int) ([]Note, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	return s.RecallNotes(query,
		[]NoteKind{NoteFact, NoteBelief, NoteDecision, NoteGotcha, NoteHotfile}, limit)
}

// PruneNotes removes low-weight, stale notes (mirrors PruneKnowledge).
func (s *Store) PruneNotes() (int, error) {
	if s == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-notePruneAge).UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`DELETE FROM notes WHERE weight < ? AND last_seen < ?`, notePruneWeight, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// TopNotes returns the highest-weight notes across the given kinds, regardless
// of a query — used to seed architecture/context awareness every session even
// when the user's prompt is vague. Bounded by `limit` and recency-weighted.
func (s *Store) TopNotes(kinds []NoteKind, limit int) ([]Note, error) {
	if s == nil || len(kinds) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 8
	}
	ph := strings.Repeat("?,", len(kinds))
	ph = ph[:len(ph)-1]
	args := make([]any, 0, len(kinds)+1)
	for _, k := range kinds {
		args = append(args, string(k))
	}
	args = append(args, limit)
	rows, err := s.db.Query(`
		SELECT id, kind, subject, content, tags, provenance, weight, confidence, created_at, last_seen
		FROM notes WHERE kind IN (`+ph+`) ORDER BY weight DESC, last_seen DESC LIMIT ?
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		if n, ok := scanNote(rows); ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// NotesByKind returns up to `limit` notes of a single kind, highest-weight
// first. Used by the reflection pass to distill raw experiences into durable,
// retrieval-cheap notes.
func (s *Store) NotesByKind(kind NoteKind, limit int) ([]Note, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, kind, subject, content, tags, provenance, weight, confidence, created_at, last_seen
		FROM notes WHERE kind = ? ORDER BY weight DESC, last_seen DESC LIMIT ?
	`, string(kind), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		if n, ok := scanNote(rows); ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func (s *Store) pruneToCapNotes() {
	if s == nil {
		return
	}
	_, _ = s.db.Exec(`
		DELETE FROM notes
		WHERE id NOT IN (
			SELECT id FROM notes ORDER BY weight DESC, last_seen DESC LIMIT ?
		)
	`, noteMaxEntries)
}

// scanNote reads a notes row into a Note. Returns ok=false on scan failure.
func scanNote(rows *sql.Rows) (Note, bool) {
	var n Note
	var kind, subject, content, tagsJSON, provenance, createdAt, lastSeen string
	var weight, confidence float64
	var id int64
	if err := rows.Scan(&id, &kind, &subject, &content, &tagsJSON, &provenance, &weight, &confidence, &createdAt, &lastSeen); err != nil {
		return Note{}, false
	}
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	n = Note{
		ID:         id,
		Kind:       NoteKind(kind),
		Subject:    subject,
		Content:    content,
		Tags:       tags,
		Provenance: provenance,
		Weight:     weight,
		Confidence: confidence,
		CreatedAt:  parseTime(createdAt),
		LastSeen:   parseTime(lastSeen),
	}
	return n, true
}

// scoreNote returns a simple keyword-overlap relevance score (BM25-lite, no
// external dependency) for a note against query keywords.
func scoreNote(n Note, keywords []string) float64 {
	score := 0.0
	hay := strings.ToLower(n.Subject + " " + n.Content + " " + strings.Join(n.Tags, " "))
	for _, kw := range keywords {
		if strings.Contains(hay, kw) {
			score += 1.0
		}
	}
	return score
}
