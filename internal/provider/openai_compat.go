package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nex-crm/wuphf/internal/agent"
	"github.com/nex-crm/wuphf/internal/config"
)

// NewOpenAICompatStreamFn builds the StreamFn factory shared by all OpenAI-
// compatible local backends (MLX-LM, Ollama, Exo, llama.cpp's server, vLLM,
// etc). The returned factory is the value providers store in Entry.StreamFn:
// the dispatcher passes it an agent slug, and the inner closure is invoked
// per agent turn with the conversation history + registered tools.
//
// kind is the provider Kind string used for error messages and for resolving
// the runtime base URL / model via config.ResolveProviderEndpoint, which
// layers env > config > compile-time defaults. Endpoints are resolved at
// stream time (not at process start) so /provider switches and
// ~/.wuphf/config.json edits take effect on the next agent turn without
// requiring a restart.
//
// Tool calls are propagated end-to-end: agent.AgentTool entries become
// OpenAI-shape `tools[]` with the tool's JSON schema, and assistant
// `tool_calls` deltas are accumulated by index and re-emitted as
// agent.StreamChunk{Type:"tool_use"} once finish_reason flips to
// "tool_calls". The accumulator keys on the OpenAI delta `index` because
// servers split a single tool call across multiple SSE frames and may
// interleave deltas for parallel tool calls.
func NewOpenAICompatStreamFn(kind, defaultBaseURL, defaultModel string) func(string) agent.StreamFn {
	registerOpenAICompatDefaults(kind, defaultBaseURL, defaultModel)
	return func(agentSlug string) agent.StreamFn {
		return func(msgs []agent.Message, tools []agent.AgentTool) <-chan agent.StreamChunk {
			ch := make(chan agent.StreamChunk, 64)
			go runOpenAICompatStream(context.Background(), ch, kind, defaultBaseURL, defaultModel, msgs, tools, "", agentSlug)
			return ch
		}
	}
}

// NewOpenAICompatStreamFnWithCtx returns a StreamFn whose underlying HTTP
// request lifetime is bound to ctx. Cancelling ctx aborts the in-flight
// request and frees the local server's inference slot — important for
// single-threaded inference servers (mlx_lm.server) where a leaked
// request blocks every queued agent until the model finishes generating.
//
// The kind must already be registered (its defaults are looked up from
// the registry populated at init() time). Callers without a turn ctx
// should use NewOpenAICompatStreamFn instead.
func NewOpenAICompatStreamFnWithCtx(ctx context.Context, kind string) agent.StreamFn {
	baseURL, model := openAICompatDefaultsFor(kind)
	if baseURL == "" && model == "" {
		// Unregistered kind: emit a clear error on the first call instead
		// of letting an empty base URL surface six layers down as a
		// confusing "URL must be absolute" error from net/http.
		return func(_ []agent.Message, _ []agent.AgentTool) <-chan agent.StreamChunk {
			ch := make(chan agent.StreamChunk, 1)
			ch <- agent.StreamChunk{
				Type:    "error",
				Content: fmt.Sprintf("openai-compat: kind %q has no registered defaults — did its init() forget to call NewOpenAICompatStreamFn?", kind),
			}
			close(ch)
			return ch
		}
	}
	return func(msgs []agent.Message, tools []agent.AgentTool) <-chan agent.StreamChunk {
		ch := make(chan agent.StreamChunk, 64)
		go runOpenAICompatStream(ctx, ch, kind, baseURL, model, msgs, tools, "", "")
		return ch
	}
}

// NewOpenAICompatStreamFnWithCtxAndModel is the per-agent variant of
// NewOpenAICompatStreamFnWithCtx. modelOverride, when non-empty, wins over
// env / config / compile-time defaults for the model field on outbound
// /v1/chat/completions requests. This is how a member's persisted
// ProviderBinding.Model lands on the wire — without it every agent would
// send the install-wide default model name regardless of what's stored on
// their member record.
func NewOpenAICompatStreamFnWithCtxAndModel(ctx context.Context, kind, modelOverride string) agent.StreamFn {
	return NewOpenAICompatStreamFnWithCtxModelAndAgent(ctx, kind, modelOverride, "")
}

