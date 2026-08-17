// Package session implements the Session lifecycle: it wires the agent loop,
// session store, model runtime, and tools together (core-spec §4.1, D1).
// Prompt/Steer/FollowUp/Abort drive a single-threaded loop; persistence
// happens on every message_end; retries follow the auto-retry policy.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"fuji/pkg/config"
	"fuji/pkg/loop"
	"fuji/pkg/messages"
	"fuji/pkg/modelrt"
	"fuji/pkg/prompt"
	"fuji/pkg/sessionmgr"
	"fuji/pkg/skills"
	"fuji/pkg/templates"
	"fuji/pkg/tools"
)

// State is the session lifecycle state.
type State string

// States.
const (
	StateIdle       State = "idle"
	StateStreaming  State = "streaming"
	StateExecuting  State = "executing"
	StateCompacting State = "compacting"
	StateRetrying   State = "retrying"
)

// EventType is the session event surface (core-spec §4.1).
type EventType string

// Session event types.
const (
	EvAgentStart           EventType = "agent_start"
	EvAgentEnd             EventType = "agent_end"
	EvTurnStart            EventType = "turn_start"
	EvTurnEnd              EventType = "turn_end"
	EvMessageStart         EventType = "message_start"
	EvMessageUpdate        EventType = "message_update"
	EvMessageEnd           EventType = "message_end"
	EvToolExecutionStart   EventType = "tool_execution_start"
	EvToolExecutionUpdate  EventType = "tool_execution_update"
	EvToolExecutionEnd     EventType = "tool_execution_end"
	EvAgentSettled         EventType = "agent_settled"
	EvQueueUpdate          EventType = "queue_update"
	EvCompactionStart      EventType = "compaction_start"
	EvCompactionEnd        EventType = "compaction_end"
	EvEntryAppended        EventType = "entry_appended"
	EvSessionInfoChanged   EventType = "session_info_changed"
	EvThinkingLevelChanged EventType = "thinking_level_changed"
	EvAutoRetryStart       EventType = "auto_retry_start"
	EvAutoRetryEnd         EventType = "auto_retry_end"
	EvBashExecutionUpdate  EventType = "bash_execution_update"
)

// Event is the session event union.
type Event struct {
	Type EventType

	// agent_end
	Messages  []messages.AgentMessage
	WillRetry bool

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

	// queue_update
	Steering []messages.AgentMessage
	FollowUp []messages.AgentMessage

	// compaction_*
	CompactionReason  string
	CompactionResult  *CompactionResult
	CompactionAborted bool
	CompactionError   string

	// entry_appended
	Entry any

	// auto_retry_*
	Attempt      int
	MaxAttempts  int
	DelayMs      int64
	ErrorMessage string
	RetrySuccess bool
	FinalError   string

	// bash_execution_update
	BashID    string
	BashDelta string

	// thinking_level_changed
	ThinkingLevel string
}

// CompactionResult mirrors the compaction outcome.
type CompactionResult struct {
	Summary          string
	FirstKeptEntryID string
	TokensBefore     int
	EntryID          string
}

// PromptOptions controls Prompt behavior.
type PromptOptions struct {
	// StreamingBehavior: "steer" queues mid-run, "followUp" queues for the
	// next natural stop. Empty means error when streaming.
	StreamingBehavior     string
	ExpandPromptTemplates bool // P7
	Skills                []skills.Skill
}

// Config wires a Session.
type Config struct {
	Cwd            string
	AgentDir       string
	Config         config.Config
	ModelRuntime   *modelrt.Runtime
	SessionManager *sessionmgr.Manager
	Skills         []skills.Skill
	Templates      []templates.Template
	Tools          []tools.Definition
	AllowedTools   []string
	ExcludedTools  []string
	BasePrompt     string // base system-prompt text; empty → embedded default
	SystemPrompt   string // optional full override; built when empty
}

