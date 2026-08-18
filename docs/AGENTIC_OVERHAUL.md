# AGENTIC_OVERHAUL.md — Harness Overhaul: make the loop never dead-end

> **Status**: Active — design recorded from the brocode-vs-opencode comparison
> (Aug 2026), first two phases implemented. Companion to **AGENT_LOOP.md** and
> **DIRECTION.md**; this document records *why* the loop stalls today, what the
> target behavior is, and the phased execution order.
>
> This is **not** feature-parity with OpenCode. It is the enforcement of the
> doctrine BroCode already wrote (evidence-first context, reactive loop, minimal
> tools) — the current harness lets the loop dead-end in a way that defeats the
> doctrine. OpenCode's harness mechanics (guaranteed exploration, native tool
> loop, no bare-thinking replies) are the reference for *what correct execution
> looks like*, not a feature list to copy.

---

## 1. The observed failure (evidence)

The user ran the same task — *"jelasin dan cek projectnya nih mengenai rotation"*
(explain + investigate the rotation feature) — through brocode and opencode with
the **same free model** (`hy3-free` / deepseek-v4-flash-free).

**opencode** (Plan mode): explained the pasted text, then ran a structured
exploration — grep → file reads → two `explore` subagents → clarifying questions
→ a concrete plan with `file:line` findings. Result: a real, evidence-backed
answer.

**brocode**: each follow-up prompt (`buat plan dlu cek code dll`, `gas`) returned
**0 content chunks** and only a "thinking trace" — e.g. the model wrote in its
reasoning *"I must emit native search calls"* and then stopped. No tool call ran,
no search happened, no evidence was gathered. The turn ended on a bare thinking
trace. That is the dead-end this overhaul fixes.

## 2. Root-cause analysis

Three concrete defects in `internal/tui/` cause the stall:

| # | Defect | Evidence (code) | Consequence |
|---|---|---|---|
| D1 | **SSE stream drops native `tool_calls`.** The stream chunk struct only carries `delta.content` / `delta.reasoning_content`, never `delta.tool_calls`. | `agent.go` `zenChatReply` SSE loop (chunk struct ~L733-744) | If the model emits a search/read tool call + reasoning (no content), the tool call is silently discarded. |
| D2 | **Reasoning is promoted to the reply text *before* the non-streaming retry runs**, so the retry (the one path that parses native tool calls via `parseZenResponse`) never fires when the model streamed reasoning only. | `agent.go` — `if text == "" && reasoningText != "" { text = reasoningText }` sits before the `if text == ""` retry | The exact transcript outcome: `0 chunks`, thinking trace shown as the "answer", no tool execution. |
| D3 | **No guaranteed evidence pass.** Exploration depends entirely on the model voluntarily emitting a tool call. The auto-context pass (`context.go` `attachFileContext`) attaches a tree + explicitly referenced files + a naive keyword scan, but skips short continuations (`isTrivialFollowup` — "gas", "lanjut") and never injects BM25/symbol evidence for investigation prompts. | `context.go` step 4 gate | A free model that won't emit tool calls gets no codebase evidence and cannot explore. |

