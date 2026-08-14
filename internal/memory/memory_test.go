package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
)

func TestRetainAndList(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if s == nil {
		t.Fatal("NewStore returned nil")
	}

	ok, err := s.Retain("Architecture", "Payment flow: service -> repository -> Prisma")
	if err != nil || !ok {
		t.Fatalf("first retain: ok=%v err=%v", ok, err)
	}
	ok, err = s.Retain("Build & Test", "bun test di crm_sales_backend")
	if err != nil || !ok {
		t.Fatalf("second retain: ok=%v err=%v", ok, err)
	}

	// File must exist.
	if _, err := os.Stat(s.Path()); err != nil {
		t.Fatalf("memory file not written: %v", err)
	}

	// Duplicate (exact) must be rejected.
	ok, err = s.Retain("Architecture", "Payment flow: service -> repository -> Prisma")
	if err != nil || ok {
		t.Fatalf("duplicate retain should be rejected: ok=%v err=%v", ok, err)
	}

	list := s.List()
	if !strings.Contains(list, "Architecture") || !strings.Contains(list, "Payment flow") {
		t.Errorf("list missing retained facts:\n%s", list)
	}
	if !strings.Contains(list, "Build & Test") || !strings.Contains(list, "bun test") {
		t.Errorf("list missing build facts:\n%s", list)
	}
}

func TestWarmStartRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	s.Retain("Decisions", "Filter omnichannel: Semua abaikan status + PIC")
	s.Retain("Gotchas", "jangan pake npx tsc global, pake lokal")

	// Fresh store reading the same file must see the facts (warm start).
	s2 := NewStore(dir)
	ws := s2.WarmStart()
	if !strings.Contains(ws, "Filter omnichannel") {
		t.Errorf("warm start missing decisions:\n%s", ws)
	}
	if !strings.Contains(ws, "jangan pake npx tsc") {
		t.Errorf("warm start missing gotchas:\n%s", ws)
	}
}

func TestRecallRelevance(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	s.Retain("Build & Test", "bun test di crm_sales_backend untuk backend")
	s.Retain("Architecture", "frontend pakai React + Vite + Tailwind")

	out := s.Recall("bagaimana cara test backend", 5)
	if !strings.Contains(out, "bun test") {
		t.Errorf("recall should rank build facts first:\n%s", out)
	}
	if !strings.Contains(out, "PROJECT MEMORY MATCHES") {
		t.Errorf("recall missing header:\n%s", out)
	}
}

func TestRecallEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	out := s.Recall("anything", 5)
	if !strings.Contains(out, "empty") && !strings.Contains(out, "No relevant") {
		t.Errorf("empty recall should say so, got:\n%s", out)
	}
}

func TestMergeCompaction(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	err := s.MergeCompaction("Fix omnichannel filter", []string{"Semua abaikan status"}, "Filter rewritten")
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	list := s.List()
	if !strings.Contains(list, "Semua abaikan status") {
		t.Errorf("decision not merged:\n%s", list)
	}
	// Placeholder decisions must never land in memory.
	s2 := NewStore(dir)
	s2.Retain("Decisions", "Compacted older context turns to preserve memory window")
	s2.MergeCompaction("Continue active conversation", []string{"Compacted older context turns to preserve memory window"}, "Context compacted successfully")
	list2 := s2.List()
	if strings.Contains(list2, "preserve memory window") {
		t.Errorf("placeholder decision leaked into memory:\n%s", list2)
	}
}

func TestAntiFeedbackLoop(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	ok, _ := s.Retain("Notes", "PROJECT MEMORY MATCHES:\n- some fake recalled block")
	if ok {
		t.Error("memory block must not be retained (feedback loop)")
	}
}

func eventMsg(evType string, msg provider.Message) store.Event {
	payload, _ := json.Marshal(msg)
	return store.Event{Type: evType, PayloadJSON: string(payload)}
}

func TestCaptureSession(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	events := []store.Event{
		eventMsg("user_msg", provider.Message{Role: "user", Content: "fix the omnichannel filter"}),
		eventMsg("assistant_msg", provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{
			{Name: "edit_file", Arguments: `{"path":"src/filter.ts"}`},
			{Name: "write_file", Arguments: `{"path":"src/new.ts"}`},
			{Name: "read_file", Arguments: `{"path":"src/other.ts"}`}, // not an edit
		}}),
		eventMsg("user_msg", provider.Message{Role: "user", Content: "done now"}),
	}
	if err := s.CaptureSession("sess_1", events); err != nil {
		t.Fatalf("capture: %v", err)
	}

	list := s.List()
	if !strings.Contains(list, "done now") {
		t.Errorf("goal not captured (should be the last user msg):\n%s", list)
	}
	if !strings.Contains(list, "src/filter.ts") || !strings.Contains(list, "src/new.ts") {
		t.Errorf("touched files not captured:\n%s", list)
	}
	if strings.Contains(list, "src/other.ts") {
		t.Errorf("read_file must not be counted as touched:\n%s", list)
	}

	// Warm start should surface the capture for a future session.
	s2 := NewStore(dir)
	ws := s2.WarmStart()
	if !strings.Contains(ws, "Touched files") || !strings.Contains(ws, "Goal: done now") {
		t.Errorf("warm start missing captured session:\n%s", ws)
	}
}

func TestCaptureSessionEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	// Engine reminders (⚠️) and commands (📖) must not become goals.
	events := []store.Event{
		eventMsg("user_msg", provider.Message{Role: "user", Content: "⚠️ Tool budget exhausted"}),
		eventMsg("assistant_msg", provider.Message{Role: "assistant", Content: "hello"}),
	}
	if err := s.CaptureSession("sess_2", events); err != nil {
		t.Fatalf("capture: %v", err)
	}
	list := s.List()
	if strings.Contains(list, "Tool budget") {
		t.Errorf("engine reminder leaked as goal:\n%s", list)
	}
}

func TestNilStore(t *testing.T) {
	var s *Store
	if s.WarmStart() != "" {
		t.Error("nil warm start should be empty")
	}
	if !strings.Contains(s.Recall("x", 5), "No project memory") {
		t.Error("nil recall should say unavailable")
	}
	if s.Path() != "" {
		t.Error("nil path should be empty")
	}
}

func TestPruneCap(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for i := 0; i < 50; i++ {
		s.Retain("Notes", strings.Repeat("fact ", 40)+string(rune('a'+i%26)))
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(s.Path()) != "memory.md" {
		t.Errorf("unexpected filename: %s", filepath.Base(s.Path()))
	}
	_ = data
}
