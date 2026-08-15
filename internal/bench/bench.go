// Package bench provides a Terminal Bench-style harness for measuring how well
// the BroCode agent loop solves real tasks: it runs a task in a throwaway
// sandbox project, then verifies the outcome with a script. It reports pass
// rate, mean task time, and mean context tokens consumed — the same axes the
// big players measure (quality, latency, cost).
//
// A benchmark case is a JSON file (or inline struct) with:
//
//	{
//	  "id": "fix-broken-import",
//	  "prompt": "The project doesn't build. Fix it.",
//	  "setup": "create a go file with a broken import",   // shell script
//	  "verify": "grep -q 'net/http' main.go",            // shell script; exit 0 = pass
//	  "maxIterations": 25
//	}
//
// Usage (headless): build a small binary or test that constructs a Runner with
// a live provider adapter and calls RunCases.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tokens"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// Case is a single benchmark task.
type Case struct {
	ID            string `json:"id"`
	Prompt        string `json:"prompt"`
	Setup         string `json:"setup"`  // shell script run in the sandbox before the agent
	Verify        string `json:"verify"` // shell script; exit 0 = pass
	MaxIterations int    `json:"maxIterations"`
}

// Result is the outcome of running one case.
type Result struct {
	ID         string
	Pass       bool
	Error      string
	Duration   time.Duration
	Iterations int
	Tokens     int // estimated context tokens consumed by the turn
	CostUSD    float64
	Answer     string
}

// Runner executes benchmark cases against a live adapter.
type Runner struct {
	Adapter provider.ProviderAdapter
	Model   string
	// SandboxRoot is where each case gets its own temp subdirectory. Empty
	// uses os.TempDir().
	SandboxRoot string
	// MaxIterations overrides the per-case loop cap (default 25).
	MaxIterations int
	// Timeout caps each case (default 10 minutes).
	Timeout time.Duration
	// Parallel runs cases concurrently (default sequential; keep 1 unless you
	// trust the provider's rate limits).
	Parallel bool
	// Verbose prints per-case progress to stderr.
	Verbose bool
}

// Run executes the cases and returns results in input order.
func (r *Runner) Run(ctx context.Context, cases []Case) []Result {
	if r.Timeout <= 0 {
		r.Timeout = 10 * time.Minute
	}
	if r.MaxIterations <= 0 {
		r.MaxIterations = 25
	}

	results := make([]Result, len(cases))
	if r.Parallel && len(cases) > 1 {
		var wg sync.WaitGroup
		for i := range cases {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = r.runCase(ctx, cases[i])
			}(i)
		}
		wg.Wait()
	} else {
		for i := range cases {
			results[i] = r.runCase(ctx, cases[i])
			if r.Verbose {
				res := results[i]
				status := "PASS"
				if !res.Pass {
					status = "FAIL"
				}
				fmt.Fprintf(os.Stderr, "  [%s] %s (%s, %d iter, ~%d tok, $%.4f)\n", status, res.ID, res.Duration.Round(time.Millisecond), res.Iterations, res.Tokens, res.CostUSD)
			}
		}
	}
	return results
}

