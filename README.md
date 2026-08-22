<div align="center">

```text
██████╗ ██████╗  ██████╗  ██████╗ ██████╗ ██████╗ ███████╗
██╔══██╗██╔══██╗██╔═══██╗██╔════╝██╔═══██╗██╔══██╗██╔════╝
██████╔╝██████╔╝██║   ██║██║     ██║   ██║██║  ██║█████╗  
██╔══██╗██╔══██╗██║   ██║██║     ██║   ██║██║  ██║██╔══╝  
██████╔╝██║  ██║╚██████╔╝╚██████╗╚██████╔╝██████╔╝███████╗
╚═════╝ ╚═╝  ╚═╝ ╚═════╝  ╚═════╝ ╚═════╝ ╚═════╝ ╚══════╝
```

**Autonomous AI Coding Agent for Software Engineering**  
*High-performance, token-efficient, and deterministic coding assistant for real-world codebases.*

<p align="center">
  <a href="https://github.com/plumpslabs/bro-code/releases"><img src="https://img.shields.io/badge/release-v0.1.28-blue.svg?style=flat-square" alt="Version"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/badge/go-1.24+-00ADD8.svg?style=flat-square&logo=go" alt="Go Version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green.svg?style=flat-square" alt="License"></a>
  <a href="#-platform-compatibility"><img src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows%20%7C%20BSD-lightgrey.svg?style=flat-square" alt="Platforms"></a>
</p>

</div>

---

## ⚡ Overview

BroCode is an autonomous AI coding agent designed for precision, reliability, and token efficiency. Rather than performing unconstrained file overwrites or speculative edits, BroCode follows a deterministic engineering cycle:

**Analyze Context → Plan Architecture → Locate Symbols → Apply Resilient Edit → Verify Build/Tests → Retain Project Rules**

### Core Engineering Principles:
1. **Targeted Precision:** Performs localized, surgical edits without unnecessary full-file rewrites.
2. **5-Tier Resilient Code Editor:** Automatically resolves indentation shifts, line trimming, CRLF/LF line endings, and sliding-window fuzzy matching to prevent patch failures.
3. **Atomic Shadow Tree Rollback:** Generates zero-loss Git plumbing snapshots under `refs/brocode/snapshots` before writing to disk, enabling instant `/undo` without corrupting working branches.
4. **Exact Offline BPE Token Accounting:** Precise context calibration using native `tiktoken-go` without external network latency.
5. **Continuous Project Knowledge:** Automatically extracts and persists verified rules, architecture patterns, and traps to `.brocode/memory.md` with dynamic BM25 retrieval.
6. **Collaborative Specialist Swarm:** Multi-agent pipeline coordinating Architect, Builder, and Auditor roles for structured code transformations.

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

Switch modes at any time using **`Shift + Tab`**:

| Mode | Badge | Description |
| :--- | :--- | :--- |
| **`BUILDER`** | 🔵 `BUILDER` | Autonomous implementation mode: inspects code, applies resilient edits, executes shell commands, and verifies tests. |
| **`PLANNER`** | 🟣 `PLANNER` | Read-only analysis mode: surveys repositories, inspects call graphs, and drafts structured roadmaps without modifying files. |
| **`MINER`** | 🟡 `MINER` | Knowledge extraction mode: analyzes repository structure and persists verified rules and patterns to `.brocode/memory.md`. |

---

## ⌨️ Interactive Autocomplete & Navigation

BroCode includes a floating suggestion popup in the terminal interface:

* **⚡ Slash Commands (`/`)**: Type `/` to display built-in commands with live descriptions.
* **📂 File Mentions (`@`)**: Type `@` anywhere in the input prompt (e.g. `@app.go`) for fuzzy path completions.
* **Controls**:
  * `↑` / `↓` — Navigate suggestions (with automatic sliding-window scrolling).
  * `Tab` or `Enter` — Select and apply the active suggestion.
  * `Esc` — Dismiss the popup.

---

## 📖 Slash Commands

| Command | Description |
| :--- | :--- |
| `/help` | Show available commands and shortcuts |
| `/undo` | **Time-Travel Rollback**: Instantly revert all file modifications from the previous turn |
| `/sessions` | Switch, resume, or manage saved chat sessions |
| `/models` | Open the interactive AI model selector |
| `/connect` | Configure LLM providers and API keys interactively |
| `/memory` | Inspect cross-session project memory and verified rules |
| `/cost` | View token usage and estimated spend breakdown |
| `/lsp` | Inspect Language Server Protocol status and diagnostics |
| `/diagnose` | Run self-contained syntax and type diagnostics on modified files |
| `/miner` | Switch active mode to MINER |
| `/new` | Start a fresh conversation session |
| `/clear` | Clear the current terminal viewport |

---

## 🌐 Supported Providers

BroCode connects natively to all major LLM providers:

* **Anthropic** (`claude-3-7-sonnet`, `claude-3-5-sonnet`, `claude-3-5-haiku`)
* **OpenAI** (`gpt-4o`, `o1`, `o3-mini`)
* **DeepSeek** (`deepseek-chat`, `deepseek-reasoner`)
* **Poolside / Laguna** (`laguna-s-2.1`)
* **Ollama / Local Models** (`qwen2.5-coder`, `llama3.3`, `deepseek-r1`)
* **OpenRouter** & **Groq**
* **OpenCode Gateway**

To configure providers, run `/connect` within the interactive interface.

---

## 🛠️ Versioning & Release Automation

BroCode follows [Semantic Versioning (SemVer)](https://semver.org/).

Use the included helper script to release updates:
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

BroCode is built with 100% Pure Go (Zero-CGO) and runs seamlessly on:
* **macOS**: Apple Silicon (`arm64`) & Intel (`amd64`)
* **Linux**: `amd64`, `arm64`, Alpine, Ubuntu, Debian, Arch, Fedora
* **Windows**: Windows 10/11 `amd64` & `arm64` (PowerShell, CMD, Git Bash)
* **FreeBSD**: `amd64`

---

## 📜 License

MIT License — see [LICENSE](LICENSE) for details.
