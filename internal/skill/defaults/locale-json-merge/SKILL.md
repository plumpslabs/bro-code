---
name: locale-json-merge
description: Merge new keys into existing locale/translation JSON files without duplicates, structural damage, or drift between locales
version: 1
---

# Locale JSON Merge

| Task | Action |
|---|---|
| Edit translation/locale JSON | ✅ Load this skill |
| Merge new i18n keys | ✅ Load this skill |
| General code edits | ❌ Skip — universal contract |

**Use ONLY when editing translation/locale JSON files** (e.g. locales/id.json, en.json) or nested JSON config — never for general code edits.

## Before editing
1. Read the target file's structure (grep for the parent key or `code_locate` it) — find the EXISTING object you must merge into.
2. Check sibling locale files: a new key must be added to EVERY locale the project ships (or the UI falls back to a default language).

## Merging
- Add the new key inside the existing parent object — NEVER duplicate the parent object or create a second copy of the file.
- Keep the same nesting depth and key order conventions as the rest of the file.
- After merging, validate the file is still parseable JSON (the tool layer validates automatically — fix any structural error it reports).

## Gotchas
- Duplicate keys in JSON: the last one wins silently — never write a key that already exists elsewhere in the file.
- Preserve non-translation fields (comments are not valid JSON — do not add them).
- If the target key already exists with different content, UPDATE in place rather than adding a sibling.
