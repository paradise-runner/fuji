// Package tools defines the tool contract and registry (core-spec §4.5, D6):
// Definition, Result, and the per-session Registry the agent loop consults.
// Concrete tools live in pkg/tools/{read,write,edit,bash,grep,find,ls,git}.
package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"fuji/pkg/messages"
)

// Update is a partial tool-execution update streamed via onUpdate.
type Update struct {
	Content   []messages.ContentBlock
	Details   any
	IsError   bool
	Terminate bool
	BashDelta string // for bash: incremental output chunk (bash_execution_update)
}

// Result is the final tool result.
type Result struct {
	Content    []messages.ContentBlock
	Details    any
	Usage      json.RawMessage `json:"usage,omitempty"`
	IsError    bool
	Terminate  bool
	AddedTools []string
}

// ContentText returns the concatenated text of the result's text blocks.
func (r Result) ContentText() string {
	var sb strings.Builder
	for _, blk := range r.Content {
		if t, ok := blk.(messages.TextContent); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

// TextResult builds a text-only result.
func TextResult(text string) Result {
	return Result{Content: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: text}}}
}

// ErrorResult builds an error result with the given message.
func ErrorResult(err error) Result {
	msg := "tool error"
	if err != nil {
		msg = err.Error()
	}
	return Result{Content: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: msg}}, IsError: true}
}

// Definition is an LLM-facing tool.
type Definition struct {
	Name             string
	Label            string
	Description      string
	PromptSnippet    string
	PromptGuidelines []string
	Parameters       json.RawMessage // JSON Schema

	// Execute runs the tool. It must never panic (the loop recovers at the
	// Execute boundary anyway). ctx carries cancellation/timeouts; onUpdate
	// streams partial progress.
	Execute func(callID string, params json.RawMessage, ctx context.Context, onUpdate func(Update)) (Result, error)
}

// New builds a Definition with a schema.
func New(name, label, description string, parameters json.RawMessage, execute func(callID string, params json.RawMessage, ctx context.Context, onUpdate func(Update)) (Result, error)) Definition {
	return Definition{
		Name: name, Label: label, Description: description,
		Parameters: parameters, Execute: execute,
	}
}

// Registry is the per-session tool set.
type Registry struct {
	mu      sync.RWMutex
	byName  map[string]*Definition
	order   []string
	allowed map[string]bool
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]*Definition{}, allowed: map[string]bool{}}
}

// Register adds a tool. Duplicate names overwrite.
func (r *Registry) Register(def Definition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[def.Name]; !exists {
		r.order = append(r.order, def.Name)
	}
	r.byName[def.Name] = &def
}

// SetAllowed restricts the registry to the given allowlist (nil = all).
func (r *Registry) SetAllowed(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allowed = map[string]bool{}
	for _, n := range names {
		r.allowed[n] = true
	}
}

// Exclude removes tools from the registry (denylist).
func (r *Registry) Exclude(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range names {
		delete(r.byName, n)
	}
}

// Get returns a tool by name (respecting allow/deny).
func (r *Registry) Get(name string) (*Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.allowed) > 0 && !r.allowed[name] {
		return nil, false
	}
	def, ok := r.byName[name]
	return def, ok
}

// List returns all registered definitions in registration order.
func (r *Registry) List() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		if len(r.allowed) > 0 && !r.allowed[name] {
			continue
		}
		if def, ok := r.byName[name]; ok {
			out = append(out, *def)
		}
	}
	return out
}

// Names returns registered tool names in order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.order))
	for _, name := range r.order {
		if len(r.allowed) > 0 && !r.allowed[name] {
			continue
		}
		if _, ok := r.byName[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// ToolResultMessage builds a toolResult AgentMessage.
func ToolResultMessage(finalized FinalizedCall) messages.AgentMessage {
	content := finalized.Result.Content
	if content == nil {
		content = []messages.ContentBlock{}
	}
	details, _ := json.Marshal(finalized.Result.Details)
	return messages.AgentMessage{
		Role:       messages.RoleToolResult,
		ToolCallID: finalized.ToolCall.ID,
		ToolName:   finalized.ToolCall.Name,
		Content:    messages.Content{Blocks: content},
		Details:    details,
		IsError:    finalized.IsError,
		Timestamp:  nowMs(),
	}
}

// FinalizedCall is a tool call with its executed result.
type FinalizedCall struct {
	ToolCall messages.ToolCall
	Result   Result
	IsError  bool
}

// ErrorResultFor builds an error result for a call.
func ErrorResultFor(err error) Result {
	return ErrorResult(err)
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}
