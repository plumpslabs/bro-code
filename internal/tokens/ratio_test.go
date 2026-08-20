package tokens

import "testing"

func TestNewTurnTokenStats(t *testing.T) {
	tests := []struct {
		name       string
		total      int
		productive int
		wantTotal  int
		wantProd   int
		wantWaste  int
	}{
		{"balanced", 1000, 700, 1000, 700, 300},
		{"all productive", 500, 500, 500, 500, 0},
		{"all wasted", 500, 0, 500, 0, 500},
		{"productive exceeds total clamps", 100, 200, 100, 100, 0},
		{"negative total clamps", -5, 10, 0, 0, 0},
		{"zero tokens", 0, 0, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewTurnTokenStats(tt.total, tt.productive)
			if s.TotalTokens != tt.wantTotal {
				t.Errorf("TotalTokens = %d, want %d", s.TotalTokens, tt.wantTotal)
			}
			if s.ProductiveTokens != tt.wantProd {
				t.Errorf("ProductiveTokens = %d, want %d", s.ProductiveTokens, tt.wantProd)
			}
			if s.WastedTokens != tt.wantWaste {
				t.Errorf("WastedTokens = %d, want %d", s.WastedTokens, tt.wantWaste)
			}
		})
	}
}

func TestTurnTokenStatsRatio(t *testing.T) {
	tests := []struct {
		name       string
		total      int
		productive int
		wantRatio  float64
		wantPct    int
	}{
		{"perfect", 1000, 1000, 1.0, 100},
		{"seventy percent", 1000, 700, 0.7, 70},
		{"zero", 0, 0, 1.0, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewTurnTokenStats(tt.total, tt.productive)
			if s.Ratio() != tt.wantRatio {
				t.Errorf("Ratio() = %v, want %v", s.Ratio(), tt.wantRatio)
			}
			if s.Percent() != tt.wantPct {
				t.Errorf("Percent() = %d, want %d", s.Percent(), tt.wantPct)
			}
		})
	}
}
