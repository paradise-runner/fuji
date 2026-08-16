package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestParseAnthropicResponse parses a recorded Anthropic-style SSE stream
// (core-spec P0b exit criteria: "SSE parse of a recorded Anthropic response").
func TestParseAnthropicResponse(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`
	rd := NewReader(strings.NewReader(stream))
	var types, datas []string
	for {
		ev, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		datas = append(datas, ev.Data)
	}
	wantTypes := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
		t.Errorf("event types = %v, want %v", types, wantTypes)
	}
	if !strings.Contains(datas[2], `"text":"Hello"`) {
		t.Errorf("delta data = %q", datas[2])
	}
	if !strings.Contains(datas[3], `"text":" world"`) {
		t.Errorf("delta2 data = %q", datas[3])
	}
}

func TestMultilineData(t *testing.T) {
	stream := "data: line1\ndata: line2\n\n"
	rd := NewReader(strings.NewReader(stream))
	ev, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Data != "line1\nline2" {
		t.Errorf("data = %q", ev.Data)
	}
	if ev.Type != "message" {
		t.Errorf("default type = %q", ev.Type)
	}
}

func TestCommentAndEmptyEventSkipped(t *testing.T) {
	stream := ": comment\n\ndata: real\n\n"
	rd := NewReader(strings.NewReader(stream))
	ev, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Data != "real" {
		t.Errorf("data = %q", ev.Data)
	}
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestWriterRoundTrip(t *testing.T) {
	var sb strings.Builder
	w := NewWriter(&sb)
	if err := w.WriteEvent(&Event{Type: "delta", ID: "7", Data: "a\nb"}); err != nil {
		t.Fatal(err)
	}
	rd := NewReader(strings.NewReader(sb.String()))
	ev, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "delta" || ev.ID != "7" || ev.Data != "a\nb" {
		t.Errorf("got %+v", ev)
	}
}

func TestCRLF(t *testing.T) {
	stream := "data: hello\r\n\r\n"
	rd := NewReader(strings.NewReader(stream))
	ev, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Data != "hello" {
		t.Errorf("data = %q", ev.Data)
	}
}
