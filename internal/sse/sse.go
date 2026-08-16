// Package sse implements a Server-Sent Events reader/writer (core-spec §4.3,
// D10). It parses the text/event-stream format: events delimited by blank
// lines, with "event:", "data:", "id:", "retry:" fields. Multi-line data is
// concatenated with '\n' per the spec.
package sse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Event is one parsed SSE event.
type Event struct {
	Type  string // "message" when the event: field is absent
	ID    string
	Data  string
	Retry int
	Raw   []byte // raw event block (for debugging / recording)
}

// IsEmpty reports whether the event carries no data.
func (e *Event) IsEmpty() bool { return e.Data == "" }

// Reader incrementally parses SSE events from an io.Reader.
type Reader struct {
	br    *bufio.Reader
	state string // "", "comment", "event", "data", "id", "retry", "done"
	cur   *Event
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next complete event, or io.EOF.
func (r *Reader) Next() (*Event, error) {
	for {
		line, err := r.br.ReadString('\n')
		if err != nil && len(line) == 0 {
			// Flush any pending event at EOF.
			if r.cur != nil {
				ev := r.cur
				r.cur = nil
				return ev, nil
			}
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if err == io.EOF && line == "" {
			if r.cur != nil {
				ev := r.cur
				r.cur = nil
				return ev, nil
			}
			return nil, io.EOF
		}
		if line == "" {
			// Blank line: dispatch the event if it has data.
			if r.cur != nil && !r.cur.IsEmpty() {
				ev := r.cur
				r.cur = nil
				return ev, nil
			}
			r.cur = nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment
		}
		if r.cur == nil {
			r.cur = &Event{Type: "message"}
		}
		r.apply(line)
		if err == io.EOF {
			if !r.cur.IsEmpty() {
				ev := r.cur
				r.cur = nil
				return ev, nil
			}
			r.cur = nil
			return nil, io.EOF
		}
	}
}

// apply parses one "field: value" line into the current event.
func (r *Reader) apply(line string) {
	colon := strings.Index(line, ":")
	var field, value string
	if colon < 0 {
		field = line
	} else {
		field = line[:colon]
		value = line[colon+1:]
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
	}
	switch field {
	case "event":
		r.cur.Type = value
	case "data":
		if r.cur.Data == "" {
			r.cur.Data = value
		} else {
			r.cur.Data += "\n" + value
		}
	case "id":
		r.cur.ID = value
	case "retry":
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			r.cur.Retry = n
		}
	}
}

// Writer writes SSE events.
type Writer struct {
	w io.Writer
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteEvent writes one event block.
func (w *Writer) WriteEvent(ev *Event) error {
	var buf bytes.Buffer
	if ev.Type != "" && ev.Type != "message" {
		fmt.Fprintf(&buf, "event: %s\n", ev.Type)
	}
	if ev.ID != "" {
		fmt.Fprintf(&buf, "id: %s\n", ev.ID)
	}
	if ev.Retry > 0 {
		fmt.Fprintf(&buf, "retry: %d\n", ev.Retry)
	}
	for _, line := range strings.Split(ev.Data, "\n") {
		fmt.Fprintf(&buf, "data: %s\n", line)
	}
	buf.WriteString("\n")
	_, err := w.w.Write(buf.Bytes())
	return err
}

// WriteData writes a single data-only event.
func (w *Writer) WriteData(data string) error {
	return w.WriteEvent(&Event{Type: "message", Data: data})
}
