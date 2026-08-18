# DIRECTION.md — Product Direction & Agent Doctrine

> Companion to **PHILOSOPHY.md** (why the project exists, resource principles) and
> **TECH_STACK.md** (what stack and why). This document answers the question
> neither covers: *"what kind of agent is BroCode, and how does it behave?"*
> Every behavioral feature added should be consistent with the doctrine here.

**Status**: Draft — captures the design discussion (Aug 2026). Decisions are
locked; implementation is phased (see §Migration).

---

## 1. Executive decision: STANDALONE. 100%.

BroCode does **not** depend on Matcha (or any external agent-discipline layer)
as a runtime component. Matcha's proven ideas are **absorbed natively** into
BroCode's own Go implementation. This is a one-way decision, not a
"for now" — compatibility adapters are only built if a *real, demonstrated
use-case* appears later, never preemptively.

**Why:**

1. **Different purposes.** Matcha is a *productivity/engineering-discipline
   layer for any coding agent*. BroCode is *a full coding agent* that owns its
   whole lifecycle: reasoning → context → risk → tools → execution →
   verification → review. Wrapping one inside the other adds an orchestration
   layer to a tool whose core value is minimalism.
2. **The project's own doctrine forbids it** (PHILOSOPHY.md): single static
   binary, CGO-free, idle RSS < 80MB, no always-on secondary process, suckless.
   A runtime dependency on an npm package + Node.js hook subprocesses violates
   all of it.
3. **Coherence wins.** One runtime, one state, one lifecycle. No
   `agent → matcha → MCP → hook → back to agent` indirection.
4. **Not every piece of Matcha's ceremony is optimal for us.** Matcha ships 7
   checkpoints, 6 agents, hooks, MCP, commands, adapters — sensible for a
   general-purpose layer. For BroCode, the question is: *"does this checkpoint
   actually improve the outcome of this task?"* If not, skip it. That power to
   cut ceremony is exactly the standalone advantage.

**The formula:** take Matcha's proven sequence
(`purpose → reuse → assess → implement → verify → review`), redesign it as
native BroCode behavior, and extend it with what a *live-production* agent
needs: risk, blast radius, dirty-state preservation, rollback, stop
conditions, evidence. Matcha becomes inspiration, not infrastructure.

---

## 2. Identity: cautious senior-engineer agent that happens to be fast

BroCode is **not** "a fast coding agent". Speed is a *consequence* of not
doing stupid work, never of reducing thinking.

> **Never make a change merely because you can.** If the agent cannot explain
> *why* a change is needed, it must not make it.

Senior behavior comes from the **workflow**, not from a persona prompt like
*"You are a senior developer…"* (which models ignore). It is encoded in
decision gates, risk tiers, and stop conditions below.

---

## 3. The BroCode Engineering Loop

```
                    PURPOSE
                       ↓
                    REUSE
                       ↓
                    STACK
                       ↓
              IMPACT / RISK ANALYSIS   ← depth is decided by risk tier
                       ↓
                  PLAN DECISION        ← explicit; recorded for complex tasks
                       ↓
                 IMPLEMENT MINIMAL     ← smallest safe change
                       ↓
                    VERIFY             ← static → build → test → behavior → diff review
                       ↓
                     REVIEW
                       ↓
                      SHIP
```

Risk level determines the depth of *every* stage. A README typo walks the
loop in milliseconds; a production auth change walks it with investigation,
plan, impact analysis, tests, and review.

### 3.1 Fast Path / Deep Path routing

The loop is not a single conveyor belt — every request is first routed by an
**Intent Router** through a **Context Engine** and **Risk Analysis**, then
branches:

```
USER
  │
  ▼
Intent Router
  │
  ▼
Context Engine
  │
  ▼
Risk Analysis
  │
  ├── SIMPLE ──→ FAST PATH (edit → verify → done, instant)
  │
  └── COMPLEX ─→ DEEP PATH (investigate → plan → impact → implement → verify → review)
                  │
                  ▼
                TOOLS
                  │
                  ▼
                VERIFY
                  │
              ┌───┴───┐
              ▼       ▼
            PASS     FAIL
              │       │
              ▼       ▼
            DONE    REPAIR
```

Fast ≠ hurried. Fast = not wasting time on what is unnecessary. Deep = being
deliberately slow up front so the whole task is fast (30s of investigation
beats 2h of repairing a wrong production change).

---

## 4. Core doctrine

### 4.1 Understand → Investigate → Decide → Change → Verify → Review

