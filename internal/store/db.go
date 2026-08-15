package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store manages SQLite database persistence for sessions and events.
type Store struct {
	db *sql.DB
}

// Session represents a single chat/agent session.
type Session struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	ProjectPath string    `json:"project_path"`
	Status      string    `json:"status"`
	Mode        string    `json:"mode,omitempty"` // last active engine mode ("BUILDER"/"PLANNER"/"MINER")
}

// Event represents an append-only log entry in the events table.
type Event struct {
	ID          int64      `json:"id"`
	SessionID   string     `json:"session_id"`
	Seq         int        `json:"seq"`
	Type        string     `json:"type"` // 'user_msg' | 'reasoning' | 'tool_call' | 'tool_result' | 'compaction_summary' | 'assistant_msg'
	PayloadJSON string     `json:"payload_json"`
	Tokens      int        `json:"tokens"`
	CreatedAt   time.Time  `json:"created_at"`
	HiddenAt    *time.Time `json:"hidden_at,omitempty"`
}

// NewStore opens or creates the SQLite database at default path.
func NewStore(dbPath string) (*Store, error) {
	if dbPath == "" {
		home, _ := os.UserHomeDir()
		dbPath = filepath.Join(home, ".config", "brocode", "brocode.db")
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	s := &Store{db: db}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

func (s *Store) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		project_path TEXT,
		status TEXT,
		mode TEXT DEFAULT 'BUILDER'
	);

	CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		payload_json TEXT NOT NULL,
		tokens INTEGER DEFAULT 0,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		hidden_at TIMESTAMP,
		FOREIGN KEY(session_id) REFERENCES sessions(id)
	);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Migration for databases created before the mode column existed: add it
	// with the same default as fresh schemas. SQLite lacks "ADD COLUMN IF NOT
	// EXISTS", so probe the column list first.
	rows, err := s.db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		return err
	}
	defer rows.Close()
	hasMode := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "mode" {
			hasMode = true
			break
		}
	}
	if !hasMode {
		if _, err := s.db.Exec("ALTER TABLE sessions ADD COLUMN mode TEXT DEFAULT 'BUILDER'"); err != nil {
			return err
		}
	}

	return nil
}

// CreateSession initializes a new session row.
func (s *Store) CreateSession(sessionID, projectPath string) error {
	_, err := s.db.Exec("INSERT INTO sessions (id, project_path, status, mode) VALUES (?, ?, ?, ?)", sessionID, projectPath, "active", "BUILDER")
	return err
}

// GetSessionMode returns the last persisted engine mode for a session, or ""
// when the session row is missing (callers treat "" as BUILDER).
func (s *Store) GetSessionMode(sessionID string) (string, error) {
	var mode string
	err := s.db.QueryRow("SELECT COALESCE(mode, '') FROM sessions WHERE id = ?", sessionID).Scan(&mode)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return mode, nil
}

// UpdateSessionMode persists the active engine mode for a session so a later
// resume (`-c` or /sessions) continues in the same mode.
func (s *Store) UpdateSessionMode(sessionID, mode string) error {
	_, err := s.db.Exec("UPDATE sessions SET mode = ? WHERE id = ?", mode, sessionID)
	return err
}

// AppendEvent appends a new event into the immutable event log.
func (s *Store) AppendEvent(sessionID string, eventType, payloadJSON string, tokens int) (*Event, error) {
	var seq int
	err := s.db.QueryRow("SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE session_id = ?", sessionID).Scan(&seq)
	if err != nil {
		seq = 1
	}

	res, err := s.db.Exec(
		"INSERT INTO events (session_id, seq, type, payload_json, tokens) VALUES (?, ?, ?, ?, ?)",
		sessionID, seq, eventType, payloadJSON, tokens,
	)
	if err != nil {
		return nil, err
	}

	id, _ := res.LastInsertId()
	return &Event{
		ID:          id,
		SessionID:   sessionID,
		Seq:         seq,
		Type:        eventType,
		PayloadJSON: payloadJSON,
		Tokens:      tokens,
		CreatedAt:   time.Now(),
	}, nil
}

