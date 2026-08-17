# Evaluation: LangChain `deepagents` for the WUPHF agent harness

> Status: **assessment / decision pending**. No code changes.
> Branch: `worktree-deepagents-harness-eval`. Base: `origin/main` @ `ec467159`.
> Date: 2026-06-19.

## TL;DR

**Do not adopt `deepagents` as our harness. Adopt it as a reference design.**

`deepagents` is a **Python-first** harness (with a JS port, `deepagentsjs`) built on
**LangGraph**. WUPHF's harness is **100% Go** and ships as a single self-contained
binary (the Wails desktop decision explicitly chose an *in-process broker, no
sidecar*). There is no Go `deepagents`. So "use deepagents for our harness"
reduces to three options, and the language/runtime boundary is the decisive fact:

| Option | What it means | Verdict |
|---|---|---|
| **A. Replace** | Rewrite the harness in Python on LangGraph | ❌ No — kills the single-binary product + Wails in-process decision + ~all of `internal/agent` + provider + broker glue |
| **B. Sidecar** | Run `deepagents` (Python or `deepagentsjs`) as an IPC/HTTP sidecar | ⚠️ Narrow — only for *foreign fleets* (the Slack "agents-from-anywhere" membrane) or hosted/Nex-cloud, **not** the core OSS binary |
| **C. Borrow the design** | Port the 3 missing pillars into Go as MCP tools + loop changes | ✅ Yes — this is the real value, ships in-language, preserves deploy story |

Recommendation: **Option C now; keep Option B on the table only for the membrane /
hosted lane.** Reject Option A.

---

## DECISION (2026-06-19) — supersedes the TL;DR above

The founder chose a **full move onto LangGraph**, not a borrow-the-design port. A
`/plan-eng-review` scope challenge first mis-aimed at the *inner* loop; the founder
corrected the target: **Claude Code/Codex are great inner harnesses and stay — the weak,
hand-rolled layer is WUPHF's coordination *above* them.** Final shape (D4):

- **LangGraph = orchestrator-of-record (Python).** Replaces the hand-rolled coordination:
  the agent tick/turn-loop (`internal/agent`) and `internal/team`'s decomposition,
  sequencing, lifecycle state machine, scheduling, escalation. Owns task lifecycle +
  run-state via a LangGraph checkpointer.
- **Inner execution kept:** Claude Code (via the Claude Agent SDK) / Codex, invoked from
  graph nodes; they keep using `teammcp` tools over MCP.
- **Go = host, not coordinator:** durable *business* store, API/WS transport, `teammcp`
  tools, integrations. Run/orchestration state moves to LangGraph's checkpointer; business
  records stay in Go; a one-way projection keeps the web unchanged.
- **deepagents:** optional, only for the CEO decompose/delegate node (`write_todos`+`task`).
- **Why lower-risk than the inner-loop framing:** the "can deepagents beat Claude Code" bet
  is retired (inner harness untouched); the remaining bet is "LangGraph ≥ hand-rolled
  coordination," a low bar.

