package transport

import (
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

// anthropicMessagesBody is the request body for POST {base}/v1/messages.
type anthropicMessagesBody struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      []json.RawMessage  `json:"system,omitempty"`
	Messages    []anthropicMsg     `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Stream      bool               `json:"stream"`
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicMsg struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// runAnthropic streams an Anthropic Messages API response.
func runAnthropic(p *modelrt.Provider, m modelrt.Model, ctx context.Context, c modelrt.Context, opts modelrt.StreamOptions, apiKey string, s *modelrt.Stream, b *partialBuilder) error {
	reqCtx, cancel := contextFor(ctx, opts.Timeout)
	defer cancel()

	thinkingLevel := m.ClampThinkingLevel(opts.ThinkingLevel)

	body := anthropicMessagesBody{
		Model:     m.ID,
		MaxTokens: maxOutput(opts.MaxTokens, m.MaxOutput),
		Stream:    true,
		Messages:  anthropicMessages(c.Messages),
		Tools:     anthropicTools(c.Tools),
	}
	if c.SystemPrompt != "" {
		body.System = []json.RawMessage{anBlock(map[string]any{"type": "text", "text": c.SystemPrompt})}
	}
	if thinkingLevel != "off" && len(m.ThinkingLevels) > 0 {
		body.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: thinkingBudget(thinkingLevel)}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimSuffix(p.BaseURL, "/") + "/v1/messages"
	client := &http.Client{Timeout: 0}
	extra := map[string]string{"anthropic-version": "2023-06-01"}
	for k, v := range opts.Headers {
		extra[k] = v
	}
	resp, err := httpDo(reqCtx, client, "POST", url, apiKey, payload, extra)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes := readAllLimited(resp.Body, 4096)
		return errorFromResponse(resp, bodyBytes)
	}

	return consumeAnthropicSSE(resp.Body, s, b)
}

func consumeAnthropicSSE(r io.Reader, s *modelrt.Stream, b *partialBuilder) error {
	rd := newSSE(r)
	for {
		ev, err := rd.Next()
		if err == io.EOF {
			// Stream ended without message_stop: mid-stream drop.
			return fmt.Errorf("stream ended unexpectedly")
		}
		if err != nil {
			return err
		}
		if err := handleAnthropicEvent(ev, s, b); err != nil {
			return err
		}
		if b.stopReason == messages.StopStop || b.stopReason == messages.StopToolUse || b.stopReason == messages.StopLength {
			emitFinal(s, b, modelrt.EvStop, "")
			return nil
		}
	}
}

func handleAnthropicEvent(ev *sse.Event, s *modelrt.Stream, b *partialBuilder) error {
	var msg map[string]any
	if err := jsonUnmarshal([]byte(ev.Data), &msg); err != nil {
		return nil // ignore malformed data frames
	}
	typ, _ := msg["type"].(string)
	switch typ {
	case "message_start":
		if m, ok := msg["message"].(map[string]any); ok {
			if u, ok := m["usage"].(map[string]any); ok {
				b.usage.Input = intOf(u["input_tokens"])
				b.usage.CacheRead = intOf(u["cache_read_input_tokens"])
				b.usage.CacheWrite = intOf(u["cache_creation_input_tokens"])
			}
		}
	case "content_block_start":
		index := intOf(msg["index"])
		cb, _ := msg["content_block"].(map[string]any)
		ctype, _ := cb["type"].(string)
		switch ctype {
		case "text":
			b.textBlock(index)
			emit(s, b, modelrt.EvTextStart, index, "", nil)
		case "thinking":
			b.thinkingBlock(index)
			emit(s, b, modelrt.EvThinkingStart, index, "", nil)
		case "tool_use":
			b.initRawArgs()
			id, _ := cb["id"].(string)
			name, _ := cb["name"].(string)
			b.setToolIDName(index, id, name)
			tc := b.toolBlock(index)
			emit(s, b, modelrt.EvToolCallStart, index, "", tc)
		}
	case "content_block_delta":
		index := intOf(msg["index"])
		delta, _ := msg["delta"].(map[string]any)
		dtype, _ := delta["type"].(string)
		switch dtype {
		case "text_delta":
			text, _ := delta["text"].(string)
			b.appendText(index, text)
			emit(s, b, modelrt.EvTextDelta, index, text, nil)
		case "thinking_delta":
			text, _ := delta["thinking"].(string)
			b.appendThinking(index, text)
			emit(s, b, modelrt.EvThinkingDelta, index, text, nil)
		case "input_json_delta":
			partial, _ := delta["partial_json"].(string)
			b.appendToolArgs(index, partial)
			tc := b.toolBlock(index)
			emit(s, b, modelrt.EvToolCallDelta, index, partial, tc)
		}
	case "content_block_stop":
		index := intOf(msg["index"])
		if index >= 0 && index < len(b.blocks) {
			switch b.blocks[index].(type) {
			case messages.TextContent:
				emit(s, b, modelrt.EvTextEnd, index, "", nil)
			case messages.ThinkingContent:
				emit(s, b, modelrt.EvThinkingEnd, index, "", nil)
			case messages.ToolCall:
				tc := b.toolBlock(index)
				emit(s, b, modelrt.EvToolCallEnd, index, "", tc)
			}
		}
	case "message_delta":
		if delta, ok := msg["delta"].(map[string]any); ok {
			reason, _ := delta["stop_reason"].(string)
			b.rawStopReason = reason
			switch reason {
			case "end_turn", "stop_sequence":
				b.stopReason = messages.StopStop
			case "max_tokens":
				b.stopReason = messages.StopLength
			case "tool_use":
				b.stopReason = messages.StopToolUse
			default:
				b.stopReason = messages.StopStop
			}
		}
		if u, ok := msg["usage"].(map[string]any); ok {
			b.usage.Output = intOf(u["output_tokens"])
			b.haveUsage = true
		}
	case "message_stop":
		// The consumer checks stopReason; nothing more to do.
	case "error":
		if errMap, ok := msg["error"].(map[string]any); ok {
			msgText, _ := errMap["message"].(string)
			return fmt.Errorf("%s", msgText)
		}
		return fmt.Errorf("provider error")
	}
	return nil
}

// anthropicMessages converts LLM messages to Anthropic wire format.
func anthropicMessages(msgs []messages.AgentMessage) []anthropicMsg {
	out := make([]anthropicMsg, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case messages.RoleUser:
			out = append(out, anthropicMsg{Role: "user", Content: contentToAnthropic(m.Content, nil)})
		case messages.RoleAssistant:
			out = append(out, anthropicMsg{Role: "assistant", Content: contentToAnthropic(m.Content, nil)})
		case messages.RoleToolResult:
			out = append(out, anthropicMsg{Role: "user", Content: contentToAnthropic(m.Content, &m)})
		}
	}
	return out
}

func contentToAnthropic(c messages.Content, toolResult *messages.AgentMessage) []json.RawMessage {
	var blocks []json.RawMessage
	emitText := func(text string) {
		blocks = append(blocks, anBlock(map[string]any{"type": "text", "text": text}))
	}
	if c.IsText() {
		emitText(c.Text)
	} else {
		for _, blk := range c.Blocks {
			switch v := blk.(type) {
			case messages.TextContent:
				emitText(v.Text)
			case messages.ThinkingContent:
				// Replayed thinking: native Anthropic thinking cannot be
				// replayed; send as text.
				emitText(v.Thinking)
			case messages.ToolCall:
				args, err := json.Marshal(v.Arguments)
				if err != nil {
					args = []byte("{}")
				}
				blocks = append(blocks, anBlock(map[string]any{
					"type":  "tool_use",
					"id":    v.ID,
					"name":  v.Name,
					"input": json.RawMessage(args),
				}))
			case messages.ImageContent:
				blocks = append(blocks, anBlock(map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": v.MimeType,
						"data":       v.Data,
					},
				}))
			}
		}
	}
	if toolResult != nil {
		var resultBlocks []json.RawMessage
		for _, blk := range blocks {
			tr := map[string]any{
				"type":        "tool_result",
				"tool_use_id": toolResult.ToolCallID,
				"content":     []json.RawMessage{blk},
			}
			if toolResult.IsError {
				tr["is_error"] = true
			}
			resultBlocks = append(resultBlocks, anBlock(tr))
		}
		return resultBlocks
	}
	return blocks
}

// anBlock marshals a block map to raw JSON.
func anBlock(m map[string]any) json.RawMessage {
	data, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{"type":"text","text":""}`)
	}
	return data
}

func anthropicTools(tools []modelrt.ToolSpec) []anthropicTool {
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
		}
		out = append(out, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return out
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	default:
		return 0
	}
}

func maxOutput(requested, modelMax int) int {
	if requested > 0 && requested < modelMax {
		return requested
	}
	if modelMax > 0 {
		return modelMax
	}
	return 8192
}

// thinkingBudget maps thinking levels to Anthropic budget tokens.
func thinkingBudget(level string) int {
	switch level {
	case "minimal":
		return 2048
	case "low":
		return 4096
	case "medium":
		return 8192
	case "high":
		return 16384
	case "xhigh":
		return 32768
	case "max":
		return 64000
	default:
		return 8192
	}
}

func contextFor(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}
