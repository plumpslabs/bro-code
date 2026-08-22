# BroCode Architecture Specification

> **Version**: v0.1.30  
> **Target Audience**: Developers, contributors, and systems engineers.

This document details the internal architecture, data flow, and subsystems of BroCode.

---

## 1. High-Level System Architecture

BroCode is built as a single, statically compiled Go binary with zero CGO dependencies. Its architecture is divided into decoupled packages:

```
┌───────────────────────────────────────────────────────────────────────────┐
│                           USER INTERACTION LAYER                          │
│     Bubbletea TUI (internal/ui)  │  Headless CLI (cmd/brocode)           │
│     • Interactive Autocomplete   │  • Batch Turn Queue                   │
│     • Activity & Diff Viewport   │  • Model / Provider Picker            │
└─────────────────────────────────────┬─────────────────────────────────────┘
                                      │
┌─────────────────────────────────────▼─────────────────────────────────────┐
│                           ORCHESTRATION ENGINE                            │
│     Loop Engine (internal/loop)                                           │
│     • ReAct Planning & Execution Cycle                                    │
│     • Context Budget Management & Auto-Compaction                         │
│     • Specialist Swarm Coordinator (Architect -> Builder -> Auditor)     │
└───────────────────┬───────────────────────────────────┬───────────────────┘
                    │                                   │
┌───────────────────▼───────────────┐   ┌───────────────▼───────────────────┐
│          CODE INTELLIGENCE        │   │          TOOL RUNTIME             │
│ • Exact BPE Tokenizer (tokens)    │   │ • 5-Tier Resilient Fuzzy Editor   │
│ • BM25 Search & Symbol Index      │   │ • Atomic Git Shadow Rollback      │
│ • Language Server Protocol (LSP)  │   │ • 3-Tier Resilient Web Search     │
│ • Continuous Memory & Gotchas     │   │ • Sandbox & Command Execution     │
└───────────────────┬───────────────┘   └───────────────┬───────────────────┘
                    │                                   │
┌───────────────────▼───────────────────────────────────▼───────────────────┐
│                           PERSISTENCE & STORAGE                           │
│ • SQLite Event Store (store)   • Project Knowledge (.brocode/memory.md)  │
│ • Snapshot Tree (refs/brocode) • Session Transcripts (~/.brocode/sessions)│
└───────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Core Subsystems

### 2.1 The 5-Tier Resilient Code Editor (`internal/tool/fuzzy_edit.go`)
Patch failures caused by minor whitespace drift or CRLF differences are the #1 failure mode in AI coding agents. BroCode's editor uses a 5-tier resolution ladder:

1. **Tier 1: Exact Match** — Fast string index lookup.
2. **Tier 2: CRLF Normalization** — Strips and unifies `\r\n` to `\n`.
3. **Tier 3: Line-Trimmed Match** — Matches lines ignoring leading and trailing whitespace.
4. **Tier 4: Indentation-Aligned Match** — Detects structural indentation shifts (e.g. nested inside a new `if` block) and automatically re-indents the replacement chunk.
5. **Tier 5: Levenshtein Sliding Window** — Computes minimum edit distance across sliding candidate line windows (similarity threshold $\ge 0.85$).

### 2.2 Atomic Git Shadow Rollback Engine (`internal/tool/git_shadow.go`)
Before any destructive edit is executed, BroCode writes an in-memory Git tree object directly to the Git object database using low-level plumbing commands:
* Snapshots are committed to `refs/brocode/snapshots/turn-<id>`.
* The user's active branch and commit history remain completely untouched.
* The `/undo` command restores the exact working-tree state in $<50\text{ms}$.

### 2.3 Exact Offline BPE Token Accounting (`internal/tokens/`)
Instead of rough character heuristics ($4 \text{ chars} \approx 1 \text{ token}$), BroCode embeds offline `tiktoken-go` byte-pair encoding tables. This enables:
* Exact context window headroom calculation.
* Deterministic auto-compaction before model context limits are exceeded.
* Zero external API calls for token counting.

### 2.4 Multi-Agent Specialist Swarm (`internal/subagent/swarm.go`)
For complex refactoring tasks, the engine coordinates three distinct roles:
1. **Architect** (`PLANNER` mode): Analyzes call trees and defines the architectural specification.
2. **Builder** (`BUILDER` mode): Implements localized file changes and runs unit tests.
3. **Auditor** (`PLANNER` mode): Performs AST verification, checks for dead code, and verifies that conventions are upheld.

### 2.5 Continuous Memory & Gotchas (`internal/memory/`)
Cross-session project knowledge is stored in `.brocode/memory.md`:
* **Gotchas & Traps**: Traps discovered during failed builds or edge cases are captured automatically.
* **Warm-Start Injection**: On startup, relevant facts are scored via BM25 against the user's prompt and dynamically injected into the system prompt.
* **Bounded Line Budget**: Hard cap at 600 lines with FIFO section pruning to prevent prompt bloat.

### 2.6 Multi-Tier Web Search Engine (`internal/tool/web_search_free.go`)
Documentation search operates on a 3-tier fallback architecture:
* **Tier 1**: Exa Semantic Search (active when `EXA_API_KEY` is set).
* **Tier 2**: Tavily AI Search (active when `TAVILY_API_KEY` is set).
* **Tier 3**: DuckDuckGo HTML Lite (zero-config, zero API keys, 100% free fallback).

---

## 3. Concurrency & Memory Safety Guarantees

1. **Context Timeout Bounding**: Every external HTTP request, LSP RPC call, and shell execution is bounded by `context.WithTimeout` and explicit cancellation.
2. **Deterministic Teardown**: Exiting the TUI triggers clean cancellation of in-flight turns, draining scout goroutines and closing LSP connections.
3. **Bounded Memory Allocation**: All response readers are capped with `io.LimitReader` to avoid excessive heap allocation.
