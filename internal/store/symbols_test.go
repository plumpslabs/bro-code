package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractSymbolsGo(t *testing.T) {
	src := `package main

import "fmt"

func (s *Server) handleLogin() {
	x := 1
}

func parseConfig() {
	y := 2
}

type User struct {
	Name string
}
`
	syms := extractSymbols(src, "go")
	if len(syms) == 0 {
		t.Fatal("expected symbols, got none")
	}
	// Find handleLogin and verify it spans from its def line to before the next.
	var login SymbolRange
	for _, s := range syms {
		if s.Name == "handleLogin" {
			login = s
		}
	}
	if login.Name == "" {
		t.Fatalf("handleLogin not found in %+v", syms)
	}
	if login.Kind != "method" {
		t.Errorf("expected kind method, got %s", login.Kind)
	}
	if login.Start < 1 {
		t.Errorf("expected positive start line, got %d", login.Start)
	}
	if login.End < login.Start {
		t.Errorf("End (%d) must be >= Start (%d)", login.End, login.Start)
	}
}

func TestUpdateAndRecallSymbols(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewStore(filepath.Join(tmpDir, "test_brocode.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	// A 5000-line file: the model only ever reads a slice, but the graph must
	// know every symbol's position.
	var b strings.Builder
	b.WriteString("package big\n\n")
	b.WriteString("func alpha() {}\n")
	b.WriteString("func beta() {}\n")
	for i := 0; i < 10; i++ {
		b.WriteString("func gamma() {}\n")
	}
	for i := 0; i < 4940; i++ {
		b.WriteString("// padding\n")
	}
	b.WriteString("func omega() {}\n")
	content := b.String()

	// Simulate a partial read: only the head is "shown", but capture uses full content.
	if err := st.UpdateKnowledge("file:big.go", "go", content, nil, nil); err != nil {
		t.Fatalf("UpdateKnowledge: %v", err)
	}

	hints, err := st.QueryKnowledge("omega handler")
	if err != nil {
		t.Fatalf("QueryKnowledge: %v", err)
	}
	if len(hints) == 0 {
		t.Fatal("expected to recall big.go")
	}
	found := false
	for _, h := range hints {
		for _, s := range h.Entry.Symbols {
			if s.Name == "omega" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("omega symbol not indexed across the whole file: %+v", hints[0].Entry.Symbols)
	}

	// FormatKnowledgeHints must surface line spans so the model can jump.
	formatted := FormatKnowledgeHints(hints, "omega handler")
	if !strings.Contains(formatted, "L") {
		t.Fatalf("expected line spans in hints, got:\n%s", formatted)
	}
	if !strings.Contains(formatted, "omega") {
		t.Fatalf("expected omega symbol named in hints, got:\n%s", formatted)
	}
}
