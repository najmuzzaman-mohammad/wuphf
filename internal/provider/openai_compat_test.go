package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nex-crm/wuphf/internal/agent"
)

// helper: drain a StreamFn channel into a slice of chunks.
func drainChunks(t *testing.T, ch <-chan agent.StreamChunk) []agent.StreamChunk {
	t.Helper()
	var out []agent.StreamChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

// helper: build an SSE response body from raw event payloads (each becomes
// one `data: ...` frame), terminated by `data: [DONE]`.
func sseBody(events ...string) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("data: ")
		b.WriteString(ev)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestParseOpenAISSEStream_TextDeltas verifies plain content deltas produce
// ordered text chunks.
func TestParseOpenAISSEStream_TextDeltas(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"Hello "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
		`{"choices":[{"delta":{"content":"!"}, "finish_reason":"stop"}]}`,
	)

	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "test", strings.NewReader(body))
	}()

	chunks := drainChunks(t, ch)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 text chunks, got %d: %+v", len(chunks), chunks)
	}
	want := []string{"Hello ", "world", "!"}
	for i, c := range chunks {
		if c.Type != "text" {
			t.Errorf("chunk[%d].Type = %q, want text", i, c.Type)
		}
		if c.Content != want[i] {
			t.Errorf("chunk[%d].Content = %q, want %q", i, c.Content, want[i])
		}
	}
}

// TestParseOpenAISSEStream_ToolCallAcrossDeltas verifies the accumulator
// concatenates argument fragments and emits one tool_use chunk on [DONE].
func TestParseOpenAISSEStream_ToolCallAcrossDeltas(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_files","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\".\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "test", strings.NewReader(body))
	}()

	chunks := drainChunks(t, ch)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 tool_use chunk, got %d: %+v", len(chunks), chunks)
	}
	c := chunks[0]
	if c.Type != "tool_use" {
		t.Fatalf("chunk type = %q, want tool_use", c.Type)
	}
	if c.ToolName != "list_files" {
		t.Errorf("ToolName = %q, want list_files", c.ToolName)
	}
	if c.ToolUseID != "call_1" {
		t.Errorf("ToolUseID = %q, want call_1", c.ToolUseID)
	}
	if got, _ := c.ToolParams["path"].(string); got != "." {
		t.Errorf("ToolParams[path] = %v, want \".\"", c.ToolParams["path"])
	}
	if !strings.Contains(c.ToolInput, `"path":".`) {
		t.Errorf("ToolInput = %q, expected raw JSON with path", c.ToolInput)
	}
}

// TestParseOpenAISSEStream_MixedTextAndToolCall verifies a turn that emits
// some text before invoking a tool produces both chunk kinds in the right
// order.
func TestParseOpenAISSEStream_MixedTextAndToolCall(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"Looking it up..."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "test", strings.NewReader(body))
	}()
	chunks := drainChunks(t, ch)

	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (text then tool_use), got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Type != "text" || chunks[0].Content != "Looking it up..." {
		t.Errorf("chunk[0] = %+v, expected text 'Looking it up...'", chunks[0])
	}
	if chunks[1].Type != "tool_use" || chunks[1].ToolName != "search" {
		t.Errorf("chunk[1] = %+v, expected tool_use search", chunks[1])
	}
}

// TestParseOpenAISSEStream_ParallelToolCalls verifies two tool calls
// streamed at separate indexes both surface as distinct tool_use chunks
// in first-seen order.
func TestParseOpenAISSEStream_ParallelToolCalls(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"b","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "test", strings.NewReader(body))
	}()
	chunks := drainChunks(t, ch)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 tool_use chunks, got %d", len(chunks))
	}
	if chunks[0].ToolName != "a" || chunks[1].ToolName != "b" {
		t.Errorf("tool order = [%s, %s], want [a, b]", chunks[0].ToolName, chunks[1].ToolName)
	}
}

// TestParseOpenAISSEStream_MalformedToolArgs verifies an unparseable JSON
// arg surfaces as an error chunk identifying the tool, not a silent
// no-op.
func TestParseOpenAISSEStream_MalformedToolArgs(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"oops","arguments":"{not-json"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	chunks := drainChunks(t, ch)
	if len(chunks) != 1 || chunks[0].Type != "error" {
		t.Fatalf("expected one error chunk, got %+v", chunks)
	}
	if !strings.Contains(chunks[0].Content, `tool "oops"`) {
		t.Errorf("error did not name the tool: %q", chunks[0].Content)
	}
	if !strings.Contains(chunks[0].Content, "mlx-lm") {
		t.Errorf("error did not name the kind: %q", chunks[0].Content)
	}
}

