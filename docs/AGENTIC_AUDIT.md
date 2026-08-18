# AGENTIC_AUDIT.md — Full Audit of BroCode's Agentic Layer

> **Status**: Complete (Aug 2026). Full read of the agentic layer —
> `internal/tui/{agent,builder,context,update,render,mock,session,compact}.go`,
> `internal/agentic/*`, `internal/search/*` — benchmarked against Claude Code
> (Agent SDK), OpenCode, and the shared 5-layer harness blueprint.
>
> This is the evidence base for the overhaul in **AGENTIC_OVERHAUL.md**. Every
> finding is a verdict: keep / fix / build. Nothing here is decorative.

---

## 1. How the loop actually works today (verified)

```
User prompt
  → send() appends chatMsg(roleUser), resets toolLoop budget
  → agentWorkCmd (goroutine):
       q = attachFileContext(q, plannerMode)      // tree + attachments + evidence pass
       zenChatReply (SSE stream)                   // reasoning → content
         · stream empty → non-streaming retry      // parses native tool_calls
         · still reasoning-only → stall recovery   // evidence + 1 re-prompt (≤3 calls)
  → agentResultMsg → UI reveals text, interleaved edit/tool cards
  → tool blocks parsed (bash fences, <tool_call>, SEARCH/REPLACE, cat >)
  → risky commands → permission popover (allow once / always / deny)
  → runAgenticToolsCmdDeny (background, parallel fan-out)
  → agentToolResultMsg → "[SYSTEM TOOL RESULT]" prepended to queue
  → drainQueue → send() again (loop continues, ≤ maxToolLoops=8 per user turn)
```

It **is** a ReAct loop — but every tool round-trip passes through the TUI event
loop (each turn is a full model call). Strengths: bounded (maxToolLoops +
repetition guard), permission gates, interleaved live tool cards, stall
recovery. The loop is sound; the gaps are in tool fidelity and context depth.

---

## 2. Findings by layer

### 2.1 The Agent loop — `agent.go`, `update.go`, `builder.go`

| # | Verdict | Finding |
|---|---|---|
| A1 | ✅ keep | Reactive loop + adaptive router (Fast/Normal/Deep) matches doctrine (AGENT_LOOP §1-2). |
| A2 | ✅ keep | Bounded iteration: `maxToolLoops=8` per user turn + `toolRepeat`/`toolPrevCmds` repetition guard (update.go:217, app.go:58). |
| A3 | ✅ keep | Stall recovery + evidence pass (P0/P1, landed): reasoning-only replies never dead-end; ≤3 model calls. |
| A4 | ⚠️ fix | **SSE stream drops native `tool_calls`** (chunk struct has no ToolCalls field, agent.go SSE loop). Today recovered via the non-streaming retry — works, but burns a full round-trip and breaks the model's native tool protocol. → P2: parse `delta.tool_calls` in-stream. |
| A5 | ⚠️ fix | Tool feedback round-trips through the TUI event queue per turn; a 6-tool task = 6 full model calls + UI hops. Fine at this scale, but parallel-tool turns (opencode/Claude batch independent calls in ONE turn) would halve latency. → P4/P6. |
| A6 | 🔧 build | No global turn budget (`steps`) — only per-user-turn `maxToolLoops`. A long autonomous run ("refactor auth") can chain 8 rounds/user turn across many turns. Claude/openCode cap by turns AND cost. → P3: per-task budget. |

### 2.2 The Harness

**Queue + priority** (update.go): ✅ keep — tool feedback prepended to queue front = the "priority gate" pattern.

**Permissions** (`agentic/permission.go`, update.go popover): ✅ strong.
- `rm -rf /`-class hard-deny (never overridable) — better than most.
- Session allow-list ("always allow"), cd-escape gate, force-push gate, deny fed back to the model.
- ⚠️ Gap vs enterprise: gating is **first-word based**, not pattern-based (`Bash(git push)` glob). No per-tool permission config (read/grep/list vs write/edit). → P3 partial.

**Context engine** (`context.go`): ✅ strong core.
- Tree + file attachments (mtime-validated, session-seen cache), AGENTS.md cache, BM25 index + symbols, evidence pass (no classifier), P4 token discipline.
- ⚠️ `searchProjectFiles` (naive keyword count walk, ≤300 files, reads every file) duplicates what the cached BM25 index already does better — legacy path, kept for full-content attach. Acceptable but slow on big repos; keep both is confusing. → P6: unify on the index.

**Skills / MCP**: 🔧 — `.agents/skills` convention documented but no skill-loading in the loop; MCP filter layer exists (lazy catalog→inspect→execute) but no servers wired. Per DIRECTION.md this is intentionally deferred. OK.

**Memory** (`session.go`, `compact.go`): ✅ per-project JSONL sessions (retention-capped), compaction L0-L3 (pinned goal / verbatim tail / ledger / offload). No long-term MEMORY.md-style memory — by design (doctrine: bounded RAM). OK.

### 2.3 The Runtime

✅ Single static binary, CGO-free, session persistence, cancellable contexts (ESC → context cancel). No durable execution — correct for a TUI.

### 2.4 Presentation

✅ Strong: streaming reveal with adaptive chunking, interleaved ⚙ edit/tool cards at emission point, ask popover (multi-question) + permission popover, mouse wheel/drag-select, web dashboard (`internal/web`). This is ahead of most harnesses.

### 2.5 Observability

✅ Good: phase trace (thinking→reasoning→writing→done), tool activity panel, real token usage from API settlement (P3), collapsible thinking traces + diffs. 🔧 No request logging/tracing to disk — fine for now.

---

