package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/plumpslabs/bro-code/internal/search"
)

// CodeOutlineTool extracts structural symbols with line ranges and call info.
type CodeOutlineTool struct{}

func (t *CodeOutlineTool) Name() string { return "code_outline" }
func (t *CodeOutlineTool) Description() string {
	return "Extract structural symbols, line ranges, and call graph outline for a file. Essential for large/monolithic files (1000+ lines) to inspect structure with zero token waste."
}
func (t *CodeOutlineTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to outline",
			},
		},
		"required": []string{"path"},
	}
}
func (t *CodeOutlineTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	args.Path = resolvePath(args.Path)

	syms, err := search.OutlineFile(args.Path)
	if err != nil {
		return "", fmt.Errorf("failed to outline %s: %w", args.Path, err)
	}
	if len(syms) == 0 {
		return fmt.Sprintf("No structural symbols extracted from %s.", args.Path), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📑 Symbol Outline for %s (%d symbols total):\n", args.Path, len(syms))
	for _, s := range syms {
		callsStr := ""
		if len(s.Calls) > 0 {
			callsStr = fmt.Sprintf(" -> calls: [%s]", strings.Join(s.Calls, ", "))
		}
		docStr := ""
		if s.Doc != "" {
			firstLine := strings.Split(s.Doc, "\n")[0]
			docStr = fmt.Sprintf(" // %s", firstLine)
		}
		fmt.Fprintf(&sb, "  [L%d-L%d] %-7s %s%s%s\n", s.StartLine, s.EndLine, s.Kind, s.Signature, callsStr, docStr)
	}

	return CapOutput(sb.String()), nil
}

// CodeImpactTool analyzes blast radius and caller/callee dependencies before editing.
type CodeImpactTool struct{}

func (t *CodeImpactTool) Name() string { return "code_impact" }
func (t *CodeImpactTool) Description() string {
	return "Analyze the blast radius, callers, callees, and safe refactoring sequence for a symbol before modifying or moving it across files."
}
func (t *CodeImpactTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"symbol": map[string]any{
				"type":        "string",
				"description": "Symbol name to analyze (function, method, struct, class)",
			},
			"file": map[string]any{
				"type":        "string",
				"description": "Optional file path where the symbol is defined",
			},
		},
		"required": []string{"symbol"},
	}
}
func (t *CodeImpactTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Symbol string `json:"symbol"`
		File   string `json:"file"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Symbol) == "" {
		return "", fmt.Errorf("symbol is required")
	}
	if args.File != "" {
		args.File = resolvePath(args.File)
	}

	cwd, _ := os.Getwd()
	report, err := search.AnalyzeImpact(cwd, args.Symbol, args.File)
	if err != nil {
		return "", fmt.Errorf("failed to analyze impact for %s: %w", args.Symbol, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🎯 Blast Radius Analysis: %s\n", report.Symbol)
	if report.File != "" {
		fmt.Fprintf(&sb, "  📍 Defined in: %s [L%d-L%d]\n", report.File, report.StartLine, report.EndLine)
	}
	fmt.Fprintf(&sb, "  ⚠️  Blast Radius: %s\n", report.BlastRadius)
	fmt.Fprintf(&sb, "  📊 Fan-in (Callers): %d | Fan-out (Dependencies): %d\n", report.FanIn, report.FanOut)
	if len(report.Callees) > 0 {
		fmt.Fprintf(&sb, "  🔗 Internal Calls: %s\n", strings.Join(report.Callees, ", "))
	}
	fmt.Fprintf(&sb, "  🪜 Safe Protocol: %s\n\n", report.Extraction)

	if len(report.Callers) > 0 {
		fmt.Fprintf(&sb, "📍 Identified Call Sites (%d total):\n", len(report.Callers))
		for i, c := range report.Callers {
			if i >= 15 {
				fmt.Fprintf(&sb, "  ... and %d more call sites\n", len(report.Callers)-15)
				break
			}
			fmt.Fprintf(&sb, "  - %s:%d: %s\n", c.File, c.Line, c.Snippet)
		}
	} else {
		fmt.Fprintf(&sb, "✅ No external callers found. Completely safe to refactor or move in isolation.\n")
	}

	return CapOutput(sb.String()), nil
}

// RefactorClusterTool groups functions in a large file into cohesive target modules.
type RefactorClusterTool struct{}

func (t *RefactorClusterTool) Name() string { return "refactor_cluster" }
func (t *RefactorClusterTool) Description() string {
	return "Perform modularity clustering on a large/monolithic file. Groups tightly coupled functions into logical target modules to guide safe refactoring."
}
func (t *RefactorClusterTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the monolithic file to cluster",
			},
		},
		"required": []string{"path"},
	}
}
func (t *RefactorClusterTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	args.Path = resolvePath(args.Path)

	clusters, err := search.ClusterFileSymbols(args.Path)
	if err != nil {
		return "", fmt.Errorf("failed to cluster %s: %w", args.Path, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🧩 Modularity Clustering for %s (%d clusters recommended):\n\n", args.Path, len(clusters))

	for _, c := range clusters {
		fmt.Fprintf(&sb, "📦 Cluster %d: %s (~%d lines)\n", c.ID, strings.ToUpper(c.PrimaryTheme), c.TotalLines)
		fmt.Fprintf(&sb, "   Suggested Target: %s\n", c.SuggestedFile)
		fmt.Fprintf(&sb, "   Cohesion: %d internal calls | Coupling: %d external calls\n", c.InternalCalls, c.ExternalCalls)
		fmt.Fprintf(&sb, "   Symbols: [%s]\n\n", strings.Join(c.Symbols, ", "))
	}

	fmt.Fprintf(&sb, "🪜 Recommended Refactor Sequence:\n")
	for i, c := range clusters {
		fmt.Fprintf(&sb, "  %d. Extract Cluster %d (%s) -> verify LSP & tests\n", i+1, c.ID, c.PrimaryTheme)
	}

	return CapOutput(sb.String()), nil
}