// NewOpenAICompatStreamFnWithCtxModelAndAgent is the slug-aware variant used
// by providers whose HTTP surface can preserve server-side per-agent state.
func NewOpenAICompatStreamFnWithCtxModelAndAgent(ctx context.Context, kind, modelOverride, agentSlug string) agent.StreamFn {
	baseURL, model := openAICompatDefaultsFor(kind)
	if baseURL == "" && model == "" {
		return func(_ []agent.Message, _ []agent.AgentTool) <-chan agent.StreamChunk {
			ch := make(chan agent.StreamChunk, 1)
			ch <- agent.StreamChunk{
				Type:    "error",
				Content: fmt.Sprintf("openai-compat: kind %q has no registered defaults — did its init() forget to call NewOpenAICompatStreamFn?", kind),
			}
			close(ch)
			return ch
		}
	}
	return func(msgs []agent.Message, tools []agent.AgentTool) <-chan agent.StreamChunk {
		ch := make(chan agent.StreamChunk, 64)
		go runOpenAICompatStream(ctx, ch, kind, baseURL, model, msgs, tools, modelOverride, agentSlug)
		return ch
	}
}

// openAICompatDefaultsByKind is populated by NewOpenAICompatStreamFn so
// the ctx-aware variant can resolve compile-time defaults without
// duplicating the kind→endpoint table.
var (
	openAICompatDefaultsMu sync.RWMutex
	openAICompatDefaults   = map[string]openAICompatKindDefaults{}
)

type openAICompatKindDefaults struct {
	baseURL string
	model   string
}

func registerOpenAICompatDefaults(kind, baseURL, model string) {
	openAICompatDefaultsMu.Lock()
	defer openAICompatDefaultsMu.Unlock()
	openAICompatDefaults[kind] = openAICompatKindDefaults{baseURL, model}
}

func openAICompatDefaultsFor(kind string) (string, string) {
	openAICompatDefaultsMu.RLock()
	defer openAICompatDefaultsMu.RUnlock()
	d := openAICompatDefaults[kind]
	return d.baseURL, d.model
}

// OpenAICompatDefaults returns the compile-time default base URL and
// model for kind, or ("", "") if kind has no registered defaults. The
// status endpoint in internal/team/ uses this to resolve the effective
// endpoint shown to users without duplicating the kind→endpoint table.
// Exported because the team package can't import the unexported helper.
func OpenAICompatDefaults(kind string) (baseURL, model string) {
	return openAICompatDefaultsFor(kind)
}

// normalizeOpenAICompatEndpoint joins baseURL with /chat/completions,
// auto-appending /v1 when the user pasted a server's listening address
// without it (a common copy/paste from `mlx_lm.server` startup output —
// `Starting httpd at 127.0.0.1 on port 8080` doesn't mention /v1, so a
// naive base_url=http://127.0.0.1:8080 would 404 with no hint at the
// fix).
//
// Query strings on the base URL are split off before the /v1 test so
// `https://gw/v1?key=abc` round-trips to
// `https://gw/v1/chat/completions?key=abc` rather than concatenating
// junk. Auth-via-querystring is rare for this surface but supported by
// some proxied gateways.
func normalizeOpenAICompatEndpoint(baseURL string) string {
	pathPart, queryPart := baseURL, ""
	if idx := strings.IndexByte(baseURL, '?'); idx >= 0 {
		pathPart, queryPart = baseURL[:idx], baseURL[idx:]
	}
	pathPart = strings.TrimRight(pathPart, "/")
	if !strings.HasSuffix(pathPart, "/v1") &&
		!strings.Contains(pathPart, "/v1/") {
		pathPart += "/v1"
	}
	return pathPart + "/chat/completions" + queryPart
}

// httpClientForOpenAICompat is overridable for tests (httptest.Server transport).
var httpClientForOpenAICompat = func() *http.Client {
	// Long-running streaming connections — no overall request timeout
	// (Timeout: 0) and no ResponseHeaderTimeout because a busy local
	// server can take 30s+ to emit the first SSE frame for a 32B model.
	// Dial timeout is bounded explicitly so a wedged DNS resolver or
	// unreachable host can't hang an agent turn until the OS-level
	// connect timeout fires (>60s on macOS). Default 5s is comfortable
	// for loopback; users pointing WUPHF_OLLAMA_BASE_URL at a remote box
	// (e.g. a Mac Studio over Wi-Fi) can bump via
	// WUPHF_OPENAI_COMPAT_DIAL_TIMEOUT_SECONDS.
	tr := &http.Transport{
		DialContext: (&net.Dialer{Timeout: openAICompatDialTimeout()}).DialContext,
	}
	return &http.Client{Transport: tr, Timeout: 0}
}

func openAICompatDialTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("WUPHF_OPENAI_COMPAT_DIAL_TIMEOUT_SECONDS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Second
}

func setOpenAICompatProviderHeaders(req *http.Request, kind, agentSlug string) {
	apiKey := resolveOpenAICompatAPIKey(kind)
	if apiKey == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	if kind != KindHermesAgent {
		return
	}
	sessionID := hermesAgentSessionID(agentSlug)
	if sessionID == "" {
		return
	}
	// Hermes only accepts caller-supplied session headers on authenticated
	// requests. Tying them to the office slug gives each WUPHF member a stable
	// Hermes-side conversation and memory scope, matching the per-agent
	// continuity users expect from OpenClaw-backed members.
	req.Header.Set("X-Hermes-Session-Id", sessionID)
	req.Header.Set("X-Hermes-Session-Key", sessionID)
}

func resolveOpenAICompatAPIKey(kind string) string {
	envKind := strings.ToUpper(strings.ReplaceAll(kind, "-", "_"))
	if v := strings.TrimSpace(os.Getenv("WUPHF_" + envKind + "_API_KEY")); v != "" {
		return v
	}
	if kind == KindAtlasCloud {
		if v := strings.TrimSpace(os.Getenv("ATLASCLOUD_API_KEY")); v != "" {
			return v
		}
		if v := strings.TrimSpace(os.Getenv("ATLAS_CLOUD_API_KEY")); v != "" {
			return v
		}
	}
	if kind == KindHermesAgent {
		if v := strings.TrimSpace(os.Getenv("API_SERVER_KEY")); v != "" {
			return v
		}
	}
	if kind == KindOpenclawHTTP {
		if v := strings.TrimSpace(os.Getenv("OPENCLAW_GATEWAY_TOKEN")); v != "" {
			return v
		}
		if v := config.ResolveOpenclawToken(); v != "" {
			return v
		}
	}
	return ""
}

// OpenAICompatAPIKey returns the bearer token WUPHF will use for kind's
// OpenAI-compatible HTTP requests, if one is configured. It is exported for
// local runtime health probes so authenticated gateways such as OpenClaw and
// Hermes don't look offline just because /v1/models rejects anonymous calls.
func OpenAICompatAPIKey(kind string) string {
	return resolveOpenAICompatAPIKey(kind)
}

func hermesAgentSessionID(agentSlug string) string {
	slug := strings.ToLower(strings.TrimSpace(agentSlug))
	if slug == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	clean := strings.Trim(b.String(), "-_.")
	if clean == "" {
		return ""
	}
	id := "wuphf-" + clean
	if len(id) > 256 {
		return id[:256]
	}
	return id
}

func runOpenAICompatStream(
	parentCtx context.Context,
	ch chan<- agent.StreamChunk,
	kind, defaultBaseURL, defaultModel string,
	msgs []agent.Message, tools []agent.AgentTool,
	modelOverride string,
	agentSlug string,
) {
	defer close(ch)

	baseURL, model := config.ResolveProviderEndpoint(kind, defaultBaseURL, defaultModel)
	if trimmed := strings.TrimSpace(modelOverride); trimmed != "" {
		model = trimmed
	}
	endpoint := normalizeOpenAICompatEndpoint(baseURL)

	body := openaiRequest{
		Model:    model,
		Stream:   true,
		Messages: agentMsgsToOpenAI(msgs),
		// Ask the server to emit a final SSE frame containing token usage.
		// Recognised by ollama, recent mlx_lm.server, exo, llama.cpp's
		// server, and the canonical OpenAI API. Servers that don't
		// implement this option ignore the unknown field, so the request
		// stays well-formed; we just won't see usage from those servers
		// (which is the same as today's behaviour).
		StreamOptions: &openaiStreamOptions{IncludeUsage: true},
	}
	if kind == KindOpenclawHTTP {
		body.User = hermesAgentSessionID(agentSlug)
	}
	if len(tools) > 0 {
		body.Tools = agentToolsToOpenAI(tools)
		body.ToolChoice = "auto"
	}

	payload, err := json.Marshal(body)
	if err != nil {
		ch <- agent.StreamChunk{Type: "error", Content: fmt.Sprintf("openai-compat (%s): marshal request: %v", kind, err)}
		return
	}

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		ch <- agent.StreamChunk{Type: "error", Content: fmt.Sprintf("openai-compat (%s): build request: %v", kind, err)}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	setOpenAICompatProviderHeaders(req, kind, agentSlug)

	resp, err := httpClientForOpenAICompat().Do(req)
	if err != nil {
		ch <- agent.StreamChunk{Type: "error", Content: fmt.Sprintf("openai-compat (%s): connect %s: %v. Is the local server running?", kind, endpoint, err)}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read up to 8 KiB so we can surface the server's error JSON without
		// pulling a multi-megabyte HTML page into the chunk.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		ch <- agent.StreamChunk{
			Type:    "error",
			Content: fmt.Sprintf("openai-compat (%s): HTTP %d from %s: %s", kind, resp.StatusCode, endpoint, strings.TrimSpace(string(errBody))),
		}
		return
	}

	parseOpenAISSEStream(ch, kind, resp.Body)
}

