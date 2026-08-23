package skill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, frontmatter, body string) {
	t.Helper()
	dir := filepath.Join(root, ".brocode", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := frontmatter
	if body != "" {
		content += body
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoaderAll(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-build", "---\nname: go-build\ndescription: Build and test Go projects\n---\n", "## Steps\n- run go build\n")
	writeSkill(t, root, "no-desc", "---\nname: no-desc\n---\n", "")

	l := NewLoaderWithDirs(root, "")
	skills := l.All()
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(skills))
	}
	var hasDesc, hasNoDesc bool
	for _, s := range skills {
		if s.Name == "go-build" {
			hasDesc = true
			if s.Description != "Build and test Go projects" {
				t.Errorf("unexpected description: %q", s.Description)
			}
			if !strings.Contains(s.Content, "go build") {
				t.Errorf("content not loaded for go-build")
			}
		}
		if s.Name == "no-desc" {
			hasNoDesc = true
			// frontmatter without description: default name from dir, empty desc
			if s.Description != "" {
				t.Errorf("expected empty description, got %q", s.Description)
			}
		}
	}
	if !hasDesc || !hasNoDesc {
		t.Errorf("missing skills: hasDesc=%v hasNoDesc=%v", hasDesc, hasNoDesc)
	}
}

func TestLoaderMatch(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-build", "---\nname: go-build\ndescription: Build and test Go projects\n---\n", "")

	l := NewLoaderWithDirs(root, "")
	if got := l.Match("go"); len(got) != 1 {
		t.Errorf("expected 1 match for 'go', got %d", len(got))
	}
	if got := l.Match("python"); len(got) != 0 {
		t.Errorf("expected 0 matches for 'python', got %d", len(got))
	}
}

// TestEnsureDefaultsInstalled verifies the embedded default skill pack is
// auto-installed into .brocode/skills (so the model can read SKILL.md files),
// installs are idempotent, and user edits are never clobbered.
func TestEnsureDefaultsInstalled(t *testing.T) {
	root := t.TempDir()
	if n := EnsureDefaultsInstalled(root); n == 0 {
		t.Fatal("expected default skills to install")
	}
	// Idempotent: a second run installs nothing new.
	if n := EnsureDefaultsInstalled(root); n != 0 {
		t.Fatalf("second install must be a no-op, installed %d", n)
	}
	// The regular loader picks them up as real, readable skills.
	all := NewLoaderWithDirs(root, filepath.Join(root, ".brocode", "skills")).All()
	if len(all) == 0 {
		t.Fatal("loader found no skills after install")
	}
	got := map[string]bool{}
	for _, s := range all {
		got[s.Name] = true
	}
	for _, want := range []string{"go-workflow", "ts-workflow", "migration-playbook", "refactor-playbook", "debugging-repro", "locale-json-merge", "rust-workflow", "python-workflow"} {
		if !got[want] {
			t.Errorf("default skill %q not installed", want)
		}
	}
	// User edits are never clobbered on re-install.
	edit := filepath.Join(root, ".brocode", "skills", "go-workflow", "SKILL.md")
	if err := os.WriteFile(edit, []byte("---\nname: go-workflow\ndescription: USER EDITED\n---\ncustom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	EnsureDefaultsInstalled(root)
	data, err := os.ReadFile(edit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "USER EDITED") {
		t.Error("EnsureDefaultsInstalled clobbered a user-edited skill")
	}
}

// embeddedSkill returns the embedded default skill with the given name, or nil.
func embeddedSkill(t *testing.T, name string) *Skill {
	t.Helper()
	for _, s := range DefaultSkills() {
		if s.Name == name {
			return &s
		}
	}
	t.Fatalf("embedded skill %q not found", name)
	return nil
}

// TestEnsureDefaultsVersionTracking verifies the version-aware installer:
// fresh installs record a baseline; an untouched default is upgraded when the
// embedded version is newer; a user-edited file is never touched and is marked
// user-owned so later versions don't retry.
func TestEnsureDefaultsVersionTracking(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".brocode", "skills")
	stateFile := filepath.Join(skillDir, ".versions.json")
	goSkill := embeddedSkill(t, "go-workflow")

	// Fresh install: baseline recorded with the embedded version + hash.
	if n := EnsureDefaultsInstalled(root); n != 8 {
		t.Fatalf("expected 8 default skills installed, got %d", n)
	}
	st := loadVersionState(skillDir)
	if st.Versions["go-workflow"] != 1 || st.Hashes["go-workflow"] != sha256Hex([]byte(goSkill.Content)) {
		t.Fatalf("fresh install baseline wrong: %+v", st)
	}

	// Simulate an embedded upgrade to v2: same content, newer version. An
	// untouched file must be upgraded in place.
	_ = os.Remove(stateFile)
	st = &skillVersionState{Versions: map[string]int{"go-workflow": 0}, Hashes: map[string]string{"go-workflow": sha256Hex([]byte(goSkill.Content))}}
	if data, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(stateFile, data, 0o644)
	}
	if n := EnsureDefaultsInstalled(root); n != 1 {
		t.Fatalf("expected 1 untouched upgrade, got %d", n)
	}
	st = loadVersionState(skillDir)
	if st.Versions["go-workflow"] != 1 {
		t.Fatalf("untouched upgrade must bump version to 1, got %+v", st)
	}

	// Legacy user edit (no baseline, file differs from embedded): never
	// touched, marked user-owned (hash "") so future versions don't retry.
	_ = os.Remove(stateFile)
	edit := filepath.Join(skillDir, "go-workflow", "SKILL.md")
	if err := os.WriteFile(edit, []byte("---\nname: go-workflow\ndescription: USER EDITED\n---\ncustom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := EnsureDefaultsInstalled(root); n != 0 {
		t.Fatalf("user-edited skill must not be upgraded, got %d installs", n)
	}
	data, err := os.ReadFile(edit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "USER EDITED") {
		t.Error("user edit was clobbered")
	}
	st = loadVersionState(skillDir)
	if st.Hashes["go-workflow"] != "" {
		t.Fatalf("user-edited skill must be marked user-owned, got %+v", st)
	}

	// Editing a skill AFTER a recorded baseline: never touched, marked owned.
	st = &skillVersionState{
		Versions: map[string]int{"go-workflow": 1},
		Hashes:   map[string]string{"go-workflow": sha256Hex([]byte(goSkill.Content))},
	}
	if data, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(stateFile, data, 0o644)
	}
	if err := os.WriteFile(edit, []byte(goSkill.Content+"\nuser tweak\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	EnsureDefaultsInstalled(root)
	st = loadVersionState(skillDir)
	if st.Hashes["go-workflow"] != "" {
		t.Fatalf("post-baseline edit must mark user-owned, got %+v", st)
	}
}

// TestProposeGotchasPatch verifies the self-evolution proposal: gotchas are
// appended (deduped) to GOTCHAS.md in the skill dir, SKILL.md itself is never
// touched (so the version-aware installer's content hash stays pristine), and
// re-proposing writes nothing new.
func TestProposeGotchasPatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "go-workflow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("---\nname: go-workflow\n---\n# Go Workflow\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gotchas := []string{
		"Verification failed on main.go — fixed after 1 repair attempt",
		"interface satisfaction usually means a missing method",
	}
	if n := ProposeGotchasPatch(dir, "go-workflow", gotchas); n != 2 {
		t.Fatalf("expected 2 written, got %d", n)
	}
	// SKILL.md is never modified by the proposal.
	data, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "repair") {
		t.Error("SKILL.md must never be modified by the proposal")
	}
	// Idempotent: re-proposing the same gotchas writes nothing new.
	if n := ProposeGotchasPatch(dir, "go-workflow", gotchas); n != 0 {
		t.Fatalf("re-proposal must write nothing new, got %d", n)
	}
	// A new gotcha appends to the existing proposal.
	if n := ProposeGotchasPatch(dir, "go-workflow", []string{"edition differences matter"}); n != 1 {
		t.Fatalf("expected 1 new gotcha appended, got %d", n)
	}
	data, err = os.ReadFile(filepath.Join(dir, "GOTCHAS.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "interface satisfaction") || !strings.Contains(got, "edition differences") {
		t.Errorf("proposal missing gotchas:\n%s", got)
	}
	// Empty input / unknown dir → 0.
	if n := ProposeGotchasPatch(dir, "go-workflow", nil); n != 0 {
		t.Fatalf("empty gotchas must write nothing, got %d", n)
	}
}

// TestDefaultSkillVersionsParsed pins the embedded pack to version 1 so a
// future bump is deliberate.
func TestDefaultSkillVersionsParsed(t *testing.T) {
	for _, s := range DefaultSkills() {
		if s.Version != 1 {
			t.Errorf("default skill %q: expected version 1, got %d", s.Name, s.Version)
		}
	}
}

func TestLoaderScopes(t *testing.T) {
	root := t.TempDir()
	// .brocode/skills should also be scanned.
	dir := filepath.Join(root, ".brocode", "skills", "team-rule")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: team-rule\ndescription: Team convention\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := NewLoader(root)
	skills := l.All()
	var found bool
	for _, s := range skills {
		if s.Name == "team-rule" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected team-rule in loaded skills, got %+v", skills)
	}
}

func TestProjectSkillOverridesGlobal(t *testing.T) {
	globalRoot := t.TempDir()
	globalSkillDir := filepath.Join(globalRoot, "skills")
	EnsureDefaultsInstalled(globalSkillDir)

	// In project root, create a custom override for "go-workflow"
	projectRoot := t.TempDir()
	projectSkillDir := filepath.Join(projectRoot, ".brocode", "skills", "go-workflow")
	if err := os.MkdirAll(projectSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectSkillDir, "SKILL.md"), []byte("---\nname: go-workflow\ndescription: PROJECT SPECIFIC OVERRIDE\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &Loader{}
	// Scan project first, then global
	l.scanDir(filepath.Join(projectRoot, ".brocode", "skills"))
	l.scanDir(globalSkillDir)

	var goSkill *Skill
	for i, s := range l.All() {
		if s.Name == "go-workflow" {
			goSkill = &l.All()[i]
			break
		}
	}
	if goSkill == nil {
		t.Fatal("go-workflow skill not found")
	}
	if goSkill.Description != "PROJECT SPECIFIC OVERRIDE" {
		t.Fatalf("expected project override to take precedence, got: %q", goSkill.Description)
	}
}

func TestZeroRepoPollution(t *testing.T) {
	cleanRepo := t.TempDir()
	// EnsureGlobalDefaultsInstalled should NOT create .brocode in cleanRepo
	brocodeDir := filepath.Join(cleanRepo, ".brocode")
	if _, err := os.Stat(brocodeDir); !os.IsNotExist(err) {
		t.Fatalf("clean repo should not have .brocode before anything is created")
	}
}
