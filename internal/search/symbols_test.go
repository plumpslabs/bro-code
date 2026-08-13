package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractGoSymbols(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "sample.go")
	content := `package main

type Config struct {
	Name string
}

type Runner interface {
	Run()
}

func ExecuteTask(c Config) error {
	return nil
}
`
	if err := os.WriteFile(goFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	syms, err := ExtractSymbols(goFile)
	if err != nil {
		t.Fatalf("ExtractSymbols failed: %v", err)
	}

	if len(syms) != 3 {
		t.Fatalf("expected 3 symbols, got %d: %+v", len(syms), syms)
	}

	summary := FormatSymbolSummary([]string{goFile})
	if !strings.Contains(summary, "ExecuteTask") || !strings.Contains(summary, "Config") || !strings.Contains(summary, "Runner") {
		t.Fatalf("expected symbols in summary, got:\n%s", summary)
	}
}

func TestExtractGenericSymbols(t *testing.T) {
	dir := t.TempDir()
	pyFile := filepath.Join(dir, "sample.py")
	content := `class UserAgent:
    def __init__(self):
        pass

def process_data():
    pass
`
	if err := os.WriteFile(pyFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	syms, err := ExtractSymbols(pyFile)
	if err != nil {
		t.Fatalf("ExtractSymbols generic failed: %v", err)
	}

	if len(syms) < 2 {
		t.Fatalf("expected at least 2 symbols, got %d: %+v", len(syms), syms)
	}
}

func TestBuildRepoMap(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "models.go")
	contentA := `package main
type User struct { ID string }
type Order struct { ID string }
`
	fileB := filepath.Join(dir, "service.go")
	contentB := `package main
func ProcessUser(u User) {}
func ProcessOrder(o Order) {}
`
	_ = os.WriteFile(fileA, []byte(contentA), 0o644)
	_ = os.WriteFile(fileB, []byte(contentB), 0o644)

	repoMap := BuildRepoMap(dir, 5)
	if !strings.Contains(repoMap, "models.go") || !strings.Contains(repoMap, "User") {
		t.Fatalf("expected models.go in repoMap, got:\n%s", repoMap)
	}
}
