package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAskBlocks(t *testing.T) {
	text := "Analysis first.\n\n" +
		"[Q]Which database?[/Q]\n[O]SQLite[/O]\n[O]PostgreSQL[/O]\n\n" +
		"[Q]Deploy now?[/Q]\n[O]Yes[/O]\n[O]No[/O]\n[M]true[/M]"

	questions, cleaned := ParseAskBlocks(text)
	if len(questions) != 2 {
		t.Fatalf("expected 2 questions, got %d: %+v", len(questions), questions)
	}

	q0 := questions[0]
	if q0.Question != "Which database?" {
		t.Errorf("expected question 1 text, got %q", q0.Question)
	}
	if len(q0.Options) != 2 || q0.Options[0] != "SQLite" || q0.Options[1] != "PostgreSQL" {
		t.Errorf("unexpected options: %v", q0.Options)
	}
	if q0.Multi {
		t.Error("question 1 should be single-select")
	}

	q1 := questions[1]
	if q1.Question != "Deploy now?" {
		t.Errorf("expected question 2 text, got %q", q1.Question)
	}
	if !q1.Multi {
		t.Error("question 2 should be multi-select ([M]true[/M])")
	}

	// The surrounding analysis survives; all marker blocks are stripped.
	if !strings.Contains(cleaned, "Analysis first.") {
		t.Errorf("expected analysis preserved in cleaned text, got %q", cleaned)
	}
	for _, banned := range []string{"[Q]", "[/Q]", "[O]", "[/O]", "[M]", "[/M]"} {
		if strings.Contains(cleaned, banned) {
			t.Errorf("marker %s leaked into cleaned text: %q", banned, cleaned)
		}
	}
}

func TestParseAskBlocksANSIAndFences(t *testing.T) {
	// Colored CLI output wrapping the block in a code fence must still parse,
	// and the emptied fence must be cleaned up.
	text := "\x1b[0mHere is my take.\x1b[0m\n\n```text\n" +
		"[Q]Pick one[/Q]\n[O]A[/O]\n[O]B[/O]\n```"

	questions, cleaned := ParseAskBlocks(text)
	if len(questions) != 1 {
		t.Fatalf("expected 1 question, got %d", len(questions))
	}
	if questions[0].Question != "Pick one" {
		t.Errorf("unexpected question: %q", questions[0].Question)
	}
	if len(questions[0].Options) != 2 {
		t.Errorf("expected 2 options, got %v", questions[0].Options)
	}
	if strings.Contains(cleaned, "```") {
		t.Errorf("emptied code fence not cleaned: %q", cleaned)
	}
	if !strings.Contains(cleaned, "Here is my take.") {
		t.Errorf("analysis lost after fence cleanup: %q", cleaned)
	}
}

func TestParseAskBlocksNoMarkers(t *testing.T) {
	text := "Just a normal answer with no questions."
	questions, cleaned := ParseAskBlocks(text)
	if questions != nil {
		t.Errorf("expected nil questions, got %+v", questions)
	}
	if cleaned != text {
		t.Errorf("expected unchanged text, got %q", cleaned)
	}
}

func TestOpenCodeCLIAskFlow(t *testing.T) {
	tmp := t.TempDir()
	fakeCLI := filepath.Join(tmp, "opencode")
	script := "#!/bin/sh\n" +
		"if echo \"$*\" | grep -q \"clarification questions\"; then\n" +
		"  echo 'FINAL ANSWER with Postgres'\n" +
		"else\n" +
		"  echo 'Here is my analysis.'\n" +
		"  echo '[Q]Which database?[/Q]'\n" +
		"  echo '[O]SQLite[/O]'\n" +
		"  echo '[O]PostgreSQL[/O]'\n" +
		"fi\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	a := &OpenCodeAdapter{cliPath: fakeCLI, http: NewOpenAIAdapter("http://127.0.0.1:1/v1", "")}
	var asked []AskQuestion
	a.AskUser = func(_ context.Context, qs []AskQuestion) ([]AskResult, error) {
		asked = qs
		return []AskResult{{Question: qs[0].Question, Answers: []string{"PostgreSQL"}}}, nil
	}

	res, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "hy3-free",
		Messages: []Message{{Role: "user", Content: "which db?"}},
	}, nil)
	if err != nil {
		t.Fatalf("CompleteWithProgress failed: %v", err)
	}

	if len(asked) != 1 {
		t.Fatalf("expected AskUser to be called once with 1 question, got %d: %+v", len(asked), asked)
	}
	if asked[0].Question != "Which database?" {
		t.Errorf("unexpected question presented: %q", asked[0].Question)
	}
	if len(asked[0].Options) != 2 || asked[0].Options[1] != "PostgreSQL" {
		t.Errorf("unexpected options presented: %v", asked[0].Options)
	}

	if !strings.Contains(res.Content, "FINAL ANSWER with Postgres") {
		t.Errorf("expected the continuation answer, got %q", res.Content)
	}
	if strings.Contains(res.Content, "[Q]") || strings.Contains(res.Content, "[O]") {
		t.Errorf("question markers leaked into the final answer: %q", res.Content)
	}
}

