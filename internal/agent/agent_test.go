package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAgentContent(t *testing.T) {
	raw := `---
name: auditor
description: Security & Vulnerability Auditor
mode: PLANNER
model: deepseek-reasoner
temperature: 0.2
tools:
  allow: [read_file, grep, glob]
  deny: [edit_file, write_file]
permissions:
  allow:
    - "npm audit*"
    - "git log*"
  deny:
    - "git push*"
    - "rm -rf*"
hooks:
  on_turn_start: "git fetch origin"
  on_turn_end: "echo 'Done audit'"
---

# Security Auditor
You are an expert security auditor.
1. Inspect vulnerability reports.
2. Formulate audit matrices.
`
	ag, err := ParseAgentContent(raw, "/test/path/auditor.md")
	if err != nil {
		t.Fatalf("failed to parse agent content: %v", err)
	}

	if ag.Name != "auditor" {
		t.Errorf("expected name 'auditor', got %q", ag.Name)
	}
	if ag.Mode != "PLANNER" {
		t.Errorf("expected mode 'PLANNER', got %q", ag.Mode)
	}
	if ag.Model != "deepseek-reasoner" {
		t.Errorf("expected model 'deepseek-reasoner', got %q", ag.Model)
	}
	if ag.Temperature == nil || *ag.Temperature != 0.2 {
		t.Errorf("expected temperature 0.2, got %v", ag.Temperature)
	}
	if !strings.Contains(ag.Prompt, "# Security Auditor") {
		t.Errorf("expected prompt to contain heading, got: %s", ag.Prompt)
	}

	// Test Tool Permissions
	if !ag.IsToolAllowed("read_file") {
		t.Error("expected read_file to be allowed")
	}
	if !ag.IsToolAllowed("grep") {
		t.Error("expected grep to be allowed")
	}
	if ag.IsToolAllowed("edit_file") {
		t.Error("expected edit_file to be denied")
	}
	if ag.IsToolAllowed("bash") {
		t.Error("expected bash to be denied because not in allow list")
	}

	// Test Command Permissions
	allowed, denied := ag.CheckCommand("npm audit --json")
	if !allowed || denied {
		t.Errorf("expected 'npm audit --json' allowed=true denied=false, got allowed=%v denied=%v", allowed, denied)
	}

	allowed, denied = ag.CheckCommand("git push origin main")
	if allowed || !denied {
		t.Errorf("expected 'git push origin main' allowed=false denied=true, got allowed=%v denied=%v", allowed, denied)
	}

	// Test Hooks Conversion
	hooksList := ag.ToHooks()
	if len(hooksList) != 2 {
		t.Fatalf("expected 2 hooks, got %d", len(hooksList))
	}
}

func TestLoaderPrecedence(t *testing.T) {
	globalDir := t.TempDir()
	projectDir := t.TempDir()

	// Write global agent: "reviewer" (model: gpt-4o)
	globalFile := filepath.Join(globalDir, "reviewer.md")
	_ = os.WriteFile(globalFile, []byte(`---
name: reviewer
description: Global Reviewer
model: gpt-4o
---
Global Reviewer Prompt
`), 0o644)

	// Write project agent: "reviewer" (model: claude-3-5-sonnet)
	projectAgentsDir := filepath.Join(projectDir, ".brocode", "agents")
	_ = os.MkdirAll(projectAgentsDir, 0o755)
	projectFile := filepath.Join(projectAgentsDir, "reviewer.md")
	_ = os.WriteFile(projectFile, []byte(`---
name: reviewer
description: Project Reviewer Override
model: claude-3-5-sonnet
---
Project Reviewer Prompt
`), 0o644)

	loader := &Loader{}
	loader.scanDir(projectAgentsDir, true)
	loader.scanDir(globalDir, false)

	if len(loader.All()) != 1 {
		t.Fatalf("expected 1 agent due to deduplication, got %d", len(loader.All()))
	}

	ag := loader.Find("reviewer")
	if ag == nil {
		t.Fatal("expected reviewer agent to be found")
	}
	if ag.Model != "claude-3-5-sonnet" {
		t.Errorf("expected project agent override (claude-3-5-sonnet), got: %s", ag.Model)
	}
	if !ag.IsProject {
		t.Errorf("expected IsProject=true")
	}
}
