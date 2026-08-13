// Package search provides BM25 relevance matching for tool/skill
// definitions. Hand-rolled, zero dependency — deliberately: our corpus is
// only dozens to hundreds of short descriptions, and this project's
// philosophy is against unnecessary dependencies (see docs/TECH_STACK.md §4).
//
// Bounded by design (Principle 1): the index is built once from a fixed
// in-memory corpus. If the corpus ever becomes dynamic or unbounded, an
// eviction/retention policy must exist BEFORE that happens — not be patched
// in later.
package search

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Document is a searchable item.
type Document struct {
	ID      string // unique identifier (e.g., tool/skill name or file path)
	Title   string // short name
	Body    string // searchable text
	Snippet string // optional preview line shown with results ("" = derive from Body)

	// Path is the file path (when ID is a path). It is indexed as part of the
	// searchable text so a term living in a DIRECTORY name ranks the file
	// above one where the term only appears in content: "rotation" must find
	// `.../lead-rotation/LeadRotationService.js` before a tiptap component
	// that merely mentions "rotation" in its body (observed: the LineShape
	// mis-answer — see docs/AGENTIC_OVERHAUL.md).
	Path string
}

// Result is a single match ordered by BM25 score.
type Result struct {
	ID      string
	Title   string
	Score   float64
	Snippet string // one-line preview so the caller can pick without reading the full body
}

// Index is an in-memory BM25 index. It holds data only — no background
// goroutines, no unbounded growth.
type Index struct {
	docs     []Document
	postings map[string]map[int]int // term -> docID -> term frequency
	docFreq  map[string]int         // term -> number of docs containing the term
	docLen   []int                  // token count per document
	avgLen   float64
	n        int
}

// Standard BM25 parameters (Okapi BM25).
const (
	k1 = 1.2
	b  = 0.75
)

// pathTokenWeight is the multiplier applied to tokens that come from the
// file PATH (directory names). Path terms are the highest-signal locator a
// user has ("lead-rotation", "auth", "payment") but appear exactly once,
// while a term buried in file BODY can repeat many times and out-score it
// under plain BM25 TF (observed: "rotation" 5x in LineShape.tsx's body beat
// the 1x path occurrence in .../lead-rotation/LeadRotationService.js, so the
// agent answered about the WRONG rotation). Weighting path tokens keeps a
// directory match on top of a body-only match (see
// TestSearchPathBoostRanksDirectoryMatchAboveBodyMatch).
const pathTokenWeight = 8

// New builds an index from a corpus. Returns an empty index for an empty
// corpus. Complexity is O(total tokens) — once, at startup (Principle 2:
// metadata only, not full content).
func New(docs []Document) *Index {
	ix := &Index{
		docs:     docs,
		postings: make(map[string]map[int]int),
		docFreq:  make(map[string]int),
		docLen:   make([]int, len(docs)),
	}
	total := 0
	for i, d := range docs {
		seen := make(map[string]bool)
		pathTokens := tokenSet(d.Path) // exact path tokens — the ones to boost
		for _, t := range tokenize(d.Path + " " + d.Title + " " + d.Body) {
			freq := 1
			if pathTokens[t] {
				freq = pathTokenWeight // path tokens are the strongest locator
			}
			ix.docLen[i] += freq
			if ix.postings[t] == nil {
				ix.postings[t] = make(map[int]int)
			}
			if !seen[t] {
				seen[t] = true
				ix.docFreq[t]++
			}
			ix.postings[t][i] += freq
		}
		total += ix.docLen[i]
	}
	ix.n = len(docs)
	if ix.n > 0 {
		ix.avgLen = float64(total) / float64(ix.n)
	}
	return ix
}

// Len returns the number of documents in the index (bounded corpus size).
func (ix *Index) Len() int { return ix.n }

// Docs returns a copy of the indexed documents, for read-only listing.
func (ix *Index) Docs() []Document {
	out := make([]Document, len(ix.docs))
	copy(out, ix.docs)
	return out
}

