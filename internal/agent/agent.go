package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumpslabs/bro-code/internal/hooks"
	"gopkg.in/yaml.v3"
)

// ToolFilter defines allowed and denied tool patterns for a custom agent.
type ToolFilter struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty" json:"deny,omitempty"`
}

// PermissionFilter defines allowed and denied bash command patterns.
type PermissionFilter struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty" json:"deny,omitempty"`
}

// CustomAgent represents a user-defined agent or custom mode.
type CustomAgent struct {
	Name        string            `yaml:"name" json:"name"`
	Description string            `yaml:"description" json:"description"`
	Mode        string            `yaml:"mode" json:"mode"` // BUILDER, PLANNER, MINER, SUBAGENT
	Model       string            `yaml:"model,omitempty" json:"model,omitempty"`
	Temperature *float64          `yaml:"temperature,omitempty" json:"temperature,omitempty"`
	Tools       ToolFilter        `yaml:"tools,omitempty" json:"tools,omitempty"`
	Permissions PermissionFilter  `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	Hooks       map[string]string `yaml:"hooks,omitempty" json:"hooks,omitempty"` // on_turn_start, on_turn_end, on_tool_call, on_tool_result, on_turn_error
	Prompt      string            `yaml:"-" json:"prompt"`
	Path        string            `yaml:"-" json:"path"`
	IsProject   bool              `yaml:"-" json:"is_project"`
}

// IsToolAllowed checks if a tool is permitted under this agent's tool filter.
func (a *CustomAgent) IsToolAllowed(toolName string) bool {
	if a == nil {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(toolName))

	// Check Deny rules first
	for _, d := range a.Tools.Deny {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "*" || d == name {
			return false
		}
		if ok, _ := filepath.Match(d, name); ok {
			return false
		}
	}

	// If Allow list is specified, tool must be in Allow list
	if len(a.Tools.Allow) > 0 {
		for _, al := range a.Tools.Allow {
			al = strings.ToLower(strings.TrimSpace(al))
			if al == "*" || al == name {
				return true
			}
			if ok, _ := filepath.Match(al, name); ok {
				return true
			}
		}
		return false
	}

	return true
}

// CheckCommand evaluates a bash command against the agent's permission filters.
func (a *CustomAgent) CheckCommand(cmd string) (allowed bool, denied bool) {
	if a == nil {
		return false, false
	}
	clean := strings.ToLower(strings.TrimSpace(cmd))

	for _, d := range a.Permissions.Deny {
		d = strings.ToLower(strings.TrimSpace(d))
		if ok, _ := filepath.Match(d, clean); ok {
			return false, true
		}
	}

	for _, al := range a.Permissions.Allow {
		al = strings.ToLower(strings.TrimSpace(al))
		if ok, _ := filepath.Match(al, clean); ok {
			return true, false
		}
	}

	return false, false
}

// ToHooks converts the agent's declarative hooks into hooks.Hook structs.
func (a *CustomAgent) ToHooks() []hooks.Hook {
	if a == nil || len(a.Hooks) == 0 {
		return nil
	}
	var out []hooks.Hook
	for eventName, cmd := range a.Hooks {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}
		var ev hooks.Event
		switch strings.ToLower(strings.ReplaceAll(eventName, "_", "-")) {
		case "on-turn-start":
			ev = hooks.EventTurnStart
		case "on-turn-end":
			ev = hooks.EventTurnEnd
		case "on-turn-error":
			ev = hooks.EventTurnError
		case "on-tool-call":
			ev = hooks.EventToolCall
		case "on-tool-result":
			ev = hooks.EventToolResult
		default:
			continue
		}
		out = append(out, hooks.Hook{
			Event:   ev,
			Command: cmd,
			Timeout: 30,
		})
	}
	return out
}

// GlobalAgentsDir returns ~/.config/brocode/agents.
func GlobalAgentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "brocode", "agents")
}

// ProjectAgentsDir returns <workspaceDir>/.brocode/agents.
func ProjectAgentsDir(workspaceDir string) string {
	if workspaceDir == "" {
		return ""
	}
	return filepath.Join(workspaceDir, ".brocode", "agents")
}

// Loader discovers and caches custom agents across project and global directories.
type Loader struct {
	agents []CustomAgent
}

// NewLoader loads all custom agents. Project-level agents override global ones with the same name.
func NewLoader(workspaceDir string) *Loader {
	return NewLoaderWithDirs(workspaceDir, GlobalAgentsDir())
}

// NewLoaderWithDirs initializes agent discovery across explicit project and global directories.
func NewLoaderWithDirs(workspaceDir, globalDir string) *Loader {
	l := &Loader{}

	// 1. Project level (<workspaceDir>/.brocode/agents/*.md)
	if pDir := ProjectAgentsDir(workspaceDir); pDir != "" {
		l.scanDir(pDir, true)
	}

	// 2. Global level (~/.config/brocode/agents/*.md)
	if globalDir != "" {
		l.scanDir(globalDir, false)
	}

	// 3. Fallback (~/.brocode/agents/*.md)
	if home, err := os.UserHomeDir(); err == nil && globalDir != "" {
		legacyDir := filepath.Join(home, ".brocode", "agents")
		if legacyDir != globalDir {
			l.scanDir(legacyDir, false)
		}
	}

	return l
}

func (l *Loader) scanDir(dirPath string, isProject bool) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			fullPath := filepath.Join(dirPath, entry.Name())
			if ag, err := ParseAgentFile(fullPath); err == nil {
				ag.IsProject = isProject
				if !l.hasAgent(ag.Name) {
					l.agents = append(l.agents, *ag)
				}
			}
		}
	}
}

func (l *Loader) hasAgent(name string) bool {
	for _, a := range l.agents {
		if strings.EqualFold(a.Name, name) {
			return true
		}
	}
	return false
}

// All returns all loaded custom agents.
func (l *Loader) All() []CustomAgent {
	return l.agents
}

// Find finds an agent by name (case-insensitive).
func (l *Loader) Find(name string) *CustomAgent {
	for i, a := range l.agents {
		if strings.EqualFold(a.Name, name) {
			return &l.agents[i]
		}
	}
	return nil
}

// ParseAgentFile reads and unmarshals a custom agent Markdown file with YAML frontmatter.
func ParseAgentFile(filePath string) (*CustomAgent, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	return ParseAgentContent(string(data), filePath)
}

// ParseAgentContent parses markdown content with YAML frontmatter into a CustomAgent.
func ParseAgentContent(content, filePath string) (*CustomAgent, error) {
	var ag CustomAgent
	baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	if baseName == "" || baseName == "." {
		baseName = "custom-agent"
	}
	ag.Name = baseName
	ag.Mode = "BUILDER"
	ag.Path = filePath

	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return &ag, nil
	}

	if strings.TrimSpace(lines[0]) == "---" {
		endIdx := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				endIdx = i
				break
			}
		}
		if endIdx > 0 {
			frontmatter := strings.Join(lines[1:endIdx], "\n")
			if err := yaml.Unmarshal([]byte(frontmatter), &ag); err != nil {
				return nil, fmt.Errorf("failed to parse YAML frontmatter: %w", err)
			}
			if endIdx+1 < len(lines) {
				ag.Prompt = strings.TrimSpace(strings.Join(lines[endIdx+1:], "\n"))
			}
		} else {
			ag.Prompt = strings.TrimSpace(content)
		}
	} else {
		ag.Prompt = strings.TrimSpace(content)
	}

	if ag.Name == "" {
		ag.Name = baseName
	}
	if ag.Mode == "" {
		ag.Mode = "BUILDER"
	}
	ag.Mode = strings.ToUpper(ag.Mode)

	return &ag, nil
}
