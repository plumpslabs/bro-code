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
} // Legacy-v1 OpenCode free model list
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
	// AskUser presents interactive questions to the user when the CLI model
	// writes a structured clarification block ([Q]/[O]) instead of being able
	// to call the ask_user tool. Wired by the TUI; nil = headless (no modal,
	// the question text is returned as-is).
	AskUser AskUserHandler
	// MCPStatus is a short summary of connected MCP servers (names only),
	// injected into the CLI prompt so the model answers MCP questions directly
	// from context instead of exploring config files with bash (which opencode's
	// own permission system then blocks). Wired by the TUI; empty = omit.
	MCPStatus string
}

// NewOpenCodeAdapter creates an OpenCode provider adapter.
func NewOpenCodeAdapter() *OpenCodeAdapter {
	detected, binPath := DetectOpenCode()
	a := &OpenCodeAdapter{
		// HTTP fallback uses the official free-model gateway (same base URL
		// declared in the registry) — never a personal/third-party endpoint.
		http: NewOpenAIAdapter("https://router.opencode.ai/v1", ""),
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
//
// When an interactive AskUser handler is wired (the TUI), clarification
// questions the CLI model writes as structured [Q]/[O] blocks are intercepted:
// the block is removed from the answer, presented as the interactive selection
// modal, and the user's answers are fed back to the CLI so the model continues
// and produces its final answer within the same turn.
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

	// The CLI model runs inside opencode's own agent loop with opencode's
	// system prompt, so it would identify itself as "opencode". Anchor the
	// identity with a short preamble — it only shapes identity answers.
	prompt := brocodeIdentityPrompt + "\n\n" + userPrompt

	// Orient the model about what MCP servers BroCode has connected, so
	// questions like "what MCP is available?" are answered from context
	// instead of triggering filesystem exploration.
	if a.MCPStatus != "" {
		prompt += "\n\n" + a.MCPStatus
	}

	// When the interactive ask modal is available, teach the CLI model to
	// structure its clarification questions so they can become the modal.
	// Without a handler (headless) the model behaves exactly as before.
	if a.AskUser != nil {
		prompt += "\n\n" + askMarkerInstructions
	}

	// Ask/answer rounds: the model may write question blocks, we present them
	// as the modal, feed the answers back, and let it finish. Capped so a
	// model that keeps asking can never loop forever.
	const maxAskRounds = 3
	lastClean := ""
	for round := 0; round < maxAskRounds; round++ {
		cleanOut, ok := a.runCLI(ctx, opencodeMod, prompt, onProgress)
		if !ok {
			// CLI failed, timed out, or produced nothing: fall back to the
			// HTTP router with the original request.
			return a.http.Complete(ctx, req)
		}

		questions, cleaned := ParseAskBlocks(cleanOut)
		if len(questions) == 0 || a.AskUser == nil {
			return buildCLIResponse(userPrompt, cleanOut, opencodeMod)
		}
		lastClean = cleaned

		results, err := a.AskUser(ctx, questions)
		if err != nil || len(results) == 0 {
			// User skipped or the interaction failed: return the model's answer
			// with the question block stripped (the questions were already
			// visible in the modal the user just dismissed).
			return buildCLIResponse(userPrompt, cleaned, opencodeMod)
		}

		prompt = brocodeIdentityPrompt + "\n\n" + userPrompt
		if a.MCPStatus != "" {
			prompt += "\n\n" + a.MCPStatus
		}
		prompt += "\n\n" + cleaned +
			"\n\nThe user answered your clarification questions:\n" +
			formatAskResults(results) +
			"\n\nContinue and provide your final answer now. If everything is clear, answer directly and do NOT include any question blocks."
	}

	// Ran out of ask rounds: return the last cleaned answer (no question blocks).
	return buildCLIResponse(userPrompt, lastClean, opencodeMod)
}

// runCLI executes one `opencode run --model <model> <prompt>` invocation with
// a bounded timeout and returns the cleaned answer. ok is false when the CLI
// is missing, times out, or produces no output (callers fall back to HTTP).
func (a *OpenCodeAdapter) runCLI(ctx context.Context, model, prompt string, onProgress func(string)) (string, bool) {
	// Bound the CLI run so a hung opencode process (no response, network
	// stall, waiting on input) can never block the turn forever. On timeout
	// the subprocess is killed and we fall back to the HTTP router.
	completionCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(completionCtx, a.cliPath, "run", "--model", model, prompt)
	cmd.Stdin = strings.NewReader("") // Non-blocking stdin pipe prevents TTY hanging

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", false
	}

	if err := cmd.Start(); err != nil {
		return "", false
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
		return "", false
	}

	// Answer = clean stdout; fall back to last stderr block if stdout empty.
	cleanOut := stripOpenCodeHeader(stdoutStr)
	if cleanOut == "" {
		cleanOut = stripOpenCodeHeader(strings.Join(stderrLines, "\n"))
	}

	if err == nil && cleanOut != "" {
		return cleanOut, true
	}
	return "", false
}

// buildCLIResponse assembles the completion response for a successful CLI run.
func buildCLIResponse(userPrompt, content, model string) (*CompletionResponse, error) {
	return &CompletionResponse{
		Content:   content,
		Reasoning: "Executed via local OpenCode CLI (" + model + ")",
		Usage: Usage{
			PromptTokens:     len(userPrompt) / 4,
			CompletionTokens: len(content) / 4,
			TotalTokens:      (len(userPrompt) + len(content)) / 4,
		},
		FinishReason: "stop",
	}, nil
}
