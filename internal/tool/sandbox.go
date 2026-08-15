package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ── Sandbox permissions ────────────────────────────────────────────────────
// The sandbox is a granular per-tool permission policy that applies to EVERY
// tool (not just bash commands like the gate). It is configured in
// `.brocode/sandbox.json` (project) or `~/.config/brocode/sandbox.json`
// (global). Semantics:
//
//	deny:            tool names that are hard-blocked, no prompt.
//	allowOnly:       when non-empty, ONLY these tools are permitted — everything
//	                 else is blocked (least-privilege mode). Never includes the
//	                 interactive tools (ask_user/review_changes) or subagent/scout.
//	denyCommands:    substring patterns matched against bash commands and git
//	                 actions; a match hard-blocks the call (e.g. "git push --force").
//	allowCommands:   substring patterns that bypass denyCommands (more specific
//	                 allowance wins).
//
// If no sandbox config exists, everything behaves exactly as before (only the
// bash gate applies). The sandbox is advisory to the loop: a blocked tool call
// returns an explicit error the model can adapt around.

// ContainerSandbox routes every bash command through a Docker container for
// real OS-level isolation. The project root is mounted read-write at
// /workspace and commands run as `sh -c <cmd>` inside the chosen image, so a
// destructive or buggy command cannot touch the host. Opt-in via
// .brocode/sandbox.json:
//
//	{ "container": { "enabled": true, "image": "golang:1.23-alpine" } }
//
// The image should contain the toolchain the project needs (go, node, etc.).
// When enabled but docker is unavailable the tool errors clearly — it never
// silently falls back to running on the host (that would defeat the point).
type ContainerSandbox struct {
	Enabled bool   `json:"enabled"`
	Image   string `json:"image"`
}

// Sandbox is a parsed permission policy.
type Sandbox struct {
	Deny          []string          `json:"deny"`          // tool names blocked outright
	AllowOnly     []string          `json:"allowOnly"`     // if set, only these tools run
	DenyCommands  []string          `json:"denyCommands"`  // substrings blocked in bash/git
	AllowCommands []string          `json:"allowCommands"` // substrings that override denyCommands
	Container     *ContainerSandbox `json:"container"`     // when enabled, bash runs inside Docker
}

// LoadSandbox reads the first sandbox.json found (project then global).
// Missing/invalid files yield an empty (disabled) sandbox, never an error.
func LoadSandbox(projectRoot string) *Sandbox {
	s := &Sandbox{}
	paths := []string{
		filepath.Join(projectRoot, ".brocode", "sandbox.json"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "brocode", "sandbox.json"))
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if json.Unmarshal(data, s) == nil {
			return s
		}
	}
	return s
}

// Disabled reports whether no restrictions are configured.
func (s *Sandbox) Disabled() bool {
	return s == nil || (len(s.Deny) == 0 && len(s.AllowOnly) == 0 && len(s.DenyCommands) == 0)
}

// CheckTool evaluates the sandbox for a tool invocation. Returns the reason a
// call is blocked, or "" when it may proceed. deniedByPolicy distinguishes a
// hard sandbox block from a gate ask (so callers never prompt for a blocked
// tool).
func (s *Sandbox) CheckTool(name, argsJSON string) (reason string) {
	if s.Disabled() {
		return ""
	}

	// Interactive / recursive tools are never part of the allow-only set.
	if len(s.AllowOnly) > 0 {
		if !containsStr(s.AllowOnly, name) {
			return "tool " + name + " is not in the sandbox allow-only list"
		}
	}
	if containsStr(s.Deny, name) {
		return "tool " + name + " is denied by the sandbox"
	}

	// Command-level patterns (bash + git).
	switch name {
	case "bash":
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Command != "" {
			if r := s.checkCommand(args.Command); r != "" {
				return r
			}
		}
	case "git":
		var args struct {
			Action string `json:"action"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Action != "" {
			if r := s.checkCommand("git " + args.Action); r != "" {
				return r
			}
		}
	}
	return ""
}

func (s *Sandbox) checkCommand(cmd string) string {
	if len(s.DenyCommands) == 0 {
		return ""
	}
	low := strings.ToLower(cmd)
	for _, pat := range s.DenyCommands {
		if strings.Contains(low, strings.ToLower(pat)) {
			// A more specific allowance overrides the denial.
			for _, allow := range s.AllowCommands {
				if strings.Contains(low, strings.ToLower(allow)) {
					return ""
				}
			}
			return "command matches sandbox deny pattern: " + pat
		}
	}
	return ""
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
