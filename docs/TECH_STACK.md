# TECH_STACK.md — Tech Stack Decision & Rationale

> Companion document to **PHILOSOPHY.md**. If PHILOSOPHY.md answers *"why
> this project exists and what its principles are"*, this document answers
> *"what do we use, and why not something else"*. Every choice here must be
> justifiable against the principles in PHILOSOPHY.md — if it is not, it is
> a wrong decision.

## Context & constraints (from PHILOSOPHY.md)

This project exists because of a personal complaint that turned out to be
massive: AI coding tools are **slow, RAM-hungry, heavy**. Constraints that
cannot be negotiated:

1. **Single static binary, CGO-free wherever possible** — easy cross-compile
   for Mac M1 + Linux, easy distribution via GitHub releases.
2. **In-process, no always-on secondary process** — no server/daemon running
   the whole session (the Bun server pattern that made opencode bloat).
   **Short-lived helpers spawned lazily on demand** (e.g., for embeddings,
   §7) do not violate this rule — what is banned is a process that lives for
   the session, not a one-shot process that is born and dies.
3. **Performance budget**: startup < 200ms warm, idle RSS < 80MB, flatline
   while idle, local action < 500ms, TUI frame < 16ms (~60fps).
4. **Mandatory interop**: MCP client/server, `AGENTS.md`, skill conventions —
   but without carrying MCP token tax into context.
5. **North star**: productive token ratio. Every added dependency must answer
   "does this reduce tokens or add load?"

## Final stack summary

| Layer | Choice | Why (short) |
|---|---|---|
| Runtime | **Go 1.25+** | Small footprint, single binary, goroutines for subagents |
| TUI | **Bubble Tea v2** (`charm.land/bubbletea/v2`) + lipgloss v2 + bubbles v2 | De facto standard Go TUI, reactive, mature, delta renderer |
| Diff | **hexops/gotextdiff** | Myers O(ND), used by gopls/VSCode Go |
| Search/relevance | **Hand-rolled BM25** (`internal/search`) | Pure Go, zero dep, ~0MB |
| Storage | **JSONL + compaction** (default) / `ncruces/go-sqlite3` (if querying needed) | 0MB or +2–4MB, CGO-free |
| MCP | **mark3labs/mcp-go** + custom filter layer | Most mature in Go (SSE + stdio) |
| Embeddings | **NONE in MVP** (deferred, see §7) | Local embeddings = 150–350MB RAM → blows the budget |
| Vector search | **NONE in MVP** (deferred) | `chromem-go` only if truly needed later |

---

## 1. Runtime: Go

**Choice**: Go 1.25+ (current as of mid-2026).

**Baseline RSS of an empty process**: Rust ~1–3MB · Go ~5–10MB · Bun ~10–15MB ·
Node ~30–40MB.

**Rationale**:
- Go beats Node/Bun on footprint (Node is 3–4x heavier) and static
  single-binary. The opencode (Bun) lesson is a real warning: a heavy
  runtime + separate process = RAM bloat.
- Go beats Rust on development speed — and the Go TUI/MCP ecosystem is
  mature (Bubble Tea, mark3labs/mcp-go). Rust stays a candidate for extreme
  squeezing later, but not now.
- Goroutines are the natural model for parallel subagents, without a heavy
  process/thread per agent.

**Forbidden anti-pattern**: do not start in Node/Bun/Electron because "it is
more familiar". That is exactly the road to a 400MB+ baseline.

---

## 2. TUI: Bubble Tea

