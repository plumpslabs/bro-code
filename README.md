# BroCode 🧠

> **A cautious, high-performance senior-engineer agent that is fast because it wastes nothing.**

BroCode is an AI coding agent CLI combining a beautiful native terminal UI (TUI) with a highly efficient, deterministic Go-based orchestration engine. It rejects the "prompt -> code -> done" hallucination model in favor of a strict senior engineering workflow: 

**Understand → Investigate → Decide → Change → Verify → Review.**

Designed incredibly lean: transparent resource usage, architectural efficiency, and strict performance budgets. No Python virtualenvs, no background daemons, no bloat. 

Measured footprint (August 2026): **binary stripped 8.7 MB**, **idle RSS ~5 MB**, **near-instant startup**.

---

## ⚡ Core Philosophy

Mainstream AI coding tools are slow, RAM-hungry, and reckless. BroCode operates on different principles:

1. **Never make a change merely because you can.** If the agent doesn't know WHY a change is needed, it will not touch it.
2. **Minimal Change Principle.** Find the root cause and apply the smallest safe change. No unnecessary full-file rewrites.
3. **Blast Radius Awareness.** Analyzes dependencies before editing. High-risk edits (Auth, DB, Core) require investigation.
4. **Evidence Before Claim.** Uses native `grep`/search before claiming a pattern exists. No hallucinations.
5. **Verification Layer.** Syntax -> Build -> Test -> Diff Review. Automatically enforced.

## ✨ Features

- **TUI + Headless in one binary** — interactive UI and CI/automation mode share the same pipeline.
- **Dynamic Provider Discovery** — Automatically fetches and tests available models from multiple providers (Poolside, Groq, DeepSeek, Lalarasa, OpenCode, Antigravity) without hardcoded static lists.
- **Agentic Routing** — Automatically routes tasks to `Fast`, `Normal`, or `Deep` paths based on query complexity.
- **Risk & Snapshot Engine (L0-L3)** — Automatically detects the risk level of file edits. High-risk edits (L2/L3) trigger automatic workspace snapshots before writing, allowing safe rollbacks.
- **Live Transparency Panel** — Real-time tracking of active model, context window, git branch/path, MCP filter status, sub-agents, and recent tool activity.
- **Session Persistence** — Chat history saved as JSONL in `~/.brocode/sessions`, resumable with `-c`.
- **Slash-Command System** — Type `/` to get a live command suggestion popup (`↑↓` to navigate, `tab`/`enter` to select).
- **No Bloat** — Hand-rolled BM25 search, no embeddings, no always-on secondary processes. Written purely in Go.

## 🚀 Installation

```bash
make build        # → ./bin/brocode
make install      # → ~/go/bin/brocode (or: make install BINDIR=/usr/local/bin)
```

Cross-compile binaries: `make build-all` (linux/amd64, linux/arm64, windows/amd64, darwin/amd64, darwin/arm64).

## 🎮 Quick Start

```bash
./bin/brocode          # Interactive TUI (landing screen → chat)
./bin/brocode -c       # Resume last session
```

Type a question (e.g. `mcp`, `diff`, `memory`) or any other command.
On first open, a pixel wordmark appears centered with the input as a separate form; typing `/` opens a live command suggestion popup.

### Slash Commands

| Command | Description |
|---|---|
| `/connect` | Connect an LLM provider (Poolside, Lalarasa, Groq, OpenCode, etc.) |
| `/models` | Select active AI model (Live dynamically fetched) |
| `/search` | Search tools & skills (BM25) |
| `/diff` | Myers diff demo |
| `/agents` | Primary agent + lazy sub-agents |
| `/mcp` | MCP server status |
| `/usage` | Usage & context window |
| `/memory` | Session memory plan |
| `/tools` | List indexed tools & skills |
| `/theme` | Open theme picker (or `/theme <name>` to set directly) |
| `/queue` | Manage prompt queue (or `ctrl+q`) |
| `/clear` | Start fresh conversation |
| `/help` | Show help text |
| `/quit` | Quit brocode |

### Keyboard Shortcuts

| Key | Action |
|---|---|
| `enter` | Send / accept suggestion |
| `↑` `↓` | Prompt history / scroll |
| `pgup` / `pgdown` / mouse scroll | Scroll viewport |
| `ctrl+o` | Expand/collapse diff hunk & thinking traces |
| `ctrl+y` | Copy answer |
| `ctrl+q` | Prompt queue |
| `?` | Help |
| `q` / `ctrl+c` | Quit |

## 🛠️ Development

```bash
make test        # go test -race ./...
make check       # go vet + gofmt check
make build       # single binary, CGO-free, stripped, version-stamped
make measure     # binary size + startup time
```

## 📂 Project Layout

```
cmd/brocode/      entrypoint (TUI + headless share one pipeline)
internal/tui/     Bubble Tea UI (Bubble Tea v2 + lipgloss v2 + bubbles v2)
internal/agentic/ Agentic Engine (Routing, Risk Evaluation, Snapshots)
internal/search/  BM25 relevance (hand-rolled, zero deps)
internal/diff/    Myers diff (hexops/gotextdiff)
```

## 📜 License

MIT — see [LICENSE](LICENSE).
