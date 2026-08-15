package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/tool"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func TestUTF16ToByte(t *testing.T) {
	cases := []struct {
		in    string
		units int
		want  int
	}{
		{"hello", 0, 0},
		{"hello", 3, 3},
		{"hello", 99, 5},
		{"héllo", 1, 1},        // é (1 UTF-16 unit) starts at byte 1
		{"a\U00010000b", 2, 1}, // 2 units ends right after 'a'
		{"a\U00010000b", 3, 5}, // 3 units = through the surrogate pair → byte offset of 'b'
	}
	for _, c := range cases {
		if got := utf16ToByte(c.in, c.units); got != c.want {
			t.Errorf("utf16ToByte(%q, %d) = %d, want %d", c.in, c.units, got, c.want)
		}
	}
}

func TestApplyTextEditsSingleLine(t *testing.T) {
	content := "package main\n\nfunc main() {}\n"
	edits := []protocol.TextEdit{
		{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 12}}, NewText: "package foo"},
	}
	got := applyTextEdits(content, edits)
	if got != "package foo\n\nfunc main() {}\n" {
		t.Errorf("applyTextEdits = %q", got)
	}
}

func TestApplyTextEditsDescendingOrder(t *testing.T) {
	content := "abcXYZdef\n"
	// Two edits, intentionally in ascending order: replace "XYZ" and "abc".
	edits := []protocol.TextEdit{
		{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 3}, End: protocol.Position{Line: 0, Character: 6}}, NewText: "123"},
		{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 3}}, NewText: "zzz"},
	}
	got := applyTextEdits(content, edits)
	if got != "zzz123def\n" {
		t.Errorf("applyTextEdits = %q, want %q", got, "zzz123def\n")
	}
}

func TestApplyTextEditsMultiLine(t *testing.T) {
	content := "one\ntwo\nthree\n"
	edits := []protocol.TextEdit{
		{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 2, Character: 5}}, NewText: "ALL"},
	}
	got := applyTextEdits(content, edits)
	if got != "ALL\n" {
		t.Errorf("applyTextEdits = %q, want %q", got, "ALL\n")
	}
}

func TestApplyWorkspaceEditRecordsChanges(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	if err := os.WriteFile(a, []byte("func a() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("func b() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tool.ResetChanges()

	we := &protocol.WorkspaceEdit{
		Changes: map[uri.URI][]protocol.TextEdit{
			uri.File(a): {
				{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 6}}, NewText: "funcA"},
			},
			uri.File(b): {
				{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 6}}, NewText: "funcB"},
			},
		},
	}
	out, err := applyWorkspaceEdit(we)
	if err != nil {
		t.Fatalf("applyWorkspaceEdit: %v", err)
	}
	if !strings.Contains(out, "2 file(s)") {
		t.Errorf("applyWorkspaceEdit summary = %q", out)
	}
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	if string(da) != "funcA() {}\n" || string(db) != "funcB() {}\n" {
		t.Errorf("files after edit: a=%q b=%q", da, db)
	}
	if changes := tool.PeekChanges(); len(changes) != 2 {
		t.Errorf("RecordChange count = %d, want 2", len(changes))
	}
}

func TestApplyWorkspaceEditNilAndEmpty(t *testing.T) {
	if _, err := applyWorkspaceEdit(nil); err == nil {
		t.Error("applyWorkspaceEdit(nil) should error")
	}
	if _, err := applyWorkspaceEdit(&protocol.WorkspaceEdit{}); err == nil {
		t.Error("applyWorkspaceEdit(empty) should error")
	}
}
