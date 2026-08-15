package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeModule creates a Go module in a temp dir, chdirs into it, and restores
// the original cwd on cleanup.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	return dir
}

func TestRunTestsStructuredFail(t *testing.T) {
	writeModule(t, map[string]string{
		"go.mod":       "module probe\n\ngo 1.21\n",
		"math.go":      "package probe\n\nfunc Add(a, b int) int { return a }\n",
		"math_test.go": "package probe\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"expected 3\")\n\t}\n}\n",
	})
	rt := &RunTestsTool{Plan: func() []string { return []string{"go test ./..."} }}

	out, err := rt.Execute(context.Background(), "{}")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "run_tests: executed 1 command(s)") {
		t.Errorf("expected executed-count line, got:\n%s", out)
	}
	if !strings.Contains(out, "[FAIL] go test ./...") {
		t.Errorf("expected FAIL marker for the command, got:\n%s", out)
	}
	if !strings.Contains(out, "TestAdd") {
		t.Errorf("expected failing test name in output, got:\n%s", out)
	}
	if !strings.Contains(out, "1 package(s) failed") || !strings.Contains(out, "0 package(s) passed") {
		t.Errorf("expected structured package summary, got:\n%s", out)
	}
}

func TestRunTestsStructuredPass(t *testing.T) {
	writeModule(t, map[string]string{
		"go.mod":       "module probe\n\ngo 1.21\n",
		"math.go":      "package probe\n\nfunc Add(a, b int) int { return a + b }\n",
		"math_test.go": "package probe\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"expected 3\")\n\t}\n}\n",
	})
	rt := &RunTestsTool{Plan: func() []string { return []string{"go test ./..."} }}

	out, err := rt.Execute(context.Background(), "{}")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "[PASS] go test ./...") {
		t.Errorf("expected PASS marker for the command, got:\n%s", out)
	}
	if !strings.Contains(out, "ALL COMMANDS PASSED.") {
		t.Errorf("expected ALL COMMANDS PASSED summary, got:\n%s", out)
	}
}

func TestRunTestsNoPlanDetects(t *testing.T) {
	// With no injected Plan, defaultTestPlan must detect go.mod and run go test.
	writeModule(t, map[string]string{
		"go.mod":  "module probe\n\ngo 1.21\n",
		"math.go": "package probe\n\nfunc Add(a, b int) int { return a + b }\n",
	})
	rt := &RunTestsTool{}

	out, err := rt.Execute(context.Background(), "{}")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "go test ./...") {
		t.Errorf("expected go test detection, got:\n%s", out)
	}
	if !strings.Contains(out, "ALL COMMANDS PASSED.") {
		t.Errorf("expected ALL COMMANDS PASSED summary, got:\n%s", out)
	}
}
