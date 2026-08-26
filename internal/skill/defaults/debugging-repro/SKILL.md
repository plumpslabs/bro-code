---
name: debugging-repro
description: Reproduce-first debugging for bug reports — run the failing command/test, observe the failure, fix, re-verify
version: 1
---

# Debugging / Reproduce-First

| Task | Action |
|---|---|
| Bug report / crash / regression | ✅ Load this skill |
| Feature implementation | ❌ Skip — universal contract |
| Lint / diagnostics only | ❌ Skip — universal contract |

**Use ONLY when the user reports a bug, crash, regression, or "not working" behavior** — never for feature work, diagnostics-only tasks, or lint-cleanup (those follow the universal contract).

## Reproduce before editing
1. Run the failing command or test exactly as reported (or the closest project-native equivalent) and OBSERVE it fail.
2. Record the exact error text — it is your verification baseline and your success criterion.
3. If you cannot reproduce, say so and do NOT edit blind. Ask for the missing steps/context.

## Fix
- Read the failing code path surgically (code_locate + the exact line range) before changing anything.
- Change ONE hypothesis at a time; do not shotgun multiple fixes into one edit.
- If the same error persists after two attempts, stop and re-diagnose — repeating the fix is a loop, not progress.

## Re-verify
- Re-run the exact command from step 1. Passing the same command that previously failed IS the proof.
- Also run the file's neighboring tests — a fix that breaks a sibling test is not done.

## Distill
- When the fix lands, the engine may extract a lesson into project memory — confirm the lesson is specific and true before it is retained.
