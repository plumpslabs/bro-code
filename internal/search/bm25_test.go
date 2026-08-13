package search

import (
	"fmt"
	"strings"
	"testing"
)

func TestSearchRanksRelevantDocFirst(t *testing.T) {
	ix := New(SampleCorpus())
	res := ix.Search("mcp", 5)
	if len(res) == 0 {
		t.Fatal("expected at least one result for query 'mcp'")
	}
	if res[0].ID != "skill-mcp" {
		t.Fatalf("expected skill-mcp first, got %q (score %.3f)", res[0].ID, res[0].Score)
	}
}

// TestSearchPathBoostRanksDirectoryMatchAboveBodyMatch is the regression test
// for the LineShape mis-answer: a file whose PATH contains the query term (the
// CRM lead-rotation service) must rank ABOVE a file where the term only
// appears in the body (a tiptap line-shape component with a "rotation"
// attribute). The path was never indexed before, so body matches always won.
func TestSearchPathBoostRanksDirectoryMatchAboveBodyMatch(t *testing.T) {
	ix := New([]Document{
		{
			ID:    "crm-react-vite-tailwind-modern/src/components/tiptap/LineShape.tsx",
			Title: "LineShape.tsx",
			Body:  "addAttributes rotation parseFloat(getAttribute(data-rotation)) renderHTML transform rotate deg",
		},
		{
			ID:    "crm_sales_backend/src/services/lead-rotation/LeadRotationService.js",
			Title: "LeadRotationService.js",
			Path:  "crm_sales_backend/src/services/lead-rotation/LeadRotationService.js",
			Body:  "const prisma = require; class LeadRotationService { importSchedule csv rows }",
		},
	})
	res := ix.Search("rotation", 5)
	if len(res) < 2 {
		t.Fatalf("expected both docs to match 'rotation', got %+v", res)
	}
	// The CRM service (path token) must outrank the tiptap component (body only).
	if res[0].ID != "crm_sales_backend/src/services/lead-rotation/LeadRotationService.js" {
		t.Fatalf("path match must outrank body match, got %q first (scores %v)", res[0].ID, res)
	}
}

// TestSearchPathBoostUpdateAddsPath: Update() must index the path too, so an
// incremental refresh keeps path ranking after a file change.
func TestSearchPathBoostUpdateAddsPath(t *testing.T) {
	ix := New([]Document{
		{ID: "a/b/LeadRotationService.js", Title: "LeadRotationService.js", Body: "rotation attr in body"},
		{ID: "x/LineShape.tsx", Title: "LineShape.tsx", Body: "rotation visual"},
	})
	ix.Update("a/b/LeadRotationService.js", Document{
		ID:    "a/b/LeadRotationService.js",
		Title: "LeadRotationService.js",
		Path:  "a/b/lead-rotation/LeadRotationService.js",
		Body:  "rotation attr in body",
	})
	res := ix.Search("rotation", 5)
	if len(res) == 0 || res[0].ID != "a/b/LeadRotationService.js" {
		t.Fatalf("Update must keep path ranking, got %+v", res)
	}
}

func TestSearchCaseInsensitiveAndMultiword(t *testing.T) {
	ix := New(SampleCorpus())
	// "Myers" is uppercase in the corpus; a lowercase query must still match.
	res := ix.Search("myers", 3)
	if len(res) == 0 {
		t.Fatal("expected match for lowercase query 'myers'")
	}
	// Two-word query — combined score; the edit tool must win.
	res = ix.Search("edit diff", 3)
	if len(res) == 0 || res[0].ID != "tool-edit" {
		t.Fatalf("expected tool-edit first for 'edit diff', got %+v", res)
	}
}

func TestSearchNoMatchReturnsEmpty(t *testing.T) {
	ix := New(SampleCorpus())
	if res := ix.Search("zyxwvunlikelyterm", 5); len(res) != 0 {
		t.Fatalf("expected no results, got %+v", res)
	}
}

