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

	// 5. Test /search-key command status and configuration
	m.handleSlashCommand("/search-key")
	lastMsg = m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "SEARCH:") {
		t.Fatalf("expected web search info message, got: %s", lastMsg)
	}

	m.handleSlashCommand("/search-key tvly-test12345678")
	lastMsg = m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "Configured Successfully") {
		t.Fatalf("expected key configured message, got: %s", lastMsg)
	}

	// Verify footer badge in View()
	viewStr := m.View().Content
	if !strings.Contains(viewStr, "🌐:Tavily") {
		t.Fatalf("expected '🌐:Tavily' in footer banner View(), got:\n%s", viewStr)
	}

	m.handleSlashCommand("/search-key clear")
	lastMsg = m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "Search Key Cleared") {
		t.Fatalf("expected key cleared message, got: %s", lastMsg)
	}

	viewStr = m.View().Content
	if !strings.Contains(viewStr, "🌐:Free") {
		t.Fatalf("expected '🌐:Free' in footer banner after clear, got:\n%s", viewStr)
	}

	// 6. Test /context7-key command status and configuration
	m.handleSlashCommand("/context7-key")
	lastMsg = m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "CONTEXT7:") {
		t.Fatalf("expected context7 info message, got: %s", lastMsg)
	}

	m.handleSlashCommand("/context7-key c7_test_sample123")
	lastMsg = m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "Configured Successfully") {
		t.Fatalf("expected c7 key configured message, got: %s", lastMsg)
	}

	m.handleSlashCommand("/context7-key clear")
	lastMsg = m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "Key Cleared") {
		t.Fatalf("expected c7 key cleared message, got: %s", lastMsg)
	}

	// 7. Test /trace and /provenance command
	m.handleSlashCommand("/trace")
	lastMsg = m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "PROVENANCE:") {
		t.Fatalf("expected PROVENANCE output from /trace, got: %s", lastMsg)
	}
}

func TestAllSlashCommandsDispatch(t *testing.T) {
	cmds := []string{
		"/help", "/h", "/?",
		"/cost", "/tokens", "/usage",
		"/mode", "/mode builder", "/mode planner", "/mode miner",
		"/builder", "/plan", "/planner", "/mine", "/miner",
		"/history",
		"/clear",
		"/diagnose",
		"/spec build a user login authentication service",
		"/tournament compare bubble sort and quick sort algorithms",
		"/repair",
		"/deps",
		"/provenance",
		"/verify",
		"/copy", "/y",
		"/mouse",
		"/lsp",
		"/skills",
		"/memory",
		"/ask what is Go interface?",
		"/trace",
		"/agent",
		"/debug",
	}

	for _, c := range cmds {
		t.Run(c, func(t *testing.T) {
			m := newTestApp()
			m.width = 120
			m.height = 40
			// Every slash command should dispatch without panicking or erroring unexpectedly
			_, cmd := m.handleSlashCommand(c)
			_ = cmd
			if len(m.messages) == 0 {
				t.Fatalf("command %q did not produce any message or state change", c)
			}
		})
	}
}

