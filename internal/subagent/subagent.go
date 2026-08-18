// Package subagent implements sub-agents: isolated agent loops that run with
// their own fresh context while sharing the main provider adapter and tool
// set. The model can delegate a focused task (or several tasks in parallel)
// and receive each sub-agent's final answer — the building block for
// task decomposition and parallel research, mirroring opencode's sub-agents.
//
// Safety: a sub-agent runs with a sub-registry that (1) drops the interactive
// tools (ask_user, review_changes) and the subagent tool itself (no recursion),
// and (2) denies every gated command — destructive shell operations always
// require the main agent's approval modal, never a silent background run.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/loop"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// Runner executes isolated sub-agent turns against the same provider adapter
// the main loop uses.
type Runner struct {
	Adapter provider.ProviderAdapter
	Model   string
	Tools   *tool.Registry // the main registry; sub-agents get a safe subset
	// BudgetUSD, when > 0, caps each sub-agent turn's estimated provider spend
	// (hard stop → graceful synthesis). Runaway sub-agents can no longer burn
	// tokens to the 10-minute wall-clock cap with no cost limit.
	BudgetUSD float64
	// Store, when non-nil, persists each sub-agent's isolated conversation to
	// SQLite so delegated work is auditable after the fact. Nil keeps the
	// previous behavior (fresh context, nothing persisted).
	Store *store.Store
	// Ask, when set, is the confirmation gate for MUTATING parallel tasks. A
	// sub-agent flagged Mutates (write/delete/exec) must be explicitly approved
	// by the user before it runs — this is the "controlled" half of BroCode's
	// parallel orchestration: fan-out is free and fast, but any side-effecting
	// parallel agent cannot act without a confirm. Nil = deny mutating tasks.
	Ask func(question string, options []string) (string, error)
	// ContextWindow is the token limit for sub-agent contexts. 0 defaults to 128k.
	ContextWindow int
}

// SubAgent is a single delegated task.
type SubAgent struct {
	ID        string `json:"id,omitempty"`         // optional label for the result
	Task      string `json:"task"`                 // the isolated task description
	Mode      string `json:"mode,omitempty"`       // "BUILDER" (default) or "PLANNER" (read-only)
	TargetDir string `json:"target_dir,omitempty"` // optional target sub-repository or working directory (e.g. 'services/payment' or 'auth-service')
	// Mutates marks a task that may write/delete/execute. Mutating parallel
	// agents are never run without explicit user confirmation (see Runner.Ask);
	// this keeps fan-out fast and read-only by default but still lets the model
	// request a supervised parallel mutation when it is actually useful.
	Mutates bool `json:"mutates,omitempty"`
}

// runOne executes one sub-agent in its own isolated loop and returns its final
// answer. onUpdate (may be nil) receives progress lines from the sub-loop.
func (r *Runner) runOne(ctx context.Context, id, task, mode, targetDir string, onUpdate loop.TurnOutputHandler) (string, error) {
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("empty task for sub-agent %q", id)
	}

	if targetDir != "" {
		task = fmt.Sprintf("[Target Working Directory / Repository: %s]\n%s", targetDir, task)
	}

	// Fresh, isolated context: the sub-agent cannot see the main conversation
	// history (only its own task). Persists to the shared store only when one
	// is wired (production audit trail); nil keeps the sub-conversation
	// ephemeral (default, and always the case in tests).
	win := r.ContextWindow
	if win <= 0 {
		win = 128000
	}
	ctxMgr := bcontext.NewManager("sub_"+id, r.Store, win)

	subTools := r.Tools.SubRegistry()

	eng := loop.NewEngine(r.Adapter, subTools, ctxMgr, r.Model)
	if mode == "" {
		mode = "BUILDER"
	}
	eng.SetMode(mode)
	if r.BudgetUSD > 0 {
		eng.SetBudgetUSD(r.BudgetUSD)
	}

	// One turn with a focused directive. The loop guard, tool budget and
	// verification ladder all apply inside the sub-loop as well.
	return eng.RunTurn(ctx, task, onUpdate)
}

// Run executes a single sub-agent and returns its answer.
func (r *Runner) Run(ctx context.Context, task string) (string, error) {
	return r.runOne(ctx, "1", task, "BUILDER", "", nil)
}