// TestParseOpenAISSEStream_JSONInContentToolCallFallback verifies the
// Qwen-style tool-call dialect (entire content is a single
// {"name":...,"arguments":{...}} JSON object) surfaces as a tool_use chunk
// even when the server doesn't populate structured tool_calls deltas.
func TestParseOpenAISSEStream_JSONInContentToolCallFallback(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Lisbon\"}}"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)

	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	chunks := drainChunks(t, ch)
	// Expect: one text chunk (the streamed content) and one tool_use chunk
	// synthesized from it on flush.
	var sawToolUse bool
	for _, c := range chunks {
		if c.Type == "tool_use" {
			sawToolUse = true
			if c.ToolName != "get_weather" {
				t.Errorf("ToolName = %q, want get_weather", c.ToolName)
			}
			if got, _ := c.ToolParams["city"].(string); got != "Lisbon" {
				t.Errorf("ToolParams[city] = %v, want Lisbon", c.ToolParams["city"])
			}
		}
	}
	if !sawToolUse {
		t.Fatalf("fallback did not emit tool_use chunk: %+v", chunks)
	}
}

// TestParseOpenAISSEStream_JSONInContentFallback_OnlyWhenWholeContentIsJSON
// guards against false-positive fallbacks: prose that mentions a tool name
// must not be misread as an invocation.
func TestParseOpenAISSEStream_JSONInContentFallback_OnlyWhenWholeContentIsJSON(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"I would call get_weather but I won't right now."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	ch := make(chan agent.StreamChunk, 8)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	for _, c := range drainChunks(t, ch) {
		if c.Type == "tool_use" {
			t.Fatalf("prose mentioning a tool was misread as an invocation: %+v", c)
		}
	}
}