Never `Prompt → Code → Done`. For a request like *"add caching to the users
endpoint"*, the agent must first find out: current endpoint shape, existing
cache abstractions, whether Redis is already in the stack, whether production
deploys it, invalidation strategy, consistency impact. Then:

```
Decision: existing Redis infra found. Reuse existing cache abstraction. No new dependency.
```

### 4.2 Change Risk Engine

Every change gets a risk score before implementation:

| Tier | Examples | Workflow |
|---|---|---|
| **L0 LOW** | README, formatting, local rename, trivial test | edit → done |
| **L1 MEDIUM** | business logic, API behavior, service changes, DB query | inspect + verify |
| **L2 HIGH** | migration, auth, payment, security, prod config, dependency change, public API, data deletion, deployment | investigate → plan → show impact → implement → verify → review |
| **L3 EXTREME** | irreversible / cross-cutting production changes | L2 + **explicit user approval** |

### 4.3 Blast radius analysis

Before editing, the agent maps what touches the target. Editing
`User.Status` means: API response, auth middleware, DB queries, admin
dashboard, background workers, tests. **Blast radius = HIGH** → do not edit a
single file blindly. This matters more than repository search.

### 4.4 Lightweight dependency graph (lazy)

Minimal chain: `symbol → file → imports → references → tests`. Used to answer
"what is affected by editing X?" and to aim verification at that area.

**Constraint (doctrine/P2):** built lazily on demand, never eagerly at
startup. A full repo graph is a memory and startup-time bomb.

### 4.5 Before-Change Snapshot

Before any task that touches files: record `git status`, `git diff`, current
branch, `HEAD` as a **task checkpoint** (commit hash, dirty files). If the
task fails, roll back *task* changes without touching the user's
pre-existing work.

**Hard rule:** never `git reset --hard` or destructively clean uncommitted
user changes without explicit permission.

### 4.6 Dirty-state preservation

If the target file is already modified by the user before the task started,
the agent must **preserve the existing diff**, not overwrite the file. This
is senior behavior that matters more than a smarter model.

### 4.7 Minimal Change Principle

1 bug → root cause → smallest safe fix → regression test → done. A refactor
of 12 files because of 1 bug requires a *why* that survives scrutiny.

### 4.8 Proportionality (anti-conservatism guard)

The risk engine cuts *both* ways. A README typo must not trigger
architecture analysis. Senior-*thinking*, not senior-looking:

- L0 typo → edit → done (instant).
- L3 production auth → deep investigation (deliberately slow early, fast overall:
  30s of investigation is cheaper than 2h repairing a wrong production change).

### 4.9 STOP conditions

The agent must know when *not* to continue:

- requirement ambiguous
- target behavior unclear
- production impact unknown
- migration irreversible
- security assumption uncertain
- existing implementation conflicts
- tests unavailable for a risky change
- unexpected files affected

Behavior: *"I found X. Before continuing I need to confirm Y."* — better than
fake confidence.

### 4.10 Evidence before claim

Claims must be backed by search. "Redis is not used in this project" is only
said after searching for `redis`, `go-redis`, `cache`, `docker-compose`, env
vars. Otherwise: *"I couldn't find an existing Redis integration."* The
wording difference is the senior signal.

### 4.11 Decision Records (continuity)

For complex tasks, persist a task record (matches the existing
`.agents/plan/current.md` pattern):

```
Goal:
Constraints:
Findings:
Decision:
Risk: HIGH
Verification:
```

If context is lost, the agent does not start from zero.

### 4.12 Migrations are special

`ALTER TABLE …` is auto-classified **HIGH**. Internal checklist:
backward compatibility, existing data, nullable/default, index impact,
locking, rollback, deploy order. Never a bare `ALTER TABLE`.

### 4.13 Layered verification

Test-passed ≠ done. Layers:

```
CHANGE → STATIC → BUILD → TEST → BEHAVIOR → DIFF REVIEW
```

- DB: migration syntax → up → down → affected queries.
- API: compile → unit → integration → contract.

---

## 5. What we take from Matcha vs what we reject

| Absorb (natively, in Go) | Reject (runtime coupling) |
|---|---|
| Purpose/Intent discovery | npm package dependency (`@plumpslabs/matcha`) |
| Reuse-before-write discipline | Node.js hook subprocesses (planning gate, shield, post-write) |
| Verify-before-done gate | Matcha philosophy embedded in the system prompt (token tax) |
| `// matcha:` decision markers (as a convention) | "matcha" as a built-in agent mode |
| `.agents/` + skills + AGENTS.md conventions (ecosystem standard) | auto-install side effects at startup |
| L0–L3 tiering (matches existing intensity levels) | hooks/MCP/commands as required orchestration |

