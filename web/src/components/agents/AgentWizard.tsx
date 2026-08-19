// biome-ignore-all lint/a11y/noStaticElementInteractions: Modal backdrop uses pointer hit-testing while dialog controls retain keyboard handling.
// biome-ignore-all lint/a11y/useKeyWithClickEvents: Backdrop pointer dismissal is paired with a window Escape listener while the modal is open.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import {
  generateAgent,
  getConfig,
  getLocalProvidersStatus,
  type LLMRuntimeKind,
  type LocalProviderStatus,
  post,
} from "../../api/client";
import { useWindowEscape } from "../../hooks/useWindowEscape";
import { track } from "../../lib/analytics";
import {
  CUSTOM_MODEL_VALUE,
  INHERIT_MODEL_VALUE,
  isCatalogModel,
  modelOptionsForKind,
} from "../../lib/modelCatalog";
import { DEFAULT_LLM_KINDS } from "../../lib/providerBinding";

// "inherit" is the wizard-only sentinel that maps to an absent ProviderBinding
// in the POST body (the broker then falls back to the install-wide default at
// dispatch time). Gateway kinds (openclaw / hermes-agent) are deliberately
// absent — agents bound to a gateway are imported through the Integrations
// app, not created in this wizard.
type ProviderChoice = "inherit" | LLMRuntimeKind;
type WizardMode = "describe" | "manual";

interface AgentFormData {
  name: string;
  slug: string;
  role: string;
  emoji: string;
  provider: ProviderChoice;
  model: string;
  expertise: string;
  soul: string;
}

const INITIAL_FORM: AgentFormData = {
  name: "",
  slug: "",
  role: "",
  emoji: "",
  provider: "inherit",
  model: "",
  expertise: "",
  soul: "",
};

// Human-readable labels for the runtime picker. Kinds the broker hasn't
// registered are skipped at render time, so missing labels here aren't fatal —
// they just fall back to the raw kind string.
const PROVIDER_LABELS: Record<LLMRuntimeKind, string> = {
  "claude-code": "Claude Code",
  atlascloud: "Atlas Cloud",
  codex: "Codex",
  opencode: "Opencode",
  "mlx-lm": "MLX-LM",
  ollama: "Ollama",
  exo: "Exo",
};

