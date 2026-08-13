package store

import (
	"path/filepath"
	"testing"
)

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
