package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"fuji/pkg/messages"
	"fuji/pkg/modelrt"
	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// fakeStreamFn builds a StreamFn from a scripted sequence of responses.
// Each response is an assistant message; the fake emits text deltas then the
// terminal event.
type fakeStreamFn struct {
	mu        sync.Mutex
	responses []messages.AssistantMessage
	index     int
	abortOn   int // abort (close without terminal) at this response index; -1 = never
}

func newFake(responses ...messages.AssistantMessage) *fakeStreamFn {
	return &fakeStreamFn{responses: responses, abortOn: -1}
}

func (f *fakeStreamFn) make(model modelrt.Model, ctx context.Context, c modelrt.Context, opts modelrt.StreamOptions) *modelrt.Stream {
	s := modelrt.NewStream()
	f.mu.Lock()
	if f.index >= len(f.responses) {
		f.mu.Unlock()
		go func() {
			defer s.Close()
			s.Send(modelrt.StreamEvent{Type: modelrt.EvError, Message: errorMsg(model, "no more responses")})
		}()
		return s
	}
	resp := f.responses[f.index]
	f.index++
	if f.abortOn >= 0 && f.index-1 >= f.abortOn {
		f.mu.Unlock()
		go func() {
			defer s.Close()
			// abort mid-stream: send a delta then close without terminal event
			s.Send(modelrt.StreamEvent{Type: modelrt.EvTextDelta, Message: partialOf(model, "partial...")})
			time.Sleep(5 * time.Millisecond)
		}()
		return s
	}
	f.mu.Unlock()
	go func() {
		defer s.Close()
		// Build a partial snapshot that accumulates content blocks like the
		// real transports do.
		var blocks []messages.ContentBlock
		for _, blk := range resp.Content {
			switch v := blk.(type) {
			case messages.TextContent:
				blocks = append(blocks, messages.TextContent{Type: messages.ContentText, Text: v.Text})
				s.Send(modelrt.StreamEvent{Type: modelrt.EvTextDelta, Message: snapshotOf(model, blocks)})
			case messages.ThinkingContent:
				blocks = append(blocks, messages.ThinkingContent{Type: messages.ContentThinking, Thinking: v.Thinking})
				s.Send(modelrt.StreamEvent{Type: modelrt.EvThinkingDelta, Message: snapshotOf(model, blocks)})
			case messages.ToolCall:
				blocks = append(blocks, v)
				tc := v
				s.Send(modelrt.StreamEvent{Type: modelrt.EvToolCallDelta, Message: snapshotOf(model, blocks), ToolCall: &tc})
			}
		}
		m := messages.AgentMessage{}
		v := m.WithAssistant(resp, 1)
		s.Send(modelrt.StreamEvent{Type: modelrt.EvStop, Message: &v})
	}()
	return s
}

func partialOf(model modelrt.Model, text string) *messages.AgentMessage {
	return snapshotOf(model, []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: text}})
}

func snapshotOf(model modelrt.Model, blocks []messages.ContentBlock) *messages.AgentMessage {
	m := messages.AgentMessage{}
	am := messages.AssistantMessage{
		API: string(model.API), Provider: model.ProviderID, Model: model.ID,
		Content: blocks,
		Usage:   messages.Usage{Cost: messages.Cost{}},
	}
	v := m.WithAssistant(am, 1)
	return &v
}

func errorMsg(model modelrt.Model, err string) *messages.AgentMessage {
	m := messages.AgentMessage{}
	am := messages.AssistantMessage{
		API: string(model.API), Provider: model.ProviderID, Model: model.ID,
		Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopError, ErrorMessage: err,
	}
	v := m.WithAssistant(am, 1)
	return &v
}

func testModel() modelrt.Model {
	return modelrt.Model{ID: "test", ProviderID: "fake", API: modelrt.APIAnthropic, ThinkingLevels: []string{"off", "low", "medium", "high"}}
}

func streamFnOf(f *fakeStreamFn) StreamFn {
	return f.make
}

// runEventLoop runs the loop capturing events.
func runEventLoop(t *testing.T, prompts []messages.AgentMessage, cfg Config) []Event {
	t.Helper()
	var events []Event
	Run(prompts, Context{}, cfg, func(e Event) {
		events = append(events, e)
	})
	return events
}

