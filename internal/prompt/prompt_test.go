package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseInput() *Input {
	return &Input{
		Mode:         "BUILDER",
		Iteration:    1,
		ProjectCtx:   "src/, cmd/, internal/",
		RepoMap:      "REPO MAP:\nmain.go → cmd/main",
		UserPrompt:   "fix the Go build",
		LSPAvailable: 2,
	}
}

// TestAssembleParityWithLegacyPrompt pins the refactor as behavior-neutral: the
// block-built prompt must still carry every historically important block —
// identity, project context, repo map, skills, LSP, memory, pre-flight
// diagnostics (iteration 1 only), auto-fixes, plan mode, and the mode rules.
func TestAssembleParityWithLegacyPrompt(t *testing.T) {
	in := baseInput()
	in.Skills = []SkillEntry{{Name: "go-workflow", Description: "Verify Go projects"}}
	in.MemoryWarm = "Build: go build ./...\n"
	in.Preflight = "PRE-GATHERED LSP DIAGNOSTICS:\n  error 12:5 unused"
	in.PreflightAuto = "fixed 3 deprecated APIs"

	p, _ := Assemble(in)
	for _, want := range []string{
		"You are BroCode CLI, an autonomous AI coding assistant.",
		"You are working in this project:",
		"REPO MAP:",
		"AVAILABLE SKILLS:",
		"go-workflow: Verify Go projects",
		"LSP AVAILABLE (2 language server(s))",
		"PROJECT MEMORY",
		"PRE-GATHERED LSP DIAGNOSTICS:",
		"PRE-APPLIED AUTO-FIXES",
		"ACTIVE ENGINE MODE: BUILDER",
		"Engine Mode Rules (BUILDER):",
		"BATCH & STAY LEAN",        // builder rule b3b
		"PLAN-THEN-ACT (multi-step tasks):", // builder rule b12
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// Pre-flight blocks must NOT leak into later iterations (cached prefix
	// stability): the engine renders iteration>1 only after iteration 1.
	p2, _ := Assemble(func() *Input { i := *in; i.Iteration = 2; return &i }())
	if strings.Contains(p2, "PRE-GATHERED LSP DIAGNOSTICS:") {
		t.Error("pre-flight block leaked into iteration 2 prompt")
	}
}

// TestAssemblePlanMode verifies the plan-then-act directive renders when the
// engine gates an implementation task.
func TestAssemblePlanMode(t *testing.T) {
	in := baseInput()
	in.PlanMode = true
	p, _ := Assemble(in)
	if !strings.Contains(p, "PLAN MODE") || !strings.Contains(p, "ask_user") {
		t.Errorf("plan mode directive missing:\n%s", p)
	}
}

// TestTuningDisablesRulesAndBlocks verifies the tuning surface: a rule can be
// switched off by ID and a whole block can be disabled by name.
func TestTuningDisablesRulesAndBlocks(t *testing.T) {
	in := baseInput()
	p, _ := Assemble(in)
	if !strings.Contains(p, "ANTI-LOOP EFFICIENCY") {
		t.Fatalf("rule b11 should be on by default")
	}

	in.Tuning = &Tuning{
		BlockEnabled: map[string]bool{},
		RulesOff:     map[string][]string{"BUILDER": {"b11"}},
	}
	p2, _ := Assemble(in)
	if strings.Contains(p2, "ANTI-LOOP EFFICIENCY") {
		t.Error("disabled rule b11 still rendered")
	}
	if !strings.Contains(p2, "BATCH & STAY LEAN") {
		t.Error("rule b3b must remain on when only b11 is off")
	}

	// Disabling the skills block removes the catalog entirely.
	in2 := baseInput()
	in2.Skills = []SkillEntry{{Name: "go-workflow", Description: "Verify Go projects"}}
	in2.Tuning = &Tuning{BlockEnabled: map[string]bool{"skills": false}}
	p3, _ := Assemble(in2)
	if strings.Contains(p3, "AVAILABLE SKILLS:") {
		t.Error("skills block rendered despite tuning disable")
	}
	// The identity + mode blocks are Always and cannot be disabled.
	if !strings.Contains(p3, "You are BroCode CLI") {
		t.Error("identity block must always render")
	}
	if !strings.Contains(p3, "ACTIVE ENGINE MODE") {
		t.Error("mode block must always render")
	}
}

// TestSkillCatalogRelevanceFiltering verifies progressive disclosure at scale:
// a small catalog lists everything; a large one is cut to the top-K relevant
// skills (with a floor so it never vanishes).
func TestSkillCatalogRelevanceFiltering(t *testing.T) {
	many := make([]SkillEntry, 0, 30)
	for i := 0; i < 30; i++ {
		many = append(many, SkillEntry{Name: "skill-" + string(rune('a'+i%26)) + string(rune('0'+i/26)), Description: "generic capability number " + string(rune('0'+i))})
	}
	many[0] = SkillEntry{Name: "go-workflow", Description: "verify and fix Go projects with go build vet test"}
	many[1] = SkillEntry{Name: "ts-workflow", Description: "verify and fix TypeScript projects with tsc"}
	many[2] = SkillEntry{Name: "debugging-repro", Description: "reproduce-first debugging for bug reports"}

	in := baseInput() // UserPrompt: "fix the Go build"
	in.Skills = many
	p, _ := Assemble(in)

	// Catalog was relevance-filtered: the Go workflow skill is in, but not all
	// 30 generic ones are listed.
	if !strings.Contains(p, "go-workflow") {
		t.Error("relevant go-workflow skill filtered out")
	}
	if strings.Contains(p, "skill-a1") { // i=26, low-relevance generic skill
		t.Error("irrelevant low-ranked skill leaked into filtered catalog")
	}
	lines := 0
	for _, line := range strings.Split(p, "\n") {
		if strings.HasPrefix(line, "- ") {
			lines++
		}
	}
	if lines > 8 {
		t.Errorf("filtered catalog listed %d skills, want <= cap 8", lines)
	}

	// With no prompt, the floor keeps the first min entries.
	in2 := baseInput()
	in2.UserPrompt = ""
	in2.Skills = many
	p2, _ := Assemble(in2)
	count := 0
	for _, line := range strings.Split(p2, "\n") {
		if strings.HasPrefix(line, "- ") {
			count++
		}
	}
	if count < 5 {
		t.Errorf("empty prompt should keep at least min=5 skills, got %d", count)
	}

	// A small catalog (<= threshold) lists everything untouched.
	in3 := baseInput()
	in3.Skills = []SkillEntry{{Name: "go-workflow", Description: "Go"}}
	p3, _ := Assemble(in3)
	if !strings.Contains(p3, "go-workflow") {
		t.Error("small catalog must list its skill")
	}
}

// TestStackHintAndBoost verifies the detected repo stack (a) renders a one-line
// STACK hint and (b) biases the skill-catalog ranking so the matching language
// skill surfaces even when the user prompt never names the language.
func TestStackHintAndBoost(t *testing.T) {
	in := baseInput()
	in.UserPrompt = "fix the broken build" // no language named
	in.Stacks = []Stack{{Name: "go", Files: []string{"go.mod", "main.go"}}}
	in.Skills = []SkillEntry{
		{Name: "ts-workflow", Description: "verify and fix TypeScript projects with tsc"},
		{Name: "go-workflow", Description: "verify and fix Go projects with go build vet test"},
	}

	p, _ := Assemble(in)
	if !strings.Contains(p, "STACK: go (go.mod, main.go)") {
		t.Errorf("STACK hint with evidence files missing from prompt:\n%s", p)
	}
	// Both skills are under the threshold so both are listed — the boost matters
	// for ORDER at scale, covered by topSkills directly below.
	if !strings.Contains(p, "go-workflow") || !strings.Contains(p, "ts-workflow") {
		t.Error("small catalog must list all skills regardless of stack")
	}

	// At scale (filtering active), the Go skill must outrank the TS skill even
	// though the prompt only says "fix the broken build".
	goSkill := in.Skills[1] // go-workflow
	tsSkill := in.Skills[0] // ts-workflow
	words := tokenize(in.UserPrompt)
	goScore := skillScore(goSkill, words, in.Stacks)
	tsScore := skillScore(tsSkill, words, in.Stacks)
	if goScore <= tsScore {
		t.Errorf("go-workflow (%d) must outrank ts-workflow (%d) in a Go repo", goScore, tsScore)
	}
	// The boost is what flips it: without the stack, "fix" matches both
	// descriptions roughly equally and the TS skill is not below the Go one.
	if goScore <= skillScore(tsSkill, words, nil) {
		t.Errorf("stack boost must lift go-workflow above unboosted ts-workflow: %d vs %d", goScore, skillScore(tsSkill, words, nil))
	}
}

// TestAssembleCostAccounting verifies every rendered block reports a positive
// token cost and the costs sum to roughly the whole prompt's estimate.
func TestAssembleCostAccounting(t *testing.T) {
	in := baseInput()
	in.Skills = []SkillEntry{{Name: "go-workflow", Description: "Verify Go projects"}}
	p, costs := Assemble(in)
	if len(costs) == 0 {
		t.Fatal("expected per-block costs")
	}
	total := 0
	for name, c := range costs {
		if c <= 0 {
			t.Errorf("block %q reported non-positive cost %d", name, c)
		}
		total += c
	}
	est := len(p) / 3 // rough chars-per-token sanity bound
	if total < est/4 || total > est*4 {
		t.Errorf("block cost sum %d far from prompt estimate ~%d", total, est)
	}
}

// TestLoadTuning verifies the JSON tuning surface: defaults on missing/corrupt
// files, and field-level merge over defaults for partial files.
func TestLoadTuning(t *testing.T) {
	dir := t.TempDir()

	// Missing file → defaults.
	d := LoadTuning(filepath.Join(dir, "nope.json"))
	if d.SkillCatalogCap != 8 || d.SkillCatalogThreshold != 15 {
		t.Errorf("defaults wrong: %+v", d)
	}
	if !d.BlockOn("repomap") {
		t.Error("default tuning must enable all blocks")
	}

	// Partial file → merge (only the provided fields override).
	partial := filepath.Join(dir, "partial.json")
	if err := os.WriteFile(partial, []byte(`{"skill_catalog_cap": 4, "rules_off": {"BUILDER": ["b11"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := LoadTuning(partial)
	if p.SkillCatalogCap != 4 {
		t.Errorf("cap not applied: %d", p.SkillCatalogCap)
	}
	if p.SkillCatalogThreshold != 15 {
		t.Errorf("unset field must keep default, got %d", p.SkillCatalogThreshold)
	}
	if len(p.RulesOff["BUILDER"]) != 1 || p.RulesOff["BUILDER"][0] != "b11" {
		t.Errorf("rules_off not applied: %+v", p.RulesOff)
	}

	// Corrupt file → defaults, never an error.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := LoadTuning(bad)
	if b.SkillCatalogCap != 8 {
		t.Errorf("corrupt tuning must fall back to defaults, got %+v", b)
	}
}
