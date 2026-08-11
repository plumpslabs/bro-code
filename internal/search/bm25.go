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
	ID    string // unique identifier (e.g., tool/skill name)
	Title string // short name
	Body  string // one-line description
}

// Result is a single match ordered by BM25 score.
type Result struct {
	ID    string
	Title string
	Score float64
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
		for _, t := range tokenize(d.Title + " " + d.Body) {
			ix.docLen[i]++
			if ix.postings[t] == nil {
				ix.postings[t] = make(map[int]int)
			}
			if !seen[t] {
				seen[t] = true
				ix.docFreq[t]++
			}
			ix.postings[t][i]++
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

	scores := make([]float64, ix.n)
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
		out = append(out, Result{ID: ix.docs[i].ID, Title: ix.docs[i].Title, Score: scores[i]})
	}
	return out
}

// tokenize lowercases and splits on non-alphanumeric characters. Stopwords
// and stemming are deliberately skipped at this scale — add them if the
// corpus actually grows.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