// TestParseOpenAISSEStream_QwenToolsTagFallback verifies the
// <tools>{...}</tools> dialect Qwen2.5-Coder uses surfaces as a tool_use
// chunk. This is what the live MLX-LM test caught: the model emits
//
//	<tools>
//	{"name": "echo_phrase", "arguments": {"phrase": "x"}}
//	</tools>
//	The function returned ...
//
// rather than bare JSON.
func TestParseOpenAISSEStream_QwenToolsTagFallback(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"<tools>\n{\"name\": \"echo_phrase\", \"arguments\": {\"phrase\": \"unified-steele\"}}\n</tools>\n"}}]}`,
		`{"choices":[{"delta":{"content":"\nThe function returned the phrase 'unified-steele'."}, "finish_reason":"stop"}]}`,
	)
	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	chunks := drainChunks(t, ch)
	var sawToolUse bool
	for _, c := range chunks {
		if c.Type == "tool_use" {
			sawToolUse = true
			if c.ToolName != "echo_phrase" {
				t.Errorf("ToolName = %q, want echo_phrase", c.ToolName)
			}
			if got, _ := c.ToolParams["phrase"].(string); got != "unified-steele" {
				t.Errorf("ToolParams[phrase] = %v, want unified-steele", c.ToolParams["phrase"])
			}
		}
	}
	if !sawToolUse {
		t.Fatalf("Qwen <tools> dialect did not emit tool_use; chunks: %+v", chunks)
	}
}

// TestParseOpenAISSEStream_QwenImEndFallback locks in the Qwen-style
// `{json}<|im_end|>` shape mlx_lm.server actually emits (no <tools>
// wrapper, just bare JSON followed by the chat-template terminator).
// This was the failure mode in screenshot bug "agent reply is the raw
// tool-call JSON" — the trailing `<|im_end|>` made the previous
// HasSuffix("}") check bail out, and the JSON got streamed back to
// the user verbatim.
func TestParseOpenAISSEStream_QwenImEndFallback(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"{\"name\": \"team_broadcast\", \"arguments\": {\"channel\": \"ceo__human\", \"content\": \"hi team\"}}<|im_end|>"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	ch := make(chan agent.StreamChunk, 8)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	var sawToolUse bool
	for _, c := range drainChunks(t, ch) {
		if c.Type == "tool_use" {
			sawToolUse = true
			if c.ToolName != "team_broadcast" {
				t.Errorf("ToolName = %q, want team_broadcast", c.ToolName)
			}
			ch, _ := c.ToolParams["channel"].(string)
			if ch != "ceo__human" {
				t.Errorf("ToolParams[channel] = %v, want ceo__human", c.ToolParams["channel"])
			}
		}
	}
	if !sawToolUse {
		t.Fatal("Qwen <|im_end|> dialect did not surface as tool_use — agent reply will be the raw JSON")
	}
}

// TestParseOpenAISSEStream_QwenMarkdownFenceFallback covers the
// REAL Qwen2.5-Coder dialect captured in the agent log: model wraps
// the tool-call JSON in a markdown code fence and adds the chat-
// template terminator. Caught a user-visible bug where the agent
// reply rendered as a JSON code block instead of executing the tool.
//
// Sample raw stream from the headless-codex-planner.log:
//
//	```json
//	{"name": "team_broadcast", "arguments": {...}}
//	```<|im_end|>
func TestParseOpenAISSEStream_QwenMarkdownFenceFallback(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"`+
			"```json\\n{\\\"name\\\": \\\"team_broadcast\\\", \\\"arguments\\\": "+
			"{\\\"channel\\\": \\\"human__planner\\\", \\\"content\\\": \\\"Hi!\\\"}}\\n"+
			"```<|im_end|>"+
			`"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	ch := make(chan agent.StreamChunk, 8)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	var sawToolUse bool
	for _, c := range drainChunks(t, ch) {
		if c.Type == "tool_use" {
			sawToolUse = true
			if c.ToolName != "team_broadcast" {
				t.Errorf("ToolName = %q, want team_broadcast", c.ToolName)
			}
		}
	}
	if !sawToolUse {
		t.Fatal("Markdown-fenced Qwen tool call did not surface as tool_use — agent reply will be the raw JSON code block")
	}
}

// TestStripChatTemplateTerminators covers the per-template-family
// suffix stripper directly. The helper underpins both the markdown-
// fence dialect and the bare-JSON dialect — a regression here lets a
// trailing chat-template marker break the HasSuffix("}") check
// downstream and the agent reply silently regresses to raw JSON.
func TestStripChatTemplateTerminators(t *testing.T) {
	cases := []struct{ in, want string }{
		// Each known terminator stripped individually.
		{`{"name":"x"}<|im_end|>`, `{"name":"x"}`},
		{`{"name":"x"}<|endoftext|>`, `{"name":"x"}`},
		{`{"name":"x"}<|end_of_text|>`, `{"name":"x"}`},
		{`{"name":"x"}<|eot_id|>`, `{"name":"x"}`},
		{`{"name":"x"}<|eom_id|>`, `{"name":"x"}`},
		{`{"name":"x"}</s>`, `{"name":"x"}`},
		// Trailing whitespace + terminator combos. Mixing actual
		// whitespace before the terminator MUST get stripped — the
		// loop alternates TrimRight(whitespace) and TrimSuffix
		// (terminator) so callers don't have to care about ordering.
		{"{\"name\":\"x\"}\n<|im_end|>", `{"name":"x"}`},
		{"{\"name\":\"x\"}<|im_end|> \t", `{"name":"x"}`},
		{"{\"name\":\"x\"}<|im_end|>\r\n", `{"name":"x"}`},
		// Multiple terminators chained (some templates double-emit).
		{"{\"name\":\"x\"}<|im_end|></s>", `{"name":"x"}`},
		// Plain content untouched.
		{"plain text", "plain text"},
		// Empty.
		{"", ""},
		// Terminator appearing in the middle stays put — only suffix
		// matches strip. Otherwise we'd corrupt content that mentions
		// the marker mid-sentence.
		{`prose <|im_end|> with trailing}`, `prose <|im_end|> with trailing}`},
	}
	for _, tc := range cases {
		got := stripChatTemplateTerminators(tc.in)
		if got != tc.want {
			t.Errorf("stripChatTemplateTerminators(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestExtractFirstBalancedToolCall covers the lenient-fallback dialect
// directly: a balanced {...} object at index 0, optionally followed by
// trailing whitespace / template markers / a brief summary. Conservative
// by design — anything that risks misreading prose-with-an-example as
// a tool invocation must return ok=false.
func TestExtractFirstBalancedToolCall(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantOK      bool
		wantToolID  string
		wantHasArgs bool
	}{
		{
			name:        "bare object",
			in:          `{"name":"do","arguments":{"a":1}}`,
			wantOK:      true,
			wantToolID:  "do",
			wantHasArgs: true,
		},
		{
			name:        "object plus brief trailing summary",
			in:          `{"name":"do","arguments":{}}` + "\n\nDone.",
			wantOK:      true,
			wantToolID:  "do",
			wantHasArgs: true,
		},
		{
			name:        "object plus very long trailing prose rejected",
			in:          `{"name":"do","arguments":{}}` + strings.Repeat(" trailing", 50),
			wantOK:      false,
			wantToolID:  "",
			wantHasArgs: false,
		},
		{
			name:       "prose-then-object rejected",
			in:         `Here's how: {"name":"do","arguments":{}}`,
			wantOK:     false,
			wantToolID: "",
		},
		{
			name:       "trailing brace in another { rejected",
			in:         `{"name":"a","arguments":{}}{"name":"b"}`,
			wantOK:     false,
			wantToolID: "",
		},
		{
			name:       "unbalanced braces rejected",
			in:         `{"name":"a","arguments":{}`,
			wantOK:     false,
			wantToolID: "",
		},
		{
			name:       "string with a literal brace inside doesn't confuse depth",
			in:         `{"name":"do","arguments":{"a":"x{}y"}}`,
			wantOK:     true,
			wantToolID: "do",
		},
		{
			name:       "escaped quote in string doesn't break parser",
			in:         `{"name":"do","arguments":{"a":"\""}}`,
			wantOK:     true,
			wantToolID: "do",
		},
		{
			name:       "missing arguments rejected",
			in:         `{"name":"do"}`,
			wantOK:     false,
			wantToolID: "",
		},
		{
			name:       "string arguments rejected",
			in:         `{"name":"do","arguments":"hello"}`,
			wantOK:     false,
			wantToolID: "",
		},
		{
			name:       "empty",
			in:         "",
			wantOK:     false,
			wantToolID: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, args, ok := extractFirstBalancedToolCall(tc.in)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (name=%q args=%q)", ok, tc.wantOK, name, args)
			}
			if name != tc.wantToolID {
				t.Errorf("name = %q, want %q", name, tc.wantToolID)
			}
			if tc.wantHasArgs && !strings.HasPrefix(args, "{") {
				t.Errorf("args = %q, want object-shape", args)
			}
		})
	}
}