func eventTypes(events []Event) []EventType {
	out := make([]EventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func TestAttributionHeadersThreaded(t *testing.T) {
	f := newFake(messages.AssistantMessage{
		API: "anthropic", Provider: "fake", Model: "test",
		Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "hello"}},
		Usage:      messages.Usage{Cost: messages.Cost{}},
		StopReason: messages.StopStop,
	})
	var gotHeaders map[string]string
	fn := streamFnOf(f)
	wrapper := func(model modelrt.Model, ctx context.Context, c modelrt.Context, opts modelrt.StreamOptions) *modelrt.Stream {
		gotHeaders = opts.Headers
		return fn(model, ctx, c, opts)
	}
	cfg := Config{
		Model:         testModel(),
		StreamFn:      wrapper,
		AppURL:        "https://myapp.com",
		AppTitle:      "My AI Assistant",
		AppCategories: "cli-agent,cloud-agent",
	}
	runEventLoop(t, []messages.AgentMessage{messages.UserMessage(messages.Content{Text: "hi"}, 1)}, cfg)
	if gotHeaders == nil {
		t.Fatal("headers not set")
	}
	if gotHeaders["HTTP-Referer"] != "https://myapp.com" {
		t.Errorf("HTTP-Referer = %q", gotHeaders["HTTP-Referer"])
	}
	if gotHeaders["X-OpenRouter-Title"] != "My AI Assistant" {
		t.Errorf("title = %q", gotHeaders["X-OpenRouter-Title"])
	}
	if gotHeaders["X-OpenRouter-Categories"] != "cli-agent,cloud-agent" {
		t.Errorf("categories = %q", gotHeaders["X-OpenRouter-Categories"])
	}
}

// TestSingleTurn: model replies with plain text (stop).
func TestSingleTurn(t *testing.T) {
	f := newFake(messages.AssistantMessage{
		API: "anthropic", Provider: "fake", Model: "test",
		Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "hello there"}},
		Usage:      messages.Usage{Input: 1, Output: 1, TotalTokens: 2, Cost: messages.Cost{}},
		StopReason: messages.StopStop,
	})
	cfg := Config{Model: testModel(), StreamFn: streamFnOf(f)}
	events := runEventLoop(t, []messages.AgentMessage{messages.UserMessage(messages.Content{Text: "hi"}, 1)}, cfg)

	want := []EventType{EvAgentStart, EvTurnStart, EvMessageStart, EvMessageEnd, EvMessageStart, EvMessageEnd, EvTurnEnd, EvAgentEnd}
	if !sameTypes(eventTypes(events), want) {
		t.Fatalf("event types = %v, want %v", eventTypes(events), want)
	}
	// agent_end carries the new messages (prompt + assistant).
	last := events[len(events)-1]
	if len(last.Messages) != 2 {
		t.Fatalf("newMessages = %d", len(last.Messages))
	}
	if last.Messages[1].Content.TextOf() != "hello there" {
		t.Errorf("assistant text = %q", last.Messages[1].Content.TextOf())
	}
	// The assistant message was pushed into context before tools.
	// turn_end carries the message + empty tool results.
	for _, e := range events {
		if e.Type == EvTurnEnd && e.Message != nil && e.Message.StopReason != messages.StopStop {
			t.Errorf("turn_end message = %+v", e.Message)
		}
	}
}