// Session is the agent session.
type Session struct {
	cfg Config

	ctx    context.Context
	cancel context.CancelFunc

	mu                       sync.Mutex
	state                    State
	messages                 []messages.AgentMessage
	model                    modelrt.Model
	thinking                 string
	retryAttempt             int
	pendingStreamingBehavior string

	steerQueue    []messages.AgentMessage
	followUpQueue []messages.AgentMessage

	subscribers []func(Event)
	settled     chan struct{}

	compactionRunning bool
	abortCompaction   context.CancelFunc

	systemPrompt string
	tools        []tools.Definition
}

// New creates a Session.
func New(cfg Config) (*Session, error) {
	if cfg.Cwd == "" {
		cfg.Cwd = "."
	}
	if cfg.ModelRuntime == nil {
		return nil, fmt.Errorf("session: ModelRuntime is required")
	}
	if cfg.SessionManager == nil {
		return nil, fmt.Errorf("session: SessionManager is required")
	}
	model, ok := cfg.ModelRuntime.Model(cfg.Config.Provider, cfg.Config.ModelID)
	if !ok {
		return nil, fmt.Errorf("session: unknown model %s/%s", cfg.Config.Provider, cfg.Config.ModelID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
		state:    StateIdle,
		model:    model,
		thinking: cfg.Config.ThinkingLevel,
		settled:  make(chan struct{}),
	}
	if s.thinking == "" {
		s.thinking = config.DefaultThinkingLevel
	}
	s.tools = cfg.Tools
	if cfg.Config.ToolTimeout > 0 {
		// Apply per-tool timeout budgets (core-spec §5.3): wrap each tool's
		// Execute with a timeout context derived from the session context.
		for i := range s.tools {
			s.tools[i] = wrapToolTimeout(s.tools[i], cfg.Config.ToolTimeout)
		}
	}
	s.systemPrompt = cfg.SystemPrompt
	if s.systemPrompt == "" {
		base := cfg.BasePrompt
		if base == "" {
			base = prompt.DefaultBase()
		}
		s.systemPrompt = buildSystemPrompt(base, cfg.Tools, cfg.Skills)
	}
	sc := cfg.SessionManager.BuildSessionContext()
	s.messages = sc.Messages
	if sc.ThinkingLevel != "off" {
		s.thinking = sc.ThinkingLevel
	}
	if sc.Model != nil {
		if m, ok := cfg.ModelRuntime.Model(sc.Model.Provider, sc.Model.ModelID); ok {
			s.model = m
		}
	}
	// Record the configured thinking level in the session store so context
	// building reflects it (getSessionContextSettings defaults to "off"
	// without a thinking_level_change entry).
	if len(sc.Messages) == 0 && s.thinking != "off" && cfg.SessionManager.GetLeafEntry() == nil {
		cfg.SessionManager.AppendThinkingLevelChange(s.thinking)
	}
	return s, nil
}

// wrapToolTimeout derives a timeout context for every tool Execute call.
func wrapToolTimeout(def tools.Definition, timeout time.Duration) tools.Definition {
	orig := def.Execute
	def.Execute = func(callID string, params json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
		tctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return orig(callID, params, tctx, onUpdate)
	}
	return def
}

// buildSystemPrompt renders the base + tools + skills prompt.
func buildSystemPrompt(base string, toolsList []tools.Definition, sk []skills.Skill) string {
	var b strings.Builder
	b.WriteString(base)
	if !strings.HasSuffix(strings.TrimRight(base, " \t\n"), "\n") {
		b.WriteString("\n")
	}
	if len(toolsList) > 0 {
		b.WriteString("\n# Available tools\n\n")
		for _, t := range toolsList {
			b.WriteString("## " + t.Name + "\n\n")
			if t.Description != "" {
				b.WriteString(t.Description + "\n\n")
			}
			if len(t.Parameters) > 0 {
				var params map[string]any
				if err := json.Unmarshal(t.Parameters, &params); err == nil {
					props, _ := params["properties"].(map[string]any)
					if len(props) > 0 {
						b.WriteString("Parameters:\n")
						names := make([]string, 0, len(props))
						for k := range props {
							names = append(names, k)
						}
						sort.Strings(names)
						for _, k := range names {
							p, _ := props[k].(map[string]any)
							desc, _ := p["description"].(string)
							b.WriteString("- " + k)
							if desc != "" {
								b.WriteString(": " + desc)
							}
							b.WriteString("\n")
						}
						b.WriteString("\n")
					}
				}
			}
			if t.PromptSnippet != "" {
				b.WriteString(t.PromptSnippet + "\n\n")
			}
			for _, g := range t.PromptGuidelines {
				b.WriteString("- " + g + "\n")
			}
		}
	}
	if skillsPrompt := skills.FormatSkillsForPrompt(sk); skillsPrompt != "" {
		b.WriteString("\n" + skillsPrompt + "\n")
	}
	return b.String()
}

// --- subscription ----------------------------------------------------------

// Subscribe registers an event listener; returns unsubscribe.
func (s *Session) Subscribe(fn func(Event)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers = append(s.subscribers, fn)
	i := len(s.subscribers) - 1
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.subscribers = append(s.subscribers[:i], s.subscribers[i+1:]...)
	}
}