// TestStripMarkdownCodeFence locks in the fence-stripping helper that
// underpins the dialect above, plus its degenerate cases.
func TestStripMarkdownCodeFence(t *testing.T) {
	cases := []struct{ in, want string }{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"```json\n{\"a\":1}", `{"a":1}`},          // missing trailing fence
		{"  ```json\n{\"a\":1}\n```  ", `{"a":1}`}, // leading/trailing whitespace
		{"plain text, no fence", "plain text, no fence"},
		{"```", ""}, // degenerate
	}
	for _, tc := range cases {
		got := stripMarkdownCodeFence(tc.in)
		if got != tc.want {
			t.Errorf("stripMarkdownCodeFence(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseOpenAISSEStream_BalancedJSONIgnoresProseSummary covers the
// case where the model emits the tool-call JSON followed by a brief
// "I'll send that now" trailer — common with smaller open models.
// The first balanced object should still be detected as a tool call.
func TestParseOpenAISSEStream_BalancedJSONIgnoresProseSummary(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"{\"name\": \"team_broadcast\", \"arguments\": {\"channel\": \"general\", \"content\": \"x\"}}\n\nDone."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	ch := make(chan agent.StreamChunk, 8)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	var sawToolUse bool
	for _, c := range drainChunks(t, ch) {
		if c.Type == "tool_use" {
			sawToolUse = true
		}
	}
	if !sawToolUse {
		t.Fatal("balanced JSON + brief trailer did not surface as tool_use")
	}
}

// TestParseOpenAISSEStream_BalancedJSONRejectsProsePrecedingJSON keeps
// the parser conservative: prose-then-JSON is treated as prose. We
// don't want a model that types "Here's an example: {…}" to trigger
// an actual tool invocation.
func TestParseOpenAISSEStream_BalancedJSONRejectsProsePrecedingJSON(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"Here is an example: {\"name\":\"do\",\"arguments\":{}}"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	ch := make(chan agent.StreamChunk, 8)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	for _, c := range drainChunks(t, ch) {
		if c.Type == "tool_use" {
			t.Fatalf("prose-then-JSON misread as invocation: %+v", c)
		}
	}
}

// TestParseOpenAISSEStream_QwenToolsTagFallback_OnlyOneBlock guards against
// a model that emits multiple <tools> blocks (sometimes happens when it's
// reasoning aloud). Conservative path: do not invoke either; surface as
// text only.
func TestParseOpenAISSEStream_QwenToolsTagFallback_OnlyOneBlock(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"thinking...\n<tools>{\"name\":\"a\",\"arguments\":{}}</tools>\nor maybe\n<tools>{\"name\":\"b\",\"arguments\":{}}</tools>"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	ch := make(chan agent.StreamChunk, 8)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	for _, c := range drainChunks(t, ch) {
		if c.Type == "tool_use" {
			t.Fatalf("ambiguous multi-<tools> content was misread as an invocation: %+v", c)
		}
	}
}

// TestParseOpenAISSEStream_StructuredToolCallsBeatFallback confirms that
// when the server DOES emit structured tool_calls, we use those and don't
// also synthesize a duplicate from any text content the model emitted.
func TestParseOpenAISSEStream_StructuredToolCallsBeatFallback(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"{\"name\": \"shadow\", \"arguments\": {}}"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"real_tool","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	ch := make(chan agent.StreamChunk, 8)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "mlx-lm", strings.NewReader(body))
	}()
	var toolUses []agent.StreamChunk
	for _, c := range drainChunks(t, ch) {
		if c.Type == "tool_use" {
			toolUses = append(toolUses, c)
		}
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected exactly 1 tool_use, got %d (%+v)", len(toolUses), toolUses)
	}
	if toolUses[0].ToolName != "real_tool" {
		t.Errorf("structured path was overridden by fallback: %q", toolUses[0].ToolName)
	}
}

// TestParseOpenAISSEStream_IgnoresEmptyDataFrames verifies that empty
// `data:\n\n` frames — which ollama and llama.cpp's server emit as
// keep-alive heartbeats under load — don't abort the stream. Earlier
// versions hit this mid-stream and turned every keep-alive into a fatal
// "unexpected end of JSON input" error chunk; the live MLX tests didn't
// catch it because mlx_lm.server doesn't emit them.
func TestParseOpenAISSEStream_IgnoresEmptyDataFrames(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data:\n\n" +
		"data: \n\n" + // also exercise the trailing-space variant
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	ch := make(chan agent.StreamChunk, 8)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "ollama", strings.NewReader(body))
	}()
	chunks := drainChunks(t, ch)
	for _, c := range chunks {
		if c.Type == "error" {
			t.Fatalf("empty data frame should not produce error chunk: %+v", c)
		}
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 text chunks (a, b), got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Content != "a" || chunks[1].Content != "b" {
		t.Errorf("ordered text wrong: %+v", chunks)
	}
}

// TestNormalizeOpenAICompatEndpoint_AppendsV1WhenMissing prevents the
// silent-404 footgun where a user pasting `http://127.0.0.1:8080` from
// mlx_lm.server's startup banner gets a confusing
// `HTTP 404 from http://127.0.0.1:8080/chat/completions` instead of a
// working request.
func TestNormalizeOpenAICompatEndpoint_AppendsV1WhenMissing(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/v1/chat/completions"},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080/v1/chat/completions"},
		{"http://127.0.0.1:8080/v1", "http://127.0.0.1:8080/v1/chat/completions"},
		{"http://127.0.0.1:8080/v1/", "http://127.0.0.1:8080/v1/chat/completions"},
		// Multi-tenant proxies sometimes nest /v1 under a path prefix.
		{"https://gateway.example.com/api/v1/proxy", "https://gateway.example.com/api/v1/proxy/chat/completions"},
		// Auth-via-query (rare but supported by some proxied gateways):
		// query string must round-trip after the path join.
		{"https://gw/v1?key=abc", "https://gw/v1/chat/completions?key=abc"},
		{"https://gw?key=abc", "https://gw/v1/chat/completions?key=abc"},
		{"https://gw/?key=abc", "https://gw/v1/chat/completions?key=abc"},
	}
	for _, tc := range cases {
		got := normalizeOpenAICompatEndpoint(tc.in)
		if got != tc.want {
			t.Errorf("normalizeOpenAICompatEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNewOpenAICompatStreamFnWithCtx_UnregisteredKindEmitsClearError
// prevents a future bug where someone calls the ctx-aware factory with
// a kind that wasn't registered via NewOpenAICompatStreamFn first.
// Without the guard, runOpenAICompatStream would build a relative URL
// and net/http would fail six layers down with a confusing message.
func TestNewOpenAICompatStreamFnWithCtx_UnregisteredKindEmitsClearError(t *testing.T) {
	fn := NewOpenAICompatStreamFnWithCtx(context.Background(), "definitely-not-registered")
	chunks := drainChunks(t, fn(nil, nil))
	if len(chunks) != 1 || chunks[0].Type != "error" {
		t.Fatalf("expected one error chunk, got %+v", chunks)
	}
	if !strings.Contains(chunks[0].Content, "definitely-not-registered") {
		t.Errorf("error did not name the kind: %q", chunks[0].Content)
	}
	if !strings.Contains(chunks[0].Content, "no registered defaults") {
		t.Errorf("error missing diagnostic hint: %q", chunks[0].Content)
	}
}

// TestOpenAICompatDialTimeout_EnvOverride documents the env knob users
// can flip when pointing the local-LLM provider at a remote box with
// flaky first-connection latency (Wi-Fi, TLS handshake, etc.).
func TestOpenAICompatDialTimeout_EnvOverride(t *testing.T) {
	t.Setenv("WUPHF_OPENAI_COMPAT_DIAL_TIMEOUT_SECONDS", "")
	if got := openAICompatDialTimeout(); got != 5*time.Second {
		t.Errorf("default = %v, want 5s", got)
	}
	t.Setenv("WUPHF_OPENAI_COMPAT_DIAL_TIMEOUT_SECONDS", "30")
	if got := openAICompatDialTimeout(); got != 30*time.Second {
		t.Errorf("override = %v, want 30s", got)
	}
	// Garbage env value must fall back to default rather than zero (which
	// would mean "no timeout" and reintroduce the original bug).
	t.Setenv("WUPHF_OPENAI_COMPAT_DIAL_TIMEOUT_SECONDS", "not-a-number")
	if got := openAICompatDialTimeout(); got != 5*time.Second {
		t.Errorf("garbage env = %v, want 5s fallback", got)
	}
	t.Setenv("WUPHF_OPENAI_COMPAT_DIAL_TIMEOUT_SECONDS", "0")
	if got := openAICompatDialTimeout(); got != 5*time.Second {
		t.Errorf("zero env = %v, want 5s fallback (zero would mean no timeout)", got)
	}
}

// TestParseOpenAISSEStream_IgnoresCommentLines verifies SSE comment lines
// (servers sometimes send keep-alive pings as `: ping`) don't break parsing.
func TestParseOpenAISSEStream_IgnoresCommentLines(t *testing.T) {
	body := ": keep-alive\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		": another\n\n" +
		"data: [DONE]\n\n"
	ch := make(chan agent.StreamChunk, 4)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "test", strings.NewReader(body))
	}()
	chunks := drainChunks(t, ch)
	if len(chunks) != 1 || chunks[0].Type != "text" || chunks[0].Content != "ok" {
		t.Fatalf("expected single text 'ok', got %+v", chunks)
	}
}

// TestOpenAICompatStreamFn_HTTP500SurfacesAsError exercises the full HTTP
// path: a 500 from the server should produce one error chunk including the
// kind, status, and response body excerpt.
func TestOpenAICompatStreamFn_HTTP500SurfacesAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"out of memory"}`)
	}))
	defer srv.Close()

	t.Setenv("WUPHF_MLX_LM_BASE_URL", srv.URL+"/v1")

	factory := NewOpenAICompatStreamFn(KindMLXLM, "http://unused", "default-model")
	stream := factory("agent-1")
	chunks := drainChunks(t, stream(
		[]agent.Message{{Role: "user", Content: "hi"}},
		nil,
	))

	if len(chunks) != 1 || chunks[0].Type != "error" {
		t.Fatalf("expected one error chunk, got %+v", chunks)
	}
	body := chunks[0].Content
	if !strings.Contains(body, "HTTP 500") {
		t.Errorf("error did not mention HTTP 500: %q", body)
	}
	if !strings.Contains(body, "out of memory") {
		t.Errorf("error did not include server body: %q", body)
	}
	if !strings.Contains(body, "mlx-lm") {
		t.Errorf("error did not name the kind: %q", body)
	}
}

// TestOpenAICompatStreamFn_TextStream_EndToEnd exercises the full HTTP path
// with a streamed 200 response to make sure the SSE plumbing matches what
// httptest's ResponseWriter produces (Transfer-Encoding handling, flushing,
// reader bufferng, etc.).
func TestOpenAICompatStreamFn_TextStream_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, ev := range []string{
			`{"choices":[{"delta":{"content":"al"}}]}`,
			`{"choices":[{"delta":{"content":"pha"}, "finish_reason":"stop"}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	t.Setenv("WUPHF_OLLAMA_BASE_URL", srv.URL+"/v1")

	factory := NewOpenAICompatStreamFn(KindOllama, "http://unused", "default-model")
	chunks := drainChunks(t, factory("a")(
		[]agent.Message{{Role: "user", Content: "say alpha"}},
		nil,
	))

	if len(chunks) != 2 {
		t.Fatalf("expected 2 text chunks, got %d (%+v)", len(chunks), chunks)
	}
	got := chunks[0].Content + chunks[1].Content
	if got != "alpha" {
		t.Errorf("concatenated text = %q, want \"alpha\"", got)
	}
}

// TestOpenAICompatStreamFn_RequestBodyShape verifies the outgoing JSON
// payload uses the OpenAI shape (model, messages, stream:true, tools[]).
func TestOpenAICompatStreamFn_RequestBodyShape(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		got = string(buf)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	t.Setenv("WUPHF_EXO_BASE_URL", srv.URL+"/v1")
	t.Setenv("WUPHF_EXO_MODEL", "test-model")

	factory := NewOpenAICompatStreamFn(KindExo, "http://unused", "unused-default")
	stream := factory("a")
	for range stream(
		[]agent.Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "ping"}},
		[]agent.AgentTool{{Name: "t1", Description: "d", Schema: map[string]any{"type": "object"}}},
	) {
	}

	for _, want := range []string{
		`"model":"test-model"`,
		`"stream":true`,
		`"tool_choice":"auto"`,
		`"name":"t1"`,
		`"role":"system"`,
		`"role":"user"`,
		`"stream_options":{"include_usage":true}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("request body missing %q\nbody: %s", want, got)
		}
	}
}

func TestOpenAICompatStreamFn_HermesHeadersWhenAuthenticated(t *testing.T) {
	var gotAuth string
	var gotSessionID string
	var gotSessionKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSessionID = r.Header.Get("X-Hermes-Session-Id")
		gotSessionKey = r.Header.Get("X-Hermes-Session-Key")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	t.Setenv("WUPHF_HERMES_AGENT_BASE_URL", srv.URL+"/v1")
	t.Setenv("WUPHF_HERMES_AGENT_API_KEY", "test-hermes-key")

	factory := NewOpenAICompatStreamFn(KindHermesAgent, "http://unused", "hermes-agent")
	for range factory("Agent One!")([]agent.Message{{Role: "user", Content: "ping"}}, nil) {
	}

	if gotAuth != "Bearer test-hermes-key" {
		t.Fatalf("Authorization = %q, want bearer key", gotAuth)
	}
	if gotSessionID != "wuphf-agent-one" {
		t.Fatalf("X-Hermes-Session-Id = %q, want wuphf-agent-one", gotSessionID)
	}
	if gotSessionKey != gotSessionID {
		t.Fatalf("X-Hermes-Session-Key = %q, want %q", gotSessionKey, gotSessionID)
	}
}

func TestOpenAICompatStreamFn_HermesOmitsSessionHeadersWithoutAuth(t *testing.T) {
	var gotAuth string
	var gotSessionID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSessionID = r.Header.Get("X-Hermes-Session-Id")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	t.Setenv("WUPHF_HERMES_AGENT_BASE_URL", srv.URL+"/v1")
	t.Setenv("WUPHF_HERMES_AGENT_API_KEY", "")
	t.Setenv("API_SERVER_KEY", "")

	factory := NewOpenAICompatStreamFn(KindHermesAgent, "http://unused", "hermes-agent")
	for range factory("agent-one")([]agent.Message{{Role: "user", Content: "ping"}}, nil) {
	}

	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
	if gotSessionID != "" {
		t.Fatalf("X-Hermes-Session-Id = %q, want empty without auth", gotSessionID)
	}
}

func TestOpenAICompatStreamFn_OpenclawHTTPUsesGatewayToken(t *testing.T) {
	var gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	t.Setenv("WUPHF_OPENCLAW_HTTP_BASE_URL", srv.URL+"/v1")
	t.Setenv("WUPHF_OPENCLAW_HTTP_API_KEY", "")
	t.Setenv("OPENCLAW_GATEWAY_TOKEN", "gateway-token")
	t.Setenv("WUPHF_OPENCLAW_TOKEN", "")

	factory := NewOpenAICompatStreamFn(KindOpenclawHTTP, "http://unused", "openclaw/default")
	for range factory("agent-one")([]agent.Message{{Role: "user", Content: "ping"}}, nil) {
	}

	if gotAuth != "Bearer gateway-token" {
		t.Fatalf("Authorization = %q, want OpenClaw gateway bearer token", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"openclaw/default"`) {
		t.Fatalf("request model = %s, want openclaw/default", gotBody)
	}
	if !strings.Contains(gotBody, `"user":"wuphf-agent-one"`) {
		t.Fatalf("request user = %s, want stable WUPHF agent user", gotBody)
	}
}