// RunMany executes the given tasks — concurrently when parallel is true,
// sequentially otherwise — and returns one report per task. Completed agents
// stream a one-line progress update through onUpdate (may be nil) so the caller
// sees results arrive incrementally instead of waiting for the whole batch.
func (r *Runner) RunMany(ctx context.Context, agents []SubAgent, parallel bool, onUpdate loop.TurnOutputHandler) ([]string, error) {
	if len(agents) == 0 {
		return nil, fmt.Errorf("no sub-agent tasks provided")
	}

	// Confirm-gate: fan-out is read-only by default. Any task flagged Mutates
	// (write/delete/exec) must be explicitly approved by the user via Runner.Ask
	// before it runs — parallel side effects are never silent. Without an Ask
	// handler, mutating tasks are denied outright (fail-closed).
	mutating := false
	for _, a := range agents {
		if a.Mutates {
			mutating = true
			break
		}
	}
	allowed := !mutating
	if mutating && r.Ask != nil {
		ans, err := r.Ask("Parallel sub-agents include MUTATING tasks (write/delete/exec). Approve them?", []string{"yes", "no"})
		if err == nil && strings.EqualFold(strings.TrimSpace(ans), "yes") {
			allowed = true
		}
	}

	results := make([]string, len(agents))
	errs := make([]error, len(agents))

	run := func(i int) {
		id := agents[i].ID
		if id == "" {
			id = fmt.Sprintf("%d", i+1)
		}
		if agents[i].Mutates && !allowed {
			errs[i] = fmt.Errorf("sub-agent %q is mutating and was not approved by the user", id)
			if onUpdate != nil {
				onUpdate(loop.StateBlocked, fmt.Sprintf("🤖 Sub-agent %s DENIED (mutating, not confirmed)", id))
			}
			return
		}
		results[i], errs[i] = r.runOne(ctx, id, agents[i].Task, agents[i].Mode, agents[i].TargetDir, onUpdate)
		if onUpdate != nil {
			status := "DONE"
			if errs[i] != nil {
				status = "FAILED"
			}
			onUpdate(loop.StateObserving, fmt.Sprintf("🤖 Sub-agent %s %s", id, status))
		}
	}

	if parallel && len(agents) > 1 {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 3) // cap concurrent sub-agents to protect the provider
		for i := range agents {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				run(i)
			}(i)
		}
		wg.Wait()
	} else {
		for i := range agents {
			run(i)
		}
	}

	reports := make([]string, 0, len(agents))
	for i := range agents {
		id := agents[i].ID
		if id == "" {
			id = fmt.Sprintf("%d", i+1)
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("### Sub-agent %s — %s\n", id, truncate(agents[i].Task, 200)))
		if errs[i] != nil {
			sb.WriteString("**Status:** FAILED\n" + errs[i].Error())
		} else {
			sb.WriteString("**Status:** DONE\n" + results[i])
		}
		reports = append(reports, sb.String())
	}
	return reports, nil
}

