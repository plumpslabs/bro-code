package tokens

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		minTok int
		maxTok int
	}{
		{
			name:   "Empty string",
			input:  "",
			minTok: 0,
			maxTok: 0,
		},
		{
			name:   "Simple English sentence",
			input:  "The quick brown fox jumps over the lazy dog.",
			minTok: 8,
			maxTok: 14,
		},
		{
			name: "Go function with CamelCase and operators",
			input: `func CalculateFinalUserBalance(accountID string, deltaAmount float64) (float64, error) {
	if deltaAmount == 0.0 {
		return 0.0, nil
	}
	return deltaAmount * 1.05, nil
}`,
			minTok: 25,
			maxTok: 50,
		},
		{
			name:   "CJK text",
			input:  "你好世界，这是一个测试",
			minTok: 8,
			maxTok: 16,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateTokens(tc.input)
			if got < tc.minTok || got > tc.maxTok {
				t.Errorf("EstimateTokens(%q) = %d, want between [%d, %d]", tc.input, got, tc.minTok, tc.maxTok)
			}
		})
	}
}

// TestEstimateTokensForModelDeepSeek pins the model-aware DeepSeek calibration
// to the official documented ratios (api-docs.deepseek.com/quick_start/
// token_usage): 1 English char ≈ 0.3 token, 1 Chinese char ≈ 0.6 token.
func TestEstimateTokensForModelDeepSeek(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		minTok int
		maxTok int
	}{
		{
			name:   "English prose ≈ 0.3 token/char",
			text:   strings.Repeat("hello world ", 20), // 240 chars, no newlines
			minTok: 60,
			maxTok: 85,
		},
		{
			name:   "Chinese ≈ 0.6 token/char",
			text:   strings.Repeat("你好世界", 10), // 40 CJK chars
			minTok: 18,
			maxTok: 30,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateTokensForModel(tc.text, "deepseek-v4-flash")
			if got < tc.minTok || got > tc.maxTok {
				t.Errorf("EstimateTokensForModel(deepseek, %q) = %d, want between [%d, %d]", tc.text, got, tc.minTok, tc.maxTok)
			}
			// CJK: the generic heuristic (0.85 tok/char) must NOT be used for
			// the deepseek family — the model-aware path is strictly cheaper.
			gotGeneric := EstimateTokens(tc.text)
			if strings.ContainsRune(tc.text, '你') && got >= gotGeneric {
				t.Errorf("deepseek CJK estimate %d must be below generic estimate %d", got, gotGeneric)
			}
		})
	}
	// Unknown models fall back to the generic heuristic unchanged.
	if got := EstimateTokensForModel("some-random-model", "some-random-model"); got != EstimateTokens("some-random-model") {
		t.Errorf("unknown model must fall back to EstimateTokens, got %d", got)
	}
}