func TestOpenAICompatStreamFn_AtlasCloudUsesEnvAliasAndDefaults(t *testing.T) {
	var gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	t.Setenv("WUPHF_ATLASCLOUD_BASE_URL", srv.URL+"/v1")
	t.Setenv("WUPHF_ATLASCLOUD_MODEL", "")
	t.Setenv("WUPHF_ATLASCLOUD_API_KEY", "")
	t.Setenv("ATLASCLOUD_API_KEY", "atlas-test-key")
	t.Setenv("ATLAS_CLOUD_API_KEY", "")

	factory := NewOpenAICompatStreamFn(KindAtlasCloud, "http://unused", defaultAtlasCloudModel)
	for range factory("agent-one")([]agent.Message{{Role: "user", Content: "ping"}}, nil) {
	}

	if gotAuth != "Bearer atlas-test-key" {
		t.Fatalf("Authorization = %q, want Atlas Cloud bearer key", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"`+defaultAtlasCloudModel+`"`) {
		t.Fatalf("request model = %s, want %s", gotBody, defaultAtlasCloudModel)
	}
}

// TestParseOpenAISSEStream_TrailingUsageFrame covers the canonical pattern
// from `stream_options.include_usage`: a final SSE frame with empty choices
// and a populated `usage` block. It must produce exactly one trailing
// usage chunk carrying the prompt/completion token counts.
func TestParseOpenAISSEStream_TrailingUsageFrame(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":7,"total_tokens":49}}`,
	)

	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "test", strings.NewReader(body))
	}()

	chunks := drainChunks(t, ch)
	var usage *agent.StreamChunk
	for i := range chunks {
		if chunks[i].Type == "usage" {
			usage = &chunks[i]
		}
	}
	if usage == nil {
		t.Fatalf("expected a usage chunk, got %+v", chunks)
	}
	if usage.InputTokens != 42 || usage.OutputTokens != 7 {
		t.Errorf("usage chunk = {input:%d output:%d}, want {42, 7}", usage.InputTokens, usage.OutputTokens)
	}
	// The usage chunk must come AFTER the text chunks so consumers that
	// stream-render text don't have to special-case ordering.
	lastTextIdx := -1
	usageIdx := -1
	for i, c := range chunks {
		switch c.Type {
		case "text":
			lastTextIdx = i
		case "usage":
			usageIdx = i
		}
	}
	if usageIdx <= lastTextIdx {
		t.Errorf("usage chunk emitted at index %d, expected after last text at %d", usageIdx, lastTextIdx)
	}
}

