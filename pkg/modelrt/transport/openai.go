package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"fuji/pkg/messages"
	"fuji/pkg/modelrt"
)

// openAIMessage is one Chat Completions message.
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	Reasoning  string           `json:"reasoning_content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	Name       string           `json:"name,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIBody struct {
	Model               string            `json:"model"`
	Messages            []openAIMessage   `json:"messages"`
	Tools               []openAITool      `json:"tools,omitempty"`
	Stream              bool              `json:"stream"`
	StreamOptions       *openAIStreamOpts `json:"stream_options,omitempty"`
	ReasoningEffort     *string           `json:"reasoning_effort,omitempty"`
	MaxTokens           int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
}

type openAIStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// runOpenAI streams an OpenAI-compatible Chat Completions response.
func runOpenAI(p *modelrt.Provider, m modelrt.Model, ctx context.Context, c modelrt.Context, opts modelrt.StreamOptions, apiKey string, s *modelrt.Stream, b *partialBuilder) error {
	reqCtx, cancel := contextFor(ctx, opts.Timeout)
	defer cancel()

	thinkingLevel := m.ClampThinkingLevel(opts.ThinkingLevel)

	body := openAIBody{
		Model:         m.ID,
		Messages:      openAIMessages(c.Messages, c.SystemPrompt),
		Tools:         openAITools(c.Tools),
		Stream:        true,
		StreamOptions: &openAIStreamOpts{IncludeUsage: true},
	}
	if thinkingLevel != "off" {
		lvl := thinkingLevel
		body.ReasoningEffort = &lvl
	}
	if opts.MaxTokens > 0 {
		mt := opts.MaxTokens
		body.MaxCompletionTokens = &mt
	} else if m.MaxOutput > 0 {
		mt := m.MaxOutput
		body.MaxCompletionTokens = &mt
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimSuffix(p.BaseURL, "/") + "/chat/completions"
	client := &http.Client{Timeout: 0}
	resp, err := httpDo(reqCtx, client, "POST", url, apiKey, payload, opts.Headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes := readAllLimited(resp.Body, 4096)
		return errorFromResponse(resp, bodyBytes)
	}

	return consumeOpenAISSE(resp.Body, s, b)
}

func consumeOpenAISSE(r io.Reader, s *modelrt.Stream, b *partialBuilder) error {
	rd := newSSE(r)
	// Track per-index tool-call accumulation state.
	toolIDs := map[int]string{}
	toolNames := map[int]string{}
	toolArgs := map[int]string{}
	// OpenAI tool-call indices are a separate namespace from content block
	// indices; map wire index → content block index (allocated at end).
	toolBlockIdx := map[int]int{}
	allocToolBlock := func(wire int) int {
		if bi, ok := toolBlockIdx[wire]; ok {
			return bi
		}
		bi := len(b.blocks)
		toolBlockIdx[wire] = bi
		b.toolBlock(bi)
		return bi
	}
	for {
		ev, err := rd.Next()
		if err == io.EOF {
			// Some providers (notably OpenRouter free tier) close the SSE
			// stream after the finish_reason chunk WITHOUT a [DONE]
			// sentinel. If a finish_reason was already recorded, treat the
			// clean close as a normal stop; only error when the stream died
			// before completing (no finish_reason).
			if b.stopReason != "" {
				emitFinal(s, b, modelrt.EvStop, "")
				return nil
			}
			return fmt.Errorf("stream ended unexpectedly")
		}
		if err != nil {
			return err
		}
		// Ignore [DONE].
		if strings.TrimSpace(ev.Data) == "[DONE]" {
			if b.stopReason == "" {
				b.stopReason = messages.StopStop
			}
			emitFinal(s, b, modelrt.EvStop, "")
			return nil
		}
		var chunk map[string]any
		if err := jsonUnmarshal([]byte(ev.Data), &chunk); err != nil {
			continue
		}
		// Usage (sent as a separate trailing chunk with stream_options
		// include_usage — comes after finish_reason, before [DONE]).
		if u, ok := chunk["usage"].(map[string]any); ok {
			b.usage.Input = intOf(u["prompt_tokens"])
			b.usage.Output = intOf(u["completion_tokens"])
			b.usage.TotalTokens = intOf(u["total_tokens"])
			if det, ok := u["prompt_tokens_details"].(map[string]any); ok {
				b.usage.CacheRead = intOf(det["cached_tokens"])
			}
			if det, ok := u["completion_tokens_details"].(map[string]any); ok {
				reasoning := intOf(det["reasoning_tokens"])
				b.usage.Reasoning = &reasoning
			}
			b.haveUsage = true
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		// Reasoning content → thinking block.
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			b.thinkingBlock(0)
			b.appendThinking(0, rc)
			emit(s, b, modelrt.EvThinkingDelta, 0, rc, nil)
		}
		// Content text.
		if content, ok := delta["content"].(string); ok && content != "" {
			textIdx := findOrCreateTextIndex(b, 0)
			b.appendText(textIdx, content)
			emit(s, b, modelrt.EvTextDelta, textIdx, content, nil)
		}
		// Tool calls.
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tcAny := range tcs {
				tc, _ := tcAny.(map[string]any)
				wire := intOf(tc["index"])
				index := allocToolBlock(wire)
				if id, ok := tc["id"].(string); ok && id != "" {
					toolIDs[wire] = id
				}
				if fn, ok := tc["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						toolNames[wire] = name
					}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						toolArgs[wire] += args
						b.setToolArgs(index, toolArgs[wire])
						tcBlock := b.toolBlock(index)
						emit(s, b, modelrt.EvToolCallDelta, index, args, tcBlock)
					}
				}
				if id := toolIDs[wire]; id != "" {
					b.setToolIDName(index, id, toolNames[wire])
				}
			}
		}
		// finish_reason: record it, keep consuming (usage chunk follows).
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" && fr != "null" {
			b.rawStopReason = fr
			switch fr {
			case "tool_calls":
				b.stopReason = messages.StopToolUse
			case "length":
				b.stopReason = messages.StopLength
			default:
				b.stopReason = messages.StopStop
			}
			// Emit toolcall_end for any in-flight tool calls.
			for wire := range toolArgs {
				bi := allocToolBlock(wire)
				tc := b.toolBlock(bi)
				emit(s, b, modelrt.EvToolCallEnd, bi, "", tc)
			}
		}
	}
}

