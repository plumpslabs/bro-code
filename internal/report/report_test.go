package report

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(t.TempDir() + "/brocode_test.db")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st
}

func mustAppend(t *testing.T, st *store.Store, sessionID, typ, payload string, tokens int) {
	t.Helper()
	if _, err := st.AppendEvent(sessionID, typ, payload, tokens); err != nil {
		t.Fatalf("AppendEvent %s: %v", typ, err)
	}
}

func TestBuildSessionReport(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateSession("s1", "/tmp/proj"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	// A user prompt + an assistant turn that edits a file = productive.
	mustAppend(t, st, "s1", "user_msg", "please fix the bug", 50)

	editTurn := provider.Message{
		Role:    "assistant",
		Content: "",
		Model:   "hy3-free",
		ToolCalls: []provider.ToolCall{
			{ID: "1", Name: "edit_file", Arguments: `{"path":"a.go"}`},
		},
	}
	eb, _ := json.Marshal(editTurn)
	mustAppend(t, st, "s1", "assistant_msg", string(eb), 100)

	// A failed tool result.
	mustAppend(t, st, "s1", "tool_result", "error: file not found", 30)

	// A final answering turn with no tool calls.
	answer := provider.Message{Role: "assistant", Content: "fixed in a.go", Model: "hy3-free"}
	ab, _ := json.Marshal(answer)
	mustAppend(t, st, "s1", "assistant_msg", string(ab), 40)

	mustAppend(t, st, "s1", "compaction_summary", "summary", 0)

	r, err := Build(st, "s1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if r.UserMsgs != 1 {
		t.Errorf("UserMsgs = %d, want 1", r.UserMsgs)
	}
	if r.AssistantTurns != 2 {
		t.Errorf("AssistantTurns = %d, want 2", r.AssistantTurns)
	}
	if r.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", r.ToolCalls)
	}
	if r.ToolResults != 1 || r.ToolFailures != 1 {
		t.Errorf("ToolResults=%d ToolFailures=%d, want 1/1", r.ToolResults, r.ToolFailures)
	}
	if r.Compactions != 1 {
		t.Errorf("Compactions = %d, want 1", r.Compactions)
	}
	if len(r.Models) != 1 || r.Models[0] != "hy3-free" {
		t.Errorf("Models = %v, want [hy3-free]", r.Models)
	}
	// Both turns are productive (edit_file + final answer) → 100%.
	if r.ProductivePct != 100 {
		t.Errorf("ProductivePct = %d, want 100", r.ProductivePct)
	}
	// High tool failure rate (1/1 > 0.3) should be flagged.
	if !containsAnomaly(r.Anomalies, "high tool failure rate") {
		t.Errorf("expected high tool failure rate anomaly, got %v", r.Anomalies)
	}

	// JSON round-trips.
	j, err := r.RenderJSON()
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var round SessionReport
	if err := json.Unmarshal([]byte(j), &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.SessionID != "s1" {
		t.Errorf("round.SessionID = %s", round.SessionID)
	}
}

func TestBuildMissingSession(t *testing.T) {
	st := newTestStore(t)
	if _, err := Build(st, "nope"); err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestSummarizeMergesReports(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"a", "b"} {
		if err := st.CreateSession(id, "/p"); err != nil {
			t.Fatal(err)
		}
		mustAppend(t, st, id, "assistant_msg", `{"role":"assistant","model":"hy3-free","content":"x"}`, 10)
	}
	reports, err := BuildAll(st, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	agg := Summarize(reports)
	if agg.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", agg.SessionCount)
	}
	if agg.TotalTurns != 2 {
		t.Errorf("TotalTurns = %d, want 2", agg.TotalTurns)
	}
	if _, err := agg.RenderJSON(); err != nil {
		t.Fatalf("AggregateReport.RenderJSON: %v", err)
	}
}

func containsAnomaly(list []string, sub string) bool {
	for _, s := range list {
		if len(s) >= len(sub) && s[:len(sub)] == sub {
			return true
		}
	}
	return false
}
