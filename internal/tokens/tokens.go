// Package tokens estimates LLM token counts cheaply and deterministically.
//
// It lives in its own leaf package so both the context manager (compaction
// thresholds) and the provider adapters (cost tracking) can share one
// estimator without an import cycle.
package tokens

import "strings"

// EstimateTokens approximates LLM token counts when the exact BPE encoder
// is not available. Calibrated: code ≈3.2-3.8 chars/tok, English prose ≈4 chars/tok,
// CJK ≈1.2 chars/tok.
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
			if r > 0x2E7F && r < 0x9FFF {
				cjk++
			} else {
				ascii++
			}
		}

		trimmed := strings.TrimSpace(line)
		isCode := strings.ContainsAny(trimmed, "{}();=\"'") || strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "const ") || strings.HasPrefix(trimmed, "var ")

		charsPerToken := 4.0
		if isCode {
			charsPerToken = 3.3
		}

		lineTokens := float64(ascii)/charsPerToken + float64(cjk)*0.85
		if lineTokens < 1 {
			lineTokens = 1
		}
		total += int(lineTokens)
	}
	return total
}