**Choice**: **Bubble Tea v2** (`charm.land/bubbletea/v2`) + Lip Gloss v2
(`charm.land/lipgloss/v2`) for styling + Bubbles v2 (`charm.land/bubbles/v2`;
textinput, viewport, spinner, key). v1 is fully replaced — per the official
[UPGRADE_GUIDE_V2.md](https://github.com/charmbracelet/bubbletea/blob/main/UPGRADE_GUIDE_V2.md):
`View()` now returns `tea.View` (declarative `AltScreen` / `WindowTitle`
fields instead of program options), key events are `tea.KeyPressMsg`, and
synchronized output + delta rendering are automatic.

**Rationale**: the de facto standard Go TUI — used by lazygit, `gh`, and the
Charm ecosystem; Elm architecture (Model-View-Update) fits async streaming
(LLM tokens, tool output). Rejected alternatives: `tview` (widget-based,
less active), `gocui`/`termui` (legacy/maintenance-only).

**Anti-lag rules (the "React-in-terminal" trap users complain about)**:
1. **Batch/coalesce streaming** — never send one `tea.Msg` per token. Buffer
   in a channel, flush with a ticker capped at ~30–60fps.
2. **Virtualized viewport** (`bubbles/viewport`) — render only visible
   lines. Never render a 1000-line diff + coloring in `View()` every frame.
3. **Precompute styling** — syntax highlight/diff coloring computed once per
   chunk, not re-done every render.
4. **Avoid heavy string concat in `View()`** — reuse buffers.
5. **Styles once, at package level** — never re-create
   `lipgloss.NewStyle()` inside `View()` (allocation + GC pressure every
   frame).
6. **Collapsible blocks** — long/tool content (diff hunks, thinking
   traces) collapses by default and expands with `ctrl+o` (Claude Code
   pattern). Bounded rendering: hidden content is never rendered per
   frame; collapse state is per-message display state, not persisted.
7. **Small, precomputed overlays** — modal pickers (`/connect`, `/theme`)
   and the `/` command suggestion popup are *small* centered/anchored
   boxes rendered from precomputed styles; swatch data is built once at
   package load (immutable, like the `Themes` map). The theme picker
   never applies a theme until the user explicitly confirms — no
   accidental cycling, and applying swaps the whole precomputed style set
   in one call (`setTheme`).
8. **No focus machine; scrolling is always on** — there is no tab-focus
   cycle to get lost in: the input is always the typing surface, and the
   chat scrolls via `↑↓`/`pgup`/`pgdown` **and the mouse wheel**
   (`v.MouseMode = tea.MouseModeCellMotion` + `MouseWheelMsg`, both
   declarative in v2) no matter where you "are". Text keys `j`/`k` are
   never hijacked — they stay typing characters.
9. **Right panel = transparency dashboard, width-budgeted** — the status
   panel shows model + context window + live token estimate (header too),
   git branch + repo-relative path (found by walking up from cwd,
   worktree-aware), MCP filter + servers, sub-agents, and bounded
   activity. Every row is clipped to the panel content width so no line
   can ever overflow the border (Principle 1). Real values replace the
   estimates when the provider/MCP layers land (Principle 3: numbers come
   from settlement, not guesses).

**v2 status**: done — the TUI runs on Bubble Tea v2 / Lip Gloss v2 /
Bubbles v2. The "Cursed Renderer" (delta-based rendering) and synchronized
output are automatic in v2. Leak rule (unchanged): the #1 bubbletea memory
leak is an unbound background `tea.Cmd`/`ticker` goroutine that never shuts
down — every background goroutine must be bound to a cancellable
`context.Context` (see PHILOSOPHY.md P1). The streaming ticker here stops as
soon as the reply completes.

---

## 3. Diff: hexops/gotextdiff *(correction from the original proposal)*

**Choice**: `github.com/hexops/gotextdiff` — Myers diff O(ND), used by gopls
& the VSCode Go extension. Standard unified diff output, ready to patch.

**Why it replaces `sergi/go-diff`** (previously proposed):
- The whole point of this component: **close the opencode RAM spike root
  cause** (Levenshtein O(n×m) — a matrix of `len_a × len_b`).
- Myers O(ND): cost scales with the size of the *differences* (D), not the
  product of file sizes — safe for large files.
- `sergi/go-diff` (Google diff-match-patch port) uses heuristics and can
  degrade/eat memory on large inputs — a poor fit for this need.
- `go-difflib` (Python difflib port) is stagnant, lower performance.

**Rule**: the edit tool must work at the *line diff* level (unified hunk),
not character level — deterministic patches and token savings (send hunks to
the model, not whole new files).

---

## 4. Relevance search: hand-rolled BM25 *(factual correction: kelindar/search is not BM25)*

**Choice**: **BM25 written in-house** in `internal/search` — pure Go, zero
dependencies, ~0MB, full control.

**Important factual correction**: the original proposal and our research
claimed `kelindar/search` is a BM25 library — **that was wrong**. After
verifying every released version (v0.1.0 → v0.4.1) via `go doc` + official
README: kelindar/search **never had BM25**. It is a **semantic vector
search** library: `Index[T]` brute-force vectors + `NewVectorizer` requiring
a GGUF model file and a shared library (`libllama_go.dylib/.so`) via purego
— which **violates our single-binary CGO-free requirement**. So
kelindar/search goes into the *deferred* bucket with embeddings (see §7),
not used now.

**Why hand-rolled BM25 (and why embeddings are NOT needed) in the MVP**:
- Our corpus: dozens to hundreds of short descriptions (tool/skill name +
  one-line description). At this scale, keyword matching (BM25) is accurate
  — semantic embeddings only help at thousands+ items with natural-language
  variation.
- Local embeddings = 150–350MB RAM at inference (see §7) → **blows the idle
  RSS < 80MB budget**.
- Hand-rolled BM25 is ~150 lines: tokenize + postings + IDF/TF scoring.
  Zero deps, zero binary bloat, and no risk of the library changing
  direction (kelindar genuinely changed direction — from full-text to
  vector — between versions, proof that zero-dep for core components is the
  right call).

**Usage**: index tool/skill metadata (name + description) at startup (fast,
small), query with the model's natural language → top-k matches → full
schema injected only then (progressive discovery, Principle 2). The index is
bounded at the point of creation (Principle 1): built once from a fixed
corpus; if the corpus becomes dynamic, an eviction policy must exist first.

Rejected alternatives: **Bleve** (heavy, full storage engine, overkill),
**kelindar/search** (not BM25 + needs a shared lib — deferred for the vector
phase).

---

## 5. Storage: JSONL + compaction (default) / ncruces/go-sqlite3 *(correction)*

**Default**: **JSONL append-only + rotation/compaction** for event logs
(usage, session, tool_calls). If relational queries are needed later →
**`github.com/ncruces/go-sqlite3`**.

**Pure-Go SQLite comparison**:

| Driver | How | Binary size | Performance |
|---|---|---|---|
| `mattn/go-sqlite3` | CGO (real C library) | small | best — but CGO is the enemy of single-binary cross-compile |
| `modernc.org/sqlite` | C→Go transpile (ccgo) | **+10–15MB** | 20–40% slower than mattn (was 2–4x, now closing) |
| `ncruces/go-sqlite3` | SQLite compiled to WASM (wasip1) | **+2–4MB** | often *faster* than modernc |

**Rationale**:
- The original proposal (`modernc.org/sqlite`) is okay but **the heaviest** —
  +10–15MB of binary is expensive for a project obsessed with footprint.
- If the need is only telemetry (write-heavy, rarely queried), **SQLite is
  overkill** — JSONL append (`O_APPEND`) is atomic, 0MB, human-readable, and
  rotation/retention *naturally* realizes Principle 5 (TTL/cleanup from day
  one). Note: PHILOSOPHY.md mentions SQLite as an *example* — this
  JSONL-first decision is a **deliberate deviation** (lighter), with the
  same retention obligations.
- ncruces when SQL is needed: CGO-free, light, WAL + UDFs work.
- **Cross-session memory (differentiator from PHILOSOPHY.md §5)**: the store
  design is **TBD** (MemGPT pattern: core/recall/archival), but one thing is
  decided now: it follows the same retention rules (`created_at` + explicit
  prune policy in the initial schema) — not "added later".

**Mandatory if using SQLite (opencode lesson, Principle 5)**: `PRAGMA
auto_vacuum=ON`, regular WAL checkpoints, bounded `mmap_size` (never let the
DB map into the address space), `created_at` + explicit retention in the
initial schema.

---

## 6. MCP: mark3labs/mcp-go + custom filter layer

**Choice**: `github.com/mark3labs/mcp-go` — the most mature in the Go
ecosystem, SSE + stdio transports, high-level ergonomics.

**Three obligations**:
1. **Wrap it behind your own internal interface** (`ToolProvider`,
   `Transport`, etc.) — so you can swap to the official
   `modelcontextprotocol/go-sdk` once spec 2026-07-28 (stateless, MRTR,
   cacheable `tools/list` with `ttlMs`/`cacheScope`) matures further. Do not
   lock code to a library API.
2. **A custom filter/proxy layer is MANDATORY** (do not trust built-ins) —
   implements Principle 2: progressive discovery (catalog → inspect →
   execute). Cache `tools/list` per TTL hints; inject full schemas only when
   a tool is actually called. This is what prevents MCP token tax (40k+
   tokens of schemas before a single user message).
3. **Size cap on tool output (Principle 6)** — MCP server output is
   *untrusted*: an explicit per-output cap is required, and large outputs go
   to a file (reference the path), not raw into context. This is also an
   anti-DoS defense for the context window.

---

## 7. Embeddings & vector search: DEFERRED *(the biggest correction from the original proposal)*

**Original proposal**: `kelindar/search` for embeddings + llama.cpp → **wrong
on the facts** (kelindar/search is not an embedding library) and **blows the
budget**.

**Real numbers**:

| Model | Params | fp32 file | int8 file | int4 file | **RAM at inference in Go** |
|---|---|---|---|---|---|
| all-MiniLM-L6-v2 | 22.7M | ~91MB | ~24MB | ~14–16MB | **~150–250MB** |
| bge-small-en-v1.5 | 33.4M | ~134MB | ~35MB | ~20–22MB | **~200–350MB** |

**Conclusion**: local embeddings in the main binary are not feasible — even
the smallest int4 model still needs 150MB+ runtime RAM, far above the idle
RSS < 80MB budget. Never embed a model in the main binary.

**When embeddings may come in (and how)**:
1. Only if BM25 proves insufficient (semantic matching across languages/
   synonyms at scale).
2. The model **loads lazily on demand**, not at startup — and ideally
   **outside the main binary** (a separate helper binary/plugin spawned as
   needed), so the main process stays under budget.
3. Or use the **provider's embedding API** — 0 local RAM, small cost per
   event. Fits rare matching.

**Vector search**: if ever needed, `chromem-go` (zero-dependency, JSON
persistence, brute-force cosine is enough at hundreds of items) — but do not
install it before embeddings are actually needed.

---

## 8. Compaction architecture: tiered, quality-preserving *(implemented — see PHILOSOPHY.md P4)*

> **Status (Aug 2026): implemented in `internal/tui/app.go`.** Trigger fires
> preventively in `send()` when the forecast transcript exceeds **70%** of the
> provider window (never wait for the cliff; Claude Code's 83.5% trigger is
> proven destructive). L0 goal + L1 verbatim tail (a 25% window share, min 4
> msgs) survive word-for-word; the middle folds into one visible, persisted L2
> ledger message (`buildLedger`), so nothing is compacted silently and the
> result survives a `-c` resume. Every compaction is reported: status notice,
> `N× compact` badge + fold count in the right panel, percent color-coded
> (green < 70% trigger, sand 70–80%, red > 80%).

Compaction is destructive; aggressive compaction is a self-bomb (evidence:
"Governance Decay", arXiv:2606.22528 — constraint violations jump 0% →
30–59% after compaction). So bro-code's doctrine is **avoid compaction first,
compact with structure second**.

| Tier | What lives there | Compaction policy |
|---|---|---|
| **L0 — Pinned core** | goal, user constraints, changed files, decisions, KEEP-directives | **Never compacted** (immutable block) |
| **L1 — Verbatim tail** | last N turns | Kept word-for-word while tactical |
| **L2 — Summary head** | older turns → **structured state ledger** (JSON/Markdown: hypotheses, subtasks, facts, decisions) | Hierarchical block merge, never monolithic prose |
| **L3 — External offload** | full details | Persistent store (JSONL/SQLite, §5), retrievable on demand |

Implementation notes:
- L3 uses the same store as Principle 5 (JSONL default / ncruces) — compaction
  is *recoverable* by design, nothing truly lost.
- The summarizer for L2 is an LLM call — it must be budgeted (Principle 4:
  estimate before send) and its output **measured**: a constraint-survival
  test (re-inject constraints into the summary, verify survival) plus
  task-success comparison on dev sessions.
- Tool references: KEEP-directives (Claude Code `/compact`), summarize/
  truncate strategies (opencode), memory tiers (MemGPT), progressive
  disclosure & note-taking (Anthropic context engineering), HANDOFF files.

## Build & distribution

- **CGO-free** = `CGO_ENABLED=0` in CI (GitHub Actions matrix: macOS arm64,
  linux amd64/arm64) → per-platform release zips.
- **Strip the binary**: `-trimpath -ldflags "-s -w"` (verified: 8.7MB
  stripped on the current stack, Aug 2026). Optional UPX for extreme cases;
  target binary < 20MB.
- One `go.mod`; module path: `github.com/plumpslabs/bro-code`.
- Minimum runtime: Go 1.25.

## Measured footprint (verified baseline, Aug 2026)

The philosophy is numbers, not vibes. Empirically measured on the skeleton:

| Metric | Measured | Budget | Status |
|---|---|---|---|
| Binary size (stripped `-s -w`) | **8.7MB** (v2 stack + session JSONL + landing/suggest/theme picker + status panel + BM25/diff/tokens) | < 20MB | ✅ wide margin |
| Tokenizer | **none embedded** — calibrated heuristic forecast, labeled "~" | — | ✅ (see evaluation below) |
| Idle RSS (TUI process) | **~5MB** | < 80MB | ✅ ~16x margin (typical bubbletea apps: 10–25MB) |
| Startup (headless) | **< 10ms** | < 200ms warm | ✅ |
| Modules in go.mod | 30 | — | TUI stack dominates the binary; several are test-only |

Caveats: the ~5MB idle RSS was sampled before a full first render in a
non-responsive pty (a fully rendered idle TUI may be higher — typical
bubbletea apps run 10–25MB); the < 10ms startup is the tool's resolution
floor (`time -p` prints 0.00), so treat it as "~instant".

**Token-counting evaluation (Aug 2026)** — every pure-Go BPE tokenizer was
measured and REJECTED on footprint: `tiktoken-go/tokenizer` embeds all
encodings + a regex engine (~+14MB → 22MB, blows the gate); `pkoukk/tiktoken-go`
with all vocab (~+5.7MB); even a custom loader embedding only `cl100k_base`
(~+1MB vocab) carries the core lib. Conclusion: follow doctrine P3 instead —
`internal/tokens.Estimate()` is a calibrated heuristic forecast (CJK ≈ 1
rune/token, Latin ≈ 4 chars/token, +4 tokens role overhead per message)
LABELED "~" in every UI; the exact numbers always come from the API response
(settlement). Context usage is recomputed on every chat change (`refreshCtx`).

**Early-warning threshold (when to act, not just how to measure)**: any
change that pushes binary > **10MB** or idle RSS > **50MB** requires a
written rationale in this doc before merging — well before the hard
20MB/80MB budget.

**Future debloat lever**: the TUI uses `textinput`, `viewport`, `spinner`,
and `key` from `bubbles` v2. If slimming is ever needed, hand-rolling the
text input + viewport would drop most of the `bubbles` module (~10 modules)
at the cost of re-implementing key bindings and cursor blink.

**Verification ritual** (run before claiming anything is "lean"):
- Binary size: `ls -lh` on the stripped build.
- Idle RSS: run the TUI, sample `ps -o rss= -p <pid>` while idle.
- Startup: `/usr/bin/time -p ./brocode --search x` (macOS) or `time`.
- Test-only deps: `go mod why <module>` — "(main module does not need
  package …)" means it does NOT enter the binary (verified for
