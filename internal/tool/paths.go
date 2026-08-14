package tool

import (
	"os"
	"strings"
)

// resolvePath makes a model-supplied path usable even when it was written
// with a leading slash ("/crm_sales_backend/src" — a very common LLM habit
// that treats the path as repo-rooted). Such a path would otherwise resolve
// against the filesystem root and fail: grep returns "no matches", read_file
// errors, and the model burns rounds being confused ("this is strange, the
// file is empty?") until the tool budget aborts the turn.
//
// Resolution rule: if the path starts with "/" but does not exist as an
// absolute path, try the leading-slash-stripped form relative to the process
// cwd (the project root). Genuine absolute paths that exist are kept
// untouched.
func resolvePath(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}
	rel := strings.TrimPrefix(p, "/")
	if rel == "" {
		return p
	}
	if _, err := os.Stat(rel); err == nil {
		return rel
	}
	return p
}