// parseOpenAISSEStream consumes the SSE body and translates it into wuphf's
// StreamChunk vocabulary. Pulled out for direct testing without needing an
// HTTP layer.
func parseOpenAISSEStream(ch chan<- agent.StreamChunk, kind string, body io.Reader) {
	reader := bufio.NewReaderSize(body, 64<<10)

	// toolAccumulators[index] = partial tool call being assembled across
	// multiple SSE frames. OpenAI streams tool args as JSON-string fragments;
	// we concatenate and parse once finish_reason fires.
	toolAccumulators := map[int]*toolAccum{}
	var emitOrder []int // preserve emission order matching first-seen index

	// textBuf collects content deltas across the whole stream so we can fall
	// back to JSON-in-content tool-call detection at flush time. Some open
	// models (notably Qwen2.5-Coder-* via mlx_lm.server) emit
	// {"name":"...","arguments":{...}} as plain content instead of structured
	// tool_calls deltas; if the structured path is empty, we re-parse the
	// accumulated text and synthesize a tool_use chunk so the rest of wuphf
	// doesn't have to care which dialect the model picked.
	var textBuf strings.Builder

	// latestUsage holds the most recent usage frame seen this turn. With
	// stream_options.include_usage the server sends exactly one trailing
	// usage frame; some servers (llama.cpp's, older mlx_lm.server) instead
	// stamp partial usage on every event, so we always take the last one.
	var latestUsage *openaiUsage

	flushUsage := func() {
		if latestUsage == nil {
			return
		}
		ch <- agent.StreamChunk{
			Type:         "usage",
			InputTokens:  latestUsage.PromptTokens,
			OutputTokens: latestUsage.CompletionTokens,
		}
	}

	flushTools := func() {
		emittedStructured := false
		for _, idx := range emitOrder {
			acc := toolAccumulators[idx]
			if acc == nil || acc.name == "" {
				continue
			}
			emittedStructured = true
			rawArgs := acc.argsBuilder.String()
			params := map[string]any{}
			if strings.TrimSpace(rawArgs) != "" {
				if err := json.Unmarshal([]byte(rawArgs), &params); err != nil {
					ch <- agent.StreamChunk{
						Type:    "error",
						Content: fmt.Sprintf("openai-compat (%s): tool %q produced unparseable arguments: %v\nraw: %s", kind, acc.name, err, rawArgs),
					}
					continue
				}
			}
			ch <- agent.StreamChunk{
				Type:       "tool_use",
				ToolName:   acc.name,
				ToolParams: params,
				ToolUseID:  acc.id,
				ToolInput:  rawArgs,
			}
		}
		if !emittedStructured {
			// No structured tool_calls — try the JSON-in-content fallback.
			// Conservative: only emit if the entire text is a single JSON
			// object with both "name" and "arguments". A wrong parse is worse
			// than a missed one because the orchestrator may execute it.
			if name, args, ok := parseJSONInContentToolCall(textBuf.String()); ok {
				params := map[string]any{}
				_ = json.Unmarshal([]byte(args), &params)
				ch <- agent.StreamChunk{
					Type:       "tool_use",
					ToolName:   name,
					ToolParams: params,
					ToolInput:  args,
				}
			}
		}
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			payload, ok := stripSSEDataPrefix(line)
			if ok {
				// Empty `data:` frames are SSE keep-alive heartbeats —
				// ollama and llama.cpp's server emit them under load.
				// Skip silently; treating them as malformed JSON would
				// abort the whole turn.
				if payload == "" {
					continue
				}
				if payload == "[DONE]" {
					flushTools()
					flushUsage()
					return
				}
				processOpenAISSEEvent(ch, kind, payload, toolAccumulators, &emitOrder, &textBuf, &latestUsage)
			}
		}
		if err != nil {
			if err == io.EOF {
				flushTools()
				flushUsage()
				return
			}
			// Surface anything we already captured before the read
			// failed — a long generation that broke connection mid-
			// stream still produced real tokens the user wants
			// counted, matching the headless runner's intent at
			// headless_openai_compat.go's "Record token counts even
			// on partial / errored turns" comment.
			flushUsage()
			ch <- agent.StreamChunk{Type: "error", Content: fmt.Sprintf("openai-compat (%s): read stream: %v", kind, err)}
			return
		}
	}
}

