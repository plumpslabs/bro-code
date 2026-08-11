// Package diff wraps the Myers diff (O(ND)) for the edit tool.
//
// Why Myers instead of Levenshtein: Myers cost scales with the size of the
// DIFFERENCES (D), not the product of file sizes — this closes the RAM
// spike root cause that once made opencode blow up (see
// docs/PHILOSOPHY.md P1).
//
// Output is always a line-level unified diff — the edit tool sends hunks to
// the model, not whole files (saves tokens, deterministic).
package diff

import (
	"fmt"

	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
)

// Unified returns the unified diff between before and after.
// Old/new labels are only used for headers; the edit tool can pass paths.
func Unified(oldLabel, newLabel, before, after string) string {
	edits := myers.ComputeEdits(span.URI(oldLabel), before, after)
	u := gotextdiff.ToUnified(oldLabel, newLabel, before, edits)
	return fmt.Sprint(u)
}
