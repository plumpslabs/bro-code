// Package learn implements BroCode's self-improving control layer: it observes
// each turn's context utilization and nudges the global efficiency knobs (today:
// the compaction trigger ratio) so the agent gets smarter about its own token
// budget the longer it is used — without any user tuning.
//
// Why this matters vs. the big players: Claude Code / Cursor ship a fixed
// compaction threshold. BroCode measures reality (how full the window actually
// gets) and converges the threshold to keep context utilization in a high-signal
// band — compacting earlier when the window runs hot, keeping more context when
// there is headroom. The tuned value persists per user (~/.config/brocode) so
// every future session starts warm.
//
// The same Learner is the natural home for future adaptive knobs (tool-description
// budget, model-routing thresholds, parallel fan-out width) — each one gets an
// Observe* call and a clamped nudge, all persisted in one JSON file.
package learn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// defaultRatio mirrors context.defaultCompactionRatio; used when no learned
// value exists yet or the stored file is missing/corrupt.
const defaultRatio = 0.60

// Tunable bounds keep the adaptive ratio in a sane, research-backed range:
// never so high the window overflows, never so low we compact on every turn.
const (
	ratioMin = 0.40
	ratioMax = 0.85
	// target band: keep utilization inside [targetLow, targetHigh]; nudge only
	// when it strays outside, so the value converges instead of oscillating.
	targetLow  = 0.55
	targetHigh = 0.82
)

// Config is the persisted, self-tuning state.
type Config struct {
	CompactionRatio float64 `json:"compaction_ratio"`

	// Rolling stats (diagnostic + future tuning). Kept across sessions so the
	// learner has history to converge from on the very first turn of a new day.
	Turns        int     `json:"turns"`
	SumUtil      float64 `json:"sum_util"`      // sum of per-turn utilizations
	OverflowHits int     `json:"overflow_hits"` // turns that hit the hard fitMessages guard
}

// Learner owns the adaptive config and persists it to disk. It is safe for
// concurrent use (the engine may observe from the turn goroutine while the UI
// reads stats).
type Learner struct {
	mu     sync.Mutex
	path   string
	config Config
	dirty  bool // unsaved change since last Save
}

// DefaultPath returns ~/.config/brocode/learn.json (matching BroCode's other
// global-state files). It returns "" if the home dir is unavailable, in which
// case the caller should use an in-memory-only Learner.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "brocode", "learn.json")
}

// NewLearner loads the config from path (or seeds defaults when missing). A path
// of "" yields an in-memory learner that still adapts within the process but
// never persists.
func NewLearner(path string) *Learner {
	l := &Learner{path: path, config: Config{CompactionRatio: defaultRatio}}
	if path == "" {
		return l
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var c Config
		if json.Unmarshal(data, &c) == nil && c.CompactionRatio > 0 {
			l.config = c
			if l.config.CompactionRatio < ratioMin {
				l.config.CompactionRatio = ratioMin
			}
			if l.config.CompactionRatio > ratioMax {
				l.config.CompactionRatio = ratioMax
			}
		}
	}
	return l
}

// CompactionRatio returns the current tuned trigger threshold.
func (l *Learner) CompactionRatio() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.config.CompactionRatio
}

// ObserveTurn feeds one finished turn's context utilization (0..1+) into the
// learner. It nudges the compaction ratio toward the target band and persists
// (throttled to every 5 turns to avoid disk thrash).
func (l *Learner) ObserveTurn(util float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.config.Turns++
	l.config.SumUtil += util

	// Nudge toward the target band only when outside it, so the value converges.
	switch {
	case util > targetHigh:
		// Window runs hot — compact earlier (lower ratio).
		l.config.CompactionRatio = clamp(l.config.CompactionRatio-0.03, ratioMin, ratioMax)
	case util < targetLow:
		// Plenty of headroom — keep more context (raise ratio).
		l.config.CompactionRatio = clamp(l.config.CompactionRatio+0.02, ratioMin, ratioMax)
	}

	l.dirty = true
	// Persist every few turns; cheap and avoids losing a long session's tuning.
	if l.config.Turns%5 == 0 {
		l.saveLocked()
	}
}

// ObserveOverflow marks a turn that hit the hard fitMessages guard (the window
// was genuinely too big even after compaction) — an even stronger signal to
// compact earlier next time.
func (l *Learner) ObserveOverflow() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.config.OverflowHits++
	l.config.CompactionRatio = clamp(l.config.CompactionRatio-0.05, ratioMin, ratioMax)
	l.dirty = true
	if l.config.Turns%5 == 0 {
		l.saveLocked()
	}
}

// Stats returns a short human-readable summary for the HUD / debug commands.
func (l *Learner) Stats() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	avg := 0.0
	if l.config.Turns > 0 {
		avg = l.config.SumUtil / float64(l.config.Turns)
	}
	return "adaptive: compaction@" + formatPct(l.config.CompactionRatio) +
		" · turns=" + itoa(l.config.Turns) +
		" · avg-util=" + formatPct(avg) +
		" · overflows=" + itoa(l.config.OverflowHits)
}

// Save flushes pending changes to disk. Safe to call often; no-op when there is
// nothing dirty or no path is configured.
func (l *Learner) Save() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.saveLocked()
}

func (l *Learner) saveLocked() error {
	if !l.dirty || l.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(l.config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	l.dirty = false
	return os.WriteFile(l.path, data, 0o644)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func formatPct(v float64) string {
	return itoa(int(v*100)) + "%"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
