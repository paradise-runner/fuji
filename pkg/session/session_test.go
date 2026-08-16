package session_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fuji/pkg/config"
	"fuji/pkg/messages"
	"fuji/pkg/modelrt"
	"fuji/pkg/session"
	"fuji/pkg/sessionmgr"
	"fuji/pkg/skills"
	"fuji/pkg/tools"
	"fuji/pkg/tools/all"

	_ "fuji/pkg/modelrt/transport"
)

const assistantTape = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from the tape"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`

func newSession(t *testing.T, srv *httptest.Server, manager *sessionmgr.Manager) *session.Session {
	t.Helper()
	cfg := config.Config{
		Provider: "anthropic", BaseURL: srv.URL, ModelID: "claude-sonnet-4-5",
		ThinkingLevel: "medium", TurnTimeout: 30 * time.Second,
	}
	runtime := modelrt.New(cfg, func(k string) string { return "" }, map[string]string{"anthropic": "test-key"})
	reg := tools.NewRegistry()
	all.RegisterAll(reg, "/tmp", all.BuildResolvers(""), 30*time.Second)
	s, err := session.New(session.Config{
		Cwd:            "/tmp",
		AgentDir:       "",
		Config:         cfg,
		ModelRuntime:   runtime,
		SessionManager: manager,
		Tools:          reg.List(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestPromptEndToEndPersistsSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, assistantTape)
	}))
	defer srv.Close()

	mgr := sessionmgr.InMemory("/tmp", sessionmgr.NewSessionOptions{})
	s := newSession(t, srv, mgr)

	var events []session.Event
	s.Subscribe(func(e session.Event) {
		events = append(events, e)
	})

	if err := s.Prompt("write a test", session.PromptOptions{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !s.IsIdle() {
		t.Error("session not idle after prompt")
	}
	// Session store: initial thinking_level_change + user + assistant.
	entries := mgr.GetEntries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].Type != "thinking_level_change" || entries[0].ThinkingLevel != "medium" {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1].Type != "message" || entries[1].Message.Role != messages.RoleUser {
		t.Errorf("entry 1 = %+v", entries[1])
	}
	if entries[2].Message == nil || entries[2].Message.Role != messages.RoleAssistant {
		t.Errorf("entry 2 = %+v", entries[2])
	}
	if entries[2].Message.Content.TextOf() != "Hello from the tape" {
		t.Errorf("assistant text = %q", entries[2].Message.Content.TextOf())
	}
	// Event sequence includes agent_start and agent_end.
	var sawStart, sawEnd, sawSettled bool
	for _, e := range events {
		switch e.Type {
		case session.EvAgentStart:
			sawStart = true
		case session.EvAgentEnd:
			sawEnd = true
		case session.EvAgentSettled:
			sawSettled = true
		}
	}
	if !sawStart || !sawEnd || !sawSettled {
		t.Errorf("missing lifecycle events: start=%v end=%v settled=%v", sawStart, sawEnd, sawSettled)
	}
	// Working transcript.
	msgs := s.Messages()
	if len(msgs) != 2 {
		t.Errorf("messages = %d", len(msgs))
	}
}

func TestPromptWhileStreamingRequiresBehavior(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":{}}}
`)
		flusher.Flush()
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	mgr := sessionmgr.InMemory("/tmp", sessionmgr.NewSessionOptions{})
	s := newSession(t, srv, mgr)

	done := make(chan error, 1)
	go func() { done <- s.Prompt("first", session.PromptOptions{}) }()
	time.Sleep(100 * time.Millisecond)
	// Second prompt while streaming without behavior → error.
	if err := s.Prompt("second", session.PromptOptions{}); err == nil {
		t.Error("expected error for prompt while streaming")
	}
	// With steer behavior → queued.
	if err := s.Prompt("steer me", session.PromptOptions{StreamingBehavior: "steer"}); err != nil {
		t.Errorf("steer prompt: %v", err)
	}
	<-done
}

func TestAbortMidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":{}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}
`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	mgr := sessionmgr.InMemory("/tmp", sessionmgr.NewSessionOptions{})
	s := newSession(t, srv, mgr)
	var lastAssistant *messages.AgentMessage
	s.Subscribe(func(e session.Event) {
		if e.Type == session.EvMessageEnd && e.Message != nil && e.Message.Role == messages.RoleAssistant {
			lastAssistant = e.Message
		}
	})
	done := make(chan error, 1)
	go func() { done <- s.Prompt("hi", session.PromptOptions{}) }()
	time.Sleep(150 * time.Millisecond)
	if err := s.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	<-done
	if lastAssistant == nil {
		t.Fatal("no assistant message recorded")
	}
	if lastAssistant.StopReason != messages.StopError && lastAssistant.StopReason != messages.StopAborted {
		t.Errorf("stopReason = %q", lastAssistant.StopReason)
	}
}