// Search returns up to topK most relevant documents. Results are capped at
// topK (bounded — Principle 1) at the point of creation, not patched later.
func (ix *Index) Search(query string, topK int) []Result {
	if topK <= 0 {
		return nil
	}
	if topK > ix.n {
		topK = ix.n
	}
	terms := tokenize(query)
	if len(terms) == 0 || ix.avgLen == 0 {
		// Empty corpus or all documents without tokens — avoid 0/0 (NaN)
		// in the TF formula. Defensive guard, not the normal path.
		return nil
	}

	// Sized by len(ix.docs) (not ix.n): Remove() leaves empty slots behind, and
	// a live posting can reference a slot past the live count.
	scores := make([]float64, len(ix.docs))
	for _, t := range terms {
		posting, ok := ix.postings[t]
		if !ok {
			continue // term not in corpus — skip
		}
		// IDF with smoothing (standard BM25 formula).
		idf := math.Log(1 + (float64(ix.n)-float64(ix.docFreq[t])+0.5)/(float64(ix.docFreq[t])+0.5))
		for docID, freq := range posting {
			dl := float64(ix.docLen[docID])
			tf := float64(freq) * (k1 + 1) / (float64(freq) + k1*(1-b+b*dl/ix.avgLen))
			scores[docID] += idf * tf
		}
	}

	// Collect docs with score > 0 and sort descending.
	idxs := make([]int, 0, ix.n)
	for i, s := range scores {
		if s > 0 {
			idxs = append(idxs, i)
		}
	}
	sort.Slice(idxs, func(a, b int) bool {
		if scores[idxs[a]] != scores[idxs[b]] {
			return scores[idxs[a]] > scores[idxs[b]]
		}
		return idxs[a] < idxs[b] // stable, deterministic
	})
	if len(idxs) > topK {
		idxs = idxs[:topK]
	}

	out := make([]Result, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, Result{
			ID:      ix.docs[i].ID,
			Title:   ix.docs[i].Title,
			Score:   scores[i],
			Snippet: snippetOf(ix.docs[i]),
		})
	}
	return out
}

// snippetOf returns the document's preview line: the explicit Snippet when
// set, otherwise the first non-empty body line (bounded — a preview must
// never flood the result list).
func snippetOf(d Document) string {
	if strings.TrimSpace(d.Snippet) != "" {
		return clipLine(d.Snippet, 80)
	}
	for _, ln := range strings.Split(d.Body, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return clipLine(t, 80)
		}
	}
	return ""
}

