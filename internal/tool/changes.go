package tool

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	bcontext "github.com/plumpslabs/bro-code/internal/context"
)

func init() {
	bcontext.FileChangesFormatter = func(payloadJSON string) string {
		var ch []FileChange
		if err := json.Unmarshal([]byte(payloadJSON), &ch); err == nil && len(ch) > 0 {
			return FileChangesMessage(ch)
		}
		return ""
	}
}

// FileChange records one file mutation made by a native tool during a turn,
// with the content before and after so the UI can render a +/- diff summary
// per file (created / modified / deleted) at the end of the response.
type FileChange struct {
	Path   string `json:"path"`
	Action string `json:"action"` // "created" | "modified" | "deleted"
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
}

// The recorder is package-level (like the snapshot registry) because tools are
// stateless shared instances: write_file/edit_file/delete_file record here
// regardless of which registry executed them, and the UI drains the list once
// per user turn (reset at turn start, taken at turn end). Sub-agent writes
// during a turn land in the same list, which is correct — they are real file
// changes the user should see.
var (
	changeMu sync.Mutex
	changes  []FileChange
)

// RecordChange appends a file mutation to the current turn's change list.
func RecordChange(c FileChange) {
	changeMu.Lock()
	defer changeMu.Unlock()
	changes = append(changes, c)
	if len(changes) > 512 {
		// Bound pathological turns (thousands of tiny writes) — the newest
		// changes are the ones worth summarizing.
		changes = changes[len(changes)-512:]
	}
}

// ResetChanges clears the turn's change list (called at user-turn start).
func ResetChanges() {
	changeMu.Lock()
	defer changeMu.Unlock()
	changes = nil
}

// TakeChanges returns the turn's recorded changes and clears the list (called
// at user-turn end, after the answer is appended).
func TakeChanges() []FileChange {
	changeMu.Lock()
	defer changeMu.Unlock()
	out := changes
	changes = nil
	return out
}

// ChangesLen returns how many changes are recorded this turn.
func ChangesLen() int {
	changeMu.Lock()
	defer changeMu.Unlock()
	return len(changes)
}

// PeekChanges returns a copy of the turn's recorded changes WITHOUT clearing
// the list (unlike TakeChanges). Used by review gates to size the diff.
func PeekChanges() []FileChange {
	changeMu.Lock()
	defer changeMu.Unlock()
	out := make([]FileChange, len(changes))
	copy(out, changes)
	return out
}

// FileChangesSep separates the compact per-file block from the full diff block
// inside a FILES message. The UI renders the compact part by default and the
// diff part when the user expands the block.
const FileChangesSep = "\n====DIFF====\n"

// FileChangesMessage builds the full FILES message for the turn's changes:
// compact per-file rows (one line each with action + line counts), a
// separator, then the per-file unified diff. The UI shows the compact rows
// collapsed and the diff when expanded.
func FileChangesMessage(ch []FileChange) string {
	if len(ch) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("FILES:\n")
	sb.WriteString(FileChangesCompact(ch))
	sb.WriteString(FileChangesSep)
	sb.WriteString(FileChangesDiff(ch))
	return strings.TrimSuffix(sb.String(), "\n")
}

// FileChangesCompact renders one line per file: action glyph, path and line
// counts — the collapsed view.
func FileChangesCompact(ch []FileChange) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📄 %d file(s) changed — ctrl+f expand/collapse\n", len(ch))
	for _, c := range ch {
		switch c.Action {
		case "created":
			fmt.Fprintf(&sb, "  ✚ %s  (created · %d lines)\n", c.Path, lineCount(c.New))
		case "deleted":
			fmt.Fprintf(&sb, "  ✖ %s  (deleted · %d lines)\n", c.Path, lineCount(c.Old))
		default:
			add, del := diffCounts(c.Old, c.New)
			fmt.Fprintf(&sb, "  ✎ %s  (modified · +%d −%d)\n", c.Path, add, del)
		}
	}
	return sb.String()
}

// FileChangesDiff renders the full per-file unified diff — the expanded view.
// Each file's diff is capped so a huge rewrite cannot flood the terminal.
const maxDiffLinesPerFile = 60

// FileChangesOneLine renders a single compact summary line for the activity
// slot: total file count, net line deltas, and a per-file breakdown. Used for
// the real-time "what just changed" HUD during a turn (P2 #2) — kept to one
// line so it never floods the term.
func FileChangesOneLine(ch []FileChange) string {
	if len(ch) == 0 {
		return ""
	}
	totalAdd, totalDel := 0, 0
	parts := make([]string, 0, len(ch))
	for _, c := range ch {
		var add, del int
		switch c.Action {
		case "created":
			add = lineCount(c.New)
		case "deleted":
			del = lineCount(c.Old)
		default:
			add, del = diffCounts(c.Old, c.New)
		}
		totalAdd += add
		totalDel += del
		base := c.Path
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		parts = append(parts, fmt.Sprintf("%s +%d −%d", base, add, del))
	}
	label := "file"
	if len(ch) > 1 {
		label = "files"
	}
	return fmt.Sprintf("%d %s · +%d −%d  (%s)", len(ch), label, totalAdd, totalDel, strings.Join(parts, " · "))
}

func FileChangesDiff(ch []FileChange) string {
	var sb strings.Builder
	for i, c := range ch {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(c.Path + "\n")
		var diff string
		switch c.Action {
		case "created":
			diff = addPrefixLines(c.New, "+")
		case "deleted":
			diff = addPrefixLines(c.Old, "-")
		default:
			diff = unifiedDiff(c.Path, c.Old, c.New)
		}
		lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
		if len(lines) > maxDiffLinesPerFile {
			lines = lines[:maxDiffLinesPerFile]
			lines = append(lines, fmt.Sprintf("  … +%d more lines", len(strings.Split(diff, "\n"))-maxDiffLinesPerFile))
		}
		sb.WriteString(strings.Join(lines, "\n") + "\n")
	}
	return sb.String()
}

// unifiedDiff returns a git-style unified diff between two contents (the same
// rendered form the edit_file tool returns, complete with +/- markers and
// hunk headers). Unified implements fmt.Formatter, so %s renders it.
func unifiedDiff(path, old, new string) string {
	edits := myers.ComputeEdits(span.URIFromPath(path), old, new)
	unified := gotextdiff.ToUnified("a/"+path, "b/"+path, old, edits)
	return strings.TrimSuffix(fmt.Sprintf("%s", unified), "\n")
}

// diffCounts counts added (+) and removed (−) lines between two contents by
// walking the unified diff lines.
func diffCounts(old, new string) (add, del int) {
	unified := unifiedDiff("diff", old, new)
	for _, l := range strings.Split(unified, "\n") {
		switch {
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
			add++
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
			del++
		}
	}
	return
}

// addPrefixLines prefixes every non-empty line with a marker (+ / -) for the
// created/deleted views.
func addPrefixLines(content, marker string) string {
	var sb strings.Builder
	for _, l := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		if l == "" {
			continue
		}
		sb.WriteString(marker + " " + l + "\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(s, "\n"), "\n"))
}
