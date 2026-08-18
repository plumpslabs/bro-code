package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Embedder calls an OpenAI-compatible /embeddings endpoint — the same shape
// as OpenAI's text-embedding-3-small — so semantic re-ranking works with any
// compatible gateway the user configures. Nil-safe: when an Embedder is not
// wired, search_code stays BM25-only.
type Embedder struct {
	BaseURL string
	APIKey  string
	Model   string
	client  *http.Client
}

// NewEmbedder builds an embedder for an OpenAI-compatible base URL.
func NewEmbedder(baseURL, apiKey, model string) *Embedder {
	return &Embedder{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// embedBatch embeds texts via POST {base}/embeddings. Inputs are truncated
// to keep each request cheap; the response shape follows the OpenAI API.
func (e *Embedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.Model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([][]float32, 0, len(payload.Data))
	for _, d := range payload.Data {
		out = append(out, d.Embedding)
	}
	return out, nil
}

// Cosine returns the cosine similarity of two vectors (0 when either is
// empty), used to score how well a file matches the query semantically.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// truncateForEmbedding keeps the most informative slice of a file for
// embedding (head + tail), bounding the request size.
func truncateForEmbedding(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max/2]) + "\n…\n" + string(r[len(r)-max/2:])
}

// semanticCacheEntry is one cached file embedding, keyed by content hash so
// edits invalidate only the files that actually changed.
type semanticCacheEntry struct {
	Hash   string    `json:"hash"`
	Vector []float32 `json:"vector"`
}

// SemanticCache persists per-file embeddings under .brocode/index so repeated
// queries don't re-pay embedding cost for unchanged files.
type SemanticCache struct {
	mu    sync.Mutex
	path  string
	files map[string]semanticCacheEntry // abs path → entry
}

// NewSemanticCache loads (or initializes) the cache file for root.
func NewSemanticCache(root string) *SemanticCache {
	dir := filepath.Join(root, ".brocode", "index")
	_ = os.MkdirAll(dir, 0o755)
	c := &SemanticCache{path: filepath.Join(dir, "semantic.json"), files: map[string]semanticCacheEntry{}}
	if data, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(data, &c.files)
	}
	return c
}

func contentHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:12])
}

// EmbedWithCache returns the vector for path, embedding + caching on miss.
func (c *SemanticCache) EmbedWithCache(ctx context.Context, e *Embedder, path string, maxBytes int) ([]float32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		data = data[:maxBytes]
	}
	hash := contentHash(data)

	c.mu.Lock()
	if entry, ok := c.files[path]; ok && entry.Hash == hash {
		v := entry.Vector
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	vecs, err := e.embedBatch(ctx, []string{string(data)})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("no embedding returned for %s", path)
	}
	v := vecs[0]

	c.mu.Lock()
	// Cap the cache at 500 entries; drop the oldest by insert order.
	if len(c.files) >= 500 {
		var oldest string
		for p := range c.files {
			if oldest == "" || p < oldest {
				oldest = p
			}
		}
		delete(c.files, oldest)
	}
	c.files[path] = semanticCacheEntry{Hash: hash, Vector: v}
	data2, _ := json.Marshal(c.files)
	c.mu.Unlock()
	_ = os.WriteFile(c.path, data2, 0o644)
	return v, nil
}

// ReRank re-ranks BM25 results by embedding cosine similarity: the query and
// each candidate file are embedded (with a persistent per-file cache), and
// the top `limit` most similar files are returned. On any embedding failure
// (no endpoint, bad key, unsupported model) it returns the BM25 candidates
// untouched, truncated to limit — search_code never breaks because of it.
func ReRank(ctx context.Context, root, query string, results []bm25Result, e *Embedder, limit int) []bm25Result {
	if e == nil || len(results) == 0 {
		return results
	}
	if limit <= 0 {
		limit = 5
	}

	queryVec, err := e.embedBatch(ctx, []string{truncateForEmbedding(query, 500)})
	if err != nil || len(queryVec) == 0 {
		return results
	}
	cache := NewSemanticCache(root)

	type scored struct {
		res bm25Result
		sim float64
	}
	var scoredList []scored
	for _, r := range results {
		path := r.Doc.ID
		vec, err := cache.EmbedWithCache(ctx, e, path, 4000)
		if err != nil {
			continue
		}
		scoredList = append(scoredList, scored{res: r, sim: Cosine(queryVec[0], vec)})
	}
	if len(scoredList) == 0 {
		return results
	}
	sort.Slice(scoredList, func(i, j int) bool { return scoredList[i].sim > scoredList[j].sim })
	if len(scoredList) > limit {
		scoredList = scoredList[:limit]
	}
	out := make([]bm25Result, 0, len(scoredList))
	for _, s := range scoredList {
		out = append(out, s.res)
	}
	return out
}

// ReRankDocs re-ranks in-memory BM25 results (e.g. project-memory facts) by
// embedding cosine similarity. Unlike ReRank — which persists a per-file
// embedding cache keyed by path — docs here are short texts embedded directly,
// so the candidate set must already be small (the caller limits it, typically
// top-8 from BM25). The query and each candidate are embedded in one batch;
// on ANY embedding failure it returns the BM25 order untouched — hybrid
// retrieval never breaks BM25-only operation.
func ReRankDocs(ctx context.Context, query string, results []bm25Result, e *Embedder, limit int) []bm25Result {
	if e == nil || len(results) == 0 {
		return results
	}
	if limit <= 0 {
		limit = 5
	}
	if len(results) <= limit {
		limit = len(results)
	}

	qvec, err := e.embedBatch(ctx, []string{truncateForEmbedding(query, 500)})
	if err != nil || len(qvec) == 0 {
		return results
	}
	texts := make([]string, len(results))
	for i, r := range results {
		texts[i] = truncateForEmbedding(r.Doc.Body, 500)
	}
	vecs, err := e.embedBatch(ctx, texts)
	if err != nil || len(vecs) != len(results) {
		return results
	}

	type scored struct {
		res bm25Result
		sim float64
	}
	sc := make([]scored, len(results))
	for i, v := range vecs {
		sc[i] = scored{res: results[i], sim: Cosine(qvec[0], v)}
	}
	sort.Slice(sc, func(i, j int) bool { return sc[i].sim > sc[j].sim })

	out := make([]bm25Result, len(sc))
	for i := range sc {
		out[i] = sc[i].res
	}
	return out
}