// TestMultiTurnToolLoop: model calls a tool; tool executes; model then stops.
func TestMultiTurnToolLoop(t *testing.T) {
	readTool := tools.New("read", "Read", "read a file",
		mustSchemaRaw(schema.Object([]string{"path"}, map[string]schema.Schema{"path": schema.Str("file")})),
		func(callID string, params json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			var args struct{ Path string }
			_ = json.Unmarshal(params, &args)
			return tools.TextResult("contents of " + args.Path), nil
		})

	f := newFake(
		messages.AssistantMessage{
			API: "anthropic", Provider: "fake", Model: "test",
			Content: []messages.ContentBlock{
				messages.TextContent{Type: messages.ContentText, Text: "let me read"},
				messages.ToolCall{Type: messages.ContentToolCall, ID: "call_1", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
			},
			Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopToolUse,
		},
		messages.AssistantMessage{
			API: "anthropic", Provider: "fake", Model: "test",
			Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "done now"}},
			Usage:      messages.Usage{Cost: messages.Cost{}},
			StopReason: messages.StopStop,
		},
	)
	cfg := Config{
		Model:    testModel(),
		StreamFn: streamFnOf(f),
	}
	ctx := Context{Tools: []tools.Definition{readTool}}
	var events []Event
	Run([]messages.AgentMessage{messages.UserMessage(messages.Content{Text: "go"}, 1)}, ctx, cfg, func(e Event) { events = append(events, e) })

	types := eventTypes(events)
	// Expect two turns: tool_use turn then final turn.
	want := []EventType{
		EvAgentStart, EvTurnStart,
		EvMessageStart, EvMessageEnd, // prompt
		EvMessageStart, EvMessageUpdate, EvMessageEnd, // assistant (text delta + toolcall delta)
		EvToolExecutionStart, EvToolExecutionEnd,
		EvMessageStart, EvMessageEnd, // tool result
		EvTurnEnd,
		EvTurnStart,
		EvMessageStart, EvMessageEnd, // assistant
		EvTurnEnd,
		EvAgentEnd,
	}
	if !sameTypes(types, want) {
		t.Fatalf("event types:\n got %v\nwant %v", types, want)
	}
	// Verify tool result message content.
	for _, e := range events {
		if e.Type == EvMessageEnd && e.Message != nil && e.Message.Role == messages.RoleToolResult {
			if e.Message.Content.TextOf() != "contents of a.txt" {
				t.Errorf("tool result = %q", e.Message.Content.TextOf())
			}
			if e.Message.ToolCallID != "call_1" {
				t.Errorf("toolCallId = %q", e.Message.ToolCallID)
			}
		}
	}
}

// TestSteeringAndFollowUpOrdering: steering injected after tool turn, follow-up
// only when the agent would otherwise stop.
func TestSteeringAndFollowUpOrdering(t *testing.T) {
	seq := 0
	getSteer := func() []messages.AgentMessage {
		seq++
		if seq == 1 {
			return []messages.AgentMessage{messages.UserMessage(messages.Content{Text: "steer"}, 2)}
		}
		return nil
	}
	followCount := 0
	getFollow := func() []messages.AgentMessage {
		followCount++
		if followCount == 1 {
			return []messages.AgentMessage{messages.UserMessage(messages.Content{Text: "follow"}, 3)}
		}
		return nil
	}
	f := newFake(
		messages.AssistantMessage{API: "anthropic", Provider: "fake", Model: "test", Content: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "first"}}, Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopStop},
		messages.AssistantMessage{API: "anthropic", Provider: "fake", Model: "test", Content: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "second (after steer)"}}, Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopStop},
		messages.AssistantMessage{API: "anthropic", Provider: "fake", Model: "test", Content: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "third (after follow)"}}, Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopStop},
	)
	cfg := Config{
		Model:               testModel(),
		StreamFn:            streamFnOf(f),
		GetSteeringMessages: getSteer,
		GetFollowUpMessages: getFollow,
	}
	var msgs []messages.AgentMessage
	Run([]messages.AgentMessage{messages.UserMessage(messages.Content{Text: "go"}, 1)}, Context{}, cfg, func(e Event) {
		if e.Type == EvMessageEnd && e.Message != nil && e.Message.Role == messages.RoleUser {
			msgs = append(msgs, *e.Message)
		}
	})
	// Order: prompt, steer, assistant, follow, assistant.
	if len(msgs) != 3 {
		t.Fatalf("user messages = %d, want 3", len(msgs))
	}
	if msgs[0].Content.TextOf() != "go" || msgs[1].Content.TextOf() != "steer" || msgs[2].Content.TextOf() != "follow" {
		t.Errorf("order = %q, %q, %q", msgs[0].Content.TextOf(), msgs[1].Content.TextOf(), msgs[2].Content.TextOf())
	}
}

