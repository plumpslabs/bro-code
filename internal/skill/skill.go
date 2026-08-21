package skill

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Skill represents a loaded SKILL.md document.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Content     string `json:"content"`
	// Version is the skill's own frontmatter version (defaults start at 1).
	// The installer uses it to upgrade untouched default skills safely.
	Version int `json:"version,omitempty"`
	// Builtin marks skills from the embedded default pack (before they are
	// materialized to disk). After EnsureDefaultsInstalled they are regular
	// files and Builtin is false.
	Builtin bool `json:"builtin,omitempty"`
}

// Loader manages lazy loading of skill files.
type Loader struct {
	skills []Skill
}

// NewLoader initializes skill discovery across project (.brocode/skills) and home locations (~/.config/brocode/skills).
func NewLoader(workspaceDir string) *Loader {
	l := &Loader{}
	l.scanDir(filepath.Join(workspaceDir, ".brocode", "skills"))

	home, _ := os.UserHomeDir()
	l.scanDir(filepath.Join(home, ".config", "brocode", "skills"))
	return l
}

func (l *Loader) scanDir(dirPath string) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			skillFile := filepath.Join(dirPath, entry.Name(), "SKILL.md")
			if data, err := os.ReadFile(skillFile); err == nil {
				content := string(data)
				name, desc := parseFrontmatter(content, entry.Name())
				l.skills = append(l.skills, Skill{
					Name:        name,
					Description: desc,
					Version:     parseFrontmatterVersion(content),
					Path:        skillFile,
					Content:     content,
				})
			}
		}
	}
}

// All returns every loaded skill (used to inject the full skill list into
// the system prompt so the model knows what it can load).
func (l *Loader) All() []Skill {
	return l.skills
}

// Match performs fuzzy matching against skill name and description.
func (l *Loader) Match(query string) []Skill {
	var matches []Skill
	q := strings.ToLower(query)
	for _, s := range l.skills {
		if strings.Contains(strings.ToLower(s.Name), q) || strings.Contains(strings.ToLower(s.Description), q) {
			matches = append(matches, s)
		}
	}
	return matches
}

// parseFrontmatterVersion reads the version field from a SKILL.md frontmatter
// (0 when absent — unversioned skills are treated as version 0 by the
// installer, so an embedded v1+ always wins the comparison).
func parseFrontmatterVersion(content string) int {
	lines := strings.Split(content, "\n")
	inFrontmatter := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			}
			break // closing delimiter — body does not count
		}
		if inFrontmatter {
			if v, found := strings.CutPrefix(trimmed, "version:"); found {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err == nil {
					return n
				}
				return 0
			}
		}
	}
	return 0
}

func parseFrontmatter(content, defaultName string) (string, string) {
	lines := strings.Split(content, "\n")
	name := defaultName
	desc := ""

	inFrontmatter := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			} else {
				break
			}
		}
		if inFrontmatter {
			if strings.HasPrefix(trimmed, "name:") {
				name = strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
			}
			if strings.HasPrefix(trimmed, "description:") {
				desc = strings.TrimSpace(strings.TrimPrefix(trimmed, "description:"))
			}
		}
	}
	return name, desc
}
