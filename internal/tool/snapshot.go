package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// ── Snapshot lifecycle ─────────────────────────────────────────────────────
// Snapshot creates a `.bro_bak` copy before a risky edit; RestoreLastSnapshot
// reverts it; CleanupStaleSnapshots removes them. A snapshot lives EXACTLY ONE
// TURN: CleanupStaleSnapshots runs at the next real user prompt (the user
// moving on = changes accepted), so backups never accumulate on disk. The
// registry is bounded so a pathological session cannot grow memory either.
var (
	snapMu      sync.Mutex
	snapBackups []snapEntry // backup paths still live, most recent last
)

// snapEntry pairs a unique backup path with the original file it captured.
type snapEntry struct {
	backup string
	orig   string
}

const maxSnapBackups = 64

// snapSerial guarantees unique backup paths even when the same file is edited
// multiple times in one turn: every snapshot of file X gets its own
// .bro_bak.N path, so undoing N steps walks back through N distinct versions
// instead of overwriting the same backup.
var snapSerial int

// Snapshot captures the state of a file before modification. If the repo is a
// git repository, git already tracks the original — the manual backup is a
// failsafe for files git does not cover (untracked, ignored) and for the
// one-turn rollback window.
func Snapshot(filePath string) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil // New file, nothing to snapshot
	}

	snapMu.Lock()
	snapSerial++
	backupPath := fmt.Sprintf("%s.bro_bak.%d", filePath, snapSerial)
	snapBackups = append(snapBackups, snapEntry{backup: backupPath, orig: filePath})
	if len(snapBackups) > maxSnapBackups {
		snapBackups = snapBackups[len(snapBackups)-maxSnapBackups:]
	}
	snapMu.Unlock()

	if err := os.WriteFile(backupPath, content, 0o644); err != nil {
		return err
	}
	return nil
}

// CleanupStaleSnapshots removes every registered .bro_bak backup and returns
// how many were deleted. Called at the start of a real user turn (the tool
// loop's internal iterations never touch it): a snapshot is the one-turn
// rollback window; the user's next prompt is the accept signal.
func CleanupStaleSnapshots() int {
	snapMu.Lock()
	defer snapMu.Unlock()
	n := 0
	for _, e := range snapBackups {
		if os.Remove(e.backup) == nil {
			n++
		}
	}
	snapBackups = nil
	return n
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
