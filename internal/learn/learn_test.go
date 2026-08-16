package learn

import (
	"path/filepath"
	"testing"
)

func tmpPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "learn.json")
}

func TestNewLearnerDefaults(t *testing.T) {
	l := NewLearner(tmpPath(t))
	if got := l.CompactionRatio(); got != defaultRatio {
		t.Fatalf("default ratio = %v, want %v", got, defaultRatio)
	}
}

func TestObserveTurnNudgesDownWhenHot(t *testing.T) {
	l := NewLearner(tmpPath(t))
	start := l.CompactionRatio()
	// Simulate a window running hot (utilization above the target band).
	for range 6 {
		l.ObserveTurn(0.95)
	}
	if got := l.CompactionRatio(); got >= start {
		t.Fatalf("hot utilization should lower the ratio: start=%v got=%v", start, got)
	}
	if got := l.CompactionRatio(); got < ratioMin {
		t.Fatalf("ratio fell below floor: %v", got)
	}
}

func TestObserveTurnNudgesUpWhenCold(t *testing.T) {
	l := NewLearner(tmpPath(t))
	// Push the ratio down first, then feed cold (low-utilization) turns.
	for range 6 {
		l.ObserveTurn(0.95)
	}
	low := l.CompactionRatio()
	for i := 0; i < 6; i++ {
		l.ObserveTurn(0.10)
	}
	if got := l.CompactionRatio(); got <= low {
		t.Fatalf("cold utilization should raise the ratio: low=%v got=%v", low, got)
	}
	if got := l.CompactionRatio(); got > ratioMax {
		t.Fatalf("ratio exceeded ceiling: %v", got)
	}
}

func TestObserveOverflowDropsRatio(t *testing.T) {
	l := NewLearner(tmpPath(t))
	before := l.CompactionRatio()
	l.ObserveOverflow()
	if got := l.CompactionRatio(); got >= before {
		t.Fatalf("overflow should drop the ratio: before=%v got=%v", before, got)
	}
}

func TestLearnerPersists(t *testing.T) {
	p := tmpPath(t)
	l := NewLearner(p)
	for i := 0; i < 6; i++ {
		l.ObserveTurn(0.95)
	}
	if err := l.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded := NewLearner(p)
	if reloaded.CompactionRatio() != l.CompactionRatio() {
		t.Fatalf("persisted ratio mismatch: %v vs %v", reloaded.CompactionRatio(), l.CompactionRatio())
	}
	if reloaded.Stats() == "" {
		t.Fatalf("Stats() returned empty")
	}
}

func TestInMemoryLearnerNoPanic(t *testing.T) {
	l := NewLearner("") // no path → in-memory only
	l.ObserveTurn(0.9)
	if err := l.Save(); err != nil {
		t.Fatalf("in-memory save should not error: %v", err)
	}
}
