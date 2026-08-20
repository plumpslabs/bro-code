// Package scope provides smart scope pre-selection: given a user prompt,
// it quickly identifies which files/directories are most relevant so BroCode
// can focus its exploration instead of scanning the entire workspace.
package repo

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// keywordScope maps prompt keywords to file path patterns and directory names.
// When a user prompt contains a keyword, files matching these patterns are
// boosted in relevance. This is intentionally conservative — false positives
// are filtered out later by the relevance scorer.
var keywordScope = map[string][]string{
	"login":  {"login", "auth", "signin", "sign_in", "oauth", "session"},
	"logout": {"logout", "auth", "session", "signout"},
	"register": {"register", "signup", "sign_up"},
	"password": {"password", "auth", "credential", "reset", "forgot"},
	"auth":  {"auth", "guard", "middleware", "protected", "permission"},
	"audit": {}, // handled by file-size/error heuristics
	"delete": {},
	"create": {},
	"edit":  {},
	"test": {"test", "_test", "spec", "tests"},
	"config": {"config", "setting", "env", ".yml", ".yaml", ".toml", ".json"},
	"database": {"db", "database", "migration", "schema", "model"},
	"api": {"api", "route", "handler", "endpoint", "controller"},
	"ui": {"component", "page", "view", "screen", "element"},
	"style": {"style", "css", "scss", "theme", "color"},
	"deploy": {"deploy", "docker", "kubernetes", "helm", "ci", "cd"},
	"error": {"error", "exception", "catch", "fail", "panic"},
	"cache": {"cache", "redis", "memcache"},
}

// RelevanceResult is a file path with a 0-1 relevance score to the prompt.
type RelevanceResult struct {
	Path     string
	Score    float64
	Reason   string // human-readable reason for the score
}

// ScoreFiles ranks files by relevance to a user prompt keyword.
// Uses filename matching, directory path matching, and file extension hints.
// Returns top `limit` results sorted by score descending.
func ScoreFiles(files []string, prompt string, limit int) []RelevanceResult {
	keywords := extractKeywords(prompt)
	if len(keywords) == 0 || len(files) == 0 {
		return nil
	}

	type scoredFile struct {
		path  string
		score float64
		reason string
	}
	scored := make([]scoredFile, 0, len(files))

	for _, f := range files {
		score, reason := scoreFile(f, keywords)
		if score > 0 {
			scored = append(scored, scoredFile{f, score, reason})
		}
	}

	// Sort by score descending.
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	if limit > 0 && limit < len(scored) {
		scored = scored[:limit]
	}

	out := make([]RelevanceResult, len(scored))
	for i, s := range scored {
		out[i] = RelevanceResult{Path: s.path, Score: s.score, Reason: s.reason}
	}
	return out
}

// extractKeywords pulls significant tokens from the prompt and maps them
// to scope keywords.
func extractKeywords(prompt string) []string {
	lower := strings.ToLower(prompt)
	var kws []string
	for _, kw := range allKeywords(keywordScope) {
		if strings.Contains(lower, kw) {
			kws = append(kws, kw)
		}
	}
	return kws
}

func allKeywords(m map[string][]string) []string {
	seen := map[string]bool{}
	for k := range m {
		if k != "" && !seen[k] {
			seen[k] = true
		}
	}
	for _, aliases := range m {
		for _, a := range aliases {
			if a != "" && !seen[a] {
				seen[a] = true
			}
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// scoreFile returns a 0-1 relevance score for a file given keywords.
func scoreFile(f string, keywords []string) (float64, string) {
	lower := strings.ToLower(f)

	var score float64
	var reason string

	// Filename direct match = highest weight.
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			s := 0.6
			score += s
			if score > 0 {
				reason = "filename matches: " + kw
			}
		}
	}

	// Directory name match = medium weight.
	dir := filepath.Dir(f)
	for _, kw := range keywords {
		if strings.Contains(strings.ToLower(dir), kw) {
			s := 0.3
			score += s
			reason = appendReason(reason, "directory: "+kw)
		}
	}

	// Extension hint match.
	for _, kw := range keywords {
		aliases, ok := keywordScope[kw]
		if !ok {
			continue
		}
		for _, alias := range aliases {
			if strings.HasSuffix(lower, "."+alias) {
				s := 0.2
				score += s
				reason = appendReason(reason, "ext: "+alias)
			}
		}
	}

	// Hot-file boost: files in the repo map's hot-files list.
	// (Handled caller-side — we just return the score.)

	// Normalize to 0-1.
	if score > 1.0 {
		score = 1.0
	}
	return score, reason
}

func appendReason(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + ", " + add
}

// SuggestFocusDirs returns the directories that contain the highest-scoring
// files, as a suggestion for BroCode to focus exploration. Returns at most 5.
func SuggestFocusDirs(results []RelevanceResult) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, r := range results {
		d := filepath.Dir(r.Path)
		if !seen[d] && d != "." && len(dirs) < 5 {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// SummarizeScope produces a short markdown summary of which files/dirs
// BroCode should focus on, for injection into the system prompt.
func SummarizeScope(results []RelevanceResult, prompt string) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("🎯 **SMART SCOPE**: The user prompt contains keywords that suggest focus on specific areas. Prioritize these files (ranked by relevance):\n")
	limit := 8
	if len(results) > limit {
		results = results[:limit]
	}
	for i, r := range results {
		short := r.Path
		if i < len(results) {
			sb.WriteString(fmt.Sprintf("%.1f. `%s` (%.0f%% relevant — %s)\n",
				r.Score*100, short, r.Score*100, r.Reason))
		}
	}
	if dirs := SuggestFocusDirs(results); len(dirs) > 0 {
		sb.WriteString("\nFocus exploration under: `" + strings.Join(dirs, "`, `") + "`")
	}
	return sb.String()
}
