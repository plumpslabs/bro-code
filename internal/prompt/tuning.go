package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Tuning is the runtime tuning surface for the system prompt. It lets users
// disable blocks, disable individual mode rules, and set the skill-catalog
// budgets without recompiling — the "tuning instruction" layer of the prompt
// architecture. Persisted as JSON at DefaultTuningPath; a missing or corrupt
// file falls back to DefaultTuning (never blocks a run).
type Tuning struct {
	// BlockEnabled toggles named blocks off (nil/absent = all enabled).
	// The identity + mode blocks are Always and cannot be disabled.
	BlockEnabled map[string]bool `json:"blocks,omitempty"`
	// RulesOff lists rule IDs to disable, keyed by mode ("BUILDER", "PLANNER",
	// "MINER"). Rule IDs are the stable identifiers in rules.go (b1, b2, ...).
	RulesOff map[string][]string `json:"rules_off,omitempty"`
	// SkillCatalogThreshold: above this many installed skills, the catalog is
	// relevance-filtered instead of listed in full.
	SkillCatalogThreshold int `json:"skill_catalog_threshold"`
	// SkillCatalogCap is the max skills listed when filtering is active.
	SkillCatalogCap int `json:"skill_catalog_cap"`
	// SkillCatalogMin keeps at least this many skills even with a weak prompt.
	SkillCatalogMin int `json:"skill_catalog_min"`
}

// DefaultTuning returns the shipping defaults: every block on, every rule on,
// and a relevance-filtered catalog once more than 15 skills are installed.
func DefaultTuning() *Tuning {
	return &Tuning{
		BlockEnabled:          map[string]bool{},
		RulesOff:              map[string][]string{},
		SkillCatalogThreshold: 15,
		SkillCatalogCap:       8,
		SkillCatalogMin:       5,
	}
}

// BlockOn reports whether a named block should render. Blocks absent from the
// map are enabled by default.
func (t *Tuning) BlockOn(name string) bool {
	if t == nil || t.BlockEnabled == nil {
		return true
	}
	on, ok := t.BlockEnabled[name]
	return !ok || on
}

// DefaultTuningPath returns ~/.config/brocode/tuning.json (matching BroCode's
// other global-state files). Returns "" when the home dir is unavailable.
func DefaultTuningPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "brocode", "tuning.json")
}

// LoadTuning reads the tuning file, merging only the fields present over the
// defaults so a partial file cannot accidentally disable everything. A missing
// or corrupt file yields DefaultTuning — tuning must never break a run.
func LoadTuning(path string) *Tuning {
	t := DefaultTuning()
	if path == "" {
		return t
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return t
	}
	var raw struct {
		BlockEnabled          map[string]bool     `json:"blocks"`
		RulesOff              map[string][]string `json:"rules_off"`
		SkillCatalogThreshold int                 `json:"skill_catalog_threshold"`
		SkillCatalogCap       int                 `json:"skill_catalog_cap"`
		SkillCatalogMin       int                 `json:"skill_catalog_min"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return t
	}
	if raw.BlockEnabled != nil {
		t.BlockEnabled = raw.BlockEnabled
	}
	if raw.RulesOff != nil {
		t.RulesOff = raw.RulesOff
	}
	if raw.SkillCatalogThreshold > 0 {
		t.SkillCatalogThreshold = raw.SkillCatalogThreshold
	}
	if raw.SkillCatalogCap > 0 {
		t.SkillCatalogCap = raw.SkillCatalogCap
	}
	if raw.SkillCatalogMin > 0 {
		t.SkillCatalogMin = raw.SkillCatalogMin
	}
	return t
}
