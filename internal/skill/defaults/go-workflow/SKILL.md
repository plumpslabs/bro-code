---
name: go-workflow
description: Verify and fix Go projects — build, vet, test with the project's own toolchain, gopls diagnostics, and common Go gotchas
version: 1
---

# Go Workflow

| Task | Action |
|---|---|
| Go build/vet/test error | ✅ Load this skill |
| Go file edit | ✅ Load this skill |
| Non-Go project | ❌ Skip — universal contract |

**Use ONLY when this is a Go codebase** (go.mod, go.work, or *.go files under cmd//internal/). If the repo has no go.mod, it is NOT a Go project — skip this skill and let the universal contract apply.

## Verification (source of truth)
- Run the project's own toolchain first: `go build ./...`, then `go vet ./...`, then the relevant tests (`go test ./...` or a focused package).
- `go test` failing? Read the failure, fix, re-run just that package — do NOT re-run the whole suite after every edit.

## Diagnostics
- `lsp_scan` is the linter: gopls already covers go vet + type errors + deprecated + unused. Do NOT `go install` golangci-lint/staticcheck/revive.
- If LSP is unavailable, rely on `go vet`/`go build` and OFFER to run `/lsp-install` for the user (or propose it via ask_user) — do not merely instruct them to do it manually.

## Edits
- Fix type errors before anything else — treat them as blockers.
- Run `gofmt` conventions manually: tabs for indentation, no trailing whitespace, error strings lowercase without punctuation.
- Keep error wrapping idiomatic: `fmt.Errorf("...: %w", err)` — never swallow errors with `_ =`.

## Gotchas
- `go mod tidy` after adding/removing imports; verify `go.sum` is committed.
- Interface satisfaction errors usually mean a missing method — check the full method set, not just the reported line.
