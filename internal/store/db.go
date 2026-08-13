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
		status TEXT
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
	_, err := s.db.Exec(schema)
	return err
}

// CreateSession initializes a new session row.
func (s *Store) CreateSession(sessionID, projectPath string) error {
	_, err := s.db.Exec("INSERT INTO sessions (id, project_path, status) VALUES (?, ?, ?)", sessionID, projectPath, "active")
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

// ListSessions retrieves all sessions from the SQLite database.
func (s *Store) ListSessions() ([]Session, error) {
	rows, err := s.db.Query("SELECT id, created_at, project_path, status FROM sessions ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.CreatedAt, &sess.ProjectPath, &sess.Status); err != nil {
			return nil, err
		}
		list = append(list, sess)
	}
	return list, nil
}

// ListSessionsByProjectPath retrieves sessions created in specific directory path.
func (s *Store) ListSessionsByProjectPath(projectPath string) ([]Session, error) {
	if projectPath == "" {
		return s.ListSessions()
	}
	rows, err := s.db.Query("SELECT id, created_at, project_path, status FROM sessions WHERE project_path = ? ORDER BY created_at DESC", projectPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.CreatedAt, &sess.ProjectPath, &sess.Status); err != nil {
			return nil, err
		}
		list = append(list, sess)
	}
	if len(list) == 0 {
		return s.ListSessions()
	}
	return list, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
