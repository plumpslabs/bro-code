package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/search"
)

func TestCheckpointCreateListRestore(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// Project files to snapshot.
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "app.js"), []byte("const a = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &CheckpointTool{}
	ctx := context.Background()

	// Create.
	out, err := tool.Execute(ctx, `{"action":"create","name":"before-rewrite"}`)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !strings.Contains(out, "before-rewrite") || !strings.Contains(out, "2 files") {
		t.Errorf("unexpected create output: %q", out)
	}

	// Modify the file, then restore.
	if err := os.WriteFile(filepath.Join(dir, "src", "app.js"), []byte("BROKEN CONTENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = tool.Execute(ctx, `{"action":"restore","name":"before-rewrite"}`)
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if !strings.Contains(out, "Restored") {
		t.Errorf("unexpected restore output: %q", out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "src", "app.js"))
	if string(data) != "const a = 1;\n" {
		t.Errorf("restore did not bring back original content: %q", string(data))
	}

	// List.
	out, err = tool.Execute(ctx, `{"action":"list"}`)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(out, "before-rewrite") {
		t.Errorf("expected checkpoint in list, got %q", out)
	}

	// Restore unknown → clear error.
	if _, err := tool.Execute(ctx, `{"action":"restore","name":"nope"}`); err == nil {
		t.Error("restoring unknown checkpoint should error")
	}

	// Invalid name rejected.
	if _, err := tool.Execute(ctx, `{"action":"create","name":"bad/name"}`); err == nil {
		t.Error("invalid checkpoint name should error")
	}
}

// TestCheckpointGitNative verifies that in a git repo the snapshot is
// git-native: the manifest records a commit SHA, git history/branches are
// untouched, and restore brings tracked files back exactly.
func TestCheckpointGitNative(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "test")

	// Baseline commit, then an uncommitted tracked change + an untracked file.
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("const a = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "app.js")
	git("commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("const a = 2;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "newfile.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &CheckpointTool{}
	out, err := tool.Execute(context.Background(), `{"action":"create","name":"snap"}`)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !strings.Contains(out, "git-native") || !strings.Contains(out, "sha") {
		t.Errorf("expected git-native create output, got %q", out)
	}

	// The user's branches/stash must be untouched.
	if branches := git("branch"); branches != "* master" && branches != "* main" {
		t.Errorf("git-native checkpoint polluted branches: %q", branches)
	}
	if stash := git("stash", "list"); stash != "" {
		t.Errorf("git-native checkpoint polluted stash: %q", stash)
	}

	// Corrupt the file, restore, verify exact tracked + untracked recovery.
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("BROKEN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "newfile.txt")); err != nil {
		t.Fatal(err)
	}
	out, err = tool.Execute(context.Background(), `{"action":"restore","name":"snap"}`)
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "app.js"))
	if string(data) != "const a = 2;\n" {
		t.Errorf("git restore did not bring back tracked content: %q", string(data))
	}
	if data, err := os.ReadFile(filepath.Join(dir, "newfile.txt")); err != nil || string(data) != "untracked\n" {
		t.Errorf("untracked file not restored: %q err=%v", string(data), err)
	}
	if !strings.Contains(out, "Restored") {
		t.Errorf("unexpected restore output: %q", out)
	}
}

func TestCheckpointDoesNotSelfSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &CheckpointTool{}
	ctx := context.Background()
	if _, err := tool.Execute(ctx, `{"action":"create","name":"cp1"}`); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if _, err := tool.Execute(ctx, `{"action":"create","name":"cp2"}`); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// cp1's snapshot must NOT contain cp2's files (no recursion).
	cp1Manifest, err := os.ReadFile(filepath.Join(dir, ".brocode", "checkpoints", "cp1", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cp1Manifest), "checkpoints") {
		t.Errorf(".brocode/checkpoints leaked into a snapshot: %s", string(cp1Manifest))
	}
}

func TestCodeLocateTool(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "service.js"), []byte("class PaymentService { charge() {} }\nmodule.exports = PaymentService;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "use.js"), []byte("const PaymentService = require('./service'); new PaymentService().charge();\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pure usage — no declaration — the true referencer.
	if err := os.WriteFile(filepath.Join(dir, "src", "call.js"), []byte("new PaymentService().charge();\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &CodeLocateTool{Index: search.BuildGlobalIndex(dir)}
	out, err := tool.Execute(context.Background(), `{"name":"PaymentService"}`)
	if err != nil {
		t.Fatalf("code_locate failed: %v", err)
	}
	if !strings.Contains(out, "service.js") || !strings.Contains(out, "Referenced by") {
		t.Errorf("unexpected locate output: %q", out)
	}
	if !strings.Contains(out, "call.js") {
		t.Errorf("expected call.js among referencers: %q", out)
	}
	// use.js imports the module (require) — visible as importer/occurrence.
	if !strings.Contains(out, "use.js") {
		t.Errorf("expected use.js in the report: %q", out)
	}

	// Missing index → clear error.
	broken := &CodeLocateTool{}
	if _, err := broken.Execute(context.Background(), `{"name":"x"}`); err == nil {
		t.Error("nil index should error")
	}
}
