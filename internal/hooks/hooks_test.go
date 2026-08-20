package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a hooks.json with the given hooks into dir/.brocode and
// returns the dir.
func writeConfig(t *testing.T, dir string, json string) string {
	t.Helper()
	broDir := filepath.Join(dir, ".brocode")
	if err := os.MkdirAll(broDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broDir, "hooks.json"), []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadAndForEvent(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"hooks": [
		{"event": "on-turn-start", "command": "echo start"},
		{"event": "on-tool-call", "command": "echo tool"},
		{"event": "on-turn-end", "command": "echo end"}
	]}`)

	m := Load(dir)
	if got := len(m.ForEvent(EventTurnStart)); got != 1 {
		t.Fatalf("expected 1 on-turn-start hook, got %d", got)
	}
	if got := len(m.ForEvent(EventToolCall)); got != 1 {
		t.Fatalf("expected 1 on-tool-call hook, got %d", got)
	}
	if got := len(m.ForEvent(EventTurnError)); got != 0 {
		t.Fatalf("expected 0 on-turn-error hooks, got %d", got)
	}
}

func TestLoadMissingConfigIsNil(t *testing.T) {
	m := Load(t.TempDir())
	if m == nil {
		t.Fatal("Load must never return nil")
	}
	if got := m.Run(context.Background(), EventTurnStart, nil); got != "" {
		t.Fatalf("expected empty output with no hooks, got %q", got)
	}
}

func TestRunPassesEnvData(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"hooks": [
		{"event": "on-tool-call", "command": "echo BROCODE_QUERY=$BROCODE_QUERY"}
	]}`)

	m := Load(dir)
	out := m.Run(context.Background(), EventToolCall, map[string]string{"query": "hello"})
	if !strings.Contains(out, "BROCODE_QUERY=hello") {
		t.Fatalf("expected BROCODE_QUERY=hello in output, got %q", out)
	}
}

func TestToolCallHookOverrides(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"hooks": [
		{"event": "on-tool-call", "command": "echo VETOED:$(cat)"}
	]}`)

	m := Load(dir)
	out := m.Run(context.Background(), EventToolCall, map[string]string{"tool": "bash"})
	if !strings.Contains(out, "VETOED") {
		t.Fatalf("expected on-tool-call override output, got %q", out)
	}
}

func TestNonToolEventsDiscardOutput(t *testing.T) {
	// Output from non-tool-call events must not be returned.
	dir := t.TempDir()
	writeConfig(t, dir, `{"hooks": [
		{"event": "on-turn-end", "command": "echo done"}
	]}`)

	m := Load(dir)
	if out := m.Run(context.Background(), EventTurnEnd, nil); out != "" {
		t.Fatalf("expected empty return for on-turn-end, got %q", out)
	}
}

func TestAsyncHookDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"hooks": [
		{"event": "on-turn-start", "command": "sleep 5", "async": true}
	]}`)

	m := Load(dir)
	// Async hook returns immediately even though command sleeps.
	out := m.Run(context.Background(), EventTurnStart, nil)
	if out != "" {
		t.Fatalf("async hook should return empty immediately, got %q", out)
	}
}

func TestNilManagerSafe(t *testing.T) {
	var m *Manager
	if got := m.ForEvent(EventTurnStart); got != nil {
		t.Fatal("nil manager ForEvent must return nil")
	}
	if got := m.Run(context.Background(), EventTurnStart, nil); got != "" {
		t.Fatalf("nil manager Run must return empty, got %q", got)
	}
}

func TestTimeoutHook(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"hooks": [
		{"event": "on-tool-call", "command": "sleep 5", "timeout": 1}
	]}`)

	m := Load(dir)
	// Tool-call hooks return their output (override) so the timeout marker is
	// observable; non-tool-call events discard output by design.
	out := m.Run(context.Background(), EventToolCall, map[string]string{"tool": "bash"})
	if !strings.Contains(out, "[hook timeout]") {
		t.Fatalf("expected timeout marker, got %q", out)
	}
}
