package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCosine(t *testing.T) {
	if got := Cosine([]float32{1, 0}, []float32{1, 0}); got != 1 {
		t.Errorf("identical vectors: got %v want 1", got)
	}
	if got := Cosine([]float32{1, 0}, []float32{0, 1}); got > 0.001 {
		t.Errorf("orthogonal vectors: got %v want ~0", got)
	}
	if got := Cosine(nil, []float32{1, 0}); got != 0 {
		t.Errorf("empty vector: got %v want 0", got)
	}
	if got := Cosine([]float32{1}, []float32{1, 2}); got != 0 {
		t.Errorf("mismatched lengths: got %v want 0", got)
	}
}

func TestTruncateForEmbedding(t *testing.T) {
	short := "hello"
	if got := truncateForEmbedding(short, 10); got != short {
		t.Errorf("short input changed: %q", got)
	}
	long := strings.Repeat("x", 1000)
	got := truncateForEmbedding(long, 100)
	if len(got) >= len(long) || !strings.Contains(got, "…") {
		t.Errorf("long input not truncated with marker, len=%d", len(got))
	}
}

// fakeEmbedServer returns vectors derived from the input text so different
// texts get different (but deterministic) vectors.
func fakeEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var data []map[string]any
		for _, text := range req.Input {
			vec := make([]float32, 8)
			for i, ch := range text {
				vec[i%8] += float32(int(ch) % 7)
			}
			data = append(data, map[string]any{"embedding": vec, "index": len(data)})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

func TestEmbedderEmbedBatch(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	e := NewEmbedder(srv.URL, "sk-test", "text-embedding-3-small")
	vecs, err := e.embedBatch(context.Background(), []string{"payment flow", "auth"})
	if err != nil {
		t.Fatalf("embedBatch: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 8 {
		t.Fatalf("unexpected vectors: len=%d dim=%d", len(vecs), len(vecs[0]))
	}
}

func TestEmbedderRejectsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", 404)
	}))
	defer srv.Close()

	e := NewEmbedder(srv.URL, "sk-test", "nope")
	if _, err := e.embedBatch(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected error on HTTP 404")
	}
}

func TestSemanticCacheRoundtrip(t *testing.T) {
	root := t.TempDir()
	cache := NewSemanticCache(root)
	srv := fakeEmbedServer(t)
	defer srv.Close()
	e := NewEmbedder(srv.URL, "", "m")

	path := filepath.Join(root, "a.go")
	if err := os.WriteFile(path, []byte("func payment() {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	v1, err := cache.EmbedWithCache(context.Background(), e, path, 4000)
	if err != nil {
		t.Fatalf("first embed: %v", err)
	}
	// Second call must hit the cache (same hash → no server roundtrip needed,
	// verified by the cache file existing and the vector matching).
	v2, err := cache.EmbedWithCache(context.Background(), e, path, 4000)
	if err != nil {
		t.Fatalf("cached embed: %v", err)
	}
	if Cosine(v1, v2) < 0.999 {
		t.Errorf("cached vector differs from fresh embed: %v", Cosine(v1, v2))
	}
	// Cache file persisted.
	if _, err := os.Stat(filepath.Join(root, ".brocode", "index", "semantic.json")); err != nil {
		t.Errorf("semantic cache file not persisted: %v", err)
	}

	// Editing the file changes the hash → re-embed.
	if err := os.WriteFile(path, []byte("func payment() { changed }"), 0o644); err != nil {
		t.Fatal(err)
	}
	v3, err := cache.EmbedWithCache(context.Background(), e, path, 4000)
	if err != nil {
		t.Fatalf("re-embed after edit: %v", err)
	}
	if Cosine(v2, v3) == 1 {
		t.Error("edited file should re-embed (vector must change)")
	}
}

func TestReRankFallsBackOnEmbeddingFailure(t *testing.T) {
	root := t.TempDir()
	// A server that always errors — ReRank must return BM25 candidates intact.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	e := NewEmbedder(srv.URL, "k", "m")

	docs := []Document{
		{ID: filepath.Join(root, "auth.go"), Title: "auth.go", Body: "func login(token string) { }"},
		{ID: filepath.Join(root, "pay.go"), Title: "pay.go", Body: "func charge(amount int) { }"},
	}
	idx := NewBM25(docs)
	results := idx.Search("login token", 2)
	reranked := ReRank(context.Background(), root, "login token", results, e, 5)
	if len(reranked) != len(results) {
		t.Errorf("ReRank on failure should fall back to BM25: got %d want %d", len(reranked), len(results))
	}
	if reranked == nil && results != nil {
		t.Error("ReRank returned nil on failure")
	}
}
