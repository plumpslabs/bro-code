package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConventionDebugLeftovers(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.ts", "export function x() {\n  console.log('debug here');\n  const y: any = foo;\n  return y;\n}\n")
	issues := checkFileConventions(p)
	var kinds []string
	for _, i := range issues {
		kinds = append(kinds, i.Kind)
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, "debug") {
		t.Errorf("expected debug issue, got %q", joined)
	}
	if !strings.Contains(joined, "type-safety") {
		t.Errorf("expected type-safety issue, got %q", joined)
	}
}

func TestConventionMarkers(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "svc.py", "def foo():\n    # TODO: implement later\n    pass\n")
	issues := checkFileConventions(p)
	found := false
	for _, i := range issues {
		if i.Kind == "marker" && i.Line == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected TODO marker at line 2, got %+v", issues)
	}
}

func TestConventionGoPrintln(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "main.go", "package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n")
	issues := checkFileConventions(p)
	for _, i := range issues {
		if i.Kind == "debug" {
			return
		}
	}
	t.Errorf("expected fmt.Println flagged in Go, got %+v", issues)
}

func TestConventionCleanFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "clean.ts", "export function add(a: number, b: number): number {\n  return a + b;\n}\n")
	if issues := checkFileConventions(p); len(issues) != 0 {
		t.Errorf("clean file should have no issues, got %+v", issues)
	}
}

func TestDuplicateSymbols(t *testing.T) {
	dir := t.TempDir()
	known := map[string]map[string]bool{
		"old/helpers.ts": {"formatDate": true},
	}
	os.MkdirAll(filepath.Join(dir, "new"), 0755)
	p := writeFile(t, dir, "new/utils.ts", "export function formatDate(d: Date): string {\n  return d.toISOString();\n}\n")
	issues := findDuplicateSymbols([]string{p}, known)
	found := false
	for _, i := range issues {
		if i.Kind == "duplicate" && strings.Contains(i.Message, "formatDate") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected duplicate formatDate flagged, got %+v", issues)
	}
}

func TestExtractToolPath(t *testing.T) {
	if got := extractToolPath(`{"path":"src/app.ts","content":"x"}`); got != "src/app.ts" {
		t.Errorf("expected src/app.ts, got %q", got)
	}
	if got := extractToolPath(`{"content":"x"}`); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestUsageTrackerPricing(t *testing.T) {
	u := NewUsageTracker()
	u.Record("deepseek-chat", provider.Usage{PromptTokens: 1000000, CompletionTokens: 0, TotalTokens: 1000000})
	if u.TotalCost() < 0.26 || u.TotalCost() > 0.28 {
		t.Errorf("deepseek-chat 1M input should cost ~$0.27, got $%.4f", u.TotalCost())
	}
	// Free models cost nothing.
	u2 := NewUsageTracker()
	u2.Record("deepseek-v4-flash-free", provider.Usage{PromptTokens: 1000000, TotalTokens: 1000000})
	if u2.TotalCost() != 0 {
		t.Errorf("free model should cost $0, got $%.4f", u2.TotalCost())
	}
	// Unknown models report $0 (no hallucination).
	u3 := NewUsageTracker()
	u3.Record("some/unknown-model", provider.Usage{PromptTokens: 100000, TotalTokens: 100000})
	if u3.TotalCost() != 0 {
		t.Errorf("unknown model should cost $0, got $%.4f", u3.TotalCost())
	}
	// Summary mentions the model and total.
	if s := u.Summary(); !strings.Contains(s, "deepseek-chat") || !strings.Contains(s, "TOTAL") {
		t.Errorf("summary missing model/total: %s", s)
	}
}

func TestReviewEditedFilesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.ts", "export function x() {\n  console.log('oops');\n  return 1;\n}\n")

	// Engine wired with the edited file tracked → review must flag the debug.
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	engine := NewEngine(&captureAdapter{}, tools, ctxMgr, "test-model")
	engine.editedFiles = []string{p}
	out := engine.reviewEditedFiles()
	if !strings.Contains(out, "console.log") {
		t.Errorf("review should flag console.log, got %q", out)
	}
	if engine.editedFiles != nil {
		t.Error("editedFiles should reset after review")
	}
}
