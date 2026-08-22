package store

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// Playbook represents an automated self-healing error fix entry.
type Playbook struct {
	ID          string    `json:"id"`
	Pattern     string    `json:"pattern"`
	RootCause   string    `json:"root_cause"`
	Solution    string    `json:"solution"`
	Category    string    `json:"category"`
	Occurrences int       `json:"occurrences"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsed    time.Time `json:"last_used"`
}

// RecordPlaybook stores or increments a solution playbook for a verified error pattern.
func (s *Store) RecordPlaybook(pattern, rootCause, solution, category string) error {
	if s == nil || s.db == nil {
		return nil
	}
	pattern = strings.TrimSpace(pattern)
	solution = strings.TrimSpace(solution)
	if pattern == "" || solution == "" {
		return nil
	}
	if category == "" {
		category = "error_fix"
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(pattern[:min(128, len(pattern))])))
	id := fmt.Sprintf("pb_%s", hash[:16])

	query := `
	INSERT INTO playbooks (id, pattern, root_cause, solution, category, occurrences, created_at, last_used)
	VALUES (?, ?, ?, ?, ?, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	ON CONFLICT(id) DO UPDATE SET
		root_cause = excluded.root_cause,
		solution = excluded.solution,
		occurrences = occurrences + 1,
		last_used = CURRENT_TIMESTAMP;
	`
	_, err := s.db.Exec(query, id, pattern, rootCause, solution, category)
	return err
}

// MatchPlaybook searches for a playbook whose pattern matches the given error text.
func (s *Store) MatchPlaybook(errText string) (*Playbook, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	errText = strings.TrimSpace(errText)
	if errText == "" {
		return nil, nil
	}

	rows, err := s.db.Query(`SELECT id, pattern, root_cause, solution, category, occurrences, created_at, last_used FROM playbooks ORDER BY occurrences DESC, last_used DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	lowerErr := strings.ToLower(errText)
	var best *Playbook
	for rows.Next() {
		var pb Playbook
		if err := rows.Scan(&pb.ID, &pb.Pattern, &pb.RootCause, &pb.Solution, &pb.Category, &pb.Occurrences, &pb.CreatedAt, &pb.LastUsed); err != nil {
			continue
		}
		lowerPattern := strings.ToLower(pb.Pattern)
		// Check for substring or keyword presence
		if strings.Contains(lowerErr, lowerPattern) || strings.Contains(lowerPattern, lowerErr) {
			best = &pb
			break
		}
		// Also check significant token overlap
		patternTokens := strings.Fields(lowerPattern)
		matchCount := 0
		for _, tok := range patternTokens {
			if len(tok) >= 4 && strings.Contains(lowerErr, tok) {
				matchCount++
			}
		}
		if len(patternTokens) >= 3 && matchCount >= len(patternTokens)*2/3 {
			best = &pb
			break
		}
	}
	return best, rows.Err()
}

// ListPlaybooks retrieves the top playbooks stored in the database.
func (s *Store) ListPlaybooks(limit int) ([]Playbook, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, pattern, root_cause, solution, category, occurrences, created_at, last_used FROM playbooks ORDER BY occurrences DESC, last_used DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Playbook
	for rows.Next() {
		var pb Playbook
		if err := rows.Scan(&pb.ID, &pb.Pattern, &pb.RootCause, &pb.Solution, &pb.Category, &pb.Occurrences, &pb.CreatedAt, &pb.LastUsed); err != nil {
			continue
		}
		out = append(out, pb)
	}
	return out, rows.Err()
}
