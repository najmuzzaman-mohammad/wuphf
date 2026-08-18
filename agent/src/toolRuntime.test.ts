import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, beforeAll, expect, test } from "bun:test";
import { currentRunSignal } from "./runContext.js";
import { createServer } from "./service.js";
import { AgentStore } from "./store.js";
import { type CapabilityTree, runTool } from "./toolRuntime.js";
import type { Tool, ToolCallResult, WorkflowSpec } from "./wire.js";

// Tool fixtures are inlined (not imported from tools.ts) so this file has no
// coupling to the authoring module while it is edited in parallel.

function makeTool(code: string, inputs: Tool["inputs"] = [], name = "t"): Tool {
	return { name, title: "T", purpose: "p", inputs, code };
}

const READ_TOOL = makeTool(
	[
		"async function weeklySummary() {",
		"  const records = await data.list('records', { since: '7d' });",
		"  return nex.ai.summarize(records, { style: 'concise recap' });",
		"}",
	].join("\n"),
	[],
	"read_tool",
);

const GATED_TOOL = makeTool(
	[
		"async function notifyOwner(record) {",
		"  const result = await nex.send(record, 'heads up');",
		"  return `notified ${record}: ${result}`;",
		"}",
	].join("\n"),
	[{ name: "record", type: "string" }],
	"gated_tool",
);

test("a read tool (data.list + summarize) runs ok with actions recorded", async () => {
	const r = await runTool(READ_TOOL, {});
	expect(r.status).toBe("ok");
	if (r.status !== "ok") throw new Error("unreachable");
	// data.list returns [] in the sim, summarize records "0 items" honestly.
	expect(r.actions.some((a) => a.startsWith("data.list("))).toBe(true);
	expect(r.actions.some((a) => a.startsWith("nex.ai.summarize("))).toBe(true);
});

test("a gated write (nex.send) halts needs_approval by default (default deny)", async () => {
	const r = await runTool(GATED_TOOL, { record: "Acme" });
	expect(r.status).toBe("needs_approval");
	if (r.status !== "needs_approval") throw new Error("unreachable");
	expect(r.gate.capability).toBe("nex.send");
	expect(r.gate.detail).toContain("Acme");
});

test("the same gated call with approved: true completes", async () => {
	const r = await runTool(GATED_TOOL, { record: "Acme" }, { approved: true });
	expect(r.status).toBe("ok");
	if (r.status !== "ok") throw new Error("unreachable");
	expect(r.actions.some((a) => a.startsWith("nex.send("))).toBe(true);
});

test("nex.send is gated too", async () => {
	const t = makeTool('async function ping() { return nex.send("#sales", "hi"); }');
	const denied = await runTool(t, {});
	expect(denied.status).toBe("needs_approval");
	if (denied.status !== "needs_approval") throw new Error("unreachable");
	expect(denied.gate.capability).toBe("nex.send");
	const sent = await runTool(t, {}, { approved: true });
	expect(sent.status).toBe("ok");
});

test("a thrown error -> status error (with prior actions kept)", async () => {
	const t = makeTool('async function boom() { await data.list("records"); throw new Error("kaput"); }');
	const r = await runTool(t, {});
	expect(r.status).toBe("error");
	if (r.status !== "error") throw new Error("unreachable");
	expect(r.detail).toContain("kaput");
	expect(r.actions.some((a) => a.startsWith("data.list("))).toBe(true);
});

test("the code scan rejects import (dynamic and static) and eval", async () => {
	for (const code of [
		'async function t() { const fs = await import("fs"); return "x"; }',
		'import fs from "fs";\nasync function t() { return "x"; }',
		'async function t() { return eval("1+1"); }',
	]) {
		const r = await runTool(makeTool(code), {});
		expect(r.status).toBe("error");
	}
});

test("dangerous globals are shadowed to undefined inside tool code", async () => {
	const t = makeTool("async function t() { return String(typeof fetch) + '/' + String(typeof process); }");
	const r = await runTool(t, {});
	expect(r.status).toBe("ok");
	if (r.status !== "ok") throw new Error("unreachable");
	expect(r.result).toBe("undefined/undefined");
});

