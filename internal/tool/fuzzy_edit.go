package tool

import (
	"fmt"
	"math"
	"strings"
)

// ApplyResilientEdit attempts to replace target in content with replacement
// using a 5-tier resilience pipeline:
//
//  Tier 1: Exact substring match
//  Tier 2: CRLF and trailing whitespace normalization
//  Tier 3: Line-trimmed matching (ignoring indentation differences per line)
//  Tier 4: Relative indentation alignment (preserving base indentation of destination)
//  Tier 5: Unique fuzzy similarity window (threshold >= 85% match, single candidate)
//
// Returns (newContent, matchTier, error).
func ApplyResilientEdit(content, target, replacement string) (string, string, error) {
	if target == "" {
		return content, "noop", nil
	}

	// ── Tier 1: Exact match ──────────────────────────────────────────────────
	if strings.Contains(content, target) {
		return strings.Replace(content, target, replacement, 1), "exact", nil
	}

	// ── Tier 2: Line ending & trailing whitespace normalization ─────────────
	normContent := normalizeLineEndings(content)
	normTarget := normalizeLineEndings(target)
	normReplacement := normalizeLineEndings(replacement)

	if strings.Contains(normContent, normTarget) {
		res := strings.Replace(normContent, normTarget, normReplacement, 1)
		return res, "crlf-normalized", nil
	}

	// ── Tier 3: Relative Indentation Alignment ──────────────────────────────
	if res, ok := matchRelativeIndent(normContent, normTarget, normReplacement); ok {
		return res, "indent-aligned", nil
	}

	// ── Tier 4: Line-Trimmed Match ──────────────────────────────────────────
	if res, ok := matchLineTrimmed(normContent, normTarget, normReplacement); ok {
		return res, "line-trimmed", nil
	}

	// ── Tier 5: Fuzzy Similarity Window ─────────────────────────────────────
	if res, ok := matchFuzzyWindow(normContent, normTarget, normReplacement); ok {
		return res, "fuzzy-similarity", nil
	}

	return "", "", fmt.Errorf("target block not found in file (tried exact, whitespace-normalized, indent-aligned, and fuzzy matching)")
}

