package tool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// WorktreeInfo represents an active git worktree.
type WorktreeInfo struct {
	Directory string `json:"directory"`
	Branch    string `json:"branch"`
	Head      string `json:"head"`
}

// WorktreeManager manages isolated background git worktrees.
type WorktreeManager struct {
	WorkspaceDir string
}

// NewWorktreeManager creates a new WorktreeManager for a workspace.
func NewWorktreeManager(workspaceDir string) *WorktreeManager {
	return &WorktreeManager{WorkspaceDir: workspaceDir}
}

var nonAlphanumericSlugRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// CreateWorktree creates a lightweight isolated worktree under .brocode/worktrees/<name>.
func (m *WorktreeManager) CreateWorktree(taskName string) (string, string, error) {
	if m.WorkspaceDir == "" {
		cwd, _ := os.Getwd()
		m.WorkspaceDir = cwd
	}

	// Verify git repo
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = m.WorkspaceDir
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("git worktree requires a git repository: %w", err)
	}

	slug := strings.ToLower(strings.TrimSpace(taskName))
	slug = nonAlphanumericSlugRe.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 30 {
		slug = slug[:30]
	}
	if slug == "" {
		slug = "task"
	}

	id := fmt.Sprintf("wt-%d-%s", time.Now().Unix()%100000, slug)
	branchName := "brocode/" + id
	worktreeRoot := filepath.Join(m.WorkspaceDir, ".brocode", "worktrees")
	_ = os.MkdirAll(worktreeRoot, 0o755)
	worktreeDir := filepath.Join(worktreeRoot, id)

	// Execute git worktree add
	addCmd := exec.Command("git", "worktree", "add", worktreeDir, "-b", branchName)
	addCmd.Dir = m.WorkspaceDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("git worktree add failed: %s (%w)", string(out), err)
	}

	// Auto-copy .env if present in root for runtime config continuity
	srcEnv := filepath.Join(m.WorkspaceDir, ".env")
	if data, err := os.ReadFile(srcEnv); err == nil {
		_ = os.WriteFile(filepath.Join(worktreeDir, ".env"), data, 0o600)
	}

	return worktreeDir, branchName, nil
}

// RemoveWorktree deletes the worktree and prunes git tracking.
func (m *WorktreeManager) RemoveWorktree(worktreeDir, branchName string, deleteBranch bool) error {
	if worktreeDir == "" || worktreeDir == "/" || worktreeDir == m.WorkspaceDir {
		return fmt.Errorf("invalid worktree directory: %q", worktreeDir)
	}
	// Must be inside .brocode/worktrees/ to protect user workspace
	if !strings.Contains(worktreeDir, filepath.Join(".brocode", "worktrees")) {
		return fmt.Errorf("refusing to remove non-brocode worktree: %q", worktreeDir)
	}

	cmd := exec.Command("git", "worktree", "remove", worktreeDir, "--force")
	cmd.Dir = m.WorkspaceDir
	_ = cmd.Run()

	// Clean up dir if left
	_ = os.RemoveAll(worktreeDir)

	// Prune worktree metadata
	pruneCmd := exec.Command("git", "worktree", "prune")
	pruneCmd.Dir = m.WorkspaceDir
	_ = pruneCmd.Run()

	if deleteBranch && branchName != "" && branchName != "main" && branchName != "master" {
		delCmd := exec.Command("git", "branch", "-D", branchName)
		delCmd.Dir = m.WorkspaceDir
		_ = delCmd.Run()
	}

	return nil
}

// MergeWorktree merges the worktree branch into the active branch.
func (m *WorktreeManager) MergeWorktree(branchName string) (string, error) {
	cmd := exec.Command("git", "merge", branchName, "--no-ff", "-m", fmt.Sprintf("Merge isolated BroCode worktree: %s", branchName))
	cmd.Dir = m.WorkspaceDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git merge %s failed: %s (%w)", branchName, string(out), err)
	}
	return string(out), nil
}

// ListWorktrees returns all active worktrees.
func (m *WorktreeManager) ListWorktrees() ([]WorktreeInfo, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = m.WorkspaceDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}

	var list []WorktreeInfo
	var cur WorktreeInfo
	lines := strings.Split(string(out), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "worktree ") {
			if cur.Directory != "" {
				list = append(list, cur)
			}
			cur = WorktreeInfo{Directory: strings.TrimPrefix(l, "worktree ")}
		} else if strings.HasPrefix(l, "branch ") {
			cur.Branch = strings.TrimPrefix(l, "branch ")
			cur.Branch = strings.TrimPrefix(cur.Branch, "refs/heads/")
		} else if strings.HasPrefix(l, "HEAD ") {
			cur.Head = strings.TrimPrefix(l, "HEAD ")
		}
	}
	if cur.Directory != "" {
		list = append(list, cur)
	}

	return list, nil
}
