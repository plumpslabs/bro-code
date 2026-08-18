package tokens

import (
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		minTok  int
		maxTok  int
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
