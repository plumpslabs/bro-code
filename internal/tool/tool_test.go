package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/memory"
)

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

func TestCodeSymbolsTool(t *testing.T) {
	tmpDir := t.TempDir()
	f := filepath.Join(tmpDir, "svc.js")
	content := `class UserService {
  async findAll() { return []; }
  async findByEmail(email) { return null; }
}
function helper() {}
`
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	out, err := reg.Execute(context.Background(), "code_symbols", `{"paths":["`+f+`"]}`)
	if err != nil {
		t.Fatalf("code_symbols: %v", err)
	}
	if !strings.Contains(out, "UserService") || !strings.Contains(out, "findAll") || !strings.Contains(out, "helper") {
		t.Errorf("code_symbols output missing symbols: %q", out)
	}
	if !strings.Contains(out, "[class]") && !strings.Contains(out, "class") {
		t.Errorf("code_symbols should show kind: %q", out)
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