func TestOpenCodeCLIAskSkipped(t *testing.T) {
	tmp := t.TempDir()
	fakeCLI := filepath.Join(tmp, "opencode")
	script := "#!/bin/sh\n" +
		"echo 'Here is my analysis.'\n" +
		"echo '[Q]Which database?[/Q]'\n" +
		"echo '[O]SQLite[/O]'\n" +
		"echo '[O]PostgreSQL[/O]'\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	a := &OpenCodeAdapter{cliPath: fakeCLI, http: NewOpenAIAdapter("http://127.0.0.1:1/v1", "")}
	a.AskUser = func(_ context.Context, _ []AskQuestion) ([]AskResult, error) {
		return nil, nil // user skipped
	}

	res, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "hy3-free",
		Messages: []Message{{Role: "user", Content: "which db?"}},
	}, nil)
	if err != nil {
		t.Fatalf("CompleteWithProgress failed: %v", err)
	}
	// The analysis text survives; the question markers are stripped (the
	// questions were already shown in the modal the user dismissed).
	if !strings.Contains(res.Content, "Here is my analysis.") {
		t.Errorf("expected analysis preserved, got %q", res.Content)
	}
	if strings.Contains(res.Content, "[Q]") {
		t.Errorf("question markers should be stripped after skip: %q", res.Content)
	}
}

func TestOpenCodeCLIAskWithoutHandler(t *testing.T) {
	tmp := t.TempDir()
	fakeCLI := filepath.Join(tmp, "opencode")
	script := "#!/bin/sh\n" +
		"echo 'Here is my analysis.'\n" +
		"echo '[Q]Which database?[/Q]'\n" +
		"echo '[O]SQLite[/O]'\n" +
		"echo '[O]PostgreSQL[/O]'\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	// No AskUser handler (headless): the raw answer — including the question
	// text — is returned as-is so the user can still read and answer it.
	a := &OpenCodeAdapter{cliPath: fakeCLI, http: NewOpenAIAdapter("http://127.0.0.1:1/v1", "")}
	res, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "hy3-free",
		Messages: []Message{{Role: "user", Content: "which db?"}},
	}, nil)
	if err != nil {
		t.Fatalf("CompleteWithProgress failed: %v", err)
	}
	if !strings.Contains(res.Content, "Which database?") {
		t.Errorf("expected question text in raw answer, got %q", res.Content)
	}
}

func TestOpenCodeCLIIdentityPreamble(t *testing.T) {
	tmp := t.TempDir()
	fakeCLI := filepath.Join(tmp, "opencode")
	// Dump the received args into a file so the test can assert the identity
	// preamble was actually sent to the CLI model.
	outFile := filepath.Join(tmp, "args.txt")
	script := "#!/bin/sh\n" +
		"echo \"$*\" > " + outFile + "\n" +
		"echo 'I am BroCode, your coding agent.'\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	a := &OpenCodeAdapter{cliPath: fakeCLI, http: NewOpenAIAdapter("http://127.0.0.1:1/v1", "")}
	res, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "hy3-free",
		Messages: []Message{{Role: "user", Content: "who are you?"}},
	}, nil)
	if err != nil {
		t.Fatalf("CompleteWithProgress failed: %v", err)
	}

	args, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if !strings.Contains(string(args), "You are BroCode") {
		t.Errorf("identity preamble not sent to CLI: %q", string(args))
	}
	if strings.Contains(string(args), "You are NOT") {
		t.Errorf("identity assertion inverted? got: %q", string(args))
	}
	if !strings.Contains(res.Content, "I am BroCode") {
		t.Errorf("expected fake CLI answer, got %q", res.Content)
	}
}

func TestOpenCodeCLIMCPStatusInjected(t *testing.T) {
	tmp := t.TempDir()
	fakeCLI := filepath.Join(tmp, "opencode")
	outFile := filepath.Join(tmp, "args.txt")
	script := "#!/bin/sh\n" +
		"echo \"$*\" > " + outFile + "\n" +
		"echo 'MCP servers: git, filesystem'\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	a := &OpenCodeAdapter{cliPath: fakeCLI, http: NewOpenAIAdapter("http://127.0.0.1:1/v1", "")}
	a.MCPStatus = "Connected MCP servers: git, filesystem"
	res, err := a.CompleteWithProgress(context.Background(), CompletionRequest{
		Model:    "hy3-free",
		Messages: []Message{{Role: "user", Content: "what MCP is available?"}},
	}, nil)
	if err != nil {
		t.Fatalf("CompleteWithProgress failed: %v", err)
	}

	args, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if !strings.Contains(string(args), "Connected MCP servers: git, filesystem") {
		t.Errorf("MCP status not injected into CLI prompt: %q", string(args))
	}
	if strings.Contains(res.Content, "Connected MCP servers:") {
		t.Errorf("MCP status leaked into the model's answer: %q", res.Content)
	}
}

func TestFormatAskResults(t *testing.T) {
	out := formatAskResults([]AskResult{
		{Question: "Which DB?", Answers: []string{"PostgreSQL"}},
		{Question: "Multi?", Answers: []string{"A", "B"}, Custom: "C"},
	})
	if !strings.Contains(out, "Which DB?") || !strings.Contains(out, "PostgreSQL") {
		t.Errorf("unexpected formatted results: %q", out)
	}
	if !strings.Contains(out, "A; B") {
		t.Errorf("expected joined multi answers, got %q", out)
	}
}
