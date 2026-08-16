package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// scriptedAdapter replays a fixed list of completions, then answers "done"
// forever. Calls with a nil Tools slice are main-loop/review completions, so
// it does not need to distinguish them — the test orders the script.
type scriptedAdapter struct {
	responses []provider.CompletionResponse
	idx       int
}

func (m *scriptedAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if m.idx < len(m.responses) {
		r := m.responses[m.idx]
		m.idx++
		return &r, nil
	}
	return &provider.CompletionResponse{Content: "done"}, nil
}

func editCall(path, target, replacement string) provider.ToolCall {
	// json.Marshal JSON-escapes the payload so multi-line targets/replacements
	// are valid JSON (raw newlines in a JSON string are illegal).
	args, _ := json.Marshal(map[string]string{"path": path, "target": target, "replacement": replacement})
	return provider.ToolCall{
		ID:        "tc_" + path,
		Name:      "edit_file",
		Arguments: string(args),
	}
}

func bashCall(cmd string) provider.ToolCall {
	return provider.ToolCall{
		ID:        "tc_bash_" + cmd,
		Name:      "bash",
		Arguments: `{"command":"` + cmd + `"}`,
	}
}

// newTempGoModule creates a tiny Go module in a temp dir, chdirs into it, and
// returns a cleanup that restores the original cwd and removes the dir. Used so
// planVerification (which reads ".") and runVerification (which executes in
// ".") see a real, isolated project.
func newTempGoModule(t *testing.T, mainGo string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
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

func newEngineWith(adapter provider.ProviderAdapter) (*Engine, *bcontext.Manager) {
	tools := tool.NewRegistry()
	ctxMgr := bcontext.NewManager("test_sess", nil, 128000)
	e := NewEngine(adapter, tools, ctxMgr, "test-model")
	e.reviewLLMEnabled = false
	return e, ctxMgr
}

func contextText(ctxMgr *bcontext.Manager) string {
	var sb strings.Builder
	for _, m := range ctxMgr.Messages() {
		if m.Content != "" {
			sb.WriteString(m.Content);sb.WriteString("\n")
		}
	}
	return sb.String()
}

// TestReproduceGateBlocksEditUntilRepro: the TSR REPRODUCE contract. A bug-fix
// task arms the gate; an edit call before any reproduction is blocked with the
// gate message; after a bash command FAILS (a reproduction), the same edit is
// allowed and actually applied.
func TestReproduceGateBlocksEditUntilRepro(t *testing.T) {
	dir := newTempGoModule(t, "package main\n\n// probe\nfunc main() {}\n")
	adapter := &scriptedAdapter{responses: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{editCall("main.go", "// probe", "// probe-edited")}},
		{ToolCalls: []provider.ToolCall{bashCall("exit 1")}},
		{ToolCalls: []provider.ToolCall{editCall("main.go", "// probe", "// probe-edited")}},
		{Content: "fixed"},
	}}
	e, ctxMgr := newEngineWith(adapter)

	res, err := e.RunTurn(context.Background(), "fix the bug in main.go", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if res != "fixed" {
		t.Fatalf("expected answer 'fixed', got %q", res)
	}
	if !e.reproEstablished {
		t.Fatalf("expected reproEstablished=true after failing bash output")
	}
	txt := contextText(ctxMgr)
	if !strings.Contains(txt, "TSR REPRODUCE GATE") {
		t.Fatalf("expected TSR REPRODUCE GATE message in context, got:\n%s", txt)
	}
	// The final edit must have been allowed (applied to main.go), proving the
	// gate opened after the reproduction.
	data, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "// probe-edited") {
		t.Fatalf("expected edit to have been applied after repro, main.go:\n%s", data)
	}
}

// TestReproduceGateSatisfiedByProvidedRepro: when the user already pasted the
// error/stack trace in the prompt, the gate is pre-satisfied and edits flow
// without any nagging.
func TestReproduceGateSatisfiedByProvidedRepro(t *testing.T) {
	newTempGoModule(t, "package main\n\n// probe\nfunc main() {}\n")
	adapter := &scriptedAdapter{responses: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{editCall("main.go", "// probe", "// probe-edited")}},
		{Content: "fixed"},
	}}
	e, ctxMgr := newEngineWith(adapter)

	query := "fix this error: panic: runtime error, stack:\n./main.go:4: main()\nvar x = boom"
	if _, err := e.RunTurn(context.Background(), query, nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	txt := contextText(ctxMgr)
	if strings.Contains(txt, "TSR REPRODUCE GATE") {
		t.Fatalf("expected no reproduce gate when the user provided the error, got:\n%s", txt)
	}
}