function slugify(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

interface AgentWizardProps {
  open: boolean;
  onClose: () => void;
  onCreated?: () => void;
}

const AGENT_WIZARD_DIALOG_PROPS = {
  role: "dialog",
  "aria-modal": true,
  "aria-labelledby": "agent-wizard-title",
} as const;

// WizardModelPicker mirrors AgentProfilePanel's ModelPicker — curated catalog
// dropdown plus a "Custom…" escape hatch for power users. The wizard's
// "Inherit default" provider state disables the picker entirely; the field is
// re-enabled when the user picks a specific runtime.
function WizardModelPicker({
  providerKind,
  value,
  disabled,
  onChange,
  localStatuses,
}: {
  providerKind: LLMRuntimeKind | "";
  value: string;
  disabled: boolean;
  onChange: (next: string) => void;
  localStatuses: LocalProviderStatus[];
}) {
  const options = modelOptionsForKind(providerKind, localStatuses);
  const valueIsCatalog = isCatalogModel(providerKind, value, localStatuses);
  const [customMode, setCustomMode] = useState(!valueIsCatalog && value !== "");
  // Re-sync custom mode when the runtime kind switches under us.
  // Without this, picking a different runtime after entering a custom
  // model leaves the dropdown stuck in custom mode against the new
  // catalog (or vice versa).
  useEffect(() => {
    const shouldBeCustom =
      !isCatalogModel(providerKind, value, localStatuses) && value !== "";
    setCustomMode(shouldBeCustom);
  }, [providerKind, value, localStatuses]);
  // useRef-driven focus into the custom input. Matches the
  // AgentProfilePanel ModelPicker pattern: biome forbids the JSX
  // autoFocus attribute but the imperative form is sanctioned and the
  // immediate-focus UX is load-bearing after picking Custom….
  const customInputRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (customMode) customInputRef.current?.focus();
  }, [customMode]);
  const selectValue =
    customMode || !valueIsCatalog
      ? CUSTOM_MODEL_VALUE
      : value || INHERIT_MODEL_VALUE;
  return (
    <div style={{ display: "flex", gap: 6, width: "100%" }}>
      <select
        id="agent-model"
        value={selectValue}
        disabled={disabled}
        onChange={(e) => {
          const next = e.target.value;
          if (next === CUSTOM_MODEL_VALUE) {
            setCustomMode(true);
            return;
          }
          setCustomMode(false);
          onChange(next);
        }}
        style={{ flex: customMode ? "0 0 160px" : 1 }}
      >
        {options.map((o) => (
          <option key={o.value || "default"} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      {customMode && (
        <input
          ref={customInputRef}
          className="input"
          type="text"
          placeholder="e.g. claude-opus-4-7"
          value={value}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
          style={{
            fontFamily: "var(--font-mono)",
            fontSize: 12,
            flex: 1,
          }}
          aria-label="Custom model id"
        />
      )}
    </div>
  );
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: Existing cognitive complexity is baselined for a focused follow-up refactor.
export function AgentWizard({ open, onClose, onCreated }: AgentWizardProps) {
  const [mode, setMode] = useState<WizardMode>("describe");
  const [prompt, setPrompt] = useState("");
  const [generating, setGenerating] = useState(false);
  const [form, setForm] = useState<AgentFormData>(INITIAL_FORM);
  const [slugEdited, setSlugEdited] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const queryClient = useQueryClient();

  // Pull the registered runtime list off the wire so the picker stays in sync
  // with whatever providers the Go layer has registered. Falls back to a
  // hardcoded core set on first paint / fetch failure so the wizard is
  // usable even when /config is briefly unavailable.
  const configQuery = useQuery({
    queryKey: ["config"],
    queryFn: getConfig,
    enabled: open,
    staleTime: 30_000,
  });
  const localStatusQuery = useQuery({
    queryKey: ["local-providers-status"],
    queryFn: getLocalProvidersStatus,
    enabled: open,
    staleTime: 30_000,
  });
  const localStatuses: LocalProviderStatus[] = localStatusQuery.data ?? [];
  const llmKinds: LLMRuntimeKind[] = (configQuery.data?.llm_provider_kinds ??
    DEFAULT_LLM_KINDS) as LLMRuntimeKind[];

  async function handleGenerate() {
    const trimmed = prompt.trim();
    if (!trimmed) {
      setError("Describe the agent you want first.");
      return;
    }
    setGenerating(true);
    setError(null);
    try {
      const tmpl = await generateAgent(trimmed);
      const generatedSlug = tmpl.slug || "";
      // The CEO-side generator may suggest a runtime/model pair. Only
      // honor it when the suggested provider is one we'd surface in the
      // picker (i.e. a non-gateway registered kind) — gateway kinds must
      // come in through the Integrations app, never through the wizard.
      const suggestedProvider = tmpl.provider as LLMRuntimeKind | undefined;
      const providerInList =
        suggestedProvider && llmKinds.includes(suggestedProvider);
      setForm({
        name: tmpl.name || "",
        slug: generatedSlug,
        role: tmpl.role || "",
        emoji: tmpl.emoji || "",
        provider: providerInList ? suggestedProvider : "inherit",
        model: tmpl.model || "",
        expertise: (tmpl.expertise || []).join(", "),
        soul: tmpl.personality || "",
      });
      setSlugEdited(generatedSlug.length > 0);
      setMode("manual");
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : "Generation failed";
      setError(message);
    } finally {
      setGenerating(false);
    }
  }

  const updateField = useCallback(
    <K extends keyof AgentFormData>(field: K, value: AgentFormData[K]) => {
      setForm((prev) => {
        const next = { ...prev, [field]: value };
        if (field === "name" && !slugEdited) {
          next.slug = slugify(value as string);
        }
        return next;
      });
      setError(null);
    },
    [slugEdited],
  );

  const expertiseTags = useMemo(() => {
    return form.expertise
      .split(",")
      .map((t) => t.trim())
      .filter(Boolean);
  }, [form.expertise]);

  const canSubmit = form.name.trim().length > 0 && form.slug.trim().length > 0;

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();

    if (!canSubmit) return;
    setSubmitting(true);
    setError(null);

    try {
      // Encode provider as an explicit binding only when the user picked a
      // specific runtime. "inherit" sends an absent provider field so the
      // broker leaves Provider zero-valued; the dispatch resolver then
      // falls back to the install-wide default at turn time.
      const trimmedModel = form.model.trim();
      const providerBody =
        form.provider === "inherit"
          ? undefined
          : trimmedModel
            ? { kind: form.provider, model: trimmedModel }
            : { kind: form.provider };
      const body = {
        action: "create",
        slug: form.slug,
        name: form.name,
        role: form.role || undefined,
        emoji: form.emoji || undefined,
        provider: providerBody,
        expertise: expertiseTags.length > 0 ? expertiseTags : undefined,
        // The soul/personality seeds the agent's SOUL.md (its persona, voice,
        // and boundaries) — the file the broker loads into the system prompt.
        personality: form.soul.trim() || undefined,
      };

      await post("/office-members", body);
      track("agent_created", { source: "wizard", from_blueprint: false });
      await queryClient.invalidateQueries({ queryKey: ["office-members"] });

      setForm(INITIAL_FORM);
      setSlugEdited(false);
      onCreated?.();
      onClose();
    } catch (err: unknown) {
      const message =
        err instanceof Error ? err.message : "Failed to create agent";
      setError(message);
    } finally {
      setSubmitting(false);
    }
  }

  const handleCancel = useCallback(() => {
    if (generating || submitting) return;
    setForm(INITIAL_FORM);
    setSlugEdited(false);
    setError(null);
    setMode("describe");
    setPrompt("");
    onClose();
  }, [generating, onClose, submitting]);

  function handleOverlayClick(e: React.MouseEvent) {
    if (e.target === e.currentTarget) {
      handleCancel();
    }
  }

  useWindowEscape(open, handleCancel);

  if (!open) return null;

  return (
    <div
      className="agent-wizard-overlay"
      role="presentation"
      onClick={handleOverlayClick}
    >
      <div className="agent-wizard-modal card" {...AGENT_WIZARD_DIALOG_PROPS}>
        <div className="agent-wizard-title" id="agent-wizard-title">
          Create agent
        </div>

        {/* Mode toggle */}
        <div className="channel-wizard-tabs" style={{ marginBottom: 16 }}>
          <button
            type="button"
            className={`channel-wizard-tab${mode === "describe" ? " active" : ""}`}
            onClick={() => {
              setMode("describe");
              setError(null);
            }}
          >
            Describe
          </button>
          <button
            type="button"
            className={`channel-wizard-tab${mode === "manual" ? " active" : ""}`}
            onClick={() => {
              setMode("manual");
              setError(null);
            }}
          >
            Manual
          </button>
        </div>

        {mode === "describe" ? (
          <div className="agent-wizard-form">
            <div className="agent-wizard-field">
              <label className="label" htmlFor="agent-prompt">
                Describe the agent you want
              </label>
              <textarea
                id="agent-prompt"
                className="input"
                placeholder='e.g. "A DevOps engineer who manages CI/CD and infrastructure"'
                value={prompt}
                onChange={(e) => {
                  setPrompt(e.target.value);
                  setError(null);
                }}
                rows={3}
                style={{
                  minHeight: 80,
                  resize: "vertical",
                  padding: "10px 12px",
                  lineHeight: 1.5,
                }}
              />
              <span
                style={{
                  fontSize: 11,
                  color: "var(--text-tertiary)",
                  marginTop: 6,
                  display: "block",
                }}
              >
                AI will draft a slug, name, role, expertise, and personality.
                You can edit before creating.
              </span>
            </div>

            {error ? <div className="agent-wizard-error">{error}</div> : null}

            <div className="agent-wizard-footer">
              <button
                type="button"
                className="btn btn-ghost btn-sm"
                onClick={handleCancel}
                disabled={generating}
              >
                Cancel
              </button>
              <button
                type="button"
                className="btn btn-primary btn-sm"
                onClick={handleGenerate}
                disabled={generating || !prompt.trim()}
              >
                {generating ? "Generating..." : "Generate"}
              </button>
            </div>
          </div>
        ) : (
          <form className="agent-wizard-form" onSubmit={handleSubmit}>
            {/* Name */}
            <div className="agent-wizard-field">
              <label className="label" htmlFor="agent-name">
                Name
              </label>
              <input
                id="agent-name"
                className="input"
                type="text"
                placeholder="e.g. Sales Rep"
                value={form.name}
                onChange={(e) => updateField("name", e.target.value)}
              />
            </div>

            {/* Slug */}
            <div className="agent-wizard-field">
              <label className="label" htmlFor="agent-slug">
                Slug
              </label>
              <input
                id="agent-slug"
                className="input"
                type="text"
                placeholder="auto-generated-from-name"
                value={form.slug}
                onChange={(e) => {
                  setSlugEdited(true);
                  updateField("slug", e.target.value);
                }}
              />
            </div>

            {/* Role */}
            <div className="agent-wizard-field">
              <label className="label" htmlFor="agent-role">
                Role
              </label>
              <input
                id="agent-role"
                className="input"
                type="text"
                placeholder="e.g. SDR, Engineer, Support"
                value={form.role}
                onChange={(e) => updateField("role", e.target.value)}
              />
            </div>

            {/* Soul / personality — seeds SOUL.md */}
            <div className="agent-wizard-field">
              <label className="label" htmlFor="agent-soul">
                Soul{" "}
                <span
                  style={{ fontWeight: 400, color: "var(--text-tertiary)" }}
                >
                  (personality, voice, boundaries — optional)
                </span>
              </label>
              <textarea
                id="agent-soul"
                className="input"
                placeholder="e.g. Relentless about pipeline, allergic to vanity metrics. Direct, never fluffy."
                value={form.soul}
                onChange={(e) => updateField("soul", e.target.value)}
                rows={3}
                style={{
                  minHeight: 72,
                  resize: "vertical",
                  padding: "10px 12px",
                  lineHeight: 1.5,
                }}
              />
              <span className="op-hint">
                Seeds this agent's SOUL.md — the persona loaded into its system
                prompt. You can refine it (and the other instruction files)
                anytime from the agent's profile.
              </span>
            </div>

            {/* Emoji */}
            <div className="agent-wizard-field">
              <label className="label" htmlFor="agent-emoji">
                Emoji
              </label>
              <input
                id="agent-emoji"
                className="input"
                type="text"
                placeholder="e.g. robot face"
                value={form.emoji}
                onChange={(e) => updateField("emoji", e.target.value)}
                maxLength={4}
                style={{ width: 80 }}
              />
            </div>

            {/* Provider + Model */}
            <div className="agent-wizard-field">
              <label className="label" htmlFor="agent-provider">
                Runtime
              </label>
              <select
                id="agent-provider"
                value={form.provider}
                onChange={(e) =>
                  updateField("provider", e.target.value as ProviderChoice)
                }
              >
                <option value="inherit">
                  Inherit default (
                  {configQuery.data?.llm_provider ?? "claude-code"})
                </option>
                {llmKinds.map((kind) => (
                  <option key={kind} value={kind}>
                    {PROVIDER_LABELS[kind] ?? kind}
                  </option>
                ))}
              </select>
              {form.provider === "inherit" ? (
                <span className="op-hint">
                  Inherits the install default. Pick a specific runtime to pin
                  this agent — you can also change it later from the agent's
                  profile.
                </span>
              ) : (
                <span className="op-hint">
                  This agent will run on{" "}
                  {PROVIDER_LABELS[form.provider] ?? form.provider} on every
                  turn. Change anytime from the agent's profile.
                </span>
              )}
            </div>
            <div className="agent-wizard-field">
              <label className="label" htmlFor="agent-model">
                Model{" "}
                <span
                  style={{ fontWeight: 400, color: "var(--text-tertiary)" }}
                >
                  (optional)
                </span>
              </label>
              <WizardModelPicker
                providerKind={form.provider === "inherit" ? "" : form.provider}
                value={form.model}
                disabled={form.provider === "inherit"}
                onChange={(next) => updateField("model", next)}
                localStatuses={localStatuses}
              />
              <span className="op-hint">
                Pick from common models for the chosen runtime, or use "Custom…"
                to type any model id. Leave on "Use runtime default" to let the
                runtime decide.
              </span>
            </div>

            {/* Expertise */}
            <div className="agent-wizard-field">
              <label className="label" htmlFor="agent-expertise">
                Expertise{" "}
                <span
                  style={{ fontWeight: 400, color: "var(--text-tertiary)" }}
                >
                  (comma-separated)
                </span>
              </label>
              <input
                id="agent-expertise"
                className="input"
                type="text"
                placeholder="e.g. outreach, cold email, pipeline"
                value={form.expertise}
                onChange={(e) => updateField("expertise", e.target.value)}
              />
              {expertiseTags.length > 0 && (
                <div className="agent-panel-tags" style={{ marginTop: 6 }}>
                  {expertiseTags.map((tag) => (
                    <span key={tag} className="agent-panel-tag">
                      {tag}
                    </span>
                  ))}
                </div>
              )}
            </div>

            {error ? <div className="agent-wizard-error">{error}</div> : null}

            {/* Footer */}
            <div className="agent-wizard-footer">
              <button
                type="button"
                className="btn btn-ghost btn-sm"
                onClick={handleCancel}
                disabled={submitting}
              >
                Cancel
              </button>
              <button
                type="submit"
                className="btn btn-primary btn-sm"
                disabled={!canSubmit || submitting}
              >
                {submitting ? "Creating..." : "Create"}
              </button>
            </div>
          </form>
        )}
      </div>
    </div>
  );
}

/**
 * Hook to manage wizard open/close state from any component.
 * Usage:
 *   const { open, show, hide } = useAgentWizard()
 *   <button onClick={show}>New Agent</button>
 *   <AgentWizard open={open} onClose={hide} />
 */
export function useAgentWizard() {
  const [open, setOpen] = useState(false);
  const show = useCallback(() => setOpen(true), []);
  const hide = useCallback(() => setOpen(false), []);
  return { open, show, hide };
}
