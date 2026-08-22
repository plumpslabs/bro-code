package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumpslabs/bro-code/internal/search"
)

// BlastRadiusTool performs impact analysis on symbols or files across the codebase.
type BlastRadiusTool struct {
	Index *search.GlobalIndex
}

func (t *BlastRadiusTool) Name() string { return "blast_radius" }
func (t *BlastRadiusTool) Description() string {
	return "Analyze the blast radius (ripple impact) of modifying or refactoring a symbol or file: returns definitions, referencing callers across files, direct module importers, and risk level (LOW/MEDIUM/HIGH/CRITICAL). Call BEFORE modifying exported symbols or core files to know all affected call-sites in advance."
}
func (t *BlastRadiusTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{"type": "string", "description": "Symbol name (function, class, struct, method) or file path to analyze impact for"},
		},
		"required": []string{"target"},
	}
}
func (t *BlastRadiusTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	target := strings.TrimSpace(args.Target)
	if target == "" {
		return "", fmt.Errorf("blast_radius requires a 'target' symbol or file path")
	}
	if t.Index == nil {
		return "", fmt.Errorf("global codebase index is unavailable")
	}
	report := t.Index.BlastRadius(target)
	return CapOutput(report.Format()), nil
}
