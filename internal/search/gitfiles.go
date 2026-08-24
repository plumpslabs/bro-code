package search

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// gitLsFiles returns tracked files via `git ls-files` as absolute paths.
// Falls back to nil when git is unavailable or cwd is not a git repo.
// Used by BuildGlobalIndex to skip the slow filepath.WalkDir on git repos.
func gitLsFiles(root string) []string {
	// Check if git is available and we're in a repo
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		return nil
	}

	cmd = exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
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
		// Convert to absolute path
		abs := filepath.Join(root, filepath.ToSlash(p))
		// Skip binary extensions and sensitive files
		ext := filepath.Ext(abs)
		if IsBinaryExt(ext) || isSensitiveName(filepath.Base(abs)) {
			continue
		}
		files = append(files, abs)
	}
	return files
}
