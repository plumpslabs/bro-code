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
	return estimateWithRates(text, 3.3, 4.0, 0.85)
}

// EstimateTokensForModel is EstimateTokens with model-family calibration.
// Model families with an officially documented character→token ratio use it
// instead of the generic heuristic; unknown models fall back to EstimateTokens.
//
// DeepSeek (official docs, api-docs.deepseek.com/quick_start/token_usage):
// 1 English char ≈ 0.3 token (≈3.33 chars/token), 1 Chinese char ≈ 0.6 token.
// The generic heuristic (4 chars/tok English, 0.85 tok/char CJK) overestimates
// DeepSeek CJK by ~40%, so the official ratios are applied for the family.
func EstimateTokensForModel(text, model string) int {
	if strings.HasPrefix(model, "deepseek-") || strings.HasPrefix(model, "deepseek/") {
		return estimateWithRates(text, 10.0/3.0, 10.0/3.0, 0.6)
	}
	return EstimateTokens(text)
}

// estimateWithRates is the shared counting loop. Newlines count as one token
// each; a line of ascii uses charsPerToken (code lines use codeCharsPerToken)
// and each CJK rune contributes tokensPerChar. A non-empty line always counts
// at least one token.
func estimateWithRates(text string, codeCharsPerToken, proseCharsPerToken, tokensPerChar float64) int {
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

		charsPerToken := proseCharsPerToken
		if isCode {
			charsPerToken = codeCharsPerToken
		}

		lineTokens := float64(ascii)/charsPerToken + float64(cjk)*tokensPerChar
		if lineTokens < 1 {
			lineTokens = 1
		}
		total += int(lineTokens)
	}
	return total
}
