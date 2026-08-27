<div align="center">

```text
┌┐ ┬─┐┌─┐╔═╗┌─┐┌┬┐┌─┐
├┴┐├┬┘│ │║  │ │ ││├┤ 
└─┘┴└─└─┘╚═╝└─┘─┴┘└─┘
ship less, ship right

BroCode v0.1.54
```

**Autonomous AI Coding Agent for High-Performance Software Engineering**  
*Deterministic, token-efficient, zero-data-race coding assistant for real-world production codebases.*

<p align="center">
  <a href="https://github.com/plumpslabs/bro-code/releases"><img src="https://img.shields.io/badge/release-v0.1.54-blue.svg?style=flat-square" alt="Version"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/badge/go-1.24+-00ADD8.svg?style=flat-square&logo=go" alt="Go Version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green.svg?style=flat-square" alt="License"></a>
  <a href="#-platform-compatibility"><img src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows%20%7C%20BSD-lightgrey.svg?style=flat-square" alt="Platforms"></a>
</p>

</div>

---

## ⚡ Overview

BroCode is an autonomous AI coding agent designed for precision, reliability, and token efficiency. Rather than performing unconstrained file overwrites or speculative edits, BroCode follows a deterministic engineering cycle:

$$\text{Analyze Context} \longrightarrow \text{Plan Architecture} \longrightarrow \text{Locate Symbols} \longrightarrow \text{Apply Resilient Edit} \longrightarrow \text{Verify Build/Tests} \longrightarrow \text{Retain Project Rules}$$

### 🛡️ Core Engineering Pillars:
1. **Targeted Precision:** Performs localized, surgical edits without unnecessary full-file rewrites.
2. **5-Tier Resilient Code Editor:** Automatically resolves indentation shifts, line trimming, CRLF/LF line endings, and sliding-window fuzzy matching to prevent patch failures.
3. **Atomic Git Shadow Tree Rollback:** Generates zero-loss Git plumbing snapshots under `refs/brocode/snapshots` before writing to disk, enabling instant `/undo` without corrupting working branches.
4. **Cryptographic AI Provenance Engine:** Tracks every modification with SHA-256 Merkle chains and signed SBOM artifacts to mathematically verify AI authorship via `/verify` and `/trace`.
5. **Stack-Agnostic Dependency Resolver & Blast Radius:** Analyzes project dependencies across Node.js, Go, Python, Rust, PHP, Ruby, and .NET to auto-resolve missing packages and map blast radiuses.
6. **Multi-Provider Circuit Breaker & Fallback Router:** Automatically fails over to backup models upon rate limits or server errors with cooldown management and vendor isolation.
7. **Exact Offline BPE Token Accounting & Auto-Compaction:** Native `tiktoken-go` offline tokenizer ensures deterministic context compaction at 80% capacity with 5-part structured memory preservation.
8. **Native LSP & One-Click Auto-Fix:** Auto-detects installed language servers (TypeScript, Go, Python, Rust, C/C++, Vue, Svelte) to deliver project-wide diagnostics, code actions, and auto-imports.

---

## 📦 Installation

Choose the installation method that fits your workflow:

### Method 1: One-Line Installer

**macOS & Linux (Bash/Zsh):**
```bash
curl -fsSL https://raw.githubusercontent.com/plumpslabs/bro-code/main/scripts/install.sh | bash
```

**Windows (PowerShell):**
```powershell
irm https://raw.githubusercontent.com/plumpslabs/bro-code/main/scripts/install.ps1 | iex
```

### Method 2: Go Install
```bash
go install github.com/plumpslabs/bro-code/cmd/brocode@latest
```

### Method 3: Homebrew (macOS & Linux)
```bash
brew tap plumpslabs/tap
brew install brocode
```

