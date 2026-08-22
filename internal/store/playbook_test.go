package store

import (
	"path/filepath"
	"testing"
)

func TestPlaybookStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	// 1. Record a playbook
	pattern := "Cannot find module '@/../prisma/prisma.client'"
	rootCause := "Path alias required in Jest config"
	solution := "Add moduleNameMapper to jest.config.js"
	if err := s.RecordPlaybook(pattern, rootCause, solution, "jest_fix"); err != nil {
		t.Fatalf("RecordPlaybook failed: %v", err)
	}

	// 2. Match the playbook with raw compiler error text
	rawError := `FAIL tests/unit/service.test.js
  ● Test suite failed to run
    Cannot find module '@/../prisma/prisma.client' from 'src/services/db.js'
    at require (native)`

	pb, err := s.MatchPlaybook(rawError)
	if err != nil {
		t.Fatalf("MatchPlaybook failed: %v", err)
	}
	if pb == nil {
		t.Fatal("expected matching playbook, got nil")
	}
	if pb.Solution != solution {
		t.Errorf("expected solution %q, got %q", solution, pb.Solution)
	}
	if pb.Occurrences != 1 {
		t.Errorf("expected occurrences 1, got %d", pb.Occurrences)
	}

	// 3. Record again (occurrences should increment)
	_ = s.RecordPlaybook(pattern, rootCause, solution, "jest_fix")
	list, err := s.ListPlaybooks(10)
	if err != nil {
		t.Fatalf("ListPlaybooks failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 playbook, got %d", len(list))
	}
	if list[0].Occurrences != 2 {
		t.Errorf("expected occurrences 2, got %d", list[0].Occurrences)
	}
}
