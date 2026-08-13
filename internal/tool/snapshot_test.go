package tool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUndoMultipleSteps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// v1 -> v2
	if err := Snapshot(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	// v2 -> v3
	if err := Snapshot(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("v3"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := SnapshotCount(); got != 2 {
		t.Fatalf("expected 2 live snapshots, got %d", got)
	}

	// Jump back 2 steps in one call: v3 -> v1
	restored, err := RestoreNSnapshots(2)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 2 {
		t.Fatalf("expected 2 restored, got %d", restored)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "v1" {
		t.Fatalf("expected file back to v1, got %q", content)
	}
	if got := SnapshotCount(); got != 0 {
		t.Fatalf("expected 0 remaining snapshots, got %d", got)
	}
}

func TestUndoToolStepsArgument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	Snapshot(path)
	os.WriteFile(path, []byte("v2"), 0o644)
	Snapshot(path)
	os.WriteFile(path, []byte("v3"), 0o644)

	tool := &UndoTool{}
	out, err := tool.Execute(nil, `{"steps": 2}`)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("expected non-empty undo output")
	}
	content, _ := os.ReadFile(path)
	if string(content) != "v1" {
		t.Fatalf("expected v1 after steps=2, got %q", content)
	}
}

func TestUndoNoSnapshots(t *testing.T) {
	tool := &UndoTool{}
	out, err := tool.Execute(nil, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("expected informative message when nothing to undo")
	}
}
