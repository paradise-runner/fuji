// Package loop implements fuji's single-threaded agent loop (core-spec §4.2,
// D13): the state machine that converts conversation context to provider
// messages, streams assistant responses, executes tool calls sequentially,
// and repeats until the model stops calling tools. It has no I/O knowledge;
// everything arrives via Config callbacks.
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"

	"fuji/pkg/messages"
	"fuji/pkg/modelrt"
	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// EventType identifies a loop event.
type EventType string

// Loop event types (core-spec §4.1 AgentEvent surface).
const (
	EvAgentStart          EventType = "agent_start"
	EvAgentEnd            EventType = "agent_end"
	EvTurnStart           EventType = "turn_start"
	EvTurnEnd             EventType = "turn_end"
	EvMessageStart        EventType = "message_start"
	EvMessageUpdate       EventType = "message_update"
	EvMessageEnd          EventType = "message_end"
	EvToolExecutionStart  EventType = "tool_execution_start"
	EvToolExecutionUpdate EventType = "tool_execution_update"
	EvToolExecutionEnd    EventType = "tool_execution_end"
)

// Event is the loop event union.
type Event struct {
	Type EventType

	// agent_end
	Messages []messages.AgentMessage

	// turn_end / message_*
	Message     *messages.AgentMessage
	ToolResults []messages.AgentMessage

	// message_update
	AssistantMessageEvent *modelrt.StreamEvent

	// tool_execution_*
	ToolCallID    string
	ToolName      string
	Args          map[string]any
	PartialResult any
	Result        *tools.Result
	IsError       bool
}

// Context is the agent context snapshot (system prompt + transcript + tools).
type Context struct {
	SystemPrompt string
	Messages     []messages.AgentMessage
	Tools        []tools.Definition
}

// ShouldStopContext is passed to ShouldStopAfterTurn / PrepareNextTurn.
type ShouldStopContext struct {
	Message     *messages.AgentMessage
	ToolResults []messages.AgentMessage
	Context     Context
	NewMessages []messages.AgentMessage
}

// BeforeToolCallContext is passed to BeforeToolCall.
type BeforeToolCallContext struct {
	AssistantMessage *messages.AgentMessage
	ToolCall         messages.ToolCall
	Args             map[string]any
	Context          Context
}

// BeforeToolCallResult controls blocking.
type BeforeToolCallResult struct {
	Block  bool
	Reason string
}

// AfterToolCallContext is passed to AfterToolCall.
type AfterToolCallContext struct {
	AssistantMessage *messages.AgentMessage
	ToolCall         messages.ToolCall
	Args             map[string]any
	Result           tools.Result
	IsError          bool
	Context          Context
}

// AfterToolCallResult allows partial overrides.
type AfterToolCallResult struct {
	Content   []messages.ContentBlock
	Details   any
	IsError   *bool
	Terminate *bool
}

// TurnUpdate is returned by PrepareNextTurn.
type TurnUpdate struct {
	Context       *Context
	Model         *modelrt.Model
	ThinkingLevel *string
}

// StreamFn is the provider stream function (satisfied by modelrt.Runtime).
// Per the StreamFn contract it never returns an error for request/model/
// runtime failures — failures are encoded in the stream.
type StreamFn func(model modelrt.Model, ctx context.Context, c modelrt.Context, opts modelrt.StreamOptions) *modelrt.Stream

// Config wires the loop to its environment.
type Config struct {
	Model         modelrt.Model
	ThinkingLevel string
	SystemPrompt  string
	APIKey        string

	ConvertToLLM     func([]messages.AgentMessage) []messages.AgentMessage
	TransformContext func([]messages.AgentMessage, context.Context) []messages.AgentMessage
	GetAPIKey        func(provider string) string

	ShouldStopAfterTurn func(ShouldStopContext) bool
	PrepareNextTurn     func(ShouldStopContext) *TurnUpdate
	GetSteeringMessages func() []messages.AgentMessage
	GetFollowUpMessages func() []messages.AgentMessage
	BeforeToolCall      func(BeforeToolCallContext, context.Context) (*BeforeToolCallResult, error)
	AfterToolCall       func(AfterToolCallContext, context.Context) (*AfterToolCallResult, error)

	Signal   context.Context
	StreamFn StreamFn
	Timeout  modelrt.StreamOptions // per-turn options (thinking level handled separately)

	// App attribution (OpenRouter). Sent as HTTP-Referer, X-OpenRouter-Title,
	// and X-OpenRouter-Categories headers on provider requests.
	AppURL        string
	AppTitle      string
	AppCategories string
}

