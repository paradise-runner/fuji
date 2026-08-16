package bash_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"fuji/pkg/tools"
	"fuji/pkg/tools/bash"
)

func run(t *testing.T, def tools.Definition, params string, ctx context.Context) tools.Result {
	t.Helper()
	res, err := def.Execute("call_1", json.RawMessage(params), ctx, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res
}

func TestBashBasic(t *testing.T) {
	dir := t.TempDir()
	def := bash.New(dir, 30*time.Second)
	res := run(t, def, `{"command":"echo hello && pwd"}`, context.Background())
	if res.IsError {
		t.Fatalf("bash error: %q", res.ContentText())
	}
	if !strings.Contains(res.ContentText(), "hello") || !strings.Contains(res.ContentText(), dir) {
		t.Errorf("output = %q", res.ContentText())
	}
}

func TestBashExitCode(t *testing.T) {
	dir := t.TempDir()
	def := bash.New(dir, 30*time.Second)
	res := run(t, def, `{"command":"exit 3"}`, context.Background())
	if !res.IsError {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(res.ContentText(), "Command exited with code 3") {
		t.Errorf("output = %q", res.ContentText())
	}
}

func TestBashTimeout(t *testing.T) {
	dir := t.TempDir()
	def := bash.New(dir, 30*time.Second)
	start := time.Now()
	res := run(t, def, `{"command":"sleep 10","timeout":1}`, context.Background())
	if !res.IsError {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(res.ContentText(), "Command timed out after 1 seconds") {
		t.Errorf("output = %q", res.ContentText())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took too long: %v", elapsed)
	}
}

func TestBashAbort(t *testing.T) {
	dir := t.TempDir()
	def := bash.New(dir, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res := run(t, def, `{"command":"sleep 10"}`, ctx)
	if !res.IsError {
		t.Fatal("expected abort error")
	}
	if !strings.Contains(res.ContentText(), "Command aborted") {
		t.Errorf("output = %q", res.ContentText())
	}
}

func TestBashMissingCwd(t *testing.T) {
	def := bash.New("/nonexistent/dir-xyz", 30*time.Second)
	res := run(t, def, `{"command":"echo hi"}`, context.Background())
	if !res.IsError || !strings.Contains(res.ContentText(), "working directory does not exist") {
		t.Errorf("output = %q (isError=%v)", res.ContentText(), res.IsError)
	}
}

func TestBashTruncationAndFullOutputPath(t *testing.T) {
	dir := t.TempDir()
	def := bash.New(dir, 30*time.Second)
	// Generate > 50KB of output.
	res := run(t, def, `{"command":"for i in $(seq 1 3000); do echo line-$i; done"}`, context.Background())
	if res.IsError {
		t.Fatalf("bash error: %q", res.ContentText())
	}
	out := res.ContentText()
	if !strings.Contains(out, "line-3000") {
		t.Error("tail output should be present")
	}
	if !strings.Contains(out, "[Showing lines") || !strings.Contains(out, "Full output:") {
		t.Errorf("no truncation notice: %.120q", out)
	}
	// The full output path should point to an existing file.
	idx := strings.Index(out, "Full output: ")
	if idx >= 0 {
		path := strings.TrimSpace(out[idx+len("Full output: "):])
		path = strings.TrimSuffix(path, "]")
		if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "line-3000") {
			t.Errorf("full output file missing/incomplete: %v", err)
		}
	}
}

func TestBashStderrIncluded(t *testing.T) {
	dir := t.TempDir()
	def := bash.New(dir, 30*time.Second)
	res := run(t, def, `{"command":"echo to-stderr >&2; echo to-stdout"}`, context.Background())
	if res.IsError {
		t.Fatalf("bash error: %q", res.ContentText())
	}
	if !strings.Contains(res.ContentText(), "to-stderr") || !strings.Contains(res.ContentText(), "to-stdout") {
		t.Errorf("output = %q", res.ContentText())
	}
}

func TestBashANSIStripped(t *testing.T) {
	dir := t.TempDir()
	def := bash.New(dir, 30*time.Second)
	res := run(t, def, `{"command":"printf '\\033[31mred\\033[0m plain'"}`, context.Background())
	if res.IsError {
		t.Fatalf("bash error: %q", res.ContentText())
	}
	if strings.Contains(res.ContentText(), "\x1b") {
		t.Errorf("ANSI not stripped: %q", res.ContentText())
	}
	if !strings.Contains(res.ContentText(), "red") {
		t.Errorf("output = %q", res.ContentText())
	}
}

func TestBashStreamingUpdates(t *testing.T) {
	dir := t.TempDir()
	def := bash.New(dir, 30*time.Second)
	var updates []string
	_, err := def.Execute("call_1", json.RawMessage("{\"command\":\"printf 'a'; sleep 0.05; printf 'b'\"}"), context.Background(), func(u tools.Update) {
		updates = append(updates, strings.Join(texts(u), ""))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) == 0 {
		t.Fatal("no streaming updates received")
	}
}

func texts(u tools.Update) []string {
	var out []string
	for _, blk := range u.Content {
		if t, ok := blk.(interface{ TextOf() string }); ok {
			_ = t
		}
	}
	return out
}
