import { expect, test } from "bun:test";
import { buildCapabilities, type CapabilityConfig, capabilityConfigFromEnv, GATED_CAPABILITIES } from "./capabilities.js";
import type { CapabilityFn, CapabilityTree } from "./toolRuntime.js";

function cap(tree: CapabilityTree, path: string): CapabilityFn {
	let node: CapabilityTree | CapabilityFn = tree;
	for (const part of path.split(".")) node = (node as CapabilityTree)[part];
	return node as CapabilityFn;
}

type FetchFn = NonNullable<CapabilityConfig["fetch"]>;
type CompleteFn = NonNullable<CapabilityConfig["complete"]>;

function jsonFetch(body: unknown, status = 200): FetchFn {
	return (async () => new Response(JSON.stringify(body), { status })) as unknown as FetchFn;
}

function fakeComplete(text: string): CompleteFn {
	return (async () => ({ content: [{ type: "text", text }] })) as unknown as CompleteFn;
}

const MODEL = { id: "test-model" } as unknown as NonNullable<CapabilityConfig["aiModel"]>;

// --- composition ---------------------------------------------------------------

test("unconfigured host: integrations.call throws an explanatory error", async () => {
	const tree = buildCapabilities({});
	await expect(Promise.resolve(cap(tree, "integrations.call")("gmail", "GMAIL_FETCH_EMAILS"))).rejects.toThrow(
		/not connected yet/,
	);
});

test("unconfigured host: nex.browser degrades to a simulated marker", async () => {
	const tree = buildCapabilities({});
	const out = await cap(tree, "nex.browser")("open the vendor portal");
	expect(String(out)).toContain("browser engine not configured");
});

test("capabilityConfigFromEnv reads the broker seam + model gate", () => {
	const cfg = capabilityConfigFromEnv({
		WUPHF_BROKER_URL: "http://127.0.0.1:7893",
		WUPHF_BROKER_TOKEN: "tok",
	});
	expect(cfg.brokerUrl).toBe("http://127.0.0.1:7893");
	expect(cfg.brokerToken).toBe("tok");
	expect(cfg.aiModel).toBeUndefined(); // TOOL_RUNTIME_MODEL unset -> simulated ai
	expect(cfg.callTimeoutMs).toBeUndefined(); // TOOL_CALL_TIMEOUT_MS unset -> default cap
});

test("capabilityConfigFromEnv threads TOOL_CALL_TIMEOUT_MS into callTimeoutMs", () => {
	expect(capabilityConfigFromEnv({ TOOL_CALL_TIMEOUT_MS: "120000" }).callTimeoutMs).toBe(120_000);
	// Garbage / non-positive values fall back to the default (unset).
	expect(capabilityConfigFromEnv({ TOOL_CALL_TIMEOUT_MS: "soon" }).callTimeoutMs).toBeUndefined();
	expect(capabilityConfigFromEnv({ TOOL_CALL_TIMEOUT_MS: "0" }).callTimeoutMs).toBeUndefined();
	expect(capabilityConfigFromEnv({ TOOL_CALL_TIMEOUT_MS: "-5" }).callTimeoutMs).toBeUndefined();
});

test("GATED_CAPABILITIES lists every mutating capability the send-gate must hold", () => {
	// toolRuntime.ts default-allows anything NOT in this set — these outbound
	// paths disappearing from it would silently un-gate external mutations.
	expect(GATED_CAPABILITIES.has("nex.send")).toBe(true);
	expect(GATED_CAPABILITIES.has("nex.browser")).toBe(true);
	// The catalog is domain-neutral now: no crm.* capability exists to gate.
	expect(GATED_CAPABILITIES.has("crm.assign")).toBe(false);
});

// --- simulated fallbacks (empty input honesty) ----------------------------------

test("simulated nex.run with a blank input is honest, not 'Ran on  (simulated).'", async () => {
	// QA HIGH-1: a routine ran a generic tool with no bound input, so nex.run("")
	// interpolated an empty string -> "Ran on  (simulated)." (double space, zero
	// information). The digest must not carry that string and must say WHY nothing
	// ran (no model/broker connected).
	const tree = buildCapabilities({});
	const out = String(await cap(tree, "nex.run")(""));
	expect(out).not.toContain("Ran on  (simulated)."); // the double-space bug
	expect(out).not.toMatch(/on\s{2,}/); // no empty interpolation anywhere
	expect(out.toLowerCase()).toContain("simulated");
	expect(out).toContain("no AI model"); // honest about why
});