// parseJSONInContentToolCall returns (name, rawArgs, true) when content
// contains an unambiguous tool invocation that didn't come through the
// structured tool_calls SSE deltas. Three dialects are recognised, in
// this order:
//
//  1. <tools>{json}</tools> block (Qwen2.5-Coder default) — embedded in
//     prose, only the body is parsed.
//  2. Bare JSON object {"name":"<id>","arguments":{...}} — emitted by
//     the OpenAI-compatible side of mlx_lm.server when a model isn't
//     OpenAI-fine-tuned. Trailing chat-template markers like
//     `<|im_end|>` / `<|endoftext|>` and trailing prose summaries are
//     stripped before the match so a real tool call still fires.
//  3. The first balanced {…} that parses as a {name, arguments} pair
//     and is followed only by chat-template tokens, whitespace, or
//     trailing summary lines. Lenient enough to catch Qwen's
//     `{json}<|im_end|>` shape without firing on prose-with-an-example.
//
// Returns ok=false on anything else: prose mentioning a tool, JSON with
// extra fields, arguments-as-string, multiple <tools> blocks, etc.
// Conservative by design — a false negative shows up as text; a false
// positive executes a tool that wasn't actually requested.
func parseJSONInContentToolCall(content string) (string, string, bool) {
	s := strings.TrimSpace(content)
	s = stripChatTemplateTerminators(s)
	s = stripMarkdownCodeFence(s)

	// Dialect 1: <tools>{...}</tools>. Check first because it can be
	// embedded inside surrounding prose, whereas the others require
	// the JSON to dominate the content.
	if name, args, ok := extractToolsTagToolCall(s); ok {
		return name, args, true
	}

	// Dialect 2: bare JSON object spanning (essentially) the whole
	// content — after we've already stripped the trailing template
	// terminators above.
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		if name, args, ok := parseToolCallObject(s); ok {
			return name, args, true
		}
	}

	// Dialect 3: first balanced {…} object. Required to start at index
	// 0 (so the tool-call is the first thing the model emitted) — if
	// the model wrote prose THEN a JSON, we treat it as prose. The
	// substring must parse as a {name, arguments} pair AND nothing
	// after it can be code/JSON; only whitespace, template markers,
	// and a brief trailing summary are tolerated.
	if name, args, ok := extractFirstBalancedToolCall(s); ok {
		return name, args, true
	}

	return "", "", false
}

// stripMarkdownCodeFence removes a leading ```optional-lang\n and a
// trailing ``` so a tool-call JSON wrapped in markdown still parses.
// Qwen2.5-Coder reflexively wraps code-shaped output (including JSON
// tool calls) in fences when it thinks it's "writing code"; this is
// what produced the user-visible bug where the agent reply was a
// rendered code block of JSON instead of an executed tool.
func stripMarkdownCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		// Drop the opening fence line: ``` or ```json or ```anything.
		if nl := strings.IndexByte(t, '\n'); nl >= 0 {
			t = t[nl+1:]
		}
	}
	t = strings.TrimSpace(t)
	t = strings.TrimSuffix(t, "```")
	return strings.TrimSpace(t)
}

