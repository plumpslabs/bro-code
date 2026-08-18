package lsp

import (
	"slices"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/plumpslabs/bro-code/internal/tool"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// applyWorkspaceEdit applies a language server's WorkspaceEdit to disk and
// records every mutation through tool.RecordChange, so the turn's diff
// summary, undo snapshots and verification gates all see the LSP-driven edits.
// It supports the classic `changes` shape and the newer `documentChanges`
// text-edit shape; create/rename/delete-file operations are rejected with a
// clear error — the agent should do those itself with write_file/delete_file.
// Returns a per-file summary, or "no changes needed" when edits were no-ops.
func applyWorkspaceEdit(we *protocol.WorkspaceEdit) (string, error) {
	if we == nil {
		return "", fmt.Errorf("server returned no edits")
	}

	// Aggregate text edits per URI from both shapes. A document's edits are
	// non-overlapping but not guaranteed sorted; they are applied end-first
	// so earlier offsets stay valid (standard LSP client convention).
	perURI := make(map[uri.URI][]protocol.TextEdit)
	for u, edits := range we.Changes {
		perURI[u] = append(perURI[u], edits...)
	}
	for _, dc := range we.DocumentChanges {
		switch op := dc.(type) {
		case *protocol.TextDocumentEdit:
			for _, el := range op.Edits {
				if te, ok := el.(*protocol.TextEdit); ok {
					perURI[op.TextDocument.URI] = append(perURI[op.TextDocument.URI], *te)
				}
			}
		case *protocol.CreateFile:
			return "", fmt.Errorf("code action wants to create %s — do it yourself with write_file instead", op.URI)
		case *protocol.RenameFile:
			return "", fmt.Errorf("code action wants to rename %s → %s — do it yourself with write_file/delete_file instead", op.OldURI, op.NewURI)
		case *protocol.DeleteFile:
			return "", fmt.Errorf("code action wants to delete %s — do it yourself with delete_file instead", op.URI)
		default:
			return "", fmt.Errorf("unsupported workspace edit operation %T", dc)
		}
	}
	if len(perURI) == 0 {
		return "", fmt.Errorf("server returned an empty edit")
	}

	order := make([]uri.URI, 0, len(perURI))
	for u := range perURI {
		order = append(order, u)
	}
	slices.Sort(order)

	var updated []string
	for _, u := range order {
		path := u.FsPath()
		if path == "" {
			return "", fmt.Errorf("cannot apply edit: URI %s has no filesystem path", u)
		}
		old, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot apply edit to %s: %w", path, err)
		}
		newContent := applyTextEdits(string(old), perURI[u])
		if newContent == string(old) {
			continue
		}
		// One-turn rollback window, matching write_file/edit_file.
		_ = tool.Snapshot(path)
		if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		tool.RecordChange(tool.FileChange{Path: path, Action: "modified", Old: string(old), New: newContent})
		updated = append(updated, path)
	}
	if len(updated) == 0 {
		return "No changes needed (edits produced identical content).", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Applied LSP edits to %d file(s):\n", len(updated))
	for _, p := range updated {
		fmt.Fprintf(&sb, "  • %s\n", p)
	}
	return strings.TrimSpace(sb.String()), nil
}

// applyTextEdits applies non-overlapping text edits to content, end-first, and
// returns the result. LSP positions are UTF-16 code-unit offsets, so character
// offsets are converted to byte offsets via utf16ToByte.
func applyTextEdits(content string, edits []protocol.TextEdit) string {
	if len(edits) == 0 {
		return content
	}
	sorted := append([]protocol.TextEdit(nil), edits...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].Range.Start, sorted[j].Range.Start
		if a.Line != b.Line {
			return a.Line > b.Line
		}
		return a.Character > b.Character
	})

	lines := strings.Split(content, "\n")
	for _, ed := range sorted {
		start := int(ed.Range.Start.Line)
		end := int(ed.Range.End.Line)
		if start < 0 || start >= len(lines) {
			continue
		}
		if end < start {
			end = start
		}
		if end >= len(lines) {
			end = len(lines) - 1
		}
		startByte := utf16ToByte(lines[start], int(ed.Range.Start.Character))
		endByte := utf16ToByte(lines[end], int(ed.Range.End.Character))

		var sb strings.Builder
		sb.WriteString(lines[start][:startByte])
		sb.WriteString(ed.NewText)
		if end > start {
			sb.WriteString(lines[end][endByte:])
		} else {
			sb.WriteString(lines[start][endByte:])
		}
		replacement := strings.Split(sb.String(), "\n")
		newLines := make([]string, 0, len(lines)-(end-start)+len(replacement))
		newLines = append(newLines, lines[:start]...)
		newLines = append(newLines, replacement...)
		newLines = append(newLines, lines[end+1:]...)
		lines = newLines
	}
	return strings.Join(lines, "\n")
}





// utf16ToByte converts a UTF-16 code-unit offset in s to a byte offset,
// clamping at the end of the string. LSP character offsets are UTF-16 code
// units, so a surrogate pair counts as two units while occupying 4 bytes;
// a position falling mid-pair clamps to the start of that pair.
func utf16ToByte(s string, units int) int {
	if units <= 0 {
		return 0
	}
	consumed := 0
	for i, r := range s {
		step := 1
		if r >= 0x10000 {
			step = 2
		}
		if consumed+step > units {
			return i
		}
		consumed += step
	}
	return len(s)
}