The doctrine already demands the right behavior (`AGENT_LOOP.md` §5: *"find
evidence → relevant symbols/files → LLM reasoning"*, §1 reactive loop). The
harness just doesn't enforce it.

## 3. Target behavior (definition of done)

No prompt-keyword classifier. The harness guarantees these behaviors through
mode/router decisions and observed failures only:

1. **Evidence is gathered for depth-promised tasks.** For plan mode and tasks
   the router scores NORMAL/DEEP, a compact BM25-ranked file list + symbol map
   (paths + matched snippets, bounded ≤ 5 files / ~1.5 KB) is injected into the
   prompt before the model call — whether or not the model ever calls the
   `search` tool.
2. **A native tool call is never lost.** Tool calls in SSE (or the non-streaming
   retry) always become executable `<tool_call>` blocks fed to the tool runner.
3. **The turn never dead-ends on a thinking trace.** If the model replies with
   reasoning and nothing else (no content, no tool call) — in any language, for
   any prompt — brocode runs the evidence pass and re-prompts **once** with a
   `[SYSTEM TOOL RESULT]` it cannot ignore. Bounded: ≤ 3 model calls per user
   prompt (stream + retry + recovery); the recovery call is skipped when no
   evidence matches (no token burn on casual chat).
4. **Tool results always synthesize.** After evidence/tool results, the model is
   instructed to present a final answer, not loop or re-read the same files
   (existing directive 15, now structurally supported).

## 4. Execution phases

| Phase | What | Status |
|---|---|---|
| **P0** | Kill the dead-end: reorder the non-streaming retry before reasoning promotion (fixes D2), add bounded stall-recovery re-prompt with evidence (fixes D3-at-the-end). | ✅ done |
| **P1** | Evidence pass: `shouldRunEvidencePass` + `explorationEvidence` (BM25 index + symbol map, term-matched snippets, 2KB hard cap) wired into `attachFileContext`. No prompt-keyword classifier — triggers are planner mode / router ≥ 4 / observed stall. | ✅ done |
| **P2** | Native `tool_calls` parsed in the SSE stream (accumulated per index, converted via shared `toolCallsToBlocks`) — no non-streaming retry needed for tool-call replies. | ✅ done |
| **P3** | Plan/Build enforcement in code: planner mode narrows `toolsPayload` to search/read/ask (model can't emit write/edit/bash); per-task tool budget (`maxTaskToolLoops`); glob-pattern allow rules (`"git push *"`) in `GateCommand`. | ✅ done |
| **P4** | In-turn parallel tool calls. **Audit finding**: already exists — all tool blocks in one reply run with parallel fan-out in a single background pass; P2 completes it for native calls. Remaining: `glob`/`list` tools + session `todowrite`/`todoread`. | partial |
| **P5** | Environment block (cwd, date, platform) in the system prompt. | ✅ done |

**Explicitly rejected** (per `MATCHA_PROJECT.md`): embeddings/vector search,
LSP servers, full subagent machinery, MCP expansion, ripgrep/Node runtime
dependencies. The fix is harness discipline, not new heavy dependencies.

## 5. Implementation notes (P0 + P1)

Design rule that survived review: **no prompt-keyword classifier.** Enterprise
agents (Claude Code's "grep in a loop", opencode's explore subagent) let the
model drive search; the harness keeps the loop alive. The trigger for the
evidence pass is either a mode/router decision or an observed failure — never
a hardcoded list of Indonesian/English words (the first draft's word lists
were removed as fragile and non-enterprise).

- `context.go`:
  - `shouldRunEvidencePass(q, plannerMode)` — plan mode, or
    `agentic.EvaluateComplexity ≥ 4`. Tool-result turns always excluded.
  - `evidenceQuery(q)` — raw prompt's tokens ≥ 3 chars; stopword suppression
    is left to BM25's IDF (statistical, language-agnostic) instead of lists.
  - `explorationEvidence(q, seen)` — cached BM25 `fileSearch` (top 5), a
    term-matched snippet per file (first line in the first 100 containing a
    query term, else the index snippet), plus `search.FormatSymbolSummary`
    capped to the top 2 files. Whole block < ~1.5 KB.
  - `attachFileContext(q, plannerMode)` — new param; evidence block injected
    after the keyword scan when `shouldRunEvidencePass` is true. Only caller:
    `agent.go` `agentWorkCmd`.
- `agent.go` `zenChatReply`:
  - Move the non-streaming retry **before** `text = reasoningText` promotion
    (fixes the dropped-tool-call dead-end: the retry parses native tool_calls).
  - Stall recovery fires for **any** reasoning-only reply (no content, no tool
    call) — the trigger is the observed stall itself, so "gas" in any language
    is covered. `explorationEvidence` on the raw prompt; one final non-streaming
    call with the evidence appended as a `[SYSTEM TOOL RESULT]` user message,
    skipped when no evidence matches (no token burn on casual prompts).
- Tests: `internal/tui/context_test.go` — `shouldRunEvidencePass`,
  `evidenceQuery`, `explorationEvidence` boundedness/content/empty-match,
  `stripEnrichedPrompt`, and the full stall-recovery flow in `zenChatReply`
  over an `httptest` server (proves the ≤ 3-call bound: stream + retry +
  recovery).

## 6. Acceptance criteria

1. `go build ./...`, `go vet ./...`, `go test ./...` all green (P7).
2. A prompt that makes the model stall (reasoning-only reply) — e.g. `gas`
   after a plan, in any language — never renders a bare thinking trace as the
   final answer: the stall recovery injects evidence and re-prompts once.
3. A model that emits a native `search` tool call (with reasoning, no content)
   over the stream still executes the search (via the non-streaming retry).
4. Evidence injection is bounded: ≤ 5 files, snippets only, single re-prompt
   cap (P1/P4 token discipline).
