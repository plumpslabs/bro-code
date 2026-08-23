package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

func TestCustomAgentSlashCommands(t *testing.T) {
	tmpDir := t.TempDir()
	agentsDir := filepath.Join(tmpDir, ".brocode", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a test auditor agent
	auditorContent := `---
name: auditor
description: Security Auditor Test
mode: PLANNER
tools:
  allow: [read_file, grep]
  deny: [edit_file, write_file]
---
# Auditor Directives
You are a security auditor.
`
	if err := os.WriteFile(filepath.Join(agentsDir, "auditor.md"), []byte(auditorContent), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(origDir) }()

	ctx := bcontext.NewManager("test-model", nil, 4000)
	m := NewApp(provider.AppConfig{}, provider.DetectedProvider{}, "test-model", nil, tool.NewRegistry(), ctx, nil, nil, nil, 0, nil, nil, "⚡ test")

	// 1. Test /agents command
	m.handleSlashCommand("/agents")
	lastMsg := m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "auditor") || !strings.Contains(lastMsg, "Security Auditor Test") {
		t.Fatalf("expected /agents to list auditor, got: %s", lastMsg)
	}

	// 2. Test /agent auditor
	m.handleSlashCommand("/agent auditor")
	if m.activeAgent == nil || m.activeAgent.Name != "auditor" {
		t.Fatalf("expected active agent 'auditor', got: %v", m.activeAgent)
	}
	if m.mode != "PLANNER" {
		t.Fatalf("expected mode switched to PLANNER, got: %s", m.mode)
	}

	// 3. Test /agent reset
	m.handleSlashCommand("/agent reset")
	if m.activeAgent != nil {
		t.Fatalf("expected active agent nil after reset, got: %v", m.activeAgent)
	}
}
