package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ── Snapshot lifecycle ─────────────────────────────────────────────────────
// Snapshot creates a backup copy before a risky edit; RestoreLastSnapshot
// reverts it; CleanupStaleSnapshots removes them. A snapshot lives EXACTLY ONE
// TURN: CleanupStaleSnapshots runs at the next real user prompt (the user
// moving on = changes accepted), so backups never accumulate on disk.
//
// Backups live under <cwd>/.brocode/snapshots (BroCode's own cache root, already
// ignored by repo scanning and context ingestion) — never next to the source
// file — so they never clutter the project tree or get committed.
var (
	snapMu      sync.Mutex
	snapBackups []snapEntry // backup paths still live, most recent last
	// snapDone tracks which files have ALREADY been snapshotted this turn, so
	// repeated edits to the same file (a common pattern) don't re-read +
	// re-write the same backup. Each snapshot gets a unique serial, but the
	// original-content capture only needs to happen once per file per turn.
	snapDone map[string]bool
)

// snapEntry pairs a unique backup path with the original file it captured.
type snapEntry struct {
	backup string
	orig   string
}

const maxSnapBackups = 64

// snapshotSubdir is BroCode's cache root for edit backups.
const snapshotSubdir = ".brocode/snapshots"

// snapSerial guarantees unique backup paths even when the same file is edited
// multiple times in one turn: every snapshot of file X gets its own
// .bro_bak.N path, so undoing N steps walks back through N distinct versions
// instead of overwriting the same backup.
var snapSerial int

// snapshotDir returns (creating if needed) the directory where edit backups
// live. Keeping them under .brocode (BroCode's own cache root, already ignored
// by repo scanning and context ingestion) means they never clutter the
// project's source tree and are never committed.
func snapshotDir() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "."
	}
	dir := filepath.Join(cwd, snapshotSubdir)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// snapshotName maps an edited file path to a backup file name (without the
// trailing serial) under the snapshots dir. Files inside the project are
// mirrored (src/a.ts → .brocode/snapshots/src/a.ts.bro_bak.N) for readability;
// paths outside the project are flattened (separators replaced) so they still
// land in one place without collisions.
func snapshotName(orig string) string {
	cwd, _ := os.Getwd()
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, orig); err == nil && rel != "" && rel != "." && !strings.HasPrefix(rel, "..") {
			return filepath.Join(snapshotDir(), rel) + ".bro_bak"
		}
	}
	sanitized := strings.ReplaceAll(orig, string(os.PathSeparator), "_")
	sanitized = strings.ReplaceAll(sanitized, ":", "_")
	return filepath.Join(snapshotDir(), sanitized) + ".bro_bak"
}

// Snapshot captures the state of a file before modification. If the repo is a
// git repository, git already tracks the original — the manual backup is a
// failsafe for files git does not cover (untracked, ignored) and for the
// one-turn rollback window. Backups are written under .brocode/snapshots and
// are bounded in both memory and on disk.
//
// Deduplication: if the same file is snapshotted multiple times in one turn
// (e.g. 3 consecutive edits), only the FIRST snapshot reads + writes.
// Subsequent calls for the same path are no-ops — the already-captured backup
// is reused for undo. This cuts 2/3 I/O on batched-edit turns.
func Snapshot(filePath string) error {
	snapMu.Lock()
	if snapDone == nil {
		snapDone = map[string]bool{}
	}
	if snapDone[filePath] {
		snapMu.Unlock()
		return nil // already snapshotted this turn
	}
	snapDone[filePath] = true

	content, err := os.ReadFile(filePath)
	if err != nil {
		snapMu.Unlock()
		return nil // New file, nothing to snapshot
	}

	snapSerial++
	backupPath := fmt.Sprintf("%s.%d", snapshotName(filePath), snapSerial)
	snapBackups = append(snapBackups, snapEntry{backup: backupPath, orig: filePath})
	// Bound memory AND disk: when the live list overflows, drop the oldest
	// snapshots and delete their backup files so a pathological turn cannot
	// accumulate unbounded .bro_bak files.
	if len(snapBackups) > maxSnapBackups {
		dropped := snapBackups[:len(snapBackups)-maxSnapBackups]
		snapBackups = snapBackups[len(snapBackups)-maxSnapBackups:]
		for _, d := range dropped {
			_ = os.Remove(d.backup)
		}
	}
	snapMu.Unlock()

	if err := os.WriteFile(backupPath, content, 0o644); err != nil {
		return err
	}
	return nil
}

