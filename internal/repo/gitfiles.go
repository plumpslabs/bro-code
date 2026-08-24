package repo

import (
	"github.com/plumpslabs/bro-code/internal/gitutil"
	"path/filepath"
)

// gitLsFiles returns tracked files via `git ls-files` as relative paths.
// Uses the shared gitutil package to avoid spawning duplicate git processes.
// On Windows, each exec.Command costs 50-200ms — sharing the process cache
// across repo + search packages eliminates ~400ms of startup overhead.
func gitLsFiles(root string) []string {
	rel := gitutil.LsFiles(root)
	if rel == nil {
		return nil
	}
	// Filter out sensitive files (repo-specific guard)
	files := make([]string, 0, len(rel))
	for _, p := range rel {
		if isSensitiveRepoName(filepath.Base(p)) {
			continue
		}
		files = append(files, p)
	}
	return files
}