---

## 6. Resource allocation (when time is limited)

| Effort | Area |
|---|---|
| **40%** | Context + Repository Intelligence (blast radius, lazy dep graph, evidence) |
| **25%** | Orchestration + Agent Loop (risk engine, stop conditions, decision records) |
| **15%** | Verification + Risk (layered verify, snapshots, rollback) |
| **10%** | Performance + Memory (budget, lazy everything) |
| **10%** | TUI / UX |

Explicitly **not** the alternative shape (MCP 30% / plugins 20% / subagents
20% / compatibility 20% / core 10%) — that makes the feature list big but the
core ordinary.

---

## 7. Migration from current Matcha coupling

Reality check (verified in code, Aug 2026): BroCode is currently **deeply
coupled** to Matcha. Phase 0 is *unwinding*, not *building*.

| Phase | Action | Effort |
|---|---|---|
| **P0** | Remove the startup `npm install -g @plumpslabs/matcha` side effect (`app.go` `New()`). No network at startup, ever. | trivial |
| **P1** | Remove `EnsureGlobalSetup` runtime installs + registry fetch (`session.go`); stop writing matcha agent files into `~/.brocode/`. | small |
| **P2** | Replace Node.js hook subprocesses (`matcha_hooks.go`) with native Go checks (or make them explicitly opt-in), so the binary has zero Node dependency. | medium |
| **P3** | Rework `agent.go` system prompt: absorb the engineering philosophy as native BroCode persona; drop the "matcha" branding + hardcoded subagent list. | small |
| **P4** | Remove/repurpose the `matcha` agent mode + `/matcha*` commands; keep only the conventions (`// matcha:` markers, `.agents/` files) as text. | small |
| **P5** | Add the senior-engineer doctrine features (risk engine, blast radius, snapshots, stop conditions) as native modules. | large — the real work |

**Keep, unchanged:** AGENTS.md / `.agents/` / skills / MCP interop — those are
ecosystem conventions this project explicitly preserves (PHILOSOPHY.md).

---

## 8. Alignment with PHILOSOPHY.md

| Doctrine here | Existing principle |
|---|---|
| Risk engine / STOP conditions | P4 — preventive, not reactive; fire before the problem |
| Lazy dependency graph / evidence search | P2 — lazy + relevance-scored loading |
| Evidence before claim | P3 — one source of truth; honest numbers & claims |
| Before-change snapshot / rollback | P1/P5 — bounded state, explicit retention |
| Layered verification | P7 — boundedness is tested, not assumed |
| Minimal change / proportionality | Worse-is-better; suckless |
| Standalone, no Node/npm coupling | Single static binary; no always-on process |
| Absorbed (not embedded) philosophy | North star: productive token ratio — no token tax |

---

## 9. What differentiates BroCode

Not "BroCode has 50 tools" — but:

> **BroCode knows when NOT to use a tool.**

Not "BroCode can edit 20 files at once" — but:

> **BroCode knows when editing 20 files is dangerous.**

Not "BroCode always finishes the task" — but:

> **BroCode knows when the safest answer is to stop and ask.**

For someone running live production projects, that is worth more than
maximizing autonomous behavior.

## 10. Target architecture (component view)

How the doctrine maps onto actual components — everything native, one
runtime:

```
                    🍵 BROCODE
                          │
        Senior Engineering Behavior
                          │
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
   Context Engine     Risk Engine       Intent Engine
   (blast radius,     (L0–L3 tiers,     (purpose, reuse,
    dep graph,         stop conditions)  fast/deep routing)
    evidence)
        │                 │                 │
        └─────────────────┼─────────────────┘
                          ▼
                    Orchestrator
                          │
                   ┌──────┴──────┐
                   ▼             ▼
               Fast Path     Deep Path
                   │             │
                   └──────┬──────┘
                          ▼
                        Tools
                          │
                          ▼
                        Verify
                          │
                   ┌──────┴──────┐
                   ▼             ▼
                  PASS          FAIL
                   │             │
                   ▼             ▼
                  Ship         Repair
```

## 11. The one-sentence identity

> Fast because it wastes nothing. Senior because it knows when *not* to act.
> And on risky changes it is deliberately slow at first, so it is fast as a
> whole — 30 seconds of investigation beats 2 hours repairing a wrong change
> to production code.

---

## 12. Positioning: highest engineering throughput, not most features

BroCode is **not** "a smaller OpenCode" and not "the most complete agent".
Its position:

> The agent with the **highest engineering throughput per unit of
> context/time** — the fastest path from *task → working code*.