func TestSearchTopKIsBounded(t *testing.T) {
	ix := New(SampleCorpus())
	for _, topK := range []int{0, -1, 1, 3, 100} {
		res := ix.Search("tool", topK)
		if topK <= 0 {
			if len(res) != 0 {
				t.Fatalf("topK=%d: expected empty, got %d results", topK, len(res))
			}
			continue
		}
		want := topK
		if want > len(SampleCorpus()) {
			want = len(SampleCorpus())
		}
		if len(res) > want {
			t.Fatalf("topK=%d: got %d results (must be <= corpus size)", topK, len(res))
		}
		// Must be sorted descending.
		for i := 1; i < len(res); i++ {
			if res[i-1].Score < res[i].Score {
				t.Fatalf("results not sorted desc at %d: %v > %v", i, res[i-1].Score, res[i].Score)
			}
		}
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	ix := New(SampleCorpus())
	if res := ix.Search("   ", 5); len(res) != 0 {
		t.Fatalf("expected no results for empty query, got %+v", res)
	}
}

func TestSearchEmptyCorpus(t *testing.T) {
	ix := New(nil)
	if res := ix.Search("anything", 5); len(res) != 0 {
		t.Fatalf("expected no results for empty corpus, got %+v", res)
	}
}

func TestSearchResultSnippet(t *testing.T) {
	// An explicit Snippet wins; otherwise the first non-empty Body line is
	// used (blank lines skipped, bounded).
	ix := New([]Document{
		{ID: "a", Title: "A", Body: "package auth\n\n// handles refresh tokens\nfunc X() {}", Snippet: "explicit preview"},
		{ID: "b", Title: "B", Body: "package db\n\nvar conn string"},
		{ID: "c", Title: "C", Body: "second line only\n\nauth conn"},
	})
	got := ix.Search("auth conn", 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 matches, got %+v", got)
	}
	byID := map[string]string{}
	for _, r := range got {
		byID[r.ID] = r.Snippet
	}
	if byID["a"] != "explicit preview" {
		t.Fatalf("explicit snippet must win, got %q", byID["a"])
	}
	if byID["b"] != "package db" {
		t.Fatalf("fallback first non-empty line, got %q", byID["b"])
	}
	if byID["c"] != "second line only" {
		t.Fatalf("blank lines must be skipped, got %q", byID["c"])
	}
	// The snippet is bounded to 80 runes.
	long := New([]Document{{ID: "x", Title: "X", Body: strings.Repeat("token ", 40)}})
	r := long.Search("token", 1)
	if len(r) != 1 || len([]rune(r[0].Snippet)) > 81 {
		t.Fatalf("snippet must be bounded, got %q", r[0].Snippet)
	}
}

func TestIndexUpdateReplacesContent(t *testing.T) {
	ix := New([]Document{
		{ID: "a", Title: "A", Body: "old token"},
		{ID: "b", Title: "B", Body: "stable token"},
	})
	if res := ix.Search("old", 5); len(res) != 1 || res[0].ID != "a" {
		t.Fatalf("setup: expected a to match 'old', got %+v", res)
	}
	ix.Update("a", Document{ID: "a", Title: "A", Body: "brand new content"})
	if res := ix.Search("old", 5); len(res) != 0 {
		t.Fatalf("old content must be gone after Update, got %+v", res)
	}
	if res := ix.Search("brand", 5); len(res) != 1 || res[0].ID != "a" {
		t.Fatalf("new content must match after Update, got %+v", res)
	}
	if res := ix.Search("stable", 5); len(res) != 1 || res[0].ID != "b" {
		t.Fatalf("unrelated doc must be unaffected, got %+v", res)
	}
}

func TestIndexUpdateAddsNewDoc(t *testing.T) {
	ix := New(nil)
	ix.Update("c", Document{ID: "c", Title: "C", Body: "fresh doc"})
	if ix.Len() != 1 {
		t.Fatalf("expected Len()=1 after upsert, got %d", ix.Len())
	}
	if res := ix.Search("fresh", 5); len(res) != 1 || res[0].ID != "c" {
		t.Fatalf("upsert must add a new doc, got %+v", res)
	}
}

func TestIndexRemoveDropsDoc(t *testing.T) {
	ix := New([]Document{
		{ID: "a", Title: "A", Body: "alpha"},
		{ID: "b", Title: "B", Body: "beta"},
		{ID: "c", Title: "C", Body: "charlie"},
	})
	ix.Remove("b")
	if ix.Len() != 2 {
		t.Fatalf("expected Len()=2 after Remove, got %d", ix.Len())
	}
	if res := ix.Search("beta", 5); len(res) != 0 {
		t.Fatalf("removed doc must not match, got %+v", res)
	}
	if res := ix.Search("alpha", 5); len(res) != 1 || res[0].ID != "a" {
		t.Fatalf("other docs must survive, got %+v", res)
	}
	// Remove + re-add the same ID appends fresh (slots never collide).
	ix.Update("b", Document{ID: "b", Title: "B2", Body: "beta v2"})
	if ix.Len() != 3 {
		t.Fatalf("expected Len()=3 after re-add, got %d", ix.Len())
	}
	if res := ix.Search("beta", 5); len(res) != 1 || res[0].ID != "b" {
		t.Fatalf("re-added doc must match, got %+v", res)
	}
}

func TestIndexCompactsHoles(t *testing.T) {
	// Remove → re-add churn must never grow the internal doc slice without
	// bound: compactLive rebuilds from the live docs past a hole threshold.
	ix := New(nil)
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("doc-%d", i)
		ix.Update(id, Document{ID: id, Title: "T", Body: "unique token " + id})
	}
	if ix.Len() != 60 {
		t.Fatalf("setup: expected 60 live docs, got %d", ix.Len())
	}
	for round := 0; round < 10; round++ {
		for i := 0; i < 60; i += 2 {
			ix.Remove(fmt.Sprintf("doc-%d", i))
		}
		for i := 0; i < 60; i += 2 {
			ix.Update(fmt.Sprintf("doc-%d", i), Document{ID: fmt.Sprintf("doc-%d", i), Title: "T", Body: "unique token doc-" + fmt.Sprintf("%d", i)})
		}
	}
	if internal := len(ix.docs); internal > 2*ix.Len()+32 {
		t.Fatalf("doc slice grew unbounded: %d slots for %d live docs", internal, ix.Len())
	}
	if ix.Len() != 60 {
		t.Fatalf("live count must stay 60, got %d", ix.Len())
	}
	// Search still ranks correctly after compaction.
	res := ix.Search("doc-59", 5)
	if len(res) == 0 || res[0].ID != "doc-59" {
		t.Fatalf("search must survive compaction, got %+v", res)
	}
}
