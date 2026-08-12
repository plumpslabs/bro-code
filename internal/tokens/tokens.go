// Package tokens provides a calibrated FORECAST of token counts — the local
// estimate shown before a request, per doctrine P3 (tracking = source of
// truth). The industry-standard BPE tokenizers (tiktoken) were evaluated and
// rejected: every pure-Go port embeds vocabularies + a regex engine that add
// +5–14MB to the stripped binary (measured Aug 2026), blowing the project's
// footprint gate (binary 8.7MB → >13MB). The doctrine explicitly permits a
// heuristic forecast as long as it is calibrated and LABELED as an estimate;
// the exact numbers always come from the API response (settlement).
//
// Calibration: cl100k-style BPE runs ~4 chars/token on Latin text and ~1
// token per CJK ideograph. Code/markdown inflate counts slightly; the
// overhead is absorbed by the per-message role tax added by callers.
package tokens

import "unicode"

// Estimate returns the approximate token count of s under a cl100k-like BPE:
// CJK ideographs count ~1 token each, everything else ~4 chars per token.
// Always an estimate — label it "~" in any UI.
func Estimate(s string) int {
	if s == "" {
		return 0
	}
	cjk, rest := 0, 0
	for _, r := range []rune(s) {
		if isCJK(r) {
			cjk++
		} else {
			rest++
		}
	}
	return cjk + rest/4
}

// isCJK reports whether r is a CJK ideograph or kana — the character classes
// that occupy ~1 token per rune in cl100k-style BPE.
func isCJK(r rune) bool {
	switch {
	case r >= 0x2E80 && r <= 0x9FFF: // CJK radicals, symbols, unified ideographs
		return true
	case r >= 0x3040 && r <= 0x30FF: // hiragana, katakana
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // hangul syllables
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK compatibility ideographs
		return true
	default:
		return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)
	}
}
