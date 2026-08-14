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
	"github.com/plumpslabs/bro-code/internal/tool"
)

// Runner executes isolated sub-agent turns against the same provider adapter
// the main loop uses.
type Runner struct {
	Adapter provider.ProviderAdapter
	Model   string
	Tools   *tool.Registry // the main registry; sub-agents get a safe subset
}

// SubAgent is a single delegated task.
type SubAgent struct {
	ID   string `json:"id,omitempty"` // optional label for the result
	Task string `json:"task"`         // the isolated task description
}

// runOne executes one sub-agent in its own isolated loop and returns its final
// answer. onUpdate (may be nil) receives progress lines from the sub-loop.
func (r *Runner) runOne(ctx context.Context, id, task string, onUpdate loop.TurnOutputHandler) (string, error) {
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("empty task for sub-agent %q", id)
	}

	// Fresh, isolated context: the sub-agent cannot see the main conversation
	// history (only its own task) and persists nothing to the session store.
	ctxMgr := bcontext.NewManager("sub_"+id, nil, 128000)

	subTools := r.Tools.SubRegistry()

	eng := loop.NewEngine(r.Adapter, subTools, ctxMgr, r.Model)
	eng.SetMode("BUILDER")

	// One turn with a focused directive. The loop guard, tool budget and
	// verification ladder all apply inside the sub-loop as well.
	return eng.RunTurn(ctx, task, onUpdate)
}

// Run executes a single sub-agent and returns its answer.
func (r *Runner) Run(ctx context.Context, task string) (string, error) {
	return r.runOne(ctx, "1", task, nil)
}

// RunMany executes the given tasks — concurrently when parallel is true,
// sequentially otherwise — and returns one report per task.
func (r *Runner) RunMany(ctx context.Context, agents []SubAgent, parallel bool, onUpdate loop.TurnOutputHandler) ([]string, error) {
	if len(agents) == 0 {
		return nil, fmt.Errorf("no sub-agent tasks provided")
	}

	results := make([]string, len(agents))
	errs := make([]error, len(agents))

	run := func(i int) {
		id := agents[i].ID
		if id == "" {
			id = fmt.Sprintf("%d", i+1)
		}
		results[i], errs[i] = r.runOne(ctx, id, agents[i].Task, onUpdate)
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
	go func() {
		defer cancel()
		sm.sem <- struct{}{}
		defer func() { <-sm.sem }()
		result, err := sm.Runner.runOne(tctx, id, task, nil)
		sm.mu.Lock()
		job.Done = true
		job.Result = result
		job.Err = err
		sm.mu.Unlock()
	}()
	return id, nil
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
						"id":   map[string]any{"type": "string", "description": "Optional short label (e.g. 'frontend')"},
						"task": map[string]any{"type": "string", "description": "The isolated task description"},
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
		return strings.Join(reports, "\n\n"), nil
	case strings.TrimSpace(args.Task) != "":
		return t.Runner.Run(tctx, args.Task)
	default:
		return "", fmt.Errorf("subagent requires either 'task' or 'tasks'")
	}
}
