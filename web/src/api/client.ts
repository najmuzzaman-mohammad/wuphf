/**
 * Typed WuphfAPI client.
 * Mirrors every method from the legacy IIFE in index.legacy.html.
 */

import type { InjectedAnalyticsConfig } from "../lib/analytics";
import { trackOn } from "../lib/analytics";

const apiBase = "/api";
let brokerDirect = "http://localhost:7890";
let useProxy = true;
let token: string | null = null;
const brokerHandshakeTimeoutMs = 8000;

// The analytics config the broker injects via /api-token. Captured at boot so
// RootRoute can configure PostHog without a second round trip. Null until
// initApi runs (or when the broker omits the block, e.g. older servers).
let injectedAnalytics: InjectedAnalyticsConfig | null = null;

/** The PostHog runtime config the broker injected at boot, if any. */
export function getInjectedAnalyticsConfig(): InjectedAnalyticsConfig | null {
  return injectedAnalytics;
}

/** Coarse length bucket for a message body — never the content itself. */
function lengthBucket(text: string): "empty" | "short" | "medium" | "long" {
  const n = text.trim().length;
  if (n === 0) return "empty";
  if (n < 80) return "short";
  if (n < 400) return "medium";
  return "long";
}

// ── Init ──

export async function initApi(): Promise<void> {
  try {
    const r = await fetch("/api-token");
    const data = await r.json();
    const { token: nextToken, broker_url: brokerUrl } = data;
    token = nextToken;
    if (brokerUrl) {
      brokerDirect = String(brokerUrl).replace(/\/+$/, "");
    }
    if (data && typeof data.analytics === "object" && data.analytics !== null) {
      injectedAnalytics = data.analytics as InjectedAnalyticsConfig;
    }
    useProxy = true;
  } catch {
    useProxy = false;
    try {
      const r = await fetch(`${brokerDirect}/web-token`);
      const data = await r.json();
      const { token: nextToken } = data;
      token = nextToken;
    } catch {
      // broker unreachable — will fail on first request
    }
  }
}

export async function connectBroker(
  brokerUrl: string,
  brokerToken?: string,
): Promise<void> {
  const nextBroker = brokerUrl.trim().replace(/\/+$/, "");
  if (!nextBroker) {
    throw new Error("Broker URL is required");
  }
  try {
    const parsed = new URL(nextBroker);
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
      throw new Error("unsupported protocol");
    }
  } catch {
    throw new Error("Broker URL must be a valid http:// or https:// URL");
  }
  let nextToken = brokerToken?.trim() || null;
  const r = await fetchWithTimeout(
    `${nextBroker}/health`,
    brokerHandshakeTimeoutMs,
  );
  if (!r.ok) {
    const text = (await r.text().catch(() => "")).trim();
    throw new Error(text || `${r.status} ${r.statusText}`);
  }
  if (!nextToken) {
    const tokenResp = await fetchWithTimeout(
      `${nextBroker}/web-token`,
      brokerHandshakeTimeoutMs,
    );
    if (!tokenResp.ok) {
      const text = (await tokenResp.text().catch(() => "")).trim();
      throw new Error(text || `${tokenResp.status} ${tokenResp.statusText}`);
    }
    const data = await tokenResp.json();
    const candidate = typeof data?.token === "string" ? data.token.trim() : "";
    if (!candidate) {
      throw new Error("Broker /web-token response did not include a token");
    }
    nextToken = candidate;
  }
  brokerDirect = nextBroker;
  token = nextToken;
  useProxy = false;
}

async function fetchWithTimeout(
  url: string,
  timeoutMs: number,
): Promise<Response> {
  const controller = new AbortController();
  const timeout = globalThis.setTimeout(() => controller.abort(), timeoutMs);
  try {
    return await fetch(url, { signal: controller.signal });
  } catch (err) {
    if (err instanceof Error && err.name === "AbortError") {
      throw new Error("Timed out connecting to your workspace. Give it a moment and try again.");
    }
    throw err;
  } finally {
    globalThis.clearTimeout(timeout);
  }
}

// ── Internal helpers ──

function baseURL(): string {
  return useProxy ? apiBase : brokerDirect;
}

function authHeaders(): Record<string, string> {
  const h: Record<string, string> = { "Content-Type": "application/json" };
  if (token) h.Authorization = `Bearer ${token}`;
  return h;
}

interface RequestOptions {
  signal?: AbortSignal;
}

/**
 * Default timeout for read (GET) requests. Without it, a wedged broker
 * left every list surface on an eternal spinner (the Notebooks tab sat
 * on "Loading bookshelf…" for 60s+ in the v3 eval) because plain
 * `fetch` never gives up. 20s is generous for any healthy list
 * endpoint while still letting surfaces flip to an honest
 * "broker not responding — retry" state. Callers with a longer-running
 * read can pass their own `timeoutMs`.
 */
export const GET_TIMEOUT_MS = 20_000;

