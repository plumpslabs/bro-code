// agent.go — Provider execution layer: runs the active LLM provider for one
// prompt, handles SSE streaming, CLI wrappers, and the mock fallback pipeline.
package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	
	"github.com/plumpslabs/bro-code/internal/agentic"

	tea "charm.land/bubbletea/v2"
)

// sendPhase safely pushes a status/trace update to the TUI.
// Non-blocking if the channel is full (the run continues regardless).
func sendPhase(ch chan<- agentTraceMsg, phase, line string) {
	if ch == nil {
		return
	}
	select {
	case ch <- agentTraceMsg{phase: phase, line: line}:
	default:
	}
}

// ── Unified Phase Workflow ──────────────────────────────────────────────
// All providers follow the same phase progression for consistent UX:
//
//	thinking… → [processing… →] receiving response… → done
//
// Streaming providers (OpenCode) add intermediate phases:
//
//	thinking… → reasoning… → writing response… → done
//
// The providerWorkflow struct encapsulates the common pattern.
type providerWorkflow struct {
	ch        chan<- agentTraceMsg
	provider  string
	model     string
	startTime time.Time
}

// newWorkflow creates a workflow helper for a provider.
func newWorkflow(ch chan<- agentTraceMsg, provider, model string, startTime time.Time) *providerWorkflow {
	return &providerWorkflow{ch: ch, provider: provider, model: model, startTime: startTime}
}

// phaseThinking sends the initial thinking phase (first thing every provider does).
func (w *providerWorkflow) phaseThinking() {
	sendPhase(w.ch, "thinking…", "→ "+w.provider+" · "+w.model)
}

// phaseProcessing sends the processing/connecting phase.
func (w *providerWorkflow) phaseProcessing() {
	sendPhase(w.ch, "processing…", "→ connecting to "+w.provider+" API")
}

// phaseReceiving sends the receiving response phase.
func (w *providerWorkflow) phaseReceiving() {
	sendPhase(w.ch, "receiving response…", "→ waiting for model reply")
}

// phaseDone sends the final done phase with elapsed time.
func (w *providerWorkflow) phaseDone(elapsed time.Duration, detail string) {
	sendPhase(w.ch, "done", "→ "+detail+" · "+fmt.Sprintf("%.1fs", elapsed.Seconds()))
}

// phaseError sends an error phase.
func (w *providerWorkflow) phaseError(errMsg string) {
	sendPhase(w.ch, "error", "→ "+errMsg)
}

// appendTrace appends one dimmed process line to the in-chat log, bounded.
func appendTrace(trace []string, line string) []string {
	trace = append(trace, line)
	if len(trace) > maxTrace {
		trace = trace[len(trace)-maxTrace:]
	}
	return trace
}

// detectPhase infers agent activity phase from an output line.
// Matches both generic keywords AND opencode-specific output format.
func detectPhase(line string) string {
	lower := strings.ToLower(line)
	trimmed := strings.TrimSpace(line)

	// ── opencode-specific patterns (→Read, ✱Glob, + Thought, etc.) ──
	if strings.HasPrefix(trimmed, "→Read") || strings.HasPrefix(trimmed, "→read") {
		return "reading files…"
	}
	if strings.HasPrefix(trimmed, "✱Glob") || strings.HasPrefix(trimmed, "✱glob") {
		return "searching files…"
	}
	if strings.HasPrefix(trimmed, "+ Thought") || strings.HasPrefix(trimmed, "+ thought") {
		return "thinking…"
	}
	if strings.HasPrefix(trimmed, "→Edit") || strings.HasPrefix(trimmed, "→Write") || strings.HasPrefix(trimmed, "→edit") || strings.HasPrefix(trimmed, "→write") {
		return "writing code…"
	}
	if strings.HasPrefix(trimmed, "❯ Bash") || strings.HasPrefix(trimmed, "❯ bash") || strings.HasPrefix(trimmed, "→Bash") {
		return "running command…"
	}
	if strings.HasPrefix(trimmed, "→MCP") || strings.HasPrefix(trimmed, "→mcp") {
		return "using tool…"
	}

	// ── Generic patterns (fallback for other providers) ──
	switch {
	case strings.Contains(lower, "reading") || strings.Contains(lower, "scanning") || strings.Contains(lower, "searching"):
		return "reading files…"
	case strings.Contains(lower, "thinking") || strings.Contains(lower, "reasoning"):
		return "thinking…"
	case strings.Contains(lower, "writing") || strings.Contains(lower, "editing") || strings.Contains(lower, "creating"):
		return "writing code…"
	case strings.Contains(lower, "running") || strings.Contains(lower, "executing") || strings.Contains(lower, "command"):
		return "running command…"
	case strings.Contains(lower, "tool") || strings.Contains(lower, "function_call") || strings.Contains(lower, "tool_use"):
		return "using tool…"
	case strings.Contains(lower, "bash") || strings.Contains(lower, "shell"):
		return "running shell…"
	default:
		return ""
	}
}

// zenEndpoint is the OpenCode Zen gateway — the OpenAI-compatible chat
// completions endpoint the opencode CLI routes its free "opencode/" models
// through. Free models are served WITHOUT an API key (verified empirically
// Aug 2026: zero credentials on the machine, direct curl succeeds).
const zenEndpoint = "https://opencode.ai/zen/v1/chat/completions"

// stripAttribution removes the "⚡ provider/model · time · tokens" footer that
// brocode appends to every reply, so it never pollutes the model's context on
// later turns.
func stripAttribution(s string) string {
	if idx := strings.LastIndex(s, "\n\n  ⚡ "); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}

