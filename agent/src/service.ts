// Thin HTTP/SSE service the operator FE talks to (no broker). Bun.serve, no
// framework:
//   GET  /health        liveness
//   GET  /providers     which inference paths are available (subscription/BYOK/local)
//   POST /build/stream  description -> the pi-mono agent assembles a WorkflowSpec (SSE)
//   POST /tools/build   teach a workflow -> the agent authors a tool (persisted per agent when `app` is set)
//   POST /tools/call    the app's chat calls a saved tool (sandboxed; gated -> approval)
//   POST /run           execute a compiled spec deterministically (gated step -> CQ1)
//   GET  /tools?agent=              the agent's persisted tools
//   POST /routines/run              the BROKER fires a due routine here (approved: false).
//                                   Definitions/cron/versioning/run history live in the
//                                   broker's scheduler registry, not in this service.
//   GET  /sessions?agent=           session list; GET /sessions/<id>?agent= transcript
//   POST /sessions                  new manual session; POST /sessions/<id>/message append
//   GET  /artifacts?agent=          the agent's saved run artifacts

import { timingSafeEqual } from "node:crypto";
import { streamWorkflow } from "./buildAgent.js";
import { buildCapabilities, capabilityConfigFromEnv, fetchAppSchema } from "./capabilities.js";
import { runWorkflow } from "./executor.js";
import { providersPayload } from "./providers.js";
import { runRoutine } from "./routineRunner.js";
import { PiSessions } from "./sessions.js";
import { AgentStore, defaultDataDir, sanitizeAgentId } from "./store.js";
import { runTool } from "./toolRuntime.js";
import { resolveToolAuthoring, runtimeAICapabilityConfig } from "./serviceAuthor.js";
import { buildTool } from "./tools.js";
import { type BuildRequest, type RunRequest, SCHEMA_VERSION, type ToolBuildRequest, type ToolCallRequest, type WorkflowSpec } from "./wire.js";

type BuildEvent = { type: "step"; step: WorkflowSpec["steps"][number] } | { type: "spec"; spec: WorkflowSpec };
type BuildStream = (message: string, opts: { toolId?: string; signal?: AbortSignal }) => AsyncGenerator<BuildEvent>;

export interface ServerOptions {
	port?: number;
	// Interface to bind. Defaults to loopback (WUPHF_AGENT_HOST) — this service
	// runs code and must never be reachable off-box without an explicit opt-in.
	hostname?: string;
	// Shared secret required as `Authorization: Bearer <token>` on every route
	// except /health. Defaults to WUPHF_AGENT_TOKEN. When empty the service is
	// unauthenticated and MUST stay loopback-bound (enforced in createServer).
	authToken?: string;
	// Override the build engine in tests so they never hit a live model.
	buildStream?: BuildStream;
	// Override the per-agent store (tests point it at a tmp dir).
	store?: AgentStore;
	// Override the pi-backed session layer (tests point it at a tmp dir).
	sessions?: PiSessions;
	// Override authoring resolution in tests (deterministic fake model author).
	authoring?: typeof resolveToolAuthoring;
}

/** True when `host` only ever resolves to the local machine. */
function isLoopbackHost(host: string): boolean {
	const h = host.trim().toLowerCase();
	return h === "" || h === "127.0.0.1" || h === "::1" || h === "localhost";
}

/** Constant-time bearer-token check. Returns true only when a non-empty token
 * is configured AND the request presents exactly that token. */
function authorized(req: Request, token: string): boolean {
	if (token === "") return false;
	const header = req.headers.get("authorization") ?? "";
	const prefix = "Bearer ";
	if (!header.startsWith(prefix)) return false;
	const presented = Buffer.from(header.slice(prefix.length));
	const expected = Buffer.from(token);
	// timingSafeEqual throws on length mismatch — guard first so length itself
	// is not a fast-path oracle, then compare the bytes in constant time.
	if (presented.length !== expected.length) return false;
	return timingSafeEqual(presented, expected);
}

function json(data: unknown, status = 200): Response {
	return new Response(JSON.stringify(data), { status, headers: { "content-type": "application/json" } });
}

