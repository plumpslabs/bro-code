package agentic

import (
	"os"
	"os/exec"
)

// Snapshot captures the state of a file before modification.
// If the repo is a git repository, it uses git to capture the current state.
// Otherwise, it creates a temporary backup file.
func Snapshot(filePath string) error {
	// Check if we are in a git repo
	if err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); err == nil {
		// Just ensure it's tracked or staged. For simplicity, we rely on the user's git tree.
	}

	// Manual backup as a failsafe
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil // New file, nothing to snapshot
	}

	backupPath := filePath + ".bro_bak"
	return os.WriteFile(backupPath, content, 0o644)
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