// stripChatTemplateTerminators removes trailing chat-template end-of-
// turn tokens that some servers leak into the content stream. mlx_lm
// .server with Qwen-family templates emits `<|im_end|>`; Llama
// templates emit `<|eot_id|>` or `<|end_of_text|>`. Removing these
// lets the {…}-suffix check on the JSON-in-content path actually fire
// when the model wrapped its tool call as `{json}<|im_end|>`.
func stripChatTemplateTerminators(s string) string {
	terminators := []string{
		"<|im_end|>",
		"<|endoftext|>",
		"<|end_of_text|>",
		"<|eot_id|>",
		"<|eom_id|>",
		"</s>",
	}
	for changed := true; changed; {
		changed = false
		s = strings.TrimRight(s, " \t\r\n")
		for _, t := range terminators {
			if strings.HasSuffix(s, t) {
				s = strings.TrimSuffix(s, t)
				changed = true
			}
		}
	}
	return strings.TrimSpace(s)
}

// extractFirstBalancedToolCall scans for the first `{` at index 0 and
// walks brace depth (respecting strings + escapes) to find the
// matching `}`. The substring is then validated as a tool-call object.
// What follows must be only whitespace, chat-template terminators
// (already stripped above), or a brief trailing summary <= 200 chars
// without further braces — preventing prose-with-example from
// triggering an invocation.
func extractFirstBalancedToolCall(s string) (string, string, bool) {
	if !strings.HasPrefix(s, "{") {
		return "", "", false
	}
	depth := 0
	inString := false
	escaped := false
	end := -1
	for i, r := range s {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inString {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if r == '{' {
			depth++
		} else if r == '}' {
			depth--
			if depth == 0 {
				end = i + 1
				break
			}
		}
	}
	if end < 0 {
		return "", "", false
	}
	candidate := s[:end]
	tail := strings.TrimSpace(s[end:])
	// The tail can be empty, a brief summary, or repeated template
	// markers (already stripped — left in for safety). Reject if the
	// tail itself contains another `{` — that's prose mentioning JSON,
	// not a clean tool-call-then-summary.
	if strings.ContainsRune(tail, '{') {
		return "", "", false
	}
	if len(tail) > 200 {
		return "", "", false
	}
	return parseToolCallObject(candidate)
}

// extractToolsTagToolCall pulls the body out of a single <tools>...</tools>
// block, then defers to parseToolCallObject for shape validation.
// Multiple <tools> blocks → ok=false (ambiguous, conservative).
func extractToolsTagToolCall(content string) (string, string, bool) {
	openIdx := strings.Index(content, "<tools>")
	if openIdx < 0 {
		return "", "", false
	}
	if strings.Count(content, "<tools>") != 1 {
		return "", "", false
	}
	closeIdx := strings.Index(content, "</tools>")
	if closeIdx < 0 || closeIdx < openIdx {
		return "", "", false
	}
	body := strings.TrimSpace(content[openIdx+len("<tools>") : closeIdx])
	if !strings.HasPrefix(body, "{") || !strings.HasSuffix(body, "}") {
		return "", "", false
	}
	return parseToolCallObject(body)
}

// parseToolCallObject validates a single {"name":..., "arguments":{...}}
// object. Shared between dialects.
func parseToolCallObject(jsonStr string) (string, string, bool) {
	var probe struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &probe); err != nil {
		return "", "", false
	}
	if strings.TrimSpace(probe.Name) == "" || len(probe.Arguments) == 0 {
		return "", "", false
	}
	args := strings.TrimSpace(string(probe.Arguments))
	// Only object-shaped arguments; reject string/number/array which are
	// almost certainly the model talking about a tool, not invoking one.
	if !strings.HasPrefix(args, "{") {
		return "", "", false
	}
	return probe.Name, args, true
}

func stripSSEDataPrefix(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", false
	}
	if !strings.HasPrefix(line, "data:") {
		// Ignore SSE comments (":...") and other event-stream metadata.
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}

