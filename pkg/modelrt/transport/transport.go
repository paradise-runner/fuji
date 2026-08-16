// Package transport implements the raw HTTP/SSE provider transports
// (core-spec §4.3, D10): Anthropic Messages and OpenAI-compatible Chat
// Completions. Each maps provider SSE events to the shared modelrt stream
// events. Failures never escape as errors — they are encoded as error/aborted
// events with a final assistant message (StreamFn contract).
package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"fuji/internal/sse"
	"fuji/pkg/messages"
	"fuji/pkg/modelrt"
)

// partialBuilder accumulates an assistant message during streaming.
type partialBuilder struct {
	API           string
	Provider      string
	Model         string
	Timestamp     int64
	blocks        []messages.ContentBlock
	rawArgs       map[int]string
	stopReason    messages.StopReason
	rawStopReason string
	usage         messages.Usage
	haveUsage     bool
	errorMessage  string
}

func newPartialBuilder(api, provider, model string) *partialBuilder {
	return &partialBuilder{API: api, Provider: provider, Model: model, Timestamp: time.Now().UnixMilli()}
}

// ensureText returns the index of the text block at the given content index,
// appending a new block when needed.
func (b *partialBuilder) textBlock(index int) int {
	for len(b.blocks) <= index {
		b.blocks = append(b.blocks, messages.TextContent{Type: messages.ContentText, Text: ""})
	}
	return index
}

func (b *partialBuilder) thinkingBlock(index int) int {
	for len(b.blocks) <= index {
		b.blocks = append(b.blocks, messages.ThinkingContent{Type: messages.ContentThinking, Thinking: ""})
	}
	return index
}

func (b *partialBuilder) toolBlock(index int) *messages.ToolCall {
	for len(b.blocks) <= index {
		b.blocks = append(b.blocks, messages.ToolCall{Type: messages.ContentToolCall, Arguments: map[string]any{}})
	}
	tc, ok := b.blocks[index].(messages.ToolCall)
	if !ok {
		b.blocks[index] = messages.ToolCall{Type: messages.ContentToolCall, Arguments: map[string]any{}}
		tc = b.blocks[index].(messages.ToolCall)
	}
	return &tc
}

// setTool replaces the tool block at index.
func (b *partialBuilder) setTool(index int, tc messages.ToolCall) {
	for len(b.blocks) <= index {
		b.blocks = append(b.blocks, messages.ToolCall{Type: messages.ContentToolCall, Arguments: map[string]any{}})
	}
	b.blocks[index] = tc
}

// snapshot returns a deep-ish copy of the partial assistant message.
func (b *partialBuilder) snapshot() *messages.AgentMessage {
	m := messages.AgentMessage{}
	blocks := make([]messages.ContentBlock, len(b.blocks))
	for i, blk := range b.blocks {
		blocks[i] = cloneBlock(blk)
	}
	usage := b.usage
	am := messages.AssistantMessage{
		Content:       blocks,
		API:           b.API,
		Provider:      b.Provider,
		Model:         b.Model,
		Usage:         usage,
		StopReason:    b.stopReason,
		RawStopReason: b.rawStopReason,
		ErrorMessage:  b.errorMessage,
	}
	if !b.haveUsage {
		am.Usage = messages.Usage{Cost: messages.Cost{}}
	}
	v := m.WithAssistant(am, b.Timestamp)
	return &v
}

func cloneBlock(b messages.ContentBlock) messages.ContentBlock {
	switch v := b.(type) {
	case messages.TextContent:
		return v
	case messages.ThinkingContent:
		return v
	case messages.ToolCall:
		args := make(map[string]any, len(v.Arguments))
		for k, val := range v.Arguments {
			args[k] = val
		}
		v.Arguments = args
		return v
	default:
		return b
	}
}

func (b *partialBuilder) appendText(index int, delta string) {
	i := b.textBlock(index)
	if t, ok := b.blocks[i].(messages.TextContent); ok {
		t.Text += delta
		b.blocks[i] = t
	}
}

func (b *partialBuilder) appendThinking(index int, delta string) {
	i := b.thinkingBlock(index)
	if t, ok := b.blocks[i].(messages.ThinkingContent); ok {
		t.Thinking += delta
		b.blocks[i] = t
	}
}