// TestParseOpenAISSEStream_NoUsageFrameStaysSilent verifies that when the
// server doesn't emit a usage frame (e.g. older mlx_lm.server, servers that
// don't implement stream_options.include_usage), the parser does NOT
// fabricate a zero-valued usage chunk. The headless runner relies on
// "no usage chunk → don't call RecordAgentUsage" to avoid recording empty
// rows.
func TestParseOpenAISSEStream_NoUsageFrameStaysSilent(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)

	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "test", strings.NewReader(body))
	}()

	for _, c := range drainChunks(t, ch) {
		if c.Type == "usage" {
			t.Fatalf("expected no usage chunk, got %+v", c)
		}
	}
}

// TestParseOpenAISSEStream_AllZeroUsageDoesNotLatch covers the latch
// invariant separately from the no-frame-at-all case above. A frame
// with `prompt_tokens=0, completion_tokens=0` MUST NOT fabricate a
// usage chunk, otherwise a server that emits a degenerate empty
// usage frame on errored turns would burn a Requests bump in the
// broker without any real numbers attached. (Kept as its own test
// so a future change to the latch policy can't silently regress this
// edge by passing the broader no-usage-server test.)
func TestParseOpenAISSEStream_AllZeroUsageDoesNotLatch(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
	)

	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "test", strings.NewReader(body))
	}()

	for _, c := range drainChunks(t, ch) {
		if c.Type == "usage" {
			t.Fatalf("zero-valued usage frame must not produce a usage chunk, got %+v", c)
		}
	}
}