// TestAbortMidStream: stream drops mid-way → assistant message with
// stopReason error/aborted, loop ends.
func TestAbortMidStream(t *testing.T) {
	f := newFake(messages.AssistantMessage{API: "anthropic", Provider: "fake", Model: "test", Content: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "x"}}, Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopStop})
	f.abortOn = 0
	cfg := Config{Model: testModel(), StreamFn: streamFnOf(f)}
	events := runEventLoop(t, []messages.AgentMessage{messages.UserMessage(messages.Content{Text: "hi"}, 1)}, cfg)

	var finalErr *messages.AgentMessage
	for _, e := range events {
		if e.Type == EvMessageEnd && e.Message != nil && e.Message.Role == messages.RoleAssistant {
			finalErr = e.Message
		}
	}
	if finalErr == nil {
		t.Fatal("no assistant message")
	}
	if finalErr.StopReason != messages.StopError {
		t.Errorf("stopReason = %q, want error", finalErr.StopReason)
	}
	// Loop terminated with agent_end.
	if events[len(events)-1].Type != EvAgentEnd {
		t.Errorf("last event = %v", events[len(events)-1].Type)
	}
}

// TestBlockedToolPath: beforeToolCall blocks → error tool result, loop continues.
func TestBlockedToolPath(t *testing.T) {
	blocked := false
	readTool := tools.New("read", "Read", "read",
		mustSchemaRaw(schema.Object(nil, nil)),
		func(callID string, params json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			blocked = true // must not run
			return tools.TextResult("ran"), nil
		})
	f := newFake(
		messages.AssistantMessage{
			API: "anthropic", Provider: "fake", Model: "test",
			Content: []messages.ContentBlock{
				messages.ToolCall{Type: messages.ContentToolCall, ID: "c1", Name: "read", Arguments: map[string]any{}},
			},
			Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopToolUse,
		},
		messages.AssistantMessage{
			API: "anthropic", Provider: "fake", Model: "test",
			Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "ok"}},
			Usage:      messages.Usage{Cost: messages.Cost{}},
			StopReason: messages.StopStop,
		},
	)
	cfg := Config{
		Model:    testModel(),
		StreamFn: streamFnOf(f),
		BeforeToolCall: func(ctx BeforeToolCallContext, signal context.Context) (*BeforeToolCallResult, error) {
			return &BeforeToolCallResult{Block: true, Reason: "no reads allowed"}, nil
		},
	}
	var events []Event
	Run([]messages.AgentMessage{messages.UserMessage(messages.Content{Text: "go"}, 1)}, Context{Tools: []tools.Definition{readTool}}, cfg, func(e Event) { events = append(events, e) })
	if blocked {
		t.Error("tool executed despite block")
	}
	var sawBlockedResult bool
	for _, e := range events {
		if e.Type == EvToolExecutionEnd && e.IsError {
			sawBlockedResult = true
			if !containsStr(e.Result, "no reads allowed") {
				t.Errorf("blocked result = %+v", e.Result)
			}
		}
	}
	if !sawBlockedResult {
		t.Error("no blocked tool result")
	}
}

// TestUnknownTool: model calls a tool that isn't registered.
func TestUnknownTool(t *testing.T) {
	f := newFake(messages.AssistantMessage{
		API: "anthropic", Provider: "fake", Model: "test",
		Content: []messages.ContentBlock{
			messages.ToolCall{Type: messages.ContentToolCall, ID: "c1", Name: "nope", Arguments: map[string]any{}},
		},
		Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopToolUse,
	})
	cfg := Config{Model: testModel(), StreamFn: streamFnOf(f)}
	events := runEventLoop(t, []messages.AgentMessage{messages.UserMessage(messages.Content{Text: "go"}, 1)}, cfg)
	var sawErr bool
	for _, e := range events {
		if e.Type == EvToolExecutionEnd && e.IsError {
			sawErr = true
			if !containsStr(e.Result, "not found") {
				t.Errorf("result = %+v", e.Result)
			}
		}
	}
	if !sawErr {
		t.Error("expected unknown-tool error result")
	}
}

