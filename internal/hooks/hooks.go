// Package hooks provides lifecycle hooks for the agent loop, mirroring the
// pattern of Claude Code / opencode: the user defines shell commands that run
// at specific points in the turn (start, end, on error, before/after tool
// calls). Hooks let users enforce policies (git pull before each turn, notify
// on completion, deny certain tools, capture context) without touching code.
package hooks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Event is a lifecycle point where hooks can run.
type Event string

const (
	// EventTurnStart runs once when a turn begins (before the LLM call).
	EventTurnStart Event = "on-turn-start"
	// EventTurnEnd runs once when a turn finishes (answer produced).
	EventTurnEnd Event = "on-turn-end"
	// EventTurnError runs when a turn fails or is aborted.
	EventTurnError Event = "on-turn-error"
	// EventToolCall runs before each tool execution. Returning non-empty
	// output REPLACES the tool result (policy hooks can veto/override).
	EventToolCall Event = "on-tool-call"
	// EventToolResult runs after each tool execution with its output.
	EventToolResult Event = "on-tool-result"
)

// Hook is one configured command for an event.
type Hook struct {
	Event    Event    `json:"event"`
	Command  string   `json:"command"`
	Timeout  int      `json:"timeout,omitempty"`  // seconds, default 30
	Async    bool     `json:"async,omitempty"`    // fire-and-forget
	Blocking bool     `json:"blocking,omitempty"` // if false, run and continue
	Env      []string `json:"env,omitempty"`      // extra KEY=value
}

// Config is the hooks file format: {"hooks": [...]}.
type Config struct {
	Hooks []Hook `json:"hooks"`
}

// Manager loads and runs hooks.
type Manager struct {
	hooks []Hook
	mu    chan struct{} // serialize async runs (1 slot)
}

// Load reads .brocode/hooks.json (project) then ~/.config/brocode/hooks.json
// (global). Missing files are not errors — hooks are optional.
func Load(projectRoot string) *Manager {
	m := &Manager{mu: make(chan struct{}, 1)}

	paths := []string{
		filepath.Join(projectRoot, ".brocode", "hooks.json"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "brocode", "hooks.json"))
	}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}
		m.hooks = append(m.hooks, cfg.Hooks...)
	}
	return m
}

// ForEvent returns the hooks configured for a given event.
func (m *Manager) ForEvent(ev Event) []Hook {
	if m == nil {
		return nil
	}
	var out []Hook
	for _, h := range m.hooks {
		if h.Event == ev {
			out = append(out, h)
		}
	}
	return out
}

// Run executes all hooks for an event. For on-tool-call hooks, the returned
// string (from the last hook that produced output) can override the tool
// result; for other events the output is discarded.
func (m *Manager) Run(ctx context.Context, ev Event, data map[string]string) string {
	if m == nil {
		return ""
	}
	hooks := m.ForEvent(ev)
	if len(hooks) == 0 {
		return ""
	}

	override := ""
	for _, h := range hooks {
		if h.Async {
			go m.runOne(h, data)
			continue
		}
		out := m.runOne(h, data)
		if ev == EventToolCall && strings.TrimSpace(out) != "" {
			override = out
		}
	}
	return override
}

func (m *Manager) runOne(h Hook, data map[string]string) string {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", h.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", h.Command)
	}
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, h.Env...)
	for k, v := range data {
		cmd.Env = append(cmd.Env, "BROCODE_"+strings.ToUpper(k)+"="+v)
	}

	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "[hook timeout]"
	}
	if err != nil {
		return strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out))
}
