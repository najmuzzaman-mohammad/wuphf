# Migration plan: WUPHF orchestration → LangGraph (orchestrator-of-record)

> Branch `worktree-deepagents-harness-eval`. 2026-06-19. Base `origin/main` @ `ec467159`.
> Re-aimed after the founder's clarification: the weak, hand-rolled layer is WUPHF's
> **coordination above** Claude Code/Codex, not the inner loop. The inner CLIs are great
> and stay. Companions: [`deepagents-harness-evaluation.md`](./deepagents-harness-evaluation.md)
> (decision trail) and [`../../spikes/deepagents-seam/REPORT.md`](../../spikes/deepagents-seam/REPORT.md)
> (Python↔teammcp-over-MCP proven). This is the artifact the 4-section eng review runs against.

## 1. Scope & decision (D1–D4)

Replace WUPHF's hand-rolled coordination with a **Python LangGraph orchestrator that is the
orchestrator-of-record** (owns task lifecycle + run-state). Keep **Claude Code/Codex** as the
inner execution, invoked from graph nodes. **Go** becomes the durable-store + transport + tools
+ CLI host. deepagents is optional sugar for the CEO decompose/delegate node.

**Replace (→ LangGraph):** the agent tick/turn-loop (`internal/agent`), and the coordination
logic in `internal/team` — CEO decomposition, multi-agent sequencing, dependency ordering,
the lifecycle state machine, scheduling, escalation.

**Keep:** Claude Code/Codex (inner harness), `teammcp` (76 tools), the durable business store,
the web API/transport, integrations/membrane.

**Explicit non-goals:** ❌ rewrite the inner harness; ❌ rewrite teammcp tools; ❌ replace the
durable business store; ❌ port the *whole* Go backend to Python; ❌ big-bang cutover;
❌ run two orchestrators that co-own the same task (per-task single ownership, §8).

## 2. Why this target is lower-risk than the inner-loop framing

The inner harness is the part hardest to beat and easiest to break. We keep it untouched.
The bet shrinks from "deepagents on our models ≥ Claude Code" (unproven, probably false on day
one) to "LangGraph ≥ WUPHF's hand-rolled coordination" — a low bar you've already called. The
capability risk that dominated the earlier plan is mostly gone.

## 3. Architecture

```
   TS web ◄──► Go host:  business store · API/WS transport · teammcp tools · integrations  (KEEP)
                         ▲ task-status projection            ▲ tools (MCP, broker token)
   ┌─────────────────────┴────────────────────────────────── ┴───────────────────────────┐
   │  Python LangGraph orchestrator   (orchestrator-of-record — NEW)                       │
   │   • task lifecycle = graph state; durable via LangGraph checkpointer                  │
   │   • CEO decompose/plan node (optionally deepagents write_todos + task)                │
   │   • multi-agent sequencing · dependencies · scheduling · escalation                   │
   │   • HITL via interrupt() ─► existing approval surface ─► Command(resume=…)            │
   │             │ each work-step node invokes the GREAT inner harness:                    │
   │             ▼                                                                          │
   │     Claude Code (Claude Agent SDK) / Codex (CLI)  ── use teammcp tools over MCP        │
   └────────────────────────────────────────────────────────────────────────────────────────┘
```

**Data-ownership split (the rule that prevents dual-source-of-truth):**

| Fact | Owner | Notes |
|---|---|---|
| Orchestration / run-state (task graph, node status, pending interrupts, turn cursors) | **LangGraph checkpointer** (NEW store) | "What is running and where." |
| Business records (office members, integrations, wiki, skills, channels/messages, task *records*) | **Go store** (KEEP) | "The company." |
| Web's view of task status | **Projection** | One-way LangGraph → Go task records → web. Web renders unchanged. |

No single fact lives in two stores. Orchestration facts are LangGraph's; business facts are Go's;
the projection is one-way and derived.

## 4. Component inventory

