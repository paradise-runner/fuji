package modelrt_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fuji/pkg/config"
	"fuji/pkg/messages"
	"fuji/pkg/modelrt"

	// Registers the anthropic/openai transports via init().
	_ "fuji/pkg/modelrt/transport"
)

func newRuntime(t *testing.T, provider, baseURL, key string) *modelrt.Runtime {
	cfg := config.Config{
		Provider:         provider,
		BaseURL:          baseURL,
		ModelID:          "test-model",
		CompactionWindow: 200000,
		ThinkingLevel:    "medium",
	}
	r := modelrt.New(cfg, func(k string) string { return "" }, map[string]string{provider: key})
	return r
}

const anthropicTape = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicTapeReplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if !strings.Contains(r.Header.Get("anthropic-version"), "2023") {
			t.Errorf("anthropic-version missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicTape)
	}))
	defer srv.Close()

	r := newRuntime(t, "anthropic", srv.URL, "test-key")
	model, _ := r.Model("anthropic", "claude-sonnet-4-5")
	ctx := modelrt.Context{
		SystemPrompt: "sys",
		Messages: []messages.AgentMessage{
			messages.UserMessage(messages.Content{Text: "hi"}, 1),
		},
	}
	stream := r.Stream(model, context.Background(), ctx, modelrt.StreamOptions{ThinkingLevel: "off"})

	var events []modelrt.EventType
	var text strings.Builder
	var final *messages.AgentMessage
	for ev := range stream.Events() {
		events = append(events, ev.Type)
		if ev.Type == modelrt.EvTextDelta {
			text.WriteString(ev.Delta)
		}
		if ev.Type == modelrt.EvStop || ev.Type == modelrt.EvError || ev.Type == modelrt.EvAborted {
			final = ev.Message
		}
	}
	// Expected: text_start, text_delta x2, text_end, stop.
	want := []modelrt.EventType{modelrt.EvTextStart, modelrt.EvTextDelta, modelrt.EvTextDelta, modelrt.EvTextEnd, modelrt.EvStop}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if text.String() != "Hello world" {
		t.Errorf("text = %q", text.String())
	}
	if final == nil {
		t.Fatal("no final message")
	}
	if final.StopReason != messages.StopStop {
		t.Errorf("stopReason = %q", final.StopReason)
	}
	if final.Content.TextOf() != "Hello world" {
		t.Errorf("content = %q", final.Content.TextOf())
	}
	if final.API != "anthropic" || final.Provider != "anthropic" || final.Model != "claude-sonnet-4-5" {
		t.Errorf("identity = %s/%s/%s", final.API, final.Provider, final.Model)
	}
}

const anthropicToolTape = `event: message_start
data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","content":[],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"file.txt\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicToolCallReplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicToolTape)
	}))
	defer srv.Close()

	r := newRuntime(t, "anthropic", srv.URL, "key")
	model, _ := r.Model("anthropic", "claude-sonnet-4-5")
	stream := r.Stream(model, context.Background(), modelrt.Context{}, modelrt.StreamOptions{})
	final := stream.Collect(nil)
	if final == nil {
		t.Fatal("no final message")
	}
	if final.StopReason != messages.StopToolUse {
		t.Errorf("stopReason = %q", final.StopReason)
	}
	tcs := final.AssistantToolCalls()
	if len(tcs) != 1 {
		t.Fatalf("tool calls = %d", len(tcs))
	}
	if tcs[0].Name != "read" || tcs[0].ID != "toolu_01" {
		t.Errorf("tool call = %+v", tcs[0])
	}
	if tcs[0].Arguments["path"] != "file.txt" {
		t.Errorf("args = %v", tcs[0].Arguments)
	}
}

func TestAuthFailureEncodedInStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()

	r := newRuntime(t, "anthropic", srv.URL, "bad-key")
	model, _ := r.Model("anthropic", "claude-sonnet-4-5")
	stream := r.Stream(model, context.Background(), modelrt.Context{}, modelrt.StreamOptions{})

	var final *messages.AgentMessage
	var sawError bool
	for ev := range stream.Events() {
		if ev.Type == modelrt.EvError {
			sawError = true
			final = ev.Message
		}
	}
	if !sawError {
		t.Fatal("expected error event")
	}
	if final == nil || final.StopReason != messages.StopError {
		t.Fatalf("final = %+v", final)
	}
	if !strings.Contains(final.ErrorMessage, "authentication failed") {
		t.Errorf("error message = %q", final.ErrorMessage)
	}
}

func TestMidStreamDropEncoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":{}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}
`) // no message_stop — connection drops
	}))
	defer srv.Close()

	r := newRuntime(t, "anthropic", srv.URL, "key")
	model, _ := r.Model("anthropic", "claude-sonnet-4-5")
	stream := r.Stream(model, context.Background(), modelrt.Context{}, modelrt.StreamOptions{})
	var final *messages.AgentMessage
	for ev := range stream.Events() {
		if ev.Type == modelrt.EvError || ev.Type == modelrt.EvAborted {
			final = ev.Message
		}
	}
	if final == nil || final.StopReason != messages.StopError {
		t.Fatalf("expected error final, got %+v", final)
	}
}

