package tool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTimeTravelRollbackUndo(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")

	if err := os.WriteFile(fileA, []byte("original A"), 0o644); err != nil {
		t.Fatalf("failed to write fileA: %v", err)
	}
	if err := os.WriteFile(fileB, []byte("original B"), 0o644); err != nil {
		t.Fatalf("failed to write fileB: %v", err)
	}

	// Snapshot before edits
	if err := Snapshot(fileA); err != nil {
		t.Fatalf("snapshot fileA failed: %v", err)
	}
	if err := Snapshot(fileB); err != nil {
		t.Fatalf("snapshot fileB failed: %v", err)
	}

	// Modify files
	_ = os.WriteFile(fileA, []byte("modified A"), 0o644)
	_ = os.WriteFile(fileB, []byte("modified B"), 0o644)

	if count := SnapshotCount(); count != 2 {
		t.Fatalf("expected 2 live snapshots, got %d", count)
	}

	// Perform Time-Travel Rollback
	restored := RestoreAllSnapshots()
	if restored != 2 {
		t.Fatalf("expected 2 restored files, got %d", restored)
	}

	contentA, _ := os.ReadFile(fileA)
	contentB, _ := os.ReadFile(fileB)

	if string(contentA) != "original A" {
		t.Errorf("fileA expected 'original A', got '%s'", string(contentA))
	}
	if string(contentB) != "original B" {
		t.Errorf("fileB expected 'original B', got '%s'", string(contentB))
	}
}
