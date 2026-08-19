import type {
  ConfigSnapshot,
  LLMRuntimeKind,
  LocalProviderStatus,
} from "../api/client";
import type { PrereqResult } from "../components/onboarding/runtimes";

export interface RuntimeProviderOption {
  id: LLMRuntimeKind;
  label: string;
  desc: string;
  kind: "cli" | "local";
  binary?: string;
}

export const RUNTIME_PROVIDER_OPTIONS: readonly RuntimeProviderOption[] = [
  {
    id: "claude-code",
    label: "Claude Code",
    desc: "Anthropic Claude via Claude Code CLI",
    kind: "cli",
    binary: "claude",
  },
  {
    id: "codex",
    label: "Codex",
    desc: "OpenAI Codex CLI agent",
    kind: "cli",
    binary: "codex",
  },
  {
    id: "opencode",
    label: "Opencode",
    desc: "Opencode CLI with your configured model backend",
    kind: "cli",
    binary: "opencode",
  },
  {
    id: "atlascloud",
    label: "Atlas Cloud",
    desc: "Hosted OpenAI-compatible model routing",
    kind: "local",
  },
  {
    id: "mlx-lm",
    label: "MLX-LM",
    desc: "Apple Silicon local OpenAI-compatible runtime",
    kind: "local",
  },
  {
    id: "ollama",
    label: "Ollama",
    desc: "Local model runner via OpenAI-compatible API",
    kind: "local",
  },
  {
    id: "exo",
    label: "Exo",
    desc: "Distributed local inference pool",
    kind: "local",
  },
] as const;

const OPTION_BY_ID = new Map(RUNTIME_PROVIDER_OPTIONS.map((p) => [p.id, p]));

export function runtimeProviderLabel(id: string): string {
  return OPTION_BY_ID.get(id as LLMRuntimeKind)?.label ?? id;
}

export function normalizeProviderList(
  values: readonly string[] | undefined,
): LLMRuntimeKind[] {
  const seen = new Set<string>();
  const out: LLMRuntimeKind[] = [];
  for (const raw of values ?? []) {
    const id = raw.trim().toLowerCase() as LLMRuntimeKind;
    if (!OPTION_BY_ID.has(id) || seen.has(id)) continue;
    seen.add(id);
    out.push(id);
  }
  return out;
}

export function configuredRuntimeProviders(
  cfg: ConfigSnapshot,
): LLMRuntimeKind[] {
  const priority = normalizeProviderList(cfg.llm_provider_priority);
  if (priority.length > 0) return priority;
  return cfg.llm_provider ? normalizeProviderList([cfg.llm_provider]) : [];
}

export function statusByLocalProvider(
  statuses: readonly LocalProviderStatus[] | undefined,
): Map<string, LocalProviderStatus> {
  const byKind = new Map<string, LocalProviderStatus>();
  for (const s of statuses ?? []) byKind.set(s.kind, s);
  return byKind;
}

export function prereqByBinary(
  prereqs: readonly PrereqResult[] | undefined,
): Map<string, PrereqResult> {
  const byBinary = new Map<string, PrereqResult>();
  for (const p of prereqs ?? []) byBinary.set(p.name, p);
  return byBinary;
}

export function runtimeProviderIsConnected(
  option: RuntimeProviderOption,
  deps: {
    prereqs?: Map<string, PrereqResult>;
    localStatuses?: Map<string, LocalProviderStatus>;
  },
): boolean {
  if (option.kind === "cli") {
    if (!option.binary) return false;
    const prereq = deps.prereqs?.get(option.binary);
    if (!prereq?.found) return false;
    if (prereq.session_probed === true) return prereq.signed_in === true;
    return true;
  }
  const status = deps.localStatuses?.get(option.id);
  return Boolean(
    status?.platform_supported && status.binary_installed && status.reachable,
  );
}

export function connectedRuntimeProviders(deps: {
  prereqs?: readonly PrereqResult[];
  localStatuses?: readonly LocalProviderStatus[];
}): RuntimeProviderOption[] {
  const prereqs = prereqByBinary(deps.prereqs);
  const localStatuses = statusByLocalProvider(deps.localStatuses);
  return RUNTIME_PROVIDER_OPTIONS.filter((option) =>
    runtimeProviderIsConnected(option, { prereqs, localStatuses }),
  );
}

export function configuredConnectedRuntimeProviders(
  cfg: ConfigSnapshot,
  deps: {
    prereqs?: readonly PrereqResult[];
    localStatuses?: readonly LocalProviderStatus[];
  },
): RuntimeProviderOption[] {
  const configured = configuredRuntimeProviders(cfg);
  if (configured.length === 0) return [];
  const connectedById = new Set(
    connectedRuntimeProviders(deps).map((option) => option.id),
  );
  return configured
    .map((id) => OPTION_BY_ID.get(id))
    .filter(
      (option): option is RuntimeProviderOption =>
        option !== undefined && connectedById.has(option.id),
    );
}