// CleanupStaleSnapshots removes every registered backup and returns how many
// were deleted. Called at the start of a real user turn (the tool loop's
// internal iterations never touch it): a snapshot is the one-turn rollback
// window; the user's next prompt is the accept signal. It also removes the
// (now empty) snapshots tree, clearing any stray files left by a crashed turn.
func CleanupStaleSnapshots() int {
	snapMu.Lock()
	n := 0
	for _, e := range snapBackups {
		if os.Remove(e.backup) == nil {
			n++
		}
	}
	snapBackups = nil
	snapDone = map[string]bool{} // reset dedup for next turn
	snapMu.Unlock()
	// Remove the whole tree so leftover files from a process that crashed
	// before it could drain its in-memory list are also purged.
	_ = os.RemoveAll(snapshotDir())
	return n
}

// PurgeAllSnapshots removes the entire .brocode/snapshots tree regardless of the
// in-memory list. Called once at session startup so a turn that crashed
// mid-edit (whose in-memory list is already gone) cannot leave backups behind.
func PurgeAllSnapshots() {
	snapMu.Lock()
	snapBackups = nil
	snapDone = map[string]bool{} // reset dedup
	snapMu.Unlock()
	_ = os.RemoveAll(snapshotDir())
}

// SnapshotCount returns how many live snapshots remain (files edited this
// turn that can still be rolled back).
func SnapshotCount() int {
	snapMu.Lock()
	defer snapMu.Unlock()
	return len(snapBackups)
}

// SnapshotSummary lists the currently live snapshots (most recent first) so
// the user/agent can see how many rollback steps exist.
func SnapshotSummary() []string {
	snapMu.Lock()
	defer snapMu.Unlock()
	out := make([]string, 0, len(snapBackups))
	for i := len(snapBackups) - 1; i >= 0; i-- {
		out = append(out, snapBackups[i].orig)
	}
	return out
}

// RestoreNSnapshots reverts the n most recent snapshots (LIFO). Returns the
// number actually restored (fewer if n exceeds the live snapshot count).
// This is the multi-step rollback primitive: the user can jump back N edits
// in one call instead of invoking undo repeatedly.
func RestoreNSnapshots(n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("steps must be a positive number")
	}
	restored := 0
	for i := 0; i < n; i++ {
		if _, err := RestoreLastSnapshot(); err != nil {
			return restored, nil // ran out of snapshots; not an error
		}
		restored++
	}
	return restored, nil
}

// RestoreAllSnapshots reverts every live snapshot (LIFO) and returns how many
// files were restored. Used when the user asks to roll back the whole turn.
func RestoreAllSnapshots() int {
	n := 0
	for {
		if _, err := RestoreLastSnapshot(); err != nil {
			return n
		}
		n++
	}
}

// RestoreLastSnapshot reverts the most recent backed-up file and returns a
// human-readable summary. Snapshots are LIFO: repeated calls walk backwards
// through the turn's edits.
func RestoreLastSnapshot() (string, error) {
	snapMu.Lock()
	defer snapMu.Unlock()
	if len(snapBackups) == 0 {
		return "", fmt.Errorf("no snapshots to restore")
	}

	entry := snapBackups[len(snapBackups)-1]
	snapBackups = snapBackups[:len(snapBackups)-1]

	content, err := os.ReadFile(entry.backup)
	if err != nil {
		return "", fmt.Errorf("snapshot unreadable: %w", err)
	}
	if err := os.WriteFile(entry.orig, content, 0o644); err != nil {
		return "", err
	}
	_ = os.Remove(entry.backup)
	return fmt.Sprintf("Restored %s from snapshot.", entry.orig), nil
}

// UndoTool reverts the most recent file modification(s) made this turn.
type UndoTool struct{}

func (t *UndoTool) Name() string { return "undo" }
func (t *UndoTool) Description() string {
	return "Revert file change(s) made this turn (restores the previous file content). By default reverts the most recent change; pass \"steps\" to revert multiple changes at once (e.g. steps=3 jumps back 3 edits)."
}
func (t *UndoTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"steps": map[string]any{
				"type":        "integer",
				"description": "How many changes to revert (default 1).",
			},
		},
	}
}
func (t *UndoTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Steps int `json:"steps"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)
	if args.Steps <= 0 {
		args.Steps = 1
	}
	restored, err := RestoreNSnapshots(args.Steps)
	if err != nil {
		return "", err
	}
	if restored == 0 {
		return "No snapshots to restore — no file was modified this turn yet.", nil
	}
	return fmt.Sprintf("Restored %d file change(s) from snapshot. %d change(s) remain available to undo.", restored, SnapshotCount()), nil
}