// TestParseOpenAISSEStream_LaterPartialUsageDoesNotClobber covers the
// monotonic-latch invariant: once we've seen a frame with real numbers
// (e.g. prompt=42, completion=7), a later frame that's strictly smaller
// (e.g. prompt=42, completion=0 — observed pattern from speculative-
// decoding rejects on mlx_lm.server) must not overwrite the captured
// count. Without this guard a turn's completion tokens would be silently
// lost, surfacing as zero in the user's usage panel.
func TestParseOpenAISSEStream_LaterPartialUsageDoesNotClobber(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"Hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":7,"total_tokens":49}}`,
		`{"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":0,"total_tokens":42}}`,
	)

	ch := make(chan agent.StreamChunk, 16)
	go func() {
		defer close(ch)
		parseOpenAISSEStream(ch, "test", strings.NewReader(body))
	}()

	var usage *agent.StreamChunk
	for _, c := range drainChunks(t, ch) {
		c := c
		if c.Type == "usage" {
			usage = &c
		}
	}
	if usage == nil {
		t.Fatalf("expected a usage chunk")
	}
	if usage.InputTokens != 42 || usage.OutputTokens != 7 {
		t.Errorf("usage = {input:%d output:%d}, want {42, 7} (later partial frame must not clobber)", usage.InputTokens, usage.OutputTokens)
	}
}
