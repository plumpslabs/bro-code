package ui

import (
	"strings"
	"testing"
)

func TestExtractRecommendations(t *testing.T) {
	rawResponse := `Saya telah selesai mengimplementasikan sistem authentication JWT.

### 💡 Senior Recommendations
- [ ] **Lanjutkan test auth** — Lanjutkan test auth dengan beberapa validator email dan password
- [ ] **Pasang rate limiter** — Tambahkan rate limiter Redis pada endpoint /api/auth/login
- [ ] **Dokumentasikan API** — Generate OpenAPI swagger spec untuk endpoint auth
`

	recs := ExtractRecommendations(rawResponse)
	if len(recs) != 3 {
		t.Fatalf("expected 3 recommendations, got %d", len(recs))
	}

	if recs[0].Title != "Lanjutkan test auth" {
		t.Errorf("expected title 'Lanjutkan test auth', got %q", recs[0].Title)
	}
	if recs[0].Prompt != "Lanjutkan test auth dengan beberapa validator email dan password" {
		t.Errorf("expected prompt matched, got %q", recs[0].Prompt)
	}
	if recs[1].Title != "Pasang rate limiter" {
		t.Errorf("expected title 'Pasang rate limiter', got %q", recs[1].Title)
	}
}

func TestExtractRecommendationsNumberedFormat(t *testing.T) {
	rawResponse := `Perubahan query database sudah dioptimasi.

### Senior Next Actions
1. **Tambahkan index DB** — Buat migration index pada kolom user_id dan created_at
2. **Benchmark query** — Jalankan benchmark latency query sebelum dan sesudah index
`

	recs := ExtractRecommendations(rawResponse)
	if len(recs) != 2 {
		t.Fatalf("expected 2 recommendations, got %d", len(recs))
	}
	if recs[0].Title != "Tambahkan index DB" {
		t.Errorf("expected title 'Tambahkan index DB', got %q", recs[0].Title)
	}
}

func TestRenderRecommendationsBar(t *testing.T) {
	recs := []QuickRecommendation{
		{Index: 1, Title: "Test Auth", Prompt: "Run auth tests", Clicked: false},
		{Index: 2, Title: "Add Rate Limit", Prompt: "Add rate limiting", Clicked: true},
	}

	rendered := RenderRecommendationsBar(recs, 100)
	if !strings.Contains(rendered, "Test Auth") {
		t.Errorf("expected active recommendation in rendered bar, got: %s", rendered)
	}
	if !strings.Contains(rendered, "~~[2] Add Rate Limit~~") {
		t.Errorf("expected clicked recommendation to be struck through, got: %s", rendered)
	}
}

func TestTriggerRecommendationWhenIdleAndBusy(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 40

	m.activeRecommendations = []QuickRecommendation{
		{Index: 1, Title: "Test Auth", Prompt: "Run auth tests", Clicked: false},
		{Index: 2, Title: "Add Rate Limit", Prompt: "Add rate limiting", Clicked: false},
	}

	// 1. When agent is Busy: triggering queues the prompt
	m.turnRunning = true
	m.triggerRecommendation(1)

	if len(m.pendingQueue) != 1 {
		t.Fatalf("expected 1 queued prompt when busy, got %d", len(m.pendingQueue))
	}
	if m.pendingQueue[0].Text != "Run auth tests" {
		t.Errorf("expected queued prompt 'Run auth tests', got %q", m.pendingQueue[0].Text)
	}
	if !m.activeRecommendations[0].Clicked {
		t.Errorf("expected recommendation 1 to be marked clicked")
	}

	// 2. Triggering already-clicked recommendation does nothing
	m.triggerRecommendation(1)
	if len(m.pendingQueue) != 1 {
		t.Fatalf("triggering clicked recommendation should not add to queue, got %d", len(m.pendingQueue))
	}
}
