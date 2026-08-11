# bro-code

An AI coding agent CLI — single static binary, terminal-native, headless-capable.

Built lean by design: transparent resource usage, architectural efficiency,
and a performance budget enforced in CI. No embeddings, no background
daemons, no bloat.

## Quick start

```bash
make build
./bin/brocode

# Headless (CI/automation)
./bin/brocode --search mcp
./bin/brocode --diff
./bin/brocode --version
```

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
