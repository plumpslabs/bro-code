package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/store"
)

// AggregateReport summarizes a set of SessionReports — the cross-session
// benchmark view. Like SessionReport it contains only aggregate signals.
type AggregateReport struct {
	SessionCount         int     `json:"session_count"`
	TotalTurns           int     `json:"total_assistant_turns"`
	TotalToolCalls       int     `json:"total_tool_calls"`
	TotalFileChanges     int     `json:"total_file_changes"`
	TotalTokens          int     `json:"total_tokens"`
	TotalCostUSD         float64 `json:"total_estimated_cost_usd"`
	MeanProductivePct    int     `json:"mean_productive_ratio_pct"`
	SessionsWithAnomalies int    `json:"sessions_with_anomalies"`

	Models      []string       `json:"models"`
	AnomalyFreq map[string]int `json:"anomaly_frequency"`
}

// BuildAll reconstructs reports for every session, optionally filtered to
// sessions created at or after `since` (zero time = no filter). Sessions that
// fail to load are skipped so one corrupt row never aborts a bulk export.
func BuildAll(st *store.Store, since time.Time) ([]*SessionReport, error) {
	sessions, err := st.ListSessions()
	if err != nil {
		return nil, err
	}
	var reports []*SessionReport
	for _, s := range sessions {
		if !since.IsZero() && s.CreatedAt.Before(since) {
			continue
		}
		r, err := Build(st, s.ID)
		if err != nil {
			continue
		}
		reports = append(reports, r)
	}
	sort.Slice(reports, func(i, j int) bool {
		return reports[i].CreatedAt.After(reports[j].CreatedAt)
	})
	return reports, nil
}

// Summarize folds a set of reports into a single cross-session benchmark view.
func Summarize(reports []*SessionReport) *AggregateReport {
	a := &AggregateReport{AnomalyFreq: map[string]int{}}
	modelSet := map[string]bool{}
	var pctSum int
	for _, r := range reports {
		a.SessionCount++
		a.TotalTurns += r.AssistantTurns
		a.TotalToolCalls += r.ToolCalls
		a.TotalFileChanges += r.FileChanges
		a.TotalTokens += r.TotalTokens
		a.TotalCostUSD += r.CostUSD
		pctSum += r.ProductivePct
		for _, m := range r.Models {
			modelSet[m] = true
		}
		if len(r.Anomalies) > 0 {
			a.SessionsWithAnomalies++
		}
		for _, an := range r.Anomalies {
			key := normalizeAnomaly(an)
			a.AnomalyFreq[key]++
		}
	}
	if a.SessionCount > 0 {
		a.MeanProductivePct = pctSum / a.SessionCount
	}
	for m := range modelSet {
		a.Models = append(a.Models, m)
	}
	sort.Strings(a.Models)
	return a
}

// normalizeAnomaly collapses "kind (detail)" into "kind" so frequencies group.
func normalizeAnomaly(an string) string {
	if idx := strings.Index(an, " ("); idx >= 0 {
		return an[:idx]
	}
	return an
}

// RenderMarkdown returns a compact cross-session summary.
func (a *AggregateReport) RenderMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# BroCode Usage Benchmark\n\n")
	fmt.Fprintf(&b, "- Sessions: %d\n", a.SessionCount)
	fmt.Fprintf(&b, "- Assistant turns: %d\n", a.TotalTurns)
	fmt.Fprintf(&b, "- Tool calls: %d\n", a.TotalToolCalls)
	fmt.Fprintf(&b, "- File changes: %d\n", a.TotalFileChanges)
	fmt.Fprintf(&b, "- Total tokens: %s\n", fmtTokens(a.TotalTokens))
	fmt.Fprintf(&b, "- Total estimated cost: $%.4f\n", a.TotalCostUSD)
	fmt.Fprintf(&b, "- Mean productive ratio: %d%%\n", a.MeanProductivePct)
	fmt.Fprintf(&b, "- Sessions with anomalies: %d\n", a.SessionsWithAnomalies)
	fmt.Fprintf(&b, "- Models: %s\n", strings.Join(a.Models, ", "))

	if len(a.AnomalyFreq) > 0 {
		fmt.Fprintf(&b, "\n## Anomaly Frequency\n")
		keys := make([]string, 0, len(a.AnomalyFreq))
		for k := range a.AnomalyFreq {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "- %s: %d\n", k, a.AnomalyFreq[k])
		}
	}
	return b.String()
}

// RenderJSON returns the indented JSON form.
func (a *AggregateReport) RenderJSON() (string, error) {
	out, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}