// findOrCreateTextIndex returns the index of the text block, creating one at
// the end (thinking blocks come first from OpenAI-compatible reasoning).
func findOrCreateTextIndex(b *partialBuilder, hint int) int {
	for i, blk := range b.blocks {
		if _, ok := blk.(messages.TextContent); ok {
			return i
		}
	}
	return b.textBlock(len(b.blocks))
}

// openAIMessages converts LLM messages to Chat Completions format.
func openAIMessages(msgs []messages.AgentMessage, systemPrompt string) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs)+1)
	if systemPrompt != "" {
		out = append(out, openAIMessage{Role: "system", Content: jsonString(systemPrompt)})
	}
	for _, m := range msgs {
		switch m.Role {
		case messages.RoleUser:
			out = append(out, openAIMessage{Role: "user", Content: contentToOpenAI(m.Content)})
		case messages.RoleAssistant:
			om := openAIMessage{Role: "assistant", Content: contentToOpenAI(m.Content)}
			for _, blk := range m.Content.Blocks {
				if tc, ok := blk.(messages.ToolCall); ok {
					args, _ := json.Marshal(tc.Arguments)
					om.ToolCalls = append(om.ToolCalls, openAIToolCall{
						ID:       tc.ID,
						Type:     "function",
						Function: openAIToolFunction{Name: tc.Name, Arguments: string(args)},
					})
				}
			}
			out = append(out, om)
		case messages.RoleToolResult:
			out = append(out, openAIMessage{
				Role:       "tool",
				ToolCallID: m.ToolCallID,
				Content:    contentToOpenAI(m.Content),
			})
		}
	}
	return out
}

func contentToOpenAI(c messages.Content) json.RawMessage {
	if c.IsText() {
		return jsonString(c.Text)
	}
	var parts []json.RawMessage
	for _, blk := range c.Blocks {
		switch v := blk.(type) {
		case messages.TextContent:
			parts = append(parts, jsonString(v.Text))
		case messages.ThinkingContent:
			parts = append(parts, jsonString(v.Thinking))
		case messages.ImageContent:
			parts = append(parts, anBlock(map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:" + v.MimeType + ";base64," + v.Data},
			}))
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	data, _ := json.Marshal(parts)
	return data
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func openAITools(tools []modelrt.ToolSpec) []openAITool {
	out := make([]openAITool, 0, len(tools))
	for _, t := range tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
		}
		out = append(out, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}
	return out
}
