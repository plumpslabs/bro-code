package tokens

// TurnTokenStats summarizes a single agent turn's token economy: how many
// tokens were spent producing the deliverable (answer + file mutations) versus
// how many were overhead (exploration that changed nothing). The ratio is
// BroCode's north-star efficiency metric — a high ratio means the agent went
// straight to the result instead of thrashing through the codebase.
type TurnTokenStats struct {
	// TotalTokens is every completion token consumed by the turn.
	TotalTokens int
	// ProductiveTokens is the subset that produced the deliverable: a final
	// answer, or a round that executed a file-mutating tool.
	ProductiveTokens int
	// WastedTokens is TotalTokens - ProductiveTokens (overhead/exploration).
	WastedTokens int
}

// NewTurnTokenStats builds stats from raw counters, deriving WastedTokens and
// clamping against negative or out-of-range inputs.
func NewTurnTokenStats(total, productive int) TurnTokenStats {
	if total < 0 {
		total = 0
	}
	if productive < 0 {
		productive = 0
	}
	if productive > total {
		productive = total
	}
	return TurnTokenStats{
		TotalTokens:      total,
		ProductiveTokens: productive,
		WastedTokens:     total - productive,
	}
}

// Ratio returns ProductiveTokens / TotalTokens in [0,1]. It returns 1.0 when
// there were no tokens (nothing wasted, nothing to measure).
func (s TurnTokenStats) Ratio() float64 {
	if s.TotalTokens <= 0 {
		return 1.0
	}
	return float64(s.ProductiveTokens) / float64(s.TotalTokens)
}

// Percent returns Ratio as a whole-number percentage (0–100).
func (s TurnTokenStats) Percent() int {
	return int(s.Ratio() * 100)
}
