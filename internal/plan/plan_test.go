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

func TestParseMarkdownPlan_UniversalLanguageAgnostic(t *testing.T) {
	// Scenario 1: Numbered heading format (like event 88 from session logs)
	md1 := `## Rencana Perbaikan UI Bubble Kontak

### Masalah
Teks putih di latar belakang biru.

### Solusi
Berikut rencananya:

#### 1. **Identifikasi Pola Bubble Kontak**
Pastikan selector CSS benar.

#### 2. **Perbaikan Warna Teks di Mode Terang**
Ganti class Tailwind.

#### 3. **Perbaikan Warna Teks di Mode Gelap**
Gunakan text-slate-200.

#### 4. **Penempatan Ikon yang Sesuai**
Tambahkan UserIcon dan Phone.

- crm-react-vite-tailwind-modern/src/pages/followup/section/OmnichannelPanel/components/MessageBubble.tsx
`
	p1 := ParseMarkdownPlan(md1)
	if p1.Goal != "Rencana Perbaikan UI Bubble Kontak" {
		t.Errorf("expected goal 'Rencana Perbaikan UI Bubble Kontak', got '%s'", p1.Goal)
	}
	if len(p1.Steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(p1.Steps))
	}
	if len(p1.Files) != 1 {
		t.Errorf("expected 1 file, got %d", len(p1.Files))
	}

	// Scenario 2: Japanese numbered list format
	md2 := `# 認証機能の実装計画

1. JWTトークンの生成関数の作成
2. ミドルウェアへのトークン検証の追加
3. ユニットテストの作成

- /pkg/auth/jwt.go
- /pkg/middleware/auth.go
`
	p2 := ParseMarkdownPlan(md2)
	if p2.Goal != "認証機能の実装計画" {
		t.Errorf("expected Japanese goal, got '%s'", p2.Goal)
	}
	if len(p2.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(p2.Steps))
	}
	if len(p2.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(p2.Files))
	}
}