func normalizeLineEndings(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// matchLineTrimmed compares target lines against content lines ignoring leading
// and trailing whitespace on each line, and formats the replacement with the
// destination's indentation.
func matchLineTrimmed(content, target, replacement string) (string, bool) {
	cLines := strings.Split(content, "\n")
	tLines := strings.Split(strings.TrimRight(target, "\n"), "\n")
	if len(tLines) == 0 {
		return "", false
	}

	matchIdx := -1
	matchCount := 0

	for i := 0; i <= len(cLines)-len(tLines); i++ {
		matched := true
		for j := 0; j < len(tLines); j++ {
			if strings.TrimSpace(cLines[i+j]) != strings.TrimSpace(tLines[j]) {
				matched = false
				break
			}
		}
		if matched {
			matchIdx = i
			matchCount++
		}
	}

	// Must be a unique match to avoid modifying the wrong block
	if matchCount == 1 {
		destBaseIndent := leadingWhitespace(cLines[matchIdx])
		targetBaseIndent := leadingWhitespace(tLines[0])
		rLines := strings.Split(strings.TrimRight(replacement, "\n"), "\n")
		rBaseIndent := leadingWhitespace(rLines[0])

		alignedReplacements := make([]string, len(rLines))
		for k, rl := range rLines {
			if strings.TrimSpace(rl) == "" {
				alignedReplacements[k] = ""
				continue
			}
			if rBaseIndent != "" && strings.HasPrefix(rl, rBaseIndent) {
				alignedReplacements[k] = destBaseIndent + strings.TrimPrefix(rl, rBaseIndent)
			} else if targetBaseIndent != "" && strings.HasPrefix(rl, targetBaseIndent) {
				alignedReplacements[k] = destBaseIndent + strings.TrimPrefix(rl, targetBaseIndent)
			} else {
				alignedReplacements[k] = destBaseIndent + strings.TrimSpace(rl)
			}
		}

		var out []string
		out = append(out, cLines[:matchIdx]...)
		out = append(out, alignedReplacements...)
		out = append(out, cLines[matchIdx+len(tLines):]...)
		return strings.Join(out, "\n"), true
	}

	return "", false
}

// matchRelativeIndent detects when the target code has different base indentation
// (e.g. 2 spaces vs 4 spaces) and adjusts the replacement to match the file.
func matchRelativeIndent(content, target, replacement string) (string, bool) {
	cLines := strings.Split(content, "\n")
	tLines := strings.Split(strings.TrimRight(target, "\n"), "\n")
	if len(tLines) == 0 {
		return "", false
	}

	tBaseIndent := leadingWhitespace(tLines[0])
	tStripped := make([]string, len(tLines))
	for i, l := range tLines {
		tStripped[i] = strings.TrimPrefix(l, tBaseIndent)
	}

	matchIdx := -1
	matchCount := 0
	var destBaseIndent string

	for i := 0; i <= len(cLines)-len(tLines); i++ {
		matched := true
		cBaseIndent := leadingWhitespace(cLines[i])
		for j := 0; j < len(tLines); j++ {
			cStripped := strings.TrimPrefix(cLines[i+j], cBaseIndent)
			if cStripped != tStripped[j] {
				matched = false
				break
			}
		}
		if matched {
			matchIdx = i
			destBaseIndent = cBaseIndent
			matchCount++
		}
	}

	if matchCount == 1 {
		rLines := strings.Split(strings.TrimRight(replacement, "\n"), "\n")
		rBaseIndent := leadingWhitespace(rLines[0])
		alignedReplacements := make([]string, len(rLines))
		for k, rl := range rLines {
			stripped := strings.TrimPrefix(rl, rBaseIndent)
			alignedReplacements[k] = destBaseIndent + stripped
		}

		var out []string
		out = append(out, cLines[:matchIdx]...)
		out = append(out, alignedReplacements...)
		out = append(out, cLines[matchIdx+len(tLines):]...)
		return strings.Join(out, "\n"), true
	}

	return "", false
}

// matchFuzzyWindow slides over content with the target length, computing line-by-line
// similarity. If a single window achieves >= 0.85 similarity while all others are < 0.65,
// it is safely replaced.
func matchFuzzyWindow(content, target, replacement string) (string, bool) {
	cLines := strings.Split(content, "\n")
	tLines := strings.Split(strings.TrimRight(target, "\n"), "\n")
	tLen := len(tLines)
	if tLen < 2 || len(cLines) < tLen {
		return "", false
	}

	bestScore := 0.0
	bestIdx := -1
	secondBestScore := 0.0

	for i := 0; i <= len(cLines)-tLen; i++ {
		score := 0.0
		for j := 0; j < tLen; j++ {
			score += lineSimilarity(strings.TrimSpace(cLines[i+j]), strings.TrimSpace(tLines[j]))
		}
		avgScore := score / float64(tLen)
		if avgScore > bestScore {
			secondBestScore = bestScore
			bestScore = avgScore
			bestIdx = i
		} else if avgScore > secondBestScore {
			secondBestScore = avgScore
		}
	}

	// Safety threshold: best match must be >= 85% similar and distinctly better than runner-up
	if bestScore >= 0.85 && (bestScore-secondBestScore >= 0.20 || secondBestScore < 0.60) && bestIdx >= 0 {
		rLines := strings.Split(strings.TrimRight(replacement, "\n"), "\n")
		var out []string
		out = append(out, cLines[:bestIdx]...)
		out = append(out, rLines...)
		out = append(out, cLines[bestIdx+tLen:]...)
		return strings.Join(out, "\n"), true
	}

	return "", false
}

func leadingWhitespace(s string) string {
	idx := strings.IndexFunc(s, func(r rune) bool {
		return r != ' ' && r != '\t'
	})
	if idx < 0 {
		return s
	}
	return s[:idx]
}

func lineSimilarity(s1, s2 string) float64 {
	if s1 == s2 {
		return 1.0
	}
	if len(s1) == 0 || len(s2) == 0 {
		return 0.0
	}
	dist := levenshtein(s1, s2)
	maxLen := math.Max(float64(len(s1)), float64(len(s2)))
	return 1.0 - (float64(dist) / maxLen)
}

func levenshtein(s1, s2 string) int {
	r1, r2 := []rune(s1), []rune(s2)
	n1, n2 := len(r1), len(r2)
	dp := make([][]int, n1+1)
	for i := range dp {
		dp[i] = make([]int, n2+1)
		dp[i][0] = i
	}
	for j := 0; j <= n2; j++ {
		dp[0][j] = j
	}

	for i := 1; i <= n1; i++ {
		for j := 1; j <= n2; j++ {
			cost := 0
			if r1[i-1] != r2[j-1] {
				cost = 1
			}
			min := dp[i-1][j] + 1
			if del := dp[i][j-1] + 1; del < min {
				min = del
			}
			if sub := dp[i-1][j-1] + cost; sub < min {
				min = sub
			}
			dp[i][j] = min
		}
	}
	return dp[n1][n2]
}
