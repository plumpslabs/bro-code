# bro-code

An AI coding agent CLI — single static binary, terminal-native, headless-capable.

Built lean by design: transparent resource usage, architectural efficiency,
and a performance budget enforced in CI. No embeddings, no background
daemons, no bloat.

## Quick start

```bash
make build
./bin/brocode          # interactive TUI (landing screen → chat)
./bin/brocode -c       # resume your last session (~/.brocode/sessions)

# Headless (CI/automation)
./bin/brocode --search mcp
./bin/brocode --diff
./bin/brocode --version
```

Inside the TUI: type a query (e.g. `mcp`, `diff`, `memory`) or a command
(`/connect`, `/search`, `/diff`, `/agents`, `/mcp`, `/usage`, `/theme`,
`/clear`). A fresh start shows a centered pixel wordmark with the input as
its own form; typing `/` opens a live command suggestion popup (`↑↓` to
navigate, `tab`/`enter` to accept). Messages are clean text with no
backgrounds: user messages carry a theme-colored vertical bar on the left
(`│ halo pagi`), agent responses are plain colored text. Diff hunks and
thinking traces collapse by default and expand with `ctrl+o`. Scrolling always works — `↑↓`/`pgup`/`pgdown` or the mouse
wheel, no tab-focus dance needed (the input is always the typing surface).
`/connect` shows the provider picker (opencode, antigravity, claude,
deepseek — UI only for now, but choosing one sizes the context-window
display); `/theme` opens a picker with live color swatches (or
`/theme <name>` to set directly). `?` shows help, `q`/`ctrl+c` quits.
Sessions persist automatically on quit and resume with `-c`.

The right-hand status panel is a live transparency dashboard: the current
model + context window + token usage (estimate until the provider layer
lands, Principle 3), the git branch + path relative to the repo root, MCP
filter state + connected servers, sub-agents, and recent tool activity.

## Development

```bash
make test        # go test ./...
make check       # go vet + gofmt check
make build       # CGO-free, stripped, version-stamped single binary
make measure     # binary size + startup time
```

## Project layout

```
cmd/brocode/      entrypoint (TUI + headless share one pipeline)
internal/tui/     Bubble Tea UI
internal/search/  BM25 relevance (hand-rolled, zero deps)
internal/diff/    Myers diff (hexops/gotextdiff)
```

## Philosophy

This project exists because mainstream AI coding tools are slow and
RAM-hungry. The guiding principles: bounded buffers by default, lazy +
relevance-scored loading, tracking numbers that are honest and cache-aware,
preventive (never reactive) budget checks, explicit TTL on all state, and
every claim of "efficiency" verified by measurement.

## License

MIT — see [LICENSE](LICENSE).
