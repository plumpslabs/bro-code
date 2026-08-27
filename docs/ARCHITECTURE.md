# BroCode Architecture Specification

> **Version**: v0.1.54  
> **Target Audience**: Developers, contributors, and systems engineers.

This document details the internal architecture, data flow, modular subsystems, and concurrency guarantees of BroCode.

---

## 1. High-Level System Architecture

BroCode is built as a single, statically compiled Go binary with **zero CGO dependencies** and **zero data races**.

```
┌───────────────────────────────────────────────────────────────────────────┐
│                           USER INTERACTION LAYER                          │
│     Bubble Tea TUI (internal/ui)         │  Headless CLI (cmd/brocode)    │
│     • app.go (Model state & brokers)     │  • Non-interactive Batch Turns │
│     • update.go (Event loop & shortcuts) │  • Session Replay Engine       │
│     • view.go (Layout & sticky footer)   │  • Diagnostic Print Output     │
│     • commands.go (30+ Slash commands)   │  • Script Automation           │
│     • modals.go (Interactive wizards)    │                                │
│     • cards.go & streaming.go (Renderers)│                                │
└─────────────────────────────────────┬─────────────────────────────────────┘
                                      │
┌─────────────────────────────────────▼─────────────────────────────────────┐
│                           ORCHESTRATION ENGINE                            │
│     Loop Engine (internal/loop)                                           │
│     • engine.go (ReAct Turn lifecycle & Tool Dispatch)                    │
│     • fallback.go (Multi-Provider Failover & Circuit Breaker)             │
│     • budget.go (Complexity Tiering & Adaptive Iteration Caps)            │
│     • compaction.go (Context Compaction & 5-Part Memory Extractor)        │
│     • dep_resolver.go (Stack-Agnostic Dependency Resolver)                │
└───────────────────┬───────────────────────────────────┬───────────────────┘
                    │                                   │
┌───────────────────▼───────────────┐   ┌───────────────▼───────────────────┐
│          CODE INTELLIGENCE        │   │          TOOL RUNTIME             │
│ • Exact BPE Tokenizer (tokens)    │   │ • 5-Tier Resilient Fuzzy Editor   │
│ • BM25 Search & Global Index      │   │ • Atomic Git Shadow Rollback      │
│ • Language Server Protocol (LSP)  │   │ • 3-Tier Resilient Web Search     │
│ • Continuous Memory & Gotchas     │   │ • Sandbox & Command Execution     │
│ • Cryptographic Provenance Engine │   │ • MCP Tool Client & Broker        │
└───────────────────┬───────────────┘   └───────────────┬───────────────────┘
                    │                                   │
┌───────────────────▼───────────────────────────────────▼───────────────────┐
│                           PERSISTENCE & STORAGE                           │
│ • SQLite Event Store (store)   • Project Knowledge (.brocode/memory.md)  │
│ • Snapshot Tree (refs/brocode) • Session Transcripts (~/.brocode/sessions)│
│ • Merkle Chain Signatures      • Plans Archive (.brocode/plans/archive/)  │
└───────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Core Subsystems & Decomposition

### 2.1 The 5-Tier Resilient Code Editor (`internal/tool/fuzzy_edit.go`)
Patch failures caused by indentation drift or line-ending mismatches are the #1 failure mode in AI coding agents. BroCode resolves edits via a 5-tier resolution ladder:

1. **Tier 1: Exact Match** — Fast string index lookup.
2. **Tier 2: CRLF Normalization** — Strips and unifies `\r\n` to `\n`.
3. **Tier 3: Line-Trimmed Match** — Matches lines ignoring leading and trailing whitespace.
4. **Tier 4: Indentation-Aligned Match** — Detects structural indentation shifts (e.g. nested inside a new block) and automatically re-indents the replacement chunk.
5. **Tier 5: Levenshtein Sliding Window** — Computes minimum edit distance across sliding candidate line windows (similarity threshold $\ge 0.85$).

### 2.2 Atomic Git Shadow Rollback Engine (`internal/tool/git_shadow.go`)
Before any destructive edit is executed, BroCode writes an in-memory Git tree object directly to the Git object database using low-level plumbing commands:
* Snapshots are committed to `refs/brocode/snapshots/turn-<id>`.
* The user's active branch and commit history remain untouched.
* The `/undo` command restores the exact working-tree state in $<50\text{ms}$.

### 2.3 Cryptographic AI Provenance Engine (`internal/provenance/provenance.go`)
BroCode implements cryptographic AI provenance tracking:
* Every turn generates a signed event record containing parent SHA-256 hash, model identity, author signature, timestamp, and touched file hashes.
* Constructs a tamper-evident Merkle chain verified with `/provenance` or `/verify`.
* Generates CycloneDX-compatible SBOM security manifests with model attestation.

### 2.4 Stack-Agnostic Dependency Resolver (`internal/loop/dep_resolver.go`)
Automatically detects missing dependencies across ecosystems:
* Node.js (`package.json` with npm, pnpm, yarn, bun)
* Go (`go.mod`)
* Python (`requirements.txt`, `pyproject.toml`, `Pipfile`)
* Rust (`Cargo.toml`)
* PHP (`composer.json`)
* Ruby (`Gemfile`)
* .NET (`*.csproj`)
* Calculates impact blast radius and prompts before execution.

### 2.5 Multi-Provider Circuit Breaker & Fallback Router (`internal/loop/fallback.go`)
* Detects transient errors, context timeouts, rate limits, and authentication errors.
* Employs exponential backoff with circuit breaker cooldowns.
* Prompts confirmation when failing over across vendors (e.g. OpenAI to Anthropic).

### 2.6 Context Budget & Auto-Compaction (`internal/loop/compaction.go` & `budget.go`)
* Computes task complexity scores (`tierSimple`, `tierMedium`, `tierComplex`) to set adaptive iteration caps.
* Triggers auto-compaction at 80% context window utilization.
* Extracts structured 5-part summaries (Goals, Decisions, Touched Files, Discoveries, Blockers) into persistent memory.

### 2.7 Native LSP Client & Self-Healing (`internal/lsp/`)
* Discovers installed language servers on `$PATH` for Go, TypeScript/JS, Python, Rust, C/C++, Vue, Svelte, HTML, CSS, and JSON.
* Streams diagnostics in real-time.
* Provides `/diagnose fix` to automatically resolve missing imports, deprecated symbols, and renames.

---

## 3. Concurrency & Memory Safety Guarantees

1. **Zero Data Races**: Verified via `go test -race ./...` across all 25 packages.
2. **Context Timeout Bounding**: Every external HTTP request, LSP RPC call, and shell execution is bounded by `context.WithTimeout` and explicit cancellation.
3. **Deterministic Teardown**: Exiting the TUI triggers clean cancellation of in-flight turns, draining scout goroutines and closing LSP connections.
4. **Kernel Memory Reclamation**: Runs explicit `runtime.GC()` and `debug.FreeOSMemory()` after each turn completion to immediately release buffers back to the OS.
