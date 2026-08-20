package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumpslabs/bro-code/internal/store"
)

// ContextRecallTool exposes the self-aware notes store to the agent: search
// across every captured action/experience/insight from past sessions (and this
// one) by keyword. This is the agent-facing "retrieve" primitive of the
// retain→recall→reflect discipline — active self-retrieval, not just passive
// warm-start injection.
//
// The store is wired by the UI (nil store = tool reports it is unavailable).
type ContextRecallTool struct {
	Store *store.Store
}

func (t *ContextRecallTool) Name() string { return "context_recall" }

func (t *ContextRecallTool) Description() string {
	return "Recall past agent activity and learned insights across sessions (experiences, hot files, facts, decisions, gotchas) by keyword. Use this BEFORE re-grepping or re-reading: if you touched this file/area before, its history is here. Includes provenance (which tool, what outcome) so you can judge relevance."
}

func (t *ContextRecallTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language query or keywords (file path, function, error message, topic)",
			},
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"", "experience", "hotfile", "fact", "belief", "decision", "gotcha"},
				"description": "Optional filter: which kind of note to recall",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results (default 8)",
			},
		},
		"required": []string{"query"},
	}
}

func (t *ContextRecallTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	if t.Store == nil {
		return "ℹ️ Self-aware context recall is not available in this session.", nil
	}
	var args struct {
		Query string `json:"query"`
		Kind  string `json:"kind"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", fmt.Errorf("context_recall requires a query")
	}
	var kinds []store.NoteKind
	if args.Kind != "" {
		kinds = []store.NoteKind{store.NoteKind(args.Kind)}
	}
	notes, err := t.Store.RecallNotes(args.Query, kinds, args.Limit)
	if err != nil {
		return "", err
	}
	if len(notes) == 0 {
		return "ℹ️ No matching context found in past sessions.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🧠 Recalled %d context note(s):\n\n", len(notes))
	for _, n := range notes {
		fmt.Fprintf(&b, "• [%s] %s\n", n.Kind, n.Subject)
		fmt.Fprintf(&b, "  %s\n", n.Content)
		if n.Provenance != "" {
			fmt.Fprintf(&b, "  provenance: %s\n", n.Provenance)
		}
		if len(n.Tags) > 0 {
			fmt.Fprintf(&b, "  tags: %s\n", strings.Join(n.Tags, ", "))
		}
		fmt.Fprintln(&b)
	}
	return b.String(), nil
}
