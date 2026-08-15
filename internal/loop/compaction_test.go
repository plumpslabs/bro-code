package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/provider"
)

func TestParseCompactionJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "plain json", raw: `{"goal":"g","files_touched":["a.go"],"decisions_made":["d"],"open_questions":[],"last_known_state":"s"}`, want: true},
		{name: "fenced json", raw: "```json\n{\"goal\":\"g\",\"files_touched\":[],\"decisions_made\":[],\"open_questions\":[],\"last_known_state\":\"s\"}\n```", want: true},
		{name: "prose around", raw: "Here you go:\n{\"goal\":\"g\",\"files_touched\":[],\"decisions_made\":[],\"open_questions\":[],\"last_known_state\":\"s\"}\nThat's all.", want: true},
		{name: "no brace", raw: "no json here", want: false},
		{name: "invalid json", raw: `{"goal": }`, want: false},
		{name: "empty object", raw: `{}`, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, ok := parseCompactionJSON(tt.raw)
			if ok != tt.want {
				t.Fatalf("parseCompactionJSON(%q) ok=%v want %v", tt.raw, ok, tt.want)
			}
			if tt.want && strings.TrimSpace(summary.Goal) == "" && len(summary.FilesTouched) == 0 && strings.TrimSpace(summary.LastKnownState) == "" && tt.raw != "{}" {
				t.Fatalf("expected parsed goal to be non-empty for %q", tt.raw)
			}
		})
	}
}

func TestParseCompactionJSONRoundTrip(t *testing.T) {
	raw := "```json\n" +
		`{"goal":"Fix the filter","files_touched":["internal/x/filter.go","internal/x/filter_test.go"],"decisions_made":["Rewrite the query builder","Drop the unused param"],"open_questions":["Verify on staging"],"last_known_state":"Core rewrite done; tests green"}` +
		"\n```"
	s, ok := parseCompactionJSON(raw)
	if !ok {
		t.Fatal("expected parse to succeed")
	}
	if s.Goal != "Fix the filter" {
		t.Errorf("goal = %q", s.Goal)
	}
	if len(s.FilesTouched) != 2 || s.FilesTouched[0] != "internal/x/filter.go" {
		t.Errorf("files_touched = %v", s.FilesTouched)
	}
	if len(s.DecisionsMade) != 2 || s.OpenQuestions[0] != "Verify on staging" {
		t.Errorf("decisions/open_questions mismatch: %v %v", s.DecisionsMade, s.OpenQuestions)
	}
	if s.LastKnownState != "Core rewrite done; tests green" {
		t.Errorf("last_known_state = %q", s.LastKnownState)
	}
}

func TestCompactionTranscriptTruncates(t *testing.T) {
	big := strings.Repeat("x", 100000)
	msgs := []provider.Message{
		{Role: "user", Content: "start"},
		{Role: "assistant", Content: big},
	}
	tr := compactionTranscript(msgs)
	if len(tr) > 70000 {
		t.Fatalf("transcript not truncated: %d chars", len(tr))
	}
	if !strings.Contains(tr, "truncated") {
		t.Error("expected truncation marker in transcript")
	}
}

// scriptedSummaryAdapter returns a canned JSON compaction summary.
type scriptedSummaryAdapter struct {
	respond string
	err     error
}

func (s *scriptedSummaryAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.respond != "" {
		return &provider.CompletionResponse{Content: s.respond}, nil
	}
	return &provider.CompletionResponse{Content: "{\"goal\":\"g\",\"files_touched\":[],\"decisions_made\":[],\"open_questions\":[],\"last_known_state\":\"s\"}"}, nil
}

func TestModelCompactionSummarySuccess(t *testing.T) {
	adapter := &scriptedSummaryAdapter{respond: "{\"goal\":\"Rewrite auth middleware\",\"files_touched\":[\"internal/auth/mw.go\"],\"decisions_made\":[\"Use context-scoped claims\"],\"open_questions\":[],\"last_known_state\":\"MW rewritten, tests pending\"}"}
	e, ctxMgr := newRouterEngine(adapter)
	e.SetPrimaryIdentity("primary", "openai-compatible")

	if err := ctxMgr.AppendUserMessage("help me fix auth"); err != nil {
		t.Fatal(err)
	}
	if err := ctxMgr.AppendAssistantTurn("BUILDER", "m", "thinking", "working", nil); err != nil {
		t.Fatal(err)
	}
	if err := ctxMgr.AppendToolResult("tc1", "grep output"); err != nil {
		t.Fatal(err)
	}

	s, ok := e.modelCompactionSummary(context.Background())
	if !ok {
		t.Fatal("expected model summary to succeed")
	}
	if s.Goal != "Rewrite auth middleware" {
		t.Errorf("goal = %q", s.Goal)
	}
	if len(s.FilesTouched) != 1 || s.FilesTouched[0] != "internal/auth/mw.go" {
		t.Errorf("files = %v", s.FilesTouched)
	}
}

func TestModelCompactionSummaryFallsBackOnError(t *testing.T) {
	adapter := &scriptedSummaryAdapter{err: context.DeadlineExceeded}
	e, ctxMgr := newRouterEngine(adapter)
	e.SetPrimaryIdentity("primary", "openai-compatible")
	_ = ctxMgr.AppendUserMessage("hello")

	if _, ok := e.modelCompactionSummary(context.Background()); ok {
		t.Fatal("expected failure path to report ok=false")
	}
}

func TestModelCompactionSummaryFallsBackOnGarbage(t *testing.T) {
	adapter := &scriptedSummaryAdapter{respond: "sorry, I cannot summarize"}
	e, ctxMgr := newRouterEngine(adapter)
	e.SetPrimaryIdentity("primary", "openai-compatible")
	_ = ctxMgr.AppendUserMessage("hello")

	if _, ok := e.modelCompactionSummary(context.Background()); ok {
		t.Fatal("expected non-JSON response to report ok=false")
	}
}

func TestModelCompactionSummaryEmptyContext(t *testing.T) {
	adapter := &scriptedSummaryAdapter{}
	e, _ := newRouterEngine(adapter)
	e.SetPrimaryIdentity("primary", "openai-compatible")

	if _, ok := e.modelCompactionSummary(context.Background()); ok {
		t.Fatal("expected empty context to report ok=false")
	}
}
