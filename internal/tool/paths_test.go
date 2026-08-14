package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolvePath verifies the leading-slash tolerance: a path written as
// "/crm_sales_backend/src" (a common LLM habit) resolves to the project-
// relative form when the absolute one does not exist, while genuine absolute
// paths stay untouched.
func TestResolvePath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte("package a"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Chdir into the temp dir so relative forms resolve (like the CLI cwd).
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	// Leading-slash path that doesn't exist as absolute → relative form wins.
	if got := resolvePath("/sub"); got != "sub" {
		t.Errorf("resolvePath(/sub) = %q, want %q", got, "sub")
	}
	// Genuine absolute path that exists → untouched.
	abs := filepath.Join(dir, "sub")
	if got := resolvePath(abs); got != abs {
		t.Errorf("resolvePath(%q) = %q, want untouched", abs, got)
	}
	// Non-slash paths pass through.
	if got := resolvePath("sub"); got != "sub" {
		t.Errorf("resolvePath(sub) = %q, want %q", got, "sub")
	}
}

// TestGrepLeadingSlashPathIsTolerated proves the end-to-end fix: a grep with
// a leading-slash path ("/sub") that does not exist as absolute must still
// find matches (the exact confusion that made models burn rounds: "grep is
// not finding matches, this is very strange").
func TestGrepLeadingSlashPathIsTolerated(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "svc.js"), []byte("export const rotate = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	gt := &GrepTool{}
	out, err := gt.Execute(context.Background(), `{"pattern":"rotate","path":"/sub"}`)
	if err != nil {
		t.Fatalf("grep with leading-slash path failed: %v", err)
	}
	if !strings.Contains(out, "rotate") {
		t.Fatalf("grep with /sub found nothing (path not resolved?): %q", out)
	}
}

// TestReadFileLeadingSlashPathIsTolerated — same tolerance for read_file.
func TestReadFileLeadingSlashPathIsTolerated(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	rt := &ReadFileTool{}
	out, err := rt.Execute(context.Background(), `{"path":"/sub/a.go"}`)
	if err != nil {
		t.Fatalf("read_file with leading-slash path failed: %v", err)
	}
	if !strings.Contains(out, "package a") {
		t.Fatalf("read_file with /sub/a.go returned wrong content: %q", out)
	}
}