func (b *partialBuilder) appendToolArgs(index int, delta string) {
	tc := b.toolBlock(index)
	// Accumulate raw args and re-parse.
	b.rawArgs[index] += delta
	if parsed, err := parseJSONObject(b.rawArgs[index]); err == nil {
		tc.Arguments = parsed
		b.setTool(index, *tc)
	}
}

func (b *partialBuilder) setToolArgs(index int, raw string) {
	if parsed, err := parseJSONObject(raw); err == nil {
		tc := b.toolBlock(index)
		tc.Arguments = parsed
		b.setTool(index, *tc)
	}
}

func (b *partialBuilder) setToolIDName(index int, id, name string) {
	tc := b.toolBlock(index)
	tc.ID = id
	tc.Name = name
	b.setTool(index, *tc)
}

// initRawArgs resets the tool-call argument accumulator.
func (b *partialBuilder) initRawArgs() { b.rawArgs = map[int]string{} }

func parseJSONObject(s string) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// emit sends an event with a partial snapshot.
func emit(s *modelrt.Stream, b *partialBuilder, evType modelrt.EventType, index int, delta string, tc *messages.ToolCall) {
	s.Send(modelrt.StreamEvent{
		Type:     evType,
		Index:    index,
		Delta:    delta,
		ToolCall: tc,
		Message:  b.snapshot(),
	})
}

func emitFinal(s *modelrt.Stream, b *partialBuilder, evType modelrt.EventType, errMsg string) {
	switch evType {
	case modelrt.EvError:
		b.stopReason = messages.StopError
	case modelrt.EvAborted:
		b.stopReason = messages.StopAborted
	}
	b.errorMessage = errMsg
	s.Send(modelrt.StreamEvent{
		Type:    evType,
		Message: b.snapshot(),
		Error:   errMsg,
	})
}

// Run is the TransportRunner entry point (registered with modelrt for the
// anthropic and openai-completions API families).
type runner struct{}

func init() {
	modelrt.RegisterTransport(modelrt.APIAnthropic, runner{})
	modelrt.RegisterTransport(modelrt.APIOpenAICompletions, runner{})
}

// Run dispatches to the right transport. Called from modelrt.
func (runner) Run(p *modelrt.Provider, m modelrt.Model, ctx context.Context, c modelrt.Context, opts modelrt.StreamOptions, apiKey string, s *modelrt.Stream) {
	b := newPartialBuilder(string(p.API), p.ID, m.ID)
	b.initRawArgs()
	var err error
	switch p.API {
	case modelrt.APIAnthropic:
		err = runAnthropic(p, m, ctx, c, opts, apiKey, s, b)
	case modelrt.APIOpenAICompletions:
		err = runOpenAI(p, m, ctx, c, opts, apiKey, s, b)
	default:
		err = fmt.Errorf("unsupported API %q", p.API)
	}
	if err != nil {
		if ctx.Err() != nil {
			emitFinal(s, b, modelrt.EvAborted, "aborted")
		} else {
			emitFinal(s, b, modelrt.EvError, err.Error())
		}
	}
}

// httpDo performs the request with auth, returning the response.
func httpDo(ctx context.Context, client *http.Client, method, url string, apiKey string, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// errorFromResponse parses provider error bodies into a message.
func errorFromResponse(resp *http.Response, body []byte) error {
	status := resp.StatusCode
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	} else if len(msg) > 500 {
		msg = msg[:500]
	}
	switch {
	case status == 401 || status == 403:
		return fmt.Errorf("authentication failed (%d): %s", status, msg)
	case status == 404:
		return fmt.Errorf("not found (%d): %s", status, msg)
	case status == 429:
		return fmt.Errorf("rate limited (%d): %s", status, msg)
	case status >= 500:
		return fmt.Errorf("provider error (%d): %s", status, msg)
	case status >= 400:
		return fmt.Errorf("request rejected (%d): %s", status, msg)
	}
	return nil
}

// readAllLimited reads up to limit bytes.
func readAllLimited(r io.Reader, limit int64) []byte {
	lr := io.LimitReader(r, limit)
	data, _ := io.ReadAll(lr)
	return data
}

// sseReader wraps sse.Reader to read raw events for a transport.
func newSSE(r io.Reader) *sse.Reader { return sse.NewReader(bufio.NewReaderSize(r, 64*1024)) }

// readLine reads a raw line (for HTTP parsing helpers).
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// jsonUnmarshal is a small alias for compactness.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