func TestTimeoutEncodedAsAbortedOrError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // hang until client disconnects
	}))
	defer srv.Close()

	r := newRuntime(t, "anthropic", srv.URL, "key")
	model, _ := r.Model("anthropic", "claude-sonnet-4-5")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	stream := r.Stream(model, ctx, modelrt.Context{}, modelrt.StreamOptions{})

	var final *messages.AgentMessage
	var terminalType modelrt.EventType
	for ev := range stream.Events() {
		if ev.Type == modelrt.EvError || ev.Type == modelrt.EvAborted {
			terminalType = ev.Type
			final = ev.Message
		}
	}
	if final == nil {
		t.Fatal("no terminal event")
	}
	if terminalType == modelrt.EvAborted {
		if final.StopReason != messages.StopAborted {
			t.Errorf("aborted stopReason = %q", final.StopReason)
		}
	} else if final.StopReason != messages.StopError {
		t.Errorf("error stopReason = %q", final.StopReason)
	}
}

func TestOpenAITapeReplay(t *testing.T) {
	tape := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"bash","arguments":"{\"command\":"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":9,"total_tokens":51,"completion_tokens_details":{"reasoning_tokens":4}}}

data: [DONE]

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, tape)
	}))
	defer srv.Close()

	r := newRuntime(t, "deepseek", srv.URL+"/v1", "key")
	model, ok := r.Model("deepseek", "test-model")
	if !ok {
		t.Fatal("model not found")
	}
	stream := r.Stream(model, context.Background(), modelrt.Context{}, modelrt.StreamOptions{ThinkingLevel: "medium"})
	var events []modelrt.EventType
	var final *messages.AgentMessage
	for ev := range stream.Events() {
		events = append(events, ev.Type)
		if ev.Type == modelrt.EvStop || ev.Type == modelrt.EvError || ev.Type == modelrt.EvAborted {
			final = ev.Message
		}
	}
	if final == nil {
		t.Fatal("no final message")
	}
	if final.StopReason != messages.StopToolUse {
		t.Errorf("stopReason = %q", final.StopReason)
	}
	if textOnly(final.Content) != "Hello" {
		t.Errorf("text = %q", textOnly(final.Content))
	}
	tcs := final.AssistantToolCalls()
	if len(tcs) != 1 || tcs[0].Name != "bash" || tcs[0].Arguments["command"] != "ls" {
		t.Errorf("tool calls = %+v", tcs)
	}
	if final.Usage == nil || final.Usage.Input != 42 || final.Usage.Reasoning == nil || *final.Usage.Reasoning != 4 {
		t.Errorf("usage = %+v", final.Usage)
	}
	// Thinking block present from reasoning_content.
	var sawThinking bool
	for _, blk := range final.Content.Blocks {
		if blk.BlockType() == messages.ContentThinking {
			sawThinking = true
		}
	}
	if !sawThinking {
		t.Error("missing thinking block")
	}
	_ = events
}

// TestOpenAICloseWithoutDone verifies the transport treats a provider that
// closes the SSE stream after a finish_reason chunk (OpenRouter free tier)
// as a normal stop instead of an error.
func TestOpenAICloseWithoutDone(t *testing.T) {
	tape := `data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":"4"},"finish_reason":null}]}

data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

` // no [DONE]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, tape)
	}))
	defer srv.Close()

	r := newRuntime(t, "openai", srv.URL+"/v1", "key")
	model, _ := r.Model("openai", "gpt-5")
	stream := r.Stream(model, context.Background(), modelrt.Context{}, modelrt.StreamOptions{})
	var final *messages.AgentMessage
	for ev := range stream.Events() {
		if ev.Type == modelrt.EvStop {
			final = ev.Message
		}
	}
	if final == nil {
		t.Fatal("expected clean EvStop")
	}
	if final.StopReason != messages.StopStop {
		t.Errorf("stopReason = %q", final.StopReason)
	}
	if textOnly(final.Content) != "4" {
		t.Errorf("text = %q", textOnly(final.Content))
	}
}

