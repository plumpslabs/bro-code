package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestConventionHardcodedSecret(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.ts", "export const apiKey = \"sk-9ec47ba83de47eed-oowisf-98d00783\";\n")
	issues := checkFileConventions(p)
	found := false
	for _, i := range issues {
		if i.Kind == "hardcoded-secret" && i.Sev == sevCritical {
			found = true
		}
	}
	if !found {
		t.Errorf("expected hardcoded secret flagged critical, got %+v", issues)
	}

	// env-var reads must NOT be flagged.
	p2 := writeFile(t, dir, "env.ts", "const apiKey = process.env.API_KEY;\n")
	if issues := checkFileConventions(p2); len(issues) != 0 {
		t.Errorf("env var read must not be flagged, got %+v", issues)
	}
}

func TestConventionEmptyCatch(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "svc.ts", "try {\n  doThing();\n} catch {}\n")
	issues := checkFileConventions(p)
	found := false
	for _, i := range issues {
		if i.Kind == "empty-catch" && i.Sev == sevError {
			found = true
		}
	}
	if !found {
		t.Errorf("expected empty catch flagged as error, got %+v", issues)
	}
}

func TestConventionSQLInjection(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "db.ts", "const q = \"SELECT * FROM users WHERE id = \" + userId;\n")
	issues := checkFileConventions(p)
	found := false
	for _, i := range issues {
		if i.Kind == "sql-injection" && i.Sev == sevCritical {
			found = true
		}
	}
	if !found {
		t.Errorf("expected SQL injection flagged critical, got %+v", issues)
	}
}

func TestConventionGoSwallowedError(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "main.go", "package main\n\nfunc f() {\n\tif err != nil {}\n}\n")
	issues := checkFileConventions(p)
	for _, i := range issues {
		if i.Kind == "swallowed-error" {
			return
		}
	}
	t.Errorf("expected swallowed error flagged in Go, got %+v", issues)
}

func TestConventionSeverityInFormat(t *testing.T) {
	issues := []conventionIssue{
		{Path: "a.ts", Line: 3, Kind: "hardcoded-secret", Sev: sevCritical, Message: "hardcoded-secret: sk-xxx"},
		{Path: "b.ts", Line: 9, Kind: "marker", Sev: sevInfo, Message: "marker: TODO"},
	}
	out := formatConventionIssues(issues)
	if !strings.Contains(out, "[critical]") || !strings.Contains(out, "[info]") {
		t.Errorf("format should show severity, got %q", out)
	}
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
	engine.SetReviewLLM(false)
	engine.editedFiles = []string{p}
	out := engine.reviewEditedFiles(context.Background())
	if !strings.Contains(out, "console.log") {
		t.Errorf("review should flag console.log, got %q", out)
	}
	if engine.editedFiles != nil {
		t.Error("editedFiles should reset after review")
	}
}

// seniorReviewAdapter returns a fixed senior-review finding instead of an
// answer, so Layer 2 (LLM review) can be tested without a real provider.
type seniorReviewAdapter struct {
	captureAdapter
}

func (s *seniorReviewAdapter) Complete(_ context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if strings.Contains(req.Messages[0].Content, "senior code reviewer") {
		return &provider.CompletionResponse{Content: "svc.js:12 — N+1 query in loop → batch load\n"}, nil
	}
	return &provider.CompletionResponse{Content: "Done"}, nil
}

// blockingReviewAdapter blocks the LLM review call until the context is
// done. Proves the review inherits turn cancellation (ESC) instead of the old
// context.Background, which ignored ESC and could stall the turn up to the
// HTTP client's full timeout.
type blockingReviewAdapter struct{ captureAdapter }

func (s *blockingReviewAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if strings.Contains(req.Messages[0].Content, "senior code reviewer") {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &provider.CompletionResponse{Content: "Done"}, nil
}

func TestReviewLLMUsesTurnContext(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "svc.js", "export function list() {\n  return users.map(u => db.query(u.id));\n}\n")

	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	engine := NewEngine(&blockingReviewAdapter{}, tools, ctxMgr, "test-model")

	engine.editedFiles = []string{p}
	// Canceled up front — the ESC-equivalent state. The review must abort
	// promptly instead of blocking on the provider.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	out := engine.reviewEditedFiles(ctx)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("LLM review ignored turn cancellation: took %v", elapsed)
	}
	if out != "" {
		t.Errorf("review should abort on canceled ctx, got %q", out)
	}
}

func TestReviewLayer2LLMRunsOnceOnCleanLayer1(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "svc.js", "export function list() {\n  return users.map(u => db.query(u.id));\n}\n")

	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &seniorReviewAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	engine.editedFiles = []string{p}
	out := engine.reviewEditedFiles(context.Background())
	// Layer 1 is clean (no console.log/any/TODO), so Layer 2 must run and
	// surface the N+1 finding.
	if !strings.Contains(out, "N+1") {
		t.Errorf("Layer 2 should flag N+1, got %q", out)
	}
}

