package repo

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// gitLsFilesCache caches the result of `git ls-files` per root directory
// for the duration of the process. Invalidated by workspace mtime check
// in cachedListProjectFiles.
var (
	gitLsFilesCacheMu sync.RWMutex
	gitLsFilesCacheDir string
	gitLsFilesCache    []string
)

// gitLsFiles returns tracked files via `git ls-files`, which is:
//   - Instant (<50ms) even on 100k+ file repos
//   - Respects .gitignore automatically
//   - Only returns source files (no build artifacts, no node_modules, etc.)
//
// Falls back to nil when git is not available or cwd is not a git repo.
// Results are cached per root directory for the session.
func gitLsFiles(root string) []string {
	gitLsFilesCacheMu.RLock()
	if gitLsFilesCacheDir == root && gitLsFilesCache != nil {
		result := gitLsFilesCache
		gitLsFilesCacheMu.RUnlock()
		return result
	}
	gitLsFilesCacheMu.RUnlock()

	// Check if git is available and we're in a repo
	if !isGitRepo(root) {
		return nil
	}

	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	// git ls-files -z outputs null-separated paths
	parts := strings.Split(string(out), "\x00")
	files := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		// Convert to forward slashes for consistency
		p = filepath.ToSlash(p)
		// Skip hidden files and sensitive files
		if strings.HasPrefix(p, ".") || isSensitiveRepoName(filepath.Base(p)) {
			continue
		}
		files = append(files, p)
	}

	// Cache the result
	gitLsFilesCacheMu.Lock()
	gitLsFilesCacheDir = root
	gitLsFilesCache = files
	gitLsFilesCacheMu.Unlock()

	return files
}


