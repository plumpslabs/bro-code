package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/loop"
)

// Role defines the specialist role of a swarm participant.
type Role string

const (
	RoleArchitect Role = "ARCHITECT" // Researches, plans, and outputs concrete blueprints (Read-only)
	RoleBuilder   Role = "BUILDER"   // Implements the blueprint with surgical code edits
	RoleAuditor   Role = "AUDITOR"   // Audits changes for correctness, regressions, and tests
)

// SwarmTask represents a coordinated multi-stage task.
type SwarmTask struct {
	Goal       string        `json:"goal"`
	Context    string        `json:"context,omitempty"`
	AutoVerify bool          `json:"auto_verify,omitempty"`
	Timeout    time.Duration `json:"timeout,omitempty"`
}

// SwarmResult contains the combined synthesis from all swarm stages.
type SwarmResult struct {
	Goal           string   `json:"goal"`
	ArchitectSpec  string   `json:"architect_spec"`
	BuilderOutput  string   `json:"builder_output"`
	AuditorVerdict string   `json:"auditor_verdict"`
	Success        bool     `json:"success"`
	TouchedFiles   []string `json:"touched_files,omitempty"`
	Duration       string   `json:"duration"`
}

// ExecuteSwarm coordinates a 3-tier specialist swarm (Architect -> Builder -> Auditor).
// It runs each specialist with dedicated mode isolation, budget bounds, and progress streaming.
func (r *Runner) ExecuteSwarm(ctx context.Context, task SwarmTask, onUpdate loop.TurnOutputHandler) (*SwarmResult, error) {
	if strings.TrimSpace(task.Goal) == "" {
		return nil, fmt.Errorf("empty swarm goal")
	}

	start := time.Now()
	timeout := task.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := &SwarmResult{Goal: task.Goal}

	// ── Phase 1: ARCHITECT (Discovery & Blueprinting) ───────────────────────
	if onUpdate != nil {
		onUpdate(loop.StateActing, "🏛️ [Swarm] ARCHITECT: Inspecting architecture & drafting blueprint...")
	}

	archPrompt := fmt.Sprintf(`You are the ARCHITECT specialist in a collaborative swarm.
GOAL: %s
CONTEXT: %s

TASK:
1. Explore relevant files, call graphs, and definitions.
2. Formulate a concise, bulleted implementation BLUEPRINT.
3. List the exact files, functions, and interfaces that need modification.
4. Highlight any edge cases or risks.
DO NOT write full replacement code files; keep the specification clear and actionable for the BUILDER agent.`, task.Goal, task.Context)

	archOutput, err := r.runOne(tctx, "architect", archPrompt, "PLANNER", onUpdate)
	if err != nil {
		return nil, fmt.Errorf("architect phase failed: %w", err)
	}
	res.ArchitectSpec = strings.TrimSpace(archOutput)

	// ── Phase 2: BUILDER (Surgical Implementation) ──────────────────────────
	if onUpdate != nil {
		onUpdate(loop.StateActing, "🔨 [Swarm] BUILDER: Implementing blueprint with surgical edits...")
	}

	buildPrompt := fmt.Sprintf(`You are the BUILDER specialist in a collaborative swarm.
GOAL: %s
ARCHITECT BLUEPRINT:
%s

TASK:
1. Apply the blueprint surgically using edit_file / write_file.
2. Avoid over-engineering; keep changes minimal, robust, and DRY.
3. Once edits are made, summarize exactly what was modified.`, task.Goal, res.ArchitectSpec)

	buildOutput, err := r.runOne(tctx, "builder", buildPrompt, "BUILDER", onUpdate)
	if err != nil {
		return nil, fmt.Errorf("builder phase failed: %w", err)
	}
	res.BuilderOutput = strings.TrimSpace(buildOutput)

	// ── Phase 3: AUDITOR (Verification & Quality Gate) ──────────────────────
	if onUpdate != nil {
		onUpdate(loop.StateActing, "🕵️ [Swarm] AUDITOR: Reviewing quality, tests, and security...")
	}

	auditPrompt := fmt.Sprintf(`You are the AUDITOR specialist in a collaborative swarm.
GOAL: %s
BUILDER SUMMARY:
%s

TASK:
1. Inspect the modified code for syntax errors, regressions, N+1 query patterns, or missing error checks.
2. If project tests exist, verify them or check LSP diagnostics.
3. Provide a final verdict (PASSED or ISSUES_FOUND) with concise findings.`, task.Goal, res.BuilderOutput)

	auditOutput, err := r.runOne(tctx, "auditor", auditPrompt, "PLANNER", onUpdate)
	if err != nil {
		return nil, fmt.Errorf("auditor phase failed: %w", err)
	}
	res.AuditorVerdict = strings.TrimSpace(auditOutput)
	res.Success = !strings.Contains(strings.ToUpper(res.AuditorVerdict), "ISSUES_FOUND")
	res.Duration = time.Since(start).Round(time.Millisecond).String()

	if onUpdate != nil {
		status := "COMPLETED"
		if !res.Success {
			status = "REQUIRES_ATTENTION"
		}
		onUpdate(loop.StateObserving, fmt.Sprintf("✨ [Swarm] %s in %s", status, res.Duration))
	}

	return res, nil
}

// SwarmTool exposes the collaborative swarm to the agent loop.
type SwarmTool struct {
	Runner *Runner
}

func (t *SwarmTool) Name() string { return "swarm_execute" }
func (t *SwarmTool) Description() string {
	return "Coordinate a 3-agent specialist swarm (Architect -> Builder -> Auditor) for complex refactoring or multi-file features. The swarm plans, implements, and audits autonomously."
}
func (t *SwarmTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal": map[string]any{
				"type":        "string",
				"description": "High-level goal for the swarm to accomplish.",
			},
			"context": map[string]any{
				"type":        "string",
				"description": "Optional background hints or relevant file paths.",
			},
			"auto_verify": map[string]any{
				"type":        "boolean",
				"description": "Whether the auditor should run project test commands.",
			},
		},
		"required": []string{"goal"},
	}
}

func (t *SwarmTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	if t.Runner == nil {
		return "", fmt.Errorf("swarm runner is not configured")
	}
	var args struct {
		Goal       string `json:"goal"`
		Context    string `json:"context"`
		AutoVerify bool   `json:"auto_verify"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Goal) == "" {
		return "", fmt.Errorf("goal cannot be empty")
	}

	task := SwarmTask{
		Goal:       args.Goal,
		Context:    args.Context,
		AutoVerify: args.AutoVerify,
	}

	result, err := t.Runner.ExecuteSwarm(ctx, task, nil)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🐝 Swarm Pipeline Finished in %s\n\n", result.Duration))
	sb.WriteString("=== 🏛️ ARCHITECT SPEC ===\n" + result.ArchitectSpec + "\n\n")
	sb.WriteString("=== 🔨 BUILDER OUTPUT ===\n" + result.BuilderOutput + "\n\n")
	sb.WriteString("=== 🕵️ AUDITOR VERDICT ===\n" + result.AuditorVerdict + "\n")
	return sb.String(), nil
}
