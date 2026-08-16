package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cliTape = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":{"input_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The answer is 42"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`

func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fuji")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func TestCLIRunEndToEnd(t *testing.T) {
	bin := buildCLI(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, cliTape)
	}))
	defer srv.Close()

	workDir := t.TempDir()
	sessDir := filepath.Join(t.TempDir(), "sessions")
	cmd := exec.Command(bin, "run",
		"--prompt", "list the files",
		"--cwd", workDir,
		"--provider", "anthropic",
		"--base-url", srv.URL,
		"--model", "claude-sonnet-4-5",
		"--thinking", "off",
		"--session-dir", sessDir,
	)
	cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY=test-key")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fuji run failed: %v\n%s", err, out)
	}
	// Session file created.
	files, err := filepath.Glob(filepath.Join(sessDir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no session file in %s: %v", sessDir, err)
	}
	// Session file contains user + assistant messages.
	lines := readLines(t, files[0])
	if len(lines) < 3 {
		t.Fatalf("session has %d lines: %v", len(lines), lines)
	}
	var header, userMsg, asstMsg map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &header)
	_ = json.Unmarshal([]byte(lines[1]), &userMsg)
	_ = json.Unmarshal([]byte(lines[2]), &asstMsg)
	if header["type"] != "session" || header["version"] != float64(3) {
		t.Errorf("header = %v", header)
	}
	if userMsg["type"] != "message" {
		t.Errorf("line2 = %v", userMsg)
	}
	msg := asstMsg["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("assistant msg = %v", msg)
	}
	if !strings.Contains(fmt.Sprint(msg), "The answer is 42") {
		t.Errorf("assistant content missing: %v", msg)
	}
}

func TestCLIRunMissingPrompt(t *testing.T) {
	bin := buildCLI(t)
	cmd := exec.Command(bin, "run", "--cwd", t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected failure without --prompt")
	}
	if !strings.Contains(string(out), "--prompt is required") {
		t.Errorf("output = %q", out)
	}
}

func TestCLIRunAuthFailure(t *testing.T) {
	bin := buildCLI(t)
	cmd := exec.Command(bin, "run",
		"--prompt", "hi",
		"--cwd", t.TempDir(),
		"--provider", "anthropic",
		"--session-dir", t.TempDir(),
	)
	cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if !strings.Contains(string(out), "no API key") {
		t.Errorf("output = %q", out)
	}
}

func TestCLIVersion(t *testing.T) {
	bin := buildCLI(t)
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "fuji ") {
		t.Errorf("version = %q", out)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}