**New**
| Component | Where | Notes |
|---|---|---|
| `orchestrator/` LangGraph app | Python | The graph: lifecycle, decomposition, sequencing, HITL. |
| Checkpointer | Python | Official `langgraph-checkpoint-sqlite`/`-postgres`; this store = run-state source of truth. |
| Node → inner-harness invokers | Python | `claude` node via **Claude Agent SDK**; `codex` node via CLI/exec. Pass teammcp MCP config + broker token. |
| Projection bridge | Go ↔ Python | Orchestrator emits task-status events → Go persists as task records for the web. |
| Run-state migration shim | both | `broker-state.json` lifecycle fields → LangGraph checkpoint (§8, prod fixtures per `TODOS.md`). |
| Supervisor | Go (`internal/runtimebin`) | Start/health/restart the Python orchestrator sidecar. |

**Reused unchanged** — Claude Code/Codex, `teammcp` + broker-token launch pattern, durable
business store, web API/WS, integrations/membrane, `prompt_builder` logic (ported or called).

**Deleted last (P5):** `internal/agent` loop + `internal/team` coordination logic.

## 5. The three seams

**5a. Run-state / checkpointer seam (highest-risk — now spiked, see
[`../../spikes/langgraph-runstate/REPORT.md`](../../spikes/langgraph-runstate/REPORT.md)).**
The spike round-tripped WUPHF's real 13-state lifecycle through a LangGraph checkpointer in
both ownership variants (13/13). It picks the **re-hydrate** variant: the **Go broker record
stays the single source of durable truth**; the LangGraph checkpoint is a *rebuildable cache*
re-hydrated from the Go record on restart. This avoids physically migrating run-state into a
new store, removes dual-source drift, and makes a lost checkpoint recoverable. It still
satisfies D4 (LangGraph decides what runs; Go holds the record). Two rules the spike nailed:
(1) **carry `lifecycle_state` directly, never re-derive from the 4-tuple** — derivation is
lossy for 3 states (`intake→ready`, `decision→review`, `changes_requested→running`) and is a
legacy-only fallback; (2) contradictory legacy tuples **fail loud to `unknown`** for operator
triage, never a silent wrong state.

**5b. Inner-execution seam.** A work-step node invokes the inner harness:
- **Claude:** the **Claude Agent SDK** (Python) drives Claude Code in-process with an MCP config
  (teammcp) + permission mode; cleaner than shelling the CLI from Python.
- **Codex:** CLI/exec with the same MCP config.
- Both get `WUPHF_BROKER_TOKEN` + `WUPHF_BROKER_ADDR` so teammcp tools authenticate to the broker
  (same trust model as today). Node streams inner output back into graph state.

**5c. Web/transport seam.** Keep the web unchanged: Go projects LangGraph run-state into the
existing task/lifecycle shapes the web already consumes, and streams events over the existing WS.
The web does not learn there's a Python orchestrator.

## 6. HITL mapping

