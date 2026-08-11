package search

import "testing"

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