function sse(name: string, data: unknown): string {
	return `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;
}

function schemaMismatch(v: number | undefined): boolean {
	return v != null && v !== SCHEMA_VERSION;
}

/** True when this string is a usable agent id (sanitizes to a safe filename). */
function validAgentId(agent: string): boolean {
	try {
		sanitizeAgentId(agent);
		return true;
	} catch {
		return false;
	}
}

/** ?agent=<id> for the GET routes; null when missing or unusable. */
function agentParam(url: URL): string | null {
	const a = url.searchParams.get("agent")?.trim();
	return a && validAgentId(a) ? a : null;
}

/** The shared validation-ladder head for the persistence POST/PATCH routes:
 * JSON parse guard -> shape guard (object + agent string) -> schema_version
 * guard. Route-specific field guards follow at each call site. */
async function parseAgentBody(req: Request): Promise<{ body: Record<string, unknown>; agent: string } | { error: Response }> {
	let raw: unknown;
	try {
		raw = await req.json();
	} catch {
		return { error: json({ error: "invalid JSON body" }, 400) };
	}
	if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
		return { error: json({ error: "invalid request body" }, 400) };
	}
	const body = raw as Record<string, unknown>;
	const agent = typeof body.agent === "string" ? body.agent.trim() : "";
	if (!agent || !validAgentId(agent)) {
		return { error: json({ error: "invalid request: agent (string) required" }, 400) };
	}
	if (body.schema_version != null && body.schema_version !== SCHEMA_VERSION) {
		return { error: json({ error: "schema_version mismatch" }, 400) };
	}
	return { body, agent };
}

export function createServer(opts: ServerOptions = {}) {
	const buildStream: BuildStream = opts.buildStream ?? (streamWorkflow as BuildStream);
	const store = opts.store ?? new AgentStore();
	// Scheduling lives in the BROKER's scheduler registry (cron, revisions, run
	// history) — it calls POST /routines/run on each fire. No interval here.
	const sessions = opts.sessions ?? new PiSessions(defaultDataDir());
	const resolveAuthoring = opts.authoring ?? resolveToolAuthoring;
	const hostname = (opts.hostname ?? process.env.WUPHF_AGENT_HOST ?? "127.0.0.1").trim();
	const authToken = (opts.authToken ?? process.env.WUPHF_AGENT_TOKEN ?? "").trim();

	// Fail closed: exposing a code-running service off-loopback without a token is
	// unauthenticated remote code execution. Refuse to boot that configuration
	// rather than silently listening on 0.0.0.0 with no auth.
	if (!isLoopbackHost(hostname) && authToken === "") {
		throw new Error("refusing to bind non-loopback host without WUPHF_AGENT_TOKEN (would be unauthenticated RCE)");
	}
	if (authToken === "") {
		console.warn("[wuphf-agent] WUPHF_AGENT_TOKEN is unset — service is UNAUTHENTICATED and bound to loopback only.");
	}

	return Bun.serve({
		port: opts.port ?? Number(process.env.PORT ?? 8820),
		hostname,
		async fetch(req) {
			const url = new URL(req.url);
			const { pathname } = url;

			// Liveness stays open. When a token is configured every other route
			// requires it; when it is not, the service runs unauthenticated but is
			// loopback-only (a non-loopback bind without a token is refused at
			// startup), so an open dev instance is never reachable off-box.
			if (req.method === "GET" && pathname === "/health") return json({ status: "ok", version: "0.0.1" });
			if (authToken !== "" && !authorized(req, authToken)) return json({ error: "unauthorized" }, 401);

			if (req.method === "GET" && pathname === "/providers") return json(providersPayload());

			if (req.method === "POST" && pathname === "/build/stream") {
				// Guard the parse BEFORE the stream starts: an unparseable body must be a
				// plain 400, not a 500 (and never a half-open SSE response).
				let body: BuildRequest;
				try {
					body = (await req.json()) as BuildRequest;
				} catch {
					return json({ error: "invalid JSON body" }, 400);
				}
				// Parseable JSON is not enough: null/[]/"x" cast to BuildRequest then read
				// as undefined. Validate the shape before dereferencing.
				if (!body || typeof body !== "object" || typeof body.message !== "string") {
					return json({ error: "invalid build request: message (string) required" }, 400);
				}
				if (schemaMismatch(body.schema_version)) return json({ error: "schema_version mismatch" }, 400);
				const stream = new ReadableStream({
					async start(controller) {
						const enc = new TextEncoder();
						controller.enqueue(enc.encode(sse("start", { message: body.message })));
						try {
							// Thread the request's abort signal so a dropped client tears the build down.
							for await (const ev of buildStream(body.message, { toolId: body.tool_id, signal: req.signal })) {
								controller.enqueue(enc.encode(sse(ev.type === "spec" ? "spec" : "step", ev)));
							}
						} catch (e) {
							controller.enqueue(enc.encode(sse("error", { message: String(e) })));
						}
						controller.close();
					},
				});
				return new Response(stream, { headers: { "content-type": "text/event-stream", "cache-control": "no-cache" } });
			}

			if (req.method === "POST" && pathname === "/tools/build") {
				// The app's chat teaches a workflow; the agent calls create_tool and
				// returns the tool it made, so the FE renders the call and lists it.
				let body: ToolBuildRequest;
				try {
					body = (await req.json()) as ToolBuildRequest;
				} catch {
					return json({ error: "invalid JSON body" }, 400);
				}
				if (!body || typeof body !== "object" || typeof body.message !== "string") {
					return json({ error: "invalid tool build request: message (string) required" }, 400);
				}
				if (schemaMismatch(body.schema_version)) return json({ error: "schema_version mismatch" }, 400);
				// Authoring resolves per-host (serviceAuthor.ts): subscription CLI,
				// API key, or Ollama when available; TOOL_AUTHOR_MODEL=0 forces the
				// deterministic stub (tests), =1 keeps the Ollama-harness override.
				// With nothing available the endpoint still answers FAST via the stub.
				const app = typeof body.app === "string" ? body.app.trim() : "";
				if (app && !validAgentId(app)) return json({ error: "invalid tool build request: unusable app id" }, 400);
				const authoring = resolveAuthoring();
				// Steer the authored tool onto the app's REAL tables: fetch its
				// schema so data.* calls use the names the app already wrote.
				const buildSchema = app ? await fetchAppSchema({ ...capabilityConfigFromEnv(), appId: app }) : "";
				const outcome = await buildTool(body.message, { ...(authoring ?? {}), tryModel: authoring != null, signal: req.signal, appSchema: buildSchema });
				console.log(`tools/build authored_by=${outcome.authored_by} via=${authoring?.via ?? "none"} app=${app || "-"}`);
				// `app` set -> PERSIST the authored tool under that agent (re-authoring a
				// same-named tool bumps version); the response tool carries `version`.
				// Stub-authored tools are NEVER persisted: a canned template landing in
				// the operator's Tools list as if it were their workflow was the worst
				// finding of the 2026-08-15 QA pass. The stub still rides the response
				// (labeled) so harness callers keep a deterministic shape.
				// `via` tells the caller WHY a stub came back: "none" means nothing
				// is configured (connect-a-model copy is right); anything else means
				// the model was tried and failed (retry copy is right). Conflating
				// the two told operators to "connect Claude Code" on machines where
				// it was already connected (2026-08-17 eval8 finding).
				const via = authoring?.via ?? "none";
				if (app && outcome.tool && outcome.authored_by === "model") {
					return json({ ...outcome, via, tool: store.upsertTool(app, outcome.tool) });
				}
				return json({ ...outcome, via });
			}

			if (req.method === "GET" && pathname === "/tools") {
				const agent = agentParam(url);
				if (!agent) return json({ error: "agent query param required" }, 400);
				return json({ tools: store.listTools(agent) });
			}

			if (req.method === "POST" && pathname === "/tools/call") {
				// The app's chat CALLS a saved tool. The tool CODE is resolved from the
				// per-agent store by (agent, name) and is NEVER taken from the request
				// body — a caller supplies inputs/args, not code, so the endpoint can
				// only run tools the agent authored and persisted. Execution is still
				// sandboxed (toolRuntime.ts); a gated capability halts with
				// needs_approval unless the request carries approved=true (CQ1).
				let body: ToolCallRequest;
				try {
					body = (await req.json()) as ToolCallRequest;
				} catch {
					return json({ error: "invalid JSON body" }, 400);
				}
				if (!body || typeof body !== "object") {
					return json({ error: "invalid tool call request" }, 400);
				}
				if (schemaMismatch(body.schema_version)) return json({ error: "schema_version mismatch" }, 400);
				const agent = typeof body.agent === "string" ? body.agent.trim() : "";
				if (!agent || !validAgentId(agent)) {
					return json({ error: "invalid tool call request: agent (string) required" }, 400);
				}
				const name = typeof body.name === "string" ? body.name.trim() : "";
				if (!name) {
					return json({ error: "invalid tool call request: name (string) required" }, 400);
				}
				const tool = store.getTool(agent, name);
				if (!tool) {
					return json({ error: `no such tool: ${name}` }, 404);
				}
				const rawInputs = tool.inputs;
				// Capabilities compose per host env: real broker/model seams when
				// configured (WUPHF_BROKER_URL/TOKEN, TOOL_RUNTIME_MODEL=1), simulated
				// otherwise. TOOL_CALL_TIMEOUT_MS raises the hard-kill deadline for
				// hosts running slow capabilities (e.g. nex.browser drives real Chrome).
				// Same positive-finite guard as capabilityConfigFromEnv, so the worker
				// deadline and the capability call bound never diverge on a bad value.
				const rawTimeoutMs = Number(process.env.TOOL_CALL_TIMEOUT_MS);
				const timeoutMs = Number.isFinite(rawTimeoutMs) && rawTimeoutMs > 0 ? rawTimeoutMs : undefined;
				return json(
					await runTool({ ...tool, inputs: rawInputs === undefined ? [] : rawInputs }, body.args ?? {}, {
						approved: body.approved === true,
						// Pass the app id so data.* binds to THIS app's real store.
						capabilities: buildCapabilities({ ...capabilityConfigFromEnv(), ...runtimeAICapabilityConfig(), appId: agent }),
						timeoutMs,
					}),
				);
			}

			if (req.method === "POST" && pathname === "/run") {
				let body: RunRequest;
				try {
					body = (await req.json()) as RunRequest;
				} catch {
					return json({ error: "invalid JSON body" }, 400);
				}
				if (!body || typeof body !== "object" || !body.spec || typeof body.spec !== "object") {
					return json({ error: "invalid run request: spec (object) required" }, 400);
				}
				if (schemaMismatch(body.schema_version)) return json({ error: "schema_version mismatch" }, 400);
				return json(await runWorkflow(body.spec, body.input ?? {}));
			}

			// ----- Routines (definitions live in the BROKER's scheduler registry;
			// this endpoint is what a broker fire / run-now calls) -----

			if (req.method === "POST" && pathname === "/routines/run") {
				const parsed = await parseAgentBody(req);
				if ("error" in parsed) return parsed.error;
				const { body, agent } = parsed;
				if (
					typeof body.slug !== "string" ||
					!body.slug.trim() ||
					typeof body.name !== "string" ||
					!body.name.trim() ||
					typeof body.prompt !== "string" ||
					!body.prompt.trim()
				) {
					return json({ error: "invalid routine run: slug, name, prompt (strings) required" }, 400);
				}
				// runRoutine executes with approved: false (SEND-GATE, default deny):
				// a gated routine records needs_approval into its transcript — it
				// never auto-sends. Run status history lands in the broker's ring.
				// Routines author through the same per-host resolution as /tools/build,
				// so a machine with a real model authors real tools on routine fires too.
				const routineAuthoring = resolveAuthoring();
				// A routine that authors a tool on the fly must also target the app's
				// real tables — fetch the schema once and pass it to the author.
				const routineSchema = await fetchAppSchema({ ...capabilityConfigFromEnv(), appId: agent });
				const result = await runRoutine(
					agent,
					{ slug: body.slug.trim(), name: body.name.trim(), prompt: body.prompt },
					{
						store,
						sessions,
						author: (p) => buildTool(p, { ...(routineAuthoring ?? {}), tryModel: routineAuthoring != null, appSchema: routineSchema }),
					},
				);
				return json({ status: result.status, digest: result.outcome, session_id: result.session.id });
			}

			// ----- Chat sessions (pi SessionManager JSONL; chat logic stays FE-side) -----

			if (req.method === "GET" && pathname === "/sessions") {
				const agent = agentParam(url);
				if (!agent) return json({ error: "agent query param required" }, 400);
				return json({ sessions: await sessions.list(agent) });
			}

			const sessionGet = req.method === "GET" ? /^\/sessions\/([^/]+)$/.exec(pathname) : null;
			if (sessionGet) {
				const agent = agentParam(url);
				if (!agent) return json({ error: "agent query param required" }, 400);
				const found = await sessions.get(agent, sessionGet[1]);
				if (!found) return json({ error: "session not found" }, 404);
				return json({ session: found.session, messages: found.messages });
			}

			if (req.method === "POST" && pathname === "/sessions") {
				const parsed = await parseAgentBody(req);
				if ("error" in parsed) return parsed.error;
				const { body, agent } = parsed;
				if (body.title !== undefined && typeof body.title !== "string") {
					return json({ error: "invalid session request: title must be a string" }, 400);
				}
				const n = (await sessions.list(agent)).filter((s) => s.kind === "manual").length + 1;
				const title = typeof body.title === "string" && body.title.trim() ? body.title.trim() : `Chat ${n}`;
				return json({ session: sessions.create(agent, title, "manual") });
			}

			const sessionMsg = req.method === "POST" ? /^\/sessions\/([^/]+)\/message$/.exec(pathname) : null;
			if (sessionMsg) {
				const parsed = await parseAgentBody(req);
				if ("error" in parsed) return parsed.error;
				const { body, agent } = parsed;
				if ((body.from !== "you" && body.from !== "nex") || typeof body.body !== "string") {
					return json({ error: 'invalid message: from ("you"|"nex") and body (string) required' }, 400);
				}
				const appended = await sessions.append(agent, sessionMsg[1], { from: body.from, body: body.body, at: new Date().toISOString() });
				if (!appended) return json({ error: "session not found" }, 404);
				return json({ ok: true });
			}

			// ----- Artifacts (routine runs always save their outcome here) -----

			if (req.method === "GET" && pathname === "/artifacts") {
				const agent = agentParam(url);
				if (!agent) return json({ error: "agent query param required" }, 400);
				return json({ artifacts: store.listArtifacts(agent) });
			}

			return json({ error: "not found" }, 404);
		},
	});
}

if (import.meta.main) {
	const server = createServer();
	console.log(`wuphf-agent service on ${server.url}`);
}