interface GetOptions {
  signal?: AbortSignal;
  /** Override the default GET_TIMEOUT_MS for slow endpoints. */
  timeoutMs?: number;
}

/**
 * Builds the signal for a GET: the caller's signal when provided,
 * otherwise a timeout signal so no read can hang forever. Exported for
 * the regression test pinning the no-eternal-spinner contract.
 */
export function getRequestSignal(options?: GetOptions): AbortSignal {
  if (options?.signal) return options.signal;
  return AbortSignal.timeout(options?.timeoutMs ?? GET_TIMEOUT_MS);
}

function describeGetError(err: unknown): Error | null {
  if (err instanceof Error && err.name === "TimeoutError") {
    return new Error("Your workspace is not responding — the request timed out.");
  }
  return null;
}

/**
 * Turn a broker error body into a human-readable message. The broker
 * answers many failures with a JSON envelope (`{"error":"wiki backend
 * is not active"}`); rendering that envelope verbatim leaked raw JSON
 * into every surface that shows `err.message` (the wiki did exactly
 * that during a broker wedge). When the body is JSON carrying a
 * sentence-shaped `error`/`message` string, surface that string as
 * plain text — sentence-cased with a trailing period. Code-shaped
 * values (`store_busy`, no whitespace) are NOT prose; keep the raw
 * body so programmatic consumers and error reports stay precise.
 */
export function humanizeApiErrorBody(bodyText: string): string | null {
  if (!bodyText) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(bodyText) as unknown;
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return null;
  }
  const rec = parsed as Readonly<Record<string, unknown>>;
  // Envelopes that carry payload beyond the message (e.g. the wiki 409
  // conflict shape with current_sha/current_content) are data, not prose —
  // callers parse them out of the message text, so leave those intact.
  if (Object.keys(rec).some((k) => k !== "error" && k !== "message")) {
    return null;
  }
  const candidate = [rec.error, rec.message].find(
    (v): v is string => typeof v === "string" && v.trim().length > 0,
  );
  if (!candidate) return null;
  const text = candidate.trim();
  // A code-like token (no whitespace) is not a sentence — leave it alone.
  if (!/\s/.test(text)) return null;
  const sentence = text.charAt(0).toUpperCase() + text.slice(1);
  return /[.!?]$/.test(sentence) ? sentence : `${sentence}.`;
}

export class ApiError extends Error {
  readonly status: number;
  readonly statusText: string;
  readonly bodyText: string;
  readonly errorCode: string | null;
  readonly retryAfter: string | null;

  constructor(args: {
    readonly status: number;
    readonly statusText: string;
    readonly bodyText: string;
    readonly errorCode?: string | null;
    readonly retryAfter?: string | null;
  }) {
    super(
      humanizeApiErrorBody(args.bodyText) ??
        (args.bodyText || `${args.status} ${args.statusText}`),
    );
    this.name = "ApiError";
    this.status = args.status;
    this.statusText = args.statusText;
    this.bodyText = args.bodyText;
    this.errorCode = args.errorCode ?? null;
    this.retryAfter = args.retryAfter ?? null;
  }
}

