# WUPHF (pronounced "woof")

[![Discord](https://img.shields.io/badge/Discord-Join%20Community-5865F2?logo=discord&logoColor=white)](https://discord.gg/gjSySC3PzV)
[![License: Sustainable Use License](https://img.shields.io/badge/license-Sustainable%20Use%20License-A87B4F)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](go.mod)

<p align="left">
  <a href="https://news.ycombinator.com/item?id=47899844">
    <img src="website/hn-badge.svg" alt="WUPHF — Hacker News Life of Product Week's #1" width="223" height="48" />
  </a>
</p>

### Build a microapp for every manual workflow.

WUPHF lets anyone turn their manual workflows into microapps across 1200+
integrations in minutes. Describe the job in one sentence — or demo it once on
a call — and your AI builds the agent that runs it: its own screen, its own
schedule, its own tools, with a human approval gate on everything it sends.
Runs local, on your machine, on your account.

> *"WUPHF. When you type it in, it contacts someone via phone, text, email, IM,
> Facebook, Twitter, and then... WUPHF."*
> — Ryan Howard, Season 7

Unlike the original WUPHF.com, this one ships work on Mondays.

## Get Started

**Prerequisites:** one agent CLI, signed in — [Claude Code](https://docs.anthropic.com/en/docs/claude-code)
by default, or [Codex CLI](https://github.com/openai/codex) / [Opencode](https://opencode.ai).
The first-run screen verifies your runtime before anything else happens.

```bash
npx wuphf
```

That's it. The browser opens, you verify your runtime, name your office, and
hand off your first workflow — you land on your first agent being built, live.

Prefer a global install?

```bash
npm install -g wuphf && wuphf
```

Building from source (requires Go and Bun):

```bash
git clone https://github.com/najmuzzaman-mohammad/wuphf.git
cd wuphf
cd web && bun install && bun run build && cd ..
go build -o wuphf ./cmd/wuphf
./wuphf
```

Routine execution runs on a small sidecar service (`agent/`). The broker
finds and supervises it automatically on source checkouts (set
`WUPHF_AGENT_DIR` to point elsewhere, or `WUPHF_AGENT_URL` if you manage it
yourself).

## What you get

Every agent ships with all six. Not a chatbot in a trench coat.

| Part | What it is |
|---|---|
| **The app** | A real screen, built live in front of you. Reads and writes real workspace data. |
| **Routines** | "Every Monday 9:00." Versioned prompts, run history, a transcript per run. New agents get a starter weekly routine from the workflow you described. |
| **Tools** | Self-authored. "Score a lead." "Post to #ae-handoffs." Teach more in the agent's chat. |
| **Knowledge** | Wikipedia-style pages about the agent, every claim cited back to its source. |
| **Data + integrations** | Its own typed tables, plus 1200+ integrations. Connect once; every agent shares it. |
| **Approval gate** | Reads are free. Writes are held until you tap approve. Then it runs 24x7. |

If your workflow names a system that is not connected yet ("audit our
HubSpot"), WUPHF asks before building — build against live workspace data
now, or hold while you connect. It never silently re-scopes your job.

## Setup prompt (for AI agents)

Paste this into Claude Code, Codex, or Cursor and let your agent drive the install:

```text
Set up https://github.com/najmuzzaman-mohammad/wuphf for me. Read `README.md`
first, then run `npx wuphf` — the web UI opens at http://localhost:7891.

Walk the onboarding: verify the runtime, name the office, and start the first
workflow. Confirm you land on an agent being built (a live build feed beside a
chat), and that when it finishes the agent shows tabs for UI, Routines, Tools,
Data, Knowledge, and Integrations.

For agent conventions read `AGENTS.md`; for internals read `ARCHITECTURE.md`;
for forking read `FORKING.md`.
```

## Options

| Flag | What it does |
|------|-------------|
| `--provider <name>` | Runtime override (`claude-code`, `codex`, `opencode`, `ollama`, `hermes-agent`, `openclaw-http`) |
| `--no-open` | Don't auto-open the browser |
| `--web-port <n>` | Change the web UI port (default 7891) |
| `--workspace <name>` | Use a specific workspace for one command (does not change the active workspace) |
| `--unsafe` | Bypass agent permission checks (local dev only) |

### Local models and custom endpoints

For custom OpenAI-compatible endpoints (LiteLLM, local proxies, Ollama):

```bash
WUPHF_OLLAMA_BASE_URL="http://127.0.0.1:20128/v1" \
WUPHF_OLLAMA_MODEL="openai/gpt-5.4-mini" \
wuphf --provider ollama --no-open
```

`--provider opencode` shells out to the `opencode` CLI binary; MLX-LM and
Ollama can be set up from the first-run screen with no cloud key at all.

### Other runtimes

Already running [Hermes Agent](https://github.com/NousResearch/hermes-agent)
or an [OpenClaw](https://openclaw.ai) gateway? Point agents at them with
`--provider hermes-agent` (default `http://127.0.0.1:8642/v1`) or
`--provider openclaw-http` (default `http://127.0.0.1:18789/v1`). Endpoints,
models, and auth are overridable via `WUPHF_HERMES_AGENT_*` /
`WUPHF_OPENCLAW_HTTP_*` env vars or `provider_endpoints` in config.

## Memory: the company brain

WUPHF ships with built-in memory — no backend choice, no API key. Your
workspace state lives in local files you can `cat`: agent knowledge, run
transcripts, and the company brain under `~/.wuphf/`. Knowledge pages are
synthesized with citations back to their sources, so you can check the
receipts on anything an agent claims.

## Other Commands

```bash
wuphf init                    # First-time setup
wuphf share                   # Invite one team member over Tailscale/WireGuard
wuphf shred                   # Delete workspace state and reopen onboarding
wuphf workspace list          # Run multiple isolated workspaces side by side
wuphf workspace switch <name> # Flip the active workspace
```

## Share With a Team Member

Two ways to invite a teammate, both from the CLI:

**Private network — Tailscale or WireGuard.** Both machines on the same mesh;
the invite never leaves the network:

```bash
wuphf share
```

**Public tunnel — no shared network needed.** The broker can spin up a
Cloudflare quick tunnel (`POST /api/share/tunnel/start`; the trycloudflare
URL is paired with a 6-digit passcode, invites are one-use and expire in 24
hours, and the join handler is rate-limited per source IP). `cloudflared`
ships with the npm install (pinned SHA256 per platform). The one-click
button for this is being resurfaced in the operator shell — until then the
endpoint is the path.

For the full walkthrough, see
[Share WUPHF With a Team Member](docs/tutorials/share-with-team-member.md).

## External Actions

Agents act through two providers — pick whichever fits:

- **One CLI** (default, local-first): actions execute through a local CLI on
  your machine; credentials never leave it.
- **Composio** (cloud-hosted OAuth): connect Gmail, Slack, HubSpot, and the
  rest of the 1200+ catalog from any agent's **Integrations** tab. Connections
  are shared across the office.

Either way, the approval gate holds every external write until you approve it.

## Privacy & Telemetry

WUPHF can send anonymous product analytics and session recordings (with typed
text masked) to help us improve it. This is **optional**, controlled by you, and
**off unless a PostHog key is configured** — a stock source build and every fork
ship with no key, so they never phone home.

Two independent toggles (onboarding and Settings), both on by default, both
reversible at any time:

- **Product analytics** — anonymous usage events: which flows are used, where
  people get stuck, error counts. We send **counts and shapes only, never your
  content** (no message text, task titles, customer data, or secrets).
- **Session recording** — recordings **mask everything you type**
  (`maskAllInputs: true`): passwords, API keys, and any form field are
  obscured. We capture layout, clicks, and navigation to fix rough edges.

No autocapture, no cookies (localStorage only). Self-hosted operators can
point at their own PostHog (`WUPHF_POSTHOG_KEY` / `WUPHF_POSTHOG_HOST`) or
leave the key unset to keep WUPHF fully dormant. Full taxonomy and policy:
[docs/specs/product-analytics.md](docs/specs/product-analytics.md).

## Why WUPHF

| | |
|---|---|
| **One agent per workflow** | Small enough to read in a minute, real enough to do the whole job — instead of one giant assistant that does everything badly. |
| **You watch it get built** | The build streams live: the screen, the routine, the tools, assembling in front of you. |
| **Honest by default** | No connected data → the app says "simulated" in plain text. Missing integration → it asks before building. Every knowledge claim carries a citation. |
| **Approval gate** | No email, Slack post, or CRM write leaves without a human tap. |
| **Local** | Runs on your machine, on your keys. Workspace state is files you can `cat`. |
| **Cost you can see** | Settings shows exactly what your agents have spent — dollars, tokens, runs. A typical agent build lands in the $1–2 range on Claude Code. |
| **Price** | Free to self-host (Sustainable Use License, your API keys). |

## Claim Status

Every claim in this README, grounded to the code that makes it true.

| Claim | Status | Where it lives |
|---|---|---|
| Describe a workflow → agent builds live with a streaming activity feed | ✅ shipped | `web/src/operator/surfaces/AppBuilderChat.tsx`, `web/src/components/apps/AppActivity.tsx` |
| Onboarding hands the first workflow straight into the build | ✅ shipped | `web/src/operator/firstWorkflowSeed.ts`, `web/src/operator/OperatorApp.tsx` |
| New agents get a starter weekly routine from the described workflow | ✅ shipped | `web/src/operator/surfaces/AppBuilderChat.tsx` |
| Routines: broker-owned cron, versioned prompts, per-run transcripts | ✅ shipped | `internal/team/scheduler_operator_routines.go`, `web/src/operator/routines/RoutinesTab.tsx` |
| The broker spawns and supervises the routine runner | ✅ shipped | `internal/team/agent_service_supervisor.go` |
| Ask-before-building when a referenced integration is not connected | ✅ shipped | `web/src/operator/builder/describedIntegrations.ts` |
| Approval gate on external writes | ✅ shipped | `web/src/operator/components/ApprovalPrompt.tsx`, `internal/team/broker_action_grants.go` |
| Knowledge pages with inline citations | ✅ shipped | `web/src/operator/surfaces/KnowledgeSurface.tsx` |
| Usage readout — cost, tokens, runs | ✅ shipped | `web/src/operator/surfaces/SettingsSurface.tsx` (GET `/usage`) |
| Operator is the only front door; legacy office routes redirect | ✅ shipped | `web/src/routes/RootRoute.tsx`, `web/e2e/tests/route-matrix.spec.ts` |
| Per-agent typed tables (Data tab) | ✅ shipped | `web/src/operator/surfaces/AppDataTab.tsx` |
| Mix runtimes: Claude Code, Codex, Opencode, local models, Hermes, OpenClaw | ✅ shipped | `internal/provider/`, first-run pre-pick screen |
| Multi-workspace, share, and public-tunnel invites | ✅ shipped | `cmd/wuphf/workspace.go`, `cmd/wuphf/tunnel.go`, `internal/workspaces/` |
| Pixel-art agent portraits (every agent has a face) | ✅ shipped | `web/src/components/ui/PixelAvatar.tsx` |
| Routine runner packaged into the npx/desktop installs | 🔜 planned | supervisor adopts it the moment it ships |

Legend: ✅ shipped · 🟡 partial · 🔜 planned. If a claim and a status disagree, the code wins — file an issue.

## Evaluate This Repo

Before you fork, run this prompt against the codebase with any AI coding assistant:

```
You are a cynical senior engineer evaluating whether to fork this repo as the
base for an agents-that-run-workflows product. No prior context — explore it
as you naturally would. Tell me: should I fork this, and what's your honest
take? Be specific: file paths, line numbers, actual evidence. "The docs are
bad" is useless. Under 500 words.
```

We run this ourselves before every release. If the AI finds something we
missed, [file an issue](https://github.com/najmuzzaman-mohammad/wuphf/issues).

## The Name

From [*The Office*](https://theoffice.fandom.com/wiki/WUPHF.com_(Website)),
Season 7. Ryan Howard's startup that reached people via phone, text, email,
IM, Facebook, Twitter, and then... WUPHF. Michael Scott invested $10,000.
Ryan burned through it. The site went offline.

The joke still fits. Except this WUPHF ships.

> *"I invested ten thousand dollars in WUPHF. Just need one good quarter."*
> — Michael Scott

Michael: still waiting on that quarter. We are not.

## Related

Other open-source projects building knowledge bases the LLM way, with less paper-company energy:

- [Astro-Han/karpathy-llm-wiki](https://github.com/Astro-Han/karpathy-llm-wiki) — agent-skill for Claude Code / Cursor.
- [dimknaf/braindb](https://github.com/dimknaf/braindb) — persistent memory database + REST API for LLM agents: typed entities with explicit relations, fuzzy + semantic retrieval over a keyword graph, temporal decay, plus a watcher that auto-ingests dropped files into a self-healing wiki.
- [lucasastorian/llmwiki](https://github.com/lucasastorian/llmwiki) — Claude MCP integration.
- [nashsu/llm_wiki](https://github.com/nashsu/llm_wiki) — desktop app, builds wikis from documents.

## Star History

<a href="https://www.star-history.com/?repos=najmuzzaman-mohammad%2Fwuphf&type=date&legend=top-left">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/chart?repos=najmuzzaman-mohammad/wuphf&type=date&theme=dark&legend=top-left" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/chart?repos=najmuzzaman-mohammad/wuphf&type=date&legend=top-left" />
   <img alt="Star History Chart" src="https://api.star-history.com/chart?repos=najmuzzaman-mohammad/wuphf&type=date&legend=top-left" />
 </picture>
</a>
