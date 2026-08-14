package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CheckpointTool snapshots the project's source tree to a named, restorable
// checkpoint — git-style rollback without touching the user's git history.
// create → copy the working tree (git-tracked + untracked when in a repo,
// otherwise a vendor/heavy-dir aware walk) into .brocode/checkpoints/<name>.
// list → existing checkpoints. restore → copy a checkpoint's files back.
type CheckpointTool struct{}

func (t *CheckpointTool) Name() string { return "checkpoint" }
func (t *CheckpointTool) Description() string {
	return "Create, list, or restore named project checkpoints — roll back the whole working tree to a saved point (like a git checkpoint, but without touching git history). Actions: create (snapshot the current source tree), list (show saved checkpoints), restore (bring files back to a saved state). Use BEFORE a risky multi-file change so you can roll back everything at once. Files are stored under .brocode/checkpoints/."
}
func (t *CheckpointTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"create", "list", "restore"}, "description": "create = snapshot now, list = show checkpoints, restore = restore a checkpoint"},
			"name":   map[string]any{"type": "string", "description": "Checkpoint name (letters, digits, dash, underscore). Required for create and restore."},
		},
		"required": []string{"action"},
	}
}

func (t *CheckpointTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Action string `json:"action"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	name := strings.TrimSpace(args.Name)

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cpDir := filepath.Join(cwd, ".brocode", "checkpoints")

	switch action {
	case "create":
		if !validCheckpointName(name) {
			return "", fmt.Errorf("checkpoint name %q invalid — use letters, digits, dash or underscore", name)
		}
		return checkpointCreate(cwd, cpDir, name)
	case "list":
		return checkpointList(cpDir), nil
	case "restore":
		if !validCheckpointName(name) {
			return "", fmt.Errorf("checkpoint name %q invalid — use letters, digits, dash or underscore", name)
		}
		return checkpointRestore(cwd, cpDir, name)
	default:
		return "", fmt.Errorf("checkpoint action must be one of: create, list, restore (got %q)", action)
	}
}

func validCheckpointName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// checkpointFiles returns the project files to snapshot, relative to cwd.
// In a git repo it uses `git ls-files -co --exclude-standard` (tracked +
// untracked, respecting .gitignore); otherwise a walk skipping heavy dirs and
// .brocode itself.
func checkpointFiles(cwd string) ([]string, error) {
	if _, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command("git", "ls-files", "-co", "--exclude-standard", "-z")
		cmd.Dir = cwd
		if out, err := cmd.Output(); err == nil {
			var files []string
			for _, f := range strings.Split(string(out), "\x00") {
				if f != "" && !strings.Contains(f, ".brocode/") {
					files = append(files, f)
				}
			}
			return files, nil
		}
	}

	var files []string
	_ = filepath.WalkDir(cwd, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(cwd, path)
		if rerr != nil {
			return nil
		}
		if d.IsDir() {
			if rel != "." {
				name := d.Name()
				if name == ".brocode" || name == ".git" || name == "node_modules" || name == "vendor" ||
					name == "dist" || name == "build" || name == ".next" || name == "target" ||
					name == "__pycache__" || name == ".venv" || name == "venv" || name == "Pods" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if rel != "." {
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	return files, nil
}

func checkpointCreate(cwd, cpDir, name string) (string, error) {
	files, err := checkpointFiles(cwd)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "No files to snapshot.", nil
	}

	target := filepath.Join(cpDir, name)
	// Fresh checkpoint: replace any existing one with the same name.
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", err
	}

	manifest := struct {
		Name    string    `json:"name"`
		Created time.Time `json:"created"`
		Files   []string  `json:"files"`
	}{Name: name, Created: time.Now(), Files: files}

	bytes := 0
	for _, rel := range files {
		src := filepath.Join(cwd, rel)
		dst := filepath.Join(target, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		data, err := os.ReadFile(src)
		if err != nil {
			continue // file disappeared mid-run
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", err
		}
		bytes += len(data)
	}

	mj, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(target, "manifest.json"), mj, 0o644); err != nil {
		return "", err
	}

	// Prune to the 20 most recent checkpoints.
	pruned := pruneCheckpoints(cpDir, 20)

	return fmt.Sprintf("✅ Checkpoint %q created: %d files, %d bytes (%s). Restore anytime with checkpoint(action: \"restore\", name: \"%s\").%s", name, len(files), bytes, manifest.Created.Format(time.RFC3339), name, pruned), nil
}

func checkpointList(cpDir string) string {
	entries, err := os.ReadDir(cpDir)
	if err != nil {
		return "No checkpoints yet. Create one with checkpoint(action: \"create\", name: \"...\")."
	}
	var names []string
	byName := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		names = append(names, name)
		var m struct {
			Created time.Time `json:"created"`
			Files   []string  `json:"files"`
		}
		if data, err := os.ReadFile(filepath.Join(cpDir, name, "manifest.json")); err == nil {
			_ = json.Unmarshal(data, &m)
		}
		byName[name] = fmt.Sprintf("  • %-24s %d files  %s", name, len(m.Files), m.Created.Format("2006-01-02 15:04"))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "No checkpoints yet. Create one with checkpoint(action: \"create\", name: \"...\")."
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d checkpoint(s):\n", len(names)))
	for _, n := range names {
		sb.WriteString(byName[n] + "\n")
	}
	return strings.TrimSpace(sb.String())
}

func checkpointRestore(cwd, cpDir, name string) (string, error) {
	src := filepath.Join(cpDir, name)
	if _, err := os.Stat(filepath.Join(src, "manifest.json")); err != nil {
		return "", fmt.Errorf("checkpoint %q not found (use checkpoint(action: \"list\") to see saved checkpoints)", name)
	}
	var m struct {
		Files []string `json:"files"`
	}
	if data, err := os.ReadFile(filepath.Join(src, "manifest.json")); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	if len(m.Files) == 0 {
		return "", fmt.Errorf("checkpoint %q has an empty manifest", name)
	}

	restored := 0
	for _, rel := range m.Files {
		data, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			continue
		}
		dst := filepath.Join(cwd, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", err
		}
		restored++
	}
	return fmt.Sprintf("✅ Restored %d files from checkpoint %q.", restored, name), nil
}

// pruneCheckpoints keeps at most max checkpoints, removing the oldest by
// manifest creation time. Returns a note when anything was pruned.
func pruneCheckpoints(cpDir string, max int) string {
	entries, err := os.ReadDir(cpDir)
	if err != nil {
		return ""
	}
	type cp struct {
		name    string
		created time.Time
	}
	var list []cp
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c := cp{name: e.Name()}
		var m struct {
			Created time.Time `json:"created"`
		}
		if data, err := os.ReadFile(filepath.Join(cpDir, e.Name(), "manifest.json")); err == nil {
			_ = json.Unmarshal(data, &m)
		}
		c.created = m.Created
		list = append(list, c)
	}
	if len(list) <= max {
		return ""
	}
	sort.Slice(list, func(i, j int) bool { return list[i].created.Before(list[j].created) })
	var removed []string
	for _, c := range list[:len(list)-max] {
		_ = os.RemoveAll(filepath.Join(cpDir, c.name))
		removed = append(removed, c.name)
	}
	return fmt.Sprintf(" (pruned %d oldest: %s)", len(removed), strings.Join(removed, ", "))
}