func processOpenAISSEEvent(
	ch chan<- agent.StreamChunk,
	kind, payload string,
	toolAccumulators map[int]*toolAccum,
	emitOrder *[]int,
	textBuf *strings.Builder,
	latestUsage **openaiUsage,
) {
	var ev openaiStreamChunk
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		ch <- agent.StreamChunk{
			Type:    "error",
			Content: fmt.Sprintf("openai-compat (%s): malformed SSE payload: %v\nraw: %s", kind, err, payload),
		}
		return
	}
	// Capture usage from any frame that carries it. With
	// stream_options.include_usage the canonical pattern is a single
	// trailing frame whose `choices` is empty and `usage` is set;
	// llama.cpp's server stamps partial usage on every event instead.
	//
	// Latch policy: only overwrite when the new frame's totals are
	// monotonically larger than what we've already seen. Prevents two
	// real failure modes:
	//   1. A degenerate trailing `0/0` frame from clobbering an earlier
	//      complete count (some servers emit one on errored turns).
	//   2. A speculative-decoding reject path where mlx_lm.server has
	//      been observed to follow up a `prompt_tokens=N, completion=K`
	//      frame with `prompt_tokens=N, completion=0` after rolling
	//      back rejected tokens — without monotonic guard we'd lose K.
	if ev.Usage != nil && latestUsage != nil {
		incoming := *ev.Usage
		if *latestUsage == nil ||
			(incoming.PromptTokens >= (*latestUsage).PromptTokens &&
				incoming.CompletionTokens >= (*latestUsage).CompletionTokens) {
			if incoming.PromptTokens > 0 || incoming.CompletionTokens > 0 {
				*latestUsage = &incoming
			}
		}
	}
	if len(ev.Choices) == 0 {
		return
	}
	choice := ev.Choices[0]
	if c := choice.Delta.Content; c != "" {
		textBuf.WriteString(c)
		ch <- agent.StreamChunk{Type: "text", Content: c}
	}
	for _, tc := range choice.Delta.ToolCalls {
		acc, exists := toolAccumulators[tc.Index]
		if !exists {
			acc = &toolAccum{}
			toolAccumulators[tc.Index] = acc
			*emitOrder = append(*emitOrder, tc.Index)
		}
		if tc.ID != "" {
			acc.id = tc.ID
		}
		if tc.Function.Name != "" {
			acc.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			acc.argsBuilder.WriteString(tc.Function.Arguments)
		}
	}
}

type toolAccum struct {
	id          string
	name        string
	argsBuilder strings.Builder
}

// --- request shape ----------------------------------------------------------

type openaiRequest struct {
	Model         string               `json:"model"`
	Messages      []openaiMessage      `json:"messages"`
	Stream        bool                 `json:"stream"`
	User          string               `json:"user,omitempty"`
	StreamOptions *openaiStreamOptions `json:"stream_options,omitempty"`
	Tools         []openaiTool         `json:"tools,omitempty"`
	ToolChoice    string               `json:"tool_choice,omitempty"`
}

// openaiStreamOptions mirrors OpenAI's `stream_options` request field. We
// only set `include_usage` today; the struct exists as a separate type
// (rather than inlining a bool) so future options like `service_tier` can
// be added without reshaping the request body.
type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiTool struct {
	Type     string             `json:"type"`
	Function openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

func agentMsgsToOpenAI(msgs []agent.Message) []openaiMessage {
	out := make([]openaiMessage, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if role == "" {
			role = "user"
		}
		out = append(out, openaiMessage{Role: role, Content: m.Content})
	}
	return out
}

func agentToolsToOpenAI(tools []agent.AgentTool) []openaiTool {
	out := make([]openaiTool, 0, len(tools))
	for _, t := range tools {
		params := t.Schema
		if params == nil {
			// Some servers reject tools without a schema; supply an empty
			// JSON-schema object so the request is well-formed.
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, openaiTool{
			Type: "function",
			Function: openaiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

// --- response shape ---------------------------------------------------------

type openaiStreamChunk struct {
	Choices []openaiStreamChoice `json:"choices"`
	// Usage is populated on the final SSE frame when the server honours
	// stream_options.include_usage. Pointer so the zero value is
	// distinguishable from `prompt_tokens: 0` (unlikely in practice but
	// keeps the JSON round-trip honest).
	Usage *openaiUsage `json:"usage,omitempty"`
}

type openaiStreamChoice struct {
	Delta        openaiStreamDelta `json:"delta"`
	FinishReason string            `json:"finish_reason"`
}

// openaiUsage captures the token counts a local OpenAI-compatible server
// emits in its trailing usage frame. We don't extract cost: local runs
// have no marginal $ cost so a zero in the broker's cost_usd column is
// the right answer.
type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openaiStreamDelta struct {
	Role      string                 `json:"role,omitempty"`
	Content   string                 `json:"content,omitempty"`
	ToolCalls []openaiStreamToolCall `json:"tool_calls,omitempty"`
}

type openaiStreamToolCall struct {
	Index    int                      `json:"index"`
	ID       string                   `json:"id,omitempty"`
	Type     string                   `json:"type,omitempty"`
	Function openaiStreamToolFunction `json:"function"`
}

type openaiStreamToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
