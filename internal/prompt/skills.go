package prompt

import (
	"fmt"
	"sort"
	"strings"
)

// renderSkillsBlock renders the AVAILABLE SKILLS catalog (level 1 of
// progressive disclosure: name + description only; the model loads SKILL.md
// itself when relevant). When the installed catalog grows past the tuning
// threshold, only the top-K most relevant skills are listed so the catalog
// cannot silently bloat every prompt (PHILOSOPHY Principle 2: relevance
// filtering before anything enters the system prompt).
func renderSkillsBlock(in *Input) string {
	if len(in.Skills) == 0 {
		return ""
	}
	t := in.Tuning
	if t == nil {
		t = DefaultTuning()
	}
	selected := in.Skills
	extra := 0
	if len(in.Skills) > t.SkillCatalogThreshold {
		selected, extra = topSkills(in.Skills, in.UserPrompt, in.Stacks, t.SkillCatalogCap, t.SkillCatalogMin)
	}
	var sb strings.Builder
	sb.WriteString("\n\nAVAILABLE SKILLS:\n")
	for _, s := range selected {
		if s.Description != "" {
			fmt.Fprintf(&sb, "- %s: %s\n", s.Name, s.Description)
		} else {
			fmt.Fprintf(&sb, "- %s\n", s.Name)
		}
	}
	if extra > 0 {
		fmt.Fprintf(&sb, "- (+%d more available — list .agents/skills and .brocode/skills for the full catalog)\n", extra)
	}
	sb.WriteString("When a task matches a skill, load its SKILL.md file (read_file) and follow its instructions.")
	return sb.String()
}

type scoredSkill struct {
	entry SkillEntry
	score int
}

// topSkills selects the most relevant skills for the user prompt, keeping at
// least min entries (when there are matches) and at most cap. With no prompt
// (or no matches), it falls back to the first min entries so the catalog never
// vanishes entirely. Stacks bias the ranking toward the repo's languages.
// Returns the selected entries and how many were dropped.
func topSkills(entries []SkillEntry, userPrompt string, stacks []Stack, capN, minN int) ([]SkillEntry, int) {
	if capN <= 0 {
		capN = 8
	}
	if minN <= 0 {
		minN = 5
	}
	if minN > capN {
		minN = capN
	}
	words := tokenize(userPrompt)
	scored := make([]scoredSkill, len(entries))
	for i, e := range entries {
		scored[i] = scoredSkill{entry: e, score: skillScore(e, words, stacks)}
	}
	// Stable sort: ties keep catalog order (builtins first).
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })

	k := capN
	if len(words) == 0 {
		k = minN
	} else {
		relevant := 0
		for _, s := range scored {
			if s.score > 0 {
				relevant++
			}
		}
		if relevant == 0 {
			k = minN
		} else if relevant < capN {
			k = relevant
			if k < minN {
				k = minN
			}
		}
	}
	if k > len(scored) {
		k = len(scored)
	}
	out := make([]SkillEntry, k)
	for i := 0; i < k; i++ {
		out[i] = scored[i].entry
	}
	return out, len(entries) - k
}

// skillScore ranks a skill against the prompt's words plus the repo's detected
// stacks. Name hits weigh double (the name is what the model keys on when
// deciding to load a skill); a skill matching a detected stack gets a fixed
// boost so the catalog follows the repo even when the user prompt never names
// the language (e.g. "fix the build" in a Go repo).
func skillScore(e SkillEntry, words []string, stacks []Stack) int {
	name := tokenize(e.Name)
	desc := tokenize(e.Description)
	score := 0
	for _, w := range words {
		for _, n := range name {
			if n == w {
				score += 2
			}
		}
		for _, d := range desc {
			if d == w {
				score++
			}
		}
	}
	// Stack affinity. Prefix match (stack length >= 2) catches "typescript"/
	// "tsc"/"gofmt" without false-positiving on unrelated words ("steps").
	for _, s := range stacks {
		low := strings.ToLower(s.Name)
		if len(low) < 2 {
			continue
		}
		for _, t := range append(name, desc...) {
			if t == low || strings.HasPrefix(t, low) {
				score += 3
				break
			}
		}
	}
	return score
}

// tokenize lowercases and splits on non-alphanumeric runs, keeping words of
// length >= 2 (single letters like "c" or "x" are noise for relevance).
func tokenize(s string) []string {
	s = strings.ToLower(s)
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}