// runCase sets up a fresh sandbox and runs one agent turn against it.
func (r *Runner) runCase(ctx context.Context, c Case) Result {
	res := Result{ID: c.ID}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	root := r.SandboxRoot
	if root == "" {
		root = os.TempDir()
	}
	sandbox, err := os.MkdirTemp(root, "bench-"+sanitize(c.ID)+"-")
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer os.RemoveAll(sandbox)

	// Setup script (optional).
	if strings.TrimSpace(c.Setup) != "" {
		if out, err := sh(sandbox, c.Setup); err != nil {
			res.Error = fmt.Sprintf("setup failed: %v\n%s", err, out)
			return res
		}
	}

	maxIter := c.MaxIterations
	if maxIter <= 0 {
		maxIter = r.MaxIterations
	}

	// Fresh context + registry per case; the agent works inside the sandbox.
	ctxMgr := bcontext.NewManager("bench_"+c.ID, nil, 128000)
	tools := tool.NewRegistry()
	tools.SetRepoRoot(sandbox)
	eng := loop.NewEngine(r.Adapter, tools, ctxMgr, r.Model)
	eng.SetMode("BUILDER")
	eng.SetMaxIterations(maxIter)

	// Run the turn inside the sandbox directory so relative tool calls and
	// verification commands operate on the case's own copy.
	prev, _ := os.Getwd()
	if err := os.Chdir(sandbox); err != nil {
		res.Error = err.Error()
		return res
	}

	var iterations int
	answer, err := eng.RunTurn(ctx, c.Prompt, func(state loop.LoopState, info string) {
		if strings.HasPrefix(info, "Turn ") && strings.HasSuffix(info, "reasoning...") {
			iterations++
		}
	})
	os.Chdir(prev)
	if err != nil {
		res.Error = err.Error()
		res.Iterations = iterations
		return res
	}
	res.Answer = answer
	res.Iterations = iterations
	res.Tokens = ctxMgr.TotalTokens()
	res.CostUSD = provider.EstimateCostUSD(r.Model, ctxMgr.TotalTokens(), tokens.CountTokens(answer, r.Model))

	// Verification script (optional; empty verify = pass by completing).
	if strings.TrimSpace(c.Verify) == "" {
		res.Pass = true
		return res
	}
	if out, err := sh(sandbox, c.Verify); err != nil {
		res.Error = fmt.Sprintf("verification failed: %v\n%s", err, out)
		return res
	}
	res.Pass = true
	return res
}

// Report summarizes a set of results.
type Report struct {
	Total        int
	Passed       int
	Failed       int
	PassRate     float64
	MeanDuration time.Duration
	MeanTokens   int
	MeanCostUSD  float64
	PerCase      []Result
}

// Summarize aggregates results and sorts them by ID for stable output.
func Summarize(results []Result) Report {
	rep := Report{Total: len(results), PerCase: results}
	for _, r := range results {
		if r.Pass {
			rep.Passed++
		} else {
			rep.Failed++
		}
		rep.MeanDuration += r.Duration
		rep.MeanTokens += r.Tokens
		rep.MeanCostUSD += r.CostUSD
	}
	if rep.Total > 0 {
		rep.PassRate = float64(rep.Passed) / float64(rep.Total) * 100
		rep.MeanDuration /= time.Duration(rep.Total)
		rep.MeanTokens /= rep.Total
		rep.MeanCostUSD /= float64(rep.Total)
	}
	sort.Slice(rep.PerCase, func(i, j int) bool { return rep.PerCase[i].ID < rep.PerCase[j].ID })
	return rep
}

// RenderReport formats the summary as a compact table.
func RenderReport(rep Report) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Benchmark: %d cases — %d passed / %d failed (%.1f%%)\n", rep.Total, rep.Passed, rep.Failed, rep.PassRate)
	fmt.Fprintf(&sb, "Mean task time: %s | Mean context tokens: ~%d | Mean cost: $%.4f\n", rep.MeanDuration.Round(time.Millisecond), rep.MeanTokens, rep.MeanCostUSD)
	sb.WriteString("\n")
	for _, r := range rep.PerCase {
		status := "PASS"
		if !r.Pass {
			status = "FAIL"
		}
		extra := ""
		if !r.Pass && r.Error != "" {
			extra = " — " + firstLine(r.Error)
		}
		fmt.Fprintf(&sb, "  [%s] %-40s %6s  %3d iter  ~%5d tok  $%7.4f%s\n", status, r.ID, r.Duration.Round(time.Millisecond), r.Iterations, r.Tokens, r.CostUSD, extra)
	}
	return sb.String()
}

// LoadCases reads benchmark cases from a JSON file (either a single case or an
// array).
func LoadCases(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var single Case
	if err := json.Unmarshal(data, &single); err == nil && single.ID != "" {
		return []Case{single}, nil
	}
	var many []Case
	if err := json.Unmarshal(data, &many); err != nil {
		return nil, fmt.Errorf("bench file must be a case or array of cases: %w", err)
	}
	return many, nil
}

// sh runs a shell script in dir and returns combined output.
func sh(dir, script string) (string, error) {
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func sanitize(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	if sb.Len() == 0 {
		return "case"
	}
	return sb.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