// TestToolPanicRecovered: a panicking tool yields an error result, not a crash.
func TestToolPanicRecovered(t *testing.T) {
	boom := tools.New("boom", "Boom", "panics",
		mustSchemaRaw(schema.Object(nil, nil)),
		func(callID string, params json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			panic("kaboom")
		})
	f := newFake(
		messages.AssistantMessage{
			API: "anthropic", Provider: "fake", Model: "test",
			Content: []messages.ContentBlock{
				messages.ToolCall{Type: messages.ContentToolCall, ID: "c1", Name: "boom", Arguments: map[string]any{}},
			},
			Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopToolUse,
		},
		messages.AssistantMessage{
			API: "anthropic", Provider: "fake", Model: "test",
			Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "recovered"}},
			Usage:      messages.Usage{Cost: messages.Cost{}},
			StopReason: messages.StopStop,
		},
	)
	cfg := Config{Model: testModel(), StreamFn: streamFnOf(f)}
	var events []Event
	Run([]messages.AgentMessage{messages.UserMessage(messages.Content{Text: "go"}, 1)}, Context{Tools: []tools.Definition{boom}}, cfg, func(e Event) { events = append(events, e) })
	var sawErr bool
	for _, e := range events {
		if e.Type == EvToolExecutionEnd && e.IsError {
			sawErr = true
			if !containsStr(e.Result, "panicked") {
				t.Errorf("result = %+v", e.Result)
			}
		}
	}
	if !sawErr {
		t.Error("expected panic-converted error result")
	}
}

// TestDeterminism: same tape + same input ⇒ identical event sequence.
func TestDeterminism(t *testing.T) {
	readTool := tools.New("read", "Read", "read",
		mustSchemaRaw(schema.Object([]string{"path"}, map[string]schema.Schema{"path": schema.Str("")})),
		func(callID string, params json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.TextResult("data"), nil
		})
	mkCfg := func() Config {
		f := newFake(
			messages.AssistantMessage{
				API: "anthropic", Provider: "fake", Model: "test",
				Content: []messages.ContentBlock{
					messages.ToolCall{Type: messages.ContentToolCall, ID: "c1", Name: "read", Arguments: map[string]any{"path": "x"}},
				},
				Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopToolUse,
			},
			messages.AssistantMessage{
				API: "anthropic", Provider: "fake", Model: "test",
				Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "final"}},
				Usage:      messages.Usage{Cost: messages.Cost{}},
				StopReason: messages.StopStop,
			},
		)
		return Config{Model: testModel(), StreamFn: streamFnOf(f)}
	}
	run := func() []EventType {
		var events []Event
		Run([]messages.AgentMessage{messages.UserMessage(messages.Content{Text: "go"}, 1)}, Context{Tools: []tools.Definition{readTool}}, mkCfg(), func(e Event) {
			events = append(events, e)
		})
		return eventTypes(events)
	}
	first := run()
	for i := 0; i < 5; i++ {
		if got := run(); !sameTypes(got, first) {
			t.Fatalf("run %d differed:\n got %v\nwant %v", i, got, first)
		}
	}
}

// TestShouldStopAfterTurn.
func TestShouldStopAfterTurn(t *testing.T) {
	f := newFake(messages.AssistantMessage{
		API: "anthropic", Provider: "fake", Model: "test",
		Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "one"}},
		Usage:      messages.Usage{Cost: messages.Cost{}},
		StopReason: messages.StopStop,
	})
	stopCalled := false
	cfg := Config{
		Model:    testModel(),
		StreamFn: streamFnOf(f),
		ShouldStopAfterTurn: func(ctx ShouldStopContext) bool {
			stopCalled = true
			return true
		},
	}
	events := runEventLoop(t, []messages.AgentMessage{messages.UserMessage(messages.Content{Text: "go"}, 1)}, cfg)
	if !stopCalled {
		t.Error("shouldStopAfterTurn not called")
	}
	if events[len(events)-1].Type != EvAgentEnd {
		t.Errorf("last = %v", events[len(events)-1].Type)
	}
}

// --- helpers ---

func sameTypes(got, want []EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func mustSchemaRaw(s schema.Schema) json.RawMessage {
	b, err := s.Raw()
	if err != nil {
		panic(err)
	}
	return b
}

func containsStr(r *tools.Result, s string) bool {
	if r == nil {
		return false
	}
	for _, blk := range r.Content {
		if t, ok := blk.(messages.TextContent); ok && contains(t.Text, s) {
			return true
		}
	}
	return false
}

func contains(hay, needle string) bool {
	return strings.Contains(hay, needle)
}
