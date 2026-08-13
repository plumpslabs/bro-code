package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	"github.com/plumpslabs/bro-code/internal/provider"
)

// Tool represents an executable native tool.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, argsJSON string) (string, error)
}

// Registry holds all registered tools.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry initializes default built-in tools.
func NewRegistry() *Registry {
	r := &Registry{tools: make(map[string]Tool)}
	r.Register(&ReadFileTool{})
	r.Register(&WriteFileTool{})
	r.Register(&EditFileTool{})
	r.Register(&ListDirTool{})
	r.Register(&GrepTool{})
	r.Register(&GlobTool{})
	r.Register(&BashTool{})
	return r
}

func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

func (r *Registry) Definitions() []provider.ToolDefinition {
	var defs []provider.ToolDefinition
	for _, t := range r.tools {
		defs = append(defs, provider.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return defs
}

func (r *Registry) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("tool '%s' not registered", name)
	}
	return t.Execute(ctx, argsJSON)
}

// ---------------- Built-in Tools ----------------

// ReadFileTool
type ReadFileTool struct{}

func (t *ReadFileTool) Name() string        { return "read_file" }
func (t *ReadFileTool) Description() string { return "Read file contents with optional line range" }
func (t *ReadFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string", "description": "Relative or absolute file path"},
			"start_line": map[string]any{"type": "integer", "description": "1-based start line (optional)"},
			"end_line":   map[string]any{"type": "integer", "description": "1-based end line (optional)"},
		},
		"required": []string{"path"},
	}
}
func (t *ReadFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	if args.StartLine > 0 || args.EndLine > 0 {
		start := args.StartLine - 1
		if start < 0 {
			start = 0
		}
		end := args.EndLine
		if end <= 0 || end > len(lines) {
			end = len(lines)
		}
		if start >= len(lines) {
			return "", fmt.Errorf("start_line out of bounds")
		}
		return strings.Join(lines[start:end], "\n"), nil
	}

	if len(lines) > 200 {
		head := strings.Join(lines[:100], "\n")
		return fmt.Sprintf("%s\n\n[file has %d lines, showing first 100 lines. Request line range for more]", head, len(lines)), nil
	}

	return string(data), nil
}

// WriteFileTool
type WriteFileTool struct{}

func (t *WriteFileTool) Name() string        { return "write_file" }
func (t *WriteFileTool) Description() string { return "Write or overwrite a file with new content" }
func (t *WriteFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Target file path"},
			"content": map[string]any{"type": "string", "description": "Complete file content"},
		},
		"required": []string{"path", "content"},
	}
}
func (t *WriteFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(args.Path), 0755); err != nil && filepath.Dir(args.Path) != "." {
		return "", err
	}

	if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		return "", err
	}
	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(args.Content), args.Path), nil
}

// EditFileTool
type EditFileTool struct{}

func (t *EditFileTool) Name() string        { return "edit_file" }
func (t *EditFileTool) Description() string { return "Edit target file by replacing old string content with new content" }
func (t *EditFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":        map[string]any{"type": "string", "description": "Target file path"},
			"target":      map[string]any{"type": "string", "description": "Exact text to replace"},
			"replacement": map[string]any{"type": "string", "description": "Replacement text"},
		},
		"required": []string{"path", "target", "replacement"},
	}
}
func (t *EditFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path        string `json:"path"`
		Target      string `json:"target"`
		Replacement string `json:"replacement"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}

	content := string(data)
	if !strings.Contains(content, args.Target) {
		// Fallback 1: Normalize CRLF line endings (\r\n -> \n)
		normContent := strings.ReplaceAll(content, "\r\n", "\n")
		normTarget := strings.ReplaceAll(args.Target, "\r\n", "\n")
		normReplacement := strings.ReplaceAll(args.Replacement, "\r\n", "\n")

		if strings.Contains(normContent, normTarget) {
			content = normContent
			args.Target = normTarget
			args.Replacement = normReplacement
		} else {
			return "", fmt.Errorf("target string not found in %s. Ensure exact line whitespace and text match by calling read_file first", args.Path)
		}
	}

	newContent := strings.Replace(content, args.Target, args.Replacement, 1)
	if err := os.WriteFile(args.Path, []byte(newContent), 0644); err != nil {
		return "", err
	}

	edits := myers.ComputeEdits(span.URIFromPath(args.Path), content, newContent)
	unified := gotextdiff.ToUnified(args.Path, args.Path, content, edits)
	return fmt.Sprintf("Successfully updated %s\nDiff:\n%s", args.Path, unified), nil
}

// ListDirTool
type ListDirTool struct{}

func (t *ListDirTool) Name() string        { return "list_dir" }
func (t *ListDirTool) Description() string { return "List directory contents" }
func (t *ListDirTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory path"},
		},
		"required": []string{"path"},
	}
}
func (t *ListDirTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		args.Path = "."
	}

	entries, err := os.ReadDir(args.Path)
	if err != nil {
		return "", err
	}

	var items []string
	for _, entry := range entries {
		suffix := ""
		if entry.IsDir() {
			suffix = "/"
		}
		items = append(items, entry.Name()+suffix)
	}

	return strings.Join(items, "\n"), nil
}

// GrepTool
type GrepTool struct{}

func (t *GrepTool) Name() string        { return "grep" }
func (t *GrepTool) Description() string { return "Search pattern in codebase (returns top 50 matches)" }
func (t *GrepTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Search pattern"},
			"path":    map[string]any{"type": "string", "description": "Search directory path"},
		},
		"required": []string{"pattern"},
	}
}
func (t *GrepTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		args.Path = "."
	}

	cmd := exec.CommandContext(ctx, "grep", "-rn", "-I", args.Pattern, args.Path)
	output, err := cmd.Output()
	if err != nil && len(output) == 0 {
		// Fallback to fixed strings match (-F) if regex pattern parsing failed
		cmdFixed := exec.CommandContext(ctx, "grep", "-rn", "-I", "-F", args.Pattern, args.Path)
		output, _ = cmdFixed.Output()
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return "No matches found.", nil
	}

	if len(lines) > 50 {
		head := strings.Join(lines[:50], "\n")
		return fmt.Sprintf("%s\n\n[showing 50/%d matches, refine query or ask for specific file]", head, len(lines)), nil
	}

	return strings.Join(lines, "\n"), nil
}

// GlobTool
type GlobTool struct{}

func (t *GlobTool) Name() string        { return "glob" }
func (t *GlobTool) Description() string { return "Find files matching pattern" }
func (t *GlobTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Glob pattern (e.g. *.go or internal/**/*.go)"},
		},
		"required": []string{"pattern"},
	}
}
func (t *GlobTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	matches, err := filepath.Glob(args.Pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "No matching files.", nil
	}
	return strings.Join(matches, "\n"), nil
}

// BashTool
type BashTool struct{}

func (t *BashTool) Name() string        { return "bash" }
func (t *BashTool) Description() string { return "Execute shell command" }
func (t *BashTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "Shell command to run"},
		},
		"required": []string{"command"},
	}
}
func (t *BashTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}

	// Basic safety filter for destructive commands
	lower := strings.ToLower(args.Command)
	if strings.Contains(lower, "rm -rf /") || strings.Contains(lower, "mkfs") || strings.Contains(lower, ":(){ :|:& };:") {
		return "", fmt.Errorf("prohibited destructive command")
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Sprintf("Command failed with error: %v\nOutput:\n%s", err, result), nil
	}
	if result == "" {
		return "Command executed successfully with no output.", nil
	}
	return result, nil
}
