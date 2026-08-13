package store

import (
	"path/filepath"
	"testing"
)

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
