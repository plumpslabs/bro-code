# AGENT_LOOP.md — Agent Loop & Architecture Doctrine

> The concrete runtime design behind **DIRECTION.md**. Where DIRECTION.md says
> *"what kind of agent BroCode is"*, this document says *"how the agent
> runtime actually works"* — the loop, the routing, the context engine, the
> tool orchestration, the verification, and the resource control.
> **Status**: Draft — recorded from design discussion (Aug 2026) as the
> reference for the V0.x build and any future overhaul.

---

## 1. Reactive loop by default — no planner ritual

The default loop for coding is **reactive**, not a long planner → executor →
reviewer chain (which burns tokens and latency):

```
User → fast context scan → LLM → tool call → tool result → LLM → tool call → … → Final
```

Planning is only activated when the task is genuinely complex:

| Task | Loop |
|---|---|
| simple | direct agent |
| medium | light planning |
| complex | planner → implement → verify |

---

## 2. Task Router — adaptive orchestration

Complexity decides the workflow; the user never picks a mode per request.

```
TASK
  │
  ▼
Task Router
  │
  ├── FAST    → edit → verify → done        (minimal latency)
  ├── NORMAL  → inspect → implement → verify
  └── COMPLEX → PLAN → implement → verify → review
```

**Complexity score** (adaptive planning, not a ritual planner):

| Score | Action |
|---|---|
| 0–3 | direct |
| 4–6 | light plan |
| 7+ | full plan → implement → verify |

Examples: *"rename foo to bar"* ≈ 1 → direct. *"migrate auth from JWT to
session-based: middleware + schema + tests"* ≈ high → plan.

---

## 3. Two-model mindset: Fast + Smart

Don't send every task to the biggest reasoning model.

| Task | Mode |
|---|---|
| read file, grep, rename variable, simple edit, compile/test, simple bug | **FAST** |
| complex debugging, big refactor, architecture, ambiguous requirement | **SMART** |

Start with **one good default model**; add a FAST/SMART router only once the
core is stable. The router moves a task to SMART only when needed — never
auto-upgrade just because a task *looks* large.

---

## 4. Intent contract & ambiguity handling

Before any action the agent builds an intent record:

```
Intent = MODIFY
Target = authentication
Goal  = session-based auth
Risk  = HIGH
```

If confidence < threshold → **ASK**. Never guess → change 20 files. The agent
has explicit **permission to do nothing**.

The four-rule ambiguity ladder:

| Situation | Behavior |
|---|---|
| clear intent | execute |
| uncertain target | inspect |
| dangerous / broad action | ask |
| ambiguous requirement | clarify |

Example: *"fix database"* → do **not** touch migrations directly. Say:
*"I found an issue in migration X and query Y — do you mean fix the migration
or the query?"*

**Separate conversation from execution** (QUESTION vs ACTION mode): *"why is
this function erroring?"* → inspect → explain. *"fix this function"* → inspect
→ edit → verify. Never take action merely because a possible fix was found.

Research cited in the discussion (Dialogue SWE-Bench): dialogue behavior is a
separate dimension from raw coding ability — a stronger model does not
automatically produce better dialogue. This is a deliberate BroCode
differentiator.

---

## 5. Context Engine — evidence-first, never context-first

The most common reason coding agents fail is bad context, not a dumb model.

> Dump evidence, not the repository.

```
User request → find evidence → relevant symbols/files → LLM reasoning
```

**Not:** `User request → dump repository context → LLM`.

### 5.1 Repository intelligence (cheap metadata, not embeddings)

On first entry into a project, detect and store a *small* metadata file:

- language, framework, package manager
- build / test / lint commands
- architecture hints

```
.brocode/
├── project.json
└── context/
```

### 5.2 Repository map (find better, not read more)

```
package → file → symbol → imports → references → tests
```

Example — *"why does refresh token fail?"* must not wander 80k files:

```
refresh token → symbol search → dependency graph → 5–15 relevant files → LLM
```

```
AuthService
 ├── auth.go
 ├── login.go
 ├── middleware.go
 ├── UserRepository
 └── auth_test.go
```

### 5.3 Minimal context recipe

For *"fix authentication middleware"*:

1. detect project → 2. detect language → 3. locate auth-related files →
4. grep middleware → 5. inspect imports → 6. inspect tests → 7. build minimal context.

LLM receives only `middleware/auth.go`, `handler/login.go`,
`service/auth.go`, `middleware/auth_test.go` — not 500 files.

### 5.4 Incremental context

Never re-send everything every iteration. Keep a **stable context**
(system + project metadata + relevant files) and a **working context**
(recent actions + tool results). Summarize old history:

```
50 messages → summary → 5 messages
```

---

## 6. Tool orchestration — LLM says WHAT, Go says HOW

The LLM must not spend reasoning on deterministic work. It declares:

```
read auth.go
read user.go
grep "JWT"
```

Go's runtime decides the actual scheduling:

```
auth.go ─────┐
user.go ─────┼── parallel
grep JWT ────┘
```

### 6.1 Tool metadata drives scheduling

```go
type ToolMeta struct {
    Name        string
    ReadOnly    bool
    Parallel    bool
    Destructive bool
}
```

- `read A, B, C` → **parallel**.
- `edit A → compile → test` → **sequential** (dependencies).
- `Destructive: true` → never parallel, always gated.

