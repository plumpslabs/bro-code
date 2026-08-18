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
