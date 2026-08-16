package jsonl

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWriterAppendAndRead(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	type item struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	if err := w.Append(item{1, "one"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(item{2, "two"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	var got item
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got != (item{1, "one"}) {
		t.Errorf("got %+v", got)
	}
}

func TestWriterRejectsLiteralNewline(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Append(json.RawMessage("{\"x\":\"a\nb\"}")); err == nil {
		t.Fatal("expected error for literal-newline JSON payload")
	}
}

func TestReaderStrict(t *testing.T) {
	src := "{\"a\":1}\n\n{\"b\":2}\n"
	rd := NewReader(strings.NewReader(src), 0)
	n := 0
	for {
		ent, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		n++
		if ent.Line != 1 && ent.Line != 3 {
			t.Errorf("line = %d", ent.Line)
		}
	}
	if n != 2 {
		t.Errorf("entries = %d", n)
	}
}

func TestReaderMalformedLine(t *testing.T) {
	src := "{\"a\":1}\nnot json\n{\"b\":2}\n"
	rd := NewReader(strings.NewReader(src), 0)
	_, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	_, err = rd.Next()
	if err == nil {
		t.Fatal("expected parse error")
	}
	pe, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if pe.Line != 2 {
		t.Errorf("line = %d, want 2", pe.Line)
	}
}

func TestDecodeWithFactory(t *testing.T) {
	src := "{\"v\":1}\n{\"v\":2}\n"
	type item struct {
		V int `json:"v"`
	}
	var sum int
	err := Decode(strings.NewReader(src), 0, func() any { return &item{} }, func(v any) error {
		sum += v.(*item).V
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum != 3 {
		t.Errorf("sum = %d", sum)
	}
}
