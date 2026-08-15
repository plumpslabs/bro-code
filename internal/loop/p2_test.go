package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/provider"
)

// recordingAdapter records the system message content of every completion so
// tests can assert the system prompt prefix is byte-identical across loop
// iterations (the provider prompt-caching guarantee).
type recordingAdapter struct {
	scriptedAdapter
	sysMsgs []string
}

func (r *recordingAdapter) Complete(ctx context.Context, req provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Messages) > 0 {
		r.sysMsgs = append(r.sysMsgs, req.Messages[0].Content)
	}
	return r.scriptedAdapter.Complete(ctx, req)
}

func TestBashFamily(t *testing.T) {
	cases := []struct {
		args string
		want string
	}{
		{`{"command":"git status"}`, "git"},
		{`{"command":"go test ./..."}`, "go"},
		{`{"command":"  npm run build  "}`, "npm"},
		{`{"command":"grep -rn foo ."}`, "grep"},
		{`{"command":""}`, ""},
		{`{}`, ""},
		{`{"command":"Go Test ./x"}`, "go"}, // case-insensitive leading word
	}
	for _, c := range cases {
		if got := bashFamily(c.args); got != c.want {
			t.Errorf("bashFamily(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestAdvanceBashFamily(t *testing.T) {
	cases := []struct {
		name            string
		fam, last       string
		streak, wantStk int
		wantFam         string
	}{
		{"first round", "grep", "", 0, 1, "grep"},
		{"same family increments", "grep", "grep", 1, 2, "grep"},
		{"family switch resets", "git", "grep", 3, 1, "git"},
		{"empty family resets", "", "grep", 3, 1, ""},
	}
	for _, c := range cases {
		fam, stk := advanceBashFamily(c.fam, c.last, c.streak)
		if fam != c.wantFam || stk != c.wantStk {
			t.Errorf("%s: advanceBashFamily(%q,%q,%d) = (%q,%d), want (%q,%d)",
				c.name, c.fam, c.last, c.streak, fam, stk, c.wantFam, c.wantStk)
		}
	}
}

// TestPromptCacheStableAcrossIterations proves the P2a win: the system prompt
// is built once per turn and every later loop iteration re-sends byte-identical
// leading tokens, so provider prompt caching actually hits.
func TestPromptCacheStableAcrossIterations(t *testing.T) {
	adapter := &recordingAdapter{scriptedAdapter: scriptedAdapter{responses: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{bashCall("ls /nope_1")}},
		{ToolCalls: []provider.ToolCall{bashCall("ls /nope_2")}},
		{Content: "done"},
	}}}
	e, _ := newEngineWith(adapter)

	if _, err := e.RunTurn(context.Background(), "apa isi project ini", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if e.sysPromptCached == "" {
		t.Fatal("sysPromptCached not populated after turn")
	}
	if !strings.Contains(e.sysPromptCached, "ACTIVE ENGINE MODE") {
		t.Fatal("cached system prompt missing mode block")
	}
	// Every main-loop completion in the turn must carry the SAME system prompt.
	if len(adapter.sysMsgs) < 3 {
		t.Fatalf("expected >=3 completions, got %d", len(adapter.sysMsgs))
	}
	for i, m := range adapter.sysMsgs {
		if m != adapter.sysMsgs[0] {
			t.Errorf("completion %d used a different system prompt (prompt caching defeated)", i)
		}
	}
}

// TestFuzzyLoopBreakBlocksSameFamilyBash proves the P2b guard: round after
// round of the same bash command family with DIFFERENT arguments (so exact
// repeat detection never fires) is eventually blocked with a strategy change
// instruction, and the turn still completes when the model answers.
func TestFuzzyLoopBreakBlocksSameFamilyBash(t *testing.T) {
	var responses []provider.CompletionResponse
	for i := 0; i < 6; i++ {
		responses = append(responses, provider.CompletionResponse{
			ToolCalls: []provider.ToolCall{bashCall("ls /nope_" + string(rune('a'+i)))},
		})
	}
	responses = append(responses, provider.CompletionResponse{Content: "selesai"})
	adapter := &scriptedAdapter{responses: responses}
	e, ctxMgr := newEngineWith(adapter)

	ans, err := e.RunTurn(context.Background(), "perbaiki sesuatu", nil)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !strings.Contains(ans, "selesai") {
		t.Fatalf("expected final answer, got %q", ans)
	}
	if !strings.Contains(contextText(ctxMgr), "LOOP GUARD") {
		t.Fatal("fuzzy loop guard was not triggered for 6 same-family rounds")
	}
	if e.bashFamilyStreak != 6 {
		t.Fatalf("bashFamilyStreak = %d, want 6", e.bashFamilyStreak)
	}
}

// TestFuzzyLoopBreakMixedFamiliesResets proves a round that mixes different
// command families is NOT counted as spinning.
func TestFuzzyLoopBreakMixedFamiliesResets(t *testing.T) {
	adapter := &scriptedAdapter{responses: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{bashCall("go test ./a"), bashCall("git status")}},
		{ToolCalls: []provider.ToolCall{bashCall("go test ./b")}},
		{Content: "done"},
	}}
	e, ctxMgr := newEngineWith(adapter)

	if _, err := e.RunTurn(context.Background(), "cek", nil); err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if strings.Contains(contextText(ctxMgr), "LOOP GUARD") {
		t.Fatal("mixed-family rounds should not trigger the fuzzy guard")
	}
	// Round 1 mixed (go+git) reset the streak; round 2 started fresh at 1.
	if e.bashFamilyStreak != 1 {
		t.Fatalf("bashFamilyStreak = %d, want 1 (mixed round reset)", e.bashFamilyStreak)
	}
}

// TestToolsForModeStructuralPruning proves P3a: read-only modes simply do not
// receive mutating tools, and PLANNER additionally loses bash. The model can
// never propose what it cannot see.
func TestToolsForModeStructuralPruning(t *testing.T) {
	e, _ := newEngineWith(&scriptedAdapter{})

	names := func(defs []provider.ToolDefinition) map[string]bool {
		m := map[string]bool{}
		for _, d := range defs {
			m[d.Name] = true
		}
		return m
	}

	builder := names(e.toolsForMode("BUILDER"))
	for _, mut := range []string{"write_file", "edit_file", "delete_file", "bash"} {
		if !builder[mut] {
			t.Errorf("BUILDER must expose %q, it was pruned", mut)
		}
	}

	miner := names(e.toolsForMode("MINER"))
	for _, mut := range []string{"write_file", "edit_file", "delete_file"} {
		if miner[mut] {
			t.Errorf("MINER must not expose %q", mut)
		}
	}
	if !miner["bash"] {
		t.Error("MINER keeps read-only bash (git log/status) — it was pruned")
	}
	if !miner["read_file"] {
		t.Error("MINER must keep read_file")
	}

	planner := names(e.toolsForMode("PLANNER"))
	for _, mut := range []string{"write_file", "edit_file", "delete_file", "bash"} {
		if planner[mut] {
			t.Errorf("PLANNER must not expose %q", mut)
		}
	}
	if !planner["read_file"] {
		t.Error("PLANNER must keep read_file")
	}
}
