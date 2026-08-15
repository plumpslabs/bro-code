package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
)

func mkEvent(seq int, etype, payload string) store.Event {
	return store.Event{
		ID:          int64(seq),
		SessionID:   "sess_test",
		Seq:         seq,
		Type:        etype,
		PayloadJSON: payload,
		CreatedAt:   time.Date(2026, 8, 15, 10, 0, seq, 0, time.UTC),
	}
}

func msgPayload(r string, content string, tc []provider.ToolCall) string {
	b, _ := json.Marshal(provider.Message{Role: r, Content: content, ToolCalls: tc, Mode: "BUILDER", Model: "m1"})
	return string(b)
}

func TestRenderReplay(t *testing.T) {
	events := []store.Event{
		mkEvent(1, "user_msg", msgPayload("user", "perbaiki bug login", nil)),
		mkEvent(2, "assistant_msg", msgPayload("assistant", "saya cek dulu", []provider.ToolCall{
			{Name: "bash", Arguments: `{"command":"go test ./..."}`},
		})),
		mkEvent(3, "tool_result", msgPayload("user", "exit status 1", nil)),
		mkEvent(4, "compaction_summary", msgPayload("user", "dirangkum", nil)),
	}
	sessions := []store.Session{{
		ID: "sess_test", ProjectPath: "/tmp/proj", Status: "active",
		CreatedAt: time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC),
	}}

	out := renderReplay("sess_test", events, sessions)

	for _, want := range []string{
		"=== Replay: sess_test ===",
		"project: /tmp/proj",
		"[#1] user_msg",
		"perbaiki bug login",
		"[#2] assistant_msg",
		"[BUILDER/m1]",
		"→ bash(",
		"[#3] tool_result",
		"exit status 1",
		"[#4] compaction_summary",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("replay output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestRenderReplayUnparseablePayload(t *testing.T) {
	out := renderReplay("sess_test", []store.Event{
		mkEvent(1, "user_msg", "{not-json"),
	}, nil)
	if !strings.Contains(out, "<unparseable payload>") {
		t.Errorf("expected unparseable payload marker, got:\n%s", out)
	}
}

func TestSingleLine(t *testing.T) {
	got := singleLine("a\nb\tc", 50)
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("singleLine left raw whitespace: %q", got)
	}
	got = singleLine("0123456789", 5)
	if got != "01234…" {
		t.Errorf("singleLine truncation = %q, want 01234…", got)
	}
}