func TestRetryPolicyOnRateLimit(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, assistantTape)
	}))
	defer srv.Close()

	cfg := config.Config{
		Provider: "anthropic", BaseURL: srv.URL, ModelID: "claude-sonnet-4-5",
		ThinkingLevel: "off", RetryMaxAttempts: 3, RetryBackoff: 10 * time.Millisecond, RetryMaxDelay: 50 * time.Millisecond,
	}
	runtime := modelrt.New(cfg, func(k string) string { return "" }, map[string]string{"anthropic": "k"})
	mgr := sessionmgr.InMemory("/tmp", sessionmgr.NewSessionOptions{})
	s, err := session.New(session.Config{
		Cwd: "/tmp", Config: cfg, ModelRuntime: runtime, SessionManager: mgr,
		Tools: []tools.Definition{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var retries int
	s.Subscribe(func(e session.Event) {
		if e.Type == session.EvAutoRetryStart {
			retries++
		}
	})
	if err := s.Prompt("hi", session.PromptOptions{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if retries != 1 {
		t.Errorf("retries = %d, want 1", retries)
	}
	msgs := s.Messages()
	last := msgs[len(msgs)-1]
	if last.Role != messages.RoleAssistant || last.Content.TextOf() != "Hello from the tape" {
		t.Errorf("final message = %+v", last)
	}
}

func TestExecuteBash(t *testing.T) {
	mgr := sessionmgr.InMemory("/tmp", sessionmgr.NewSessionOptions{})
	cfg := config.Config{Provider: "anthropic", ModelID: "claude-sonnet-4-5", ThinkingLevel: "off", BashTimeout: 10 * time.Second}
	runtime := modelrt.New(cfg, func(k string) string { return "" }, map[string]string{"anthropic": "k"})
	s, err := session.New(session.Config{
		Cwd: "/tmp", Config: cfg, ModelRuntime: runtime, SessionManager: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []string
	res, err := s.ExecuteBash("echo session-bash-works", func(c string) { chunks = append(chunks, c) })
	if err != nil {
		t.Fatalf("ExecuteBash: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v", res.ExitCode)
	}
	if res.Cancelled || res.Truncated {
		t.Errorf("res = %+v", res)
	}
	if len(chunks) == 0 || !containsStr(chunks, "session-bash-works") {
		t.Errorf("chunks = %v", chunks)
	}
	// BashExecutionMessage persisted.
	entries := mgr.GetEntries()
	last := entries[len(entries)-1]
	if last.Message == nil || last.Message.Role != messages.RoleBashExecution {
		t.Errorf("last entry = %+v", last)
	}
}

func containsStr(ss []string, needle string) bool {
	for _, s := range ss {
		if len(s) >= len(needle) {
			for i := 0; i+len(needle) <= len(s); i++ {
				if s[i:i+len(needle)] == needle {
					return true
				}
			}
		}
	}
	return false
}

func TestSetThinkingLevel(t *testing.T) {
	mgr := sessionmgr.InMemory("/tmp", sessionmgr.NewSessionOptions{})
	cfg := config.Config{Provider: "anthropic", ModelID: "claude-sonnet-4-5", ThinkingLevel: "medium"}
	runtime := modelrt.New(cfg, func(k string) string { return "" }, map[string]string{"anthropic": "k"})
	s, err := session.New(session.Config{Cwd: "/tmp", Config: cfg, ModelRuntime: runtime, SessionManager: mgr})
	if err != nil {
		t.Fatal(err)
	}
	s.SetThinkingLevel("high")
	if s.ThinkingLevel() != "high" {
		t.Errorf("thinking = %q", s.ThinkingLevel())
	}
	// New recorded the configured "medium", SetThinkingLevel appends "high".
	entries := mgr.GetEntries()
	if len(entries) != 2 || entries[0].ThinkingLevel != "medium" || entries[1].ThinkingLevel != "high" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestSkillsInSystemPromptAndExpansion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, assistantTape)
	}))
	defer srv.Close()
	mgr := sessionmgr.InMemory("/tmp", sessionmgr.NewSessionOptions{})
	cfg := config.Config{Provider: "anthropic", BaseURL: srv.URL, ModelID: "claude-sonnet-4-5", ThinkingLevel: "off"}
	runtime := modelrt.New(cfg, func(k string) string { return "" }, map[string]string{"anthropic": "k"})
	s, err := session.New(session.Config{
		Cwd: "/tmp", Config: cfg, ModelRuntime: runtime, SessionManager: mgr,
		Skills: []skills.Skill{{Name: "review", Description: "Review code", Content: "REVIEW BODY", FilePath: "/tmp/skills/review/SKILL.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := s.SystemPrompt()
	if !strings.Contains(sp, "Available skills") || !strings.Contains(sp, "review") {
		t.Errorf("system prompt missing skills: %.200q", sp)
	}
	// /skill:name expansion: prompt text becomes the skill block.
	if err := s.Prompt("/skill:review be thorough", session.PromptOptions{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	msgs := s.Messages()
	first := msgs[0]
	if !strings.Contains(first.Content.TextOf(), "REVIEW BODY") || !strings.Contains(first.Content.TextOf(), "be thorough") {
		t.Errorf("expanded prompt = %.200q", first.Content.TextOf())
	}
}
