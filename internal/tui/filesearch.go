package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/plumpslabs/bro-code/internal/search"
)

// ── Native workspace file search ──────────────────────────────────────────
// The agent's `search` tool replaces the "grep via bash" habit: one indexed
// lookup returns the top relevant FILE PATHS instead of a full LLM round-trip
// per query. Bounded by design (Principle 1): at most maxFileSearchFiles files
// of at most maxFileSearchSize bytes each, ignored dirs skipped.
//
// Freshness without full rebuilds: the index keeps a size+mtime snapshot of
// every indexed file. Each lookup does a STAT-ONLY walk (no content reads)
// and surgically Update/Remove ONLY the files that changed, were added, or
// were deleted — unchanged content is never re-read or re-tokenized.

const (
	// maxFileSearchFiles bounds the index. 300 was sized for a single small
	// project, but a monorepo (2k+ files) needs headroom so per-dir quotas can
	// actually reach every subproject's src/ before the budget runs out.
	// Indexed files are read once (stat-only refresh after), so the cost of a
	// bigger budget is a one-time scan — bounded by the size cap below.
	maxFileSearchFiles = 1000     // index at most this many files
	maxFileSearchSize  = 64 << 10 // skip bundles/minified/large files (>64 KB)
	fileSearchTopK     = 10       // results per query (bounded feedback)
)

// fileStamp identifies a file's on-disk state for change detection. mtime is
// compared at nanosecond precision — a rebuild then an edit within the same
// nanosecond is impossible for a real filesystem write.
type fileStamp struct {
	size  int64
	mtime int64 // unix nanos
}

var (
	fileIndexMu      sync.Mutex // agent goroutines + UI thread both call fileSearch
	fileIndexDir     string
	fileIndexReady   bool
	fileIndex        *search.Index
	fileIndexStamps  map[string]fileStamp
	fileIndexChecked time.Time
)

// fileIndexRefreshMin is the cooldown between stat-walks. Search calls are
// LLM-paced (seconds apart), so a 250ms window only skips redundant refreshes
// during parallel fan-out of several search calls in one reply.
const fileIndexRefreshMin = 250 * time.Millisecond

// fileSearch returns the top-K most relevant workspace files for q, refreshing
// the index first if any file changed. Read-only and goroutine-safe.
func fileSearch(q string, topK int) []search.Result {
	if topK <= 0 {
		topK = fileSearchTopK
	}
	ix := cachedFileIndex()
	if ix == nil {
		return nil
	}
	return ix.Search(q, topK)
}

// cachedFileIndex returns the workspace index, rebuilding it when the working
// directory changes and refreshing it incrementally when indexed files change
// (mtime/size drift), were added, or were removed.
func cachedFileIndex() *search.Index {
	wd, _ := os.Getwd()
	fileIndexMu.Lock()
	defer fileIndexMu.Unlock()
	if fileIndexDir != wd || !fileIndexReady {
		rebuildFileIndex(wd)
		return fileIndex
	}
	// Cooldown: stamps were verified recently — skip the stat walk.
	if time.Since(fileIndexChecked) < fileIndexRefreshMin {
		return fileIndex
	}
	refreshFileIndex(wd)
	return fileIndex
}

// rebuildFileIndex does the full initial build (or a directory change): walk,
// read, index, snapshot.
func rebuildFileIndex(wd string) {
	fileIndexDir = wd
	fileIndexReady = true
	stamps := workspaceFiles(wd)
	docs := make([]search.Document, 0, len(stamps))
	for id := range stamps {
		data, err := os.ReadFile(filepath.Join(wd, id))
		if err != nil {
			continue
		}
		content := string(data)
		if strings.ContainsRune(content, '\x00') {
			continue // binary — never a searchable source file
		}
		docs = append(docs, search.Document{
			ID:      id,
			Title:   filepath.Base(id),
			Path:    id, // index the path so directory names rank matches (e.g. "lead-rotation")
			Body:    content,
			Snippet: fileSnippet(content),
		})
	}
	// Map iteration is random — sort so the doc order (and any tie-breaking on
	// docIDs) is deterministic across builds.
	sort.Slice(docs, func(a, b int) bool { return docs[a].ID < docs[b].ID })
	fileIndex = search.New(docs)
	fileIndexStamps = stamps
	fileIndexChecked = time.Now()
}

