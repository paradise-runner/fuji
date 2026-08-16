// Package truncate implements output truncation semantics (core-spec §4.5),
// corresponding to the reference truncate module: head/tail/line truncation
// with explicit markers. Two independent limits — lines (default 2000) and
// bytes (default 50KB) — whichever is hit first wins.
package truncate

import (
	"unicode/utf8"
)

// Defaults for truncation.
const (
	DefaultMaxLines   = 2000
	DefaultMaxBytes   = 50 * 1024
	GreplineMaxLength = 500 // max chars per grep match line
)

// Options for truncation.
type Options struct {
	MaxLines int
	MaxBytes int
}

func (o Options) norm() (int, int) {
	maxLines, maxBytes := o.MaxLines, o.MaxBytes
	if maxLines <= 0 {
		maxLines = DefaultMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return maxLines, maxBytes
}

// Result describes a truncation outcome.
type Result struct {
	Content               string
	Truncated             bool
	TruncatedBy           string // "", "lines", "bytes"
	TotalLines            int
	TotalBytes            int
	OutputLines           int
	OutputBytes           int
	LastLinePartial       bool
	FirstLineExceedsLimit bool
	MaxLines              int
	MaxBytes              int
}

// splitLines splits content into lines (a trailing newline does not produce
// an extra empty line).
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			lines = append(lines, content[start:i])
			start = i + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

func byteLen(s string) int { return len(s) }

// TruncateHead keeps the first N lines/bytes.
func TruncateHead(content string, opts Options) Result {
	maxLines, maxBytes := opts.norm()
	totalBytes := byteLen(content)
	lines := splitLines(content)
	totalLines := len(lines)
	r := Result{TotalLines: totalLines, TotalBytes: totalBytes, MaxLines: maxLines, MaxBytes: maxBytes}

	if totalLines <= maxLines && totalBytes <= maxBytes {
		r.Content = content
		r.OutputLines = totalLines
		r.OutputBytes = totalBytes
		return r
	}
	if totalLines > 0 && byteLen(lines[0]) > maxBytes {
		r.Truncated = true
		r.TruncatedBy = "bytes"
		r.FirstLineExceedsLimit = true
		return r
	}
	var out []string
	var outBytes int
	truncatedBy := "lines"
	for i := 0; i < len(lines) && i < maxLines; i++ {
		lineBytes := byteLen(lines[i])
		if i > 0 {
			lineBytes++ // +1 for newline
		}
		if outBytes+lineBytes > maxBytes {
			truncatedBy = "bytes"
			break
		}
		out = append(out, lines[i])
		outBytes += lineBytes
	}
	output := joinLines(out)
	r.Content = output
	r.Truncated = true
	r.TruncatedBy = truncatedBy
	r.OutputLines = len(out)
	r.OutputBytes = byteLen(output)
	return r
}

// TruncateTail keeps the last N lines/bytes. May return a partial first line
// when a single line exceeds the byte limit.
func TruncateTail(content string, opts Options) Result {
	maxLines, maxBytes := opts.norm()
	totalBytes := byteLen(content)
	lines := splitLines(content)
	totalLines := len(lines)
	r := Result{TotalLines: totalLines, TotalBytes: totalBytes, MaxLines: maxLines, MaxBytes: maxBytes}

	if totalLines <= maxLines && totalBytes <= maxBytes {
		r.Content = content
		r.OutputLines = totalLines
		r.OutputBytes = totalBytes
		return r
	}
	var out []string
	var outBytes int
	truncatedBy := "lines"
	lastLinePartial := false
	for i := len(lines) - 1; i >= 0 && len(out) < maxLines; i-- {
		lineBytes := byteLen(lines[i])
		if len(out) > 0 {
			lineBytes++ // +1 for newline
		}
		if outBytes+lineBytes > maxBytes {
			truncatedBy = "bytes"
			if len(out) == 0 {
				truncated := truncateBytesFromEnd(lines[i], maxBytes)
				out = append([]string{truncated}, out...)
				outBytes = byteLen(truncated)
				lastLinePartial = true
			}
			break
		}
		out = append([]string{lines[i]}, out...)
		outBytes += lineBytes
	}
	output := joinLines(out)
	r.Content = output
	r.Truncated = true
	r.TruncatedBy = truncatedBy
	r.OutputLines = len(out)
	r.OutputBytes = byteLen(output)
	r.LastLinePartial = lastLinePartial
	return r
}

// truncateBytesFromEnd keeps the last maxBytes bytes on a UTF-8 boundary.
func truncateBytesFromEnd(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	// Skip continuation bytes to land on a rune boundary.
	for start < len(s) && isContinuation(s[start]) {
		start++
	}
	// If the rune at start is invalid (partial), skip one more.
	for start < len(s) && !utf8.ValidString(s[start:]) {
		start++
	}
	if start >= len(s) {
		// fall back: re-validate from a safe offset
		start = len(s) - maxBytes
		for start > 0 && isContinuation(s[start]) {
			start--
		}
	}
	return s[start:]
}

func isContinuation(b byte) bool { return b&0xc0 == 0x80 }

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// TruncateLine truncates a single line to maxChars with a marker.
func TruncateLine(line string, maxChars int) (string, bool) {
	if maxChars <= 0 {
		maxChars = GreplineMaxLength
	}
	if len(line) <= maxChars {
		return line, false
	}
	return line[:maxChars] + "... [truncated]", true
}

// FormatSize renders bytes human-readably.
func FormatSize(bytes int) string {
	switch {
	case bytes < 1024:
		return itoa(bytes) + "B"
	case bytes < 1024*1024:
		return ftoa1(float64(bytes)/1024) + "KB"
	default:
		return ftoa1(float64(bytes)/(1024*1024)) + "MB"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func ftoa1(f float64) string {
	i := int(f)
	frac := int((f - float64(i)) * 10)
	return itoa(i) + "." + itoa(frac)
}
