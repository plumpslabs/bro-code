<p align="center">
  <h1 align="center">🧠 BroCode</h1>
  <p align="center">
    <strong>The Senior-Level Autonomous AI Coding Agent for Real-World Codebases.</strong><br>
    <em>Fast, resilient, token-efficient, and cautious because it wastes nothing.</em>
  </p>
  <p align="center">
    <a href="#-features"><img src="https://img.shields.io/badge/version-v0.1.0-blue.svg?style=flat-square" alt="Version"></a>
    <a href="#-installation"><img src="https://img.shields.io/badge/go-1.24+-00ADD8.svg?style=flat-square&logo=go" alt="Go Version"></a>
    <a href="#-license"><img src="https://img.shields.io/badge/license-MIT-green.svg?style=flat-square" alt="License"></a>
    <a href="#-compatibility"><img src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows%20%7C%20BSD-lightgrey.svg?style=flat-square" alt="Platforms"></a>
  </p>
</p>

---

## ⚡ Why BroCode?

Most AI coding tools behave like impatient junior developers: they guess file paths, rewrite entire files unnecessarily, hallucinate nonexistent functions, swallow errors, and waste thousands of tokens per turn.

**BroCode operates with the discipline of a Staff Engineer:**
1. **Never edit blind:** Explores call graphs and verifies symbols before touching code.
2. **5-Tier Resilient Editing:** Never fails with "target not found" due to whitespace, CRLF, or minor line drift.
3. **Zero-Loss Atomic Rollback:** Creates shadow Git plumbing snapshots before modifications, allowing instantaneous `/undo` without corrupting your active branch history.
4. **Exact Offline BPE Token Accounting:** Accurate token windows powered by `tiktoken-go`.
5. **Continuous Project Memory:** Retains verified architectural decisions, build rules, and gotchas in `.brocode/memory.md` with bounded BM25 dynamic retrieval.
6. **Multi-Agent Specialist Swarm:** Orchestrates Architect, Builder, and Auditor agents collaboratively for complex refactoring.

---

## ✨ Key Architectural Highlights

```
┌────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                   BROCODE CORE CAPABILITIES                                    │
├───────────────────────────────┬────────────────────────────────┬───────────────────────────────┤
│ 🛡️ 5-Tier Resilient Editor    │ ⏪ Atomic Git Shadow Rollback  │ 🐝 Collaborative Swarm        │
│ Exact, CRLF, trimmed-line,    │ Zero-loss tree snapshots under │ 3-stage pipeline (Architect   │
│ indent-aligned & Levenshtein  │ `refs/brocode/snapshots`       │ -> Builder -> Auditor)        │
├───────────────────────────────┼────────────────────────────────┼───────────────────────────────┤
│ ⌨️ Interactive Autocomplete   │ 🧠 Continuous Project Memory   │ 🌐 Zero-Config Web Search     │
│ Floating popup for `/` slash  │ Bounded gotcha retention with  │ Free DuckDuckGo Lite fallback │
│ commands & `@` file mentions  │ dynamic BM25 warm-start        │ + Exa / Tavily API support    │
├───────────────────────────────┼────────────────────────────────┼───────────────────────────────┤
│ 🔍 Code Intelligence (LSP)    │ 🏢 Enterprise Ignore Matrix    │ ⚡ Pure Go / Zero-CGO         │
│ Diagnostics, hover, go-to-def │ Auto-skips dependencies &      │ Blazing fast, statically      │
│ for Go, TS/JS, Python, Rust   │ caches in 500k+ file repos     │ linked, <10ms startup time    │
└───────────────────────────────┴────────────────────────────────┴───────────────────────────────┘
```

---

## 🚀 Installation

### Option 1: Go Install (Recommended)
```bash
go install github.com/plumpslabs/bro-code/cmd/brocode@v0.1.0
```

### Option 2: Build From Source
```bash
git clone https://github.com/plumpslabs/bro-code.git
cd bro-code
go build -ldflags="-s -w" -o brocode ./cmd/brocode
sudo mv brocode /usr/local/bin/
```

### Option 3: Check CLI Version
```bash
brocode --version
# Output: BroCode v0.1.0
```