// refreshFileIndex reconciles the index with the current workspace WITHOUT a
// full rebuild: a stat-only walk collects the current file set, then only
// changed/new files are re-read and updated surgically and removed files are
// dropped. Unchanged content is never re-read.
func refreshFileIndex(wd string) {
	current := workspaceFiles(wd)
	// Removed files (including ones that grew past the size cap and vanished
	// from `current`): drop them from the index.
	for id := range fileIndexStamps {
		if _, ok := current[id]; !ok {
			fileIndex.Remove(id)
		}
	}
	// Changed + new files: read only these and upsert them.
	for id, st := range current {
		old, ok := fileIndexStamps[id]
		if ok && old == st {
			continue // unchanged — nothing to do
		}
		data, err := os.ReadFile(filepath.Join(wd, id))
		if err != nil {
			continue
		}
		content := string(data)
		if strings.ContainsRune(content, '\x00') {
			continue // became binary — leave it out of the index
		}
		fileIndex.Update(id, search.Document{
			ID:      id,
			Title:   filepath.Base(id),
			Path:    id, // index the path so directory names rank matches (e.g. "lead-rotation")
			Body:    content,
			Snippet: fileSnippet(content),
		})
		fileIndexStamps[id] = st
	}
	fileIndexChecked = time.Now()
}

// workspaceFiles returns the bounded set of indexable workspace files with
// their size+mtime stamps. STAT-ONLY (no content reads) — shared by the
// initial build and every incremental refresh, so the two can never disagree
// about what is searchable. Uses the same ignore rules as the keyword
// auto-search (shouldIgnorePath).
//
// The set is collected and PRIORITIZED rather than cut mid-walk: a plain
// alphabetical walk would stop after maxFileSearchFiles entries, and in a
// monorepo the tooling/config dotfiles (.agents/, .opencode/, .kuma/…) sort
// FIRST and eat the whole budget — the source dirs (crm_sales_backend/…)
// never get indexed, so "rotation" returns nothing real (observed: the
// LineShape-only results that mislead the agent). Collect all candidates,
// then source files rank above config/dotfiles before the cap is applied.
// indexCandidate is one file found during the stat walk, before the bounded
// index budget is applied. src marks source-code-ish paths (they outrank
// config/dotfiles); depth orders shallow files first for stable tie-breaks.
type indexCandidate struct {
	rel   string
	st    fileStamp
	src   bool
	depth int
}

func workspaceFiles(wd string) map[string]fileStamp {
	var cands []indexCandidate
	_ = filepath.Walk(wd, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(wd, path)
		if rerr != nil {
			rel = path
		}
		if shouldIgnorePath(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if info.Size() > maxFileSearchSize {
			return nil
		}
		cands = append(cands, indexCandidate{rel: rel, st: fileStamp{size: info.Size(), mtime: info.ModTime().UnixNano()}, src: isSourcePath(rel), depth: strings.Count(rel, string(filepath.Separator))})
		return nil
	})
	// Source-code paths first (deep-first within code dirs), then the rest.
	// Within source files, /src/ code outranks tests/scripts/config so the
	// index budget never gets spent on test files before the implementation
	// (observed: crm_sales_backend's scripts/ + tests/ filled the quota and
	// src/services/lead-rotation/LeadRotationService.js was never indexed).
	// Deterministic tie-break: shorter (shallower) path wins.
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].src != cands[b].src {
			return cands[a].src
		}
		if isCoreSource(cands[a].rel) != isCoreSource(cands[b].rel) {
			return isCoreSource(cands[a].rel)
		}
		if cands[a].depth != cands[b].depth {
			return cands[a].depth < cands[b].depth
		}
		return cands[a].rel < cands[b].rel
	})
	// Monorepo balance: one big subproject (a frontend with thousands of
	// files) must not starve the others. Allocate the index budget per
	// top-level directory PROPORTIONALLY to each dir's source-file count, so
	// crm_sales_backend (few hundred files, but the code the agent queries
	// for) gets a fair share next to a 10k-file frontend. Root-level files
	// (Makefile, .gitignore) are included up-front without quota.
	stamps := make(map[string]fileStamp, min(len(cands), maxFileSearchFiles))
	srcCount := map[string]int{} // top-level dir -> source-file count
	total := 0
	for _, c := range cands {
		if !c.src {
			continue
		}
		total++
		if t := topLevel(c.rel); t != "" {
			srcCount[t]++
		}
	}
	used := map[string]int{}
	for _, c := range cands {
		if len(stamps) >= maxFileSearchFiles {
			break
		}
		top := topLevel(c.rel)
		if top == "" {
			stamps[c.rel] = c.st // root-level file (Makefile, README) — always in
			continue
		}
		// Proportional quota: share of the budget that this dir's source files
		// represent (min 1 so every code dir gets at least one file).
		quota := max(1, maxFileSearchFiles*srcCount[top]/max(1, total))
		if used[top] >= quota {
			continue
		}
		used[top]++
		stamps[c.rel] = c.st
	}
	return stamps
}

