// Package messages implements fuji's message model: content blocks and the
// AgentMessage union (user / assistant / toolResult / bashExecution / custom /
// branchSummary / compactionSummary), mirroring the standard agent message
// model and its JSONL v3 serialization (core-spec §4.4, D2). JSON output is
// verbatim-compatible with the shared session log format.
package messages

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Role is a message role.
type Role string

// Roles.
const (
	RoleUser              Role = "user"
	RoleAssistant         Role = "assistant"
	RoleToolResult        Role = "toolResult"
	RoleBashExecution     Role = "bashExecution"
	RoleCustom            Role = "custom"
	RoleBranchSummary     Role = "branchSummary"
	RoleCompactionSummary Role = "compactionSummary"
)

// StopReason is a message stop reason.
type StopReason string

// Stop reasons.
const (
	StopPending StopReason = "pending"
	StopStop    StopReason = "stop"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "toolUse"
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
)

// ContentType identifies a content block type.
type ContentType string

// Content block types (TextContent, ThinkingContent, ImageContent, ToolCall).
const (
	ContentText     ContentType = "text"
	ContentThinking ContentType = "thinking"
	ContentImage    ContentType = "image"
	ContentToolCall ContentType = "toolCall"
)

// TextContent is a text block.
type TextContent struct {
	Type          ContentType `json:"type"`
	Text          string      `json:"text"`
	TextSignature string      `json:"textSignature,omitempty"`
}

// ThinkingContent is a reasoning/thinking block (thinkingSignature is the
// opaque payload providers use for multi-turn continuity).
type ThinkingContent struct {
	Type              ContentType `json:"type"`
	Thinking          string      `json:"thinking"`
	ThinkingSignature string      `json:"thinkingSignature,omitempty"`
	Redacted          bool        `json:"redacted,omitempty"`
}

// ImageContent is an image block (data is base64).
type ImageContent struct {
	Type     ContentType `json:"type"`
	Data     string      `json:"data"`
	MimeType string      `json:"mimeType"`
}

// ToolCall is an assistant tool-call block.
type ToolCall struct {
	Type             ContentType    `json:"type"`
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Arguments        map[string]any `json:"arguments"`
	ThoughtSignature string         `json:"thoughtSignature,omitempty"`
}

// ContentBlock is any content block (text, thinking, image, toolCall).
type ContentBlock interface {
	BlockType() ContentType
}

func (t TextContent) BlockType() ContentType     { return ContentText }
func (t ThinkingContent) BlockType() ContentType { return ContentThinking }
func (i ImageContent) BlockType() ContentType    { return ContentImage }
func (t ToolCall) BlockType() ContentType        { return ContentToolCall }

// UnmarshalContentBlock decodes a single content block, dispatching on its
// "type" field.
func UnmarshalContentBlock(data []byte) (ContentBlock, error) {
	var probe struct {
		Type ContentType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	switch probe.Type {
	case ContentText:
		var b TextContent
		err := json.Unmarshal(data, &b)
		return b, err
	case ContentThinking:
		var b ThinkingContent
		err := json.Unmarshal(data, &b)
		return b, err
	case ContentImage:
		var b ImageContent
		err := json.Unmarshal(data, &b)
		return b, err
	case ContentToolCall:
		var b ToolCall
		err := json.Unmarshal(data, &b)
		return b, err
	default:
		return nil, fmt.Errorf("unknown content block type %q", probe.Type)
	}
}

// Content is a message content: either a plain string (user messages) or a
// list of content blocks. It serializes to exactly the standard shape: a JSON
// string or a JSON array of blocks.
type Content struct {
	Text   string
	Blocks []ContentBlock
}

// IsText reports whether the content is a plain string.
func (c Content) IsText() bool { return c.Blocks == nil }

// TextOf returns the content as text: the plain string, or the concatenation
// of all text/thinking block text (for display purposes).
func (c Content) TextOf() string {
	if c.IsText() {
		return c.Text
	}
	var b bytes.Buffer
	for _, blk := range c.Blocks {
		switch v := blk.(type) {
		case TextContent:
			b.WriteString(v.Text)
		case ThinkingContent:
			b.WriteString(v.Thinking)
		}
	}
	return b.String()
}

// MarshalJSON implements json.Marshaler.
func (c Content) MarshalJSON() ([]byte, error) {
	if c.IsText() {
		return json.Marshal(c.Text)
	}
	return json.Marshal(c.Blocks)
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *Content) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return fmt.Errorf("empty content")
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		c.Text = s
		c.Blocks = nil
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	blocks := make([]ContentBlock, 0, len(raw))
	for _, r := range raw {
		b, err := UnmarshalContentBlock(r)
		if err != nil {
			return err
		}
		blocks = append(blocks, b)
	}
	c.Blocks = blocks
	return nil
}

// Usage holds token accounting for assistant messages.
type Usage struct {
	Input        int  `json:"input"`
	Output       int  `json:"output"`
	CacheRead    int  `json:"cacheRead"`
	CacheWrite   int  `json:"cacheWrite"`
	CacheWrite1h *int `json:"cacheWrite1h,omitempty"`
	Reasoning    *int `json:"reasoning,omitempty"`
	TotalTokens  int  `json:"totalTokens"`
	Cost         Cost `json:"cost"`
}

