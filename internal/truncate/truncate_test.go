package truncate

import (
	"strings"
	"testing"
)

func TestNoTruncation(t *testing.T) {
	content := "a\nb\nc"
	r := TruncateHead(content, Options{})
	if r.Truncated || r.Content != content {
		t.Errorf("got %+v", r)
	}
	if r.TotalLines != 3 || r.TotalBytes != 5 {
		t.Errorf("totals = %d/%d", r.TotalLines, r.TotalBytes)
	}
}

func TestTruncateHeadByLines(t *testing.T) {
	content := strings.Repeat("x\n", 100)
	r := TruncateHead(content, Options{MaxLines: 10, MaxBytes: 1 << 20})
	if !r.Truncated || r.TruncatedBy != "lines" {
		t.Fatalf("got %+v", r)
	}
	if r.OutputLines != 10 {
		t.Errorf("output lines = %d", r.OutputLines)
	}
	if r.Content != strings.Repeat("x\n", 9)+"x" {
		t.Errorf("content = %q", r.Content)
	}
}

func TestTruncateHeadByBytes(t *testing.T) {
	content := strings.Repeat("abcdefgh\n", 20) // 180 bytes
	r := TruncateHead(content, Options{MaxLines: 100, MaxBytes: 100})
	if !r.Truncated || r.TruncatedBy != "bytes" {
		t.Fatalf("got %+v", r)
	}
	if r.OutputBytes > 100 {
		t.Errorf("output bytes = %d", r.OutputBytes)
	}
	// Never a partial line.
	if !strings.HasSuffix(r.Content, "abcdefgh") && r.Content != "" {
		t.Errorf("partial line: %q", r.Content)
	}
}

func TestFirstLineExceedsLimit(t *testing.T) {
	long := strings.Repeat("y", 200)
	r := TruncateHead(long+"\nrest", Options{MaxBytes: 100})
	if !r.FirstLineExceedsLimit || r.Content != "" {
		t.Errorf("got %+v", r)
	}
}

func TestTruncateTailKeepsEnd(t *testing.T) {
	content := strings.Repeat("line\n", 50) + "THE_END"
	r := TruncateTail(content, Options{MaxLines: 5, MaxBytes: 1 << 20})
	if !r.Truncated || !strings.Contains(r.Content, "THE_END") {
		t.Errorf("got %+v", r)
	}
	if r.OutputLines != 5 {
		t.Errorf("output lines = %d", r.OutputLines)
	}
}

func TestTruncateTailPartialLastLine(t *testing.T) {
	content := strings.Repeat("z", 5000)
	r := TruncateTail(content, Options{MaxBytes: 100, MaxLines: 1 << 20})
	if !r.LastLinePartial {
		t.Errorf("got %+v", r)
	}
	if len(r.Content) > 100 {
		t.Errorf("output = %d bytes", len(r.Content))
	}
}

func TestUTF8Boundary(t *testing.T) {
	content := "héllo wörld " + strings.Repeat("🙂", 50)
	r := TruncateTail(content, Options{MaxBytes: 40})
	if !validUTF8(r.Content) {
		t.Errorf("invalid utf8: %q", r.Content)
	}
}

func TestTruncateLine(t *testing.T) {
	line := strings.Repeat("a", 1000)
	got, truncated := TruncateLine(line, 100)
	if !truncated {
		t.Error("expected truncation")
	}
	if !strings.HasSuffix(got, "... [truncated]") {
		t.Errorf("marker missing: %q", got)
	}
	short, truncated := TruncateLine("hi", 100)
	if truncated || short != "hi" {
		t.Errorf("got %q %v", short, truncated)
	}
}

func TestFormatSize(t *testing.T) {
	if got := FormatSize(500); got != "500B" {
		t.Errorf("500B = %q", got)
	}
	if got := FormatSize(2048); got != "2.0KB" {
		t.Errorf("2KB = %q", got)
	}
	if got := FormatSize(3 * 1024 * 1024); got != "3.0MB" {
		t.Errorf("3MB = %q", got)
	}
}

func validUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}
