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

// TestCaptureOutOfScopeFindings verifies the ### OUT-OF-SCOPE FINDINGS parser:
// bullets under the section are persisted to Notes (prefixed), other sections
// are ignored, and an answer without the section retains nothing.
func TestCaptureOutOfScopeFindings(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	ans := `Fixed the filter. Done.

### OUT-OF-SCOPE FINDINGS
- src/orders.go: N+1 query in listOrders — batch the fetch
- auth.go: hardcoded secret left in a test fixture

## Notes
- unrelated note
`
	n := s.CaptureOutOfScopeFindings(ans)
	if n != 2 {
		t.Fatalf("expected 2 findings retained, got %d", n)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".brocode", "memory.md"))
	if err != nil {
		t.Fatalf("memory file not written: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "Out-of-scope: src/orders.go: N+1 query in listOrders") {
		t.Errorf("N+1 finding missing from memory:\n%s", got)
	}
	if !strings.Contains(got, "Out-of-scope: auth.go: hardcoded secret") {
		t.Errorf("secret finding missing from memory:\n%s", got)
	}
	if strings.Contains(got, "unrelated note") {
		t.Errorf("content outside the findings section must NOT be captured:\n%s", got)
	}

	// No section -> 0 retained, and a repeat call must not duplicate.
	if n := s.CaptureOutOfScopeFindings("just an answer, no findings"); n != 0 {
		t.Fatalf("expected 0 retained without the section, got %d", n)
	}
	if n := s.CaptureOutOfScopeFindings(ans); n != 0 {
		t.Fatalf("repeat capture must dedupe, got %d retained", n)
	}
}

// TestSkillGotchas verifies the Skill Notes collector: per-skill distilled
// lessons come back in order, other sections are ignored, unknown skills get
// nothing.
func TestSkillGotchas(t *testing.T) {
	s := NewStore(t.TempDir())
	_, _ = s.Retain("Skill Notes", "go-workflow: Verification failed on main.go — fixed after 1 repair attempt")
	_, _ = s.Retain("Skill Notes", "ts-workflow: tsc --noEmit failed on a stale build artifact")
	_, _ = s.Retain("Skill Notes", "go-workflow: interface satisfaction usually means a missing method")
	_, _ = s.Retain("Gotchas", "go-workflow: wrong section — must not be picked up")

	got := s.SkillGotchas("go-workflow")
	if len(got) != 2 {
		t.Fatalf("expected 2 go-workflow gotchas, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "Verification failed") || !strings.Contains(got[1], "interface satisfaction") {
		t.Errorf("wrong order/content: %v", got)
	}
	if s.SkillGotchas("rust-workflow") != nil {
		t.Error("expected no gotchas for a skill with no entries")
	}
}

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

// TestAdaptiveWarmStartBudget verifies SetWarmStartBudget shrinks the warm-start
// byte cap below the default (adaptive context budgeting when the window is
// nearly full), without ever growing past the built-in max.
func TestAdaptiveWarmStartBudget(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	bigFact := strings.Repeat("content line to pad warm start output beyond cap ", 800)
	s.Retain("Notes", bigFact)

	full := s.WarmStart()
	if len(full) <= maxWarmStartBytes {
		t.Errorf("fixture too small: warm start = %d bytes, want > default cap %d", len(full), maxWarmStartBytes)
	}
	if !strings.HasSuffix(full, "… (truncated)") {
		t.Error("default cap should truncate the oversized warm start")
	}

	s.SetWarmStartBudget(512)
	capped := s.WarmStart()
	// The byte budget caps the CONTENT; the "… (truncated)" marker adds a
	// fixed suffix on top, so assert the content portion stays within budget.
	if len(capped) > 512+len("\n… (truncated)") {
		t.Errorf("adaptive budget ignored: %d bytes > 512+suffix", len(capped))
	}
	if !strings.Contains(capped, "content line to pad warm start") {
		t.Error("adaptive budget should keep the leading content")
	}

	s.SetWarmStartBudget(0)
	restored := s.WarmStart()
	if len(restored) != len(full) {
		t.Errorf("budget reset should restore default cap: got %d, want %d", len(restored), len(full))
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

// TestCaptureMinerFindings proves a MINER turn leaves durable knowledge in
// project memory even when the model never called the memory retain tool:
// the files it examined and its synthesized summary are auto-persisted.
func TestCaptureMinerFindings(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.CaptureMinerFindings(
		"This CRM is an Express/Prisma monorepo with three React frontends and a vanilla widget. Backend port 3000, admin SPA port 4000, customer portal port 4001.",
		[]string{"crm_sales_backend/app.js", "crm-react-vite-tailwind-modern/src/App.tsx", "crm-widget/src/index.js"},
	); err != nil {
		t.Fatalf("capture: %v", err)
	}

	list := s.List()
	if !strings.Contains(list, "MINER explored: crm_sales_backend/app.js") {
		t.Errorf("examined files not persisted:\n%s", list)
	}
	if !strings.Contains(list, "MINER findings: This CRM is an Express/Prisma monorepo") {
		t.Errorf("model summary not persisted:\n%s", list)
	}

	// Warm start surfaces it for a future session.
	s2 := NewStore(dir)
	if ws := s2.WarmStart(); !strings.Contains(ws, "MINER findings") {
		t.Errorf("warm start missing MINER capture:\n%s", ws)
	}
}

// TestCaptureMinerFindingsSkipsNoise proves trivial turns (greeting, empty)
// do not pollute the memory file.
func TestCaptureMinerFindingsSkipsNoise(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.CaptureMinerFindings("Halo!", nil); err != nil {
		t.Fatalf("capture: %v", err)
	}
	list := s.List()
	if strings.Contains(list, "MINER findings") {
		t.Errorf("short/greeting answer must not be persisted:\n%s", list)
	}
	if strings.Contains(list, "MINER explored") {
		t.Errorf("empty file list must not be persisted:\n%s", list)
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
	for i := range 50 {
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