Rejected en route: replacing the inner CLIs (they're the strong part); full Python backend
rewrite retiring the Go store/integrations; hybrid that leaves the weak coordination in Go.

**Spike (Option A′) — DONE, seam validated.** See
[`spikes/deepagents-seam/REPORT.md`](../../spikes/deepagents-seam/REPORT.md). Against the
real `wuphf mcp-team`, no API key: deepagents bound **84 tools = 76 live teammcp tools
(zero rewrite) + 8 deepagents builtins** (`write_todos`/`task`/virtual-FS); per-call seam
cost **~1.9 ms median**; `interrupt_on` HITL fired before the tool. One flag: `mcp-team`
cold start ≈ 6.3 s → hold one long-lived teammcp session per task, don't cold-spawn.

The spike's core proof — a Python process reaching the real `teammcp` tools over MCP —
still underpins the LangGraph target (both the orchestrator and the inner Claude/Codex
nodes need that tool access). deepagents-specific bits are now relevant only if the CEO
node uses deepagents.

**NEXT:** migration plan written —
[`deepagents-migration-plan.md`](./deepagents-migration-plan.md) (LangGraph
orchestrator-of-record). Run the 4-section eng review against it; de-risk the P4 run-state
migration (`broker-state.json` → checkpointer) and P6 deployment/Windows early.

---

## 1. What `deepagents` is

LangChain frames it as *"the batteries-included agent harness"* — a layer above
core LLM building blocks that adds production patterns so you don't rebuild them.
Source: <https://docs.langchain.com/oss/python/deepagents/overview>, `langchain-ai/deepagentsjs`.

Four pillars:

1. **Planning / task decomposition** — built-in `write_todos` tool; the agent
   writes a structured todo list (`pending`/`in_progress`/`completed`) before and
   during execution, and adapts it as it learns.
2. **Sub-agents (delegation)** — a `task` tool spawns **ephemeral** child agents
   with **fresh, isolated context** that return a single final report. Default
   `general-purpose` sub-agent auto-enabled; custom sub-agents definable.
3. **Virtual filesystem** — `ls/read_file/write_file/edit_file/glob/grep` over a
   **pluggable backend** (in-memory `StateBackend`, `StoreBackend`, on-disk
   `FilesystemBackend`, composite). Used for intermediate offloading + cross-thread
   memory. Plus automatic **summarization** of history and large tool results.
4. **Context engineering** — `SKILL.md` skills (Agent Skills standard, loaded
   progressively), `AGENTS.md` persistent memory, Anthropic prompt caching for
   static sections, and human-in-the-loop via `interrupt_on={"edit_file": True}`.

Runtime: Python package `deepagents` (and TS `deepagentsjs`), on **LangGraph** for
durable execution/streaming/HITL. `create_deep_agent(model=..., tools=[...],
system_prompt=...)` returns an invokable agent. **Model-agnostic**
(`anthropic:` / `openai:` / `google_genai:` / Ollama / OpenRouter / Fireworks).

Stated limits: sub-agents are stateless single-shot; `FilesystemMiddleware` and the
`task` tool cannot be removed; permissions don't apply to sandbox backends.

---

## 2. What WUPHF's harness actually is

There is **no single harness**. There is a **provider abstraction** with two very
different execution paths:

### Path 1 — Native Go loop (`internal/agent/`)
A state machine: `AgentLoop.Tick()` cycles `Idle → BuildContext → StreamLLM →
ExecuteTool → Done` (`internal/agent/loop.go`). One goroutine per agent
(`service.go:runAgentWorker`). Calls a pluggable `StreamFn` **once per turn**.
Agent-callable tools are a flat set of **15**, verified in `internal/agent/tools.go`:
`read_file, grep_search, glob, write_file, bash, send_message, nex_search,
nex_ask, nex_remember, nex_record_{list,get,create,update}, nex_gossip_{publish,query}`.
**No `write_todos`. No `task`/sub-agent-spawn. No virtual-FS abstraction.** Memory
is session JSONL + lossy "Office Insight" summarization + Nex gossip.
→ This is the path used for raw-API providers (OpenAI/local/Gemini).

### Path 2 — CLI providers + `teammcp` (the production path)
For providers `claude` / `codex`, the provider layer shells out to the **Claude
Code / Codex CLIs**, which connect to our **`internal/teammcp` MCP server**.
`teammcp` exposes a *rich* surface (each file = a tool group): `notebook_tools`,
`playbook_tools`, `skills`/`skill_compile`/`skill_crud`, `server_wiki_tools`,
`server_memory_tools`, `policy_tools`, `learning_tools`, `routine_tools`,
`rich_artifact_tools`, `entity_tools`, `context_tools`, action/approval gates,
`server_request_tools`, `member_approval`.
→ Here the **CLI is itself a deep-agent harness** (its own TodoWrite, Task
sub-agents, filesystem, skills), and `teammcp` layers the *company context* on top.

### Surrounding orchestration (`internal/team/`)
The broker owns tasks + lifecycle. Planning today is a **gate**, not an in-loop
todo tool: `LifecycleStatePlanning` (`broker_lifecycle_transition.go`, PR #1116
"structured planning gate — plan-first") dispatches the owner to write a plan that a
human approves before `Running`. Concurrency is worktree-keyed task lanes
(per memory: real `depends_on` serializes, independent tasks run in parallel).
Sub-agent decomposition is **message-driven** (CEO → agent via `send_message`),
not runtime child-agent spawning.

---

## 3. Capability gap map

| `deepagents` pillar | Native Go path (raw API) | CLI path (claude/codex + teammcp) |
|---|---|---|
| **Planning `write_todos`** | ❌ absent | ✅ CLI's own TodoWrite |
| **Ephemeral sub-agents (`task`)** | ❌ absent | ✅ CLI's own Task tool |
| **Virtual FS + offloading** | ⚠️ flat `read/write_file` + lossy summary; no pluggable backend | ✅ CLI FS + `notebook`/`wiki` via teammcp |
| **Skills (`SKILL.md`)** | ⚠️ knowledge snippets in packs | ✅ `skills`/`skill_compile` in teammcp |
| **Persistent memory (`AGENTS.md`)** | ✅ Nex `nex_remember` + gossip | ✅ Nex + `server_memory_tools` |
| **Summarization** | ✅ "Office Insight" compaction | ✅ CLI + teammcp |
| **HITL `interrupt_on`** | ⚠️ `PermissionMode: "plan"` gate (coarse) | ✅ action-approval gates in teammcp |
| **Model-agnostic** | ✅ provider registry | ✅ |

**Read-out:** `deepagents`' net-new value lands almost entirely on the **native
raw-API path** — the path that has no planning loop, no sub-agent spawn, and no
real VFS. The CLI path already gets deep-agent behavior *for free* from Claude
Code/Codex, and `teammcp` already implements the company-context half. So the
honest question is **not** "should we adopt deepagents," it's **"do we want the
native raw-API path to be a real deep-agent harness, and if so, build it in Go or
borrow a runtime?"**

---

## 4. The decisive constraints

1. **Language.** deepagents is Python (`deepagentsjs` is TS). Our harness, broker,
   providers, and MCP server are Go. No Go port exists.
2. **Single-binary deploy.** WUPHF ships one self-contained Go binary (8 `go:embed`
   sites; Wails desktop = 43MB single binary, *in-process broker, no sidecar* —
   an explicit founder decision). A Python/Node runtime breaks that.
3. **No-new-runtime rule** (global CLAUDE.md): "No new packages/services unless
   truly shared across contexts," and *ask before committing to a library/system*.
4. **We already have the CLI escape hatch.** Anyone wanting maximal deep-agent
   behavior today runs the `claude`/`codex` provider and gets it. deepagents would
   compete with that, not extend it.

---

## 5. The three options, in detail

### Option A — Replace the harness with Python/LangGraph ❌
Rewrites `internal/agent/*`, the provider abstraction, and the broker glue;
introduces a Python runtime; destroys the single-binary + Wails story. Throws away
a mature, working system for a reference design. Rejected on merit (and on the
no-sunk-cost principle — this isn't sunk cost, it's *active working infrastructure*).

### Option B — `deepagents` as a sidecar ⚠️ (narrow)
Run `deepagentsjs` (TS — closer to our web stack than Python) as an IPC/HTTP
service the broker talks to. Adds a runtime to ship + supervise; conflicts with the
no-sidecar core decision. **Only justified where a foreign runtime is already part
of the design:**
- The **Slack "agents-from-anywhere" membrane** (per memory): the CEO is a membrane,
  foreign fleets execute scoped, human-approved steps. A `deepagents` fleet is
  exactly such a foreign fleet — it auths to the membrane, not into our process.
- The **hosted / Nex-cloud multi-tenant** side, which is out of scope for the OSS
  single-binary repo anyway.
Not for the core OSS binary.

### Option C — Borrow the design into Go ✅ (recommended)
Treat deepagents as a validated blueprint for the native path's gaps. Port the 3
missing pillars in-language, reusing what exists:
- **`write_todos` →** a planning/todo MCP tool (in `teammcp`, so *both* paths get it)
  backed by the existing notebook/task model; surfaces structured todos instead of
  prose. Directly answers the founder's "prose substrate / no structured state" gap
  (per `sota-gap-analysis` memory).
- **`task` ephemeral sub-agent →** reuse `AgentService.Create` to spawn a scoped,
  isolated-session child agent that returns a single final report; wire it as a tool
  + a lifecycle/concurrency-lane entry. We already have worktree-keyed lanes to
  isolate it.
- **Virtual FS + offloading →** formalize a pluggable FS backend over the
  notebook/wiki/worktree we already have, and make summarization offload to files
  (not lossy in-memory compaction).
This is incremental, testable, ships as MCP tools + loop changes, and keeps the
single binary. It is the "steal/borrow/build → borrow the design, build in our
stack" move (per memory rule), and folds cleanly into the existing **SOTA-uplift**
lane rather than starting a parallel one.

---

## 6. Recommendation

1. **Reject A.**
2. **Do C** as the substance — and scope it as additions to the **SOTA-uplift**
   lane, not a new initiative. Smallest first slice: a `write_todos`-equivalent
   structured-planning tool in `teammcp` (benefits *both* execution paths), then
   ephemeral sub-agent spawn, then the FS backend.
3. **Hold B** for the membrane / hosted lane only. If we ever run foreign fleets,
   `deepagentsjs` behind the CEO membrane is a clean fit — but that's the
   agents-from-anywhere design, not "our harness."

## 7. Open questions for the founder

- Is the **native raw-API path** strategically important, or is the
  `claude`/`codex` CLI path the real product? (If the CLI path is the product, C's
  value drops and we mostly want the `write_todos`/sub-agent tools in `teammcp` for
  the CLI agents to call — still worth it, smaller.)
- Do we want `deepagents` evaluated specifically as the **foreign-fleet runtime**
  for the Slack membrane? That's the one place adopting the *code* (not just the
  design) is defensible.
- Appetite for a tiny spike: a `write_todos` MCP tool + a one-shot ephemeral
  sub-agent, behind a flag, to feel the ergonomics before committing the lane.