func (s *Session) emit(e Event) {
	s.mu.Lock()
	subs := append([]func(Event){}, s.subscribers...)
	s.mu.Unlock()
	for _, fn := range subs {
		fn(e)
	}
}

// --- state accessors -------------------------------------------------------

// State returns the current state.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// IsIdle reports whether the session is idle.
func (s *Session) IsIdle() bool { return s.State() == StateIdle }

// IsStreaming reports whether the agent loop is running.
func (s *Session) IsStreaming() bool {
	st := s.State()
	return st == StateStreaming || st == StateExecuting || st == StateRetrying
}

// IsCompacting reports whether compaction is in progress.
func (s *Session) IsCompacting() bool { return s.State() == StateCompacting }

// IsRetrying reports whether a retry is in progress.
func (s *Session) IsRetrying() bool { return s.State() == StateRetrying }

// RetryAttempt returns the current retry attempt.
func (s *Session) RetryAttempt() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retryAttempt
}

// Model returns the active model.
func (s *Session) Model() modelrt.Model {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

// ThinkingLevel returns the active thinking level.
func (s *Session) ThinkingLevel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thinking
}

// SessionFile returns the session file path.
func (s *Session) SessionFile() string { return s.cfg.SessionManager.GetSessionFile() }

// SessionID returns the session id.
func (s *Session) SessionID() string { return s.cfg.SessionManager.GetSessionID() }

// Messages returns a copy of the working transcript.
func (s *Session) Messages() []messages.AgentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]messages.AgentMessage{}, s.messages...)
}

// SystemPrompt returns the system prompt.
func (s *Session) SystemPrompt() string { return s.systemPrompt }

// ActiveToolNames returns the tool names.
func (s *Session) ActiveToolNames() []string {
	out := make([]string, len(s.tools))
	for i, t := range s.tools {
		out[i] = t.Name
	}
	return out
}

// AllTools returns the tools.
func (s *Session) AllTools() []tools.Definition { return s.tools }

// SetThinkingLevel sets the thinking level and records a change.
func (s *Session) SetThinkingLevel(l string) {
	s.mu.Lock()
	s.thinking = l
	s.mu.Unlock()
	s.cfg.SessionManager.AppendThinkingLevelChange(l)
	s.emit(Event{Type: EvThinkingLevelChanged, ThinkingLevel: l})
	s.emit(Event{Type: EvSessionInfoChanged})
}

// SetModel swaps the active model and records a change.
func (s *Session) SetModel(m modelrt.Model) error {
	s.mu.Lock()
	if s.IsStreaming() {
		s.mu.Unlock()
		return fmt.Errorf("cannot change model while streaming")
	}
	s.model = m
	s.mu.Unlock()
	s.cfg.SessionManager.AppendModelChange(m.ProviderID, m.ID)
	s.emit(Event{Type: EvSessionInfoChanged})
	return nil
}

// --- prompting -------------------------------------------------------------

