package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/tokens"
)

// MutatingTools are tool names that change the filesystem. A turn that issues
// one of these (or a final answer that ships no tool calls) is counted as
// "productive" under BroCode's north-star efficiency metric (see
// internal/tokens/ratio.go and docs/PHILOSOPHY.md).
var MutatingTools = map[string]bool{
	"edit_file":   true,
	"edit_symbol": true,
	"write_file":  true,
	"create_file": true,
	"delete_file": true,
}

// errorMarkers flag a tool_result payload as a likely failure. This is a
// heuristic only — it never inspects payload contents beyond a cheap substring
// scan, so it stays local and privacy-safe.
var errorMarkers = []string{"\"error\"", "error:", "failed", "exception", "panic", "not found", "denied"}

// PhaseStats aggregates metrics for a single engineering process phase.
type PhaseStats struct {
	Calls    int      `json:"calls"`
	Failures int      `json:"failures"`
	Tools    []string `json:"tools,omitempty"`
}

// PhaseBreakdown groups agent activity into canonical software engineering stages.
type PhaseBreakdown struct {
	Discovery    PhaseStats `json:"discovery"`    // search, read, list, locate
	Execution    PhaseStats `json:"execution"`    // edit, write, delete, bash, git
	Verification PhaseStats `json:"verification"` // test, diagnose, lsp, review
	Context      PhaseStats `json:"context"`      // memory, ask_user
}

// SessionReport is a fully aggregated, privacy-safe summary of one agent
// session. It intentionally contains NO message text, file contents, or secrets
// — only counts, token metrics, and heuristic anomaly flags. That makes it safe
// to export (md/json) as a real-world usage/benchmark dataset.
type SessionReport struct {
	SessionID   string    `json:"session_id"`
	ProjectPath string    `json:"project_path"`
	Mode        string    `json:"mode"`
	Models      []string  `json:"models"`
	CreatedAt   time.Time `json:"created_at"`
	DurationSec float64   `json:"duration_sec"`

	UserMsgs        int `json:"user_msgs"`
	AssistantTurns  int `json:"assistant_turns"`
	ToolCalls       int `json:"tool_calls"`
	ToolResults     int `json:"tool_results"`
	ToolFailures    int `json:"tool_failures"`
	Compactions     int `json:"compactions"`
	FileChanges     int `json:"file_changes"`
	DistinctTools   int `json:"distinct_tools"`

	TotalTokens  int     `json:"total_tokens"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"estimated_cost_usd"`

	ProductivePct int `json:"productive_ratio_pct"`

	Phases PhaseBreakdown `json:"phases,omitempty"`

	Anomalies []string `json:"anomalies,omitempty"`
}

