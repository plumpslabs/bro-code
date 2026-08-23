package ui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/tool"
)

func TestAdvancedCommandsInApp(t *testing.T) {
	dir := t.TempDir()
	m := newTestApp()
	m.width = 120
	m.height = 40

	// 1. Test /diff when empty
	m.handleSlashCommand("/diff")
	lastMsg := m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "No file changes recorded") {
		t.Fatalf("expected no file changes message for empty /diff, got: %s", lastMsg)
	}

	// 2. Test /diff after a file change is recorded
	tool.RecordChange(tool.FileChange{
		Path:   filepath.Join(dir, "app.go"),
		Action: "modified",
		Old:    "func Hello() string {\n\treturn \"hello\"\n}\n",
		New:    "func Hello() string {\n\treturn \"hello world\"\n}\n",
	})
	m.handleSlashCommand("/diff")
	lastMsg = m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "DIFF:") {
		t.Fatalf("expected visual diff output, got: %s", lastMsg)
	}

	// 3. Test repairResultMsg in Update
	repMsg := "🔍 Root Cause: Nil pointer\n🛠️ Fixed in auth.go:12\n✅ Tests PASS"
	m.Update(repairResultMsg(repMsg))
	if m.status != "Ready" {
		t.Errorf("expected status 'Ready', got %q", m.status)
	}

	// 4. Test /worktree list on non-git dir
	m.handleSlashCommand("/worktree list")
	if m.status != "" && m.status != "Ready" {
		t.Fatalf("unexpected status: %s", m.status)
	}
}
