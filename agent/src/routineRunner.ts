// Routine execution: running a routine = what the chat does with a prompt.
// Match one of the agent's persisted tools (light title/name word-overlap
// heuristic, mirroring the FE chat's matchTool); if nothing matches, author a
// new tool (create_tool) and persist it, then run it.
//
// The routine's DEFINITION (prompt, schedule, enable/disable, versioning, run
// history) lives in the BROKER's scheduler registry — the broker calls
// POST /routines/run with the prompt on each fire. This module only executes
// and records the transcript into the routine's pi session.
//
// SEND-GATE (hard rule): a routine run ALWAYS executes with approved: false —
// default deny. A gated capability records needs_approval into the transcript
// ("paused for your approval: …"); a scheduled run never auto-sends.
//
// Every run appends to the routine's pi session and ALWAYS saves the outcome
// as an "md" artifact (<kebab-name>-run-<n>.md).

import { buildCapabilities, capabilityConfigFromEnv } from "./capabilities.js";
import { runtimeAICapabilityConfig } from "./serviceAuthor.js";
import type { PiSessions } from "./sessions.js";
import type { AgentStore } from "./store.js";
import { type CapabilityTree, runTool, type ToolRunResult } from "./toolRuntime.js";
import { buildTool } from "./tools.js";
import type { SessionMeta, StoredTool, Tool } from "./wire.js";

/** What the broker sends on each fire (its scheduler job, projected). */
export interface RoutineRunRequest {
	/** Broker scheduler slug — the routine's stable identity. */
	slug: string;
	/** Routine label, e.g. "Monday pipeline recap". */
	name: string;
	/** The prompt the agent runs in the routine's chat session. */
	prompt: string;
}

export interface RoutineRunnerDeps {
	store: AgentStore;
	sessions: PiSessions;
	/** Host capability tree; defaults to the env-composed runtime. */
	capabilities?: CapabilityTree;
	/** Tool authoring seam; defaults to buildTool (stub unless TOOL_AUTHOR_MODEL=1). */
	author?: (prompt: string) => Promise<{ tool: Tool | null }>;
	/** Tool execution seam; defaults to runTool (tests inject a recorder). */
	execute?: typeof runTool;
	now?: () => Date;
}

export interface RoutineRunOutcome {
	session: SessionMeta;
	status: ToolRunResult["status"];
	outcome: string;
}

/** Light title/name word-overlap match (ported from the FE chat's matchTool,
 * minus the run/call/use cue — a routine prompt is already imperative). An
 * exact mention of the callable name or full title wins; otherwise the
 * strongest title-word overlap, requiring at least two words so one generic
 * word cannot hijack the prompt. */
export function matchTool(prompt: string, tools: readonly StoredTool[]): StoredTool | null {
	const lower = prompt.toLowerCase();
	const exact = tools.find((t) => lower.includes(t.name.toLowerCase()) || lower.includes(t.title.toLowerCase()));
	if (exact) return exact;
	let best: StoredTool | null = null;
	let bestScore = 0;
	for (const t of tools) {
		const words = t.title
			.toLowerCase()
			.split(/[^a-z0-9]+/)
			.filter((w) => w.length > 2);
		const score = words.filter((w) => lower.includes(w)).length;
		if (score > bestScore) {
			best = t;
			bestScore = score;
		}
	}
	return bestScore >= 2 ? best : null;
}

/** "Monday pipeline recap" -> "monday-pipeline-recap". */
export function kebab(name: string): string {
	return (
		name
			.toLowerCase()
			.replace(/[^a-z0-9]+/g, "-")
			.replace(/^-+|-+$/g, "") || "routine"
	);
}

function outcomeText(r: ToolRunResult): string {
	if (r.status === "ok") return r.result;
	if (r.status === "needs_approval") return `paused for your approval: ${r.gate.detail}`;
	return r.detail;
}

/** Run a routine NOW (the broker's scheduler fire and its run-now both land
 * here via POST /routines/run). */
export async function runRoutine(agent: string, routine: RoutineRunRequest, deps: RoutineRunnerDeps): Promise<RoutineRunOutcome> {
	const { store, sessions } = deps;
	const now = deps.now ?? (() => new Date());

	// 1. A persisted tool that matches the prompt, else author + persist one.
	let tool: Tool | null = matchTool(routine.prompt, store.listTools(agent));
	if (!tool) {
		const author = deps.author ?? ((p: string) => buildTool(p, { tryModel: process.env.TOOL_AUTHOR_MODEL === "1" }));
		const authored = (await author(routine.prompt)).tool;
		tool = authored ? store.upsertTool(agent, authored) : null;
	}

	// 2. Run it — approved: false ALWAYS (default deny; see header).
	let status: ToolRunResult["status"];
	let outcome: string;
	if (tool) {
		const execute = deps.execute ?? runTool;
		// Pass the app id so a fired routine's tool reads/writes THIS app's store.
		const capabilities = deps.capabilities ?? buildCapabilities({ ...capabilityConfigFromEnv(), ...runtimeAICapabilityConfig(), appId: agent });
		const timeoutMs = Number(process.env.TOOL_CALL_TIMEOUT_MS) || undefined;
		// Give the tool the routine's prompt as `input`. A synthesized fallback tool
		// takes a single `input` param (tools.ts authorTool) — without this it ran on
		// "" and the digest read "Ran on  (simulated).". Tools with named params
		// (lead, deal, …) ignore it, so this only enriches the generic case.
		const r = await execute(tool, { input: routine.prompt }, { approved: false, capabilities, timeoutMs });
		status = r.status;
		outcome = outcomeText(r);
	} else {
		status = "error";
		outcome = "no tool could be authored for this routine";
	}

	// 3. Transcript into the routine's pi session: the scheduled prompt in,
	// the outcome back.
	const session = await sessions.ensureRoutineSession(agent, routine.slug, routine.name);
	const at = now().toISOString();
	await sessions.append(agent, session.id, { from: "you", body: `(scheduled) ${routine.prompt}`, at });
	await sessions.append(agent, session.id, { from: "nex", body: outcome, at: now().toISOString() });

	// 4. The run artifact (always, whatever the outcome). Run COUNTS and
	// status history live in the broker's per-slug run ring, not here.
	const n = store.listArtifacts(agent).filter((a) => a.producedBy === routine.name).length + 1;
	store.addArtifact(
		agent,
		{
			type: "md",
			title: `${kebab(routine.name)}-run-${n}.md`,
			producedBy: routine.name,
			content: outcome,
			size: `${new TextEncoder().encode(outcome).length} B`,
		},
		now(),
	);

	return { session, status, outcome };
}