// Merge combines per-sub-agent reports into one concise, de-duplicated summary.
// It keeps the per-agent structure (### headers and **Status:** lines are always
// preserved) but collapses identical content lines that several agents repeat
// (the same error message, the same file path) so the merged view is small enough
// to drop straight back into the main context without re-bloating it.
func Merge(reports []string) string {
	if len(reports) == 0 {
		return ""
	}
	const maxLines = 240
	seen := map[string]bool{}
	var out []string
	total := 0
	for _, rep := range reports {
		for _, line := range strings.Split(rep, "\n") {
			t := strings.TrimSpace(line)
			if t == "" {
				out = append(out, line)
				continue
			}
			// Always keep structural lines; dedupe only repeated content.
			if strings.HasPrefix(t, "### ") || strings.HasPrefix(t, "**Status:**") {
				out = append(out, line)
				total++
				continue
			}
			if seen[t] {
				continue
			}
			seen[t] = true
			out = append(out, line)
			total++
			if total >= maxLines {
				out = append(out, "… (merged output truncated)")
				return fmt.Sprintf("Merged %d sub-agent reports (%d lines, de-duplicated):\n\n%s", len(reports), total, strings.Join(out, "\n"))
			}
		}
	}
	return fmt.Sprintf("Merged %d sub-agent reports (%d lines, de-duplicated):\n\n%s", len(reports), total, strings.Join(out, "\n"))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ── Scout: background research ─────────────────────────────────────────────
// ScoutManager runs research tasks in the background WHILE the main turn keeps
// executing (mirroring opencode's Scout). The model calls the scout tool, gets
// back a "started" receipt immediately, and keeps working; the engine drains
// completed scouts at each loop iteration and delivers their results into the
// context so the model can use them. Results that finish after the turn ends
// are parked and delivered at the start of the next turn.

// ScoutJob is one background research task.
type ScoutJob struct {
	ID     string
	Task   string
	Done   bool
	Result string
	Err    error
	cancel context.CancelFunc // cancels this scout's goroutine (abort)
}

// ScoutManager owns the background scout jobs for a session.
type ScoutManager struct {
	Runner *Runner
	mu     sync.Mutex
	jobs   map[string]*ScoutJob
	// sem caps how many scouts run CONCURRENTLY: each scout is a full agent
	// loop with its own provider calls and tool executions, so an unbounded
	// batch (a model spawning a dozen scouts at once) would hammer the
	// provider and rate-limit everything. Same cap as parallel sub-agents.
	sem chan struct{}
}

// NewScoutManager creates a scout manager backed by the given runner.
func NewScoutManager(r *Runner) *ScoutManager {
	return &ScoutManager{Runner: r, jobs: make(map[string]*ScoutJob), sem: make(chan struct{}, 3)}
}

// Start launches a background scout. Returns the job id immediately; the job
// runs in its own goroutine and its result is picked up by Drain.
func (sm *ScoutManager) Start(ctx context.Context, task string) (string, error) {
	return sm.StartWithProgress(ctx, task, nil)
}

// StartWithProgress launches a background scout, forwarding its progress lines
// to onProgress (may be nil). Returns the job id immediately.
func (sm *ScoutManager) StartWithProgress(ctx context.Context, task string, onProgress func(string)) (string, error) {
	if sm == nil || sm.Runner == nil || sm.Runner.Adapter == nil {
		return "", fmt.Errorf("scout runner is not configured")
	}
	id := fmt.Sprintf("scout%d", time.Now().UnixNano())
	job := &ScoutJob{ID: id, Task: task}
	sm.mu.Lock()
	sm.jobs[id] = job
	sm.mu.Unlock()

	// 10-minute hard cap so a hung scout cannot leak a goroutine forever, and
	// a concurrency semaphore so a scout batch never hammers the provider.
	tctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	sm.mu.Lock()
	job.cancel = cancel
	sm.mu.Unlock()
	go func() {
		defer cancel()
		sm.sem <- struct{}{}
		defer func() { <-sm.sem }()
		var upd loop.TurnOutputHandler
		if onProgress != nil {
			upd = func(st loop.LoopState, info string) { onProgress(info) }
		}
		result, err := sm.Runner.runOne(tctx, id, task, "PLANNER", "", upd)
		sm.mu.Lock()
		job.Done = true
		job.Result = result
		job.Err = err
		sm.mu.Unlock()
	}()
	return id, nil
}

// Cancel aborts a single running scout by its job id. Already-completed jobs
// are untouched.
func (sm *ScoutManager) Cancel(id string) {
	if sm == nil {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if j, ok := sm.jobs[id]; ok && j.cancel != nil {
		j.cancel()
	}
}

// CancelAll aborts every running scout. Used when the session is interrupted
// or the program exits so background goroutines are not left dangling.
func (sm *ScoutManager) CancelAll() {
	if sm == nil {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, j := range sm.jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
}

// Pending returns the number of scouts still running.
func (sm *ScoutManager) Pending() int {
	if sm == nil {
		return 0
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	n := 0
	for _, j := range sm.jobs {
		if !j.Done {
			n++
		}
	}
	return n
}

// Drain collects all completed-but-undelivered scout results and removes them
// from the manager. Returns one formatted report per finished job. Running
// jobs are left in place.
func (sm *ScoutManager) Drain() []string {
	if sm == nil {
		return nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	var reports []string
	for id, j := range sm.jobs {
		if !j.Done {
			continue
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("### Scout %s — %s\n", id, truncate(j.Task, 150)))
		if j.Err != nil {
			sb.WriteString("**Status:** FAILED\n" + j.Err.Error())
		} else {
			sb.WriteString("**Status:** DONE\n" + j.Result)
		}
		reports = append(reports, sb.String())
		delete(sm.jobs, id)
	}
	return reports
}

// ScoutTool is the native `scout` tool: it starts background research and
// returns immediately. Results are delivered to the model by the engine loop.
type ScoutTool struct {
	Manager *ScoutManager
}

func (t *ScoutTool) Name() string { return "scout" }
func (t *ScoutTool) Description() string {
	return "Start a background research task and CONTINUE your current work immediately. The scout runs its own isolated agent loop (with tools, context and verification) and its findings are delivered to you when ready — you do NOT wait. Use for tasks you want to parallelize with what you are doing now (research a separate subsystem, investigate an error in another directory, draft a design doc). Provide one clear task."
}
func (t *ScoutTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "The isolated background research task.",
			},
		},
		"required": []string{"task"},
	}
}

func (t *ScoutTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid scout arguments: %w", err)
	}
	if strings.TrimSpace(args.Task) == "" {
		return "", fmt.Errorf("scout requires a 'task'")
	}
	id, err := t.Manager.Start(ctx, args.Task)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("⏳ Scout %s started in the background: %s\nContinue your current work — I will deliver the findings when the scout finishes.", id, truncate(args.Task, 200)), nil
}

// Tool is the native `subagent` tool registered in the main registry. The
// model calls it to delegate focused work to isolated sub-agents.
type Tool struct {
	Runner *Runner
}

func (t *Tool) Name() string { return "subagent" }
func (t *Tool) Description() string {
	return "Delegate focused tasks to isolated sub-agents. Each sub-agent runs its own agent loop (with its own context, tools, and verification) and returns its final answer. Use for parallelizable work: research separate files, implement independent features, or investigate different questions simultaneously. Provide 1-6 tasks. With parallel=true they run concurrently (max 3 at once). Sub-agents cannot run commands that require your approval, ask the user, or spawn further sub-agents — do the dangerous/interactive steps yourself after gathering their results."
}
func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "A single task to delegate (alternative to tasks).",
			},
			"tasks": map[string]any{
				"type":        "array",
				"description": "Multiple tasks to delegate. Each has an optional id and a task description.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":         map[string]any{"type": "string", "description": "Optional short label (e.g. 'frontend' or 'auth-service')"},
						"task":       map[string]any{"type": "string", "description": "The isolated task description"},
						"target_dir": map[string]any{"type": "string", "description": "Optional target sub-repository or working directory (e.g. 'auth-service' or 'services/payment')"},
						"mutates":    map[string]any{"type": "boolean", "description": "Set true ONLY if the task writes, deletes, or executes commands. Mutating parallel tasks require explicit user confirmation before running — read-only research should leave this false."},
					},
					"required": []string{"task"},
				},
			},
			"parallel": map[string]any{
				"type":        "boolean",
				"description": "true runs multiple tasks concurrently (default false = sequential).",
			},
		},
	}
}

