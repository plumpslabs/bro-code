package tool

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// GitShadowManager manages zero-overhead atomic git working tree snapshots.
// It uses git plumbing (`write-tree`, `commit-tree`, `update-ref`) to snapshot
// the entire repository state before mutations without touching HEAD or creating
// user-visible branch commits.
type GitShadowManager struct {
	mu      sync.Mutex
	repoDir string
	isGit   bool
	refs    []string // stack of snapshot refs: refs/brocode/snapshots/<session>/<seq>
}

// NewGitShadowManager initializes a shadow manager for repoDir.
func NewGitShadowManager(repoDir string) *GitShadowManager {
	m := &GitShadowManager{repoDir: repoDir}
	m.checkGit()
	return m
}

func (m *GitShadowManager) checkGit() {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = m.repoDir
	out, err := cmd.Output()
	m.isGit = err == nil && strings.TrimSpace(string(out)) == "true"
}

// IsGit reports whether repoDir is a valid Git repository.
func (m *GitShadowManager) IsGit() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isGit
}

// CreateShadowSnapshot captures the current working directory state into an
// isolated git commit ref.
func (m *GitShadowManager) CreateShadowSnapshot(sessionID string, seq int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.isGit {
		return "", fmt.Errorf("not a git repository")
	}

	ref := fmt.Sprintf("refs/brocode/snapshots/%s/%d", sessionID, seq)

	// 1. Add changes to an ephemeral index or add unstaged changes
	// We run `git stash create` or `git add -A && git write-tree` in a temporary index
	addCmd := exec.Command("git", "stash", "create", fmt.Sprintf("brocode_%s_%d", sessionID, seq))
	addCmd.Dir = m.repoDir
	var out bytes.Buffer
	addCmd.Stdout = &out
	if err := addCmd.Run(); err != nil || strings.TrimSpace(out.String()) == "" {
		// If stash create is empty, working tree might be clean or stash create returned empty
		// Fall back to direct commit-tree on current HEAD
		headCmd := exec.Command("git", "rev-parse", "HEAD")
		headCmd.Dir = m.repoDir
		headOut, err := headCmd.Output()
		if err != nil {
			return "", err
		}
		commitID := strings.TrimSpace(string(headOut))
		updateCmd := exec.Command("git", "update-ref", ref, commitID)
		updateCmd.Dir = m.repoDir
		if err := updateCmd.Run(); err != nil {
			return "", err
		}
		m.refs = append(m.refs, ref)
		return ref, nil
	}

	commitID := strings.TrimSpace(out.String())
	updateCmd := exec.Command("git", "update-ref", ref, commitID)
	updateCmd.Dir = m.repoDir
	if err := updateCmd.Run(); err != nil {
		return "", err
	}

	m.refs = append(m.refs, ref)
	return ref, nil
}

// RollbackLast restores the working directory to the most recent shadow snapshot.
func (m *GitShadowManager) RollbackLast() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.isGit {
		return "", fmt.Errorf("not a git repository")
	}
	if len(m.refs) == 0 {
		return "", fmt.Errorf("no git shadow snapshots available")
	}

	lastRef := m.refs[len(m.refs)-1]
	m.refs = m.refs[:len(m.refs)-1]

	// Restore working tree from ref
	cmd := exec.Command("git", "restore", "--staged", "--worktree", "--source="+lastRef, ".")
	cmd.Dir = m.repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		// Fall back to git checkout if restore is not available on older git
		checkoutCmd := exec.Command("git", "checkout", lastRef, "--", ".")
		checkoutCmd.Dir = m.repoDir
		if out2, err2 := checkoutCmd.CombinedOutput(); err2 != nil {
			return "", fmt.Errorf("rollback failed: %s | %s", string(out), string(out2))
		}
	}

	// Clean up ref
	delCmd := exec.Command("git", "update-ref", "-d", lastRef)
	delCmd.Dir = m.repoDir
	_ = delCmd.Run()

	return lastRef, nil
}

// PurgeAll removes all brocode shadow snapshot refs.
func (m *GitShadowManager) PurgeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.isGit {
		return
	}

	for _, ref := range m.refs {
		cmd := exec.Command("git", "update-ref", "-d", ref)
		cmd.Dir = m.repoDir
		_ = cmd.Run()
	}
	m.refs = nil
}
