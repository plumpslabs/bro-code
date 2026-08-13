package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Legacy-v1 OpenCode free model list
var OpenCodeFreeModels = []string{
	"deepseek-v4-flash-free",
	"hy3-free",
	"mimo-v2.5-free",
	"laguna-s-2.1-free",
	"ling-3.0-tiny-free",
	"longcat-2.0-free",
	"nemotron-3-ultra-free",
	"nemotron-3.5-lightning-free",
	"big-pickle",
}

// DetectOpenCode checks if OpenCode CLI binary or config exists locally.
func DetectOpenCode() (bool, string) {
	if binPath, err := exec.LookPath("opencode"); err == nil && binPath != "" {
		return true, binPath
	}

	home, err := os.UserHomeDir()
	if err == nil {
		paths := []string{
			filepath.Join(home, ".opencode", "bin", "opencode"),
			filepath.Join(home, ".config", "opencode"),
			filepath.Join(home, ".local", "share", "opencode"),
		}
		for _, p := range paths {
			if st, err := os.Stat(p); err == nil {
				if !st.IsDir() {
					return true, p
				}
				bin := filepath.Join(p, "bin", "opencode")
				if _, err := os.Stat(bin); err == nil {
					return true, bin
				}
				return true, "opencode"
			}
		}
	}
	return false, ""
}

// OpenCodeAdapter routes requests to local OpenCode CLI or falls back to OpenAI-compatible router endpoint.
type OpenCodeAdapter struct {
	cliPath string
	http    *OpenAIAdapter
}

// NewOpenCodeAdapter creates an OpenCode provider adapter.
func NewOpenCodeAdapter() *OpenCodeAdapter {
	detected, binPath := DetectOpenCode()
	a := &OpenCodeAdapter{
		http: NewOpenAIAdapter("https://9router.rosyidrid.com/v1", ""),
	}
	if detected {
		a.cliPath = binPath
	}
	return a
}

func (a *OpenCodeAdapter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	if a.cliPath != "" {
		opencodeMod := req.Model
		if !strings.HasPrefix(opencodeMod, "opencode/") && !strings.HasPrefix(opencodeMod, "lalarasa/") {
			opencodeMod = "opencode/" + req.Model
		}

		userPrompt := ""
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" && req.Messages[i].Content != "" {
				userPrompt = req.Messages[i].Content
				break
			}
		}
		if userPrompt == "" && len(req.Messages) > 0 {
			userPrompt = req.Messages[len(req.Messages)-1].Content
		}

		cmd := exec.CommandContext(ctx, a.cliPath, "run", "--model", opencodeMod, userPrompt)
		cmd.Stdin = strings.NewReader("") // Non-blocking stdin pipe prevents TTY hanging
		out, err := cmd.CombinedOutput()

		rawOut := strings.TrimSpace(string(out))
		cleanOut := rawOut
		// Strip > build headers from OpenCode CLI output
		if idx := strings.Index(cleanOut, "\n\n"); idx >= 0 {
			cleanOut = strings.TrimSpace(cleanOut[idx+2:])
		} else if strings.HasPrefix(cleanOut, "> build") {
			lines := strings.Split(cleanOut, "\n")
			if len(lines) > 1 {
				cleanOut = strings.TrimSpace(strings.Join(lines[1:], "\n"))
			}
		}

		if err == nil && cleanOut != "" {
			return &CompletionResponse{
				Content:   cleanOut,
				Reasoning: "Executed via local OpenCode CLI (" + opencodeMod + ")",
				Usage: Usage{
					PromptTokens:     len(userPrompt) / 4,
					CompletionTokens: len(cleanOut) / 4,
					TotalTokens:      (len(userPrompt) + len(cleanOut)) / 4,
				},
				FinishReason: "stop",
			}, nil
		}
	}

	return a.http.Complete(ctx, req)
}