## 3. Confirmed bugs & dead code (fix list)

| # | Severity | Finding | Evidence |
|---|---|---|---|
| B1 | 🐛 **bug** | **`.bro_bak` litter**: every L2+ file write calls `agentic.Snapshot` but `Restore`/`Cleanup` have **zero callers** — backups accumulate forever and rollback (DIRECTION §4.5) is impossible. | builder.go:63-67, agentic/snapshot.go |
| B2 | 🐛 bug | **`agentic.WebSearch` is a dead stub** — never in `toolsPayload()`, returns a static string. Model is told "search web" nowhere; harmless but misleading if ever exposed. | agentic/tools.go:84 |
| B3 | 🪦 dead | **`search.BuildRepoMap`** (AST PageRank repo map) only runs in tests — the runtime never attaches it. The doctrine's "repository map (find better, not read more)" is unimplemented. | symbols.go:130, symbols_test.go |
| B4 | ⚠️ risk | **SSE tool_calls dropped** (A4) — mitigated, not fixed. Free models that emit tool calls + reasoning over the stream currently rely on the retry path. | agent.go |
| B5 | ⚠️ risk | **`RunCommandNative` has no context cancellation** — ESC drops the result but the bash process keeps running (side effects land after abort). Acknowledged in a comment, but a long `sleep 999`-style command holds resources until timeout. | update.go:2106 comment, agentic/tools.go |
| B6 | 🔧 gap | **`risk.go` L0-L3 only gates snapshots** — the tier never changes the workflow (no extra verify/review for L3 as DIRECTION §4.2 promises). | builder.go:63 |

---

## 4. Gap analysis vs best practice (what to steal, what to reject)

| Capability | Claude Code | OpenCode | BroCode today | Verdict |
|---|---|---|---|---|
| ReAct loop | ✅ | ✅ | ✅ (event-loop round trips) | keep, add in-turn batching (A5) |
| Native tool_calls in stream | ✅ | ✅ | ⚠️ via retry only | **fix (P2)** |
| Turn/step budget | `max_turns`, `max_budget_usd` | `steps` | `maxToolLoops`/user turn | add per-task budget (P3) |
| Plan mode enforcement | permission_mode=plan | tool deny | bash blocked + edits blocked in code; prompt only | **verify + harden (P3)** |
| Permission rules | `Bash(pattern)` globs, per-tool allow/deny | same | first-word gates only | extend (P3) |
| Explore subagent | Explore (read-only, cheap model) | explore subagent | none (evidence pass instead) | optional (P7) |
| Search | Grep (live) | Grep + LSP | BM25 index + symbols | keep (fast, bounded) |
| TODO tracking | TaskCreate/Update | todowrite/read | `.agents/plan/current.md` auto-save | keep + consider todos |
| Environment block (cwd/date/platform) | ✅ | ✅ | cwd only in persona | add (P5) |
| Skills loading | ✅ | ✅ | convention only | defer (doctrine) |
| Sandbox | optional | no | no (direct, timeout) | defer (doctrine) |

**Reject** (per MATCHA_PROJECT.md): embeddings, LSP, MCP servers, ripgrep/Node
deps, durable runtime, subagent swarm. The moat is context + loop discipline,
not more services.

---

## 5. Prioritized overhaul roadmap

| # | Item | Fixes | Effort | Deps |
|---|---|---|---|---|
| **P0** | Kill the dead-end (retry reorder + stall recovery + evidence pass, no classifier) | A3, D2/D3 | ✅ **done** | — |
| **P1** | Evidence pass (BM25 + symbols, bounded) | D3 | ✅ **done** | P0 |
| **P2** | Parse `delta.tool_calls` in SSE; execute native tool calls without the retry detour | B4, A4 | ✅ done | P0 |
| **P3** | Plan/Build enforcement in code (deny write/edit/mutating bash per tool) + per-task turn budget + `Bash(pattern)` permission rules | A6, B6 | ✅ done | P2 |
| **P4** | In-turn parallel tool calls (batch independent read/search calls in one model turn, sequential for mutating) | A5 | ⚠️ already-implemented (parallel fan-out) | P2 |
| **P5** | Prompt/harness polish: environment block (cwd/date/platform), split system prompt into stable prefix + per-request metadata, "act don't narrate" | cache stability (P3 doctrine) | ✅ done | — |
| **P6** | Wire `BuildRepoMap` as the lazy repo map for DeepPath/planner (cached per project) | B3 | ✅ done | P1 |
| **P7** | Fix snapshot rollback: one-turn `.bro_bak` lifecycle + `CleanupStaleSnapshots` at next user prompt; removed dead `WebSearch` stub | B1, B2 | ✅ done | — |
| **P8** | Read-only Explore subagent: bounded nested loop (≤6 rounds), read-only surface (search/read), condensed report back to the main agent | opencode-parity exploration | ✅ done | P2 |

**Delivered in the overhaul pass: P2, P3, P5, P6, P7, P8** (P0/P1 earlier; P4 found
already-implemented — all tool blocks in one reply already run with parallel
fan-out in a single background pass). Remaining: todo tools and risk-tier
workflow depth (B6).

---

## 6. Bottom line

BroCode's loop is already closer to the enterprise blueprint than the
transcript suggests: it has a bounded ReAct loop, a real permission gate
(better than opencode's default in one respect: hard-deny on `rm -rf /`),
mtime-validated context caching, interleaved tool UX, and now a no-dead-end
guarantee. The **two real defects** are (a) native tool calls not parsed in
the stream and (b) snapshot/rollback is half-built. The rest of the overhaul
is latency and depth, not correctness.
