# BroCode CLI & Command Reference

> **Version**: v0.1.56  
> **Status**: Comprehensive Reference Guide for CLI Flags, Interactive Slash Commands, Keyboard Shortcuts, Operating Modes, and Environment Variables.

---

## 1. CLI Flags & Terminal Options

```bash
brocode [flags] [prompt]
```

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `-v`, `--version` | bool | `false` | Print BroCode version, git commit, build timestamp, and brand logo |
| `-c`, `--continue`| bool | `false` | Resume the most recent conversation session automatically |
| `-session` | string | `""` | Resume a specific session by its unique ID (e.g. `-session sess_1787...`) |
| `-provider` | string | `""` | Override primary LLM provider ID (`anthropic`, `openai`, `deepseek`, `poolside`, `ollama`, `openrouter`, `groq`) |
| `-model` | string | `""` | Override model name (e.g. `claude-3-7-sonnet`, `gpt-4o`, `deepseek-chat`, `laguna-s-2.1`) |
| `-budget` | float64| `0.0` | Set a per-task spending cap in USD (0 = unlimited). The engine stops gracefully once reached |
| `-replay` | string | `""` | Replay a recorded session transcript directly to stdout without running TUI |
| `-log` | string | `""` | Stream live agent activity events and tool calls to a specified log file |

---

## 2. Interactive Slash Commands (Complete List)

Type `/` in the prompt input to open fuzzy autocomplete with live command descriptions:

### 📁 Session & History Management
| Command | Aliases | Description |
| :--- | :--- | :--- |
| `/new` | `/n` | Start a clean, fresh conversation session |
| `/sessions` | `/s`, `/session` | Open interactive session manager modal (`Enter` resume, `d` delete, `D` delete all) |
| `/history` | — | Display the entire conversation transcript for the active session |
| `/compact` | `/summarize` | Force instant context compaction into structured 5-part session memory |
| `/clear` | `/cls`, `/reset` | Clear the current terminal chat viewport buffer |
| `/undo` | — | **Time-Travel Rollback**: Instantly revert all file modifications from the previous turn via Git shadow tree snapshots |
| `/diff` | — | Display a side-by-side or unified diff of all file changes made in the session |
| `/cost` | `/tokens`, `/usage` | Inspect real-time token consumption, context headroom, and estimated USD spend |

### 🎭 Operating Modes & Custom Agents
| Command | Aliases | Description |
| :--- | :--- | :--- |
| `/mode [name]` | — | Inspect current mode or switch to `BUILDER`, `PLANNER`, or `MINER` |
| `/builder` | — | Switch active mode to `BUILDER` (autonomous code modification and tool execution) |
| `/plan` | `/planner` | Switch active mode to `PLANNER` (read-only architectural planning and review) |
| `/plan clear` | `/plan reset` | Archive the active plan from `.brocode/current_plan.md` to `.brocode/plans/archive/` |
| `/mine` | `/miner` | Switch active mode to `MINER` (knowledge extraction & memory persistence) |
| `/agent [name]`| `/agents` | List or switch custom agent personas defined in `.brocode/agents/*.md` |

### 🧠 Code Intelligence & Diagnostics
| Command | Aliases | Description |
| :--- | :--- | :--- |
| `/diagnose` | `/scan`, `/lint` | Run project-wide LSP diagnostics and display categorized error/warning lists |
| `/diagnose fix`| `/scan fix` | Automatically apply safe LSP quick-fixes, import organizes, and renames |
| `/lsp` | — | Inspect connected Language Server Protocol managers, active servers, and install hints |
| `/deps` | `/dep-resolve` | Run stack-agnostic dependency resolver to detect and install missing packages |
| `/spec [task]` | `/spec-plan` | Execute architectural blueprint engine before writing code |
| `/tournament [task]` | `/battle` | Run multi-solution tournament benchmarking to evaluate algorithmic trade-offs |
| `/repair [task]` | `/fix` | Run autonomous root-cause analysis and automated test-driven repair |

### 🔐 Security, Provenance & Tooling
| Command | Aliases | Description |
| :--- | :--- | :--- |
| `/provenance` | `/verify` | Verify the cryptographic Merkle chain and tamper-evident AI SBOM signature |
| `/trace` | — | Trace provenance origins, timestamps, and model hashes for all touched files |
| `/mcp` | — | Open Model Context Protocol manager (`Enter` inspect tools, `a` add wizard, `d` delete, `r` reload) |
| `/debug` | — | Display runtime engine metrics, memory allocation stats, and goroutine health |
| `/skills` | `/skill` | List loaded system skills and active project-level skill extensions |
| `/memory` | `/mem` | View persistent project memory, captured gotchas, and architectural rules |
| `/ask [query]` | — | Launch an ephemeral prompt execution without polluting session history |