// TestTSRStopsOnIdenticalError: the typed revision contract stops the repair
// loop once the SAME verification error persists across 3 attempts, instead of
// burning iterations — and returns a graceful summary, never a cold failure.
func TestTSRStopsOnIdenticalError(t *testing.T) {
	newTempGoModule(t, "package main\n\nvar x string = 1\n\nfunc main() {}\n")
	adapter := &scriptedAdapter{responses: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{editCall("main.go", "var x string = 1", "var x string = 2")}},
		{Content: "tried fixing"},
		{Content: "tried again"},
		{Content: "tried a third time"},
	}}
	e, _ := newEngineWith(adapter)

	query := "fix the compile error:\nError: ./main.go:3:10: cannot use 1 (untyped int constant) as string value in variable declaration"
	res, err := e.RunTurn(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !strings.Contains(res, "could not be verified") {
		t.Fatalf("expected graceful stop message, got:\n%s", res)
	}
	if e.tsrAttempts < 3 {
		t.Fatalf("expected tsrAttempts>=3 before stopping, got %d", e.tsrAttempts)
	}
	if e.verifyErrorStreak < 3 {
		t.Fatalf("expected verifyErrorStreak>=3, got %d", e.verifyErrorStreak)
	}
	if e.State() != StateDone {
		t.Fatalf("expected StateDone after graceful stop, got %s", e.State())
	}
}

// TestLessonAutoExtractOnRepairSuccess: a verification failure that is later
// repaired distills a durable one-line lesson into project memory ## Gotchas.
func TestLessonAutoExtractOnRepairSuccess(t *testing.T) {
	dir := newTempGoModule(t, "package main\n\nvar x string = 1\n\nfunc main() {}\n")
	adapter := &scriptedAdapter{responses: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{editCall("main.go", "var x string = 1", "var x string = 2")}},
		{Content: "first attempt"},
		{ToolCalls: []provider.ToolCall{editCall("main.go", "var x string = 2", "var x int = 1")}},
		{Content: "second attempt"},
	}}
	e, _ := newEngineWith(adapter)
	e.mem = memory.NewStore(dir)

	query := "fix the compile error:\nError: ./main.go:3:10: cannot use 1 (untyped int constant) as string value in variable declaration"
	if _, err := e.RunTurn(context.Background(), query, nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !e.repairSucceeded {
		t.Fatalf("expected repairSucceeded=true after fail→pass")
	}
	data, err := os.ReadFile(filepath.Join(dir, ".brocode", "memory.md"))
	if err != nil {
		t.Fatalf("memory file not written: %v", err)
	}
	if !strings.Contains(string(data), "Verification failed on main.go") {
		t.Fatalf("expected lesson in memory Gotchas, got:\n%s", data)
	}
	if !strings.Contains(string(data), "repair attempt") {
		t.Fatalf("expected repair attempt count in lesson, got:\n%s", data)
	}
}

// reviewCaptureAdapter serves main-loop replies from a script, and answers the
// LLM review passes from a different lens: the correctness prompt returns CLEAN,
// the security prompt returns a finding. It records which review lenses fired.
type reviewCaptureAdapter struct {
	mainReplies []provider.CompletionResponse
	idx         int
	served      []string
}

func (a *reviewCaptureAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1].Content
		if strings.Contains(last, "You are a senior security & robustness reviewer") {
			a.served = append(a.served, "SECURITY")
			return &provider.CompletionResponse{Content: "main.go:3 — SQL injection risk → parameterize"}, nil
		}
		if strings.Contains(last, "You are a senior code reviewer") {
			a.served = append(a.served, "CORRECTNESS")
			return &provider.CompletionResponse{Content: "main.go:2 — swallowed error → propagate"}, nil
		}
	}
	if a.idx < len(a.mainReplies) {
		r := a.mainReplies[a.idx]
		a.idx++
		return &r, nil
	}
	return &provider.CompletionResponse{Content: "done"}, nil
}

// TestVerifierTwoAngleReview: the correctness lens runs on the first clean
// review round, its finding is fed back, and the security/edge lens runs on the
// SECOND round (after the fix) — so the same code is examined from two angles.
// The edits are deliberately LARGE (>30 lines touched) so the change clears the
// complexity gate that would otherwise collapse the review to a single angle.
func TestVerifierTwoAngleReview(t *testing.T) {
	// Each body block is 40 real lines so every edit touches >30 lines and is
	// classified high-complexity by the review gate.
	block := func(marker string) string {
		var b strings.Builder
		for i := range 40 {
			fmt.Fprintf(&b, "\t_ = %d // %s\n", i, marker)
		}
		return b.String()
	}
	newTempGoModule(t, "package main\n\n// probe\nfunc main() {\n"+block("one")+"}\n")
	adapter := &reviewCaptureAdapter{mainReplies: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{editCall("main.go", block("one"), block("two"))}},
		{Content: "first fix"},
		{ToolCalls: []provider.ToolCall{editCall("main.go", block("two"), block("three"))}},
		{Content: "second fix"},
		{ToolCalls: []provider.ToolCall{editCall("main.go", block("three"), block("four"))}},
		{Content: "done"},
	}}
	e, _ := newEngineWith(adapter)
	e.reviewLLMEnabled = true

	res, err := e.RunTurn(context.Background(), "refactor main.go to add a small helper", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if res != "done" {
		t.Fatalf("expected final answer 'done', got %q", res)
	}
	hasCorrectness, hasSecurity := false, false
	for _, s := range adapter.served {
		if s == "CORRECTNESS" {
			hasCorrectness = true
		}
		if s == "SECURITY" {
			hasSecurity = true
		}
	}
	if !hasCorrectness {
		t.Fatalf("expected correctness review lens to fire, served=%v", adapter.served)
	}
	if !hasSecurity {
		t.Fatalf("expected security review lens to fire on the second clean round, served=%v", adapter.served)
	}
}

