# BENCHMARK.md — Benchmarking & Scorecard Doctrine

> Companion to **DIRECTION.md** (§12) and **AGENT_LOOP.md**. This is the
> *"don't claim efficiency without measuring"* doctrine: BroCode's identity
> is throughput and efficiency, so those claims must be **numbers**, not
> feelings.
> **Status**: Draft — recorded from design discussion (Aug 2026).

---

## 1. Why: honest claims only

Research cited in the discussion:

- Agent **design** can cause a large performance gap even with the same
  model — on SWE-Bench Mobile, identical agent+model configurations differ by
  up to ~6×.
- Community benchmarks show claimed token savings from tools/context tricks
  do not always hold on real workloads.
- Feature-level tasks are much harder than isolated bug fixes: one
  FeatureBench-style study found a model strong on SWE-bench completing only
  ~11% of its feature-level tasks.

Therefore: *"it feels faster"* is not a metric. From day one BroCode ships a
benchmark harness, or it does not claim efficiency at all.

---

## 2. bench/ layout

```
bench/
├── simple/
├── medium/
├── large/
├── debugging/
├── refactor/
├── feature/
└── regression/
```

Tasks are chosen from the actual daily-work profile (DIRECTION.md §12):
fixes, refactors, endpoints, error diagnosis, migrations, API changes, tests —
not only isolated bug fixes.

---

## 3. BROCODE SCORECARD

Every benchmark run reports at least:

| Metric | Notes |
|---|---|
| **Task Success** | most important |
| Correctness | |
| Time to First Token (TTFT) | perceived speed |
| Time to Done | full task latency |
| Tokens / Task | token efficiency |
| Tool Calls / Task | orchestration overhead |
| Repair Attempts | self-correction quality (bounded ≤ 3) |
| Peak RAM | resource control |
| CPU | |

---

## 4. Methodology

- Run the **same tasks** across BroCode vs OpenCode vs other agents.
- **Same model** when measuring harness/workflow quality (isolates the
  harness). **Different models** = measuring the combined model+harness —
  state which one is being measured, never conflate the two.
- Track over real sessions, not just synthetic samples, because tool/context
  token savings are known to degrade on real workloads.
- The performance budget in PHILOSOPHY.md (startup < 200ms, idle RSS < 80MB,
  flatline, TUI < 16ms/frame) feeds the same scorecard — CI-gated.

---

## 5. Source note

The research references above (SWE-Bench Mobile ~6×, FeatureBench ~11%,
Dialogue SWE-Bench) are cited from the design discussion (Aug 2026). Before
relying on specific numbers in external material, verify the primary source —
consistent with the project's *evidence-before-claim* doctrine
(DIRECTION.md §4.10).
