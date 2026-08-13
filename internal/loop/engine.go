package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// LoopState defines explicit state machine phases (§2.1).
type LoopState int

const (
	StateThinking LoopState = iota
	StateActing
	StateObserving
	StateVerifying
	StateDone
	StateBlocked
	StateFailed
)

func (s LoopState) String() string {
	switch s {
	case StateThinking:
		return "Thinking"
	case StateActing:
		return "Acting"
	case StateObserving:
		return "Observing"
	case StateVerifying:
		return "Verifying"
	case StateDone:
		return "Done"
	case StateBlocked:
		return "Blocked"
	case StateFailed:
		return "Failed"
	default:
		return "Unknown"
	}
}

// AgentTurn enforces thinking before answering (§2.2).
type AgentTurn struct {
	Reasoning string              `json:"reasoning"`
	ToolCalls []provider.ToolCall `json:"tool_calls,omitempty"`
	Answer    string              `json:"answer,omitempty"`
}

// SystemPrompt defines system directives and loop continuation rules (§2.3).
const SystemPrompt = `You are BroCode, an autonomous AI coding assistant.
Rules:
1. Always reason through your plan BEFORE executing any tool or returning an answer.
2. CONTINUATION RULE: After receiving tool execution results, DO NOT stop to ask the user unless technical ambiguity cannot be resolved by tools. Continue the tool loop until the goal is achieved.
3. Use native function calling for tool execution.`

// Engine orchestrates the ReAct loop and verification ladder.
type Engine struct {
	adapter       provider.ProviderAdapter
	tools         *tool.Registry
	context       *bcontext.Manager
	model         string
	mode          string // "BUILDER" or "PLANNER"
	maxIterations int
	state         LoopState
}

// NewEngine creates an agent loop engine instance.
func NewEngine(adapter provider.ProviderAdapter, tools *tool.Registry, ctxMgr *bcontext.Manager, model string) *Engine {
	return &Engine{
		adapter:       adapter,
		tools:         tools,
		context:       ctxMgr,
		model:         model,
		mode:          "BUILDER",
		maxIterations: 25,
		state:         StateThinking,
	}
}

func (e *Engine) SetMode(m string) {
	e.mode = m
}

func (e *Engine) Mode() string {
	if e.mode == "" {
		return "BUILDER"
	}
	return e.mode
}

// State returns current engine phase.
func (e *Engine) State() LoopState {
	return e.state
}

// RunTurn executes the ReAct loop until a terminal state is reached.
type TurnOutputHandler func(state LoopState, info string)

