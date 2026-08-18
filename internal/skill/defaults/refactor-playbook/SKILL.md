---
name: refactor-playbook
description: Safe refactoring — map usages first, use lsp_rename for symbol renames, keep behavior identical, verify after each stage
version: 1
---

# Refactor Playbook

**Use ONLY when the task is a refactor**: renaming symbols, extracting helpers, restructuring packages, or simplifying logic. Never for adding features or fixing bugs — those follow the universal contract.

## Rules of the game
- A refactor changes STRUCTURE, not behavior. The test suite is your contract — it must stay green throughout.
- Small, behavior-preserving steps; verify after each. Never refactor and add features in the same edit.

## Renaming symbols
- Use `lsp_rename` for project-wide symbol renames (it updates all references atomically). For a single-file rename, `edit_file` is fine.
- After any rename: typecheck + run the affected tests.

## Extracting / restructuring
- Extract a helper only at 3+ uses (or when it materially clarifies). Keep files under ~300 LOC.
- When moving code across packages, update the import graph in the same pass and verify per move.

## Verification per stage
- After each stage: `go build`/`tsc --noEmit` (whichever applies) + the focused tests.
- Stop if the same error persists across attempts — change approach, do not repeat the fix.
