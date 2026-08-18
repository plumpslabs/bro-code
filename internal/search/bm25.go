package search

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Document is a searchable item: a file with its content as the body.
type Document struct {
	ID      string // file path
	Title   string // short name (basename)
	Body    string // full or partial content
	Snippet string // optional preview ("" = derive)
}

// tokenize splits text into lowercase word tokens.
func tokenize(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return out
}

// bm25Result is a scored match.
type bm25Result struct {
	Doc   Document
	Score float64
}

// BM25 indexes a set of documents and answers relevance queries.
type BM25 struct {
	docs    []Document
	docFreq map[string]int
	avgLen  float64
	k1      float64
	b       float64
}

// NewBM25 builds an index from documents.
func NewBM25(docs []Document) *BM25 {
	idx := &BM25{
		docs:    docs,
		docFreq: map[string]int{},
		k1:      1.5,
		b:       0.75,
	}
	total := 0
	seen := map[int]map[string]bool{}
	for i, d := range docs {
		terms := tokenize(d.Body + " " + d.Title + " " + d.ID)
		total += len(terms)
		seen[i] = map[string]bool{}
		for _, t := range terms {
			seen[i][t] = true
		}
	}
	for _, s := range seen {
		for t := range s {
			idx.docFreq[t]++
		}
	}
	if len(docs) > 0 {
		idx.avgLen = float64(total) / float64(len(docs))
	} else {
		idx.avgLen = 1
	}
	return idx
}

// Search ranks documents by BM25 relevance to the query, returning top n.
func (b *BM25) Search(query string, n int) []bm25Result {
	if n <= 0 {
		n = 10
	}
	qTerms := tokenize(query)
	if len(qTerms) == 0 {
		return nil
	}
	nDocs := float64(len(b.docs))
	if nDocs == 0 {
		return nil
	}

	var results []bm25Result
	for _, d := range b.docs {
		terms := tokenize(d.Body + " " + d.Title)
		// term frequency
		tf := map[string]int{}
		for _, t := range terms {
			tf[t]++
		}
		score := 0.0
		for _, q := range qTerms {
			f := float64(tf[q])
			if f == 0 {
				continue
			}
			df := float64(b.docFreq[q])
			idf := math.Log(1 + (nDocs-df+0.5)/(df+0.5))
			denom := f + b.k1*(1-b.b+b.b*float64(len(terms))/b.avgLen)
			score += idf * (f * (b.k1 + 1)) / denom
		}
		if score > 0 {
			results = append(results, bm25Result{Doc: d, Score: score})
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > n {
		results = results[:n]
	}
	return results
}

// IndexDir builds a BM25 index over all text files under dir (skipping heavy
// dirs). Reading whole files into memory is bounded: skip files over 2MB.
func IndexDir(dir string) ([]Document, error) {
	var docs []Document
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir && isHeavyDirName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if IsBinaryExt(ext) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() > 2*1024*1024 {
			return nil
		}
		// Never index sensitive files (.env, credentials, keys) — their
		// contents must not leak into search results.
		if isSensitiveName(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// Cap content per file to keep memory bounded on huge files.
		body := string(data)
		if len(body) > 200_000 {
			body = body[:200_000]
		}
		docs = append(docs, Document{ID: path, Title: d.Name(), Body: body})
		return nil
	})
	return docs, err
}

// isSensitiveName reports whether a file name marks secrets/credentials that
// must never be indexed (.env, .env.*, private keys, credential files).
func isSensitiveName(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, ".env") {
		return true
	}
	switch lower {
	case ".npmrc", ".pypirc", ".netrc", ".pgpass", ".htpasswd",
		"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa", "credentials.json",
		"service-account.json", "secrets.yaml", "secrets.yml",
		".git-credentials", ".dockercfg":
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".ppk", ".gpg", ".kdbx", ".ovpn"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func isHeavyDirName(name string) bool {
	switch name {
	case "node_modules", "bower_components", "jspm_packages", "vendor",
		"dist", "build", "out", "bin", "obj", "pkg", "Debug", "Release", "x64", "x86", "DerivedData",
		".git", ".svn", ".hg", ".bzr", ".CVS",
		".next", ".nuxt", ".svelte-kit", ".astro", ".docusaurus", ".vuepress", ".output",
		".parcel-cache", ".turbo", ".webpack", ".vite", ".rollup.cache", ".cache",
		"coverage", "htmlcov", "target", "__pycache__", ".pytest_cache", ".mypy_cache",
		".ruff_cache", ".tox", ".nox", ".venv", "venv", "env", ".conda", ".eggs", "pip-wheel-metadata",
		"Pods", "Carthage", ".dart_tool", ".pub-cache", ".pub",
		".gradle", ".m2", ".ivy2", ".sbt", ".cxx", ".bundle", "_build", ".elixir_ls",
		".brocode", ".idea", ".vscode", ".vs", ".settings", ".project", ".classpath", ".history",
		".terraform", ".terragrunt-cache", ".serverless", ".vagrant", ".pulumi", ".docker",
		"tmp", "temp", ".tmp", ".temp", ".yarn", ".pnpm-store":
		return true
	}
	return false
}

// FormatResults renders top search results with a snippet around the first
// query term match.
func FormatResults(results []bm25Result, query string) string {
	if len(results) == 0 {
		return "No relevant files found."
	}
	qTerms := tokenize(query)
	var sb strings.Builder
	for i, r := range results {
		snippet := r.Doc.Snippet
		if snippet == "" {
			snippet = snippetAround(r.Doc.Body, qTerms, 120)
		}
		sb.WriteString(fmt.Sprintf("%2d. [%.3f] %s", i+1, r.Score, r.Doc.ID))
		if snippet != "" {
			sb.WriteString("\n    " + strings.ReplaceAll(snippet, "\n", " "))
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

func snippetAround(body string, qTerms []string, width int) string {
	body = strings.Join(strings.Fields(body), " ")
	lower := strings.ToLower(body)
	idx := -1
	for _, t := range qTerms {
		if i := strings.Index(lower, t); i >= 0 && (idx == -1 || i < idx) {
			idx = i
		}
	}
	if idx < 0 {
		if len(body) <= width {
			return body
		}
		return body[:width] + "…"
	}
	start := idx - width/3
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(body) {
		end = len(body)
	}
	s := body[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(body) {
		s += "…"
	}
	return s
}
