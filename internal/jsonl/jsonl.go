// Package jsonl provides a strict JSON Lines reader/writer (core-spec §4.4).
// Each value must serialize to exactly one line; malformed or oversized lines
// are reported (never silently dropped) so callers can apply ignore policies
// (F1) explicitly.
package jsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Writer appends JSON values as newline-terminated JSONL lines.
type Writer struct {
	w    *bufio.Writer
	base io.Writer
	err  error
}

// NewWriter wraps w in a buffered JSONL writer.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriter(w), base: w}
}

// Append serializes v to JSON and writes it as a single line.
// Returns an error if v contains values that cannot be marshaled.
func (w *Writer) Append(v any) error {
	if w.err != nil {
		return w.err
	}
	data, err := json.Marshal(v)
	if err != nil {
		w.err = err
		return err
	}
	return w.AppendRaw(data)
}

// AppendRaw writes already-marshaled JSON as a single line.
func (w *Writer) AppendRaw(data []byte) error {
	if w.err != nil {
		return w.err
	}
	if bytes.ContainsAny(data, "\r\n") {
		err := fmt.Errorf("jsonl: value contains a newline; refusing to write multi-line entry")
		w.err = err
		return err
	}
	if _, err := w.w.Write(data); err != nil {
		w.err = err
		return err
	}
	if err := w.w.WriteByte('\n'); err != nil {
		w.err = err
		return err
	}
	return nil
}

// Flush flushes buffered output.
func (w *Writer) Flush() error {
	if w.err != nil {
		return w.err
	}
	w.err = w.w.Flush()
	return w.err
}

// Close flushes and closes the underlying writer if it is an io.Closer.
func (w *Writer) Close() error {
	if err := w.Flush(); err != nil {
		return err
	}
	if c, ok := w.base.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Entry is one parsed JSONL line with its line number (1-based).
type Entry struct {
	Line int
	Data json.RawMessage
}

// ParseError describes a malformed line.
type ParseError struct {
	Line int
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("jsonl: line %d: %v", e.Line, e.Err)
}

// Unwrap returns the underlying parse error.
func (e *ParseError) Unwrap() error { return e.Err }

// Reader reads strict JSONL: every non-empty line must be valid JSON.
type Reader struct {
	sc     *bufio.Scanner
	lineNo int
}

// NewReader returns a strict JSONL reader over r. maxLineBytes limits a single
// line; 0 means use bufio's default (64KB) — for session files pass a large
// budget (e.g. 64<<20) since tool outputs can be big.
func NewReader(r io.Reader, maxLineBytes int) *Reader {
	sc := bufio.NewScanner(r)
	if maxLineBytes > 0 {
		sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	}
	return &Reader{sc: sc}
}

// Next returns the next entry, or io.EOF when exhausted.
// Malformed lines yield a *ParseError.
func (r *Reader) Next() (*Entry, error) {
	for {
		if !r.sc.Scan() {
			if err := r.sc.Err(); err != nil {
				if r.lineNo == 0 {
					r.lineNo = 1
				}
				return nil, err
			}
			return nil, io.EOF
		}
		r.lineNo++
		line := r.sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue // tolerate blank lines between entries
		}
		raw := make([]byte, len(line))
		copy(raw, line)
		if !json.Valid(raw) {
			return nil, &ParseError{Line: r.lineNo, Err: fmt.Errorf("invalid JSON")}
		}
		return &Entry{Line: r.lineNo, Data: raw}, nil
	}
}

// Decode reads all entries and decodes each into v (which must be a pointer
// to a type that can be populated per entry; the same v is reused unless
// fn is provided). If fn is non-nil it is called with each decoded value and
// may return an error to stop reading.
func Decode(r io.Reader, maxLineBytes int, newFn func() any, fn func(any) error) error {
	rd := NewReader(r, maxLineBytes)
	for {
		ent, err := rd.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		v := newFn()
		if err := json.Unmarshal(ent.Data, v); err != nil {
			return &ParseError{Line: ent.Line, Err: err}
		}
		if err := fn(v); err != nil {
			return err
		}
	}
}

// ReadFile reads a JSONL file and returns its raw entries.
func ReadFile(path string, maxLineBytes int) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	rd := NewReader(f, maxLineBytes)
	for {
		ent, err := rd.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *ent)
	}
}
