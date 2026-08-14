package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumpslabs/bro-code/internal/memory"
)

// MemoryTool exposes the cross-session project memory to the agent:
//   - recall <query> — BM25 search over stored facts (past sessions' learnings)
//   - retain <fact> — store a durable fact (optionally in a section)
//   - list — show everything stored
//
// The store is wired by the UI (nil store = tool reports it is unavailable).
type MemoryTool struct {
	Store *memory.Store
}

func (t *MemoryTool) Name() string { return "memory" }

func (t *MemoryTool) Description() string {
	return "Cross-session project memory: recall facts learned in past sessions (BM25 relevance), retain new durable facts (architecture, build commands, decisions, gotchas), or list everything. Use recall when you suspect an earlier session already learned something about this codebase — it can save you from re-grepping."
}

func (t *MemoryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"recall", "retain", "list"},
				"description": "recall: search memory for facts matching a query; retain: store a durable fact; list: show all stored facts",
			},
			"query":   map[string]any{"type": "string", "description": "For recall: natural-language query or keywords to search for"},
			"fact":    map[string]any{"type": "string", "description": "For retain: the durable fact to store (e.g. 'Payment flow: service -> repository -> Prisma')"},
			"section": map[string]any{"type": "string", "description": "For retain (optional): section to file the fact under (Architecture, Build & Test, Decisions, Gotchas, Notes)"},
		},
		"required": []string{"action"},
	}
}

func (t *MemoryTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	if t.Store == nil {
		return "ℹ️ Project memory is not available in this session.", nil
	}
	var args struct {
		Action  string `json:"action"`
		Query   string `json:"query"`
		Fact    string `json:"fact"`
		Section string `json:"section"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	switch strings.ToLower(args.Action) {
	case "recall":
		if strings.TrimSpace(args.Query) == "" {
			return "", fmt.Errorf("memory recall requires a query")
		}
		return t.Store.Recall(args.Query, 5), nil
	case "retain":
		added, err := t.Store.Retain(args.Section, args.Fact)
		if err != nil {
			return "", err
		}
		if added {
			return "✅ Stored in project memory.", nil
		}
		return "ℹ️ Already in memory (duplicate).", nil
	case "list":
		return t.Store.List(), nil
	default:
		return "", fmt.Errorf("memory action must be recall, retain, or list")
	}
}