// Prompt starts an agent run. Blocks until the run completes (or aborts).
func (s *Session) Prompt(text string, opts PromptOptions) error {
	s.mu.Lock()
	st := s.state
	streaming := st == StateStreaming || st == StateExecuting || st == StateRetrying
	if streaming {
		behavior := opts.StreamingBehavior
		if behavior == "" {
			s.mu.Unlock()
			return fmt.Errorf("cannot prompt while streaming without a StreamingBehavior (use Steer or FollowUp)")
		}
		s.mu.Unlock()
		if behavior == "steer" {
			return s.Steer(text)
		}
		return s.FollowUp(text)
	}
	s.mu.Unlock()

	// Preflight validation.
	if err := s.cfg.ModelRuntime.CheckAuth(s.model.ProviderID); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	// Expand /skill:name and /template commands.
	text = s.expandPrompt(text, opts)

	msg := messages.UserMessage(messages.Content{Text: text}, time.Now().UnixMilli())

	s.mu.Lock()
	s.state = StateStreaming
	s.settled = make(chan struct{})
	s.mu.Unlock()

	s.runAgent([]messages.AgentMessage{msg}, false)
	s.waitSettled()
	return nil
}

// expandPrompt applies skill-command and prompt-template expansion (template
// expansion defaults on).
func (s *Session) expandPrompt(text string, opts PromptOptions) string {
	if text == "" {
		return text
	}
	if expanded, ok := skills.ExpandSkillCommand(text, s.cfg.Skills); ok {
		return expanded
	}
	if opts.ExpandPromptTemplates || strings.HasPrefix(text, "/") {
		return templates.ExpandPromptTemplate(text, s.cfg.Templates)
	}
	return text
}

// Steer queues a message for delivery after the current turn's tool calls.

// Steer queues a message for delivery after the current turn's tool calls.
func (s *Session) Steer(text string) error {
	msg := messages.UserMessage(messages.Content{Text: text}, time.Now().UnixMilli())
	s.mu.Lock()
	s.steerQueue = append(s.steerQueue, msg)
	steer := append([]messages.AgentMessage{}, s.steerQueue...)
	s.mu.Unlock()
	s.emit(Event{Type: EvQueueUpdate, Steering: steer})
	return nil
}

// FollowUp queues a message delivered only when the agent would otherwise stop.
func (s *Session) FollowUp(text string) error {
	msg := messages.UserMessage(messages.Content{Text: text}, time.Now().UnixMilli())
	s.mu.Lock()
	s.followUpQueue = append(s.followUpQueue, msg)
	follow := append([]messages.AgentMessage{}, s.followUpQueue...)
	s.mu.Unlock()
	s.emit(Event{Type: EvQueueUpdate, FollowUp: follow})
	return nil
}

// Abort cancels the in-flight operation and waits for idle.
func (s *Session) Abort() error {
	s.cancel()
	s.waitSettled()
	return nil
}

// WaitForIdle blocks until the session is idle.
func (s *Session) WaitForIdle() error {
	s.waitSettled()
	return nil
}

func (s *Session) waitSettled() {
	s.mu.Lock()
	ch := s.settled
	s.mu.Unlock()
	if ch == nil {
		return
	}
	<-ch
}

// Dispose removes listeners and cancels work.
func (s *Session) Dispose() {
	s.cancel()
	s.mu.Lock()
	s.subscribers = nil
	s.mu.Unlock()
}

// --- agent run -------------------------------------------------------------

// runAgent builds the loop config and runs it on a goroutine; the caller
// waits via waitSettled. RunAgent itself is single-threaded (D13).
func (s *Session) runAgent(prompts []messages.AgentMessage, isContinue bool) {
	priorCount := 0
	s.mu.Lock()
	priorCount = len(s.messages) - len(prompts)
	s.mu.Unlock()
	go func() {
		defer func() {
			// Subscribers settle before idle (agent_settled precedes the
			// state flip to idle).
			s.emit(Event{Type: EvAgentSettled})
			s.mu.Lock()
			s.state = StateIdle
			close(s.settled)
			s.mu.Unlock()
		}()
		s.runAgentLoop(prompts, isContinue, priorCount)
	}()
}