// systemPromptForMode returns the System Directive instructing the LLM on its
// persona and role. The prompt enforces transparent reasoning: the agent MUST
// show WHY before DOING, and each tool call must be preceded by a rationale.
func systemPromptForMode() string {
	basePersona := "You are BroCode — a cautious, high-performance senior-engineer agent.\n" +
		"You embody the philosophy: Understand → Investigate → Decide → Change → Verify → Review.\n\n" +
		"CRITICAL DIRECTIVES:\n" +
		"1. NEVER make a change merely because you can. If you don't know WHY a change is needed, DO NOT touch it.\n" +
		"2. MINIMAL CHANGE PRINCIPLE: Find the root cause and apply the smallest safe change. Do not refactor entire modules for a bug fix.\n" +
		"3. BLAST RADIUS AWARENESS: Before editing, analyze what else depends on this code. If it's HIGH risk (auth, db, core), you MUST investigate first.\n" +
		"4. EVIDENCE BEFORE CLAIM: Never hallucinate project state. Use search/grep before claiming a dependency or pattern exists.\n" +
		"5. STOP CONDITIONS: Halt and ask the user if: requirement is ambiguous, target behavior is unclear, production impact is unknown, or existing implementation conflicts.\n" +
		"6. DIRTY STATE: Do not overwrite uncommitted user changes without explicitly preserving their intent.\n" +
		"7. VERIFICATION LAYER: Verification is not just tests. It is: syntax -> build -> test -> diff review. ALWAYS verify after changing.\n" +
		"8. FILE EDITING FORMAT: To edit or create files, you MUST use Markdown code blocks with the precise filepath in the language header, like this: ```go:path/to/file.go\n[full file content]\n```.\n" +
		"9. AGENTIC TOOLS: You can run bash commands to investigate before writing code. To run a command, output a ```bash\n(commands here)\n``` block. The system will execute it and automatically return the output to you. To read files, use `cat` or `grep` in the bash block. Once you output a tool block, you must wait for the SYSTEM TOOL RESULT before continuing.\n" +
		"10. TRUE AUTONOMY & SILENT EXECUTION: DO NOT ask the user for permission to create files, edit files, or run commands. DO NOT output conversational filler like 'Step 1: I will create the folder'. Just execute the tool blocks immediately. If you need to reason out loud, you MUST wrap it in a line starting with `+ Thought`.\n\n" +
		"Tone: Crisp, deliberate, expert. You act autonomously directly in the user's workspace. Never ask the user to run commands you can run yourself."

	if data, err := os.ReadFile("AGENTS.md"); err == nil {
		basePersona += "\n\nPROJECT DIRECTIVES (AGENTS.md):\n" + string(data)
	}

	return basePersona
}

// zenMessages builds the OpenAI-style messages array from the bounded chat
// history (prior turns give the model context) plus the current prompt. The
// last chat entry is the just-appended user prompt from send(), so it is
// skipped and re-added explicitly as q. Assistant replies are sent WITHOUT
// their attribution footer.
func zenMessages(chat []chatMsg, q string) []map[string]string {
	messages := make([]map[string]string, 0, len(chat)+2)
	messages = append(messages, map[string]string{
		"role":    "system",
		"content": systemPromptForMode(),
	})
	for i := 0; i < len(chat)-1; i++ {
		switch chat[i].role {
		case roleUser:
			c := chat[i].text
			if c == "" {
				c = chat[i].content
			}
			messages = append(messages, map[string]string{"role": "user", "content": c})
		case roleAgent:
			c := chat[i].text
			if c == "" {
				c = chat[i].content
			}
			messages = append(messages, map[string]string{"role": "assistant", "content": stripAttribution(c)})
		}
	}
	
	complexity, score := agentic.EvaluateComplexity(q)
	route := "FAST PATH (Minimal inspection)"
	if complexity == agentic.DeepPath {
		route = "DEEP PATH (Full plan -> impact -> implement -> review)"
	} else if complexity == agentic.NormalPath {
		route = "NORMAL PATH (Inspect -> implement -> verify)"
	}
	
	// Inject the dynamic routing context seamlessly into the user prompt
	routedPrompt := fmt.Sprintf("%s\n\n[SYSTEM METADATA: Task Complexity Score: %d | Assigned Route: %s. Adjust your workflow depth accordingly.]", q, score, route)
	
	messages = append(messages, map[string]string{"role": "user", "content": routedPrompt})
	return messages
}

// parseZenResponse extracts the answer text, the optional reasoning trace and
// the real token usage from a Zen chat-completions response (OpenAI shape
// plus reasoning_content, which is what the gateway returns for thinking
// models).
func parseZenResponse(body []byte) (text, reasoning string, tok tokenUsage, err error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", tokenUsage{}, err
	}
	if len(resp.Choices) > 0 {
		text = resp.Choices[0].Message.Content
		reasoning = strings.TrimSpace(resp.Choices[0].Message.ReasoningContent)
	}
	tok.input = resp.Usage.PromptTokens
	tok.output = resp.Usage.CompletionTokens
	tok.total = resp.Usage.TotalTokens
	if tok.total == 0 {
		tok.total = tok.input + tok.output // some endpoints omit total_tokens
	}
	return text, reasoning, tok, nil
}