// Build reconstructs a SessionReport from the persisted session + event log.
// It never reads message contents into the report — only numeric/structural
// signals — so the output is safe to share as a benchmark dataset.
func Build(st *store.Store, sessionID string) (*SessionReport, error) {
	sess, err := st.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	events, err := st.GetSessionEvents(sessionID)
	if err != nil {
		return nil, err
	}

	r := &SessionReport{
		SessionID:   sess.ID,
		ProjectPath: sess.ProjectPath,
		Mode:        sess.Mode,
		CreatedAt:   sess.CreatedAt,
		Models:      []string{},
	}

	modelSet := map[string]bool{}
	toolSet := map[string]bool{}
	// Per-model token split so cost attribution follows the active model.
	modelInput := map[string]int{}
	modelOutput := map[string]int{}
	lastModel := ""

	var totalProd, totalTurnTokens int
	var firstTime, lastTime time.Time
	var pendingTools []string

	for _, ev := range events {
		if firstTime.IsZero() || ev.CreatedAt.Before(firstTime) {
			firstTime = ev.CreatedAt
		}
		if ev.CreatedAt.After(lastTime) {
			lastTime = ev.CreatedAt
		}

		switch ev.Type {
		case "user_msg", "tool_result", "system_msg", "file_changes":
			r.InputTokens += ev.Tokens
			if lastModel == "" {
				lastModel = "unknown"
			}
			modelInput[lastModel] += ev.Tokens
			if ev.Type == "tool_result" {
				r.ToolResults++
				isErr := looksLikeError(ev.PayloadJSON)
				if isErr {
					r.ToolFailures++
				}
				toolName := ""
				if len(pendingTools) > 0 {
					toolName = pendingTools[0]
					pendingTools = pendingTools[1:]
				}
				if isErr && toolName != "" {
					phase := classifyToolPhase(toolName)
					switch phase {
					case "discovery":
						r.Phases.Discovery.Failures++
					case "execution":
						r.Phases.Execution.Failures++
					case "verification":
						r.Phases.Verification.Failures++
					case "context":
						r.Phases.Context.Failures++
					}
				}
			}
			if ev.Type == "file_changes" {
				r.FileChanges++
			}
			if ev.Type == "user_msg" {
				r.UserMsgs++
			}
		case "assistant_msg":
			r.AssistantTurns++
			r.OutputTokens += ev.Tokens
			var msg provider.Message
			if json.Unmarshal([]byte(ev.PayloadJSON), &msg) == nil {
				if msg.Model != "" {
					modelSet[msg.Model] = true
					lastModel = msg.Model
				}
				productive := false
				for _, tc := range msg.ToolCalls {
					r.ToolCalls++
					toolSet[tc.Name] = true
					phase := classifyToolPhase(tc.Name)
					switch phase {
					case "discovery":
						r.Phases.Discovery.Calls++
						r.Phases.Discovery.Tools = addUnique(r.Phases.Discovery.Tools, tc.Name)
					case "execution":
						r.Phases.Execution.Calls++
						r.Phases.Execution.Tools = addUnique(r.Phases.Execution.Tools, tc.Name)
					case "verification":
						r.Phases.Verification.Calls++
						r.Phases.Verification.Tools = addUnique(r.Phases.Verification.Tools, tc.Name)
					case "context":
						r.Phases.Context.Calls++
						r.Phases.Context.Tools = addUnique(r.Phases.Context.Tools, tc.Name)
					}
					pendingTools = append(pendingTools, tc.Name)
					if MutatingTools[tc.Name] {
						productive = true
					}
				}
				// A final answer (no tool calls, but content) is a deliverable.
				if len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.Content) != "" {
					productive = true
				}
				if productive {
					totalProd += ev.Tokens
				}
			}
			totalTurnTokens += ev.Tokens
			modelOutput[lastModel] += ev.Tokens
		case "compaction_summary":
			r.Compactions++
		}
	}

	r.TotalTokens = r.InputTokens + r.OutputTokens
	for m := range modelSet {
		r.Models = append(r.Models, m)
	}
	sort.Strings(r.Models)
	r.DistinctTools = len(toolSet)

	for m, in := range modelInput {
		out := modelOutput[m]
		r.CostUSD += provider.EstimateCostUSD(m, in, out)
	}

	stats := tokens.NewTurnTokenStats(totalTurnTokens, totalProd)
	r.ProductivePct = stats.Percent()

	if !lastTime.IsZero() && !firstTime.IsZero() {
		r.DurationSec = lastTime.Sub(firstTime).Seconds()
	}

	r.detectAnomalies()
	return r, nil
}

// classifyToolPhase assigns a tool to its canonical process phase.
func classifyToolPhase(name string) string {
	switch name {
	case "read_file", "list_dir", "grep", "glob", "search_code", "code_locate", "fetch_url", "web_search":
		return "discovery"
	case "edit_file", "edit_symbol", "write_file", "create_file", "delete_file", "bash", "git", "undo":
		return "execution"
	case "run_tests", "diagnose", "lsp", "lsp_diagnostics", "lsp_symbols", "lsp_hover", "lsp_references", "lsp_definition", "review_changes", "lsp_auto_fix":
		return "verification"
	case "memory", "context_recall", "ask_user":
		return "context"
	default:
		return "discovery"
	}
}

func addUnique(list []string, item string) []string {
	for _, s := range list {
		if s == item {
			return list
		}
	}
	return append(list, item)
}

