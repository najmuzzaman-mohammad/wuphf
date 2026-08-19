import type { LLMRuntimeKind, LocalProviderStatus } from "../api/client";

// MODEL_CATALOG is a curated suggestion list per LLM runtime. We let users
// pick from these in the AgentProfilePanel + AgentWizard dropdowns instead
// of free-typing a model identifier — typos in model ids are a real failure
// mode (codex/claude refuse the request, the agent appears silent).
//
// Cloud runtimes ship a fixed list of well-known ids. They drift as new
// model families launch, but the "Custom…" escape hatch in the picker
// keeps power users unblocked while we update the catalog.
//
// Local runtimes return an empty list here — their model dropdown is
// populated dynamically from the /status/local-providers probe (see
// modelOptionsForKind below).
//
// Sourcing (verified 2026-05-29):
//   - claude-code → Anthropic model overview
//     https://platform.claude.com/docs/en/docs/about-claude/models
//   - codex → OpenAI models index (developers.openai.com)
//     https://developers.openai.com/api/docs/models/all
//   - opencode mixes Anthropic + OpenAI via Models.dev, so the catalog
//     surfaces the most common headline ids from each.
//
// Each list is ordered current-first so the dropdown's first non-default
// option is the recommended pick. Aliases (claude-opus-4-7, claude-sonnet-4-6,
// claude-haiku-4-5) are preferred over dated snapshots because they reduce
// surprise migrations when Anthropic pins a new dated ID under the same
// alias.
const CLOUD_MODELS: Record<
  Exclude<LLMRuntimeKind, "mlx-lm" | "ollama" | "exo">,
  string[]
> = {
  atlascloud: [
    "qwen/qwen3.5-flash",
    "deepseek-ai/deepseek-v4-pro",
  ],
  "claude-code": [
    // Current / recommended
    "claude-opus-4-8",
    "claude-sonnet-4-6",
    "claude-haiku-4-5",
    // Legacy but still available
    "claude-opus-4-7",
    "claude-opus-4-6",
    "claude-sonnet-4-5",
    "claude-opus-4-5",
    "claude-opus-4-1",
  ],
  codex: [
    // Current / recommended
    "gpt-5.5",
    "gpt-5.5-pro",
    "gpt-5.4",
    "gpt-5.4-pro",
    "gpt-5.4-mini",
    "gpt-5.4-nano",
    // Codex-specialised agentic coding model
    "gpt-5.3-codex",
    // Legacy but still available via API
    "gpt-5.2",
    "gpt-5",
    "gpt-5-mini",
    "gpt-5-nano",
    "o3",
    "o3-pro",
  ],
  opencode: [
    // Anthropic top picks
    "claude-opus-4-8",
    "claude-sonnet-4-6",
    "claude-haiku-4-5",
    // OpenAI top picks
    "gpt-5.5",
    "gpt-5.4",
    "gpt-5.3-codex",
    // Common older fallbacks
    "claude-opus-4-7",
    "gpt-5",
  ],
};

// Empty model entry maps to "use the runtime's default" (broker leaves
// ProviderBinding.Model unset and each runner picks its own default).
export const INHERIT_MODEL_VALUE = "";

// Sentinel used by the picker to fall back to a text input when none of
// the curated suggestions fit. Component code reads selectedValue ===
// CUSTOM_MODEL_VALUE and renders the free-text input.
export const CUSTOM_MODEL_VALUE = "__custom__";

export interface ModelOption {
  value: string;
  label: string;
}

// modelOptionsForKind returns the dropdown options for a given runtime
// kind. The list always begins with the "Use runtime default" entry; the
// "Custom…" entry is appended at the end so power users can pick a model
// the catalog doesn't know about yet.
//
// `localStatuses` lets the picker show real installed models for local
// runtimes (mlx-lm / ollama / exo). When undefined, local runtimes fall
// back to just {default, custom}.
export function modelOptionsForKind(
  kind: LLMRuntimeKind | "",
  localStatuses?: LocalProviderStatus[],
): ModelOption[] {
  const options: ModelOption[] = [
    { value: INHERIT_MODEL_VALUE, label: "Use runtime default" },
  ];
  if (kind === "" ) {
    return options;
  }
  if (kind === "mlx-lm" || kind === "ollama" || kind === "exo") {
    const status = localStatuses?.find((s) => s.kind === kind);
    const loaded = status?.loaded_model?.trim();
    const configured = status?.model?.trim();
    const seen = new Set<string>();
    for (const candidate of [loaded, configured]) {
      if (candidate && !seen.has(candidate)) {
        options.push({ value: candidate, label: candidate });
        seen.add(candidate);
      }
    }
  } else if (kind in CLOUD_MODELS) {
    for (const model of CLOUD_MODELS[
      kind as keyof typeof CLOUD_MODELS
    ]) {
      options.push({ value: model, label: model });
    }
  }
  options.push({ value: CUSTOM_MODEL_VALUE, label: "Custom…" });
  return options;
}

// isCatalogModel reports whether modelValue is one of the curated entries
// for the given runtime kind. Used to decide whether the picker should
// open in "select" mode (catalog match) or "custom text" mode (the saved
// value isn't in the catalog, so the user typed it).
export function isCatalogModel(
  kind: LLMRuntimeKind | "",
  modelValue: string,
  localStatuses?: LocalProviderStatus[],
): boolean {
  if (modelValue === "") return true;
  const options = modelOptionsForKind(kind, localStatuses);
  return options.some(
    (o) =>
      o.value !== CUSTOM_MODEL_VALUE && o.value === modelValue,
  );
}
