package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestKnowledgeUpdateAndQuery(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// Insert knowledge entry for a file.
	err = st.UpdateKnowledge("file:src/auth/login.go", "go", "package auth\nfunc handleLogin() {}", nil, nil)
	if err != nil {
		t.Fatalf("UpdateKnowledge failed: %v", err)
	}

	// Query should return it for relevant prompt.
	entries, err := st.QueryKnowledge("login handler")
	if err != nil {
		t.Fatalf("QueryKnowledge failed: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least 1 knowledge entry, got 0")
	}
	if entries[0].Key != "file:src/auth/login.go" {
		t.Errorf("expected key 'file:src/auth/login.go', got %s", entries[0].Key)
	}
	if entries[0].Entry.Language != "go" {
		t.Errorf("expected lang 'go', got %s", entries[0].Entry.Language)
	}

	// Non-relevant query should return empty.
	irrelevant, err := st.QueryKnowledge("quantum physics simulation")
	if err != nil {
		t.Fatalf("QueryKnowledge failed: %v", err)
	}
	if len(irrelevant) != 0 {
		t.Errorf("expected 0 entries for irrelevant query, got %d", len(irrelevant))
	}
}

func TestKnowledgeInvalidation(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// Insert then invalidate. UpdateKnowledge is now synchronous (prune is sync).
	_ = st.UpdateKnowledge("file:src/auth/login.go", "go", "package auth", nil, nil)
	entries, _ := st.QueryKnowledge("login")
	if len(entries) == 0 {
		t.Fatal("expected 1 entry before invalidation")
	}

	err = st.InvalidateKnowledge("file:src/auth/login.go")
	if err != nil {
		t.Fatalf("InvalidateKnowledge failed: %v", err)
	}

	entries, _ = st.QueryKnowledge("login")
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after invalidation, got %d", len(entries))
	}
}

func TestKnowledgeHardCap(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// Insert 60 entries — should cap to 50.
	for i := 0; i < 60; i++ {
		key := fmt.Sprintf("file:src/file_%d.go", i)
		_ = st.UpdateKnowledge(key, "go", "package main", nil, nil)
	}

	// Verify count is ≤50 via direct query.
	var count int
	err = st.db.QueryRow("SELECT COUNT(*) FROM knowledge").Scan(&count)
	if err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count > KnowledgeMaxEntries {
		t.Errorf("expected ≤%d entries, got %d", KnowledgeMaxEntries, count)
	}
}

func TestKnowledgePruneStale(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// Insert with low weight and manually age it.
	_ = st.UpdateKnowledge("file:stale.go", "go", "old content", nil, nil)
	// Manually set weight=0.5 and old last_seen.
	_, _ = st.db.Exec("UPDATE knowledge SET weight = 0.5, last_seen = ?", "2000-01-01 00:00:00")

	n, err := st.PruneKnowledge()
	if err != nil {
		t.Fatalf("PruneKnowledge failed: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 pruned entry, got %d", n)
	}

	entries, _ := st.QueryKnowledge("stale")
	if len(entries) != 0 {
		t.Errorf("expected 0 stale entries after prune, got %d", len(entries))
	}
}

func TestKnowledgeReinforceWeight(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// Same content re-read twice — hash matches → weight boosted.
	_ = st.UpdateKnowledge("file:src/main.go", "go", "package main\nfunc main(){}", nil, nil)
	_ = st.UpdateKnowledge("file:src/main.go", "go", "package main\nfunc main(){}", nil, nil)

	var weight float64
	_ = st.db.QueryRow("SELECT weight FROM knowledge WHERE key = ?", "file:src/main.go").Scan(&weight)
	if weight <= 1.5 {
		t.Errorf("expected weight > 1.5 after re-read, got %.2f", weight)
	}
}

func TestKnowledgeNewContentInvalidates(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// Insert with content hash A.
	_ = st.UpdateKnowledge("file:src/main.go", "go", "package main\nfunc old() {}", nil, nil)
	var hash1 string
	_ = st.db.QueryRow("SELECT json_extract(val, '$.hash') FROM knowledge WHERE key = ?", "file:src/main.go").Scan(&hash1)

	// Re-read with different content → hash changes → NEW entry (no weight boost).
	_ = st.UpdateKnowledge("file:src/main.go", "go", "package main\nfunc new() {}", nil, nil)
	var hash2 string
	_ = st.db.QueryRow("SELECT json_extract(val, '$.hash') FROM knowledge WHERE key = ?", "file:src/main.go").Scan(&hash2)

	if hash1 == hash2 {
		t.Errorf("expected different hashes for different content, got %s both times", hash1)
	}
}