### 6.2 Minimal tool surface

Default set: **read, edit, grep, glob, shell**. That's it. Extra tools
(git, docker, database, browser) are lazy-loaded only when needed — the model
should not weigh 30 tools on every request.

### 6.3 Tool result caps

A grep producing 20,000 lines must not reach the model. The tool layer
enforces caps (e.g. **10 KB / 200 lines**):

```
[output truncated: 10,482 lines]
Use grep/read with a narrower scope.
```

---

## 7. Editing: patch-based, not full-file rewrite

LLM → **patch** → apply → validate. Never LLM → entire file.

Benefits: fewer tokens, faster, safer, easy diff display, easy conflict
detection.

---

## 8. Verification & repair loop

Quality is preserved by **two-phase intelligence** — and phase 1 is not
always expensive:

| Task size | Phases |
|---|---|
| small | search → edit → test |
| medium | search → understand → plan → edit → test |
| large | map → plan → implement → verify → review → repair |

- **PHASE 1 (understand):** understand, search, reason, plan
- **PHASE 2 (change):** change, test, inspect, repair

Quality scales with complexity; every request is never forced through the
heaviest workflow.

Verification is part of the workflow, not an afterthought — it is what keeps
quality high even with a non-frontier model:

```
UNDERSTAND → SEARCH → IMPLEMENT → FORMAT → BUILD/TEST
                                              │
                              PASS ────────────┴────► DONE
                                              │
                                              ▼
                                       DIAGNOSE → REPAIR → TEST
```

**Repair is bounded:** `MAX_REPAIR_ATTEMPTS = 3`. After that, stop and show
the exact error/evidence — never loop forever.

**Targeted tests, not the whole suite:** detect the project (Go → `go test
./...`, TypeScript → `npm test`/`bun test`, Rust → `cargo test`), then use
dependency analysis of the changed files to run only the relevant tests. A
change to `foo_test.go` must not run the entire suite.

---

## 9. Memory management: HOT / COLD

Session memory must not grow without bound:

| Tier | Contents | Where |
|---|---|---|
| **HOT** | current prompt, recent tool calls, active context | RAM |
| **COLD** | session history (older turns) | disk (JSONL/SQLite) |

Long sessions never grow RAM unboundedly — this is the direct fix for the
opencode memory-leak class of problems.

**Cancellation chain** (Ctrl+C):

```
cancel root context → LLM request cancel → tool cancel → subprocess cancel → worker stop → memory reclaim
```

Every long-running job must carry `context.Context` with cancellation and
deadline — this matters more than chasing FPS.

---

## 10. TUI: controlled rendering, never per-token full rebuilds

```
LLM stream → event channel → state update → render
```

**Not:** `token → full UI rebuild → token → full UI rebuild`.

- event-driven rendering with dirty regions — render only what changed
- throttle UI to ~30–60 FPS while LLM tokens can arrive 100+/s
- tool execution is streamed visibly: `→ Reading auth.go`,
  `→ Running tests` — this makes the CLI *feel* fast even with equal latency

---

## 11. Go-specific tuning

Go owns the deterministic layer; the LLM owns reasoning:

| Go (deterministic) | LLM (reasoning) |
|---|---|
| concurrency, scheduling | understanding |
| cancellation, timeout | reasoning |
| filesystem, diff, caching | code generation |
| process execution, session state, tool scheduling | decision making |

- goroutines + channels + `context.Context` + `sync.Pool`
- **bounded worker pool** (e.g. 4 workers), never unbounded `go func()`
- every task wrapped in `context.WithTimeout(...)` so nothing hangs

---

## 12. Target architecture

```
                  ┌───────────────┐
                  │     TUI       │   ← event-driven, throttled render
                  └───────┬───────┘
                          │
                  ┌───────▼───────┐
                  │ Agent Runtime │
                  └───────┬───────┘
                          │
              ┌───────────┼───────────┐
              ▼           ▼           ▼
          Router      Context      Session
              │        Engine     (HOT/COLD)
       ┌──────┴──────┐
       │             │
     FAST          SMART
       │             │
       └──────┬──────┘
              │
          Provider
              │
              ▼
             LLM
              │
           Agent
              │
        Tool Planner
              │
     ┌────────┼────────┐
     ▼        ▼        ▼
   Read     Edit     Search
     │        │        │
     └────────┼────────┘
              ▼
            Verify
              │
        ┌─────┴─────┐
        ▼           ▼
     Success       Fail
        │           │
        ▼           └──→ Repair (max 3)
       DONE
```

Secret sauce is **not** the model. It is:

> Context selection + fast-path routing + deterministic tool orchestration +
> verification + aggressive resource control.

---

## 13. Anti-patterns (explicitly banned)

- Planner → executor → reviewer ritual on every request.
- Eager full-repo context dumps (evidence-first instead).
- 30 tools exposed on every request (5 primitives, lazy extension).
- Full-file rewrites when a patch suffices.
- Unbounded repair loops (max 3 attempts).
- Unbounded goroutines (bounded worker pool instead).
- Session history growing in RAM forever (HOT/COLD split).
- Auto-using the expensive model for cheap tasks (FAST/SMART router).
- Rendering the whole TUI per token (event-driven, dirty regions).
