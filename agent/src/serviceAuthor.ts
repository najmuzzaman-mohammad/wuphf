// Service-layer tool-authoring resolution. The agent CORE only knows Ollama
// (model.ts resolveModel); subscription providers resolve HERE, where the host
// machine's installed CLIs are a cheap product fact (providers.ts PATH scan)
// rather than a network guess. This is what makes "teach Nex a tool" REAL on a
// stock install: the 2026-08-15 QA pass found /tools/build silently answering
// every teach with the canned stub ("Score & route a lead") because model
// authoring was opt-in via env nobody sets.
//
// Resolution order (first available wins):
//   1. TOOL_AUTHOR_MODEL=0  -> authoring OFF (tests, constrained deployments)
//   2. TOOL_AUTHOR_MODEL=1  -> the existing Ollama-harness override (resolveModel)
//   3. ANTHROPIC_API_KEY    -> pi-ai anthropic model (pi-ai reads the key from env)
//   4. `claude` CLI on PATH -> one-shot `claude -p` completion (subscription login)
//   5. `ollama` on PATH     -> local open-weight model (core path)
//   6. nothing              -> undefined; buildTool answers with the stub, and the
//                              caller must surface authored_by="stub" honestly.
//
// Detection is a PATH/env scan (microseconds), so an unconfigured deployment
// never eats a model timeout — the original reason authoring was opt-in.

import { getModel, type Model } from "@mariozechner/pi-ai";
import { ollamaModel, resolveModel } from "./model.js";
import { detectProviders } from "./providers.js";
import type { ToolAuthorOptions } from "./tools.js";

/** Model alias handed to `claude -p`. Aliases survive CLI model renames. */
const CLAUDE_CLI_MODEL = "sonnet";

/** pi-ai model id for the ANTHROPIC_API_KEY path. */
const ANTHROPIC_API_MODEL = "claude-sonnet-4-6";

export interface ResolvedAuthoring extends ToolAuthorOptions {
	/** Which path resolution picked — logged per build so "why was this a stub?"
	 * is answerable from the service log. */
	via: "env_override" | "api_key" | "claude_cli" | "ollama";
}

/** Placeholder Model for the CLI path: authorToolWithModel requires a Model to
 * skip resolveModel(), but the CLI complete fn ignores it. */
function claudeCliModel(): Model<string> {
	return {
		id: CLAUDE_CLI_MODEL,
		name: `claude-cli:${CLAUDE_CLI_MODEL}`,
		api: "anthropic-messages",
		provider: "anthropic",
		baseUrl: "",
		reasoning: false,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 200000,
		maxTokens: 8192,
	};
}

/** One-shot completion through the Claude Code CLI (`claude -p`). Matches the
 * pi-ai `complete` seam shape closely enough for authorToolWithModel, which
 * only reads `content` text parts. The system prompt rides inline: `-p` mode
 * takes the whole prompt on stdin, and the authoring prompt is self-contained. */
async function claudeCliComplete(
	model: Model<string>,
	ctx: { systemPrompt?: string; messages: { content: unknown }[] },
	opts?: { signal?: AbortSignal },
) {
	const user = ctx.messages
		.map((m) => (typeof m.content === "string" ? m.content : ""))
		.filter(Boolean)
		.join("\n\n");
	const prompt = ctx.systemPrompt ? `${ctx.systemPrompt}\n\n---\n\n${user}` : user;
	const proc = Bun.spawn(["claude", "-p", "--model", CLAUDE_CLI_MODEL, "--output-format", "text"], {
		stdin: new TextEncoder().encode(prompt),
		stdout: "pipe",
		stderr: "pipe",
	});
	const onAbort = () => proc.kill();
	opts?.signal?.addEventListener("abort", onAbort, { once: true });
	try {
		const [text, errText, code] = await Promise.all([
			new Response(proc.stdout).text(),
			new Response(proc.stderr).text(),
			proc.exited,
		]);
		if (opts?.signal?.aborted) throw new Error("tool authoring aborted");
		if (code !== 0) throw new Error(`claude -p exited ${code}: ${errText.slice(0, 200)}`);
		// Full AssistantMessage shape so the fn satisfies the pi-ai `complete`
		// seam without unsafe casts; authorToolWithModel reads only `content`.
		const zero = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
		return {
			role: "assistant" as const,
			content: [{ type: "text" as const, text }],
			api: model.api,
			provider: model.provider,
			model: model.id,
			usage: { ...zero, totalTokens: 0, cost: { ...zero, total: 0 } },
			stopReason: "stop" as const,
			timestamp: Date.now(),
		};
	} finally {
		opts?.signal?.removeEventListener("abort", onAbort);
	}
}

/** Pick the best available authoring path for this host. Returns undefined when
 * there is none — the caller falls back to the stub and MUST label it as such. */
export function resolveToolAuthoring(
	env: Record<string, string | undefined> = process.env,
	providers = detectProviders,
): ResolvedAuthoring | undefined {
	if (env.TOOL_AUTHOR_MODEL === "0") return undefined;
	if (env.TOOL_AUTHOR_MODEL === "1") return { via: "env_override" };
	const detected = providers();
	const anthropic = detected.find((p) => p.id === "anthropic" && p.available);
	if (anthropic?.via === "api_key") {
		// getModel can miss ids on registry drift; fall through rather than fail.
		const model = getModel("anthropic", ANTHROPIC_API_MODEL);
		if (model) return { via: "api_key", model };
	}
	if (anthropic?.via === "subscription_cli") {
		return {
			via: "claude_cli",
			model: claudeCliModel(),
			complete: claudeCliComplete as ToolAuthorOptions["complete"],
		};
	}
	if (detected.some((p) => p.id === "ollama" && p.available)) {
		return { via: "ollama", model: ollamaModel() };
	}
	return undefined;
}


/**
 * Resolve the engine for RUNTIME `nex.ai.*` capabilities (summarize/score/write
 * executed inside an authored tool), reusing the SAME resolver that authored the
 * tool. Without this, a subscription-login operator (`claude` CLI on PATH, no env
 * key) authors a real tool that then emits the "nothing actually ran" stub at
 * execution, because the old runtime gate (TOOL_RUNTIME_MODEL=1 -> Ollama only)
 * never sees the subscription path. Returns {} when no engine is available, so
 * nex.ai.* stays honestly simulated rather than throwing.
 */
export function runtimeAICapabilityConfig(
	env: Record<string, string | undefined> = process.env,
	providers = detectProviders,
): { aiModel?: Model<string>; complete?: ToolAuthorOptions["complete"] } {
	const resolved = resolveToolAuthoring(env, providers);
	if (!resolved) return {};
	// env_override (TOOL_AUTHOR_MODEL=1) carries no model; resolve the core
	// Ollama/HARNESS_PROVIDER model the same way authorToolWithModel would.
	const aiModel = resolved.model ?? (resolved.via === "env_override" ? resolveModel() : undefined);
	if (!aiModel) return {};
	return { aiModel, complete: resolved.complete };
}