// Run starts an agent run with prompts.
func Run(prompts []messages.AgentMessage, ctx Context, cfg Config, emit func(Event)) []messages.AgentMessage {
	// newMessages starts with the prompts.
	newMessages := append([]messages.AgentMessage{}, prompts...)
	current := Context{
		SystemPrompt: ctx.SystemPrompt,
		Messages:     append(append([]messages.AgentMessage{}, ctx.Messages...), prompts...),
		Tools:        ctx.Tools,
	}
	emit(Event{Type: EvAgentStart})
	emit(Event{Type: EvTurnStart})
	for _, p := range prompts {
		emit(Event{Type: EvMessageStart, Message: &p})
		emit(Event{Type: EvMessageEnd, Message: &p})
	}
	runLoop(&current, &newMessages, &cfg, emit)
	return newMessages
}

// Continue continues an existing context.
func Continue(ctx Context, cfg Config, emit func(Event)) ([]messages.AgentMessage, error) {
	if len(ctx.Messages) == 0 {
		return nil, fmt.Errorf("cannot continue: no messages in context")
	}
	if ctx.Messages[len(ctx.Messages)-1].Role == messages.RoleAssistant {
		return nil, fmt.Errorf("cannot continue from message role: assistant")
	}
	var newMessages []messages.AgentMessage
	current := ctx
	emit(Event{Type: EvAgentStart})
	emit(Event{Type: EvTurnStart})
	runLoop(&current, &newMessages, &cfg, emit)
	return newMessages, nil
}

// runLoop is the single-threaded state machine.
func runLoop(ctx *Context, newMessages *[]messages.AgentMessage, cfg *Config, emit func(Event)) {
	config := *cfg
	current := *ctx
	firstTurn := true
	// Steering messages may be waiting before the run starts.
	pending := orEmpty(config.GetSteeringMessages)

	for {
		hasMoreToolCalls := true
		innerFirst := true
		for hasMoreToolCalls || len(pending) > 0 {
			if firstTurn && innerFirst {
				// turn_start already emitted by Run/Continue.
			} else {
				emit(Event{Type: EvTurnStart})
			}
			innerFirst = false
			firstTurn = false

			// Inject pending (steering / follow-up) messages.
			if len(pending) > 0 {
				for i := range pending {
					emit(Event{Type: EvMessageStart, Message: &pending[i]})
					emit(Event{Type: EvMessageEnd, Message: &pending[i]})
					current.Messages = append(current.Messages, pending[i])
					*newMessages = append(*newMessages, pending[i])
				}
				pending = nil
			}

			// Stream the assistant response.
			message := streamAssistantResponse(&current, &config, emit)

			if message.StopReason == messages.StopError || message.StopReason == messages.StopAborted {
				emit(Event{Type: EvTurnEnd, Message: message, ToolResults: nil})
				emit(Event{Type: EvAgentEnd, Messages: *newMessages})
				return
			}
			*newMessages = append(*newMessages, *message)

			toolCalls := message.AssistantToolCalls()
			var toolResults []messages.AgentMessage
			hasMoreToolCalls = false
			if len(toolCalls) > 0 {
				var terminated bool
				var results []messages.AgentMessage
				if message.StopReason == messages.StopLength {
					results = failTruncatedToolCalls(toolCalls, emit)
					terminated = false
				} else {
					results, terminated = executeToolCalls(&current, message, &config, emit)
				}
				toolResults = append(toolResults, results...)
				hasMoreToolCalls = !terminated
				for _, r := range toolResults {
					current.Messages = append(current.Messages, r)
					*newMessages = append(*newMessages, r)
				}
			}

			emit(Event{Type: EvTurnEnd, Message: message, ToolResults: toolResults})

			// PrepareNextTurn may swap context/model/thinking.
			if config.PrepareNextTurn != nil {
				if update := config.PrepareNextTurn(ShouldStopContext{
					Message: message, ToolResults: toolResults, Context: current, NewMessages: *newMessages,
				}); update != nil {
					if update.Context != nil {
						current = *update.Context
					}
					if update.Model != nil {
						config.Model = *update.Model
					}
					if update.ThinkingLevel != nil {
						config.ThinkingLevel = *update.ThinkingLevel
					}
				}
			}

			if config.ShouldStopAfterTurn != nil && config.ShouldStopAfterTurn(ShouldStopContext{
				Message: message, ToolResults: toolResults, Context: current, NewMessages: *newMessages,
			}) {
				emit(Event{Type: EvAgentEnd, Messages: *newMessages})
				return
			}

			pending = orEmpty(config.GetSteeringMessages)
		}

		// Agent would stop here — check for follow-up messages.
		followUp := orEmpty(config.GetFollowUpMessages)
		if len(followUp) > 0 {
			pending = followUp
			continue
		}
		break
	}
	emit(Event{Type: EvAgentEnd, Messages: *newMessages})
}

