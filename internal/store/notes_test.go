package store

import (
	"path/filepath"
	"testing"
)

func TestRecordNoteUpsertAndRecall(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// Record an experience note.
	if err := st.RecordNote(NoteExperience, "file:src/auth.go", "ok", "tool=read_file outcome=ok", []string{"read_file"}); err != nil {
		t.Fatalf("RecordNote failed: %v", err)
	}
	// Record a second, distinct note with a searchable subject.
	if err := st.RecordNote(NoteFact, "file:src/payments.go", "Payment flows via service->repo->Prisma", "tool=recall outcome=ok", []string{"payment", "prisma"}); err != nil {
		t.Fatalf("RecordNote failed: %v", err)
	}

	// Re-recording the same (kind, subject) should bump weight, not duplicate.
	if err := st.RecordNote(NoteExperience, "file:src/auth.go", "ok", "tool=read_file outcome=ok", []string{"read_file"}); err != nil {
		t.Fatalf("RecordNote duplicate failed: %v", err)
	}

	// Recall by keyword should find the payment fact.
	notes, err := st.RecallNotes("payment prisma", nil, 10)
	if err != nil {
		t.Fatalf("RecallNotes failed: %v", err)
	}
	if len(notes) == 0 {
		t.Fatal("expected payment fact to be recalled, got 0")
	}
	found := false
	for _, n := range notes {
		if n.Kind == NoteFact && n.Subject == "file:src/payments.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected payment fact in recall results, got %+v", notes)
	}

	// Kind filter: only facts/decisions (exclude experiences).
	factsOnly, err := st.RecallNotes("payment prisma", []NoteKind{NoteFact}, 10)
	if err != nil {
		t.Fatalf("RecallNotes filtered failed: %v", err)
	}
	for _, n := range factsOnly {
		if n.Kind != NoteFact {
			t.Errorf("kind filter leaked %s", n.Kind)
		}
	}
}

func TestQueryNotesForPromptExcludesExperiences(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	_ = st.RecordNote(NoteExperience, "file:src/x.go", "ok", "tool=read_file outcome=ok", nil)
	_ = st.RecordNote(NoteDecision, "file:src/y.go", "Use pool for connections", "tool=recall outcome=ok", []string{"pool", "connection"})

	notes, err := st.QueryNotesForPrompt("connection pool", 5)
	if err != nil {
		t.Fatalf("QueryNotesForPrompt failed: %v", err)
	}
	for _, n := range notes {
		if n.Kind == NoteExperience {
			t.Errorf("experience note leaked into prompt query: %+v", n)
		}
	}
	if len(notes) == 0 {
		t.Fatal("expected at least the decision note in prompt query")
	}
}

func TestPruneNotesCapsAndDecays(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	for i := 0; i < KnowledgeMaxEntries+20; i++ {
		_ = st.RecordNote(NoteExperience, filepath.Join("file:src", "f"+string(rune('a'+i%26))+".go"), "ok", "tool=read_file", nil)
	}
	// After recording > cap, pruneToCapNotes keeps at most the cap.
	kept, err := st.countNotes()
	if err != nil {
		t.Fatalf("countNotes failed: %v", err)
	}
	if kept > KnowledgeMaxEntries {
		t.Errorf("notes exceeded cap: got %d want <= %d", kept, KnowledgeMaxEntries)
	}
}

func (s *Store) countNotes() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM notes").Scan(&n)
	return n, err
}

func TestTopNotesSeedsArchitecture(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	_ = st.RecordNote(NoteFact, "fact:file:src/core.go", "Central orchestration module.", "reflection", []string{"core"})
	_ = st.RecordNote(NoteDecision, "decision:file:src/cache.go", "Use Redis for caching.", "reflection", []string{"cache"})
	_ = st.RecordNote(NoteExperience, "file:src/noise.go", "ok", "tool=read_file", nil)

	// Even with an empty query, TopNotes should surface the distilled facts so
	// the agent starts architecture-aware without re-reading the codebase.
	top, err := st.TopNotes([]NoteKind{NoteFact, NoteDecision, NoteGotcha, NoteHotfile}, 8)
	if err != nil {
		t.Fatalf("TopNotes failed: %v", err)
	}
	if len(top) < 2 {
		t.Fatalf("expected >=2 distilled notes, got %d", len(top))
	}
	for _, n := range top {
		if n.Kind == NoteExperience {
			t.Errorf("raw experience leaked into TopNotes: %+v", n)
		}
	}
}
