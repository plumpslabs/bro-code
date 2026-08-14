package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, frontmatter, body string) {
	t.Helper()
	dir := filepath.Join(root, ".agents", "skills", name)
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

	l := NewLoader(root)
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

	l := NewLoader(root)
	if got := l.Match("go"); len(got) != 1 {
		t.Errorf("expected 1 match for 'go', got %d", len(got))
	}
	if got := l.Match("python"); len(got) != 0 {
		t.Errorf("expected 0 matches for 'python', got %d", len(got))
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
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill from .brocode, got %d", len(skills))
	}
	if skills[0].Name != "team-rule" {
		t.Errorf("expected team-rule, got %s", skills[0].Name)
	}
}
