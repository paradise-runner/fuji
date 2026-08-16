package modelrt

import (
	"context"
	"time"

	"fuji/pkg/messages"
)

// EventType identifies a stream event.
type EventType string

// Stream event types.
const (
	EvTextStart     EventType = "text_start"
	EvTextDelta     EventType = "text_delta"
	EvTextEnd       EventType = "text_end"
	EvThinkingStart EventType = "thinking_start"
	EvThinkingDelta EventType = "thinking_delta"
	EvThinkingEnd   EventType = "thinking_end"
	EvToolCallStart EventType = "toolcall_start"
	EvToolCallDelta EventType = "toolcall_delta"
	EvToolCallEnd   EventType = "toolcall_end"
	EvStop          EventType = "stop"    // final message: reason stop|length|toolUse
	EvError         EventType = "error"   // final message: reason error
	EvAborted       EventType = "aborted" // final message: reason aborted
)

// StreamEvent is one event from a provider stream. Delta events carry a
// snapshot of the partial message; terminal events (stop/error/aborted)
// carry the final message.
type StreamEvent struct {
	Type     EventType
	Index    int                    // content block index for delta events
	Delta    string                 // text/thinking/tool-call-args delta
	ToolCall *messages.ToolCall     // for toolcall events (accumulated)
	Message  *messages.AgentMessage // partial snapshot on deltas; final on terminal
	Error    string
}

// Stream is a channel of StreamEvents. The stream is single-shot: after the
// terminal event the channel is closed. Events are produced on a background
// goroutine; consumers must range until close.
type Stream struct {
	events chan StreamEvent
}

// NewStream creates a stream with a buffered event channel.
func NewStream() *Stream {
	return &Stream{events: make(chan StreamEvent, 64)}
}

// Events returns the event channel (receive-only for consumers).
func (s *Stream) Events() <-chan StreamEvent { return s.events }

// Send emits an event (used by transports).
func (s *Stream) Send(ev StreamEvent) { s.events <- ev }

// Close closes the event channel.
func (s *Stream) Close() { close(s.events) }

// Next blocks for the next event.
func (s *Stream) Next() (StreamEvent, bool) {
	ev, ok := <-s.events
	return ev, ok
}

// Collect ranges the stream, invoking fn per event, returning the final
// message (or nil if the stream ended without a terminal event).
func (s *Stream) Collect(fn func(StreamEvent)) *messages.AgentMessage {
	var final *messages.AgentMessage
	for ev := range s.events {
		if fn != nil {
			fn(ev)
		}
		if ev.Type == EvStop || ev.Type == EvError || ev.Type == EvAborted {
			final = ev.Message
		}
	}
	return final
}

// errorStream returns a stream that immediately emits an error event.
func errorStream(err error) *Stream {
	s := NewStream()
	s.events <- StreamEvent{
		Type:    EvError,
		Error:   err.Error(),
		Message: finalErrorMessage(err),
	}
	close(s.events)
	return s
}

func finalErrorMessage(err error) *messages.AgentMessage {
	ts := time.Now().UnixMilli()
	m := messages.AgentMessage{}
	v := m.WithAssistant(messages.AssistantMessage{
		API:          "unknown",
		Provider:     "unknown",
		Model:        "unknown",
		Usage:        messages.Usage{Cost: messages.Cost{}},
		StopReason:   messages.StopError,
		ErrorMessage: err.Error(),
	}, ts)
	return &v
}

// contextFor derives a request context honoring the per-turn timeout.
func contextFor(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}
