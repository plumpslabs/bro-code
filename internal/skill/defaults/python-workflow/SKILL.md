---
name: python-workflow
description: Verify and fix Python projects — pyproject.toml-aware toolchain (uv/poetry/pip), pytest, type hints, and common Python gotchas
version: 1
---

# Python Workflow

**Use ONLY when this is a Python codebase** (pyproject.toml, setup.py, setup.cfg, requirements.txt, or *.py sources). If none exist, it is NOT a Python project — skip this skill and let the universal contract apply.

## First: detect the toolchain
- Read pyproject.toml / lockfiles to find the manager: `uv.lock` → uv, `poetry.lock` → poetry, `Pipfile.lock` → pipenv, else pip.
- Run every command with the detected manager (`uv run pytest`, `poetry run pytest`, `pytest`) — never assume the environment is activated.

## Verification (source of truth)
- Tests: `pytest` (or the project's own test script/config in pyproject.toml).
- Typecheck: only if the project configures one (`mypy`/`pyright` in pyproject.toml or CI) — otherwise the tests are the source of truth.
- Fix type errors first when the project runs a type checker — they are blockers.

## Diagnostics
- `lsp_scan`/`lsp_diagnostics` cover pyright/pylsp errors (unused imports, type errors) — that IS your linter. Do NOT install a linter mid-task.

## Edits
- Respect the project's Python version (`requires-python` in pyproject.toml) — do not use syntax newer than it.
- Type hints at API boundaries (function params/returns); infer inside. Keep imports at module top.
- When the package uses explicit `__init__.py` exports, add new public names there.

## Gotchas
- A `.venv` exists? Never run bare `pip install` — use the project's manager so the environment stays consistent.
- Import errors after adding a file usually mean a missing `__init__.py` or a package-relative import that should be absolute.
- Don't "fix" a failing test you did not touch without checking whether it was already failing before your change.
