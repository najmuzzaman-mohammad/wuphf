import type {
  LLMRuntimeKind,
  OfficeMember,
  ProviderBinding,
} from "../api/client";

// Shared provider-binding helpers for surfaces that read or display an
// agent's runtime selection (the new-task composer, agent pickers, …).
//
// AgentProfilePanel and AgentWizard predate this module and keep their own
// local copies of bindingFromMember / PROVIDER_LABELS; this is the canonical
// home for new code, and those two are a follow-up consolidation.

// Human-readable labels for the directly-dispatchable runtimes.
export const PROVIDER_LABELS: Record<LLMRuntimeKind, string> = {
  "claude-code": "Claude Code",
  atlascloud: "Atlas Cloud",
  codex: "Codex",
  opencode: "Opencode",
  "mlx-lm": "MLX-LM",
  ollama: "Ollama",
  exo: "Exo",
};

// Fallback runtime-kind list when /config has not reported llm_provider_kinds.
export const DEFAULT_LLM_KINDS: LLMRuntimeKind[] = [
  "claude-code",
  "atlascloud",
  "codex",
  "opencode",
  "mlx-lm",
  "ollama",
  "exo",
];

// bindingFromMember normalises the wire's `provider?: ProviderBinding | string`
// (string is a legacy kind-only shape) into a ProviderBinding object.
export function bindingFromMember(
  provider: OfficeMember["provider"],
): ProviderBinding {
  if (!provider) return {};
  if (typeof provider === "string") {
    return { kind: provider as ProviderBinding["kind"] };
  }
  return provider;
}

// runtimeKindFromMember resolves a member's binding kind to a known
// LLMRuntimeKind, or "" when the binding is empty, gateway-bound, or an
// unrecognised value (callers fall back to the install default).
export function runtimeKindFromMember(
  provider: OfficeMember["provider"],
  knownKinds: LLMRuntimeKind[] = DEFAULT_LLM_KINDS,
): LLMRuntimeKind | "" {
  const kind = bindingFromMember(provider).kind ?? "";
  return knownKinds.includes(kind as LLMRuntimeKind)
    ? (kind as LLMRuntimeKind)
    : "";
}