test("timeout: a never-resolving capability -> error", async () => {
	const capabilities: CapabilityTree = {
		nex: { run: () => new Promise(() => {}) },
		crm: {},
	};
	const t = makeTool("async function t(input) { return nex.run(input); }", [{ name: "input", type: "string" }]);
	const r = await runTool(t, { input: "x" }, { capabilities, timeoutMs: 20 });
	expect(r.status).toBe("error");
	if (r.status !== "error") throw new Error("unreachable");
	expect(r.detail).toContain("took too long");
});

test("HARD KILL: a synchronous infinite loop dies at the deadline (worker isolate)", async () => {
	// The old in-process sandbox could only stop WAITING (Promise.race); a sync
	// loop hung the service forever. The worker isolate terminates the thread.
	const t = makeTool("function t() { while (true) {} }");
	const started = Date.now();
	const r = await runTool(t, {}, { timeoutMs: 150 });
	const elapsed = Date.now() - started;
	expect(r.status).toBe("error");
	if (r.status !== "error") throw new Error("unreachable");
	expect(r.detail).toContain("took too long");
	expect(elapsed).toBeLessThan(2000); // returned promptly — the loop was killed, not raced
});

test("a settled run ABORTS in-flight capability calls (deferred finding [15])", async () => {
	// A never-resolving capability records the ambient run signal; when the run
	// times out, that signal must abort so a real broker fetch / model call in
	// flight is torn down, not left burning after the worker died.
	let seen: AbortSignal | undefined;
	const capabilities: CapabilityTree = {
		nex: {
			run: () =>
				new Promise(() => {
					seen = currentRunSignal();
				}),
		},
	};
	const t = makeTool("async function t() { return nex.run('x'); }");
	const r = await runTool(t, {}, { capabilities, timeoutMs: 30 });
	expect(r.status).toBe("error");
	expect(seen).toBeDefined();
	expect(seen?.aborted).toBe(true);
	expect(String(seen?.reason)).toContain("tool run settled");
});

test("the run signal is NOT aborted while capabilities execute mid-run", async () => {
	let abortedDuring: boolean | undefined;
	const capabilities: CapabilityTree = {
		nex: {
			run: () => {
				abortedDuring = currentRunSignal()?.aborted;
				return "fine";
			},
		},
	};
	const t = makeTool("async function t() { return nex.run('x'); }");
	const r = await runTool(t, {}, { capabilities });
	expect(r.status).toBe("ok");
	expect(abortedDuring).toBe(false);
});

test("nex.browser is gated: default deny with the browser-control detail", async () => {
	const t = makeTool('async function t() { return nex.browser("post the digest to the vendor portal"); }');
	const r = await runTool(t, {});
	expect(r.status).toBe("needs_approval");
	if (r.status !== "needs_approval") throw new Error("unreachable");
	expect(r.gate.capability).toBe("nex.browser");
	expect(r.gate.detail).toContain("control your browser");
});

test("injected capabilities are used AND recorded (and stay gated)", async () => {
	const seen: string[] = [];
	const capabilities: CapabilityTree = {
		data: {
			list: () => {
				seen.push("list");
				return [{ name: "OnlyRecord" }];
			},
		},
		nex: {
			ai: { summarize: (items: unknown) => `got ${(items as unknown[]).length}` },
			send: () => "sent",
		},
	};
	const read = makeTool("async function t() { const d = await data.list('records', { since: '7d' }); return nex.ai.summarize(d); }");
	const r = await runTool(read, {}, { capabilities });
	expect(r.status).toBe("ok");
	if (r.status !== "ok") throw new Error("unreachable");
	expect(r.result).toBe("got 1");
	expect(seen).toEqual(["list"]);
	expect(r.actions[0]).toBe('data.list("records", {"since":"7d"})');
	// The gate is enforced at the instrumentation layer, so an injected
	// nex.send is still default-deny.
	const gated = makeTool('async function t() { return nex.send("Acme", "hi"); }');
	const g = await runTool(gated, {}, { capabilities });
	expect(g.status).toBe("needs_approval");
});

test("an input name that collides with the sandbox is rejected", async () => {
	const t = makeTool("async function t(fetch) { return fetch; }", [{ name: "fetch", type: "string" }]);
	const r = await runTool(t, { fetch: "x" });
	expect(r.status).toBe("error");
	if (r.status !== "error") throw new Error("unreachable");
	expect(r.detail).toContain("invalid tool input name");
});

