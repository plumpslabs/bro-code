package tool

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitShadowManager(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize git repo in tempDir
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tempDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}

	run("init")
	run("config", "user.name", "BroCode Test")
	run("config", "user.email", "test@brocode.ai")

	fileA := filepath.Join(tempDir, "main.go")
	if err := os.WriteFile(fileA, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial commit")

	mgr := NewGitShadowManager(tempDir)
	if !mgr.IsGit() {
		t.Fatal("expected IsGit to be true")
	}

	// 1. Take snapshot before change
	ref, err := mgr.CreateShadowSnapshot("sess1", 1)
	if err != nil {
		t.Fatalf("CreateShadowSnapshot failed: %v", err)
	}
	if ref == "" {
		t.Fatal("expected non-empty ref")
	}

	// 2. Modify fileA and create fileB
	if err := os.WriteFile(fileA, []byte("package main\n\nfunc main() { panic(1) }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Rollback
	restoredRef, err := mgr.RollbackLast()
	if err != nil {
		t.Fatalf("RollbackLast failed: %v", err)
	}
	if restoredRef != ref {
		t.Errorf("got restoredRef %q, want %q", restoredRef, ref)
	}

	// 4. Verify fileA is restored to original
	content, err := os.ReadFile(fileA)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "package main\n\nfunc main() {}\n" {
		t.Errorf("content not restored: %q", string(content))
	}
}

func TestMultiGitShadowManager(t *testing.T) {
	dir := t.TempDir()

	initRepo := func(path string) {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		run := func(args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = path
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v in %s failed: %v (%s)", args, path, err, string(out))
			}
		}
		run("init")
		run("config", "user.name", "BroCode Test")
		run("config", "user.email", "test@brocode.ai")
		f := filepath.Join(path, "init.txt")
		_ = os.WriteFile(f, []byte("v1"), 0644)
		run("add", ".")
		run("commit", "-m", "init")
	}

	repoA := filepath.Join(dir, "service-a")
	repoB := filepath.Join(dir, "service-b")
	initRepo(repoA)
	initRepo(repoB)

	multi := NewMultiGitShadowManager(dir, []string{repoA, repoB})

	// 1. Snapshot both repos
	refs, err := multi.CreateShadowSnapshot("sess_multi", 1)
	if err != nil {
		t.Fatalf("CreateShadowSnapshot failed: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 snapshot refs, got %d", len(refs))
	}

	// 2. Mutate both repos
	_ = os.WriteFile(filepath.Join(repoA, "init.txt"), []byte("v2_modified_a"), 0644)
	_ = os.WriteFile(filepath.Join(repoB, "init.txt"), []byte("v2_modified_b"), 0644)

	// 3. Atomic multi-repo rollback
	restored, err := multi.RollbackLast()
	if err != nil {
		t.Fatalf("RollbackLast failed: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("expected 2 restored refs, got %d", len(restored))
	}

	// 4. Verify both repos are back to v1
	dataA, _ := os.ReadFile(filepath.Join(repoA, "init.txt"))
	dataB, _ := os.ReadFile(filepath.Join(repoB, "init.txt"))
	if string(dataA) != "v1" {
		t.Errorf("repoA not restored: %q", string(dataA))
	}
	if string(dataB) != "v1" {
		t.Errorf("repoB not restored: %q", string(dataB))
	}
}