func TestReviewLayer2SkippedWhenLayer1FindsIssues(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "svc.js", "export function x() {\n  console.log('debug');\n  return 1;\n}\n")

	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &seniorReviewAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	engine.editedFiles = []string{p}
	out := engine.reviewEditedFiles(context.Background())
	// Layer 1 already flagged console.log — Layer 2 must NOT spend tokens.
	if strings.Contains(out, "N+1") {
		t.Errorf("Layer 2 must be skipped when Layer 1 found issues, got %q", out)
	}
	if !strings.Contains(out, "console.log") {
		t.Errorf("Layer 1 should still flag console.log, got %q", out)
	}
}

func TestReviewLayer2CappedPerTurn(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "svc.js", "export function list() {\n  return users.map(u => db.query(u.id));\n}\n")

	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	adapter := &seniorReviewAdapter{}
	engine := NewEngine(adapter, tools, ctxMgr, "test-model")

	// Round 1: clean Layer 1 → Layer 2 runs once.
	engine.editedFiles = []string{p}
	out1 := engine.reviewEditedFiles(context.Background())
	if !strings.Contains(out1, "N+1") {
		t.Errorf("round 1 should run Layer 2, got %q", out1)
	}
	// Round 2 (model edited again after feedback): Layer 2 must NOT run again
	// — the per-turn budget is spent.
	engine.editedFiles = []string{p}
	out2 := engine.reviewEditedFiles(context.Background())
	if strings.Contains(out2, "N+1") {
		t.Errorf("round 2 must skip Layer 2 (budget), got %q", out2)
	}
}

func TestReviewDiagFnFlagsRealDiagnostics(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "app.go", "package main\n")

	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	engine := NewEngine(&captureAdapter{}, tools, ctxMgr, "test-model")
	engine.SetReviewLLM(false)
	engine.SetDiagnosticsChecker(func(path string) string {
		if path != p {
			t.Errorf("diagFn called with %q, want %q", path, p)
		}
		return "warning 2:3  use fmt.Fprintf instead of WriteString"
	})

	engine.editedFiles = []string{p}
	out := engine.reviewEditedFiles(context.Background())
	if !strings.Contains(out, "fmt.Fprintf") {
		t.Errorf("review should surface LSP diagnostics, got %q", out)
	}
}

func TestReviewDiagFnSkipsCleanFiles(t *testing.T) {
	// A clean file must NOT be reported: the diagnostics checker returns
	// "No diagnostics reported for <path>." and that is not an issue.
	dir := t.TempDir()
	p := writeFile(t, dir, "app.go", "package main\n")

	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	engine := NewEngine(&captureAdapter{}, tools, ctxMgr, "test-model")
	engine.SetReviewLLM(false)
	engine.SetDiagnosticsChecker(func(path string) string {
		return "No diagnostics reported for " + path + "."
	})

	engine.editedFiles = []string{p}
	if out := engine.reviewEditedFiles(context.Background()); out != "" {
		t.Errorf("clean LSP file must not be flagged, got %q", out)
	}
}

func TestLocalizeVerifyFailure(t *testing.T) {
	t.Run("nil checker → empty", func(t *testing.T) {
		engine := NewEngine(&captureAdapter{}, tool.NewRegistry(), bcontext.NewManager("s", nil, 128000), "m")
		engine.editedFiles = []string{"/tmp/a.go"}
		if got := engine.localizeVerifyFailure(); got != "" {
			t.Errorf("localizeVerifyFailure = %q, want empty", got)
		}
	})

	t.Run("surfaces real diagnostics", func(t *testing.T) {
		engine := NewEngine(&captureAdapter{}, tool.NewRegistry(), bcontext.NewManager("s", nil, 128000), "m")
		engine.SetDiagnosticsChecker(func(path string) string {
			return "error 1:1  undeclared name: baz"
		})
		engine.editedFiles = []string{"/tmp/a.go", "/tmp/b.go"}
		got := engine.localizeVerifyFailure()
		if !strings.Contains(got, "/tmp/a.go") || !strings.Contains(got, "undeclared name") {
			t.Errorf("localizeVerifyFailure = %q, want file + diagnostic", got)
		}
	})

	t.Run("skips clean files", func(t *testing.T) {
		engine := NewEngine(&captureAdapter{}, tool.NewRegistry(), bcontext.NewManager("s", nil, 128000), "m")
		engine.SetDiagnosticsChecker(func(path string) string {
			return "No diagnostics reported for " + path + "."
		})
		engine.editedFiles = []string{"/tmp/a.go"}
		if got := engine.localizeVerifyFailure(); got != "" {
			t.Errorf("localizeVerifyFailure = %q, want empty for clean file", got)
		}
	})
}