func (s *Session) runAgentLoop(prompts []messages.AgentMessage, isContinue bool, priorCount int) {
	cfg := loop.Config{
		Model:         s.model,
		ThinkingLevel: s.thinking,
		SystemPrompt:  s.systemPrompt,
		ConvertToLLM:  modelrt.ConvertToLLM,
		GetSteeringMessages: func() []messages.AgentMessage {
			s.mu.Lock()
			defer s.mu.Unlock()
			out := s.steerQueue
			s.steerQueue = nil
			return out
		},
		GetFollowUpMessages: func() []messages.AgentMessage {
			s.mu.Lock()
			defer s.mu.Unlock()
			out := s.followUpQueue
			s.followUpQueue = nil
			return out
		},
		Signal:              s.ctx,
		StreamFn:            s.retryingStreamFn,
		ShouldStopAfterTurn: s.compactionShouldStopHook,
		AppURL:              s.cfg.Config.AppURL,
		AppTitle:            s.cfg.Config.AppTitle,
		AppCategories:       s.cfg.Config.AppCategories,
	}
	s.mu.Lock()
	var contextMsgs []messages.AgentMessage
	if priorCount >= 0 && priorCount <= len(s.messages) {
		contextMsgs = append([]messages.AgentMessage{}, s.messages[:priorCount]...)
	} else {
		contextMsgs = append([]messages.AgentMessage{}, s.messages...)
	}
	s.mu.Unlock()
	loopCtx := loop.Context{
		SystemPrompt: s.systemPrompt,
		Messages:     contextMsgs,
		Tools:        s.tools,
	}

	emit := func(e loop.Event) {
		s.handleLoopEvent(e)
	}

	if isContinue {
		_, err := loop.Continue(loopCtx, cfg, emit)
		if err != nil {
			s.emit(Event{Type: EvMessageEnd, Message: errorAgentMessage(s.model, err.Error())})
		}
	} else {
		loop.Run(prompts, loopCtx, cfg, emit)
	}
}

// handleLoopEvent maps loop events to session events + persistence.
func (s *Session) handleLoopEvent(e loop.Event) {
	switch e.Type {
	case loop.EvAgentStart:
		s.emit(Event{Type: EvAgentStart})
	case loop.EvAgentEnd:
		s.emit(Event{Type: EvAgentEnd, Messages: e.Messages})
	case loop.EvTurnStart:
		s.mu.Lock()
		s.state = StateStreaming
		s.mu.Unlock()
		s.emit(Event{Type: EvTurnStart})
	case loop.EvTurnEnd:
		s.mu.Lock()
		s.state = StateIdle
		s.mu.Unlock()
		s.emit(Event{Type: EvTurnEnd, Message: e.Message, ToolResults: e.ToolResults})
	case loop.EvMessageStart:
		if e.Message != nil {
			s.emit(Event{Type: EvMessageStart, Message: e.Message})
		}
	case loop.EvMessageUpdate:
		s.emit(Event{Type: EvMessageUpdate, Message: e.Message, AssistantMessageEvent: e.AssistantMessageEvent})
	case loop.EvMessageEnd:
		if e.Message != nil {
			s.persistMessage(*e.Message)
			s.emit(Event{Type: EvMessageEnd, Message: e.Message})
		}
	case loop.EvToolExecutionStart:
		s.mu.Lock()
		s.state = StateExecuting
		s.mu.Unlock()
		s.emit(Event{Type: EvToolExecutionStart, ToolCallID: e.ToolCallID, ToolName: e.ToolName, Args: e.Args})
	case loop.EvToolExecutionUpdate:
		// Stream bash deltas when the update carries them.
		if u, ok := e.PartialResult.(tools.Update); ok && u.BashDelta != "" {
			s.emit(Event{Type: EvBashExecutionUpdate, BashID: e.ToolCallID, BashDelta: u.BashDelta})
		}
		s.emit(Event{Type: EvToolExecutionUpdate, ToolCallID: e.ToolCallID, ToolName: e.ToolName, Args: e.Args, PartialResult: e.PartialResult})
	case loop.EvToolExecutionEnd:
		s.mu.Lock()
		s.state = StateStreaming
		s.mu.Unlock()
		s.emit(Event{Type: EvToolExecutionEnd, ToolCallID: e.ToolCallID, ToolName: e.ToolName, Result: e.Result, IsError: e.IsError})
	}
}

