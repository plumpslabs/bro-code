package diff

import (
	"strings"
	"testing"
)

func TestUnifiedShowsChanges(t *testing.T) {
	before := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	after := "package main\n\nfunc main() {\n\tname := \"brocode\"\n\tfmt.Println(\"hello\", name)\n}\n"

	got := Unified("main.go", "main.go", before, after)
	if !strings.Contains(got, "+") || !strings.Contains(got, "-") {
		t.Fatalf("expected +/- hunks in diff, got:\n%s", got)
	}
	if !strings.Contains(got, `name := "brocode"`) {
		t.Fatalf("expected added line in diff, got:\n%s", got)
	}
}

func TestUnifiedEqualTexts(t *testing.T) {
	text := "line one\nline two\n"
	got := Unified("a.txt", "a.txt", text, text)
	// No added/removed lines allowed (headers may still be present).
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			t.Fatalf("expected no changes for identical texts, got line %q", line)
		}
	}
}

func TestUnifiedEmptyInput(t *testing.T) {
	// Empty before, non-empty after — everything must be additions, no panic.
	got := Unified("new.txt", "new.txt", "", "hello\nworld\n")
	if !strings.Contains(got, "+hello") {
		t.Fatalf("expected added lines for empty before, got:\n%s", got)
	}
}
