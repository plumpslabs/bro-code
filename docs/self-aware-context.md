# Self-Aware Context Layer — Structured Experience Compaction

BroCode's native, standalone self-aware context system. It captures **every**
agent action with provenance, consolidates experience into durable, searchable
notes across sessions, and recalls the right prior at a fraction of the token
cost of re-scanning. No external MCP, no vector database, no embeddings server.

This is the **efficient anomaly**: the big players (Mem0 / Zep / LangMem) bolt a
vector stack onto the agent and pay 200–500 ms latency per recall plus
near-duplicate degradation. BroCode does structured compaction in pure Go on the
existing session SQLite DB.

## Methodology: retain → recall → reflect

| Step      | What                                                          | Where |
|-----------|----------------------------------------------------------------|-------|
| **retain** | Every tool call records a provenance-tagged note (tool, target, outcome). Mutating tools also invalidate the Smart Context file hash. | `internal/tool/registry.go` `Execute` → `recordActionNote` + `InvalidateKnowledge` |
| **recall** | Agent-facing `context_recall` tool + silent warm-start injection of distilled notes into the system prompt (facts/decisions/gotchas/hot files). | `internal/tool/context_recall.go`, `internal/loop/engine.go` `buildSystemPrompt` |
| **reflect**| Deterministic consolidation at compaction + session end: hot files → facts, repeated failures → gotchas. No extra LLM call. | `internal/context/reflect.go` `Reflect` |

### Why deterministic reflection (not an LLM round-trip)?

The research (Hindsight 2512.12818, SimpleMem 2601.02553) shows *memory
transformation* matters more than raw storage. But the transformation can be
**structural**, not generative:

- **Hot file → fact**: a file touched N times is central infrastructure.
- **Repeated `(file, tool)` failure → gotcha**: the trap is now known.

This keeps the layer an *efficient anomaly* — compounding signal without a
summarization round-trip or a vector stack. A generative variant (LLM distill)
remains a clean extension point behind the same `Reflect` signature.

## Storage

Single SQLite DB (the existing session store), three tables:

- `knowledge` — file content hashes + co-read/edit neighbor edges + **whole-file
  symbol index** (the graph).
- `notes` — experience / hotfile / fact / belief / decision / gotcha notes with
  provenance + weight + confidence.
- `events` — session event stream (user/assistant/tool/compaction).

Both `knowledge` and `notes` share one `*store.Store` handle, so they are wired
with a single `SetKnowledgeStore` call in `internal/ui/app.go`.

## Big-file comprehension (the "5000 lines, only 1000 known" problem)

A naive capture stores only what was *shown* to the model. read_file leans by
default: files >150 lines return a head preview (or a shrinkwrap/range slice),
so a 5000-line file would leave 4000 lines permanently "unknown" to the graph.

BroCode solves this without force-cutting and without embeddings:

- **Capture happens inside `ReadFileTool`** on every read (full / shrinkwrap /
  range). It has the FULL file content in hand, so it indexes **every symbol's
  line range** (`SymbolRange{Name, Kind, Start, End}`) across the entire file —
  even when only a span was returned to the model.
- The index is regex/AST-boundary aligned per language (cAST / RepoCoder style:
  chunk on function/class boundaries, not arbitrary N lines) and capped at 80
  symbols/file.
- **Recall is position-aware (coarse-to-fine):** a query like "omega handler"
  matches the `omega` symbol and `FormatKnowledgeHints` surfaces it *first* with
  its line span (`omega (L4953-4953)`), so the agent jumps straight there with
  `read_file(start_line/end_line)` instead of re-reading the whole file.

Net effect: the agent always knows where everything is in a huge file, the graph
stays tiny (hashes + tags + ≤80 line spans, no file content), and there is zero
added read latency (capture is async + recovered).

## Benchmark (thesis validation)

`go test ./internal/store/ -run 'TestRecallQuality|TestTokenSavings' -v`

- **recall@3 = 100%** across a 60-note corpus (10 gold + 50 noise).
- **token savings = 97.6%** vs naive re-read of every file.
- `BenchmarkRecall`: ~222 µs/op, no network, no embeddings server.

Compare: vector-stack agents add 200–500 ms *per recall* and degrade on
near-duplicates. BroCode's structured compaction is sub-millisecond and
duplicate-tolerant (UPSERT-by-(kind,subject) reinforces weight).

## Files

- `internal/store/notes.go` — note store (Record / Recall / QueryForPrompt / Prune)
- `internal/store/symbols.go` — whole-file structural index (`SymbolRange`: name →
  line span), language-aware regex extraction, bounded to 80 symbols/file
- `internal/store/db.go` — `notes` + `knowledge` (with `symbols`) table schema
- `internal/tool/registry.go` — centralized capture hooks (catch-all notes, lsp invalidation, turn-file neighbor edges)
- `internal/tool/context_recall.go` — agent-facing recall tool
- `internal/tool/memory.go` — cross-session facts (memory.md) — complementary, not replaced
- `internal/loop/engine.go` — warm-start injection + session-end reflection
  - **Always-on architecture seed**: when the prompt is vague (few keyword
    matches), `TopNotes` surfaces the top-weighted facts/decisions/gotchas/hot
    files anyway, so the agent starts each session architecture-aware *without
    re-reading the whole codebase*. Bounded (top-8, recency-weighted) and
    advisory ("explore freely when uncertain") — it never suppresses exploration.
- `internal/context/manager.go` — reflection at compaction
- `internal/context/reflect.go` — deterministic consolidation
- `internal/prompt/prompt.go` — `NotesHints` block

## Design constraints honored

- **Incremental**: layered on top; memory.md / skills / knowledge surfaces
  unchanged. `context_recall` is an additive tool.
- **Standalone**: zero external dependencies; pure Go on the existing DB.
- **Latency-free**: all writes are async (`go func` + `recover`); recall is a
  single indexed query.
- **Bounded**: `notes` capped at 200 rows; stale (<7d, weight<1.0) pruned.
