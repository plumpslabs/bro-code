# PHILOSOPHY.md

> Design constitution for [project name]. This is a mandatory reference every
> time a feature is added — if a decision conflicts with a principle here,
> the principle wins, not short-term implementation convenience.

## Why this document exists

Research into Claude Code, opencode, Roo Code, Cline, and Google Antigravity
(GitHub issues, forums, blogs, official postmortems) shows five failure
patterns that **repeat across every tool**, regardless of language/stack.
That means these are not problems "already solved by others" — they are
genuinely unsolved at the industry level. This project exists to attack
those five patterns with discipline, not to add new features on top of the
same architecture, plus two principles born from follow-up research
(security of external content & testing boundedness) — **seven principles in
total**.

## Identity & Differentiation (read this first)

This started as a personal complaint that turned out to be a mass complaint:
today's AI coding tools are **slow, RAM-hungry, heavy** — they make laptops
spin fans, drain batteries, and sometimes require restarts. Reddit/Hacker
News is full of reports: sessions that keep eating RAM while idle (tens of
MB per minute), WSL jumping from 2GB to 10GB+ just from indexing + MCP
servers, React-based TUIs freezing on long output ("a demo tool built for
engineers with 128GB RAM Apple Silicon that actually runs on standard
hardware").

We are not trying to build "yet another agent". Our differentiation is three
things, and all of them **must be provable with numbers**:

1. **Transparent RAM & resource usage, not a black box.** Users deserve to
   know this process's RSS, how many tokens go in/out/cache, how much context
   is used — real-time, in the terminal, and exportable. If someone asks
   "what does this tool actually consume?", the answer should be visible on
   screen, not found by asking a forum.
2. **Efficient by architecture, not by patching.** Our baseline footprint
   should sit close to the leanest runtime available, not "400MB of Node.js
   is normal". If a component can be lazy, make it lazy. If a buffer can be
   bounded, bound it from birth. This is the fast-tools culture: people pick
   ripgrep over grep, fd over find, fzf, lazygit — not for features, but
   because **speed changes how they work**. Speed is a feature, not a
   last-minute optimization.
3. **A performance budget enforced in CI.** The web culture has "bundle <
   100KB, FCP < 1.8s" enforced by CI before shipping. We apply the same to
   an agent: startup < 200ms warm, idle RSS < 80MB, and a CI gate that
   fails a PR that blows the budget. If it is not tested, it is just a
   prayer.

Philosophy reminders: **suckless** (complexity is the mother of bloat and
bugs), **Unix philosophy** (do one thing well), and **"worse is better"**
(simplicity beats completeness — a simple tool that ships early beats a
"perfect" tool that arrives late and heavy).

**North star metric: productive token ratio** = tokens doing real work /
total tokens sent to the API. 2026 research shows mainstream tools carry a
**~70% token tax** (system prompt, MCP schemas, history re-tokenization) —
only ~27% is productive. Every new feature must answer: does this raise the
ratio or lower it? **Guard so this metric cannot be gamed**: the ratio is
computed from **settlement** (official API numbers) over the prompt that was
**actually sent**, not after eviction — if compaction discards tool output
(Principle 4), the discarded tokens must still count as cost, so the number
stays honest and users keep trusting it (Principle 3).

### Performance budget (mandatory numbers, not aspirational targets)

| Dimension | Budget | Consequence if exceeded |
|---|---|---|
| Startup (warm) | < 200ms | Startup scan reads metadata only, full content lazy (Principle 2) |
| Startup (cold) | < 1s | Do not eagerly load anything unused in the first turn |
| Idle RSS | **< 80MB** | Live structures must have eviction (Principle 1) |
| Local action (parse, AST, diff) | < 500ms | Do not re-render the whole TUI; render incrementally |
| TUI frame render (streaming) | **< 16ms/frame (~60fps)** | Batch messages ~30–60fps, virtualized viewport, no full redraws |
| Idle RAM growth | 0 (flatline) | A rising graph while idle = bug (soak test, Principle 7) |
| Tokens per task | configurable, enforced pre-request | Principle 4, including the parallel fan-out budget |

Architecture note: the runtime choice (Go/Rust vs Node/Bun) is a
**first-class decision**, not an assumption. Baseline RSS of an empty
process: Rust ~1–3MB, Go ~5–10MB, Bun ~10–15MB, Node ~30–40MB. If idle
footprint must stay under 80MB with margin, count the runtime from day
one. Interop must still work (MCP, AGENTS.md, etc.) — that is a protocol
matter, not a reason to pile on a heavy runtime.

> **Baseline verified (Aug 2026)**: binary 8.7MB stripped (`-s -w`) on the
> Bubble Tea v2 stack + session persistence + BM25/diff/tokens, idle RSS ~5MB
> (pre-full-render sample — may rise slightly once a full frame renders),
> startup ~instant (below tool resolution). The budget
> is not aspirational — it is measured. The TUI stack
> (bubbletea+bubbles+lipgloss, ~30 transitive modules) is the dominant
> dependency cost and still lands far inside the budget; the real bloat
> risks are future additions (embeddings, heavy MCP clients,
> reflection-heavy libs), which is why every one of them is gated here.
>
> **Early-warning gate**: binary > 10MB or idle RSS > 50MB → the change
> must carry a written rationale in TECH_STACK.md before merging (the hard
> budget is 20MB/80MB; this gate triggers the conversation much earlier).

## Seven Non-Negotiable Principles

### 1. Bounded by default, not unbounded then capped later
Every buffer — tool output, cache, session log, diagnostics map — has a hard
limit defined at its point of creation, not patched in after OOM reports.
opencode got hit hardest here: bash output piling up without limit, RPC
pending map without timeout, LSP diagnostics map that only grows and never
shrinks — resulting in 10GB+ RAM reports in long sessions, up to 70GB in
extreme cases (real root causes: 5 different subsystems, plus Bun SQLite
memory-mapping the entire DB file into the address space).

The scope of these limits **includes**:
- **Output budget**: reserve output/max_tokens in every request (Codex CLI
  reserves 128k output + 5% headroom; Roo Code once had to clamp max_tokens
  after the model kept "condensing"). Our philosophy previously only
  managed input buffers — that is half the story.
- **Aggregate fan-out budget**: parallel subagents multiply global cost. A
  per-buffer limit is not enough — there must be a total per-task budget
  covering every agent running in parallel.

**Rule of thumb**: for every `append`/`map[key]=value` into a structure that
lives for the process lifetime, ask "when does this get evicted?" before
writing it. If there is no answer, it is a bug that has not happened yet.

### 2. Lazy + relevance-scored loading, not eager dumps of all definitions
Skills, MCP tools, and agent definitions load into context **only if
relevant** to the current task. MCP ecosystem research shows one tool
definition is ~1000 tokens and a large server can cost 40k+ tokens just for
schemas — before a single user message is sent. SEP-1576 (official MCP
proposal) even proposes embedding-based tool matching at the protocol level
because the problem is that severe.

Two things must be understood for this principle to work:

- **The bootstrap problem**: how do you know something is relevant before
  reading its content? The answer exists in the ecosystem and is proven:
  **progressive discovery** (catalog → inspect → execute). Startup only
  reads metadata (name + one-line description). The model asks through a
  `search_tools` meta-tool, the host returns concise matches, and only then
  is the full schema injected when the tool is actually about to be called.
  Use BM25/embedding for retrieval — and **the retrieval index itself must
  be bounded** (an index over a huge repo is memory too).
- **MCP spec 2026-07-28** now provides `ttlMs` + `cacheScope` on
  `tools/list` — clients may cache the catalog. Use it; do not refresh the
  catalog every turn.

**Rule of thumb**: if the number of registered tools/skills exceeds ~15,
relevance filtering must happen before anything enters the system prompt —
not exposing all names continuously.

### 3. Tracking = source of truth, and it must be cache-aware
Token/RAM/cost numbers shown to the user must match exactly what is sent to
the API. Roo Code once displayed context usage of 170k when the actual was
44k because cached tokens were double-counted. Claude Code once had a
"thinking cache" bug that inflated tokens 10–20x and drained Max plan
quotas. Once users stop trusting the numbers, the entire tracking feature
loses its purpose.

**Resolve a tension that was previously unaddressed**: this principle once
conflicted with "model-agnostic" — because each provider's tokenizer
differs (tiktoken is open-source; Anthropic/Google are closed), how can
local numbers "match exactly"? The answer: **separate two concepts**.
- **Forecast** = local estimate *before* the request (for budgeting,
  Principle 4). It may be ~96% accurate and may use heuristics — but it
  must be calibrated per provider and labeled as an estimate.
- **Settlement** = the official numbers from the API response after the
  request. This is the only source of truth for billing/final display.

And most importantly: **token ≠ cost**. Prompt caching economics: on
Anthropic cache writes cost a +25% surcharge and cache reads are −90%;
OpenAI auto-caches at 50% off; Gemini charges per token-hour. One changed
byte in the prefix = total cache miss. **Adding an MCP tool mid-session
invalidates the entire prefix** → expensive re-injection. The rules:
- Track cache reads/writes separately, not as one number.
- Keep the prefix stable; batch tool/skill changes instead of adding them
  one by one mid-session.
- Show an input / cached-input / output breakdown — this is what makes users
  trust (and see) that caching actually works.

**Transparency must be real-time & exportable**: the statusline pattern
(`context_window.used_percentage`, RSS, quota) read by users via
scripts/JSON files is the de facto ecosystem standard (Claude Code ships a
per-turn JSON payload). We provide it from the first version, not later.

**North star**: productive token ratio is measured continuously and shown.

**Rule of thumb**: one source of truth for token counts (directly from the
API response, not a separate local estimate). Every discrepancy between
local numbers and provider numbers is a high-priority bug, not cosmetic.
Every feature that touches the prompt must be checked for its impact on the
cache prefix.

### 4. Preventive, not reactive — and compaction is lossy
Budget checks happen **before** the request is sent, not compacting after
the context is already full. Cline deliberately did not limit context
("don't-limit-context" philosophy) and became the most wasteful in its
class (~300k tokens in 5 iterations). Roo Code once had to clamp max_tokens
because its threshold was misconfigured from the start.

But research adds a lesson that was not here before: **compaction is a
destructive operation, not a free one — and aggressive compaction is a
self-bomb.** Claude Code's auto-compact fires at ~83.5% of the window and
deletes user constraints ("no parallel subagents"), debugging hypotheses,
path-scoped rules — derailing sessions; power users disable it and run
manual `/compact` with `KEEP:` directives. The evidence is now empirical,
not anecdotal: research on context compaction shows repeated summarization
silently erases user constraints — constraint-violation rates jump from 0%
(full context) to **30–59% after compaction** ("Governance Decay",
arXiv:2606.22528). And aggressive *early* compaction is counterproductive:
summarizers self-bound their output length regardless of input size, so
compressing fresh tactical data early destroys multi-step reasoning
continuity without saving proportionally more tokens. Plus there is
**context rot**: reasoning degrades as context fills even before the limit —
models remember the start and end but drown in the middle
(lost-in-the-middle). The goal is not just "never overflow" but "keep
context dense & relevant" — and **never compact when you can avoid it**.

**The primary strategy is to avoid needing compaction at all**: progressive
disclosure (Principle 2), lazy loading, and external offload (Principle 5)
keep the window dense. When compaction is genuinely unavoidable, use the
tiered model:

- **L0 — Pinned core (never compacted)**: goal, user constraints, modified
  files, architecture decisions, KEEP-directives. Immutable across
  compaction boundaries — the direct fix for the 30–59% constraint-loss
  finding.
- **L1 — Verbatim tail**: a token-budget share of the window (e.g., the
  last ~20–30%) stays word-for-word. Fresh, still-tactical data is never
  summarized.
- **L2 — Structured summary head**: older turns become a *structured state
  ledger* (active hypotheses, completed subtasks, key facts, decisions) as
  JSON/Markdown blocks — not lossy prose — and are summarized
  *hierarchically* (blocks merged over time), never as one monolithic
  re-summarization of the whole window.
- **L3 — External offload**: full details go to the persistent store
  (Principle 5), retrievable on demand, so compaction is *recoverable* —
  nothing is truly lost. Offload is **session-scoped with explicit
  retention** (pruned at session end, or N days after — same rule as every
  other stateful store, Principle 5): recoverable does not mean forever.

**Guards against the compaction bomb**:
- Compaction is *deferred*, not eager — fire when near the threshold or on
  user request; prune low-signal noise first (repeated tool outputs,
  dead-end attempts) before touching signal. **Compaction invalidates the
  cache prefix (Principle 3)** — the re-injection cost is part of the
  when-to-compact decision, and it must happen in one batch, never
  repeatedly.
- User constraints and the goal are automatically pinned to L0 — never
  eligible for summarization.
- Every compaction produces a **compaction report** (what stayed verbatim,
  what was summarized, what was offloaded, quality estimate) — visible to
  the user, expandable from L3. Never compact silently.
- Trigger compaction based on **quality & content**, not just percentage —
  fire **well before** the hard limit (never wait for 80%+), but never so
  early that fresh tactical data is destroyed.
- **Measure compaction quality, not just token savings**: every compaction
  records tokens saved AND a quality proxy — a constraint-survival test
  (re-inject the session's constraints into the summary and verify they
  survive) plus task-success comparison on dev sessions. A compaction that
  saves tokens but drops the quality score is a bug: efficiency without
  quality is the bomb. Plot the token-vs-quality tradeoff over real
  sessions; if the curve degrades, the policy is too aggressive — pull the
  trigger back.

**Rule of thumb**: every turn, estimate the candidate prompt's tokens before
sending. If past the threshold, trigger compaction/trim **before** the API
call — not waiting for an error or until the context is full.

### 5. Explicit TTL/cleanup in every stateful store
SQLite usage tables, session caches — all have a retention policy defined in
the initial schema, not "added later if someone complains". opencode's
SQLite database bloated to ~2GB in a 2-day session because there was no
auto-cleanup from the start (plus `auto_vacuum=OFF` leaving unreclaimed
free pages, and mmap pulling the whole DB file into RAM).

SQLite lessons that must be defaults:
- `PRAGMA auto_vacuum=ON` (or scheduled vacuum) + regular WAL checkpoints.
- `PRAGMA mmap_size=0` / bounded page cache — never let a large DB map into
  the address space.
- `created_at` column required; explicit default retention (auto-prune after
  N days, or archive-and-compress).

Boundary with Principle 1: **Principle 1 = in-memory state** (when does it
get evicted?), **Principle 5 = persistent state** (how long is it kept?).
Do not mix them.

This also applies to **cross-session memory** (if we have it — and we
should, because it is a long-term differentiator): the MemGPT pattern (core
= always in the prompt, recall = log searched on demand, archival = DB) is a
proven blueprint. A memory DB has its own leak risk — so its retention
policy must also be born with the schema. Principle: every table that writes
a row per event (usage, session, tool_calls, memory) must have `created_at`
+ an explicit retention policy defined with the schema, not afterwards.

### 6. External content = data, not instructions
Web fetches, MCP server output, and read file contents are all **untrusted**.
They can contain prompt injection (hidden instructions trying to steer the
agent) and can be bloated to DoS our context window (send mega-outputs to
fill context and burn quota).

The rules:
- External content is treated as **data**: never executed as instructions,
  never entering context without a label of its origin.
- **Explicit size caps** per external source (file, web, tool output) —
  relevance is never a reason to pile on unlimited content.
- Large tool outputs: do not pass them raw into context; summarize or store
  to a file and reference the path.

### 7. Boundedness is tested, not assumed
Prevention without a detection layer is a prayer. opencode only truly
stabilized (after 14–70GB reports) when: CI RSS regression gates, on-demand
heap snapshots, and tightened cache limits were added. Lesson: **memory
regressions must fail CI**, not be discovered from user reports.

The rules:
- Every PR touching the main loop / buffers / stateful stores must pass a
  **soak test**: run a synthetic workload (many turns, lots of tool output,
  long sessions), check RSS stays flatline and does not rise while idle.
- CI has an **RSS gate** + startup-time gate from the performance budget
  above.
- Provide **on-demand heap snapshot / memory dump** (env flag or command)
  for debugging leaks in the field — do not make users install profilers.
- **Measure, don't assume**: every PR that changes binary/deps must report
  the binary size + idle RSS + startup delta. Test-only deps that never
  enter the binary (`go mod why` → "main module does not need package")
  are acceptable; deps that DO enter the binary must justify themselves.
  The TUI leak rule: every background `tea.Cmd`/ticker goroutine is bound
  to a cancellable `context.Context`.

## What is preserved from the existing ecosystem (do not touch)

- **File & folder conventions**: `AGENTS.md`, `.agents/skills/<name>/SKILL.md`,
  `.agents/agent/<name>.md` with standard frontmatter — so skills/agents
  written for opencode/Claude Code run unmodified.
- **MCP client + server support** — cross-tool interoperability is a
  requirement, not a nice-to-have (use progressive discovery so it does not
  carry MCP token tax).
- **Model-agnostic** — easy provider swap, no vendor lock-in. Meaning: the
  interface is the same, but per-provider adapters exist for tokenizer,
  cache semantics, and pricing (see Principle 3).
- **Terminal-native, headless-capable** — for CI/automation, not just
  interactive. Headless mode must share the same pipeline, not a duplicated
  code path.

## Checklist before adding any feature

Before implementing, answer these questions:

1. Does this feature **reduce tokens per task** or **raise correctness per
   token**? If neither — cosmetic polish, lower priority. (And: does it
   raise the productive token ratio?)
2. Does this feature add state that lives for the process lifetime? Where is
   its limit and when is it evicted? (Principle 1). If persistent: what is
   the retention policy, from day one? (Principle 5)
3. Does this feature dump something into context without relevance
   filtering? (Principle 2) — and does it touch the prompt prefix and wreck
   the cache? (Principle 3)
4. If this feature shows numbers to the user (tokens, RAM, cost) — where do
   they come from? Forecast or settlement? Are cache reads/writes counted
   separately? And is the number **visible real-time & exportable**, not
   just in a log? (Principle 3)
5. Does this feature react after a problem occurs, or prevent it before?
   If it evicts/compacts — **what is lost, and can the user see it?**
   (Principle 4)
6. If this feature reads/receives external content — is there a size cap? Is
   the content treated as data? (Principle 6)
7. How is this feature tested so it does not break the performance budget
   (startup < 200ms, idle RSS < 80MB, flatline while idle)? Is there a
   soak test / CI RSS gate that catches it leaking? (Principle 7)

If any answer makes you hesitate, that is a signal to redesign before code
is written — not a reason to ship first and fix later. That pattern is
exactly what left the five tools from the research above with issue trackers
full of uncontrolled RAM/token reports.

## Research references

**Tools & incidents (verified 2026):**
- opencode: unbounded memory growth (5+ subsystems: bash output, Levenshtein
  diff, FileTime tracking, RPC pending map, LSP diagnostics) + Bun SQLite
  mmap bloat up to ~2GB. Fixes: `mmap_size=0`, ring buffer, LRU, TTL, CI RSS
  monitoring, heap snapshot flag. Stabilized at ~700MB–1GB afterwards.
- Claude Code: 3 separate product regressions (default effort lowered Mar 4,
  thinking-cache bug Mar 26 inflating tokens 10–20x, verbosity prompt Apr
  16) + official postmortem Apr 23 + usage reset. Auto-compact at ~83.5%
  proven to destroy working context; users moved to manual `/compact` +
  KEEP.
- Roo Code: eager tool-description loading, token-counting bug (cached
  tokens counted), sunset announced Apr 20 2026, shutdown May 15 2026.
- Cline: "don't-limit-context" philosophy → most wasteful (~300k tokens/5
  iterations), hit by metered billing, responded with spend-limit UI.
- Google Antigravity: full VS Code fork, restart as the official workaround.
- MCP ecosystem-wide: tool definition ~1000 tokens, large server 40k+ tokens
  just for schemas. SEP-1576 (dedup `$ref`, adaptive optional fields,
  embedding matching), spec 2026-07-28 (stateless, `tools/list` TTL +
  cacheScope, progressive discovery & code mode as client best practice).

**Economics & UX:**
- Token tax ~70%: of 100 tokens sent, only ~27% is productive work (2026
  session proxy analysis).
- Prompt caching: Anthropic cache write +25% / read −90%; OpenAI auto 50%;
  Gemini per-token-hour. One changed byte in the prefix = total miss.
- A bloated CLAUDE.md can eat 42k tokens per conversation before any work
  starts.
- User complaints: idle RAM creep, WSL 2GB→10GB from MCP/indexing,
  React-in-terminal TUI freezes, battery drain — HN thread "Claude Code
  dumbing down", r/ClaudeAI, daily.dev "RAM spiking".
- Transparency: statusline + JSON payload pattern
  (context_window.used_percentage, quota) parsed by users — the de facto
  standard we must provide.

**Foundations & philosophy:**
- MemGPT / Letta (arXiv:2310.08560) — core/recall/archival memory tiers.
- Anthropic "Effective Context Engineering" — progressive disclosure,
  compaction, note-taking.
- Lost in the Middle (arXiv:2307.03172) — long context ≠ quality.
- Performance budget (web) → we adopt it for the agent CLI.
- Suckless.org; Unix philosophy; "Worse is Better" (R. Gabriel).
- Fast-tools culture: ripgrep/fd/bat/fzf/lazygit — speed as a feature.
- Aider repo map (tree-sitter + PageRank, default 1000 tokens) — proof that
  token efficiency is possible and a differentiator.