// looksLikeError applies the cheap heuristic error scan to a tool_result
// payload. It does not parse or retain any content.
func looksLikeError(payload string) bool {
	low := strings.ToLower(payload)
	for _, m := range errorMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// detectAnomalies fills r.Anomalies with human-readable, privacy-safe flags.
func (r *SessionReport) detectAnomalies() {
	if r.ProductivePct < 30 && r.AssistantTurns > 0 {
		r.Anomalies = append(r.Anomalies,
			fmt.Sprintf("low productive token ratio (%d%%) — agent may be exploring without delivering", r.ProductivePct))
	}
	if r.Compactions >= 3 {
		r.Anomalies = append(r.Anomalies,
			fmt.Sprintf("excessive compaction (%d) — possible context thrash", r.Compactions))
	}
	if r.ToolResults > 0 && float64(r.ToolFailures)/float64(r.ToolResults) > 0.3 {
		r.Anomalies = append(r.Anomalies,
			fmt.Sprintf("high tool failure rate (%d/%d results look like errors)", r.ToolFailures, r.ToolResults))
	}
	if r.AssistantTurns >= 15 {
		r.Anomalies = append(r.Anomalies,
			fmt.Sprintf("long session (%d assistant turns) — consider a narrower scope", r.AssistantTurns))
	}
	if len(r.Models) > 1 {
		r.Anomalies = append(r.Anomalies,
			fmt.Sprintf("multi-model/fallback usage: %s", strings.Join(r.Models, ", ")))
	}
}

// RenderMarkdown returns a human-readable report. It contains only aggregate
// metrics — never message text or file contents.
func (r *SessionReport) RenderMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# BroCode Session Report\n\n")
	fmt.Fprintf(&b, "- **Session:** `%s`\n", r.SessionID)
	fmt.Fprintf(&b, "- **Project:** `%s`\n", r.ProjectPath)
	fmt.Fprintf(&b, "- **Mode:** %s\n", r.Mode)
	fmt.Fprintf(&b, "- **Models:** %s\n", strings.Join(r.Models, ", "))
	fmt.Fprintf(&b, "- **Created:** %s\n", r.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Duration:** %.0fs\n", r.DurationSec)

	fmt.Fprintf(&b, "\n## Activity\n")
	fmt.Fprintf(&b, "- User messages: %d\n", r.UserMsgs)
	fmt.Fprintf(&b, "- Assistant turns: %d\n", r.AssistantTurns)
	fmt.Fprintf(&b, "- Tool calls: %d (%d distinct tools)\n", r.ToolCalls, r.DistinctTools)
	fmt.Fprintf(&b, "- Tool results: %d (%d possible failures)\n", r.ToolResults, r.ToolFailures)
	fmt.Fprintf(&b, "- Compactions: %d\n", r.Compactions)
	fmt.Fprintf(&b, "- File changes: %d\n", r.FileChanges)

	if r.ToolCalls > 0 {
		fmt.Fprintf(&b, "\n## Process Breakdown\n")
		if r.Phases.Discovery.Calls > 0 {
			fmt.Fprintf(&b, "- Discovery (Search & Read): %d calls (%d failures) — %s\n",
				r.Phases.Discovery.Calls, r.Phases.Discovery.Failures, strings.Join(r.Phases.Discovery.Tools, ", "))
		}
		if r.Phases.Execution.Calls > 0 {
			fmt.Fprintf(&b, "- Execution (Edits & Shell): %d calls (%d failures) — %s\n",
				r.Phases.Execution.Calls, r.Phases.Execution.Failures, strings.Join(r.Phases.Execution.Tools, ", "))
		}
		if r.Phases.Verification.Calls > 0 {
			fmt.Fprintf(&b, "- Verification (Tests & LSP): %d calls (%d failures) — %s\n",
				r.Phases.Verification.Calls, r.Phases.Verification.Failures, strings.Join(r.Phases.Verification.Tools, ", "))
		}
		if r.Phases.Context.Calls > 0 {
			fmt.Fprintf(&b, "- Context (Memory & Prompts): %d calls (%d failures) — %s\n",
				r.Phases.Context.Calls, r.Phases.Context.Failures, strings.Join(r.Phases.Context.Tools, ", "))
		}
	}

	fmt.Fprintf(&b, "\n## Token Economy\n")
	fmt.Fprintf(&b, "- Total tokens: %s\n", fmtTokens(r.TotalTokens))
	fmt.Fprintf(&b, "- Input / Output: %s / %s\n", fmtTokens(r.InputTokens), fmtTokens(r.OutputTokens))
	fmt.Fprintf(&b, "- Estimated cost: $%.4f\n", r.CostUSD)
	fmt.Fprintf(&b, "- Productive ratio: %d%%\n", r.ProductivePct)

	if len(r.Anomalies) > 0 {
		fmt.Fprintf(&b, "\n## Anomalies\n")
		for _, a := range r.Anomalies {
			fmt.Fprintf(&b, "- ⚠️ %s\n", a)
		}
	} else {
		fmt.Fprintf(&b, "\n## Anomalies\n- ✅ none detected\n")
	}
	return b.String()
}

// RenderJSON returns the indented JSON form of the report.
func (r *SessionReport) RenderJSON() (string, error) {
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// fmtTokens mirrors loop.usage.go's compact token formatter.
func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
