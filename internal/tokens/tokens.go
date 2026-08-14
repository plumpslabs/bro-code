// Package tokens estimates LLM token counts cheaply and deterministically.
//
// It lives in its own leaf package so both the context manager (compaction
// thresholds) and the provider adapters (cost tracking) can share one
// estimator without an import cycle.
package tokens

import "strings"

// EstimateTokens approximates LLM token counts more accurately than a flat
// len/4 guess: prose ≈4 chars/token, code ≈3.5, CJK/Asian text ≈1.2 (each
// character is often its own token). The estimate is weighted per line so
// mixed content (code + prose + Asian replies) lands closer to real
// tokenizer counts, keeping thresholds honest instead of firing too early
// or overflowing late.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}

	lines := strings.Split(text, "\n")
	total := 0
	for _, line := range lines {
		if line == "" {
			total++
			continue
		}

		ascii := 0
		cjk := 0
		for _, r := range line {
			if r > 0x2E7F && r < 0x9FFF { // CJK unified ideographs & compat
				cjk++
			} else {
				ascii++
			}
		}

		trimmed := strings.TrimSpace(line)
		isCode := strings.ContainsAny(trimmed, "{}();=\"'") || strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "const ")

		charsPerToken := 4.0 // prose default
		if isCode {
			charsPerToken = 3.5 // code packs more tokens per char
		}

		lineTokens := float64(ascii)/charsPerToken + float64(cjk)*0.8 // CJK ≈ 1.25 chars/token
		if lineTokens < 1 {
			lineTokens = 1
		}
		total += int(lineTokens)
	}
	return total
}
