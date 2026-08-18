---
name: migration-playbook
description: Run safe code/data migrations — pre-verify state, incremental steps, verify after each step, keep a rollback path
version: 1
---

# Migration Playbook

**Use ONLY when the task is a migration**: schema/data changes, framework upgrades, API reshapes, or moving code across packages. For ordinary edits or bug fixes, skip this skill.

## Before touching anything
1. Map the current state: which files/tables/callers are involved (code_locate + grep for usages).
2. Identify the migration's success criterion — the verification that proves the migration worked.

## Execute incrementally
- One step at a time, smallest first (additive → then renames → then removals). Never do a big-bang rewrite.
- After EACH step: run the verification. A step that breaks the build is a step to revert, not to push through.
- Prefer additive steps that keep old and new working side by side, then remove the old in a final step.

## Rollback
- Before the first edit, ensure the previous state is recoverable (BroCode snapshots + git status).
- If a step fails verification twice with the same error, STOP and re-plan — do not repeat the same fix.

## Data migrations
- Never `DELETE`/`UPDATE` without a `WHERE`; run reads first to confirm row counts.
- For nullable/backfill changes, verify the backfill result before dropping the old column/field.
