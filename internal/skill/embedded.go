package skill

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
)

// defaultFS embeds the built-in skill pack that ships with BroCode. The pack
// covers the CONDITIONAL workflows (per-stack verification, migration, refactor,
// reproduce-first debugging, locale merging) so the base system prompt can stay
// lean: universal contract in the prompt, task-specific procedure in a skill
// that only loads when relevant (progressive disclosure).
//
// The skills are auto-installed into .brocode/skills/ on first run (see
// EnsureDefaultsInstalled) so they are real files the model can read via
// read_file, and users can edit or remove them like any other skill.
//
//go:embed defaults/*/SKILL.md
var defaultFS embed.FS

// DefaultSkills returns the embedded default pack as Skill values. They are
// not wired into any Loader; call EnsureDefaultsInstalled to materialize them
// onto disk, after which the regular Loader picks them up.
func DefaultSkills() []Skill {
	names, err := fs.Glob(defaultFS, "defaults/*/SKILL.md")
	if err != nil {
		return nil
	}
	out := make([]Skill, 0, len(names))
	for _, path := range names {
		data, err := defaultFS.ReadFile(path)
		if err != nil {
			continue
		}
		dir := filepath.Base(filepath.Dir(path))
		name, desc := parseFrontmatter(string(data), dir)
		out = append(out, Skill{
			Name:        name,
			Description: desc,
			Version:     parseFrontmatterVersion(string(data)),
			Path:        filepath.Join(".brocode", "skills", name, "SKILL.md"),
			Content:     string(data),
			Builtin:     true,
		})
	}
	return out
}

// skillVersionState records, per default skill, the version that was installed
// and the content hash at install time. The installer uses it to upgrade an
// UNTOUCHED default skill when a new BroCode ships a better version, while
// never clobbering user edits: an empty hash means "user-owned — never
// auto-upgrade this file again".
type skillVersionState struct {
	Versions map[string]int    `json:"versions"` // skill name → installed version
	Hashes   map[string]string `json:"hashes"`   // skill name → sha256 of installed SKILL.md ("" = user-owned)
}

func loadVersionState(dir string) *skillVersionState {
	st := &skillVersionState{Versions: map[string]int{}, Hashes: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(dir, ".versions.json"))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, st)
	if st.Versions == nil {
		st.Versions = map[string]int{}
	}
	if st.Hashes == nil {
		st.Hashes = map[string]string{}
	}
	return st
}

func (st *skillVersionState) save(dir string) {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".versions.json"), data, 0o644)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

// EnsureDefaultsInstalled materializes the embedded default pack into
// workspaceDir/.brocode/skills/<name>/SKILL.md and keeps them in sync with the
// binary's pack across upgrades:
//
//   - Missing skill        → installed (counted).
//   - Installed, untouched → upgraded in place when the embedded version is
//     newer (counted as an install/upgrade).
//   - Installed, user-edit → NEVER touched; marked user-owned so later
//     versions don't keep retrying.
//   - Legacy install (no version state yet) → the current file is compared to
//     the embedded content: identical = treated as the default (upgradeable
//     later), different = user-owned.
//
// Best-effort: a failure to write never breaks a session (the catalog just
// shows fewer entries).
func EnsureDefaultsInstalled(workspaceDir string) int {
	if workspaceDir == "" {
		return 0
	}
	stateDir := filepath.Join(workspaceDir, ".brocode", "skills")
	st := loadVersionState(stateDir)
	installed := 0
	for _, s := range DefaultSkills() {
		dir := filepath.Join(stateDir, s.Name)
		dest := filepath.Join(dir, "SKILL.md")
		if _, err := os.Stat(dest); err != nil {
			// Fresh install.
			if err := os.MkdirAll(dir, 0o755); err != nil {
				continue
			}
			if err := os.WriteFile(dest, []byte(s.Content), 0o644); err != nil {
				continue
			}
			st.Versions[s.Name] = s.Version
			st.Hashes[s.Name] = sha256Hex([]byte(s.Content))
			installed++
			continue
		}

		baseline, hasBaseline := st.Hashes[s.Name]
		if !hasBaseline {
			// Legacy install (predates version tracking). Compare the current
			// file against the embedded content: identical → treat as the
			// default and record a baseline so future upgrades apply;
			// different → user-owned, never touch again.
			if fileSHA256(dest) == sha256Hex([]byte(s.Content)) {
				st.Versions[s.Name] = s.Version
				st.Hashes[s.Name] = sha256Hex([]byte(s.Content))
			} else {
				st.Versions[s.Name] = s.Version
				st.Hashes[s.Name] = ""
			}
			continue
		}
		if baseline == "" {
			continue // user-owned
		}
		if fileSHA256(dest) != baseline {
			// Edited since install — respect it, mark user-owned and stop
			// retrying on every future startup.
			st.Versions[s.Name] = s.Version
			st.Hashes[s.Name] = ""
			continue
		}
		if st.Versions[s.Name] >= s.Version {
			continue // already current and untouched
		}
		// Untouched default with a newer embedded version → safe upgrade.
		if err := os.WriteFile(dest, []byte(s.Content), 0o644); err != nil {
			continue
		}
		st.Versions[s.Name] = s.Version
		st.Hashes[s.Name] = sha256Hex([]byte(s.Content))
		installed++
	}
	st.save(stateDir)
	return installed
}
