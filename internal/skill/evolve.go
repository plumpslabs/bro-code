package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProposeGotchasPatch appends repair-distilled gotchas to <skillDir>/GOTCHAS.md
// — a reviewable patch proposal for the skill's SKILL.md, so the skill evolves
// from real repairs in this project. It NEVER touches SKILL.md itself: keeping
// its content byte-identical to the installed default means the version-aware
// installer still applies official skill updates (a modified SKILL.md is marked
// user-owned and frozen from upgrades). Returns the number of NEW gotchas
// written (0 when nothing new or the directory is unusable).
func ProposeGotchasPatch(skillDir, skillName string, gotchas []string) int {
	if skillDir == "" || skillName == "" || len(gotchas) == 0 {
		return 0
	}
	dest := filepath.Join(skillDir, "GOTCHAS.md")
	existing := ""
	if data, err := os.ReadFile(dest); err == nil {
		existing = string(data)
	}

	var sb strings.Builder
	if strings.TrimSpace(existing) == "" {
		fmt.Fprintf(&sb, "# %s — repair gotchas proposed by BroCode\n\n", skillName)
		sb.WriteString("These lessons were distilled from real repair failures in this project's sessions.\n")
		sb.WriteString("Review them: merge the useful ones into SKILL.md, delete the rest (or this file).\n\n")
	} else {
		sb.WriteString(existing)
		if !strings.HasSuffix(existing, "\n") {
			sb.WriteString("\n")
		}
	}

	added := 0
	for _, g := range gotchas {
		g = strings.TrimSpace(g)
		if g == "" || strings.Contains(sb.String(), g) {
			continue
		}
		fmt.Fprintf(&sb, "- %s\n", g)
		added++
	}
	if added == 0 {
		return 0
	}
	if err := os.WriteFile(dest, []byte(sb.String()), 0o644); err != nil {
		return 0
	}
	return added
}
