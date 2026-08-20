package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndRenderMarkdownPlan(t *testing.T) {
	raw := `# 🎯 Plan: Password Hashing dengan Bcrypt

**Status:** ACTIVE

## 📋 Tasks
- [x] Step 1: Add PasswordHash field to User struct in auth.go
- [ ] Step 2: Initialize sample users with bcrypt hash
- [ ] Step 3: Implement bcrypt CompareHashAndPassword in Authenticate
- [ ] Step 4: Update auth_test.go test cases

## 📁 Impacted Files
- auth.go
- auth_test.go
`
	p := ParseMarkdownPlan(raw)
	if p.Goal != "Password Hashing dengan Bcrypt" {
		t.Errorf("expected goal 'Password Hashing dengan Bcrypt', got '%s'", p.Goal)
	}
	if len(p.Steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(p.Steps))
	}
	if p.Steps[0].Status != "done" {
		t.Errorf("expected step 1 to be done, got %s", p.Steps[0].Status)
	}
	if p.Steps[1].Status != "pending" {
		t.Errorf("expected step 2 to be pending, got %s", p.Steps[1].Status)
	}

	rendered := RenderMarkdownPlan(p)
	if !strings.Contains(rendered, "- [x] Step 1: Add PasswordHash field") {
		t.Errorf("rendered markdown missing step 1: %s", rendered)
	}
	if !strings.Contains(rendered, "- [ ] Step 2: Initialize sample users") {
		t.Errorf("rendered markdown missing step 2: %s", rendered)
	}
}

func TestSaveLoadAndArchiveCurrentPlan(t *testing.T) {
	tmpDir := t.TempDir()

	p := &Plan{
		Goal:   "Implement OAuth2",
		Status: "ACTIVE",
		Steps: []PlanStep{
			{ID: "step_1", Description: "Add OAuth2 config", Status: "done"},
			{ID: "step_2", Description: "Add callback handler", Status: "done"},
		},
	}

	if err := SaveCurrentPlan(tmpDir, p); err != nil {
		t.Fatalf("SaveCurrentPlan failed: %v", err)
	}

	loaded, err := LoadCurrentPlan(tmpDir)
	if err != nil {
		t.Fatalf("LoadCurrentPlan failed: %v", err)
	}
	if loaded.Goal != "Implement OAuth2" {
		t.Errorf("expected loaded goal 'Implement OAuth2', got '%s'", loaded.Goal)
	}
	if len(loaded.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(loaded.Steps))
	}

	archivePath, err := ArchiveCurrentPlan(tmpDir)
	if err != nil {
		t.Fatalf("ArchiveCurrentPlan failed: %v", err)
	}
	if _, err := os.Stat(archivePath); err != nil {
		t.Errorf("archived file does not exist at %s: %v", archivePath, err)
	}

	// Verify current_plan.md was cleaned up
	if _, err := os.Stat(filepath.Join(tmpDir, ".brocode", "current_plan.md")); !os.IsNotExist(err) {
		t.Errorf("expected current_plan.md to be removed after archiving")
	}
}
