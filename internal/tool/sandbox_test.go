package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/provider"
)

func TestSandboxDisabledByDefault(t *testing.T) {
	s := &Sandbox{}
	if !s.Disabled() {
		t.Error("empty sandbox must be disabled")
	}
	if reason := s.CheckTool("bash", `{"command":"rm -rf /"}`); reason != "" {
		t.Errorf("disabled sandbox must not block anything, got %q", reason)
	}
}

func TestSandboxDenyTool(t *testing.T) {
	s := &Sandbox{Deny: []string{"bash"}}
	if s.Disabled() {
		t.Error("sandbox with deny list must not be disabled")
	}
	if reason := s.CheckTool("bash", `{"command":"echo hi"}`); !strings.Contains(reason, "denied") {
		t.Errorf("expected deny reason, got %q", reason)
	}
	if reason := s.CheckTool("grep", `{"pattern":"x"}`); reason != "" {
		t.Errorf("non-denied tool should pass, got %q", reason)
	}
}

func TestSandboxAllowOnly(t *testing.T) {
	s := &Sandbox{AllowOnly: []string{"read_file", "grep", "glob"}}
	if reason := s.CheckTool("read_file", `{"path":"a.go"}`); reason != "" {
		t.Errorf("allowed tool should pass, got %q", reason)
	}
	if reason := s.CheckTool("bash", `{"command":"echo hi"}`); !strings.Contains(reason, "allow-only") {
		t.Errorf("bash must be blocked in allow-only mode, got %q", reason)
	}
}

func TestSandboxDenyCommands(t *testing.T) {
	s := &Sandbox{DenyCommands: []string{"git push --force", "rm -rf"}}
	if reason := s.CheckTool("bash", `{"command":"rm -rf ./dist"}`); !strings.Contains(reason, "deny pattern") {
		t.Errorf("expected deny pattern block, got %q", reason)
	}
	if reason := s.CheckTool("bash", `{"command":"git push origin main"}`); reason != "" {
		t.Errorf("plain push must pass, got %q", reason)
	}
	if reason := s.CheckTool("git", `{"action":"push --force origin main"}`); !strings.Contains(reason, "deny pattern") {
		t.Errorf("git force push must be blocked, got %q", reason)
	}
}

func TestSandboxAllowCommandsOverride(t *testing.T) {
	s := &Sandbox{
		DenyCommands:  []string{"rm -rf"},
		AllowCommands: []string{"rm -rf ./dist"},
	}
	if reason := s.CheckTool("bash", `{"command":"rm -rf ./dist"}`); reason != "" {
		t.Errorf("allow override should let it pass, got %q", reason)
	}
	if reason := s.CheckTool("bash", `{"command":"rm -rf ./src"}`); !strings.Contains(reason, "deny pattern") {
		t.Errorf("non-overridden path must stay blocked, got %q", reason)
	}
}

func TestLoadSandboxFromProject(t *testing.T) {
	dir := t.TempDir()
	broDir := filepath.Join(dir, ".brocode")
	os.MkdirAll(broDir, 0o755)
	os.WriteFile(filepath.Join(broDir, "sandbox.json"), []byte(`{"deny":["bash"],"denyCommands":["git push"]}`), 0o644)

	s := LoadSandbox(dir)
	if s == nil {
		t.Fatal("LoadSandbox returned nil")
	}
	if reason := s.CheckTool("bash", `{"command":"ls"}`); reason == "" {
		t.Error("bash should be denied from loaded config")
	}
}

func TestLoadSandboxMissingIsDisabled(t *testing.T) {
	s := LoadSandbox(t.TempDir())
	if !s.Disabled() {
		t.Error("sandbox with no config file must be disabled")
	}
}

func TestRegistrySandboxBlocksBeforeGate(t *testing.T) {
	r := NewRegistry()
	r.SetSandbox(&Sandbox{Deny: []string{"read_file"}})

	approved, reason, err := r.GateAction(context.Background(), provider.ToolCall{
		Name:      "read_file",
		Arguments: `{"path":"x.go"}`,
	})
	if err != nil {
		t.Fatalf("GateAction: %v", err)
	}
	if approved {
		t.Error("read_file must be denied by sandbox")
	}
	if !strings.Contains(reason, "denied") {
		t.Errorf("reason = %q, want sandbox denial", reason)
	}
}