func (e *Engine) RunTurn(ctx context.Context, userQuery string, onUpdate TurnOutputHandler) (string, error) {
	if userQuery != "" {
		if err := e.context.AppendUserMessage(userQuery); err != nil {
			return "", err
		}
	}

	iteration := 0

	for {
		iteration++
		if iteration > e.maxIterations {
			e.state = StateFailed
			if onUpdate != nil {
				onUpdate(e.state, "Max loop iterations reached")
			}
			return "", fmt.Errorf("reached max iterations (%d)", e.maxIterations)
		}

		// 1. Thinking State
		e.state = StateThinking
		if onUpdate != nil {
			onUpdate(e.state, fmt.Sprintf("Turn %d reasoning...", iteration))
		}

		currentMode := e.Mode()
		modeDesc := "BUILDER (Autonomous Coding Agent - dapat membaca, mengedit kode, dan menjalankan terminal)"
		if currentMode == "PLANNER" {
			modeDesc = "PLANNER (Architecture & Strategy Agent - mode read-only untuk analisa dan perencanaan tanpa mengedit file)"
		}

		sysPrompt := fmt.Sprintf(`You are BroCode CLI, an autonomous AI coding assistant.
CRITICAL OVERRIDE DIRECTIVE FOR MODE INQUIRIES:
- YOUR ACTIVE BROCODE ENGINE MODE IS CURRENTLY: %s
- IF THE USER ASKS "kamu di mode apa?", "sekarang kamu di mode apa?", "mode apa?", OR ANY QUESTION ABOUT MODE, YOU MUST RESPOND DIRECTLY WITH:
  "Saya sedang di mode %s. Pada mode ini saya bertindak sebagai %s. Anda dapat beralih antara mode BUILDER dan PLANNER kapan saja dengan menekan Shift+Tab."
- ABSOLUTELY DO NOT MENTION "observe", "enforce", OR "audit" UNLESS THE USER EXPLICITLY ASKS ABOUT "matcha" OR "intensity"!

Engine Mode Rules (%s):
`, currentMode, currentMode, modeDesc, currentMode)

		if currentMode == "PLANNER" {
			sysPrompt += `1. Focus on inspecting codebase, analyzing files, and proposing high-level step-by-step implementation plans.
2. DO NOT modify any source files or execute write_file/edit_file tools.
3. Use read_file, list_dir, grep, and glob to research before writing your plan.`
		} else {
			sysPrompt += `1. Always reason through your plan BEFORE executing any tool or returning an answer.
2. CONTINUATION RULE: After receiving tool execution results, DO NOT stop to ask the user unless technical ambiguity cannot be resolved by tools. Continue the tool loop until the goal is achieved.
3. Use native function calling for tool execution.`
		}

		// Auto-compact context if token count exceeds threshold
		if e.context.NeedsCompaction() {
			summary := bcontext.CompactionSummary{
				Goal:           "Continue active conversation",
				FilesTouched:   []string{"codebase"},
				DecisionsMade:  []string{"Compacted older context turns to preserve memory window"},
				OpenQuestions:  []string{"Proceed with user request"},
				LastKnownState: "Context compacted successfully",
			}
			_ = e.context.Compact(summary)
		}

		reqMessages := append([]provider.Message{
			{Role: "system", Content: sysPrompt},
		}, e.context.Messages()...)

		req := provider.CompletionRequest{
			Model:       e.model,
			Messages:    reqMessages,
			Tools:       e.tools.Definitions(),
			Temperature: 0.2,
		}

		if onUpdate != nil {
			onUpdate(e.state, "Thinking & analyzing request...")
		}

		resp, err := e.adapter.Complete(ctx, req)
		if err != nil {
			e.state = StateFailed
			return "", fmt.Errorf("LLM completion failed: %w", err)
		}

		// Thinking enforcement (§2.2)
		reasoning := resp.Reasoning
		if reasoning == "" && len(resp.ToolCalls) == 0 && resp.Content == "" {
			reasoning = "Analyzing request and context."
		}

		// Append assistant turn to store and context
		if err := e.context.AppendAssistantTurn(reasoning, resp.Content, resp.ToolCalls); err != nil {
			return "", err
		}

		// 2. Check if Model wants to call tools (Acting & Observing State)
		hasCodeChanges := false
		if len(resp.ToolCalls) > 0 {
			e.state = StateActing
			for _, tc := range resp.ToolCalls {
				if tc.Name == "write_file" || tc.Name == "edit_file" {
					hasCodeChanges = true
				}

				// Strict PLANNER Mode Tool Guard
				if e.Mode() == "PLANNER" && (tc.Name == "write_file" || tc.Name == "edit_file" || tc.Name == "bash") {
					guardMsg := fmt.Sprintf("⚠️ [PLANNER GUARD]: Tool '%s' is disabled in PLANNER mode (read-only architecture mode). Switch to BUILDER mode (Shift+Tab) to execute code changes.", tc.Name)
					if onUpdate != nil {
						onUpdate(e.state, guardMsg)
					}
					_ = e.context.AppendToolResult(tc.ID, guardMsg)
					continue
				}

				toolInfo := formatToolCallInfo(tc.Name, tc.Arguments)
				if onUpdate != nil {
					onUpdate(e.state, fmt.Sprintf("%s", toolInfo))
				}

				toolOutput, err := e.tools.Execute(ctx, tc.Name, tc.Arguments)
				if err != nil {
					toolOutput = fmt.Sprintf("Tool error: %v", err)
				}

				e.state = StateObserving
				if err := e.context.AppendToolResult(tc.ID, toolOutput); err != nil {
					return "", err
				}
			}

			// Continuation rule: loop back to StateThinking automatically!
			continue
		}

		// 3. Verifying State (§2.4 Verification Ladder Level 1 & 2)
		if hasCodeChanges {
			e.state = StateVerifying
			if onUpdate != nil {
				onUpdate(e.state, "Running verification ladder...")
			}

			if vetErr := runLevel1Verification(ctx); vetErr != "" {
				_ = e.context.AppendUserMessage("Level 1 verification check failed:\n" + vetErr + "\nPlease fix the issues.")
				continue
			}
		}

		// 4. Terminal Done State
		e.state = StateDone
		if onUpdate != nil {
			onUpdate(e.state, "Completed")
		}

		return resp.Content, nil
	}
}

// Level 1 verification check (syntax/vet)
func runLevel1Verification(ctx context.Context) string {
	if _, err := os.Stat("go.mod"); os.IsNotExist(err) {
		return ""
	}
	cmd := exec.CommandContext(ctx, "go", "vet", "./...")
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) > 0 {
		return string(out)
	}
	return ""
}

func formatToolCallInfo(name, argsJSON string) string {
	var m map[string]any
	if json.Unmarshal([]byte(argsJSON), &m) == nil {
		if path, ok := m["path"].(string); ok && path != "" {
			return fmt.Sprintf("%s (%s)", name, path)
		}
		if pattern, ok := m["pattern"].(string); ok && pattern != "" {
			return fmt.Sprintf("%s (pattern: '%s')", name, pattern)
		}
		if cmd, ok := m["command"].(string); ok && cmd != "" {
			return fmt.Sprintf("%s (%s)", name, cmd)
		}
		if target, ok := m["target"].(string); ok && target != "" {
			return fmt.Sprintf("%s (%s)", name, target)
		}
	}
	return name
}