// streamAssistantResponse streams one assistant turn.
func streamAssistantResponse(ctx *Context, cfg *Config, emit func(Event)) *messages.AgentMessage {
	// Transform context.
	messagesForLLM := ctx.Messages
	if cfg.TransformContext != nil {
		messagesForLLM = cfg.TransformContext(messagesForLLM, cfg.Signal)
	}
	// Convert to LLM messages.
	llmMessages := messagesForLLM
	if cfg.ConvertToLLM != nil {
		llmMessages = cfg.ConvertToLLM(messagesForLLM)
	}
	// Build modelrt context.
	modelCtx := modelrt.Context{
		SystemPrompt: ctx.SystemPrompt,
		Messages:     llmMessages,
		Tools:        definitionsToSpecs(ctx.Tools),
	}
	// Resolve API key.
	apiKey := cfg.APIKey
	if cfg.GetAPIKey != nil {
		if k := cfg.GetAPIKey(cfg.Model.ProviderID); k != "" {
			apiKey = k
		}
	}
	opts := cfg.Timeout
	opts.ThinkingLevel = cfg.ThinkingLevel
	opts.APIKey = apiKey
	// App attribution headers (OpenRouter / HTTP-Referer attribution).
	if cfg.AppURL != "" {
		if opts.Headers == nil {
			opts.Headers = map[string]string{}
		}
		opts.Headers["HTTP-Referer"] = cfg.AppURL
	}
	if cfg.AppTitle != "" {
		if opts.Headers == nil {
			opts.Headers = map[string]string{}
		}
		opts.Headers["X-OpenRouter-Title"] = cfg.AppTitle
	}
	if cfg.AppCategories != "" {
		if opts.Headers == nil {
			opts.Headers = map[string]string{}
		}
		opts.Headers["X-OpenRouter-Categories"] = cfg.AppCategories
	}
	signal := cfg.Signal
	if signal == nil {
		signal = context.Background()
	}

	stream := cfg.StreamFn(cfg.Model, signal, modelCtx, opts)
	if stream == nil {
		return errorMessage(cfg, "stream function returned nil")
	}

	var partial *messages.AgentMessage
	addedPartial := false
	for ev := range stream.Events() {
		switch ev.Type {
		case modelrt.EvTextStart, modelrt.EvTextDelta, modelrt.EvTextEnd,
			modelrt.EvThinkingStart, modelrt.EvThinkingDelta, modelrt.EvThinkingEnd,
			modelrt.EvToolCallStart, modelrt.EvToolCallDelta, modelrt.EvToolCallEnd:
			if ev.Message == nil {
				continue
			}
			if partial == nil {
				partial = ev.Message
				ctx.Messages = append(ctx.Messages, *partial)
				addedPartial = true
				cp := *partial
				emit(Event{Type: EvMessageStart, Message: &cp})
			} else {
				partial = ev.Message
				ctx.Messages[len(ctx.Messages)-1] = *partial
				evCopy := ev
				msgCopy := *partial
				emit(Event{Type: EvMessageUpdate, Message: &msgCopy, AssistantMessageEvent: &evCopy})
			}
		case modelrt.EvStop, modelrt.EvError, modelrt.EvAborted:
			final := ev.Message
			if final == nil {
				final = errorMessage(cfg, ev.Error)
			}
			if addedPartial {
				ctx.Messages[len(ctx.Messages)-1] = *final
			} else {
				ctx.Messages = append(ctx.Messages, *final)
				cp := *final
				emit(Event{Type: EvMessageStart, Message: &cp})
			}
			cp := *final
			emit(Event{Type: EvMessageEnd, Message: &cp})
			return final
		}
	}
	// Stream ended without a terminal event: synthesize an error.
	final := errorMessage(cfg, "provider stream ended without a terminal event")
	if addedPartial {
		ctx.Messages[len(ctx.Messages)-1] = *final
	} else {
		ctx.Messages = append(ctx.Messages, *final)
		cp := *final
		emit(Event{Type: EvMessageStart, Message: &cp})
	}
	cp := *final
	emit(Event{Type: EvMessageEnd, Message: &cp})
	return final
}

