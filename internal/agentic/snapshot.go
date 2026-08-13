package agentic

import (
	"os"
	"sync"
)

// ── Snapshot lifecycle ─────────────────────────────────────────────────────
// Snapshot creates a `.bro_bak` copy before a risky edit; Restore reverts it;
// Cleanup removes it. A snapshot lives EXACTLY ONE TURN: CleanupStaleSnapshots
// runs at the next real user prompt (the user moving on = changes accepted),
// so backups never accumulate on disk (audit finding B1). The registry is
// bounded so a pathological session cannot grow memory either.
var (
	snapMu      sync.Mutex
	snapBackups []string // backup paths still live, most recent last
)

const maxSnapBackups = 64

// Snapshot captures the state of a file before modification. If the repo is a
// git repository, git already tracks the original — the manual backup is a
// failsafe for files git does not cover (untracked, ignored) and for the
// one-turn rollback window.
func Snapshot(filePath string) error {
	// Manual backup as a failsafe
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil // New file, nothing to snapshot
	}

	backupPath := filePath + ".bro_bak"
	if err := os.WriteFile(backupPath, content, 0o644); err != nil {
		return err
	}
	snapMu.Lock()
	snapBackups = append(snapBackups, backupPath)
	if len(snapBackups) > maxSnapBackups {
		snapBackups = snapBackups[len(snapBackups)-maxSnapBackups:]
	}
	snapMu.Unlock()
	return nil
}

// CleanupStaleSnapshots removes every registered .bro_bak backup and returns
// how many were deleted. Called at the start of a real user turn (the tool
// loop's re-sends never touch it): a snapshot is the one-turn rollback window;
// the user's next prompt is the accept signal. Safe to call with none live.
func CleanupStaleSnapshots() int {
	snapMu.Lock()
	defer snapMu.Unlock()
	n := 0
	for _, p := range snapBackups {
		if os.Remove(p) == nil {
			n++
		}
	}
	snapBackups = nil
	return n
}

// Restore reverts a file to its snapshotted state.
func Restore(filePath string) error {
	backupPath := filePath + ".bro_bak"
	content, err := os.ReadFile(backupPath)
	if err != nil {
		// Backup doesn't exist, maybe it was a new file?
		return os.Remove(filePath)
	}

	err = os.WriteFile(filePath, content, 0o644)
	if err == nil {
		os.Remove(backupPath)
	}
	return err
}

// Cleanup removes the snapshot without restoring.
func Cleanup(filePath string) {
	backupPath := filePath + ".bro_bak"
	os.Remove(backupPath)
}