**Why not "smaller OpenCode":** OpenCode already ships primary agent +
subagents (Explore/General/Plan/Build), on-demand skills, compaction,
permissions — it already separates Explore as a fast read-only agent and
General for multi-step/parallel work. Copying that structure gives people no
reason to switch.

**Positioning vs the ecosystem (from the design discussion):**

| | OpenCode | Claude Code / Codex | BroCode |
|---|---|---|---|
| Focus | Flexible / extensible | Agentic coding ecosystem | **Fast execution** |
| Features | Many | Many | **Core only** |
| Planning | Can be explicit | Can | **Adaptive** |
| Subagents | Many roles | Many workflows | **Only when ROI is clear** |
| Context | Powerful | Powerful | **Minimal relevant context** |
| Tools | Many / extensible | Many | **Few high-quality primitives** |
| Model | Agnostic | Ecosystem-specific | **Agnostic** |
| Runtime | Feature-rich | Feature-rich | **Go-native** |
| Philosophy | General purpose | Autonomous coding | **Performance-first coding agent** |

**Sweet spot = medium tasks, done dozens of times a day:**

- "fix this" / "why is this error?" / "add an endpoint"
- "change the login flow" / "make a migration" / "update this API" / "add a test"
- "refactor this"

Saving 20–40s per task × 30–50 tasks/day beats winning one giant benchmark
task. Target: *"the agent that is nicest to use 50 times a day."*

**Daily workflow map (what BroCode optimizes):**

| Workflow | General-purpose agent | BroCode ideal |
|---|---|---|
| Ask about code | good | **very fast** |
| Find an implementation | good | **very fast** |
| Small fix | good | **super fast-path** |
| Bug debugging | good | **evidence → reproduce → fix → test** |
| Refactor | good | **targeted planning** |
| Medium feature | good | **plan → implement → verify** |
| Large feature | subagent/planner | **adaptive orchestration** |
| 100k+ LOC codebase | context-heavy | **repository-aware retrieval** |
| Long session | compaction | **bounded memory + checkpoint** |
| Repetitive daily work | sometimes overhead | **this is the primary target** |

| Scale of repo | What matters |
|---|---|
| ~10k LOC | trivial |
| ~100k LOC | find-better starts to matter |
| ~500k LOC | context engine is essential |
| 1M+ LOC | retrieval/indexing is critical |

**The three biggest investments, in order:** Context Engine +
Verification Engine + Orchestrator. UI is number four. (Confirms §6.)

---

## 13. Feature philosophy: 20% features, 80% correct workflow

Do not chase feature parity. If OpenCode has 100 features, BroCode does not
need 101 — it needs ~20% of the features and 80% *correct workflow*.

**The core is exactly 5 primitives:**

```
read · search · edit · shell · verify
```

Everything else (git, docker, database, browser, MCP, subagents) is an
*extension* — lazy-loaded only when a real need appears. If a feature is
never used (e.g. browser agent), it does not exist. Optimize the five core
primitives until they are genuinely excellent.

---

## 14. Roadmap V0.1 → V0.5 (phased; not 50 features at once)

| Version | Focus | Contents |
|---|---|---|
| **V0.1 Core** | run the loop | TUI, agent loop, provider, read, grep/glob, edit, shell, streaming |
| **V0.2 Intelligence** | think | task classifier, context retrieval, fast path, complex path, verification, repair loop |
| **V0.3 Performance** | be fast | parallel tools, cache, incremental context, bounded workers, cancellation, memory profiling |
| **V0.4 Reliability** | be safe | permissions, diff preview, rollback, session persistence, failure recovery |
| **V0.5 Advanced** | only if proven | subagents, skills, MCP, background tasks, multi-model routing |

Research cited in the discussion shows context files are far more common in
practice than skills/subagents, and advanced mechanisms are used shallowly.
So subagents/MCP are **never the foundation** of V0.1–V0.3.

---

## 15. The 7 core tuning rules (constitution)

Everything in AGENT_LOOP.md exists to serve these:

1. **Fast by default.**
2. **Smart when necessary.**
3. **Minimal context.**
4. **Minimal tools.**
5. **Parallel when safe.**
6. **Verify automatically.**
7. **Never let memory grow unbounded.**

---

## 16. Companion documents

- **AGENT_LOOP.md** — the agent runtime design: reactive loop, task router,
  fast/smart model routing, intent contract, context engine, tool
  orchestration, verification & repair, memory HOT/COLD, TUI rendering, Go
  tuning, target architecture.
- **BENCHMARK.md** — the scorecard, `bench/` layout, methodology, and the
  "don't claim efficiency without measuring" rule.
