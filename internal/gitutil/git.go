// Package gitutil provides shared git operations to avoid spawning
// duplicate git processes. On Windows, each exec.Command costs 50-200ms
// due to CreateProcess overhead + antivirus scanning — this package
// consolidates all git calls into cached, process-spawn-efficient helpers.
package gitutil

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// isRepoCache caches isGitRepo results per directory to avoid repeated
// `git rev-parse` calls (each costs 50-200ms on Windows).
var (
	isRepoMu    sync.RWMutex
	isRepoCache = map[string]bool{}
)

// IsGitRepo reports whether root is inside a git working tree.
// Result is cached per directory for the process lifetime.
func IsGitRepo(root string) bool {
	isRepoMu.RLock()
	if cached, ok := isRepoCache[root]; ok {
		isRepoMu.RUnlock()
		return cached
	}
	isRepoMu.RUnlock()

	// Walk up to 8 parent dirs looking for .git (matches workspace.go logic)
	cur := root
	for i := 0; i < 8; i++ {
		cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
		cmd.Dir = cur
		out, err := cmd.Output()
		if err == nil && strings.TrimSpace(string(out)) == "true" {
			isRepoMu.Lock()
			isRepoCache[root] = true
			isRepoMu.Unlock()
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur || parent == "" || parent == "." {
			break
		}
		cur = parent
	}

	isRepoMu.Lock()
	isRepoCache[root] = false
	isRepoMu.Unlock()
	return false
}

// lsFilesCache caches git ls-files results per root directory.
var (
	lsFilesMu    sync.RWMutex
	lsFilesCache = map[string][]string{}
)

// LsFiles returns tracked files via `git ls-files` as relative paths.
// Falls back to nil when git is unavailable or not a git repo.
// Result is cached per directory for the process lifetime.
func LsFiles(root string) []string {
	lsFilesMu.RLock()
	if cached, ok := lsFilesCache[root]; ok {
		lsFilesMu.RUnlock()
		return cached
	}
	lsFilesMu.RUnlock()

	if !IsGitRepo(root) {
		return nil
	}

	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	parts := strings.Split(string(out), "\x00")
	files := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		files = append(files, filepath.ToSlash(p))
	}

	lsFilesMu.Lock()
	lsFilesCache[root] = files
	lsFilesMu.Unlock()

	return files
}

// LsFilesAbs returns tracked files via `git ls-files` as absolute paths.
// Falls back to nil when git is unavailable or not a git repo.
// Result is cached per directory for the process lifetime.
func LsFilesAbs(root string) []string {
	rel := LsFiles(root)
	if rel == nil {
		return nil
	}
	abs := make([]string, len(rel))
	for i, p := range rel {
		abs[i] = filepath.Join(root, p)
	}
	return abs
}