// persistMessage appends a message to the session store on message_end.
func (s *Session) persistMessage(msg messages.AgentMessage) {
	entryID := s.cfg.SessionManager.AppendMessage(msg)
	s.mu.Lock()
	s.messages = append(s.messages, msg)
	s.mu.Unlock()
	s.emit(Event{Type: EvEntryAppended, Entry: entryID})
}

// --- retry policy ----------------------------------------------------------

// retryingStreamFn wraps the runtime stream with the auto-retry policy.
func (s *Session) retryingStreamFn(model modelrt.Model, ctx context.Context, c modelrt.Context, opts modelrt.StreamOptions) *modelrt.Stream {
	retryable := func(errMsg string) bool {
		low := strings.ToLower(errMsg)
		switch {
		case strings.Contains(low, "authentication failed"),
			strings.Contains(low, "request rejected"),
			strings.Contains(low, "not found"),
			strings.Contains(low, "context") && strings.Contains(low, "length") || strings.Contains(low, "context window"):
			// auth / 4xx validation / context-overflow → not retryable
			if strings.Contains(low, "context") {
				return false
			}
			return false
		case strings.Contains(low, "rate limited"),
			strings.Contains(low, "overloaded"),
			strings.Contains(low, "provider error (5"),
			strings.Contains(low, "stream ended unexpectedly"),
			strings.Contains(low, "timed out"):
			return true
		}
		return false
	}

	maxAttempts := s.cfg.Config.RetryMaxAttempts
	backoff := s.cfg.Config.RetryBackoff
	maxDelay := s.cfg.Config.RetryMaxDelay

	// Single attempt per stream; retries loop at the stream level.
	attempt := 0
	for {
		attempt++
		stream := s.cfg.ModelRuntime.Stream(model, ctx, c, opts)
		// Peek: if the first event is a retryable error, retry.
		ev, ok := <-stream.Events()
		if !ok {
			return stream
		}
		if ev.Type != modelrt.EvError {
			// Re-emit the peeked event and return the stream.
			out := modelrt.NewStream()
			go func() {
				defer out.Close()
				out.Send(ev)
				for e := range stream.Events() {
					out.Send(e)
				}
			}()
			return out
		}
		if !retryable(ev.Error) || attempt > maxAttempts || ctx.Err() != nil {
			return errorStreamWith(ev)
		}
		delay := backoff * time.Duration(1<<(attempt-1))
		if delay > maxDelay {
			delay = maxDelay
		}
		s.mu.Lock()
		s.state = StateRetrying
		s.retryAttempt = attempt
		s.mu.Unlock()
		s.emit(Event{Type: EvAutoRetryStart, Attempt: attempt, MaxAttempts: maxAttempts, DelayMs: delay.Milliseconds(), ErrorMessage: ev.Error})
		select {
		case <-ctx.Done():
			return errorStreamWith(ev)
		case <-time.After(delay):
		}
		s.emit(Event{Type: EvAutoRetryEnd, Attempt: attempt, RetrySuccess: false})
	}
}

func errorStreamWith(ev modelrt.StreamEvent) *modelrt.Stream {
	out := modelrt.NewStream()
	go func() {
		defer out.Close()
		out.Send(ev)
	}()
	return out
}

// retryable check for the session-level "willRetry" flag on agent_end.
func (s *Session) willRetry() bool { return false }

func errorAgentMessage(model modelrt.Model, errMsg string) *messages.AgentMessage {
	m := messages.AgentMessage{}
	v := m.WithAssistant(messages.AssistantMessage{
		API: string(model.API), Provider: model.ProviderID, Model: model.ID,
		Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopError, ErrorMessage: errMsg,
	}, time.Now().UnixMilli())
	return &v
}
