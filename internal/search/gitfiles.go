package search

import (
	"path/filepath"

	"github.com/plumpslabs/bro-code/internal/gitutil"
)

// gitLsFiles returns tracked files via `git ls-files` as absolute paths.
// Uses the shared gitutil package to avoid spawning duplicate git processes.
func gitLsFiles(root string) []string {
	abs := gitutil.LsFilesAbs(root)
	if abs == nil {
		return nil
	}
	// Filter out binary extensions and sensitive files (search-specific guard)
	files := make([]string, 0, len(abs))
	for _, p := range abs {
		ext := filepath.Ext(p)
		if IsBinaryExt(ext) || isSensitiveName(filepath.Base(p)) {
			continue
		}
		files = append(files, p)
	}
	return files
}