### Method 4: Pre-built Binaries
Download pre-compiled binaries from [GitHub Releases](https://github.com/plumpslabs/bro-code/releases) for:
* **macOS**: Apple Silicon (`arm64`) / Intel (`amd64`)
* **Linux**: `amd64` / `arm64` / Alpine
* **Windows**: `amd64` / `arm64` (`.zip`)
* **FreeBSD**: `amd64`

### Method 5: Build from Source
```bash
git clone https://github.com/plumpslabs/bro-code.git
cd bro-code
go build -ldflags="-s -w" -o brocode ./cmd/brocode
sudo mv brocode /usr/local/bin/
```

---

## 🎮 Quick Start

```bash
# Start an interactive session in the current directory
brocode

# Resume the most recent conversation session
brocode -c

# Launch with a specific provider and model
brocode -provider anthropic -model claude-3-7-sonnet

# Set a per-task spending cap (USD)
brocode -budget 1.00

# Inspect CLI version
brocode --version
```

---

## 🎭 Agent Operating Modes

Switch modes at any time using **`Shift + Tab`** or dedicated slash commands (`/builder`, `/planner`, `/miner`):

| Mode | Visual Theme | Badge | Description |
| :--- | :---: | :---: | :--- |
| **`BUILDER`** | 🟢 Green (ANSI `42`) | `🟢 BUILDER` | Autonomous implementation mode: inspects code, applies resilient edits, executes shell commands, and verifies test suites. |
| **`PLANNER`** | 🟣 Purple (ANSI `141`) | `🟣 PLANNER` | Read-only architecture & strategy mode: analyzes call trees, drafts execution roadmaps, and archives plans without modifying files. |
| **`MINER`** | 🟡 Gold (ANSI `214`) | `🟡 MINER` | Continuous knowledge extraction mode: analyzes repo structure and persists verified rules, conventions, and traps to `.brocode/memory.md`. |

---

## ⌨️ Keyboard Shortcuts & TUI Navigation

| Shortcut | Scope | Action |
| :--- | :--- | :--- |
| `Shift + Tab` | Global | Cycle operating modes (`BUILDER` ➔ `PLANNER` ➔ `MINER`) |
| `Ctrl + K` / `Alt + K` | Input / Queue | Open **Prompt Queue Management** (`e` edit, `d` delete, `m` switch mode, `K`/`J` reorder) |
| `Ctrl + P` | Viewport | Toggle **In-TUI Full-Answer Pager** (scroll last assistant answer with `PgUp`/`PgDn`/`↑`/`↓`/`Home`/`End`, exit with `q`/`Esc`) |
| `Ctrl + Y` | Global | **Copy Last Response** directly to system OS clipboard |
| `Ctrl + M` | Global | Toggle **Mouse Mode** (`SCROLL` for wheel scrolling vs `SELECT` for native terminal drag-select) |
| `Ctrl + F` | Chat Log | Toggle **Live Diff Expansion** between compact summary rows and full colorized diffs |
| `Ctrl + U` / `Alt + ⌫`| Input | Clear the entire prompt input line instantly |
| `Ctrl + V` | Input / Modals | Paste system clipboard content directly (supports multi-line prompt pastes) |
| `1` – `9` | Empty Input | Trigger interactive **Senior Recommendations** numbered 1 to 9 |
| `↑` / `↓` | Input / Autocomplete | Navigate prompt history or scroll through suggestion popup window |
| `Tab` / `Enter` | Autocomplete | Select and insert active suggestion |
| `Esc` | Global | Cancel suggestion popup, close open modal, or interrupt active turn execution |
| `Ctrl + C` | Global | Cleanly terminate session and gracefully release in-flight goroutines |

---

## 📖 Comprehensive Slash Commands Reference

Type `/` in the prompt to open fuzzy autocomplete for all 34 built-in commands:

### 1. Sessions & History
* `/new` — Start a fresh, clean conversation session.
* `/sessions` (`/s`) — Interactive session manager modal (resume, search, `d` delete, `D` delete all).
* `/history` — Display full conversation transcript for the active session.
* `/compact` (`/summarize`) — Force instant context compaction into structured session memory.
* `/clear` (`/cls`, `/reset`) — Clear current chat viewport buffer.
* `/undo` — **Time-Travel Rollback**: Revert all file changes from the last turn via Git shadow tree snapshots.
* `/diff` — Render a visual side-by-side / unified diff of all modifications made in the session.
* `/cost` (`/tokens`, `/usage`) — Inspect real-time token consumption, context usage, and estimated spend.

### 2. Modes & Agent Architecture
* `/mode [builder|planner|miner]` — Inspect or switch the active agent operating mode.
* `/builder` — Activate `BUILDER` mode (autonomous coding & command execution).
* `/plan` (`/planner`) — Activate `PLANNER` mode (read-only architectural planning).
* `/plan clear` (`/plan reset`) — Archive the active plan from `.brocode/current_plan.md` to `.brocode/plans/archive/`.
* `/mine` (`/miner`) — Activate `MINER` mode (knowledge extraction & memory persistence).
* `/agent [name]` — Inspect or switch between custom agent definitions in `.brocode/agents/*.md`.

### 3. Code Intelligence & Quality
* `/diagnose` (`/scan`, `/lint`) — Run project-wide LSP diagnostics and display categorized error/warning lists.
* `/diagnose fix` (`/scan fix`) — Automatically invoke safe quick-fixes, import organizes, and renames.
* `/lsp` — Inspect connected Language Server Protocol managers, active servers, and install hints.
* `/deps` (`/dep-resolve`) — Run the stack-agnostic dependency resolver to detect and install missing packages.
* `/spec [prompt]` — Execute the architectural specification engine to draft a full blueprint before building.
* `/tournament [prompt]` — Run competitive multi-solution tournament benchmarking to evaluate algorithmic trade-offs.
* `/repair [prompt]` — Run autonomous root-cause analysis and automated test-driven repair.

### 4. Security, Provenance & Audit
* `/provenance` (`/verify`) — Verify the cryptographic Merkle chain and tamper-evident AI SBOM signature.
* `/trace` — Trace the complete provenance origin, generation timestamps, and author hashes of touched files.
* `/mcp` — Open the Model Context Protocol manager (inspect connected servers, `a` add wizard, `d` delete, `r` reload).
* `/debug` — Display runtime engine metrics, memory allocation stats, and goroutine health.

### 5. Providers & Configuration
* `/connect` (`/c`) — Launch the interactive provider configuration wizard (API keys, endpoints, custom gateways).
* `/models` (`/m`) — Open the interactive model picker with real-time fuzzy search.
* `/search-key [key]` — Configure or check web search provider credentials (Exa / Tavily).
* `/context7-key [key]` — Configure Context7 documentation resolver credentials.
* `/update` (`/upgrade`) — Check for and install the latest BroCode release binary.
* `/quit` (`/exit`, `/q`) — Exit BroCode cleanly.

---

## 🌐 Supported LLM Providers & Gateways

BroCode connects natively to all major LLM providers with automatic fallback routing:

* **Anthropic**: `claude-3-7-sonnet`, `claude-3-5-sonnet`, `claude-3-5-haiku`
* **OpenAI**: `gpt-4o`, `o1`, `o3-mini`, `gpt-4o-mini`
* **DeepSeek**: `deepseek-chat`, `deepseek-reasoner`
* **Poolside / Laguna**: `laguna-s-2.1`
* **Ollama / Local LLMs**: `qwen2.5-coder`, `llama3.3`, `deepseek-r1`, `mistral`
* **OpenRouter** & **Groq**
* **OpenCode Gateway** & Any OpenAI-Compatible Endpoints

---

## 🛠️ Versioning & Release Automation

BroCode follows [Semantic Versioning (SemVer)](https://semver.org/).

Use the included helper script to release updates:
```bash
# Bump patch version (v0.1.40 -> v0.1.41)
./scripts/bump_version.sh patch

# Bump minor version (v0.1.40 -> v0.2.0)
./scripts/bump_version.sh minor

# Bump major version (v0.1.40 -> v1.0.0)
./scripts/bump_version.sh major
```

---

## 🌍 Platform Compatibility

BroCode is built with 100% Pure Go (Zero-CGO) and runs natively on:
* **macOS**: Apple Silicon (`arm64`) & Intel (`amd64`)
* **Linux**: `amd64`, `arm64`, Alpine, Ubuntu, Debian, Arch, Fedora
* **Windows**: Windows 10/11 `amd64` & `arm64` (PowerShell, CMD, Git Bash, Windows Terminal)
* **FreeBSD**: `amd64`

---

## 📜 License

MIT License — see [LICENSE](LICENSE) for details.
