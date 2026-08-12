package agentic

import (
	"strings"
)

// TaskComplexity represents the evaluated complexity of a user prompt.
type TaskComplexity int

const (
	FastPath TaskComplexity = iota // Score 0-3 (direct edit/verify)
	NormalPath                     // Score 4-6 (inspect -> implement -> verify)
	DeepPath                       // Score 7+ (investigate -> plan -> impact -> implement -> review)
)

// EvaluateComplexity scores a prompt and returns the routing path.
// This is a naive but fast heuristic avoiding extra LLM calls.
func EvaluateComplexity(prompt string) (TaskComplexity, int) {
	score := 0
	lowerPrompt := strings.ToLower(prompt)

	// Length heuristics
	if len(prompt) > 200 {
		score += 2
	} else if len(prompt) > 50 {
		score += 1
	}

	// Keyword heuristics
	deepKeywords := []string{"architect", "refactor", "migrate", "design", "security", "auth"}
	for _, k := range deepKeywords {
		if strings.Contains(lowerPrompt, k) {
			score += 3
		}
	}

	normalKeywords := []string{"add", "create", "implement", "update", "fix", "bug"}
	for _, k := range normalKeywords {
		if strings.Contains(lowerPrompt, k) {
			score += 1
		}
	}

	if score >= 7 {
		return DeepPath, score
	} else if score >= 4 {
		return NormalPath, score
	}
	return FastPath, score
}
