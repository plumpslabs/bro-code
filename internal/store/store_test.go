package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestDeleteSession(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	if err := st.CreateSession("del-me", "/tmp/proj"); err != nil {
		t.Fatalf("failed to create del-me: %v", err)
	}
	if err := st.CreateSession("keep-me", "/tmp/proj"); err != nil {
		t.Fatalf("failed to create keep-me: %v", err)
	}
	_, _ = st.AppendEvent("del-me", "user_msg", `{"content":"hi"}`, 5)
	_, _ = st.AppendEvent("keep-me", "user_msg", `{"content":"yo"}`, 5)

	removed, err := st.DeleteSession("del-me")
	if err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	if removed != 1 {
		t.Errorf("expected 1 event removed, got %d", removed)
	}

	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != "keep-me" {
		t.Errorf("expected only keep-me to remain, got %+v", list)
	}

	// Events must be gone too (no ON DELETE CASCADE in the schema).
	ev, err := st.GetSessionEvents("del-me")
	if err != nil {
		t.Fatalf("GetSessionEvents failed: %v", err)
	}
	if len(ev) != 0 {
		t.Errorf("expected 0 events for deleted session, got %d", len(ev))
	}

	// Deleting a missing session is a no-op, not an error.
	if _, err := st.DeleteSession("never-existed"); err != nil {
		t.Errorf("deleting missing session should not error, got %v", err)
	}
}

func TestDeleteAllSessions(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	for _, id := range []string{"sess_a", "sess_b", "sess_c"} {
		if err := st.CreateSession(id, "/tmp/proj"); err != nil {
			t.Fatalf("failed to create %s: %v", id, err)
		}
		_, _ = st.AppendEvent(id, "user_msg", `{"content":"x"}`, 5)
		_, _ = st.AppendEvent(id, "assistant_msg", `{"content":"y"}`, 7)
	}

	removed, err := st.DeleteAllSessions()
	if err != nil {
		t.Fatalf("DeleteAllSessions failed: %v", err)
	}
	if removed != 6 {
		t.Errorf("expected 6 events removed, got %d", removed)
	}

	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected 0 sessions after delete-all, got %+v", list)
	}
}

func TestCleanupReplayDuplicates(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	sessionID := "dup-session"
	if err := st.CreateSession(sessionID, "/tmp/project"); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Simulate the old resume bug: the full log [user, assistant] was
	// re-persisted on every `-c`, so the DB ends up with user/assistant
	// repeated 3×.
	for i := 0; i < 3; i++ {
		_, _ = st.AppendEvent(sessionID, "user_msg", `{"content":"halo"}`, 5)
		_, _ = st.AppendEvent(sessionID, "assistant_msg", `{"content":"Halo!"}`, 7)
	}

	removed, err := st.CleanupReplayDuplicates(sessionID)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 4 {
		t.Errorf("expected 4 duplicated events removed, got %d", removed)
	}

	events, err := st.GetSessionEvents(sessionID)
	if err != nil {
		t.Fatalf("failed to fetch events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events after cleanup, got %d", len(events))
	}
	if events[0].Type != "user_msg" || events[1].Type != "assistant_msg" {
		t.Errorf("unexpected events after cleanup: %+v", events)
	}

	// Cleanup is idempotent on clean data.
	removed, err = st.CleanupReplayDuplicates(sessionID)
	if err != nil {
		t.Fatalf("second cleanup failed: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed on clean data, got %d", removed)
	}
}

func TestCleanupReplayDuplicatesKeepsRealHistory(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	sessionID := "real-session"
	if err := st.CreateSession(sessionID, "/tmp/project"); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// A normal conversation has distinct turns — must never be touched.
	_, _ = st.AppendEvent(sessionID, "user_msg", `{"content":"halo"}`, 5)
	_, _ = st.AppendEvent(sessionID, "assistant_msg", `{"content":"Halo!"}`, 7)
	_, _ = st.AppendEvent(sessionID, "user_msg", `{"content":"bikin fitur x"}`, 9)
	_, _ = st.AppendEvent(sessionID, "assistant_msg", `{"content":"Siap!"}`, 11)

	removed, err := st.CleanupReplayDuplicates(sessionID)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed for real history, got %d", removed)
	}

	events, _ := st.GetSessionEvents(sessionID)
	if len(events) != 4 {
		t.Errorf("expected all 4 real events preserved, got %d", len(events))
	}
}

func TestStoreSessionAndEvents(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_brocode.db")

	st, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	sessionID := "test-session-123"
	if err := st.CreateSession(sessionID, "/tmp/project"); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	ev, err := st.AppendEvent(sessionID, "user_msg", `{"content":"hello"}`, 5)
	if err != nil {
		t.Fatalf("failed to append event: %v", err)
	}

	if ev.Seq != 1 {
		t.Errorf("expected sequence 1, got %d", ev.Seq)
	}

	events, err := st.GetSessionEvents(sessionID)
	if err != nil {
		t.Fatalf("failed to fetch events: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

func TestSessionModePersist(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	if err := st.CreateSession("mode-sess", "/tmp/proj"); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// New sessions default to BUILDER.
	mode, err := st.GetSessionMode("mode-sess")
	if err != nil {
		t.Fatalf("GetSessionMode failed: %v", err)
	}
	if mode != "BUILDER" {
		t.Errorf("expected default mode BUILDER, got %q", mode)
	}

	// A missing session reports "" (callers treat it as BUILDER).
	if mode, err := st.GetSessionMode("never-existed"); err != nil || mode != "" {
		t.Errorf("expected '' for missing session, got %q err=%v", mode, err)
	}

	// Persisting a mode change survives reads and list queries.
	if err := st.UpdateSessionMode("mode-sess", "PLANNER"); err != nil {
		t.Fatalf("UpdateSessionMode failed: %v", err)
	}
	mode, err = st.GetSessionMode("mode-sess")
	if err != nil || mode != "PLANNER" {
		t.Errorf("expected PLANNER after update, got %q err=%v", mode, err)
	}

	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(list) != 1 || list[0].Mode != "PLANNER" {
		t.Errorf("expected ListSessions to carry mode PLANNER, got %+v", list)
	}
}

// TestModeColumnMigration simulates a database created before the mode column
// existed: NewStore's initSchema must migrate it (ALTER TABLE) so reads work.
func TestModeColumnMigration(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_brocode.db")

	// Build a DB with the OLD schema (no mode column) + a session row.
	old, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	oldSchema := `
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		project_path TEXT,
		status TEXT
	);`
	if _, err := old.Exec(oldSchema); err != nil {
		t.Fatalf("failed to create old schema: %v", err)
	}
	if _, err := old.Exec(`INSERT INTO sessions (id, project_path, status) VALUES ('legacy', '/p', 'active')`); err != nil {
		t.Fatalf("failed to insert legacy row: %v", err)
	}
	old.Close()

	// Reopen with NewStore — initSchema must add the mode column.
	st, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer st.Close()

	mode, err := st.GetSessionMode("legacy")
	if err != nil {
		t.Fatalf("GetSessionMode on migrated db failed: %v", err)
	}
	if mode != "BUILDER" {
		t.Errorf("expected migrated default BUILDER, got %q", mode)
	}
	if err := st.UpdateSessionMode("legacy", "MINER"); err != nil {
		t.Fatalf("UpdateSessionMode on migrated db failed: %v", err)
	}
	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions on migrated db failed: %v", err)
	}
	if len(list) != 1 || list[0].Mode != "MINER" {
		t.Errorf("expected MINER after migration+update, got %+v", list)
	}
}