### ⚙️ Providers & Credentials
| Command | Aliases | Description |
| :--- | :--- | :--- |
| `/connect` | `/c` | Launch interactive provider configuration wizard (API keys, endpoints, custom gateways) |
| `/models` | `/m`, `/model` | Open interactive model selector modal with real-time fuzzy search |
| `/search-key [k]`| `/tavily-key` | Configure credentials for high-speed documentation search (Exa / Tavily) |
| `/context7-key [k]`| `/c7-key` | Configure Context7 documentation resolver credentials |
| `/copy` | `/y` | Copy the last assistant response directly to OS clipboard |
| `/mouse` | — | Toggle mouse mode between `SCROLL` (wheel scrolling) and `SELECT` (drag-select text) |
| `/update` | `/upgrade` | Check for and install the latest BroCode release binary |
| `/quit` | `/exit`, `/q` | Terminate BroCode and exit cleanly |

---

## 3. Keyboard Shortcuts

| Shortcut | Scope | Action |
| :--- | :--- | :--- |
| `Shift + Tab` | Global | Cycle operating modes (`BUILDER` ➔ `PLANNER` ➔ `MINER`) |
| `Ctrl + K` / `Alt + K` | Input / Queue | Open **Prompt Queue Management** (`e` edit, `d` delete, `m` switch mode, `K`/`J` reorder) |
| `Ctrl + P` | Viewport | Toggle **In-TUI Full-Answer Pager** (`PgUp`/`PgDn`/`↑`/`↓`/`Home`/`End` to scroll, `q`/`Esc` to exit) |
| `Ctrl + Y` | Global | **Copy Last Response** directly to system OS clipboard |
| `Ctrl + M` | Global | Toggle **Mouse Mode** (`SCROLL` for mouse wheel vs `SELECT` for native drag-selection) |
| `Ctrl + F` | Chat Log | Toggle **Live Diff Expansion** between compact summary rows and full colorized diffs |
| `Ctrl + U` / `Alt + ⌫`| Input | Clear the entire prompt input line instantly |
| `Ctrl + V` | Input / Modals | Paste system clipboard content directly (supports multi-line prompt pastes) |
| `1` – `9` | Empty Input | Trigger interactive **Senior Recommendations** numbered 1 to 9 |
| `↑` / `↓` | Input / Autocomplete | Navigate prompt history or scroll through suggestion popup window |
| `Tab` / `Enter` | Autocomplete | Select and insert active suggestion |
| `Esc` | Global | Cancel suggestion popup, close open modal, or interrupt active turn execution |
| `Ctrl + C` | Global | Cleanly terminate session and gracefully release in-flight goroutines |

---

## 4. Environment Variables

| Variable | Description |
| :--- | :--- |
| `ANTHROPIC_API_KEY` | API key for Anthropic models (`claude-3-7-sonnet`, `claude-3-5-sonnet`, `claude-3-5-haiku`) |
| `OPENAI_API_KEY` | API key for OpenAI models (`gpt-4o`, `o1`, `o3-mini`, `gpt-4o-mini`) |
| `DEEPSEEK_API_KEY` | API key for DeepSeek models (`deepseek-chat`, `deepseek-reasoner`) |
| `POOLSIDE_API_KEY` | API key for Poolside/Laguna models (`laguna-s-2.1`) |
| `OPENROUTER_API_KEY`| API key for OpenRouter models |
| `GROQ_API_KEY` | API key for Groq ultra-low-latency models |
| `EXA_API_KEY` | (Optional) API key for Tier 1 Exa web search |
| `TAVILY_API_KEY` | (Optional) API key for Tier 2 Tavily web search |
| `CONTEXT7_API_KEY` | (Optional) API key for Context7 live documentation resolver |
| `OLLAMA_HOST` | (Optional) Host URL for local Ollama instance (default: `http://localhost:11434`) |
| `BROCODE_COMPACT_MODEL` | (Optional) Model override specifically for background context compaction |
| `BROCODE_SWARM_CHEAP_MODEL` | (Optional) Model override for swarm subagents |
| `BROCODE_TOOL_DESC_BUDGET` | (Optional) Max token budget for tool schema definitions |
