# BroCode CLI & Command Reference

> **Version**: v0.1.36  
> Complete reference guide for CLI flags, interactive commands, operating modes, and environment variables.

---

## 1. CLI Flags

```bash
brocode [flags] [prompt]
```

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `-v`, `--version` | bool | `false` | Print BroCode version, commit, and build date, then exit |
| `-c`, `--continue`| bool | `false` | Resume the most recent active session |
| `-session` | string | `""` | Resume a specific session by its session ID |
| `-provider` | string | `""` | Override LLM provider ID (`anthropic`, `openai`, `deepseek`, `poolside`, `ollama`, `openrouter`, `groq`) |
| `-model` | string | `""` | Override model name (e.g. `claude-3-7-sonnet`, `gpt-4o`, `deepseek-chat`) |
| `-budget` | float64| `0.0` | Per-task spending cap in USD (0 = unlimited) |
| `-replay` | string | `""` | Replay a session trajectory to stdout without running LLM or TUI |
| `-log` | string | `""` | Write real-time activity events to a log file |

---

## 2. Interactive Slash Commands

Type `/` in the terminal input box to open the autocomplete suggestion popup.

| Command | Arguments | Description |
| :--- | :--- | :--- |
| `/help` | — | Display help text and active shortcuts |
| `/undo` | — | **Time-Travel Rollback**: Instantly revert all file modifications from the previous turn via Git shadow snapshots |
| `/sessions` | — | Open the session manager modal (`d` to delete, `D` to delete all) |
| `/models` | — | Open the interactive model picker with dynamically discovered models |
| `/connect` | — | Launch the interactive provider & API key setup wizard |
| `/memory` | — | View cross-session project memory and captured gotchas |
| `/cost` | — | Inspect token consumption and estimated spend breakdown |
| `/lsp` | — | View Language Server Protocol connection status and language diagnostics |
| `/diagnose` | — | Run self-contained syntax and type diagnostics on modified files |
| `/miner` | — | Switch active mode to MINER |
| `/new` | — | Start a clean conversation session |
| `/clear` | — | Clear the chat viewport |

---

## 3. Keyboard Shortcuts

| Shortcut | Context | Action |
| :--- | :--- | :--- |
| `Shift + Tab` | Global | Cycle operating modes (`BUILDER` &rarr; `PLANNER` &rarr; `MINER`) |
| `↑` / `↓` | Input / Popup | Navigate prompt history or scroll autocomplete suggestions |
| `Tab` / `Enter` | Autocomplete | Select and insert active suggestion |
| `Esc` | Global / Autocomplete | Dismiss popup or interrupt running turn |
| `PgUp` / `PgDn` | Chat Viewport | Scroll conversation history |
| `Ctrl + C` / `q` | Global | Exit BroCode |

---

## 4. Environment Variables

| Variable | Description |
| :--- | :--- |
| `ANTHROPIC_API_KEY` | API key for Anthropic models |
| `OPENAI_API_KEY` | API key for OpenAI models |
| `DEEPSEEK_API_KEY` | API key for DeepSeek models |
| `POOLSIDE_API_KEY` | API key for Poolside/Laguna models |
| `OPENROUTER_API_KEY`| API key for OpenRouter models |
| `GROQ_API_KEY` | API key for Groq models |
| `EXA_API_KEY` | (Optional) API key for Tier 1 Exa web search |
| `TAVILY_API_KEY` | (Optional) API key for Tier 2 Tavily web search |
| `OLLAMA_HOST` | (Optional) Host URL for local Ollama instance (default: `http://localhost:11434`) |