`golang.org/x/tools`, `golang.org/x/mod`, `sahilm/fuzzy`); the
bubbletea/bubbles/lipgloss stack (~30 modules) DOES enter the binary —
that is where the bulk of the 8.7MB comes from (session persistence adds
encoding/json; the pixel wordmark + suggestion popup + theme picker + git
info/status panel + BM25/diff/tokens add the rest), and it is the accepted
cost of the TUI.

## Mapping to PHILOSOPHY.md

| Stack decision | Principle served |
|---|---|
| Hand-rolled BM25 (`internal/search`) + progressive discovery | **P2** — lazy + relevance-scored loading |
| Myers (`hexops/gotextdiff`) | **P1** — bounded; closes the opencode RAM root cause |
| JSONL rotation / SQLite retention | **P5** — explicit TTL/cleanup |
| Custom MCP filter + `tools/list` cache | **P2 + P3** — keep the prefix stable so prompt cache does not miss |
| Size cap on MCP output | **P6** — untrusted content as data |
| Embeddings deferred (not in binary) | **Idle RSS < 80MB budget** + P2 |
| `ncruces` (+2–4MB) over modernc (+10–15MB) | Binary budget / footprint |
| Bubble Tea + anti-lag rules | Local action < 500ms budget, TUI never freezes |
| Tiered compaction (L0 pinned / L1 verbatim / L2 ledger / L3 offload) | **P4** — goal-preserving, visible, recoverable, quality-measured |