func errorMessage(cfg *Config, errMsg string) *messages.AgentMessage {
	m := messages.AgentMessage{}
	v := m.WithAssistant(messages.AssistantMessage{
		API:          string(cfg.Model.API),
		Provider:     cfg.Model.ProviderID,
		Model:        cfg.Model.ID,
		Usage:        messages.Usage{Cost: messages.Cost{}},
		StopReason:   messages.StopError,
		ErrorMessage: errMsg,
	}, nowMs())
	return &v
}

// executeToolCalls runs all tool calls sequentially.
func executeToolCalls(ctx *Context, assistant *messages.AgentMessage, cfg *Config, emit func(Event)) ([]messages.AgentMessage, bool) {
	toolCalls := assistant.AssistantToolCalls()
	var finalized []tools.FinalizedCall
	var results []messages.AgentMessage
	for _, tc := range toolCalls {
		emit(Event{Type: EvToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments})

		var fin tools.FinalizedCall
		if prep := prepareToolCall(ctx, assistant, tc, cfg); prep.immediate {
			fin = tools.FinalizedCall{ToolCall: tc, Result: prep.result, IsError: prep.isError}
		} else {
			executed := executePreparedToolCall(prep, tc, cfg, emit)
			fin = finalizeExecutedToolCall(ctx, assistant, tc, prep.args, executed, cfg)
		}
		emit(Event{Type: EvToolExecutionEnd, ToolCallID: tc.ID, ToolName: tc.Name, Result: &fin.Result, IsError: fin.IsError})
		tr := tools.ToolResultMessage(fin)
		emit(Event{Type: EvMessageStart, Message: &tr})
		emit(Event{Type: EvMessageEnd, Message: &tr})
		finalized = append(finalized, fin)
		results = append(results, tr)
		if cfg.Signal != nil && cfg.Signal.Err() != nil {
			break
		}
	}
	terminate := len(finalized) > 0
	for _, f := range finalized {
		if !f.Result.Terminate {
			terminate = false
			break
		}
	}
	return results, terminate
}

// preparedCall is a tool call ready to execute.
type preparedCall struct {
	tool      *tools.Definition
	call      messages.ToolCall
	args      json.RawMessage
	immediate bool
	result    tools.Result
	isError   bool
}

// prepareToolCall validates args and runs beforeToolCall.
func prepareToolCall(ctx *Context, assistant *messages.AgentMessage, tc messages.ToolCall, cfg *Config) preparedCall {
	tool, ok := findTool(ctx.Tools, tc.Name)
	if !ok {
		return preparedCall{immediate: true, result: tools.ErrorResult(fmt.Errorf("Tool %s not found", tc.Name)), isError: true}
	}
	// Validate arguments.
	argsRaw, err := json.Marshal(tc.Arguments)
	if err != nil {
		argsRaw = []byte("{}")
	}
	if tool.Parameters != nil && len(tool.Parameters) > 0 {
		if verr := schema.ValidateRaw(argsRaw, mustSchema(tool.Parameters)); verr != nil {
			return preparedCall{immediate: true, result: tools.ErrorResult(fmt.Errorf("invalid arguments for %s: %v", tc.Name, verr)), isError: true}
		}
	}
	if cfg.BeforeToolCall != nil {
		argsMap := tc.Arguments
		res, err := cfg.BeforeToolCall(BeforeToolCallContext{
			AssistantMessage: assistant,
			ToolCall:         tc,
			Args:             argsMap,
			Context:          *ctx,
		}, cfg.Signal)
		if err != nil {
			return preparedCall{immediate: true, result: tools.ErrorResult(err), isError: true}
		}
		if res != nil && res.Block {
			reason := "Tool execution was blocked"
			if res.Reason != "" {
				reason = res.Reason
			}
			return preparedCall{immediate: true, result: tools.ErrorResult(fmt.Errorf("%s", reason)), isError: true}
		}
	}
	if cfg.Signal != nil && cfg.Signal.Err() != nil {
		return preparedCall{immediate: true, result: tools.ErrorResult(fmt.Errorf("operation aborted")), isError: true}
	}
	return preparedCall{tool: tool, call: tc, args: argsRaw}
}

