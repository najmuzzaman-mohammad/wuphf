// The tool sandbox worker: runs a tool's code in a SEPARATE thread so the host
// can hard-kill it (worker.terminate()) — a synchronous infinite loop dies at the
// timeout instead of hanging the service (the fix for the old cooperative
// Promise.race timeout). Capabilities are NOT implemented here: every capability
// call is an RPC to the host (postMessage), where instrumentation records the
// action and the send-gate enforces default-deny. The worker holds no broker
// token, no model key, and no capability implementations.
//
// Residual ambient authority (fetch etc. exist in a worker) is still reduced by
// strict-mode parameter shadowing + the host's code scan — same defense-in-depth
// as before; the hard containment this file adds is TERMINATION + no shared state
// with the service. TODO(security): a permissioned runtime for full authority
// stripping.

interface RunMsg {
	t: "run";
	code: string;
	fnName: string;
	inputNames: string[];
	argValues: string[];
	capPaths: string[];
	shadowedGlobals: string[];
}

interface CapResMsg {
	t: "capres";
	id: number;
	ok: boolean;
	value?: unknown;
	detail?: string;
}

type HostMsg = RunMsg | CapResMsg;

let seq = 0;
const pending = new Map<number, { resolve: (v: unknown) => void; reject: (e: Error) => void }>();

function preview(v: unknown): string {
	let s: string;
	try {
		s = v === undefined ? "undefined" : JSON.stringify(v);
	} catch {
		s = String(v);
	}
	return s.length > 60 ? `${s.slice(0, 57)}…` : s;
}

/** Drop non-cloneable values (functions etc.) before they cross postMessage. */
function plain(v: unknown): unknown {
	try {
		return v === undefined ? undefined : JSON.parse(JSON.stringify(v));
	} catch {
		return preview(v);
	}
}

/** Serialize a tool's FINAL return value for the operator. Unlike preview()
 * (a 60-char label for action tracing), this preserves the whole output — an
 * authored tool that returns a structured summary object must reach the
 * execution log intact, not clipped to 57 chars. Bounded only to keep a
 * pathological payload from bloating the log / postMessage. */
function serializeResult(v: unknown): string {
	if (typeof v === "string") return v;
	let s: string;
	try {
		s = v === undefined ? "" : JSON.stringify(v);
	} catch {
		s = String(v);
	}
	const MAX = 200_000;
	return s.length > MAX ? `${s.slice(0, MAX)}… (result truncated)` : s;
}

/** Rebuild the capability tree from dotted paths as RPC proxies to the host. */
function buildProxyTree(capPaths: string[]): Record<string, unknown> {
	const root: Record<string, unknown> = {};
	for (const path of capPaths) {
		const parts = path.split(".");
		let node = root;
		for (let i = 0; i < parts.length - 1; i++) {
			node[parts[i]] = node[parts[i]] ?? {};
			node = node[parts[i]] as Record<string, unknown>;
		}
		node[parts[parts.length - 1]] = (...args: unknown[]) =>
			new Promise((resolve, reject) => {
				seq += 1;
				pending.set(seq, { resolve, reject });
				postMessage({ t: "cap", id: seq, path, args: args.map(plain) });
			});
	}
	return root;
}

self.onmessage = (ev: MessageEvent) => {
	const msg = ev.data as HostMsg;
	if (msg.t === "capres") {
		const p = pending.get(msg.id);
		if (!p) return;
		pending.delete(msg.id);
		if (msg.ok) p.resolve(msg.value);
		else p.reject(new Error(msg.detail ?? "capability failed"));
		return;
	}
	if (msg.t !== "run") return;

	void (async () => {
		try {
			// Re-validate every identifier interpolated into the compile at THIS
			// boundary too (the host already did; defense in depth): only tool CODE is
			// allowed to be free-form — names must be plain identifiers.
			const IDENT = /^[A-Za-z_$][A-Za-z0-9_$]*$/;
			for (const name of [msg.fnName, ...msg.inputNames, ...msg.shadowedGlobals]) {
				if (!IDENT.test(name)) throw new Error(`invalid identifier: ${JSON.stringify(name)}`);
			}
			const caps = buildProxyTree(msg.capPaths);
			const capRoots = Object.keys(caps);
			// Strict mode + ONLY the allowed bindings as parameters; dangerous globals
			// shadowed by trailing undefined parameters (host already scan-rejected
			// import/eval). The code is agent-authored, never operator-typed — running
			// it is this worker's purpose; containment is termination + capability RPC.
			const compiled = new Function(
				...capRoots,
				...msg.inputNames,
				...msg.shadowedGlobals,
				`"use strict";\n${msg.code}\nreturn ${msg.fnName}(${msg.inputNames.join(", ")});`,
			);
			const value: unknown = await compiled(
				...capRoots.map((n) => caps[n]),
				...msg.argValues,
				...msg.shadowedGlobals.map(() => undefined),
			);
			postMessage({ t: "done", result: serializeResult(value) });
		} catch (e) {
			postMessage({ t: "err", detail: e instanceof Error ? e.message : String(e) });
		}
	})();
};
