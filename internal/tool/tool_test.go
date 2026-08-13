package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltinTools(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.txt")

	reg := NewRegistry()
	ctx := context.Background()

	// Write File
	writeArgs := `{"path":"` + filePath + `","content":"hello line 1\nhello line 2"}`
	res, err := reg.Execute(ctx, "write_file", writeArgs)
	if err != nil {
		t.Fatalf("write_file failed: %v", err)
	}

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Fatalf("expected file to exist after write_file: %s", res)
	}

	// Read File
	readArgs := `{"path":"` + filePath + `"}`
	content, err := reg.Execute(ctx, "read_file", readArgs)
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}

	if content != "hello line 1\nhello line 2" {
		t.Errorf("unexpected content: %s", content)
	}

	// Edit File
	editArgs := `{"path":"` + filePath + `","target":"line 1","replacement":"updated line 1"}`
	_, err = reg.Execute(ctx, "edit_file", editArgs)
	if err != nil {
		t.Fatalf("edit_file failed: %v", err)
	}

	// Verify Edit
	content, _ = reg.Execute(ctx, "read_file", readArgs)
	if content != "hello updated line 1\nhello line 2" {
		t.Errorf("unexpected edited content: %s", content)
	}
}
