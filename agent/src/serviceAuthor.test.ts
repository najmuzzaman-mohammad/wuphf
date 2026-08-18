// resolveToolAuthoring picks the best authoring path for the host — and stays
// OFF (undefined -> stub) when nothing is available, so an unconfigured
// deployment never eats a model timeout.

import { expect, test } from "bun:test";
import type { Provider } from "./providers.js";
import { resolveToolAuthoring, runtimeAICapabilityConfig } from "./serviceAuthor.js";

const none: Provider[] = [
	{ id: "anthropic", label: "Anthropic", available: false, via: "none" },
	{ id: "codex", label: "OpenAI / Codex", available: false, via: "none" },
	{ id: "ollama", label: "Ollama", available: false, via: "none" },
];
function withProviders(patch: Partial<Record<string, Provider["via"]>>): () => Provider[] {
	return () =>
		none.map((p) => (patch[p.id] ? { ...p, available: true, via: patch[p.id] as Provider["via"] } : p));
}

test("TOOL_AUTHOR_MODEL=0 forces authoring off even with providers available", () => {
	expect(resolveToolAuthoring({ TOOL_AUTHOR_MODEL: "0" }, withProviders({ anthropic: "subscription_cli" }))).toBeUndefined();
});

test("TOOL_AUTHOR_MODEL=1 keeps the Ollama-harness override", () => {
	expect(resolveToolAuthoring({ TOOL_AUTHOR_MODEL: "1" }, withProviders({}))?.via).toBe("env_override");
});

test("claude CLI resolves as the subscription path with a complete fn", () => {
	const r = resolveToolAuthoring({}, withProviders({ anthropic: "subscription_cli" }));
	expect(r?.via).toBe("claude_cli");
	expect(typeof r?.complete).toBe("function");
	expect(r?.model?.provider).toBe("anthropic");
});

test("ANTHROPIC_API_KEY outranks the CLI; ollama is the last resort; none -> stub", () => {
	expect(resolveToolAuthoring({}, withProviders({ anthropic: "api_key" }))?.via).toBe("api_key");
	expect(resolveToolAuthoring({}, withProviders({ ollama: "local" }))?.via).toBe("ollama");
	expect(resolveToolAuthoring({}, () => none)).toBeUndefined();
});

// runtimeAICapabilityConfig reuses the authoring resolver so runtime nex.ai.*
// runs on the SAME engine that authored the tool — the fix for a subscription
// operator whose authored tool otherwise emits "nothing actually ran" stubs.
test("runtimeAICapabilityConfig: subscription CLI -> real model + complete fn", () => {
	const cfg = runtimeAICapabilityConfig({}, withProviders({ anthropic: "subscription_cli" }));
	expect(cfg.aiModel?.provider).toBe("anthropic");
	expect(typeof cfg.complete).toBe("function");
});

test("runtimeAICapabilityConfig: nothing available -> {} (nex.ai.* stays simulated)", () => {
	const cfg = runtimeAICapabilityConfig({}, () => none);
	expect(cfg.aiModel).toBeUndefined();
	expect(cfg.complete).toBeUndefined();
});

test("runtimeAICapabilityConfig: TOOL_AUTHOR_MODEL=1 resolves the core model", () => {
	const cfg = runtimeAICapabilityConfig({ TOOL_AUTHOR_MODEL: "1" }, withProviders({}));
	expect(cfg.aiModel).toBeDefined();
});