// TestVerifierSingleAngleForSmallEdits: a SMALL edit (≤30 lines touched) is
// gated down to a single correctness review — the expensive security/edge lens
// is reserved for high-complexity changes. Deterministic checks always run.
func TestVerifierSingleAngleForSmallEdits(t *testing.T) {
	newTempGoModule(t, "package main\n\n// probe\nfunc main() {}\n")
	adapter := &reviewCaptureAdapter{mainReplies: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{editCall("main.go", "// probe", "// ping")}},
		{Content: "first fix"},
		{ToolCalls: []provider.ToolCall{editCall("main.go", "// ping", "// pong")}},
		{Content: "done"},
	}}
	e, _ := newEngineWith(adapter)
	e.reviewLLMEnabled = true

	res, err := e.RunTurn(context.Background(), "add a small helper", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if res != "done" {
		t.Fatalf("expected final answer 'done', got %q", res)
	}
	hasCorrectness, hasSecurity := false, false
	for _, s := range adapter.served {
		if s == "CORRECTNESS" {
			hasCorrectness = true
		}
		if s == "SECURITY" {
			hasSecurity = true
		}
	}
	if !hasCorrectness {
		t.Fatalf("expected correctness lens to fire on a small edit, served=%v", adapter.served)
	}
	if hasSecurity {
		t.Fatalf("small edits must not trigger the security lens (complexity gate), served=%v", adapter.served)
	}
}

func TestDiffTouchedLines(t *testing.T) {
	one := "a\nb\nc\n"
	if n := diffTouchedLines(one, one); n != 0 {
		t.Fatalf("identical text must touch 0 lines, got %d", n)
	}
	if n := diffTouchedLines("", "a\nb\nc\n"); n != 3 {
		t.Fatalf("created file should touch 3 lines, got %d", n)
	}
	if n := diffTouchedLines("a\nb\nc\n", "a\nx\nc\n"); n != 2 {
		t.Fatalf("one-line swap = 2 touched (1 removed + 1 added), got %d", n)
	}
	// 40-line replacement → ~80 touched lines (40 removed + 40 added) → high complexity.
	bigA, bigB := "", ""
	for i := range 40 {
		bigA += fmt.Sprintf("_ = %d\n", i)
		bigB += fmt.Sprintf("_ = %d // v2\n", i)
	}
	if n := diffTouchedLines(bigA, bigB); n <= 30 {
		t.Fatalf("40-line replacement should exceed the 30-line gate, got %d", n)
	}
}

func TestLooksLikeBugFixTask(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"fix the panic in the handler", true},
		{"the login endpoint errors on empty input", true},
		{"refactor the auth module for clarity", false},
		{"add a new /health endpoint", false},
		{"gagal login, tidak jalan", true},
	}
	for _, c := range cases {
		if got := looksLikeBugFixTask(c.query); got != c.want {
			t.Errorf("looksLikeBugFixTask(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestLooksLikeProvidedRepro(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"this crashes: panic: runtime error", true},
		{"fix this:\nError: ./main.go:4:14: cannot use 1 as string", true},
		{"go test fails with exit status 1", true},
		{"please improve the error handling", false},
		{"refactor main.go", false},
	}
	for _, c := range cases {
		if got := looksLikeProvidedRepro(c.query); got != c.want {
			t.Errorf("looksLikeProvidedRepro(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestLooksLikeFailure(t *testing.T) {
	if !looksLikeFailure("Command failed with error: exit status 1") {
		t.Error("expected bash failure output to be a failure")
	}
	if !looksLikeFailure("--- FAIL: TestFoo (main_test.go:42)") {
		t.Error("expected go test failure to be a failure")
	}
	if looksLikeFailure("Command executed successfully with no output.") {
		t.Error("expected success output to NOT be a failure")
	}
	if looksLikeFailure("") {
		t.Error("empty output must not be a failure")
	}
}

func TestVerifyErrorSignature(t *testing.T) {
	a := "error: cannot use 1 as string\n\t./main.go:3:16\n\tcompile"
	b := "error: cannot use 1 as string\n\t./main.go:3:16\n\tcompile (different tail)"
	c := "error: undefined: foo\n\t./main.go:5:10"
	if verifyErrorSignature(a) == "" {
		t.Fatal("expected non-empty signature")
	}
	if verifyErrorSignature(a) != verifyErrorSignature(b) {
		t.Error("expected near-identical errors to share a signature")
	}
	if verifyErrorSignature(a) == verifyErrorSignature(c) {
		t.Error("expected different errors to differ")
	}
}
