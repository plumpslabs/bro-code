package provider

import (
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