// GetSessionEvents fetches all visible events for a session.
func (s *Store) GetSessionEvents(sessionID string) ([]Event, error) {
	rows, err := s.db.Query("SELECT id, session_id, seq, type, payload_json, tokens, created_at FROM events WHERE session_id = ? AND hidden_at IS NULL ORDER BY seq ASC", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.Seq, &ev.Type, &ev.PayloadJSON, &ev.Tokens, &ev.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// CleanupReplayDuplicates removes events that were duplicated by old resume
// logic (which re-persisted the whole log on every `-c`). It detects the
// smallest prefix that the full event list repeats exactly, keeps only that
// prefix, and deletes the repeated tail. Returns the number of events removed.
func (s *Store) CleanupReplayDuplicates(sessionID string) (int, error) {
	events, err := s.GetSessionEvents(sessionID)
	if err != nil {
		return 0, err
	}
	n := len(events)
	if n < 4 {
		return 0, nil
	}

	keep := n
	for k := 1; k <= n/2; k++ {
		// prefix of length k must equal the next k events…
		match := true
		for i := 0; i < k; i++ {
			if events[i].Type != events[k+i].Type || events[i].PayloadJSON != events[k+i].PayloadJSON {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		// …and the whole tail must be a pure repetition of that prefix.
		full := true
		for j := k; j < n; j++ {
			if events[j].Type != events[j%k].Type || events[j].PayloadJSON != events[j%k].PayloadJSON {
				full = false
				break
			}
		}
		if full {
			keep = k
			break
		}
	}

	if keep >= n {
		return 0, nil
	}

	// Events carry monotonically increasing seq per session; keep seq <= keep.
	res, err := s.db.Exec("DELETE FROM events WHERE session_id = ? AND seq > ?", sessionID, keep)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	return int(removed), nil
}

// ListSessions retrieves all sessions from the SQLite database.
func (s *Store) ListSessions() ([]Session, error) {
	rows, err := s.db.Query("SELECT id, created_at, project_path, status, COALESCE(mode, 'BUILDER') FROM sessions ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.CreatedAt, &sess.ProjectPath, &sess.Status, &sess.Mode); err != nil {
			return nil, err
		}
		list = append(list, sess)
	}
	return list, nil
}

// DeleteSession permanently removes a session and all of its events from the
// database (the events table has no ON DELETE CASCADE, so events are deleted
// first). Returns the number of events removed.
func (s *Store) DeleteSession(sessionID string) (int, error) {
	res, err := s.db.Exec("DELETE FROM events WHERE session_id = ?", sessionID)
	if err != nil {
		return 0, err
	}
	evRemoved, _ := res.RowsAffected()
	if _, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", sessionID); err != nil {
		return int(evRemoved), err
	}
	return int(evRemoved), nil
}

// DeleteAllSessions permanently removes every session and all events from the
// database. Returns the number of events removed.
func (s *Store) DeleteAllSessions() (int, error) {
	res, err := s.db.Exec("DELETE FROM events")
	if err != nil {
		return 0, err
	}
	evRemoved, _ := res.RowsAffected()
	if _, err := s.db.Exec("DELETE FROM sessions"); err != nil {
		return int(evRemoved), err
	}
	return int(evRemoved), nil
}

// ListSessionsByProjectPath retrieves sessions created in specific directory path.
func (s *Store) ListSessionsByProjectPath(projectPath string) ([]Session, error) {
	if projectPath == "" {
		return s.ListSessions()
	}
	rows, err := s.db.Query("SELECT id, created_at, project_path, status, COALESCE(mode, 'BUILDER') FROM sessions WHERE project_path = ? ORDER BY created_at DESC", projectPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.CreatedAt, &sess.ProjectPath, &sess.Status, &sess.Mode); err != nil {
			return nil, err
		}
		list = append(list, sess)
	}
	return list, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
