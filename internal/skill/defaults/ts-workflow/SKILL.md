---
name: ts-workflow
description: Verify and fix TypeScript/JavaScript projects — tsc --noEmit, package-manager-aware test runs, LSP diagnostics
version: 1
---

# TypeScript / JavaScript Workflow

**Use ONLY when this is a TypeScript/JavaScript codebase** (package.json with *.ts/*.tsx/*.js sources). If the repo has no package.json, it is NOT a TS/JS project — skip this skill and let the universal contract apply.

## First: detect the package manager
- Read package.json and lockfiles: `bun.lockb`/`bun.lock` → bun, `pnpm-lock.yaml` → pnpm, `yarn.lock` → yarn, `package-lock.json` → npm.
- Run every command with the detected manager (`bun test`, `pnpm test`, `npm test`) — never assume npm.

## Verification (source of truth)
- Typecheck: `tsc --noEmit` (or the script in package.json, e.g. `bun run typecheck`).
- Then the project's test script. Fix type errors first — they are blockers.

## Diagnostics
- `lsp_scan`/`lsp_diagnostics` cover tsserver errors, unused variables, and deprecations — that IS your linter. Do NOT install eslint mid-task.

## Edits
- Respect the module system: if the project uses ESM (`"type": "module"`), use `import`/`export`, not `require`.
- Keep types explicit at API boundaries (function params, returns); infer inside.
- When adding to an object/interface, MERGE into the existing declaration — never duplicate keys.

## Gotchas
- `tsc --noEmit` failing on a file you did not touch? Check for a stale build artifact or an unrelated pre-existing error before "fixing" it.
- Import path aliases come from tsconfig `paths` — resolve imports the way the project already does.
