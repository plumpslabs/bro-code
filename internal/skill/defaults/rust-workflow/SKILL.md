---
name: rust-workflow
description: Verify and fix Rust projects — cargo check/test with the project's own toolchain, rust-analyzer diagnostics, and common Rust gotchas
version: 1
---

# Rust Workflow

| Task | Action |
|---|---|
| Rust build/test/clippy error | ✅ Load this skill |
| Rust file edit | ✅ Load this skill |
| Non-Rust project | ❌ Skip — universal contract |

**Use ONLY when this is a Rust codebase** (Cargo.toml at the root or workspace). If the repo has no Cargo.toml, it is NOT a Rust project — skip this skill and let the universal contract apply.

## Verification (source of truth)
- Run the project's own toolchain first: `cargo check` (fast, no codegen) or `cargo build`, then the relevant tests (`cargo test` or a focused test by name).
- In a workspace, verify only the affected crate: `cargo check -p <crate>`.
- `cargo test` failing? Read the failure, fix, re-run just that test (`cargo test <name>`) — do NOT re-run the whole workspace after every edit.

## Diagnostics
- `lsp_scan`/`lsp_diagnostics` cover rust-analyzer errors (borrow checker, type errors, unused) — that IS your linter. Do NOT install clippy mid-task; if the project already runs `cargo clippy`, use it as configured.
- If LSP is unavailable, rely on `cargo check` and OFFER to run `/lsp-install` for the user (or propose it via ask_user) — do not merely instruct them to do it manually.

## Edits
- Fix borrow-checker and type errors first — they are blockers.
- Follow rustfmt conventions: 4-space indent, `snake_case` for functions/variables, `CamelCase` for types, `SCREAMING_SNAKE_CASE` for consts.
- Error handling: return `Result`/`Option` and propagate with `?` — never `panic!`/`unwrap()` in library code.

## Gotchas
- `Cargo.lock` is committed for binaries, usually ignored for libraries — follow what the repo already does.
- A borrow error usually means the borrow checker is right: restructure ownership (clone, restructure, or a lifetime) instead of fighting it with `unsafe`.
- Edition differences matter (`edition = "2021"` vs `"2024"`): some syntax/features are edition-gated — check Cargo.toml before using new syntax.
