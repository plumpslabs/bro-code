package tool

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWorktreeManager(t *testing.T) {
	dir := t.TempDir()

	// Initialize git repo
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "main.go")
	runGit("commit", "-m", "Initial commit")

	wm := NewWorktreeManager(dir)

	// 1. Create worktree
	wtDir, branch, err := wm.CreateWorktree("refactor-auth")
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Fatalf("expected worktree directory to exist at %s", wtDir)
	}

	// 2. List worktrees
	list, err := wm.ListWorktrees()
	if err != nil {
		t.Fatalf("ListWorktrees failed: %v", err)
	}
	if len(list) < 2 {
		t.Fatalf("expected at least 2 worktrees (main + isolated), got %d", len(list))
	}

	// 3. Remove worktree
	if err := wm.RemoveWorktree(wtDir, branch, true); err != nil {
		t.Fatalf("RemoveWorktree failed: %v", err)
	}

	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("expected worktree dir to be removed, but still exists: %s", wtDir)
	}
}