---

## 🎮 Quick Start

```bash
# Start an interactive session in current repository
brocode

# Resume previous active session
brocode -c

# Run with a specific provider and model
brocode -provider anthropic -model claude-3-7-sonnet

# Set per-task budget cap (USD)
brocode -budget 0.50
```

---

## 🎭 Agent Operating Modes

Switch modes instantly using **`Shift + Tab`**:

| Mode | Badge | Description |
| :--- | :--- | :--- |
| **`BUILDER`** | 🔵 `BUILDER` | Full autonomous mode: reads files, executes shell commands, applies resilient edits, and verifies tests. |
| **`PLANNER`** | 🟣 `PLANNER` | Read-only mode: analyzes code architecture, explores call graphs, and drafts implementation roadmaps without modifying files. |
| **`MINER`** | 🟡 `MINER` | Knowledge extraction mode: explores repository structure and persists verified rules and gotchas to `.brocode/memory.md`. |

---

## ⌨️ Interactive Autocomplete & Mentions

BroCode features a zero-latency floating suggestion box in the TUI:

* **⚡ Slash Commands (`/`)**: Type `/` to browse available commands.
* **📂 File Mentions (`@`)**: Type `@` anywhere in your prompt (e.g. `@app.go`) for fuzzy file path completion.
* **Controls**: Use `↑` / `↓` to navigate suggestions, `Tab` or `Enter` to select, and `Esc` to dismiss.

---

## 📖 Slash Commands

| Command | Description |
| :--- | :--- |
| `/help` | Show available commands and shortcuts |
| `/undo` | **Time-Travel Rollback**: Instantly revert all file changes from the last turn via Git shadow snapshots |
| `/sessions` | Switch, resume, or manage past chat sessions (`d` delete, `D` delete all) |
| `/models` | Open interactive model picker |
| `/connect` | Setup LLM providers & API keys interactively (Anthropic, OpenAI, DeepSeek, Ollama, etc.) |
| `/memory` | Inspect cross-session project memory, architectural decisions, and captured gotchas |
| `/cost` | View token usage and estimated spend breakdown |
| `/lsp` | Inspect connected Language Server Protocol status |
| `/diagnose` | Run self-contained type error and syntax diagnostics on modified files |
| `/miner` | Switch to MINER mode |
| `/new` | Start a fresh conversation session |
| `/clear` | Clear the chat viewport |

---

## 🌐 Supported Providers

BroCode supports all major LLM providers out of the box with zero external gateways:

- **Anthropic** (`claude-3-7-sonnet`, `claude-3-5-sonnet`, `claude-3-5-haiku`)
- **OpenAI** (`gpt-4o`, `o1`, `o3-mini`)
- **DeepSeek** (`deepseek-chat`, `deepseek-reasoner`)
- **Poolside / Laguna** (`laguna-s-2.1`)
- **Ollama / Local Models** (`qwen2.5-coder`, `llama3.3`, `deepseek-r1`)
- **OpenRouter** & **Groq**
- **OpenCode** gateway

Configure providers at any time by typing `/connect` inside BroCode.

---

## 🛠️ Versioning & Release Workflow

BroCode follows [Semantic Versioning (SemVer)](https://semver.org/).

Use the built-in version bump script to release new versions:
```bash
# Bump patch version (v0.1.0 -> v0.1.1)
./scripts/bump_version.sh patch

# Bump minor version (v0.1.0 -> v0.2.0)
./scripts/bump_version.sh minor

# Bump major version (v0.1.0 -> v1.0.0)
./scripts/bump_version.sh major
```

---

## 🌍 Platform Compatibility

BroCode is built with 100% Pure Go (Zero-CGO) and is fully tested on:
- **macOS** (Apple Silicon `arm64` & Intel `amd64`)
- **Linux** (`amd64`, `arm64`, Alpine, Ubuntu, Debian, Arch)
- **Windows** (Windows 10/11 `amd64` & `arm64` with PowerShell / CMD / Git Bash)
- **FreeBSD / BSD** (`amd64`)

---

## 📜 License

MIT License — see [LICENSE](LICENSE) for details.