export async function get<T = unknown>(
  path: string,
  params?: Record<string, string | number | boolean | null | undefined>,
  options?: GetOptions,
): Promise<T> {
  let url = baseURL() + path;
  if (params) {
    const qs = Object.entries(params)
      // Drop absent params. Both null AND undefined must be skipped — without
      // the undefined check, String(undefined) serializes as the literal
      // "undefined", which the broker then treats as a real filter value (e.g.
      // ?provider=undefined silently blanked the integrations catalog).
      .filter(([, v]) => v !== null && v !== undefined)
      .map(
        ([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`,
      )
      .join("&");
    if (qs) url += `?${qs}`;
  }
  let r: Response;
  try {
    r = await fetch(url, {
      headers: authHeaders(),
      signal: getRequestSignal(options),
    });
  } catch (err) {
    throw describeGetError(err) ?? err;
  }
  if (!r.ok) {
    throw await apiErrorFromResponse(r);
  }
  return r.json();
}

export async function getText(
  path: string,
  params?: Record<string, string | number | boolean | null | undefined>,
  options?: GetOptions,
): Promise<string> {
  let url = baseURL() + path;
  if (params) {
    const qs = Object.entries(params)
      // Drop absent params. Both null AND undefined must be skipped — without
      // the undefined check, String(undefined) serializes as the literal
      // "undefined", which the broker then treats as a real filter value (e.g.
      // ?provider=undefined silently blanked the integrations catalog).
      .filter(([, v]) => v !== null && v !== undefined)
      .map(
        ([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`,
      )
      .join("&");
    if (qs) url += `?${qs}`;
  }
  let r: Response;
  try {
    r = await fetch(url, {
      headers: authHeaders(),
      signal: getRequestSignal(options),
    });
  } catch (err) {
    throw describeGetError(err) ?? err;
  }
  if (!r.ok) {
    throw await apiErrorFromResponse(r);
  }
  return r.text();
}

/** Authenticated binary GET — for file-ish payloads (PDFs) that a plain <a>
 * cannot fetch because the auth header does not travel with link navigation. */
export async function getBlob(
  path: string,
  options?: GetOptions,
): Promise<Blob> {
  let r: Response;
  try {
    r = await fetch(baseURL() + path, {
      headers: authHeaders(),
      signal: getRequestSignal(options),
    });
  } catch (err) {
    throw describeGetError(err) ?? err;
  }
  if (!r.ok) {
    throw await apiErrorFromResponse(r);
  }
  return r.blob();
}

export async function post<T = unknown>(
  path: string,
  body?: unknown,
  options: RequestOptions = {},
): Promise<T> {
  const r = await fetch(baseURL() + path, {
    method: "POST",
    headers: authHeaders(),
    body: JSON.stringify(body),
    signal: options.signal,
  });
  if (!r.ok) {
    throw await apiErrorFromResponse(r);
  }
  return r.json();
}

/**
 * POST that returns the raw streaming Response (e.g. a text/event-stream SSE
 * body) instead of parsing JSON. The caller reads `response.body` and branches
 * on `response.status` — used by the browser-exec live run, which streams the
 * runner's events and falls back to the mock on a 503. Does NOT throw on a
 * non-2xx, so the caller can detect 503 itself.
 */
export async function postStream(
  path: string,
  body?: unknown,
  options: RequestOptions = {},
): Promise<Response> {
  return fetch(baseURL() + path, {
    method: "POST",
    headers: authHeaders(),
    body: JSON.stringify(body),
    signal: options.signal,
  });
}

export async function put<T = unknown>(
  path: string,
  body?: unknown,
): Promise<T> {
  const r = await fetch(baseURL() + path, {
    method: "PUT",
    headers: authHeaders(),
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    throw await apiErrorFromResponse(r);
  }
  return r.json();
}

export async function postWithTimeout<T = unknown>(
  path: string,
  body: unknown,
  timeoutMs: number,
): Promise<T> {
  const controller = new AbortController();
  const timeout = globalThis.setTimeout(() => controller.abort(), timeoutMs);
  try {
    const r = await fetch(baseURL() + path, {
      method: "POST",
      headers: authHeaders(),
      body: JSON.stringify(body),
      signal: controller.signal,
    });
    if (!r.ok) {
      throw await apiErrorFromResponse(r);
    }
    return r.json();
  } catch (err) {
    if (err instanceof Error && err.name === "AbortError") {
      throw new Error("Request timed out");
    }
    throw err;
  } finally {
    globalThis.clearTimeout(timeout);
  }
}

export async function patch<T = unknown>(
  path: string,
  body?: unknown,
): Promise<T> {
  const r = await fetch(baseURL() + path, {
    method: "PATCH",
    headers: authHeaders(),
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    throw await apiErrorFromResponse(r);
  }
  return r.json();
}

export async function del<T = unknown>(
  path: string,
  body?: unknown,
  extraHeaders?: Record<string, string>,
): Promise<T> {
  const r = await fetch(baseURL() + path, {
    method: "DELETE",
    headers: { ...authHeaders(), ...extraHeaders },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    throw await apiErrorFromResponse(r);
  }
  return r.json();
}

async function apiErrorFromResponse(response: Response): Promise<ApiError> {
  const bodyText = (await response.text().catch(() => "")).trim();
  return new ApiError({
    status: response.status,
    statusText: response.statusText,
    bodyText,
    errorCode: errorCodeFromBodyText(bodyText),
    retryAfter: response.headers.get("Retry-After"),
  });
}

function errorCodeFromBodyText(bodyText: string): string | null {
  if (bodyText.length === 0) return null;
  try {
    const parsed = JSON.parse(bodyText) as unknown;
    if (
      typeof parsed !== "object" ||
      parsed === null ||
      Array.isArray(parsed)
    ) {
      return null;
    }
    const { error } = parsed as Readonly<Record<string, unknown>>;
    return typeof error === "string" ? error : null;
  } catch {
    return null;
  }
}

// ── SSE ──

export function sseURL(path: string): string {
  let url = baseURL() + path;
  if (!useProxy && token) url += `?token=${encodeURIComponent(token)}`;
  return url;
}

export function websocketURL(path: string): string {
  const base =
    useProxy && brokerDirect
      ? brokerDirect
      : typeof window === "undefined"
        ? baseURL()
        : new URL(baseURL(), window.location.href)
            .toString()
            .replace(/\/$/, "");
  const url = new URL(path, base.endsWith("/") ? base : `${base}/`);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  if (token) url.searchParams.set("token", token);
  return url.toString();
}

// ── Messages ──

export interface Message {
  id: string;
  from: string;
  channel: string;
  content: string;
  /**
   * Server-assigned message kind. Empty/absent for plain chat. Known kinds:
   *  - "agent_issue"        legacy agent-authored issue banner
   *  - "system_auth_error"  system-authored provider-auth failure card (#933)
   *  - "ceo_*"              onboarding cards (form_field, chip_row, etc.)
   * The SPA's MessageBubble dispatches on this field to pick a renderer.
   */
  kind?: string;
  /**
   * Structured card payload for kinds that carry one. The broker marshals
   * this from a Go json.RawMessage so consumers receive an inline JSON
   * object (or array) — not a string. Consumers must treat every string
   * field inside as plain text (defense in depth on top of the broker-side
   * sanitizeContextValue).
   */
  payload?: unknown;
  redacted?: boolean;
  redaction_count?: number;
  redaction_reasons?: string[];
  timestamp: string;
  reply_to?: string;
  thread_id?: string;
  thread_count?: number;
  reactions?: Record<string, string[]>;
  tagged?: string[];
  usage?: TokenUsage;
}

export interface TokenUsage {
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_creation_tokens?: number;
  total_tokens?: number;
  cost_usd?: number;
}

export function getMessages(
  channel: string,
  sinceId?: string | null,
  limit = 50,
) {
  return get<{ messages: Message[] }>("/messages", {
    channel: channel || "general",
    viewer_slug: "human",
    since_id: sinceId ?? null,
    limit,
  });
}

export function postMessage(
  content: string,
  channel: string,
  replyTo?: string,
  tagged?: string[],
) {
  const body: Record<string, string | string[]> = {
    from: "you",
    channel: channel || "general",
    content,
  };
  if (replyTo) body.reply_to = replyTo;
  if (tagged && tagged.length > 0) body.tagged = tagged;
  return trackOn(post<Message>("/messages", body), "message_sent", {
    is_reply: !!replyTo,
    mention_count: tagged?.length ?? 0,
    length_bucket: lengthBucket(content),
  });
}

export function getThreadMessages(channel: string, threadId: string) {
  return get<{ messages: Message[] }>("/messages", {
    channel: channel || "general",
    thread_id: threadId,
    viewer_slug: "human",
    limit: 50,
  });
}

export function toggleReaction(msgId: string, emoji: string, channel: string) {
  return post("/messages/react", {
    message_id: msgId,
    emoji,
    channel: channel || "general",
  });
}

// ── Slash-command registry ──

/**
 * One entry from GET /commands. Mirrors the broker's `commandDescriptor`
 * shape in internal/team/broker_commands.go. Sorted alphabetically by the
 * broker — callers do not need to re-sort.
 */
export interface SlashCommandDescriptor {
  name: string;
  description: string;
  /** True when the web composer has a real handler for this command. */
  webSupported: boolean;
}

/**
 * Fetch the canonical slash-command registry from the broker. The web
 * autocomplete filters to webSupported=true; other callers may want the
 * full set for discovery.
 */
export function fetchCommands() {
  return get<SlashCommandDescriptor[]>("/commands");
}

// ── Members ──

export interface ProviderBinding {
  // kind tags the runtime or gateway for this agent. Empty string means
  // "inherit from global default". Use IsGatewayKind on a Kind to decide
  // whether to render the runtime picker (LLM kinds) or a "Managed by
  // <Gateway>" badge (gateway kinds) in the agent profile.
  kind?: LLMProvider | "";
  // model is the runtime-specific model identifier. Free-form on the wire —
  // validated by each provider implementation, not at the schema layer.
  // Common shapes: "claude-3-5-sonnet-latest", "gpt-4o", "llama3.1:8b".
  model?: string;
  // openclaw is populated only when kind === "openclaw" — it carries the
  // gateway-side session key + agent id. Set by the OpenClaw bridge bootstrap
  // path, not by the per-agent runtime picker.
  openclaw?: {
    session_key?: string;
    agent_id?: string;
  };
}

// Helper for UI code: returns true when binding.kind is a gateway-controlled
// tag. Per-agent runtime pickers and the AgentWizard should swap their UI to
// a read-only "Managed by <Gateway>" pill when this returns true.
export function isGatewayBinding(
  binding: ProviderBinding | string | undefined,
): boolean {
  if (!binding) return false;
  const kind = typeof binding === "string" ? binding : binding.kind;
  return (
    kind === "openclaw" || kind === "openclaw-http" || kind === "hermes-agent"
  );
}

export interface OfficeMember {
  slug: string;
  name: string;
  role: string;
  emoji?: string;
  status?: string;
  activity?: string;
  detail?: string;
  liveActivity?: string;
  lastTime?: string;
  task?: string;
  channel?: string;
  provider?: ProviderBinding | string;
  /** Broker-provided: serialized as `built_in`. Built-ins cannot be removed. (CEO is guarded by a separate slug check.) */
  built_in?: boolean;
  /** Per-channel disabled state when the list is sourced from `/members?channel=…`. */
  disabled?: boolean;
  /**
   * Transport-presence flag: true when an adapter session is currently live for
   * this member. Distinct from `status`/`activity` (which reflect "is the
   * agent processing right now") — `online` reflects "is the adapter
   * reachable at all". Always present (no omitempty on the Go side) so
   * "false" and "missing field" cannot be confused.
   */
  online?: boolean;
  /**
   * RFC3339 timestamp of the most recent UpsertParticipant for this slug.
   * Empty when no adapter has ever upserted (e.g. built-in members without an
   * openclaw provider) — the consumer should treat empty as "never observed"
   * and not render a "last seen" line.
   */
  last_seen_at?: string;
}

/**
 * Lane A piggybacks `humanHasPosted` onto the existing `/office-members`
 * payload (eng decision A5/P1) — additive `meta` field. When the backend
 * has not yet shipped Lane A, `meta` is absent and consumers default
 * `humanHasPosted` to `false` to avoid flashing the first-run nudge.
 */
export interface OfficeMembersMeta {
  humanHasPosted?: boolean;
}

export interface OfficeMembersResponse {
  members: OfficeMember[];
  meta?: OfficeMembersMeta;
}

export function getOfficeMembers() {
  return get<OfficeMembersResponse>("/office-members");
}

export interface GeneratedAgentTemplate {
  slug?: string;
  name?: string;
  role?: string;
  emoji?: string;
  expertise?: string[];
  personality?: string;
  provider?: string;
  model?: string;
}

export function generateAgent(prompt: string) {
  return post<GeneratedAgentTemplate>("/office-members/generate", { prompt });
}

export function getMembers(channel: string) {
  return get<{ members: OfficeMember[] }>("/members", {
    channel: channel || "general",
    viewer_slug: "human",
  });
}

// ── Channels ──

export interface Channel {
  slug: string;
  name: string;
  description?: string;
  type?: string;
  created_by?: string;
  members?: string[];
}

export interface DMChannelResponse extends Channel {
  id?: string;
  created?: boolean;
}

export function getChannels() {
  return get<{ channels: Channel[] }>("/channels");
}

export function createChannel(slug: string, name: string, description: string) {
  return trackOn(
    post("/channels", {
      action: "create",
      slug,
      name: name || slug,
      description,
      created_by: "you",
    }),
    "channel_created",
    { kind: "channel" },
  );
}

export function generateChannel(prompt: string) {
  return trackOn(
    postWithTimeout<Channel>("/channels/generate", { prompt }, 65_000),
    "channel_created",
    { kind: "generated" },
  );
}

export function createDM(agentSlug: string) {
  return trackOn(
    post<DMChannelResponse>("/channels/dm", {
      members: ["human", agentSlug],
      type: "direct",
    }),
    "channel_created",
    { kind: "dm" },
  );
}

// ── Requests ──

export interface InterviewOption {
  id: string;
  label: string;
  description?: string;
  requires_text?: boolean;
  text_hint?: string;
}

export interface SkillSimilarRef {
  slug: string;
  score: number;
  method?: string;
}

export interface InterviewMetadata {
  [key: string]: unknown;
}

export interface AgentRequest {
  id: string;
  from: string;
  question: string;
  /** Legacy field name; broker now returns `options`. Kept for compatibility. */
  choices?: InterviewOption[];
  options?: InterviewOption[];
  channel?: string;
  title?: string;
  context?: string;
  kind?: string;
  timestamp?: string;
  status?: string;
  blocking?: boolean;
  required?: boolean;
  recommended_id?: string;
  created_at?: string;
  updated_at?: string;
  /** Echoes the entity slug the request is about (e.g. a skill name). */
  reply_to?: string;
  /** Structured metadata attached by the broker (kind-specific). */
  metadata?: InterviewMetadata;
  redacted?: boolean;
  redaction_count?: number;
  redaction_reasons?: string[];
  /** Issue/task id this request belongs to, when the owner agent
   * filed the request from inside an owned Issue. The Inbox card
   * renders a breadcrumb when set so the human sees the parent
   * Issue at a glance. */
  issue_id?: string;
  /** Integration platform slug + logo for integration-scoped cards
   * (connect, fallback, and external-action approvals). Drives the
   * toolkit logo + OAuth target. Empty for non-integration requests. */
  platform?: string;
  logo_url?: string;
  /** Structured external-action payload (slice 4b): typed fields + the
   * masked raw HTTP envelope the approval card renders behind its raw
   * toggle. Absent for legacy approvals (the card falls back to the
   * parsed context string). */
  action?: ActionApprovalPayload;
  /** Set when the action gate could not reach the resolver and degraded
   * to approval-only, so the connection state is unconfirmed. The card
   * surfaces a warning (review LOW #5). */
  connection_unverified?: boolean;
}

/** The masked HTTP request an external action would send. Secrets are
 * already redacted server-side; this is display-only. */
export interface ActionEnvelope {
  method?: string;
  url?: string;
  headers?: Record<string, unknown>;
  data?: Record<string, unknown>;
}

export interface ActionApprovalPayload {
  platform?: string;
  action_id?: string;
  verb?: string;
  name?: string;
  logo_url?: string;
  account?: { name?: string; key?: string };
  raw_envelope?: ActionEnvelope;
}

export function getRequests(channel: string) {
  return get<{ requests: AgentRequest[] }>("/requests", {
    channel: channel || "general",
    viewer_slug: "human",
  });
}

// Cross-channel view. The broker's blocking check is global, so the web UI's
// global overlay + inline interview bar need every blocking request the human
// can answer, not just the ones in the current channel.
export function getAllRequests() {
  return get<{ requests: AgentRequest[] }>("/requests", {
    scope: "all",
    viewer_slug: "human",
  });
}

export function answerRequest(
  id: string,
  choiceId: string,
  customText?: string,
) {
  const body: Record<string, string> = { id, choice_id: choiceId };
  if (customText) body.custom_text = customText;
  return trackOn(post("/requests/answer", body), "interview_answered", {
    has_custom_text: !!customText,
  });
}

export function cancelRequest(id: string) {
  return post("/requests", { action: "cancel", id });
}

export interface ActionGrant {
  id: string;
  agent_slug: string;
  platform: string;
  action_scope: string;
  channel?: string;
  issue_id?: string;
  granted_by: string;
  granted_at: string;
  expires_at?: string;
  revoked_at?: string;
}

export interface CreateActionGrantInput {
  agentSlug: string;
  platform: string;
  /** A concrete action_id — never a wildcard. The broker rejects "*". */
  actionScope: string;
  channel?: string;
  issueId?: string;
}

// Mints a scoped grant so the resolver auto-approves exactly this
// (agent, platform, action_id) without re-prompting. Backs the approval
// modal's "Approve & always allow" button (deterministic-integrations slice 5b).
export function createActionGrant(input: CreateActionGrantInput) {
  return trackOn(
    post<{ grant: ActionGrant }>("/integrations/grants", {
      action: "grant",
      agent_slug: input.agentSlug,
      platform: input.platform,
      action_scope: input.actionScope,
      channel: input.channel,
      issue_id: input.issueId,
    }),
    "integration_action",
    { action: "grant", platform: input.platform },
  );
}

export function getActionGrants() {
  return get<{ grants: ActionGrant[] }>("/integrations/grants");
}

export function revokeActionGrant(id: string) {
  return trackOn(
    post<{ grant: ActionGrant }>("/integrations/grants", {
      action: "revoke",
      id,
    }),
    "integration_action",
    { action: "revoke" },
  );
}

// ── Signals / Decisions / Watchdogs / Actions ──

export function getSignals() {
  return get("/signals");
}
export function getDecisions() {
  return get("/decisions");
}
export function getWatchdogs() {
  return get("/watchdogs");
}
export function getActions() {
  return get("/actions");
}

// ── Policies ──

export interface Policy {
  id: string;
  source: string;
  rule: string;
  active?: boolean;
}

export function getPolicies() {
  return get<{ policies: Policy[] }>("/policies");
}

export function createPolicy(source: string, rule: string) {
  return post("/policies", { source, rule });
}

export function deletePolicy(id: string) {
  return del("/policies", { id });
}

// ── Scheduler / Routines ──
// Moved to ./scheduler.ts; re-exported here for back-compat with existing
// imports from "./api/client" or "../api/client".
export type {
  CreateSchedulerJobBody,
  PatchSchedulerJobBody,
  PatchSchedulerJobResponse,
  SchedulerActivity,
  SchedulerJob,
  SchedulerRevision,
  SchedulerRun,
  SystemCronSpec,
} from "./scheduler";
export {
  createSchedulerJob,
  getScheduler,
  getSchedulerActivity,
  getSchedulerRevisions,
  getSchedulerRuns,
  getSystemCronSpecs,
  patchSchedulerJob,
  restoreSchedulerRevision,
  runSchedulerJob,
} from "./scheduler";
// ── Skills (moved to ./skills to respect the file-size budget) ──
export * from "./skills";

// ── Memory ──

export function getMemory(channel: string) {
  return get("/memory", { channel: channel || "general" });
}

export function setMemory(namespace: string, key: string, value: string) {
  return post("/memory", { namespace, key, value });
}

// ── Config (Settings) ──

// LLMRuntimeKind names a directly-dispatchable LLM runtime — the kinds that
// belong in any runtime picker (Settings default-runtime, AgentProfilePanel
// Runtime section, AgentWizard provider field). Mirrors the non-gateway
// subset returned by provider.LLMProviderKinds in the Go layer.
export type LLMRuntimeKind =
  | "claude-code"
  | "atlascloud"
  | "ollama"
  | "codex"
  | "opencode"
  | "mlx-lm"
  | "exo";

// GatewayKind names a runtime that is reached through an integration gateway
// rather than dispatched directly. Gateway-bound agents are imported via the
// Integrations app (OpenClaw / Hermes) and never appear in runtime pickers;
// they receive a "Managed by <Gateway>" badge on the agent profile.
export type GatewayKind = "openclaw" | "openclaw-http" | "hermes-agent";

// LLMProvider is the union of both — used wherever a value carries either an
// LLM runtime or a gateway tag (per-agent ProviderBinding.Kind on the wire,
// ConfigSnapshot.llm_provider for backward compatibility). New UI code should
// prefer LLMRuntimeKind / GatewayKind and only widen to LLMProvider at the
// raw-wire boundary.
export type LLMProvider = LLMRuntimeKind | GatewayKind;
export type MemoryBackend = "markdown" | "nex" | "gbrain" | "none";
export type ActionProvider = "auto" | "one" | "composio" | "";

export interface ProviderEndpoint {
  base_url?: string;
  model?: string;
}

// LocalProviderStatus mirrors internal/team/local_providers_status.go.
// One entry per registered local OpenAI-compatible kind. Test
// `TestComputeLocalProviderStatuses_DocumentedSurface` keeps the JSON
// field names in lockstep with this type.
export interface LocalProviderStatus {
  kind: string;
  binary_installed: boolean;
  binary_path?: string;
  binary_version?: string;
  endpoint: string;
  model: string;
  reachable: boolean;
  loaded_model?: string;
  probed: boolean;
  probe_skipped_note?: string;
  platform_supported: boolean;
  windows_note?: string;
  install?: Record<string, string>;
  start?: Record<string, string>;
  notes?: string[];
}

export interface ConfigSnapshot {
  // Runtime
  llm_provider?: LLMProvider;
  llm_provider_configured?: boolean;
  llm_provider_priority?: string[];
  // llm_provider_kinds is the non-gateway subset of registered runtimes —
  // the safe list to render in any runtime picker. Read this off the wire
  // instead of hardcoding the union so a future provider registered on the
  // Go side appears in the UI without a frontend change.
  llm_provider_kinds?: LLMRuntimeKind[];
  // gateway_kinds is the inverse — the registered gateway runtimes. The
  // Integrations app enumerates these to know which gateway cards (OpenClaw,
  // Hermes) are compiled in and connectable.
  gateway_kinds?: GatewayKind[];
  provider_endpoints?: Record<string, ProviderEndpoint>;
  memory_backend?: MemoryBackend;
  action_provider?: ActionProvider;
  team_lead_slug?: string;
  max_concurrent_agents?: number;
  default_format?: string;
  default_timeout?: number;
  blueprint?: string;
  // Workspace
  email?: string;
  workspace_id?: string;
  workspace_slug?: string;
  dev_url?: string;
  // Company
  company_name?: string;
  company_description?: string;
  company_goals?: string;
  company_size?: string;
  company_priority?: string;
  // Polling
  insights_poll_minutes?: number;
  task_follow_up_minutes?: number;
  task_reminder_minutes?: number;
  task_recheck_minutes?: number;
  // Secret flags
  api_key_set?: boolean;
  openai_key_set?: boolean;
  anthropic_key_set?: boolean;
  gemini_key_set?: boolean;
  minimax_key_set?: boolean;
  one_key_set?: boolean;
  composio_key_set?: boolean;
  telegram_token_set?: boolean;
  openclaw_token_set?: boolean;
  openclaw_gateway_url?: string;
  // Product-analytics consent (PostHog). Both default true. `analytics_configured`
  // reports whether the broker injects a key; the frontend ORs it with its own
  // build-time key to decide whether the toggles are meaningful to show.
  analytics_telemetry_enabled?: boolean;
  analytics_session_recording_enabled?: boolean;
  analytics_configured?: boolean;
  config_path?: string;
}

export type ConfigUpdate = Partial<{
  llm_provider: LLMProvider | "";
  llm_provider_priority: LLMRuntimeKind[];
  provider_endpoints: Record<string, ProviderEndpoint>;
  memory_backend: MemoryBackend;
  action_provider: ActionProvider;
  team_lead_slug: string;
  max_concurrent_agents: number;
  default_format: string;
  default_timeout: number;
  blueprint: string;
  email: string;
  dev_url: string;
  company_name: string;
  company_description: string;
  company_goals: string;
  company_size: string;
  company_priority: string;
  insights_poll_minutes: number;
  task_follow_up_minutes: number;
  task_reminder_minutes: number;
  task_recheck_minutes: number;
  // Secret-write fields — sent as plaintext on write, never returned on read
  api_key: string;
  openai_api_key: string;
  anthropic_api_key: string;
  gemini_api_key: string;
  minimax_api_key: string;
  one_api_key: string;
  composio_api_key: string;
  telegram_bot_token: string;
  openclaw_token: string;
  openclaw_gateway_url: string;
  // Product-analytics consent toggles.
  analytics_telemetry_enabled: boolean;
  analytics_session_recording_enabled: boolean;
}>;

// The narrow slice of GET /config the operator surfaces read to decide whether
// the real voice call is available. Shared so useRealtimeConfig and
// SettingsSurface stay in lockstep if the response shape changes.
export interface ConfigStatus {
  openai_key_set?: boolean;
  realtime_model?: string;
}

export function getConfig() {
  return get<ConfigSnapshot>("/config");
}

export function updateConfig(configPatch: ConfigUpdate) {
  return post<{ status: string }>("/config", configPatch);
}

// Doctor endpoint — one entry per registered local OpenAI-compatible
// runtime. Settings page polls this; Onboarding wizard reads it on
// mount. The broker probes loopback endpoints only (see
// internal/team/local_providers_status.go), so calling this never
// triggers outbound traffic.
export function getLocalProvidersStatus() {
  return get<LocalProviderStatus[]>("/status/local-providers");
}

// ── Image generation ──

export interface ImageProviderStatus {
  kind: string;
  label: string;
  blurb: string;
  reachable: boolean;
  configured: boolean;
  base_url?: string;
  default_model?: string;
  supported_models?: string[];
  supports_image: boolean;
  supports_video: boolean;
  needs_api_key: boolean;
  api_key_set: boolean;
  implementation_ok: boolean;
  setup_hint?: string;
}

export function getImageProviders() {
  return get<{ providers: ImageProviderStatus[] }>("/image-providers");
}

export function setImageProviderConfig(opts: {
  kind: string;
  api_key?: string;
  base_url?: string;
  model?: string;
}) {
  return put<ImageProviderStatus[]>("/image-providers", opts);
}

// ── Workspace wipes (Danger Zone) ──

// WorkspaceWipeResult shape mirrors internal/workspace.Result plus the flags
// the HTTP handler adds (restart_required, redirect). The UI just needs ok +
// a reason to reload, but we surface `removed` so users can see what went.
export interface WorkspaceWipeResult {
  ok: boolean;
  restart_required?: boolean;
  redirect?: string;
  removed?: string[];
  errors?: string[];
  error?: string;
}

// resetWorkspace is the narrow wipe: clears broker runtime state only.
// Team roster, company identity, tasks, and workflows all survive. Call
// window.location.reload() after success so the UI picks up the empty
// broker state.
export function resetWorkspace() {
  return postWithTimeout<WorkspaceWipeResult>("/workspace/reset", {}, 20_000);
}

// shredWorkspace is the full wipe: broker runtime + team + company + office,
// workflows, logs, sessions, provider state, and local markdown memory.
// The broker resets in place after success so onboarding can reopen immediately.
export function shredWorkspace() {
  return postWithTimeout<WorkspaceWipeResult>("/workspace/shred", {}, 20_000);
}

// restartBroker asks the host web UI server to restart the broker listener.
// In same-origin web mode, ServeWebUI handles /api/broker/restart before the
// generic proxy, so the action still works when the broker HTTP listener is
// unreachable. The browser's SSE EventSource reconnects automatically once the
// listener is ready; useBrokerEvents refreshes auth before marking connected.
export interface BrokerRestartStatus {
  ok: boolean;
  url?: string;
}

export function restartBroker() {
  return post<BrokerRestartStatus>("/broker/restart");
}

// ── Telegram /connect wizard ──
// These mirror the TUI's `/connect telegram` flow but drive it from the web.
// Pass an explicit `token` to override what the broker has on disk; pass an
// empty string to use the saved token from config / WUPHF_TELEGRAM_BOT_TOKEN.

export interface TelegramVerifyResponse {
  ok: boolean;
  bot_name?: string;
  error?: string;
}

export interface TelegramGroup {
  // chat_id comes from Go's int64 on the wire. Telegram's API docs say chat
  // IDs may have at most 52 significant bits — exactly inside JS's
  // Number.MAX_SAFE_INTEGER (53 bits). Today's supergroup IDs (~13 digits)
  // are well below that, so a plain `number` is safe. If Telegram ever
  // widens past 52 bits this needs to become a string (or bigint with an
  // explicit serialiser) to avoid silent precision loss on the round-trip.
  chat_id: number;
  title: string;
  type: string;
}

export interface TelegramDiscoverResponse {
  groups: TelegramGroup[];
}

export interface TelegramConnectResponse {
  channel_slug: string;
  group_title: string;
}

export function verifyTelegramBot(telegramToken: string, signal?: AbortSignal) {
  return post<TelegramVerifyResponse>(
    "/telegram/verify",
    { token: telegramToken },
    { signal },
  );
}

export function discoverTelegramChats(
  telegramToken: string,
  signal?: AbortSignal,
) {
  return post<TelegramDiscoverResponse>(
    "/telegram/discover",
    { token: telegramToken },
    { signal },
  );
}

export function connectTelegramChannel(
  opts: {
    token?: string;
    chat_id: number;
    title?: string;
    type?: string;
  },
  signal?: AbortSignal,
) {
  return post<TelegramConnectResponse>("/telegram/connect", opts, { signal });
}