test("simulated nex.run names the input it would have acted on", async () => {
	const tree = buildCapabilities({});
	const out = String(await cap(tree, "nex.run")("Summarize the open office tasks"));
	expect(out).toContain("Summarize the open office tasks");
	expect(out.toLowerCase()).toContain("simulated");
});

test("labelOf-backed sims never leave a blank hole for an empty argument", async () => {
	const tree = buildCapabilities({});
	// nex.send / nex.ai.write route their arg through labelOf; a blank must render
	// the neutral marker, not a double space.
	expect(String(await cap(tree, "nex.send")(""))).not.toMatch(/\s{2,}/);
	expect(String(await cap(tree, "nex.ai.write")(""))).not.toMatch(/Drafted\s{2,}/);
});

// --- real nex.ai (stubbed complete) ---------------------------------------------

test("real nex.ai.score parses the model's integer and clamps it", async () => {
	const tree = buildCapabilities({ aiModel: MODEL, complete: fakeComplete("Score: 87") });
	expect(await cap(tree, "nex.ai.score")("Acme", { rubric: "ICP fit" })).toBe(87);
});

test("real nex.ai falls back to the simulation when the model fails", async () => {
	const throwing = (async () => {
		throw new Error("provider down");
	}) as unknown as CompleteFn;
	const tree = buildCapabilities({ aiModel: MODEL, complete: throwing });
	const score = await cap(tree, "nex.ai.score")("Acme");
	expect(typeof score).toBe("number"); // deterministic hash fallback
	const recap = await cap(tree, "nex.ai.summarize")([{ name: "Globex" }]);
	expect(String(recap)).toContain("simulated recap");
});

test("real nex.ai.summarize returns the model's text", async () => {
	const tree = buildCapabilities({ aiModel: MODEL, complete: fakeComplete("6 deals moved; Globex leads.") });
	expect(await cap(tree, "nex.ai.summarize")([1, 2, 3])).toBe("6 deals moved; Globex leads.");
});

test("nex.ai.summarize feeds the FULL input to the model, not a 57-char preview", async () => {
	let captured = "";
	const capturing = (async (_m: unknown, ctx: { messages: { content: unknown }[] }) => {
		captured = String(ctx.messages?.[0]?.content ?? "");
		return { content: [{ type: "text", text: "ok" }] };
	}) as unknown as CompleteFn;
	// Meaningful content sits well past char 57; preview() used to cut everything
	// after ~57 chars, so the model saw its own input "cut off" and refused.
	const items = Array.from({ length: 12 }, (_, i) => `incident-${i}: Falcon Logistics outage breached SLA`);
	const tree = buildCapabilities({ aiModel: MODEL, complete: capturing });
	await cap(tree, "nex.ai.summarize")(items);
	expect(captured.length).toBeGreaterThan(200);
	expect(captured).toContain("incident-11");
});

// --- real integrations.call (stubbed broker) ------------------------------------

const BROKER: CapabilityConfig = { brokerUrl: "http://broker.test", brokerToken: "tok" };

test("integrations.call executes a read and returns the result", async () => {
	const tree = buildCapabilities({
		...BROKER,
		fetch: jsonFetch({ connected: true, read_only: true, result: [{ subject: "Renewal" }] }),
	});
	const out = await cap(tree, "integrations.call")("gmail", "GMAIL_FETCH_EMAILS", { max_results: 5 });
	expect(out).toEqual([{ subject: "Renewal" }]);
});

test("integrations.call surfaces the broker's approval card for a mutation", async () => {
	const tree = buildCapabilities({
		...BROKER,
		fetch: jsonFetch({ connected: true, status: "needs_approval", request_id: "req_9" }),
	});
	const out = await cap(tree, "integrations.call")("slack", "SLACK_SENDS_A_MESSAGE", {});
	expect(String(out)).toContain("Held for your approval");
	expect(String(out)).toContain("req_9");
});

test("data.* is the empty simulation without an appId", async () => {
	const tree = buildCapabilities({ ...BROKER });
	expect(await cap(tree, "data.list")("records")).toEqual([]);
	expect(await cap(tree, "data.get")("records", "x")).toBeNull();
});

