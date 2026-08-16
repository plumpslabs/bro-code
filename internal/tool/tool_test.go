package tool

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/provider"
)

func TestReadFileBlockedForSensitiveAndHeavy(t *testing.T) {
	tmpDir := t.TempDir()
	// Sensitive file exists on disk — reading must still be blocked.
	envPath := filepath.Join(tmpDir, ".env")
	if err := os.WriteFile(envPath, []byte("SECRET=abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	nmPath := filepath.Join(tmpDir, "node_modules", "pkg", "index.js")
	if err := os.MkdirAll(filepath.Dir(nmPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nmPath, []byte("module.exports = 1"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &ReadFileTool{}
	// .env must be blocked and must NOT leak its content.
	out, err := rt.Execute(context.Background(), `{"path":"`+envPath+`"}`)
	if err == nil {
		t.Fatalf("read_file(.env) must be blocked, got: %q", out)
	}
	if strings.Contains(out+err.Error(), "SECRET=abc") {
		t.Error("secret content leaked into the error/response")
	}
	// node_modules must be blocked too.
	if _, err := rt.Execute(context.Background(), `{"path":"`+nmPath+`"}`); err == nil {
		t.Error("read_file(node_modules) must be blocked")
	}
	// A normal file still reads fine.
	okPath := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(okPath, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Execute(context.Background(), `{"path":"`+okPath+`"}`); err != nil {
		t.Errorf("read_file(main.go) must work, got %v", err)
	}
}

// TestReadFileTruncationGuidance verifies a large file (>150 lines) returns a
// short head preview with ACTIONABLE guidance (start_line/end_line ranges,
// code_locate, shrinkwrap) — not a whole-file dump that makes the model ingest
// code it only needs one span of, nor a vague "request more" that triggers
// bash sed/head/tail truncation-fighting loops.
func TestReadFileTruncationGuidance(t *testing.T) {
	tmpDir := t.TempDir()
	big := filepath.Join(tmpDir, "big.js")
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		sb.WriteString(fmt.Sprintf("// line %d\n", i))
	}
	if err := os.WriteFile(big, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &ReadFileTool{}
	out, err := rt.Execute(context.Background(), `{"path":"`+big+`"}`)
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	if !strings.Contains(out, "501 lines") {
		t.Fatalf("structural overview notice missing line count: %q", out)
	}
	if !strings.Contains(out, "start_line/end_line") || !strings.Contains(out, "code_locate") {
		t.Fatalf("structural overview must give actionable guidance: %q", out)
	}
	if strings.Contains(out, "line 300") {
		t.Fatalf("read_file returned content past the first 60 lines of the overview")
	}

	// A range read returns the requested section.
	rangeOut, err := rt.Execute(context.Background(), `{"path":"`+big+`","start_line":200,"end_line":205}`)
	if err != nil {
		t.Fatalf("range read failed: %v", err)
	}
	if !strings.Contains(rangeOut, "line 200") || !strings.Contains(rangeOut, "line 205") {
		t.Fatalf("range read missing expected lines: %q", rangeOut)
	}
}

// TestReadFileLazyRange verifies a start_line/end_line read streams only the
// requested span and never loads the rest of a large file into the response.
func TestReadFileLazyRange(t *testing.T) {
	tmpDir := t.TempDir()
	big := filepath.Join(tmpDir, "big.txt")
	var sb strings.Builder
	for i := 1; i <= 1000; i++ {
		sb.WriteString(fmt.Sprintf("line %d\n", i))
	}
	if err := os.WriteFile(big, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &ReadFileTool{}
	out, err := rt.Execute(context.Background(), `{"path":"`+big+`","start_line":400,"end_line":403}`)
	if err != nil {
		t.Fatalf("lazy range read failed: %v", err)
	}
	if !strings.Contains(out, "line 400") || !strings.Contains(out, "line 403") {
		t.Fatalf("range read missing expected lines: %q", out)
	}
	if strings.Contains(out, "line 1\n") || strings.Contains(out, "line 999") {
		t.Fatalf("range read returned out-of-span content: %q", out)
	}
}

// TestEditFilePositional verifies a start_line/end_line edit replaces just that
// span WITHOUT requiring the caller to read the whole file first.
func TestEditFilePositional(t *testing.T) {
	tmpDir := t.TempDir()
	f := filepath.Join(tmpDir, "app.go")
	content := "package main\n\nfunc a() {}\n\nfunc b() {}\n\nfunc c() {}\n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	et := &EditFileTool{}
	// func a() {} is line 3; replace lines [3,3] (inclusive) with the new signature.
	args := `{"path":"` + f + `","start_line":3,"end_line":3,"replacement":"func a() int { return 1 }"}`
	out, err := et.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("positional edit failed: %v", err)
	}
	if !strings.Contains(out, "Successfully updated") || !strings.Contains(out, "lines 3-3") {
		t.Fatalf("expected positional-edit confirmation, got: %q", out)
	}
	got, _ := os.ReadFile(f)
	want := "package main\n\nfunc a() int { return 1 }\n\nfunc b() {}\n\nfunc c() {}\n"
	if string(got) != want {
		t.Fatalf("positional edit produced wrong file:\n got=%q\nwant=%q", string(got), want)
	}
}

// TestWriteFileAppend verifies append=true adds to the end instead of replacing.
func TestWriteFileAppend(t *testing.T) {
	tmpDir := t.TempDir()
	f := filepath.Join(tmpDir, "log.txt")
	if err := os.WriteFile(f, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := &WriteFileTool{}
	if _, err := wt.Execute(context.Background(), `{"path":"`+f+`","content":"second\n","append":true}`); err != nil {
		t.Fatalf("append write failed: %v", err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "first\nsecond\n" {
		t.Fatalf("append write wrong result: %q", string(got))
	}
	// Default (no append) replaces the whole file.
	if _, err := wt.Execute(context.Background(), `{"path":"`+f+`","content":"only\n"}`); err != nil {
		t.Fatalf("overwrite write failed: %v", err)
	}
	got, _ = os.ReadFile(f)
	if string(got) != "only\n" {
		t.Fatalf("overwrite write wrong result: %q", string(got))
	}
}

// TestReadFileShrinkwrap verifies the opt-in AST shrinkwrap mode returns a
// structural overview of a large file (signatures/types retained, bodies
// stripped) with a notice — the anti-bloat path for big-file understanding.
func TestReadFileShrinkwrap(t *testing.T) {
	tmpDir := t.TempDir()
	big := filepath.Join(tmpDir, "svc.go")
	var sb strings.Builder
	sb.WriteString("package svc\n\ntype Service struct{}\n\n")
	for i := 1; i <= 60; i++ {
		sb.WriteString("func (s *Service) Method" + fmt.Sprintf("%d", i) + "(in string) string {\n")
		sb.WriteString("    x := in + \"processed\"\n")
		sb.WriteString("    y := len(x)\n")
		sb.WriteString("    return fmt.Sprintf(\"%s:%d\", x, y)\n")
		sb.WriteString("}\n\n")
	}
	if err := os.WriteFile(big, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &ReadFileTool{}
	out, err := rt.Execute(context.Background(), `{"path":"`+big+`","shrinkwrap":true}`)
	if err != nil {
		t.Fatalf("shrinkwrap read failed: %v", err)
	}
	if !strings.Contains(out, "AST shrinkwrap view") {
		t.Fatalf("expected shrinkwrap notice, got: %.200s", out)
	}
	if !strings.Contains(out, "type Service struct") {
		t.Errorf("expected type retained in shrinkwrap output")
	}
	if !strings.Contains(out, "func (s *Service) Method1") {
		t.Errorf("expected function signature retained in shrinkwrap output")
	}
	if strings.Contains(out, "processed") {
		t.Errorf("function BODIES must be stripped in shrinkwrap output")
	}
	// A small file with shrinkwrap:true stays intact (no notice, no stripping).
	small := filepath.Join(tmpDir, "small.go")
	if err := os.WriteFile(small, []byte("package main\n\nfunc main() { println(\"hi\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	smallOut, err := rt.Execute(context.Background(), `{"path":"`+small+`","shrinkwrap":true}`)
	if err != nil {
		t.Fatalf("small shrinkwrap read failed: %v", err)
	}
	if strings.Contains(smallOut, "AST shrinkwrap view") {
		t.Errorf("small files should stay intact with shrinkwrap:true, got notice")
	}
	if !strings.Contains(smallOut, "println") {
		t.Errorf("small file body must be preserved")
	}
}

func TestWriteEditBlockedForSensitive(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")

	wt := &WriteFileTool{}
	if _, err := wt.Execute(context.Background(), `{"path":"`+envPath+`","content":"NEWSECRET=1"}`); err == nil {
		t.Error("write_file(.env) must be blocked")
	}

	et := &EditFileTool{}
	if _, err := et.Execute(context.Background(), `{"path":"`+envPath+`","target":"a","replacement":"b"}`); err == nil {
		t.Error("edit_file(.env) must be blocked")
	}
}

func TestMemoryToolRecallRetainList(t *testing.T) {
	tmpDir := t.TempDir()
	st := memory.NewStore(tmpDir)
	mt := &MemoryTool{Store: st}

	// retain a fact.
	out, err := mt.Execute(context.Background(), `{"action":"retain","section":"Architecture","fact":"Auth flow: JWT via authMiddleware"}`)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if !strings.Contains(out, "Stored") {
		t.Errorf("retain = %q, want confirmation", out)
	}

	// recall it.
	out, err = mt.Execute(context.Background(), `{"action":"recall","query":"auth jwt"}`)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !strings.Contains(out, "authMiddleware") {
		t.Errorf("recall = %q, want fact in results", out)
	}

	// list shows it.
	out, err = mt.Execute(context.Background(), `{"action":"list"}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "authMiddleware") {
		t.Errorf("list = %q, want fact listed", out)
	}

	// nil store reports unavailable.
	mtNil := &MemoryTool{}
	out, _ = mtNil.Execute(context.Background(), `{"action":"list"}`)
	if !strings.Contains(out, "not available") {
		t.Errorf("nil store = %q, want unavailable note", out)
	}
}

func TestGrepNoMatchRunsOnce(t *testing.T) {
	// A pattern with no matches must NOT re-run grep with -F (the old bug:
	// exit code 1 == "no matches" was treated as an error, triggering a second
	// full scan of the tree on every empty result).
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}
	oldWD, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	gt := &GrepTool{}
	out, err := gt.Execute(context.Background(), `{"pattern":"zzz-nothing-matches-zzz"}`)
	if err != nil {
		t.Fatalf("grep no-match: %v", err)
	}
	if !strings.Contains(out, "No matches") {
		t.Errorf("grep no-match = %q, want 'No matches found'", out)
	}
}

// TestToolSurfacePruned verifies the structural pruning: the redundant
// per-file code_symbols tool is NOT part of the exposed surface (code_locate
// + search_code + read_file ranges cover symbol navigation), while the core
// read/write/explore tools remain registered.
func TestToolSurfacePruned(t *testing.T) {
	reg := NewRegistry()
	defs := reg.Definitions()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if names["code_symbols"] {
		t.Fatal("code_symbols should have been pruned (redundant with code_locate/search_code/read_file)")
	}
	for _, want := range []string{
		"read_file", "write_file", "edit_file", "delete_file", "list_dir",
		"grep", "glob", "bash", "ask_user", "fetch_url", "git", "undo",
		"web_search", "review_changes", "search_code", "memory", "run_tests",
	} {
		if !names[want] {
			t.Errorf("expected tool %q to be registered", want)
		}
	}
}

func TestReadOnlyExecutionPolicy(t *testing.T) {
	t.Cleanup(func() { CleanupStaleSnapshots() })
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "x.txt")
	os.WriteFile(target, []byte("hi\n"), 0644)

	reg := NewRegistry()
	reg.SetExecutionPolicy(true, false) // read-only files, bash allowed (MINER)

	// Execute path: mutating tools are hard-blocked at the executor level.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"write_file", `{"path":"` + tmpDir + `/new.txt","content":"x"}`},
		{"edit_file", `{"path":"` + target + `","target":"hi","replacement":"bye"}`},
		{"delete_file", `{"path":"` + target + `"}`},
		{"lsp_fix", `{"path":"` + target + `"}`},
		{"lsp_rename", `{"path":"` + target + `","line":1,"col":1,"newName":"x"}`},
	} {
		if _, err := reg.Execute(context.Background(), tc.name, tc.args); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("expected %q blocked in read-only mode, got err=%v", tc.name, err)
		}
	}

	// GateAction path (the engine loop): also denied.
	ok, _, err := reg.GateAction(context.Background(), provider.ToolCall{Name: "write_file"})
	if err != nil || ok {
		t.Fatalf("expected GateAction to deny write_file in read-only mode, ok=%v err=%v", ok, err)
	}

	// Read tools still work under a read-only policy.
	if out, err := reg.Execute(context.Background(), "read_file", `{"path":"`+target+`"}`); err != nil || !strings.Contains(out, "hi") {
		t.Fatalf("expected read_file to work in read-only mode, out=%q err=%v", out, err)
	}

	// SubRegistry inherits the policy — a MINER subagent cannot mutate either.
	sub := reg.SubRegistry()
	if _, err := sub.Execute(context.Background(), "write_file", `{"path":"`+tmpDir+`/s.txt","content":"x"}`); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected subagent write_file to be blocked via inherited policy, err=%v", err)
	}

	// PLANNER additionally blocks bash.
	reg.SetExecutionPolicy(true, true)
	if _, err := reg.Execute(context.Background(), "bash", `{"command":"echo hi"}`); err == nil || !strings.Contains(err.Error(), "bash") {
		t.Fatalf("expected bash blocked in PLANNER policy, err=%v", err)
	}

	// Clearing the policy restores execution (write now actually runs).
	reg.SetExecutionPolicy(false, false)
	if _, err := reg.Execute(context.Background(), "write_file", `{"path":"`+tmpDir+`/y.txt","content":"ok"}`); err != nil {
		t.Fatalf("expected write_file allowed after clearing policy, err=%v", err)
	}
}

func TestSearchCodeTool(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tmpDir, "src", "payment.js"), []byte("function payViaMidtrans() {} // payment gateway integration"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "auth.js"), []byte("function login() {} // jwt token auth"), 0644)

	oldWD, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	reg := NewRegistry()
	out, err := reg.Execute(context.Background(), "search_code", `{"query":"payment gateway","path":"src"}`)
	if err != nil {
		t.Fatalf("search_code: %v", err)
	}
	if !strings.Contains(out, "payment.js") {
		t.Errorf("search_code should rank payment.js first: %q", out)
	}
	if strings.Contains(out, "auth.js") && strings.Index(out, "payment.js") > strings.Index(out, "auth.js") {
		t.Errorf("payment.js should rank above auth.js: %q", out)
	}
}

func TestGlobToolLeadingSlashAndRecursive(t *testing.T) {
	tmpDir := t.TempDir()
	sub := filepath.Join(tmpDir, "src", "services", "conversation")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "ConversationService.js"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "top.go"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	oldWD, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	gt := &GlobTool{}
	ctx := context.Background()

	// Leading "/" pattern with a wildcard must be treated as repo-rooted
	// (recursive), not as an absolute filesystem path that never matches.
	out, err := gt.Execute(ctx, `{"pattern":"/ConversationService*"}`)
	if err != nil {
		t.Fatalf("glob with leading slash: %v", err)
	}
	if !strings.Contains(out, "ConversationService.js") {
		t.Errorf("leading-slash recursive glob = %q, want ConversationService.js", out)
	}

	// Bare name without wildcard should find the file recursively too.
	out, err = gt.Execute(ctx, `{"pattern":"/conversation"}`)
	if err != nil {
		t.Fatalf("glob bare name: %v", err)
	}
	if !strings.Contains(out, "conversation") {
		t.Errorf("bare-name glob = %q, want a conversation path", out)
	}

	// Plain wildcard in cwd still works.
	out, err = gt.Execute(ctx, `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatalf("glob plain: %v", err)
	}
	if !strings.Contains(out, "top.go") {
		t.Errorf("plain glob = %q, want top.go", out)
	}
}

func TestBuiltinTools(t *testing.T) {
	// write_file/edit_file snapshot the file for one-turn rollback; clean up so
	// the package-global snapshot list doesn't leak into other tests.
	t.Cleanup(func() { CleanupStaleSnapshots() })
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.txt")

	reg := NewRegistry()
	ctx := context.Background()

	// Write File
	writeArgs := `{"path":"` + filePath + `","content":"hello line 1\nhello line 2"}`
	res, err := reg.Execute(ctx, "write_file", writeArgs)
	if err != nil {
		t.Fatalf("write_file failed: %v", err)
	}

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Fatalf("expected file to exist after write_file: %s", res)
	}

	// Read File
	readArgs := `{"path":"` + filePath + `"}`
	content, err := reg.Execute(ctx, "read_file", readArgs)
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}

	if content != "hello line 1\nhello line 2" {
		t.Errorf("unexpected content: %s", content)
	}

	// Edit File
	editArgs := `{"path":"` + filePath + `","target":"line 1","replacement":"updated line 1"}`
	_, err = reg.Execute(ctx, "edit_file", editArgs)
	if err != nil {
		t.Fatalf("edit_file failed: %v", err)
	}

	// Verify Edit
	content, _ = reg.Execute(ctx, "read_file", readArgs)
	if content != "hello updated line 1\nhello line 2" {
		t.Errorf("unexpected edited content: %s", content)
	}
}

func TestAskUserToolReturnsAnswers(t *testing.T) {
	ctx := context.Background()
	var got []AskQuestion

	at := &AskUserTool{Ask: func(_ context.Context, qs []AskQuestion) ([]AskResult, error) {
		got = qs
		return []AskResult{{Question: qs[0].Question, Answers: []string{"SQLite"}}}, nil
	}}

	args := `{"questions":[{"question":"Which database?","options":["SQLite","Postgres"],"multi":false}]}`
	out, err := at.Execute(ctx, args)
	if err != nil {
		t.Fatalf("ask_user failed: %v", err)
	}

	if len(got) != 1 || got[0].Question != "Which database?" || len(got[0].Options) != 2 {
		t.Errorf("unexpected questions passed to handler: %+v", got)
	}
	if !strings.Contains(out, "SQLite") {
		t.Errorf("expected answer in tool output, got: %s", out)
	}
}

func TestAskUserToolSkipped(t *testing.T) {
	at := &AskUserTool{Ask: func(_ context.Context, _ []AskQuestion) ([]AskResult, error) {
		return nil, nil // user skipped
	}}
	out, err := at.Execute(context.Background(), `{"questions":[{"question":"Q?","options":["A","B"]}]}`)
	if err != nil {
		t.Fatalf("ask_user failed: %v", err)
	}
	if !strings.Contains(out, "skipped") {
		t.Errorf("expected skipped notice, got: %s", out)
	}
}

func TestAskUserToolRequiresHandler(t *testing.T) {
	at := &AskUserTool{}
	_, err := at.Execute(context.Background(), `{"questions":[{"question":"Q?","options":["A"]}]}`)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected handler-not-configured error, got: %v", err)
	}
}

func TestAskUserToolEmptyArgs(t *testing.T) {
	at := &AskUserTool{Ask: func(_ context.Context, _ []AskQuestion) ([]AskResult, error) {
		return nil, nil
	}}
	if _, err := at.Execute(context.Background(), `{"questions":[]}`); err == nil {
		t.Errorf("expected error for empty questions")
	}
}

func TestFetchURLTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><script>var x=1;</script><style>.a{color:red}</style></head><body><h1>Hello Docs</h1><p>Some   useful   text.</p></body></html>`))
	}))
	defer srv.Close()

	out, err := (&FetchURLTool{}).Execute(context.Background(), `{"url":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("fetch_url failed: %v", err)
	}
	if !strings.Contains(out, "Hello Docs") || !strings.Contains(out, "Some useful text") {
		t.Errorf("unexpected extracted text: %q", out)
	}
	if strings.Contains(out, "<script>") || strings.Contains(out, "<style>") {
		t.Errorf("HTML tags leaked into output: %q", out)
	}
}

func TestFetchURLToolRejectsNonHTTP(t *testing.T) {
	_, err := (&FetchURLTool{}).Execute(context.Background(), `{"url":"file:///etc/passwd"}`)
	if err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Errorf("expected scheme rejection, got %v", err)
	}
}

func TestGitToolReadOnly(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello"), 0o644)
	run("add", "a.txt")
	run("commit", "-qm", "initial")
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello world"), 0o644)

	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}

	out, err := (&GitTool{}).Execute(context.Background(), `{"action":"status"}`)
	if err != nil {
		t.Fatalf("git status failed: %v", err)
	}
	if !strings.Contains(out, "a.txt") {
		t.Errorf("expected modified file in status, got: %s", out)
	}

	out, err = (&GitTool{}).Execute(context.Background(), `{"action":"log","limit":5}`)
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	if !strings.Contains(out, "initial") {
		t.Errorf("expected commit message in log, got: %s", out)
	}

	if _, err := (&GitTool{}).Execute(context.Background(), `{"action":"push"}`); err == nil {
		t.Errorf("expected unknown-action error for push")
	}
}

func TestSnapshotAndUndo(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(tmp, []byte("original"), 0o644)

	if err := Snapshot(tmp); err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}
	_ = os.WriteFile(tmp, []byte("changed"), 0o644)

	msg, err := RestoreLastSnapshot()
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if !strings.Contains(msg, "f.txt") {
		t.Errorf("unexpected restore message: %s", msg)
	}
	data, _ := os.ReadFile(tmp)
	if string(data) != "original" {
		t.Errorf("expected restored content, got %q", data)
	}

	if n := CleanupStaleSnapshots(); n != 0 {
		t.Errorf("expected no stale snapshots after restore, cleaned %d", n)
	}
}

func TestWebSearchRequiresKey(t *testing.T) {
	t.Setenv("EXA_API_KEY", "")
	_, err := (&WebSearchTool{}).Execute(context.Background(), `{"query":"go channels"}`)
	if err == nil || !strings.Contains(err.Error(), "EXA_API_KEY") {
		t.Errorf("expected EXA_API_KEY error, got %v", err)
	}
}

func TestWebSearchEmptyQuery(t *testing.T) {
	t.Setenv("EXA_API_KEY", "test")
	if _, err := (&WebSearchTool{}).Execute(context.Background(), `{"query":"   "}`); err == nil {
		t.Errorf("expected error for empty query")
	}
}

func TestReviewChangesToolApproveAndRevert(t *testing.T) {
	t.Cleanup(func() { CleanupStaleSnapshots() })
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	file := filepath.Join(repo, "a.txt")
	_ = os.WriteFile(file, []byte("v1"), 0o644)
	run("add", "a.txt")
	run("commit", "-qm", "initial")

	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}

	// Snapshot then modify, so revert has something to restore.
	if err := Snapshot(file); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(file, []byte("v2 broken"), 0o644)

	// Approve path.
	approve := &ReviewChangesTool{Ask: func(_ context.Context, _ []AskQuestion) ([]AskResult, error) {
		return []AskResult{{Answers: []string{"✅ Looks good, continue"}}}, nil
	}}
	out, err := approve.Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("review approve failed: %v", err)
	}
	if !strings.Contains(out, "approved") {
		t.Errorf("expected approval message, got %s", out)
	}

	// Revert path: restore the snapshot.
	_ = os.WriteFile(file, []byte("v3 broken"), 0o644)
	revert := &ReviewChangesTool{Ask: func(_ context.Context, _ []AskQuestion) ([]AskResult, error) {
		return []AskResult{{Answers: []string{"↩️ Revert this turn's changes"}}}, nil
	}}
	out, err = revert.Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("review revert failed: %v", err)
	}
	if !strings.Contains(out, "rolled back") {
		t.Errorf("expected rollback message, got %s", out)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "v1" {
		t.Errorf("expected file restored to v1, got %q", data)
	}
}

func TestFileChangesRecordedOnTools(t *testing.T) {
	ResetChanges()
	defer ResetChanges()
	t.Cleanup(func() { CleanupStaleSnapshots() })
	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "a.go")

	// write_file on a NEW file → created.
	reg := NewRegistry()
	_, err := reg.Execute(context.Background(), "write_file", `{"path":"`+p+`","content":"v1"}`)
	if err != nil {
		t.Fatal(err)
	}
	ch := TakeChanges()
	if len(ch) != 1 || ch[0].Action != "created" || ch[0].Path != p {
		t.Fatalf("expected created change, got %+v", ch)
	}

	// write_file over an existing file → modified, with old content captured.
	_, err = reg.Execute(context.Background(), "write_file", `{"path":"`+p+`","content":"v2"}`)
	if err != nil {
		t.Fatal(err)
	}
	ch = TakeChanges()
	if len(ch) != 1 || ch[0].Action != "modified" || ch[0].Old != "v1" || ch[0].New != "v2" {
		t.Fatalf("expected modified change with old content, got %+v", ch)
	}

	// edit_file → modified.
	_, err = reg.Execute(context.Background(), "edit_file", `{"path":"`+p+`","target":"v2","replacement":"v3"}`)
	if err != nil {
		t.Fatal(err)
	}
	ch = TakeChanges()
	if len(ch) != 1 || ch[0].Action != "modified" {
		t.Fatalf("expected edit_file modified, got %+v", ch)
	}

	// delete_file → deleted, old content recorded.
	_, err = reg.Execute(context.Background(), "delete_file", `{"path":"`+p+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	ch = TakeChanges()
	if len(ch) != 1 || ch[0].Action != "deleted" || ch[0].Old != "v3" {
		t.Fatalf("expected deleted change with old content, got %+v", ch)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("file should be gone after delete_file, stat err=%v", err)
	}
}

func TestFileChangesMessageFormat(t *testing.T) {
	msg := FileChangesMessage([]FileChange{
		{Path: "src/new.ts", Action: "created", New: "line1\nline2\n"},
		{Path: "src/old.ts", Action: "deleted", Old: "gone\n"},
		{Path: "src/edit.ts", Action: "modified", Old: "a\n", New: "b\n"},
	})
	if !strings.HasPrefix(msg, "FILES:\n") {
		t.Errorf("message must start with FILES:, got %q", msg)
	}
	if !strings.Contains(msg, FileChangesSep) {
		t.Errorf("message must contain the compact/diff separator")
	}
	compact, diff, _ := strings.Cut(msg, FileChangesSep)
	if !strings.Contains(compact, "src/new.ts") || !strings.Contains(compact, "created") {
		t.Errorf("compact block missing file rows: %q", compact)
	}
	if !strings.Contains(diff, "+") || !strings.Contains(diff, "-") {
		t.Errorf("diff block missing +/- markers: %q", diff)
	}
}

func TestGateFileActionCreateAndDelete(t *testing.T) {
	ResetChanges()
	defer ResetChanges()
	tmpDir := t.TempDir()
	newPath := filepath.Join(tmpDir, "new.go") // does not exist → create gate
	existing := filepath.Join(tmpDir, "exists.go")
	if err := os.WriteFile(existing, []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()

	// Overwriting an existing file is NOT gated (normal edit work).
	approved, _, err := reg.GateAction(context.Background(), providerToolCall("write_file", `{"path":"`+existing+`","content":"v2"}`))
	if err != nil || !approved {
		t.Fatalf("overwrite must not be gated: approved=%v err=%v", approved, err)
	}

	// Creating a new file without a confirm handler (headless) proceeds.
	approved, _, err = reg.GateAction(context.Background(), providerToolCall("write_file", `{"path":"`+newPath+`","content":"v"}`))
	if err != nil || !approved {
		t.Fatalf("headless create must proceed: approved=%v err=%v", approved, err)
	}

	// With a handler: deny (discard) blocks the create.
	reg.SetFileActionHandler(func(_ context.Context, req FileActionRequest) (FileActionDecision, error) {
		if req.Kind != "create_file" {
			t.Errorf("expected create_file request, got %q", req.Kind)
		}
		return FileActionDecision{Allow: false}, nil
	})
	approved, reason, err := reg.GateAction(context.Background(), providerToolCall("write_file", `{"path":"`+newPath+`","content":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	if approved || !strings.Contains(reason, "discarded") {
		t.Fatalf("create must be discardable: approved=%v reason=%q", approved, reason)
	}

	// Always allow persists for the session.
	reg.SetFileActionHandler(func(_ context.Context, _ FileActionRequest) (FileActionDecision, error) {
		return FileActionDecision{Allow: true, Always: true}, nil
	})
	approved, _, err = reg.GateAction(context.Background(), providerToolCall("write_file", `{"path":"`+newPath+`","content":"v"}`))
	if err != nil || !approved {
		t.Fatalf("always-allow create must proceed: approved=%v err=%v", approved, err)
	}
	// Second call: handler must NOT be invoked again (always-allow remembered).
	calls := 0
	reg.SetFileActionHandler(func(_ context.Context, _ FileActionRequest) (FileActionDecision, error) {
		calls++
		return FileActionDecision{Allow: false}, nil
	})
	approved, _, err = reg.GateAction(context.Background(), providerToolCall("write_file", `{"path":"`+newPath+`","content":"v"}`))
	if err != nil || !approved {
		t.Fatalf("always-allow must be remembered: approved=%v err=%v", approved, err)
	}
	if calls != 0 {
		t.Fatalf("always-allow path must skip the handler, got %d calls", calls)
	}

	// delete_file is always gated.
	approved, reason, err = reg.GateAction(context.Background(), providerToolCall("delete_file", `{"path":"`+existing+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if approved || !strings.Contains(reason, "discarded") {
		t.Fatalf("delete must be gated: approved=%v reason=%q", approved, reason)
	}
}

func TestSubAgentDeniesFileActions(t *testing.T) {
	reg := NewRegistry()
	sub := reg.SubRegistry()

	approved, reason, err := sub.GateAction(context.Background(), providerToolCall("delete_file", `{"path":"/tmp/x.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if approved || !strings.Contains(reason, "sub-agents cannot") {
		t.Fatalf("sub-agent delete must be denied: approved=%v reason=%q", approved, reason)
	}
}

func TestGitToolCommitGated(t *testing.T) {
	r := NewRegistry()

	// Read-only actions always pass.
	approved, _, err := r.GateAction(context.Background(), providerToolCall("git", `{"action":"status"}`))
	if err != nil || !approved {
		t.Fatalf("git status must not be gated: approved=%v err=%v", approved, err)
	}

	// commit is gated: with a deny handler it is blocked.
	r.SetUserAskHandler(func(_ context.Context, _ []AskQuestion) ([]AskResult, error) {
		return []AskResult{{Answers: []string{"🚫 Deny"}}}, nil
	})
	approved, reason, err := r.GateAction(context.Background(), providerToolCall("git", `{"action":"commit","message":"fix"}`))
	if err != nil {
		t.Fatal(err)
	}
	if approved || !strings.Contains(reason, "denied") {
		t.Fatalf("git commit must be deniable via the gate: approved=%v reason=%q", approved, reason)
	}

	// With an allow handler it proceeds.
	r.SetUserAskHandler(func(_ context.Context, _ []AskQuestion) ([]AskResult, error) {
		return []AskResult{{Answers: []string{"✅ Allow once"}}}, nil
	})
	approved, _, err = r.GateAction(context.Background(), providerToolCall("git", `{"action":"commit","message":"fix"}`))
	if err != nil || !approved {
		t.Fatalf("git commit must be allowable: approved=%v err=%v", approved, err)
	}
}