func TestUnknownProvider(t *testing.T) {
	cfg := config.Config{Provider: "nope"}
	r := modelrt.New(cfg, func(k string) string { return "" }, nil)
	_, ok := r.Model("nope", "x")
	if ok {
		t.Fatal("should not find model")
	}
	if err := r.CheckAuth("nope"); err == nil {
		t.Fatal("expected auth error for unknown provider")
	}
}

// TestModelStrictLookupNoFallback guards D14: an explicit model id must not
// silently resolve to the provider's first catalog model.
func TestModelStrictLookupNoFallback(t *testing.T) {
	cfg := config.Config{Provider: "openai"}
	r := modelrt.New(cfg, func(k string) string { return "" }, nil)

	// An id that is not in the builtin catalog must NOT fall back to gpt-5.
	if m, ok := r.Model("openai", "meta-llama/llama-3.1-8b-instruct"); ok {
		t.Fatalf("unknown model id resolved to %q; expected not-found", m.ID)
	}

	// No id requested: provider default (first catalog model) is fine.
	m, ok := r.Model("openai", "")
	if !ok || m.ID != "gpt-5" {
		t.Fatalf("default model = %q, ok=%v; want gpt-5", m.ID, ok)
	}
}

// TestModelAddedToCatalog verifies the registerBuiltins merge appends an
// explicit custom model id to the provider catalog so lookup succeeds.
func TestModelAddedToCatalog(t *testing.T) {
	cfg := config.Config{
		Provider: "openai",
		BaseURL:  "https://openrouter.ai/api/v1",
		ModelID:  "meta-llama/llama-3.1-8b-instruct",
	}
	r := modelrt.New(cfg, func(k string) string { return "" }, nil)
	m, ok := r.Model("openai", "meta-llama/llama-3.1-8b-instruct")
	if !ok {
		t.Fatal("explicit model id should resolve after catalog merge")
	}
	if m.ID != "meta-llama/llama-3.1-8b-instruct" {
		t.Fatalf("model id = %q", m.ID)
	}
}

func TestMissingKey(t *testing.T) {
	cfg := config.Config{Provider: "anthropic"}
	r := modelrt.New(cfg, func(k string) string { return "" }, nil)
	model, _ := r.Model("anthropic", "claude-sonnet-4-5")
	stream := r.Stream(model, context.Background(), modelrt.Context{}, modelrt.StreamOptions{})
	var sawErr bool
	for ev := range stream.Events() {
		if ev.Type == modelrt.EvError {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected error event for missing key")
	}
}

func TestConvertToLLM(t *testing.T) {
	msgs := []messages.AgentMessage{
		messages.UserMessage(messages.Content{Text: "hi"}, 1),
		{Role: messages.RoleBashExecution, Command: "ls", Output: "a.txt", ExitCode: intPtr(0), Timestamp: 2},
		{Role: messages.RoleCompactionSummary, Summary: "prev", TokensBefore: 10, Timestamp: 3},
	}
	out := modelrt.ConvertToLLM(msgs)
	if len(out) != 3 {
		t.Fatalf("len = %d", len(out))
	}
	if out[1].Role != messages.RoleUser || !strings.Contains(out[1].Content.TextOf(), "Ran `ls`") {
		t.Errorf("bash conversion = %+v", out[1])
	}
	if !strings.Contains(out[2].Content.TextOf(), "compacted into the following summary") {
		t.Errorf("compaction conversion = %q", out[2].Content.TextOf())
	}
	// excludeFromContext drops bash messages.
	msgs[1].ExcludeFromContext = true
	if out := modelrt.ConvertToLLM(msgs); len(out) != 2 {
		t.Errorf("excluded not dropped: %d", len(out))
	}
}

func textOnly(c messages.Content) string {
	var sb strings.Builder
	for _, blk := range c.Blocks {
		if t, ok := blk.(messages.TextContent); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

func intPtr(i int) *int { return &i }