func (t *Tool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Task     string     `json:"task"`
		Tasks    []SubAgent `json:"tasks"`
		Parallel bool       `json:"parallel"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid subagent arguments: %w", err)
	}
	if t.Runner == nil || t.Runner.Adapter == nil {
		return "", fmt.Errorf("subagent runner is not configured")
	}

	// 10-minute hard cap so a hung sub-agent cannot stall the main turn.
	tctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	switch {
	case len(args.Tasks) > 0:
		reports, err := t.Runner.RunMany(tctx, args.Tasks, args.Parallel, nil)
		if err != nil {
			return "", err
		}
		return Merge(reports), nil
	case strings.TrimSpace(args.Task) != "":
		return t.Runner.Run(tctx, args.Task)
	default:
		return "", fmt.Errorf("subagent requires either 'task' or 'tasks'")
	}
}

// RunScoutSwarm executes parallel speculative research subagents across different
// subdirectories, returning aggregated findings without polluting the main conversation (Inovasi 1).
func (r *Runner) RunScoutSwarm(ctx context.Context, subpaths []string, goal string) string {
	if r == nil || len(subpaths) == 0 {
		return "No subpaths provided for scout swarm."
	}

	var agents []SubAgent
	for i, p := range subpaths {
		agents = append(agents, SubAgent{
			ID:   fmt.Sprintf("Scout_%d", i+1),
			Task: fmt.Sprintf("Scout directory %s to accomplish goal: %s", p, goal),
		})
	}

	reports, err := r.RunMany(ctx, agents, true, nil)
	if err != nil {
		return "Scout swarm error: " + err.Error()
	}
	return Merge(reports)
}