// Cost is the usage cost breakdown.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// AssistantMessage holds the assistant-specific fields.
type AssistantMessage struct {
	Content       []ContentBlock `json:"content"`
	API           string         `json:"api"`
	Provider      string         `json:"provider"`
	Model         string         `json:"model"`
	ResponseID    string         `json:"responseId,omitempty"`
	Usage         Usage          `json:"usage"`
	StopReason    StopReason     `json:"stopReason"`
	RawStopReason string         `json:"rawStopReason,omitempty"`
	ErrorMessage  string         `json:"errorMessage,omitempty"`
}

// ToolCalls returns the tool-call blocks of an assistant message in order.
func (m AssistantMessage) ToolCalls() []ToolCall {
	var out []ToolCall
	for _, blk := range m.Content {
		if tc, ok := blk.(ToolCall); ok {
			out = append(out, tc)
		}
	}
	return out
}

// AssistantToolCalls returns the tool-call blocks of a flat assistant
// AgentMessage in order.
func (m *AgentMessage) AssistantToolCalls() []ToolCall {
	var out []ToolCall
	for _, blk := range m.Content.Blocks {
		if tc, ok := blk.(ToolCall); ok {
			out = append(out, tc)
		}
	}
	return out
}

// HasToolCalls reports whether the message contains at least one tool call.
func (m AssistantMessage) HasToolCalls() bool { return len(m.ToolCalls()) > 0 }

// Text returns the concatenated text of the message (excluding thinking).
func (m AssistantMessage) Text() string {
	var b bytes.Buffer
	for _, blk := range m.Content {
		if t, ok := blk.(TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// AgentMessage is the union of message types. One flat struct with
// role-specific fields (JSON: role-specific keys emitted only when set), so
// serialization is compatible with the session log. Field declaration order
// follows the canonical JSON key order per role (Go marshals in declaration
// order):
//
//	user:       role, content, timestamp
//	assistant:  role, content, api, provider, model, usage, stopReason,
//	            timestamp, responseId, rawStopReason
//	toolResult: role, toolCallId, toolName, content, isError, timestamp
type AgentMessage struct {
	Role Role `json:"role"`

	// Tool result (role=toolResult).
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`

	Content   Content `json:"content,omitempty"`
	Timestamp int64   `json:"timestamp,omitempty"`

	// Assistant (role=assistant).
	API           string     `json:"api,omitempty"`
	Provider      string     `json:"provider,omitempty"`
	Model         string     `json:"model,omitempty"`
	Usage         *Usage     `json:"usage,omitempty"`
	StopReason    StopReason `json:"stopReason,omitempty"`
	IsError       bool       `json:"isError,omitempty"`
	ResponseID    string     `json:"responseId,omitempty"`
	ResponseModel string     `json:"responseModel,omitempty"`
	RawStopReason string     `json:"rawStopReason,omitempty"`
	ErrorMessage  string     `json:"errorMessage,omitempty"`

	Details        json.RawMessage `json:"details,omitempty"`
	AddedToolNames []string        `json:"addedToolNames,omitempty"`

	// Bash execution (role=bashExecution).
	Command            string `json:"command,omitempty"`
	Output             string `json:"output,omitempty"`
	ExitCode           *int   `json:"exitCode,omitempty"`
	Cancelled          bool   `json:"cancelled,omitempty"`
	Truncated          bool   `json:"truncated,omitempty"`
	FullOutputPath     string `json:"fullOutputPath,omitempty"`
	ExcludeFromContext bool   `json:"excludeFromContext,omitempty"`

	// Custom (role=custom).
	CustomType string `json:"customType,omitempty"`
	Display    bool   `json:"display,omitempty"`

	// Branch summary (role=branchSummary) / compaction summary.
	Summary      string `json:"summary,omitempty"`
	FromID       string `json:"fromId,omitempty"`
	TokensBefore int    `json:"tokensBefore,omitempty"`
}

// UserMessage builds a user message with the given content and timestamp.
func UserMessage(content Content, timestamp int64) AgentMessage {
	return AgentMessage{Role: RoleUser, Content: content, Timestamp: timestamp}
}

// WithAssistant creates a flat assistant AgentMessage from an
// AssistantMessage payload.
func (m AgentMessage) WithAssistant(a AssistantMessage, timestamp int64) AgentMessage {
	m.Role = RoleAssistant
	m.Timestamp = timestamp
	m.Content = Content{Blocks: a.Content}
	m.API = a.API
	m.Provider = a.Provider
	m.Model = a.Model
	m.ResponseID = a.ResponseID
	u := a.Usage
	m.Usage = &u
	m.StopReason = a.StopReason
	m.RawStopReason = a.RawStopReason
	m.ErrorMessage = a.ErrorMessage
	return m
}

// ToolResult builds a tool-result message.
func ToolResult(toolCallID, toolName string, content []ContentBlock, isError bool, details json.RawMessage, timestamp int64) AgentMessage {
	return AgentMessage{
		Role:       RoleToolResult,
		Content:    Content{Blocks: content},
		Timestamp:  timestamp,
		ToolCallID: toolCallID,
		ToolName:   toolName,
		IsError:    isError,
		Details:    details,
	}
}

// UnmarshalJSON implements json.Unmarshaler with role dispatch.
func (m *AgentMessage) UnmarshalJSON(data []byte) error {
	var flat struct {
		Role Role `json:"role"`
	}
	if err := json.Unmarshal(data, &flat); err != nil {
		return err
	}
	// Decode into a fresh AgentMessage through an alias to avoid recursion.
	type alias AgentMessage
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*m = AgentMessage(a)
	return nil
}