Gated nodes call LangGraph `interrupt()`; the orchestrator surfaces it through Go's existing
approval surface (the deterministic-integrations `ExternalActionApprovalCard`). Human decision →
`Command(resume=…)`. Gate policy stays in one place (the broker's action classification), so we
don't fork approval logic.

## 7. Concurrency / scheduling mapping

Today: worktree-keyed lanes; only real `depends_on` serializes; independent tasks run in parallel.
Map onto LangGraph: dependencies → graph edges; independent tasks → concurrent branches; the
worktree-per-task isolation stays (the inner CLI node runs in the task's worktree). Verify
LangGraph's concurrency model honors the "independent ⇒ parallel, dependent ⇒ serialized" rule
with a dedicated test; do not assume it.

## 8. Migration sequencing (strangler-fig; per-task single ownership)

During transition two orchestrators coexist, but **every task is owned by exactly one** — a
per-task `orchestrator=langgraph|broker` flag, never both. That bounds the anti-pattern.

| Phase | Deliverable | Gate |
|---|---|---|
| **P0** ✅ | Seam spike (Python↔teammcp over MCP) | Done. |
| **P1a** ✅ | The `orchestrator/` Python package: real lifecycle model + LangGraph graph (re-hydrate → dispatch turn → transition, HITL via `interrupt()`) + wire contract + FastAPI service + 25 green tests, key-free. | Built — `orchestrator/`, `pytest` green. |
| **P1b** | Go side: a `provider/deepagents`-style dispatch client + broker-state projection write-back + per-task `orchestrator=langgraph\|broker` flag, behind a flag, one task-type. Web renders via projection. | A real single-agent task completes through LangGraph and shows correctly in the web. |
| **P2** | CEO decompose node + dependencies + multi-agent sequencing in LangGraph. | A goal decomposes into ordered tasks and runs to completion through LangGraph. |
| **P3** | HITL interrupts + approval gates via LangGraph. | A task that hits an approval gate pauses and resumes correctly. |
| **P4** | Run-state ownership (re-hydrate, §5a — spiked ✅): Go record stays authoritative, LangGraph rehydrates on restart; carry `lifecycle_state`; prod-fixture tests (`TODOS.md` item 0). | Existing in-flight tasks resume under LangGraph without state loss. |
| **P5** | Expand to all task-types; retire `internal/agent` loop + `internal/team` coordination logic. | Parity sustained across task-types; old coordination deleted. |
| **P6** | Deployment/bundling (Python sidecar + supervisor) incl. Windows. | `wuphf` ships with a working bundled orchestrator on macOS/Linux/Windows. |

Each phase ships behind the per-task flag with E2E verification (global rule: incremental, no big-bang).

## 9. Deployment / bundling

The orchestrator is Python, so a runtime ships with the product (same tax as the earlier plan):
self-contained Python (uv/PEX) bundled beside `wuphf`, supervised by Go (`internal/runtimebin`);
Wails desktop spawns it as a sidecar (compromises "in-process, no sidecar" for the orchestrator,
not the store); container for hosted; **Windows is the highest-risk platform — schedule early in P6.**

## 10. Test strategy

- **Python:** LangGraph graph unit tests (decomposition, ordering, HITL) with a fake inner-harness
  node (no API key, spike pattern); checkpointer migration tests against **production `broker-state.json`
  fixtures** (`TODOS.md` item 0); node→Claude-SDK/Codex integration tests.
- **Go:** projection-bridge tests (LangGraph status → web shapes); supervisor lifecycle tests.
- **Cross-language:** golden fixtures for the projection + run-state shapes; oracle both directions.
- **E2E:** per task-type, a real run through LangGraph vs the broker path, compared; concurrency
  rule (§7) verified explicitly.

## 11. Risks & open questions

| Risk | Mitigation |
|---|---|
| **Run-state migration** — the in-flight-task data move. | **De-risked (spike):** re-hydrate keeps Go authoritative (no data move); carry `lifecycle_state` (lossless 13/13); unmapped tuples fail loud to `unknown`. Remaining: real prod fixtures (`TODOS.md` #0). |
| **Two orchestrators in transition** | Per-task single-ownership flag; never co-owned. |
| **Web churn** | One-way projection keeps the web API shapes unchanged. |
| **Concurrency semantics differ** | Explicit §7 test; don't assume LangGraph matches the lane rule. |
| **Deployment / Windows** | P6 early; PEX/uv; container for hosted. |
| **Capability** | Largely retired — the inner harness (Claude Code/Codex) is unchanged. |
| **Security** | Broker token flows to the Python orchestrator + inner CLIs; same trust as today's CLI; only env-var names on the wire. |

Open: does the CEO node use **deepagents** (write_todos + task) or a plain LangGraph supervisor?
Decide at P2 by prototype; not load-bearing for the architecture.

## 12. First concrete PR (P1)

A `orchestrator/` LangGraph app running one single-agent task-type end-to-end behind a per-task
flag: one Claude work-step node via the Claude Agent SDK (teammcp MCP + broker token), the official
SQLite checkpointer, and a projection bridge that writes task status into the Go store so the existing
web renders it. Reuses the spike's `seam_probe.py` MCP wiring. No change to the inner harness, the
web, or teammcp.