// executePreparedToolCall runs the tool with panic recovery.
func executePreparedToolCall(prep preparedCall, tc messages.ToolCall, cfg *Config, emit func(Event)) tools.Result {
	var result tools.Result
	var execErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				execErr = fmt.Errorf("tool %s panicked: %v\n%s", tc.Name, r, debug.Stack())
			}
		}()
		result, execErr = prep.tool.Execute(tc.ID, prep.args, cfg.Signal, func(u tools.Update) {
			emit(Event{
				Type:          EvToolExecutionUpdate,
				ToolCallID:    tc.ID,
				ToolName:      tc.Name,
				Args:          tc.Arguments,
				PartialResult: u,
			})
		})
	}()
	if execErr != nil {
		return tools.ErrorResult(execErr)
	}
	return result
}

// finalizeExecutedToolCall applies afterToolCall overrides.
func finalizeExecutedToolCall(ctx *Context, assistant *messages.AgentMessage, tc messages.ToolCall, args json.RawMessage, result tools.Result, cfg *Config) tools.FinalizedCall {
	isError := result.IsError
	if cfg.AfterToolCall != nil {
		afterResult, err := cfg.AfterToolCall(AfterToolCallContext{
			AssistantMessage: assistant,
			ToolCall:         tc,
			Args:             tc.Arguments,
			Result:           result,
			IsError:          isError,
			Context:          *ctx,
		}, cfg.Signal)
		if err != nil {
			return tools.FinalizedCall{ToolCall: tc, Result: tools.ErrorResult(err), IsError: true}
		}
		if afterResult != nil {
			if afterResult.Content != nil {
				result.Content = afterResult.Content
			}
			if afterResult.Details != nil {
				result.Details = afterResult.Details
			}
			if afterResult.IsError != nil {
				isError = *afterResult.IsError
			}
			if afterResult.Terminate != nil {
				result.Terminate = *afterResult.Terminate
			}
		}
	}
	return tools.FinalizedCall{ToolCall: tc, Result: result, IsError: isError}
}

// failTruncatedToolCalls reports every tool call as failed (a length stop
// means args may be truncated).
func failTruncatedToolCalls(toolCalls []messages.ToolCall, emit func(Event)) []messages.AgentMessage {
	var results []messages.AgentMessage
	for _, tc := range toolCalls {
		emit(Event{Type: EvToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments})
		res := tools.ErrorResult(fmt.Errorf("Tool %q was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.", tc.Name))
		fin := tools.FinalizedCall{ToolCall: tc, Result: res, IsError: true}
		emit(Event{Type: EvToolExecutionEnd, ToolCallID: tc.ID, ToolName: tc.Name, Result: &res, IsError: true})
		tr := tools.ToolResultMessage(fin)
		emit(Event{Type: EvMessageStart, Message: &tr})
		emit(Event{Type: EvMessageEnd, Message: &tr})
		results = append(results, tr)
	}
	return results
}

// --- helpers ---------------------------------------------------------------

func findTool(toolsList []tools.Definition, name string) (*tools.Definition, bool) {
	for i := range toolsList {
		if toolsList[i].Name == name {
			return &toolsList[i], true
		}
	}
	return nil, false
}

func definitionsToSpecs(defs []tools.Definition) []modelrt.ToolSpec {
	out := make([]modelrt.ToolSpec, 0, len(defs))
	for _, d := range defs {
		out = append(out, modelrt.ToolSpec{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
	}
	return out
}

func mustSchema(raw json.RawMessage) schema.Schema {
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return schema.Schema(m)
}

func orEmpty(f func() []messages.AgentMessage) []messages.AgentMessage {
	if f == nil {
		return nil
	}
	return f()
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}
