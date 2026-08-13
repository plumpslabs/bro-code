package agentic

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotLifecycle(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Snapshot creates the .bro_bak backup.
	if err := Snapshot(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".bro_bak"); err != nil {
		t.Fatalf("backup must exist after Snapshot: %v", err)
	}

	// The file changes; the backup is still the original.
	if err := os.WriteFile(p, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Restore reverts the file AND removes its own backup.
	if err := Restore(p); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "v1" {
		t.Fatalf("Restore must revert to the snapshot, got %q", data)
	}
	if _, err := os.Stat(p + ".bro_bak"); !os.IsNotExist(err) {
		t.Fatal("Restore must remove its backup")
	}

	// CleanupStaleSnapshots removes every live backup (audit fix B1: they used
	// to accumulate forever). A second snapshot after Restore is live again.
	if err := Snapshot(p); err != nil {
		t.Fatal(err)
	}
	if n := CleanupStaleSnapshots(); n != 1 {
		t.Fatalf("expected 1 live backup cleaned, got %d", n)
	}
	if _, err := os.Stat(p + ".bro_bak"); !os.IsNotExist(err) {
		t.Fatal("backup must be removed by CleanupStaleSnapshots")
	}
	// Idempotent: no live backups → nothing to do.
	if n := CleanupStaleSnapshots(); n != 0 {
		t.Fatalf("expected 0 cleanups when none live, got %d", n)
	}
}

func TestSnapshotRegistryBounded(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxSnapBackups+10; i++ {
		p := filepath.Join(dir, "f"+string(rune('a'+i%26))+string(rune('0'+i/26)))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Snapshot(p); err != nil {
			t.Fatal(err)
		}
	}
	// The registry is bounded even when Cleanup never runs.
	snapMu.Lock()
	if len(snapBackups) > maxSnapBackups {
		t.Fatalf("snapshot registry grew unbounded: %d > %d", len(snapBackups), maxSnapBackups)
	}
	snapMu.Unlock()
	CleanupStaleSnapshots()
}