> Note: P4 (lossy compaction) and P7 (boundedness tested) do not appear in
> this table because they are *behavioral* principles, not library choices —
> they are enforced via design & CI (soak tests, RSS gates), not via a
> specific dependency.

## References

- PHILOSOPHY.md (principles & performance budget)
- [kelindar/search](https://github.com/kelindar/search) — vector search
  (not BM25, needs a shared lib) — DEFERRED to the vector phase
- [hexops/gotextdiff](https://github.com/hexops/gotextdiff) — Myers, used by
  gopls
- [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3) — SQLite via
  WASM, pure Go
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — C→Go transpile
- [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) — Go MCP
  client/server
- [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk)
  — official SDK (swap candidate)
- [chromem-go](https://github.com/philippgille/chromem-go) — embeddable
  vector DB (deferred)
- "Governance Decay: How Context Compaction Silently Erases Safety
  Constraints" (arXiv:2606.22528) — evidence for the compaction bomb
- "Parallel Context Compaction for Long-Horizon Agent Serving"
  (arXiv:2605.23296)
- Bubble Tea v2: [charm.land/bubbletea/v2](https://charm.land/bubbletea) · [UPGRADE_GUIDE_V2.md](https://github.com/charmbracelet/bubbletea/blob/main/UPGRADE_GUIDE_V2.md)
- Model sizes: HuggingFace `all-MiniLM-L6-v2`, `BAAI/bge-small-en-v1.5`