// topLevel returns the first path component of a relative path ("" for
// files at the repo root, e.g. Makefile or .gitignore).
func topLevel(rel string) string {
	if i := strings.Index(rel, string(filepath.Separator)); i > 0 {
		return rel[:i]
	}
	return ""
}

// topLevelDirs returns the distinct top-level directory names among the
// candidate files — used to size the per-directory index quota.
func topLevelDirs(cands []indexCandidate) []string {
	set := map[string]bool{}
	for _, c := range cands {
		if t := topLevel(c.rel); t != "" {
			set[t] = true
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// isCoreSource reports whether a source path lives under a source root
// (src/, lib/, app/, internal/) — the implementation, as opposed to tests/
// scripts/ config at the project root. Core source outranks tests/scripts so
// the bounded index serves real code first.
func isCoreSource(rel string) bool {
	parts := strings.Split(rel, string(filepath.Separator))
	for _, p := range parts {
		switch p {
		case "src", "lib", "app", "internal", "services", "controllers", "routes":
			return true
		}
	}
	return false
}

// isSourcePath reports whether a path looks like source code (vs config,
// lockfiles, dotfiles, docs). Source files get indexing priority so the
// bounded index is never starved by tooling/config noise in a monorepo.
func isSourcePath(rel string) bool {
	base := rel
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		base = rel[i+1:]
	}
	if strings.HasPrefix(base, ".") {
		return false // dotfiles (.env, .gitignore, …)
	}
	if strings.Contains(rel, "/docs/") || strings.HasPrefix(rel, "docs/") ||
		strings.Contains(rel, "/storage/") || strings.HasPrefix(rel, "storage/") {
		return false
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(base), "."))
	switch ext {
	case "go", "js", "jsx", "ts", "tsx", "py", "rb", "php", "java", "kt", "rs",
		"c", "h", "cpp", "hpp", "cs", "swift", "vue", "svelte", "sql", "prisma":
		return true
	}
	// Extensionless but clearly a source script (Makefile, Dockerfile, …).
	if strings.HasPrefix(base, "Makefile") || strings.HasPrefix(base, "Dockerfile") ||
		base == "go.mod" || base == "package.json" || base == "Cargo.toml" ||
		base == "pyproject.toml" || base == "composer.json" {
		return true
	}
	return false
}

// fileSnippet returns the first substantive line of a file as a one-line
// preview — skipping the leading declaration run (package/module/import/etc.)
// so results show "// handles the refresh flow" instead of a useless
// "package auth". Bounded to 80 runes.
func fileSnippet(content string) string {
	lines := strings.Split(content, "\n")
	// Skip a leading declaration run (package/module/import/…) and pass the
	// remaining lines to the first-non-empty scan — otherwise "package auth"
	// would win over a real comment like "// handles the refresh flow".
	start := 0
	for i := 0; i < len(lines) && i < 3; i++ {
		t := strings.ToLower(strings.TrimSpace(lines[i]))
		if strings.HasPrefix(t, "package ") || strings.HasPrefix(t, "module ") ||
			strings.HasPrefix(t, "import ") || strings.HasPrefix(t, "import(") ||
			strings.HasPrefix(t, "#include") || strings.HasPrefix(t, "#!") ||
			strings.HasPrefix(t, "<?php") || strings.HasPrefix(t, "from ") ||
			strings.HasPrefix(t, "using ") {
			start = i + 1
			continue
		}
		break
	}
	return firstNonEmptyLine(lines[start:])
}

// firstNonEmptyLine returns the first non-blank line of a file, clipped to 80
// runes ("" when the file is blank).
func firstNonEmptyLine(lines []string) string {
	for _, ln := range lines {
		if t := strings.TrimSpace(ln); t != "" {
			if r := []rune(t); len(r) > 80 {
				return string(r[:80]) + "…"
			}
			return t
		}
	}
	return ""
}