// clipLine bounds a snippet to maxRunes runes with a trailing ellipsis.
func clipLine(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// docIndex returns the slice index of the document with the given ID, or -1.
// Removed slots (ID "") are never matched, so a re-added ID appends fresh.
func (ix *Index) docIndex(id string) int {
	for i, d := range ix.docs {
		if d.ID == id {
			return i
		}
	}
	return -1
}

// Update upserts a document: it replaces the content of an existing document
// with the same ID, or appends a new one. Only THIS document's tokens are
// re-processed — postings, docFreq, docLen, and avgLen are adjusted
// incrementally, never a full rebuild. Used by the TUI's mtime-driven index
// refresh so edited files stay searchable without re-reading unchanged ones.
func (ix *Index) Update(id string, doc Document) {
	idx := ix.docIndex(id)
	if idx < 0 {
		idx = len(ix.docs)
		ix.docs = append(ix.docs, Document{})
		ix.docLen = append(ix.docLen, 0)
		ix.n++
	}
	ix.removeTokens(idx) // drop the old content's tokens first
	ix.docs[idx] = doc
	ix.addTokens(idx, doc.Path+" "+doc.Title+" "+doc.Body)
	ix.recomputeAvg()
	ix.compactLive()
}

// Remove deletes a document by ID, dropping its tokens from the postings. The
// slice slot is left as an empty hole (ID "") rather than swap-removed — a
// swap would renumber the docIDs referenced by every other posting.
func (ix *Index) Remove(id string) {
	idx := ix.docIndex(id)
	if idx < 0 {
		return
	}
	ix.removeTokens(idx)
	ix.docs[idx] = Document{}
	ix.docLen[idx] = 0
	ix.n--
	ix.recomputeAvg()
	ix.compactLive()
}

// compactLive rebuilds the index from its live documents when removed slots
// accumulate past a threshold. A long session that churns files (delete →
// re-add the same ID, a file oscillating across the size cap) would otherwise
// grow the doc slice with empty holes forever — bounded by design (Principle
// 1). The rebuild is over the already-bounded corpus, so it is cheap and rare.
func (ix *Index) compactLive() {
	if len(ix.docs) <= 2*ix.n+32 {
		return
	}
	live := make([]Document, 0, ix.n)
	for _, d := range ix.docs {
		if d.ID != "" {
			live = append(live, d)
		}
	}
	*ix = *New(live)
}

// addTokens indexes one document's text into the shared structures, mirroring
// New()'s per-doc logic (docFreq counts a term once per document). Callers
// pass Path + Title + Body so path terms rank alongside content terms.
func (ix *Index) addTokens(idx int, text string) {
	path := ix.docs[idx].Path
	seen := make(map[string]bool)
	for _, t := range tokenize(text) {
		freq := weightedFreq(path, t)
		ix.docLen[idx] += freq
		if ix.postings[t] == nil {
			ix.postings[t] = make(map[int]int)
		}
		if !seen[t] {
			seen[t] = true
			ix.docFreq[t]++
		}
		ix.postings[t][idx] += freq
	}
}

// removeTokens removes one document's tokens from the shared structures.
func (ix *Index) removeTokens(idx int) {
	d := ix.docs[idx]
	if d.ID == "" && ix.docLen[idx] == 0 {
		return // already an empty (removed) slot
	}
	seen := make(map[string]bool)
	for _, t := range tokenize(d.Path + " " + d.Title + " " + d.Body) {
		if seen[t] {
			continue
		}
		seen[t] = true
		pm := ix.postings[t]
		if pm == nil {
			continue
		}
		// Delete the WEIGHTED frequency (path tokens were added with
		// pathTokenWeight) so docLen/postings come back exactly to 0.
		if freq, ok := pm[idx]; ok {
			ix.docLen[idx] -= freq
			delete(pm, idx)
			if len(pm) == 0 {
				delete(ix.postings, t)
				delete(ix.docFreq, t)
			} else if ix.docFreq[t] > 0 {
				ix.docFreq[t]--
			}
		}
	}
}

// recomputeAvg recalculates the average document length from the current
// docLen slice (removed slots contribute 0).
func (ix *Index) recomputeAvg() {
	total := 0
	for _, l := range ix.docLen {
		total += l
	}
	if ix.n > 0 {
		ix.avgLen = float64(total) / float64(ix.n)
	} else {
		ix.avgLen = 0
	}
}

// tokenize lowercases and splits on non-alphanumeric characters. Stopwords
// and stemming are deliberately skipped at this scale — add them if the
// corpus actually grows.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// tokenSet returns the unique tokens of s as a set (nil-safe for empty s).
// Used to identify which tokens came from the PATH (the ones that get the
// pathTokenWeight boost) vs the body.
func tokenSet(s string) map[string]bool {
	if s == "" {
		return nil
	}
	set := make(map[string]bool)
	for _, t := range tokenize(s) {
		set[t] = true
	}
	return set
}

// weightedFreq returns the posting frequency for a token in a document whose
// path is d.Path: path tokens get pathTokenWeight, everything else 1. Kept in
// one place so New, addTokens and removeTokens can never disagree (a mismatch
// would corrupt docLen/postings on update).
func weightedFreq(path string, t string) int {
	if tokenSet(path)[t] {
		return pathTokenWeight
	}
	return 1
}
