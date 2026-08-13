package provider

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ansiEscapeRe matches ANSI escape sequences (colors, cursor moves).
var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// isSpinnerPrefix reports whether a line starts with an OpenCode spinner
// frame glyph or a status prompt prefix (❯ → etc.).
func isSpinnerPrefix(line string) bool {
	for _, p := range []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "❯", "→", "├", "┃", "⬢"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

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

// stripOpenCodeHeader removes OpenCode CLI status banners (spinner frames,
// "> build · <model>" lines, prompt prefix rows) that precede the actual
// answer, including any ANSI escapes around them.
func stripOpenCodeHeader(raw string) string {
	out := strings.TrimSpace(raw)

	lines := strings.Split(out, "\n")
	var clean []string
	skipping := true
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		// Drop ANSI sequences to inspect the line content.
		plain := ansiEscapeRe.ReplaceAllString(trimmed, "")
		plain = strings.TrimSpace(plain)

		if skipping {
			if plain == "" {
				continue
			}
			// Plain "|" is a status prefix ONLY when it's the "build" banner
			// (markdown tables legitimately start with |).
			isStatus := strings.Contains(plain, "build ·") || strings.Contains(plain, "build •") ||
				strings.Contains(plain, "build·") ||
				strings.HasPrefix(plain, ">") || strings.HasPrefix(plain, "│") ||
				isSpinnerPrefix(plain)
			if isStatus {
				continue
			}
			skipping = false
		}
		clean = append(clean, l)
	}

	res := strings.TrimSpace(strings.Join(clean, "\n"))
	// Defensive: drop the "[0m" artifact some CLIs emit at the very start.
	res = strings.TrimPrefix(res, "[0m")
	return strings.TrimSpace(res)
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
	return a.CompleteWithProgress(ctx, req, nil)
}

// CompleteWithProgress runs the local OpenCode CLI (if available) while
// streaming its output lines to onProgress in real time — the agent's steps
// (build banner, tool usage, thinking) become visible in the UI exactly like
// opencode itself. The final answer is the last meaningful block of output.
func (a *OpenCodeAdapter) CompleteWithProgress(ctx context.Context, req CompletionRequest, onProgress func(string)) (*CompletionResponse, error) {
	if a.cliPath == "" {
		return a.http.Complete(ctx, req)
	}

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

	// Bound the CLI run so a hung opencode process (no response, network
	// stall, waiting on input) can never block the turn forever. On timeout
	// the subprocess is killed and we fall back to the HTTP router.
	completionCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(completionCtx, a.cliPath, "run", "--model", opencodeMod, userPrompt)
	cmd.Stdin = strings.NewReader("") // Non-blocking stdin pipe prevents TTY hanging

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return a.http.Complete(ctx, req)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return a.http.Complete(ctx, req)
	}

	if err := cmd.Start(); err != nil {
		return a.http.Complete(ctx, req)
	}

	// Forward stderr lines (status/progress) to the UI in real time.
	stderrDone := make(chan []string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		var lines []string
		last := ""
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			lines = append(lines, line)
			// Dedupe rapid spinner flicker, keep meaningful progress.
			plain := stripOpenCodeHeader(line)
			if plain != "" && plain != last && onProgress != nil {
				onProgress(plain)
				last = plain
			}
		}
		stderrDone <- lines
	}()

	// Accumulate stdout (the actual answer). The goroutine hands the finished
	// buffer back over a channel so the main path never races a strings.Builder
	// (data race) and never reads a half-drained pipe.
	stdoutDone := make(chan string, 1)
	go func() {
		var sb strings.Builder
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			sb.WriteString(scanner.Text() + "\n")
		}
		stdoutDone <- sb.String()
	}()

	err = cmd.Wait()
	stderrLines := <-stderrDone
	stdoutStr := <-stdoutDone

	if completionCtx.Err() == context.DeadlineExceeded {
		// Never surface a half-baked stream as the answer.
		return a.http.Complete(ctx, req)
	}

	// Answer = clean stdout; fall back to last stderr block if stdout empty.
	cleanOut := stripOpenCodeHeader(stdoutStr)
	if cleanOut == "" {
		cleanOut = stripOpenCodeHeader(strings.Join(stderrLines, "\n"))
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

	// CLI failed or produced nothing: fall back to the HTTP router.
	return a.http.Complete(ctx, req)
}