// ---------------------------------------------------------------------------
// Service-level: POST /tools/call
// ---------------------------------------------------------------------------

async function* fakeBuild() {
	yield {
		type: "spec" as const,
		spec: { name: "n", tool_id: "t", narration: "", clarify: null, steps: [] } as WorkflowSpec,
	};
}

// The tool CODE is resolved from the per-agent store by (agent, name); the
// request body carries only a reference + args, never code (that path was
// unauthenticated RCE — see wire.ts ToolCallRequest). Seed the store so the
// service can look the fixtures up.
const APP = "app1";
let server: ReturnType<typeof createServer>;
let base: string;
let priorAuthorModel: string | undefined;
beforeAll(() => {
	// These HTTP tests exercise the /tools/call plumbing against the SIMULATED
	// runtime, not a live model. Pin authoring off so runtimeAICapabilityConfig
	// does not resolve an ambient provider (a CI runner with ANTHROPIC_API_KEY
	// would otherwise make nex.ai.* fire a real, slow network call and this test
	// would flake on a socket timeout). Restored in afterAll.
	priorAuthorModel = process.env.TOOL_AUTHOR_MODEL;
	process.env.TOOL_AUTHOR_MODEL = "0";
	const store = new AgentStore(mkdtempSync(join(tmpdir(), "wuphf-toolcall-")));
	store.upsertTool(APP, READ_TOOL);
	store.upsertTool(APP, GATED_TOOL);
	server = createServer({ port: 0, buildStream: fakeBuild, store });
	base = server.url.toString().replace(/\/$/, "");
});
afterAll(() => {
	if (priorAuthorModel === undefined) delete process.env.TOOL_AUTHOR_MODEL;
	else process.env.TOOL_AUTHOR_MODEL = priorAuthorModel;
	server.stop(true);
});

function post(body: unknown): Promise<Response> {
	return fetch(`${base}/tools/call`, {
		method: "POST",
		headers: { "content-type": "application/json" },
		body: JSON.stringify(body),
	});
}

test("POST /tools/call runs a read tool ok", async () => {
	const res = await post({ schema_version: 1, agent: APP, name: "read_tool", args: {} });
	expect(res.status).toBe(200);
	const data = (await res.json()) as ToolCallResult;
	expect(data.status).toBe("ok");
	expect(Array.isArray(data.actions)).toBe(true);
});

test("POST /tools/call halts needs_approval, then completes with approved: true", async () => {
	const r1 = (await (await post({ schema_version: 1, agent: APP, name: "gated_tool", args: { record: "Acme" } })).json()) as ToolCallResult;
	expect(r1.status).toBe("needs_approval");
	expect(r1.gate?.capability).toBe("nex.send");
	const r2 = (await (
		await post({ schema_version: 1, agent: APP, name: "gated_tool", args: { record: "Acme" }, approved: true })
	).json()) as ToolCallResult;
	expect(r2.status).toBe("ok");
});

test("POST /tools/call never runs code from the request body (RCE regression)", async () => {
	// A body-supplied `code`/`tool` is ignored entirely: the endpoint resolves the
	// implementation from the store by (agent, name). An unknown name is a 404, so
	// there is no path to execute attacker-controlled code.
	const evil = "function pwn(){ return ([]).constructor.constructor('return process')().env.SECRET; }";
	const res = await post({ schema_version: 1, agent: APP, name: "pwn", code: evil, tool: { name: "pwn", code: evil } });
	expect(res.status).toBe(404);
});

test("POST /tools/call 400s on a missing agent/name and 404s on an unknown tool", async () => {
	for (const bad of [{}, { agent: APP }, { name: "read_tool" }, { agent: "", name: "read_tool" }, { agent: APP, name: "" }]) {
		const res = await post(bad);
		expect(res.status).toBe(400);
	}
	const unknown = await post({ agent: APP, name: "does_not_exist" });
	expect(unknown.status).toBe(404);
	const notJson = await fetch(`${base}/tools/call`, {
		method: "POST",
		headers: { "content-type": "application/json" },
		body: "{nope",
	});
	expect(notJson.status).toBe(400);
});

test("POST /tools/call rejects a schema_version mismatch", async () => {
	const res = await post({ schema_version: 99, agent: APP, name: "read_tool" });
	expect(res.status).toBe(400);
});
