package compaction

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fuji/pkg/config"
	"fuji/pkg/messages"
	"fuji/pkg/modelrt"
	"fuji/pkg/sessionmgr"

	_ "fuji/pkg/modelrt/transport"
)

func userMsg(text string) sessionmgr.Entry {
	m := messages.UserMessage(messages.Content{Text: text}, 1)
	return sessionmgr.Entry{Type: sessionmgr.EntryMessage, ID: nextID(), Message: &m}
}

func asstMsg(text string, usage messages.Usage) sessionmgr.Entry {
	am := messages.AssistantMessage{
		API: "anthropic", Provider: "anthropic", Model: "claude",
		Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: text}},
		Usage:      usage,
		StopReason: messages.StopStop,
	}
	var m messages.AgentMessage
	v := m.WithAssistant(am, 1)
	return sessionmgr.Entry{Type: sessionmgr.EntryMessage, ID: nextID(), Message: &v}
}

func toolResultMsg() sessionmgr.Entry {
	m := messages.ToolResult("c1", "read", []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "data"}}, false, nil, 1)
	return sessionmgr.Entry{Type: sessionmgr.EntryMessage, ID: nextID(), Message: &m}
}

var idCounter int

func nextID() string {
	idCounter++
	return fmt.Sprintf("id%06d", idCounter)
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(messages.UserMessage(messages.Content{Text: strings.Repeat("a", 40)}, 1)); got != 10 {
		t.Errorf("user tokens = %d, want 10", got)
	}
	am := messages.AssistantMessage{
		Content: []messages.ContentBlock{
			messages.TextContent{Type: messages.ContentText, Text: strings.Repeat("b", 20)},
			messages.ThinkingContent{Type: messages.ContentThinking, Thinking: strings.Repeat("c", 20)},
		},
		Usage: messages.Usage{Cost: messages.Cost{}}, StopReason: messages.StopStop,
	}
	var m messages.AgentMessage
	v := m.WithAssistant(am, 1)
	if got := EstimateTokens(v); got != 10 {
		t.Errorf("assistant tokens = %d, want 10", got)
	}
}

func TestShouldCompact(t *testing.T) {
	s := DefaultSettings()
	if ShouldCompact(10_000, 200_000, s) {
		t.Error("should not compact at 10k")
	}
	if !ShouldCompact(190_000, 200_000, s) {
		t.Error("should compact at 190k (reserve 16k)")
	}
	s2 := s
	s2.Enabled = false
	if ShouldCompact(190_000, 200_000, s2) {
		t.Error("disabled setting must not compact")
	}
}

// TestFindCutPointKeepsRecentTokens builds a long session and checks the cut
// keeps approximately keepRecentTokens.
func TestFindCutPointKeepsRecentTokens(t *testing.T) {
	var entries []sessionmgr.Entry
	for i := 0; i < 50; i++ {
		entries = append(entries, userMsg(fmt.Sprintf("user message number %d with some padding text here", i)))
		entries = append(entries, asstMsg(fmt.Sprintf("assistant response number %d with more content", i), messages.Usage{TotalTokens: 200, Cost: messages.Cost{}}))
		entries = append(entries, toolResultMsg())
	}
	keep := 400
	cut := FindCutPoint(entries, 0, len(entries), keep)
	if cut.FirstKeptEntryIndex <= 0 {
		t.Fatalf("cut index = %d", cut.FirstKeptEntryIndex)
	}
	// Tokens from cut to end should be >= keep (approximately).
	kept := 0
	for i := cut.FirstKeptEntryIndex; i < len(entries); i++ {
		if entries[i].Message != nil {
			kept += EstimateTokens(*entries[i].Message)
		}
	}
	if kept < keep {
		t.Errorf("kept tokens = %d < %d", kept, keep)
	}
	// Cut entry must be a valid cut point (message, not toolResult).
	if !validCutPoint(entries[cut.FirstKeptEntryIndex]) {
		t.Errorf("cut at non-valid point: %+v", entries[cut.FirstKeptEntryIndex])
	}
}

// TestFindCutPointSplitsTurn: a cut in the middle of a turn splits at the
// user message that started it.
func TestFindCutPointSplitsTurn(t *testing.T) {
	var entries []sessionmgr.Entry
	for i := 0; i < 40; i++ {
		entries = append(entries, userMsg(fmt.Sprintf("user %d", i)))
		entries = append(entries, asstMsg(fmt.Sprintf("asst %d", i), messages.Usage{TotalTokens: 100, Cost: messages.Cost{}}))
		entries = append(entries, toolResultMsg())
	}
	// Huge keepRecent so the cut lands inside the last turn.
	cut := FindCutPoint(entries, 0, len(entries), 1_000_000)
	if cut.FirstKeptEntryIndex >= len(entries) {
		t.Fatalf("cut out of range: %d", cut.FirstKeptEntryIndex)
	}
	last := entries[cut.FirstKeptEntryIndex]
	// If we cut at a toolResult, it must have split at the turn start.
	if last.Type == sessionmgr.EntryMessage && last.Message != nil && last.Message.Role == messages.RoleToolResult {
		if !cut.IsSplitTurn {
			t.Errorf("expected split turn for toolResult cut: %+v", cut)
		}
		if cut.TurnStartIndex < 0 {
			t.Error("turnStartIndex missing")
		}
	}
}

// TestPrepareAndGenerateSummary runs the full prepare→summarize flow against
// a tape server.
func TestPrepareAndGenerateSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"## Goal\nSummarize the work done.","cache_creation_input_tokens":0}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	defer srv.Close()

	cfg := config.Config{Provider: "anthropic", BaseURL: srv.URL, ModelID: "claude-sonnet-4-5"}
	runtime := modelrt.New(cfg, func(k string) string { return "" }, map[string]string{"anthropic": "k"})
	model, _ := runtime.Model("anthropic", "claude-sonnet-4-5")

	var entries []sessionmgr.Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, userMsg(fmt.Sprintf("user turn %d: do something useful", i)))
		entries = append(entries, asstMsg(fmt.Sprintf("done turn %d with results", i), messages.Usage{TotalTokens: 150, Cost: messages.Cost{}}))
	}
	prep, err := PrepareCompaction(entries, Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 200})
	if err != nil || prep == nil {
		t.Fatalf("PrepareCompaction: %v %v", prep, err)
	}
	if prep.FirstKeptEntryID == "" || len(prep.MessagesToSummarize) == 0 {
		t.Fatalf("prep = %+v", prep)
	}
	summary, usage, err := GenerateSummary(context.Background(), model, runtime.Stream, prep, "", "off")
	if err != nil {
		t.Fatalf("GenerateSummary: %v", err)
	}
	if !strings.Contains(summary, "## Goal") {
		t.Errorf("summary = %q", summary)
	}
	if usage.Output == 0 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestPrepareCompactionSkipsWhenLastIsCompaction(t *testing.T) {
	entries := []sessionmgr.Entry{
		userMsg("hi"),
		asstMsg("yo", messages.Usage{TotalTokens: 10, Cost: messages.Cost{}}),
		{Type: sessionmgr.EntryCompaction, ID: "c1", Summary: "prev"},
	}
	prep, err := PrepareCompaction(entries, Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 200})
	if err != nil || prep != nil {
		t.Fatalf("expected nil prep: %v %v", prep, err)
	}
}
