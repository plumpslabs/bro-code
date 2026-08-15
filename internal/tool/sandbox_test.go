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

func TestSandboxParseContainerConfig(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".brocode"), 0755)
	if err := os.WriteFile(filepath.Join(dir, ".brocode", "sandbox.json"),
		[]byte(`{"container":{"enabled":true,"image":"golang:1.23-alpine"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	s := LoadSandbox(dir)
	if s.Container == nil || !s.Container.Enabled {
		t.Fatal("container config not parsed")
	}
	if s.Container.Image != "golang:1.23-alpine" {
		t.Errorf("image = %q, want golang:1.23-alpine", s.Container.Image)
	}
	// Container-only config is otherwise a disabled sandbox (no deny rules).
	if !s.Disabled() {
		t.Error("a container-only sandbox with no deny rules should report disabled for permission purposes")
	}
}

func TestContainerRunArgs(t *testing.T) {
	args := containerRunArgs("/proj", "alpine:3.20", "echo hi")
	want := []string{"run", "--rm", "-v", "/proj:/workspace", "-w", "/workspace", "alpine:3.20", "sh", "-c", "echo hi"}
	if len(args) != len(want) {
		t.Fatalf("got %d args %v, want %d %v", len(args), args, len(want), want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	// No workdir → no mount flags.
	args = containerRunArgs("", "alpine", "ls")
	for _, a := range args {
		if a == "-v" || a == "/workspace" {
			t.Errorf("unexpected mount arg %q", a)
		}
	}
}

func TestBashContainerMissingDockerErrorsNoFallback(t *testing.T) {
	// When docker is genuinely unavailable (no binary on PATH) the container
	// sandbox must produce a clear error — never silently execute on the host.
	// PATH is pointed at an empty dir so exec.LookPath("docker") fails
	// regardless of what is installed on the machine.
	t.Setenv("PATH", t.TempDir())

	work := t.TempDir()
	bt := &BashTool{
		Container: &ContainerSandbox{Enabled: true, Image: "alpine:3.20"},
		WorkDir:   work,
	}
	out, err := bt.Execute(context.Background(), `{"command":"touch host-marker.txt"}`)
	if err == nil {
		t.Fatalf("expected error when docker missing, got output %q", out)
	}
	if strings.Contains(out, "host-marker") {
		t.Fatal("command ran on the host despite container sandbox — silent fallback is forbidden")
	}
	if !strings.Contains(err.Error(), "docker is not installed") {
		t.Errorf("unexpected error message: %v", err)
	}
	// The marker must NOT exist on the host.
	if _, statErr := os.Stat(filepath.Join(work, "host-marker.txt")); statErr == nil {
		t.Error("host-marker.txt was created — container sandbox bypassed")
	}
}

func TestBashContainerRunsThroughDockerNotHost(t *testing.T) {
	// A fake `docker` script on PATH proves every command goes through the
	// container path (never the host shell) and its failure is surfaced, not
	// masked by a silent fallback. The fake only echoes its argv and fails, so
	// the ONLY way the output contains FAKE_DOCKER_INVOKED is if the command
	// was handed to docker rather than run by the host shell.
	fakeBin := t.TempDir()
	fakeDocker := filepath.Join(fakeBin, "docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\necho FAKE_DOCKER_INVOKED \"$@\"\nexit 7\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	work := t.TempDir()
	bt := &BashTool{
		Container: &ContainerSandbox{Enabled: true, Image: "alpine:3.20"},
		WorkDir:   work,
	}
	out, err := bt.Execute(context.Background(), `{"command":"touch inside-container"}`)
	if err != nil {
		t.Fatalf("container path returned error: %v", err)
	}
	if !strings.Contains(out, "FAKE_DOCKER_INVOKED") {
		t.Fatalf("command did NOT go through docker: %q", out)
	}
	if !strings.Contains(out, "/workspace") || !strings.Contains(out, "alpine:3.20") {
		t.Errorf("docker invocation missing mount/image args: %q", out)
	}
	if !strings.Contains(out, "Container command failed") {
		t.Errorf("docker failure (exit 7) was not surfaced: %q", out)
	}
	if strings.Contains(out, "Command executed successfully") {
		t.Error("host execution path ran — container sandbox bypassed")
	}
	// No marker on the host: the fake docker never actually ran the command.
	if _, statErr := os.Stat(filepath.Join(work, "inside-container")); statErr == nil {
		t.Error("host file created — container sandbox bypassed")
	}
}

func TestBashDirectExecStillWorksWhenNoContainer(t *testing.T) {
	// Without a container sandbox the bash tool behaves exactly as before.
	bt := &BashTool{}
	out, err := bt.Execute(context.Background(), `{"command":"echo sandbox-off"}`)
	if err != nil {
		t.Fatalf("bash direct exec failed: %v", err)
	}
	if !strings.Contains(out, "sandbox-off") {
		t.Errorf("direct exec output = %q, want sandbox-off", out)
	}
}