test("data.list binds to the app store (query op) when an appId is set", async () => {
	let captured: { url?: string; body?: unknown } = {};
	const fetch = (async (url: string, init: { body: string }) => {
		captured = { url, body: JSON.parse(init.body) };
		return new Response(JSON.stringify({ table: { rows: [{ id: "1", name: "Meridian" }] } }), { status: 200 });
	}) as unknown as CapabilityConfig["fetch"];
	const tree = buildCapabilities({ ...BROKER, appId: "app_00000000000000aa", fetch });
	const rows = await cap(tree, "data.list")("accounts");
	expect(rows).toEqual([{ id: "1", name: "Meridian" }]);
	expect(captured.url).toBe("http://broker.test/apps/app_00000000000000aa/db");
	expect(captured.body).toMatchObject({ op: "query", table: "accounts" });
});

test("data.get finds a row by id from the app store", async () => {
	const fetch = (async () =>
		new Response(JSON.stringify({ table: { rows: [{ id: "a" }, { id: "b" }] } }), { status: 200 })) as unknown as CapabilityConfig["fetch"];
	const tree = buildCapabilities({ ...BROKER, appId: "app_00000000000000aa", fetch });
	expect(await cap(tree, "data.get")("t", "b")).toEqual({ id: "b" });
	expect(await cap(tree, "data.get")("t", "z")).toBeNull();
});

test("data.list returns [] for a table that does not exist yet (honest empty)", async () => {
	const fetch = (async () => new Response(JSON.stringify({ error: "no such table" }), { status: 404 })) as unknown as CapabilityConfig["fetch"];
	const tree = buildCapabilities({ ...BROKER, appId: "app_00000000000000aa", fetch });
	expect(await cap(tree, "data.list")("nope")).toEqual([]);
});

test("data.upsert writes a row to the app store (upsert op, key id)", async () => {
	let body: unknown;
	const fetch = (async (_url: string, init: { body: string }) => {
		body = JSON.parse(init.body);
		return new Response(JSON.stringify({ table: { rows: [] } }), { status: 200 });
	}) as unknown as CapabilityConfig["fetch"];
	const tree = buildCapabilities({ ...BROKER, appId: "app_00000000000000aa", fetch });
	await cap(tree, "data.upsert")("accounts", { id: "1", stage: "won" });
	expect(body).toMatchObject({ op: "upsert", table: "accounts", key: "id", rows: [{ id: "1", stage: "won" }] });
});

test("integrations.call throws on a broker error / disconnected platform", async () => {
	const errTree = buildCapabilities({ ...BROKER, fetch: jsonFetch({ error: "boom" }) });
	await expect(Promise.resolve(cap(errTree, "integrations.call")("gmail", "X"))).rejects.toThrow("boom");
	const discTree = buildCapabilities({ ...BROKER, fetch: jsonFetch({ connected: false }) });
	await expect(Promise.resolve(cap(discTree, "integrations.call")("gmail", "X"))).rejects.toThrow(/not connected/);
});

// --- real nex.browser (stubbed SSE) ---------------------------------------------

test("nex.browser streams the run and returns the outcome + action trace", async () => {
	const sse = [
		'data: {"type":"run","run_id":"r1"}',
		"",
		'data: {"type":"action","label":"Click New message"}',
		"",
		'data: {"type":"action","label":"Type the digest"}',
		"",
		'data: {"type":"done","result":"Posted the digest."}',
		"",
	].join("\n");
	const tree = buildCapabilities({
		...BROKER,
		fetch: (async () => new Response(sse, { status: 200 })) as unknown as FetchFn,
	});
	const out = String(await cap(tree, "nex.browser")("post the digest"));
	expect(out).toContain("Posted the digest.");
	expect(out).toContain("2 browser actions");
	expect(out).toContain("Click New message");
});

test("nex.browser fails loud when the run errors", async () => {
	const sse = ['data: {"type":"error","message":"No window for Chrome"}', ""].join("\n");
	const tree = buildCapabilities({
		...BROKER,
		fetch: (async () => new Response(sse, { status: 200 })) as unknown as FetchFn,
	});
	await expect(Promise.resolve(cap(tree, "nex.browser")("x"))).rejects.toThrow("No window for Chrome");
});