// zenChatReply performs the native OpenCode Zen chat-completions call using
// SSE streaming (`stream: true`) for real-time token delivery. The user sees
// the response being written live instead of a dead "waiting…" spinner.
// Tokens arrive via traceCh; the full response is assembled for the final
// agentResultMsg. cancel is installed as the context cancel so ESC can abort.
func zenChatReply(m Model, q, model, endpoint string, traceCh chan<- agentTraceMsg, startTime time.Time, cancel *func(), run int) tea.Msg {
	label := "opencode/" + model // display name — the wire ID is bare

	// Use unified workflow for initial phases
	wf := newWorkflow(traceCh, "opencode", model, startTime)
	wf.phaseThinking() // → thinking… · opencode/model
	reqBody, err := json.Marshal(map[string]interface{}{
		"model":       model,
		"messages":    zenMessages(m.chat, q),
		"temperature": 0.7,
		"max_tokens":  4096,
		"stream":      true,
	})
	if err != nil {
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("opencode", "", err),
			items: []activityItem{{tool: "opencode", label: "zen request build failed", status: "error", detail: err.Error()}},
		}, run: run}
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	*cancel = ctxCancel // ESC interrupt aborts the request

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(reqBody)))
	if err != nil {
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("opencode", "", err),
			items: []activityItem{{tool: "opencode", label: "zen request failed", status: "error", detail: err.Error()}},
		}, run: run}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("opencode", "", err),
			items: []activityItem{{tool: "opencode", label: "zen request failed", status: "error", detail: err.Error()}},
		}, run: run}
	}
	defer resp.Body.Close()

	// Non-streaming fallback: if the gateway doesn't return SSE (wrong
	// content-type or error status), fall back to reading the full body.
	ct := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(ct, "text/event-stream")
	if resp.StatusCode != http.StatusOK || !isSSE {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return agentResultMsg{reply: mockReply{
				text:  parseCLIError("opencode", string(body), nil),
				items: []activityItem{{tool: "opencode", label: fmt.Sprintf("zen HTTP %d", resp.StatusCode), status: "error", detail: clip(string(body), 100)}},
			}, run: run}
		}
		// Non-SSE 200 — parse as regular JSON response
		text, reasoning, tok, parseErr := parseZenResponse(body)
		if parseErr != nil || text == "" {
			errDetail := "empty response"
			if parseErr != nil {
				errDetail = parseErr.Error()
			}
			return agentResultMsg{reply: mockReply{
				text:  parseCLIError("opencode", string(body), parseErr),
				items: []activityItem{{tool: "opencode", label: "zen parse error", status: "error", detail: errDetail}},
			}, run: run}
		}
		elapsed := time.Since(startTime)
		var col *collapse
		if reasoning != "" {
			col = &collapse{
				summary: "thinking trace (" + fmt.Sprintf("%d chars", len(reasoning)) + ")",
				content: reasoning,
			}
		}
		text += fmt.Sprintf("\n\n  ⚡ %s · %.1fs · %s tokens", label, elapsed.Seconds(), fmtTokens(tok.total))
		return agentResultMsg{
			reply: mockReply{
				text: text, collapse: col,
				items: []activityItem{{tool: "opencode", label: "zen " + label, status: "ok",
					detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), tok.total)}},
			},
			tokens: tok, run: run,
		}
	}

	// ── SSE streaming ──────────────────────────────────────────────────
	sendPhase(traceCh, "reasoning…", "")

	var fullContent strings.Builder
	var reasoning strings.Builder
	var tok tokenUsage
	phase := "reasoning"
	chunkCount := 0

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		// SSE protocol: lines starting with "data: " carry the payload
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		// Parse the SSE chunk (OpenAI streaming delta format)
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Role             string `json:"role"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}

		// Extract token usage if present (some providers send it in the last chunk)
		if chunk.Usage != nil {
			tok.input = chunk.Usage.PromptTokens
			tok.output = chunk.Usage.CompletionTokens
			tok.total = chunk.Usage.TotalTokens
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		// Reasoning/thinking tokens
		if delta.ReasoningContent != "" {
			reasoning.WriteString(delta.ReasoningContent)
			if phase != "reasoning" {
				phase = "reasoning"
				sendPhase(traceCh, "reasoning…", "")
			}
			continue
		}

		// Content tokens — transition from reasoning to writing
		if delta.Content != "" {
			if phase == "reasoning" && fullContent.Len() == 0 {
				phase = "writing"
				sendPhase(traceCh, "writing response…", "")
			}
			fullContent.WriteString(delta.Content)
			chunkCount++
		}

		// Check for finish
		if chunk.Choices[0].FinishReason != nil {
			break
		}
	}

	elapsed := time.Since(startTime)
	text := strings.TrimSpace(fullContent.String())
	reasoningText := strings.TrimSpace(reasoning.String())

	if text == "" && reasoningText != "" {
		text = reasoningText
	}

	if text == "" {
		// Non-streaming fallback retry: stream gave 0 tokens → retry once synchronously with stream: false
		fallbackBody, _ := json.Marshal(map[string]interface{}{
			"model":       model,
			"messages":    zenMessages(m.chat, q),
			"temperature": 0.7,
			"max_tokens":  4096,
			"stream":      false,
		})
		fbReq, fbErr := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(fallbackBody)))
		if fbErr == nil {
			fbReq.Header.Set("Content-Type", "application/json")
			if fbResp, fbDoErr := client.Do(fbReq); fbDoErr == nil {
				defer fbResp.Body.Close()
				if fbData, readErr := io.ReadAll(fbResp.Body); readErr == nil {
					fbText, fbReason, fbTok, pErr := parseZenResponse(fbData)
					if pErr == nil && fbText != "" {
						text = fbText
						if fbReason != "" {
							reasoningText = fbReason
						}
						if fbTok.total > 0 {
							tok = fbTok
						}
					}
				}
			}
		}
	}

	if text == "" {
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("opencode", "empty streaming response from zen gateway", nil),
			items: []activityItem{{tool: "opencode", label: "zen empty response", status: "error", detail: "no content"}},
		}, run: run}
	}

	if tok.total == 0 {
		tok.total = tok.input + tok.output
	}

	var col *collapse
	if reasoningText != "" {
		col = &collapse{
			summary: "thinking trace (" + fmt.Sprintf("%d chars", len(reasoningText)) + ")",
			content: reasoningText,
		}
	}
	text += fmt.Sprintf("\n\n  ⚡ %s · %.1fs · %s tokens", label, elapsed.Seconds(), fmtTokens(tok.total))

	sendPhase(traceCh, "done", fmt.Sprintf("→ %d chunks · %.1fs", chunkCount, elapsed.Seconds()))

	return agentResultMsg{
		reply: mockReply{
			text:     text,
			collapse: col,
			items: []activityItem{{
				tool: "opencode", label: "zen " + label, status: "ok",
				detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), tok.total),
			}},
		},
		tokens: tok,
		run:    run,
	}
}

// agentWorkCmd executes the active provider for one prompt. OpenCode runs
// NATIVELY — brocode calls the OpenCode Zen gateway (OpenAI-compatible, free
// models need no API key) directly over HTTP instead of wrapping the opencode
// CLI. Antigravity still shells out to agy (its API is undocumented), and
// api-key providers (groq) go native over HTTP. cancel is a pointer so the
// in-flight request can be aborted (context cancel / subprocess kill) when
// the user presses ESC. run tags the result so stale/interrupted runs are
// dropped in Update.
func (m Model) agentWorkCmd(q string, traceCh chan<- agentTraceMsg, askCh chan<- agentQuestionMsg, answerCh chan string, cancel *func(), run int) tea.Cmd {
	selectedMod := m.selectedModel
	if selectedMod == "" {
		selectedMod = openCodeFreeModels[0]
	}
	startTime := time.Now()

	return func() tea.Msg {
		defer close(traceCh) // Always close so waitForTrace stops
		defer close(askCh)   // …and waitForAsk

		// OpenCode provider: native HTTP to the Zen gateway — no CLI wrapper.
		if m.provider == "opencode" {
			return zenChatReply(m, q, selectedMod, zenEndpoint, traceCh, startTime, cancel, run)
		}

		// Attempt real antigravity execution via agy CLI runner if installed and antigravity provider active
		// Follows the unified provider workflow: thinking → [processing → receiving → done]
		if m.provider == "antigravity" {
			agyMod := m.selectedModel
			if agyMod == "" {
				agyMod = "gemini-3.6-flash"
			}
			agyBin := "agy"
			if _, err := exec.LookPath("agy"); err != nil {
				// If agy is not in PATH, check ~/.local/bin/agy directly
				home, _ := os.UserHomeDir()
				localAgy := filepath.Join(home, ".local", "bin", "agy")
				if _, errStat := os.Stat(localAgy); errStat == nil {
					agyBin = localAgy
				} else {
					// Auto-install agy CLI package via curl installer
					installCmd := exec.Command("sh", "-c", "curl -fsSL https://antigravity.google/install.sh | sh")
					_ = installCmd.Run()
					if _, errStat2 := os.Stat(localAgy); errStat2 == nil {
						agyBin = localAgy
					}
				}
			}

			// Use unified workflow for initial phase
			wf := newWorkflow(traceCh, "antigravity", agyMod, startTime)
			wf.phaseThinking() // → thinking… · antigravity/model

			// Use context with timeout to prevent hanging
			ctx, ctxCancel := context.WithTimeout(context.Background(), 600*time.Second)
			defer ctxCancel()
			cmd := exec.CommandContext(ctx, agyBin, "run", "--model", agyMod, q)
			agyStdoutPipe, agyErr := cmd.StdoutPipe()
			if agyErr == nil && cmd.Start() == nil {
				*cancel = func() { _ = cmd.Process.Kill() } // ESC interrupt
				var agyOutput strings.Builder
				scanner := bufio.NewScanner(agyStdoutPipe)
				agyLineCount := 0
				// Track last phase to avoid spamming same phase
				lastPhase := ""
				for scanner.Scan() {
					line := scanner.Text()
					agyOutput.WriteString(line + "\n")

					if phase := detectPhase(line); phase != "" {
						if phase != lastPhase {
							// Push the phase AND the real tool line into the chat log.
							sendPhase(traceCh, phase, strings.TrimSpace(line))
							lastPhase = phase
						}
					} else if agyLineCount == 0 {
						sendPhase(traceCh, "generating response…", "→ waiting for first token")
					}
					agyLineCount++
				}
				cmdErr := cmd.Wait()
				elapsed := time.Since(startTime)
				respText := strings.TrimSpace(agyOutput.String())

				// Handle timeout or execution errors
				if cmdErr != nil {
					if ctx.Err() == context.DeadlineExceeded {
						wf.phaseError("timeout after 600s")
						return agentResultMsg{reply: mockReply{
							text:  fmt.Sprintf("⏱️ **Antigravity Timeout**\n\nCommand timed out after 600 seconds.\nThe model may be slow or the request too complex.\n\nPartial output (%d chars):\n%s", len(respText), clip(respText, 200)),
							items: []activityItem{{tool: "antigravity", label: "agy run timeout", status: "error", detail: "600s timeout"}},
						}, run: run}
					}
					// Other execution errors
					if respText == "" {
						wf.phaseError(cmdErr.Error())
						return agentResultMsg{reply: mockReply{
							text:  parseCLIError("antigravity", "", cmdErr),
							items: []activityItem{{tool: "antigravity", label: "agy run failed", status: "error", detail: cmdErr.Error()}},
						}, run: run}
					}
				}

				if len(respText) > 0 {
					attr := fmt.Sprintf("\n\n  ⚡ antigravity/%s · %.1fs", agyMod, elapsed.Seconds())
					respText += attr

					// Phase done: success
					wf.phaseDone(elapsed, fmt.Sprintf("%d chars", len(respText)))

					reply := mockReply{
						text: respText,
						items: []activityItem{
							{tool: "antigravity", label: fmt.Sprintf("agy run --model %s", agyMod), status: "ok", detail: fmt.Sprintf("%.1fs response", elapsed.Seconds())},
						},
					}
					return agentResultMsg{reply: reply, run: run}
				}
				// Empty response with no error - likely agy CLI issue
				if respText == "" {
					wf.phaseError("empty response from agy CLI")
					return agentResultMsg{reply: mockReply{
						text:  "❌ **Antigravity Empty Response**\n\nThe agy CLI returned no output.\nThis could mean:\n• agy is not properly installed or configured\n• The model is unavailable\n• Network connection issue\n\nTry: `agy auth` to re-authenticate, or switch provider with `/connect`",
						items: []activityItem{{tool: "antigravity", label: "agy empty response", status: "error", detail: "no output"}},
					}, run: run}
				}
			} else {
				// Fallback: blocking CombinedOutput if StdoutPipe fails
				out, err := cmd.CombinedOutput()
				elapsed := time.Since(startTime)
				respText := strings.TrimSpace(string(out))

				// Handle timeout
				if ctx.Err() == context.DeadlineExceeded {
					wf.phaseError("timeout after 600s")
					return agentResultMsg{reply: mockReply{
						text:  fmt.Sprintf("⏱️ **Antigravity Timeout**\n\nCommand timed out after 600 seconds.\nPartial output:\n%s", clip(respText, 200)),
						items: []activityItem{{tool: "antigravity", label: "agy run timeout", status: "error", detail: "600s timeout"}},
					}, run: run}
				}

				if err == nil && len(respText) > 0 {
					attr := fmt.Sprintf("\n\n  ⚡ antigravity/%s · %.1fs", agyMod, elapsed.Seconds())
					respText += attr

					// Phase done: success (fallback path)
					wf.phaseDone(elapsed, fmt.Sprintf("%d chars", len(respText)))

					reply := mockReply{
						text: respText,
						items: []activityItem{
							{tool: "antigravity", label: fmt.Sprintf("agy run --model %s", agyMod), status: "ok", detail: fmt.Sprintf("%.1fs response", elapsed.Seconds())},
						},
					}
					return agentResultMsg{reply: reply, run: run}
				}
				errStr := parseCLIError("antigravity", respText, err)
				reply := mockReply{
					text: errStr,
					items: []activityItem{
						{tool: "antigravity", label: fmt.Sprintf("agy run --model %s", agyMod), status: "error", detail: "provider error"},
					},
				}
				return agentResultMsg{reply: reply, run: run}
			}
		}

		// Freebuff provider: native API with saved credentials
		if m.provider == "freebuff" {
			return freebuffNativeAgentRun(m, q, traceCh, askCh, answerCh, startTime, cancel, run)
		}

		// Codebuff provider: native API with saved credentials
		if m.provider == "codebuff" {
			return codebuffNativeAgentRun(m, q, traceCh, askCh, answerCh, startTime, cancel, run)
		}

		// Groq provider: OpenAI-compatible chat completions
		// Follows the unified provider workflow: thinking → processing → receiving → done
		if m.provider == "groq" {
			apiKey := loadAPIKey("groq")
			if apiKey == "" {
				wf := newWorkflow(traceCh, "groq", "unknown", startTime)
				wf.phaseError("API key required")
				reply := mockReply{
					text:  "🔑 **Groq API Key Required**\n\nOpen `/connect` → select Groq → paste API key from console.groq.com",
					items: []activityItem{{tool: "groq", label: "groq api key missing", status: "error", detail: "no key saved"}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			groqModel := m.selectedModel
			if groqModel == "" {
				groqModel = groqModels[0]
			}

			wf := newWorkflow(traceCh, "groq", groqModel, startTime)

			// Phase 1: thinking
			wf.phaseThinking()

			// Build OpenAI-compatible request body
			reqBody := map[string]interface{}{
				"model": groqModel,
				"messages": []map[string]string{
					{"role": "user", "content": q},
				},
				"temperature": 0.7,
				"max_tokens":  4096,
			}
			reqJSON, _ := json.Marshal(reqBody)

			// Phase 2: processing
			wf.phaseProcessing()

			req, err := http.NewRequest("POST", "https://api.groq.com/openai/v1/chat/completions", strings.NewReader(string(reqJSON)))
			if err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("groq", "", err),
					items: []activityItem{{tool: "groq", label: "groq request build failed", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiKey)

			client := &http.Client{Timeout: 600 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("groq", "", err),
					items: []activityItem{{tool: "groq", label: "groq request failed", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				wf.phaseError(fmt.Sprintf("HTTP %d", resp.StatusCode))
				reply := mockReply{
					text:  parseCLIError("groq", string(respBody), nil),
					items: []activityItem{{tool: "groq", label: fmt.Sprintf("groq HTTP %d", resp.StatusCode), status: "error", detail: string(respBody[:min(100, len(respBody))])}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			// Phase 3: receiving response
			wf.phaseReceiving()

			// Parse OpenAI-compatible response
			var groqResp struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(respBody, &groqResp); err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("groq", string(respBody), err),
					items: []activityItem{{tool: "groq", label: "groq parse error", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			elapsed := time.Since(startTime)
			respText := ""
			if len(groqResp.Choices) > 0 {
				respText = groqResp.Choices[0].Message.Content
			}

			tok := tokenUsage{
				input:  groqResp.Usage.PromptTokens,
				output: groqResp.Usage.CompletionTokens,
				total:  groqResp.Usage.TotalTokens,
			}

			if respText != "" {
				attr := fmt.Sprintf("\n\n  ⚡ groq/%s · %.1fs · %s tokens", groqModel, elapsed.Seconds(), fmtTokens(groqResp.Usage.TotalTokens))
				respText += attr

				// Phase 4: done
				wf.phaseDone(elapsed, fmt.Sprintf("%d tokens", tok.total))

				reply := mockReply{
					text: respText,
					items: []activityItem{{
						tool: "groq", label: fmt.Sprintf("groq %s", groqModel), status: "ok",
						detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), groqResp.Usage.TotalTokens),
					}},
				}
				return agentResultMsg{reply: reply, tokens: tok, run: run}
			}
		}

		// Poolside provider: OpenAI-compatible chat completions
		// Follows the unified provider workflow: thinking → processing → receiving → done
		if m.provider == "poolside" {
			apiKey := loadAPIKey("poolside")
			if apiKey == "" {
				wf := newWorkflow(traceCh, "poolside", "unknown", startTime)
				wf.phaseError("API key required")
				reply := mockReply{
					text:  "🔑 **Poolside API Key Required**\n\nOpen `/connect` → select Poolside → paste API key from inference.poolside.ai",
					items: []activityItem{{tool: "poolside", label: "poolside api key missing", status: "error", detail: "no key saved"}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			poolModel := m.selectedModel
			if poolModel == "" {
				poolModel = poolsideModels[0]
			}

			wf := newWorkflow(traceCh, "poolside", poolModel, startTime)
			wf.phaseThinking()

			reqBody := map[string]interface{}{
				"model": poolModel,
				"messages": []map[string]string{
					{"role": "user", "content": q},
				},
				"temperature": 0.7,
				"max_tokens":  4096,
			}
			reqJSON, _ := json.Marshal(reqBody)

			wf.phaseProcessing()

			req, err := http.NewRequest("POST", "https://inference.poolside.ai/v1/chat/completions", strings.NewReader(string(reqJSON)))
			if err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("poolside", "", err),
					items: []activityItem{{tool: "poolside", label: "poolside request build failed", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiKey)

			client := &http.Client{Timeout: 600 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("poolside", "", err),
					items: []activityItem{{tool: "poolside", label: "poolside request failed", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				wf.phaseError(fmt.Sprintf("HTTP %d", resp.StatusCode))
				reply := mockReply{
					text:  parseCLIError("poolside", string(respBody), nil),
					items: []activityItem{{tool: "poolside", label: fmt.Sprintf("poolside HTTP %d", resp.StatusCode), status: "error", detail: string(respBody[:min(100, len(respBody))])}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			wf.phaseReceiving()

			var poolResp struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(respBody, &poolResp); err != nil {
				wf.phaseError(err.Error())
				reply := mockReply{
					text:  parseCLIError("poolside", string(respBody), err),
					items: []activityItem{{tool: "poolside", label: "poolside parse error", status: "error", detail: err.Error()}},
				}
				return agentResultMsg{reply: reply, run: run}
			}

			elapsed := time.Since(startTime)
			respText := ""
			if len(poolResp.Choices) > 0 {
				respText = poolResp.Choices[0].Message.Content
			}

			tok := tokenUsage{
				input:  poolResp.Usage.PromptTokens,
				output: poolResp.Usage.CompletionTokens,
				total:  poolResp.Usage.TotalTokens,
			}

			if respText != "" {
				attr := fmt.Sprintf("\n\n  ⚡ poolside/%s · %.1fs · %s tokens", poolModel, elapsed.Seconds(), fmtTokens(poolResp.Usage.TotalTokens))
				respText += attr

				wf.phaseDone(elapsed, fmt.Sprintf("%d tokens", tok.total))

				reply := mockReply{
					text: respText,
					items: []activityItem{{
						tool: "poolside", label: fmt.Sprintf("poolside %s", poolModel), status: "ok",
						detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), poolResp.Usage.TotalTokens),
					}},
				}
				return agentResultMsg{reply: reply, tokens: tok, run: run}
			}
		}

		if m.provider == "custom" {
			keyData := loadAPIKey("custom")
			parts := strings.Split(keyData, "|")
			endpoint := "http://localhost:11434/v1/chat/completions"
			apiKey := ""
			if len(parts) >= 1 && parts[0] != "" {
				endpoint = parts[0]
				if !strings.HasSuffix(endpoint, "/chat/completions") {
					endpoint = strings.TrimRight(endpoint, "/") + "/chat/completions"
				}
			}
			if len(parts) >= 2 {
				apiKey = parts[1]
			}
			customModel := m.selectedModel
			if customModel == "" {
				customModel = "default"
			}

			wf := newWorkflow(traceCh, "custom", customModel, startTime)
			wf.phaseThinking()
			reqBody := map[string]interface{}{
				"model":       customModel,
				"messages":    zenMessages(m.chat, q),
				"temperature": 0.7,
				"max_tokens":  8192,
			}
			reqJSON, _ := json.Marshal(reqBody)
			wf.phaseProcessing()

			req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(reqJSON)))
			if err != nil {
				wf.phaseError(err.Error())
				return agentResultMsg{reply: mockReply{
					text:  parseCLIError("custom", "", err),
					items: []activityItem{{tool: "custom", label: "request build failed", status: "error", detail: err.Error()}},
				}, run: run}
			}
			req.Header.Set("Content-Type", "application/json")
			if apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			}

			client := &http.Client{Timeout: 600 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				wf.phaseError(err.Error())
				return agentResultMsg{reply: mockReply{
					text:  parseCLIError("custom", "", err),
					items: []activityItem{{tool: "custom", label: "request failed", status: "error", detail: err.Error()}},
				}, run: run}
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				wf.phaseError(fmt.Sprintf("HTTP %d", resp.StatusCode))
				return agentResultMsg{reply: mockReply{
					text:  parseCLIError("custom", string(respBody), nil),
					items: []activityItem{{tool: "custom", label: fmt.Sprintf("HTTP %d", resp.StatusCode), status: "error", detail: string(respBody[:min(100, len(respBody))])}},
				}, run: run}
			}

			wf.phaseReceiving()
			text, _, tok, err := parseZenResponse(respBody)
			if err != nil {
				wf.phaseError(err.Error())
				return agentResultMsg{reply: mockReply{
					text:  parseCLIError("custom", string(respBody), err),
					items: []activityItem{{tool: "custom", label: "parse error", status: "error", detail: err.Error()}},
				}, run: run}
			}

			elapsed := time.Since(startTime)
			if text != "" {
				attr := fmt.Sprintf("\n\n  ⚡ custom/%s · %.1fs · %s tokens", customModel, elapsed.Seconds(), fmtTokens(tok.total))
				text += attr
				wf.phaseDone(elapsed, fmt.Sprintf("%d tokens", tok.total))
				reply := mockReply{
					text: text,
					items: []activityItem{{
						tool: "custom", label: fmt.Sprintf("custom %s", customModel), status: "ok",
						detail: fmt.Sprintf("%.1fs · %d tokens", elapsed.Seconds(), tok.total),
					}},
				}
				return agentResultMsg{reply: reply, tokens: tok, run: run}
			}
		}

		// Fallback mock pipeline — a scripted interactive agent run that shows
		// the full UX: live tool entries in the chat log, a subagent spawn,
		// and a question back to the user (↑↓ choose / type custom). The
		// answer continues the run and shapes the final reply.
		return mockAgentRun(m, q, traceCh, askCh, answerCh, startTime, run)
	}
}

// mockAgentRun simulates a multi-step coding-agent turn so the TUI's live
// process log and interactive question UX are demonstrable without a real
// tool-calling model. It streams dimmed tool entries, spawns a mock subagent,
// asks the user how to continue, then finishes with a tailored reply.
func mockAgentRun(m Model, q string, traceCh chan<- agentTraceMsg, askCh chan<- agentQuestionMsg, answerCh <-chan string, startTime time.Time, run int) tea.Msg {
	const tick = agentLatency / 5

	steps := []struct {
		phase, line string
	}{
		{"thinking…", "→ analyze prompt: " + clip(q, 48)},
		{"reading files…", "→ read internal/tui/app.go"},
		{"searching files…", "✱ glob **/*.go"},
		{"searching files…", "→ grep \"agentWorkCmd\" internal/tui/"},
		{"running command…", "❯ bash go test ./internal/tui/"},
	}
	for _, s := range steps {
		sendPhase(traceCh, s.phase, s.line)
		time.Sleep(tick)
	}

	// Spawn a mock subagent — visible in the right panel agents section.
	sendPhase(traceCh, "spawning subagent…", "(spawn @finder) → locate symbols & dependency tree")
	time.Sleep(tick)

	// Ask the user — the run pauses until they answer or cancel.
	askCh <- agentQuestionMsg{
		prompt: "Task completed. How to proceed?",
		options: []string{
			"Show answer summary",
			"Show change diff",
			"Run tests & report results",
			"Jelaskan detail implementasi",
		},
		run: run,
	}
	answer := <-answerCh
	if answer == "" {
		// Cancelled by the user (esc) — the run is aborted and its late
		// result will be dropped by the run/abort guard in Update.
		return agentResultMsg{reply: mockReply{
			text:  "interrupted — no answer submitted",
			items: []activityItem{{tool: "system", label: "question cancelled", status: "error", detail: "esc"}},
		}, run: run}
	}

	sendPhase(traceCh, "processing answer…", "→ answer diterima: "+clip(answer, 48))
	time.Sleep(tick)

	// Slash commands keep their curated replies; free-form prompts go through
	// the session-memory path (recalls prior turns — no more "sesi baru").
	var reply mockReply
	if strings.HasPrefix(q, "/") {
		reply = buildReply(q, m.index)
	} else {
		reply = conversationalReply(q, m.index, m.chat)
	}
	reply.text = "Your choice: " + answer + "\n\n" + reply.text
	reply.items = prependActivity(reply.items,
		activityItem{tool: "search", label: "grep agentWorkCmd", status: "done", detail: "3 hits"},
		activityItem{tool: "bash", label: "go test ./internal/tui/", status: "done", detail: "all pass"},
	)
	reply.subagents = []subagentState{{name: "finder", task: "locate symbols", status: "done"}}

	elapsed := time.Since(startTime)
	reply.text += fmt.Sprintf("\n\n  ⚡ mock/pipeline · %.1fs", elapsed.Seconds())
	reply.items = append(reply.items, activityItem{tool: "system", label: "mock pipeline", status: "ok", detail: fmt.Sprintf("%.1fs", elapsed.Seconds())})
	return agentResultMsg{reply: reply, run: run}
}

// freebuffNativeAgentRun executes Freebuff via native API with saved credentials.
// Follows the unified provider workflow: thinking → processing → receiving → done.
func freebuffNativeAgentRun(m Model, q string, traceCh chan<- agentTraceMsg, askCh chan<- agentQuestionMsg, answerCh <-chan string, startTime time.Time, cancel *func(), run int) tea.Msg {
	selectedMod := m.selectedModel
	if selectedMod == "" {
		selectedMod = freebuffNativeModels[0]
	}

	wf := newWorkflow(traceCh, "freebuff", selectedMod, startTime)

	// Phase 1: thinking
	wf.phaseThinking()

	// Load credentials
	authToken := loadManicodeCredentials()
	if authToken == "" {
		return agentResultMsg{reply: mockReply{
			text:  "No Freebuff credentials found.\n\nPlease install and login first:\n  npm i -g freebuff\n  freebuff",
			items: []activityItem{{tool: "freebuff", label: "credentials missing", status: "error", detail: "no auth token"}},
		}, run: run}
	}

	// Phase 2: processing
	wf.phaseProcessing()

	// Execute native API call
	response, err := freebuffNativeChat(authToken, selectedMod, q)
	if err != nil {
		wf.phaseError(err.Error())
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("freebuff", err.Error(), nil),
			items: []activityItem{{tool: "freebuff", label: "freebuff API failed", status: "error", detail: err.Error()}},
		}, run: run}
	}

	elapsed := time.Since(startTime)
	respText := strings.TrimSpace(response)

	if respText == "" {
		wf.phaseError("empty response")
		return agentResultMsg{reply: mockReply{
			text:  "Freebuff returned an empty response.",
			items: []activityItem{{tool: "freebuff", label: "empty response", status: "error", detail: "no output"}},
		}, run: run}
	}

	// Phase 3: receiving response
	wf.phaseReceiving()

	attr := fmt.Sprintf("\n\n  ⚡ freebuff/%s · %.1fs", selectedMod, elapsed.Seconds())
	respText += attr

	// Phase 4: done
	wf.phaseDone(elapsed, fmt.Sprintf("%d chars response", len(respText)))

	reply := mockReply{
		text: respText,
		items: []activityItem{{
			tool: "freebuff", label: fmt.Sprintf("freebuff %s", selectedMod), status: "ok",
			detail: fmt.Sprintf("%.1fs response", elapsed.Seconds()),
		}},
	}
	return agentResultMsg{reply: reply, run: run}
}

// codebuffNativeAgentRun executes Codebuff via native API with saved credentials.
// Follows the unified provider workflow: thinking → processing → receiving → done.
func codebuffNativeAgentRun(m Model, q string, traceCh chan<- agentTraceMsg, askCh chan<- agentQuestionMsg, answerCh <-chan string, startTime time.Time, cancel *func(), run int) tea.Msg {
	selectedMod := m.selectedModel
	if selectedMod == "" {
		selectedMod = codebuffNativeModels[0]
	}

	wf := newWorkflow(traceCh, "codebuff", selectedMod, startTime)

	// Phase 1: thinking
	wf.phaseThinking()

	// Load credentials (same as freebuff)
	authToken := loadManicodeCredentials()
	if authToken == "" {
		return agentResultMsg{reply: mockReply{
			text:  "No Codebuff credentials found.\n\nPlease install and login first:\n  npm i -g codebuff\n  codebuff",
			items: []activityItem{{tool: "codebuff", label: "credentials missing", status: "error", detail: "no auth token"}},
		}, run: run}
	}

	// Phase 2: processing
	wf.phaseProcessing()

	// Execute native API call (same backend as freebuff)
	response, err := freebuffNativeChat(authToken, selectedMod, q)
	if err != nil {
		wf.phaseError(err.Error())
		return agentResultMsg{reply: mockReply{
			text:  parseCLIError("codebuff", err.Error(), nil),
			items: []activityItem{{tool: "codebuff", label: "codebuff API failed", status: "error", detail: err.Error()}},
		}, run: run}
	}

	elapsed := time.Since(startTime)
	respText := strings.TrimSpace(response)

	if respText == "" {
		wf.phaseError("empty response")
		return agentResultMsg{reply: mockReply{
			text:  "Codebuff returned an empty response.",
			items: []activityItem{{tool: "codebuff", label: "empty response", status: "error", detail: "no output"}},
		}, run: run}
	}

	// Phase 3: receiving response
	wf.phaseReceiving()

	attr := fmt.Sprintf("\n\n  ⚡ codebuff/%s · %.1fs", selectedMod, elapsed.Seconds())
	respText += attr

	// Phase 4: done
	wf.phaseDone(elapsed, fmt.Sprintf("%d chars response", len(respText)))

	reply := mockReply{
		text: respText,
		items: []activityItem{{
			tool: "codebuff", label: fmt.Sprintf("codebuff %s", selectedMod), status: "ok",
			detail: fmt.Sprintf("%.1fs response", elapsed.Seconds()),
		}},
	}
	return agentResultMsg{reply: reply, run: run}
}

// parseCLIError categorizes raw CLI stderr into clear, actionable user messages
// for rate limits, auth failures, quota limits, and network errors.
// Handles provider-specific errors for Freebuff, Codebuff, OpenCode, etc.
func parseCLIError(providerName, rawErr string, execErr error) string {
	lower := strings.ToLower(rawErr)

	// Provider-specific error messages
	switch {
	case providerName == "freebuff":
		if strings.Contains(lower, "session") && strings.Contains(lower, "limit") {
			return fmt.Sprintf("⚠️ [Freebuff Session Limit]\n\nFreebuff free tier is limited to 6 sessions/hour.\n👉 Solution: Wait ~10 min for session reset, or switch provider via /connect")
		}
		if strings.Contains(lower, "not found") || strings.Contains(lower, "command not found") {
			return fmt.Sprintf("❌ [Freebuff Not Installed]\n\nFreebuff CLI not found.\n👉 Solution: npm install -g freebuff && freebuff")
		}
		if strings.Contains(lower, "session_model_mismatch") {
			return fmt.Sprintf("⚠️ [Freebuff Model Mismatch]\n\nModel unavailable for this session.\n👉 Solution: Auto-fallback to available model")
		}
		if strings.Contains(lower, "unauthorized") || strings.Contains(lower, "401") {
			return fmt.Sprintf("🔑 [Freebuff Auth Error]\n\nInvalid or expired credentials.\n👉 Solution: Run freebuff to re-login, then /connect again")
		}
		if strings.Contains(lower, "rate limit") || strings.Contains(lower, "429") {
			return fmt.Sprintf("⚠️ [Freebuff Rate Limit]\n\nToo many requests.\n👉 Solution: Wait a few minutes before retrying")
		}
	case providerName == "codebuff":
		if strings.Contains(lower, "rate limit") || strings.Contains(lower, "429") {
			return fmt.Sprintf("⚠️ [Codebuff Rate Limit]\n\nServer is rate-limiting requests.\n👉 Solution: Wait a few minutes or use another model via `/models`.")
		}
		if strings.Contains(lower, "session limit") || strings.Contains(lower, "5 sessions") {
			return fmt.Sprintf("⚠️ [Codebuff Session Limit]\n\nFree tier is limited to 5 sessions/hour.\n👉 Solution: Wait for the next session or use another provider via `/connect`.")
		}
	case providerName == "opencode":
		if strings.Contains(lower, "free limit") || strings.Contains(lower, "quota") {
			return fmt.Sprintf("⚠️ [OpenCode Free Limit]\n\nFree tier limit reached.\n👉 Solution: Use another model via `/models` or wait for reset.")
		}
	}

	// Generic error patterns
	switch {
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "429") || strings.Contains(lower, "too many requests"):
		return fmt.Sprintf("⚠️ [%s Rate Limit Exceeded]\n\nServer is experiencing high queue or call limit reached.\n👉 Solution: Use another model via `/models` (e.g., `mimo-v2.5-free` or `ling-3.0-tiny-free`).", providerName)
	case strings.Contains(lower, "unauthorized") || strings.Contains(lower, "401") || strings.Contains(lower, "auth") || strings.Contains(lower, "login") || strings.Contains(lower, "key"):
		return fmt.Sprintf("🔑 [%s Authentication Required]\n\nProvider authentication is invalid or token has expired.\n👉 Solution: Run `/connect` in brocode to refresh your login session.", providerName)
	case strings.Contains(lower, "quota") || strings.Contains(lower, "exceeded") || strings.Contains(lower, "insufficient"):
		return fmt.Sprintf("💳 [%s Quota Exceeded]\n\nProvider daily/monthly quota limit has been reached.\n👉 Solution: Switch to another provider in `/connect` or choose a free model in `/models`.", providerName)
	case strings.Contains(lower, "connect") || strings.Contains(lower, "network") || strings.Contains(lower, "timeout") || strings.Contains(lower, "offline"):
		return fmt.Sprintf("🌐 [%s Network Error]\n\nFailed to connect to provider server. Check your internet connection.\n👉 Solution: Ensure stable internet connection and try again.", providerName)
	default:
		if rawErr != "" {
			return fmt.Sprintf("❌ [%s Execution Error]\n\n%s", providerName, rawErr)
		}
		if execErr != nil {
			return fmt.Sprintf("❌ [%s Execution Error]\n\n%v", providerName, execErr)
		}
		return fmt.Sprintf("❌ [%s Error]\n\nAn unknown error occurred while contacting the provider.", providerName)
	}
}

// waitForTrace returns a tea.Cmd that relays the next status/trace update from
// the agent goroutine. When the channel closes (run finished) it returns nil.
func (m Model) waitForTrace() tea.Cmd {
	ch := m.traceCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		t, ok := <-ch
		if !ok {
			return nil // channel closed, agent finished
		}
		return agentTraceMsg{phase: t.phase, line: t.line}
	}
}

// waitForAsk relays a pending agent question. The goroutine closes askCh when
// the run finishes, which returns nil here and stops the relay.
func (m Model) waitForAsk() tea.Cmd {
	ch := m.askCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		q, ok := <-ch
		if !ok {
			return nil // run finished without asking
		}
		return agentQuestionMsg{prompt: q.prompt, options: q.options}
	}
}
