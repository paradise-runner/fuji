package messages

import (
	"encoding/json"
	"testing"
)

// TestRoundTripSessionMessage verifies byte-compatible round-trip of a real
// assistant message (from a recorded 0.84.x session).
func TestRoundTripSessionMessage(t *testing.T) {
	raw := `{"role":"assistant","content":[{"type":"thinking","thinking":"Let me look.","thinkingSignature":"reasoning_content"},{"type":"toolCall","id":"call_00_x","name":"bash","arguments":{"command":"ls"}}],"api":"openai-completions","provider":"deepseek","model":"deepseek-v4-flash","responseId":"chatcmpl-123","stopReason":"toolUse","timestamp":1786237125312,"usage":{"input":2932,"output":182,"cacheRead":0,"cacheWrite":0,"reasoning":64,"totalTokens":3114,"cost":{"input":0.00041048,"output":5.096e-05,"cacheRead":0,"cacheWrite":0,"total":0.00046144}}}`
	var m AgentMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Role != RoleAssistant {
		t.Fatalf("role = %q", m.Role)
	}
	if m.StopReason != StopToolUse {
		t.Errorf("stopReason = %q", m.StopReason)
	}
	if m.Usage == nil || m.Usage.Input != 2932 || m.Usage.Reasoning == nil || *m.Usage.Reasoning != 64 {
		t.Errorf("usage = %+v", m.Usage)
	}
	if len(m.Content.Blocks) != 2 {
		t.Fatalf("blocks = %d", len(m.Content.Blocks))
	}
	tc, ok := m.Content.Blocks[1].(ToolCall)
	if !ok {
		t.Fatalf("block[1] = %T", m.Content.Blocks[1])
	}
	if tc.Name != "bash" || tc.Arguments["command"] != "ls" {
		t.Errorf("tool call = %+v", tc)
	}
	// Semantic round trip: JSON objects are order-insensitive and float
	// formatting differs between Go and JS encoders; the parity harness
	// compares structurally (core-spec §10). Compare decoded trees.
	out, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var gotTree, wantTree any
	if err := json.Unmarshal(out, &gotTree); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &wantTree); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.MarshalIndent(gotTree, "", " ")
	wantJSON, _ := json.MarshalIndent(wantTree, "", " ")
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("round-trip mismatch:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestToolResultRoundTrip(t *testing.T) {
	raw := `{"role":"toolResult","toolCallId":"call_0","toolName":"read","content":[{"type":"text","text":"hi"}],"isError":true,"timestamp":100}`
	var m AgentMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if !m.IsError || m.ToolName != "read" {
		t.Errorf("got %+v", m)
	}
	out, _ := json.Marshal(&m)
	var gotTree, wantTree any
	_ = json.Unmarshal(out, &gotTree)
	_ = json.Unmarshal([]byte(raw), &wantTree)
	gotJSON, _ := json.Marshal(gotTree)
	wantJSON, _ := json.Marshal(wantTree)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("round-trip: %s vs %s", gotJSON, wantJSON)
	}
}

func TestUserMessageStringContent(t *testing.T) {
	raw := `{"role":"user","content":"hello","timestamp":1}`
	var m AgentMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if !m.Content.IsText() || m.Content.TextOf() != "hello" {
		t.Errorf("content = %+v", m.Content)
	}
	out, _ := json.Marshal(&m)
	var gotTree, wantTree any
	_ = json.Unmarshal(out, &gotTree)
	_ = json.Unmarshal([]byte(raw), &wantTree)
	gotJSON, _ := json.Marshal(gotTree)
	wantJSON, _ := json.Marshal(wantTree)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("round-trip: %s vs %s", gotJSON, wantJSON)
	}
}

func TestContentTextOf(t *testing.T) {
	c := Content{Blocks: []ContentBlock{
		TextContent{Type: ContentText, Text: "a"},
		ThinkingContent{Type: ContentThinking, Thinking: "b"},
	}}
	if got := c.TextOf(); got != "ab" {
		t.Errorf("TextOf = %q", got)
	}
}

func TestAssistantHelpers(t *testing.T) {
	a := AssistantMessage{
		API: "anthropic", Provider: "anthropic", Model: "claude",
		Content: []ContentBlock{
			TextContent{Type: ContentText, Text: "hi"},
			ToolCall{Type: ContentToolCall, ID: "tc1", Name: "read", Arguments: map[string]any{"path": "x"}},
		},
		StopReason: StopToolUse,
	}
	if !a.HasToolCalls() {
		t.Error("expected tool calls")
	}
	tcs := a.ToolCalls()
	if len(tcs) != 1 || tcs[0].Name != "read" {
		t.Errorf("tool calls = %+v", tcs)
	}
	if a.Text() != "hi" {
		t.Errorf("text = %q", a.Text())
	}
}
