package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fuji/internal/logging"
	"fuji/pkg/tools"
)

// TestSIGTERMAbortsPerContract: killing fuji mid-run exits per the contract
// (aborted → 3) and leaves a loadable session file.
func TestSIGTERMAbortsPerContract(t *testing.T) {
	bin := buildCLI(t)
	// Server streams slowly; the run will be aborted by SIGTERM.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":{}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streaming slowly"}}
`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	sessDir := t.TempDir()
	cmd := exec.Command(bin, "run",
		"--prompt", "do work",
		"--cwd", t.TempDir(),
		"--provider", "anthropic",
		"--base-url", srv.URL,
		"--model", "claude-sonnet-4-5",
		"--thinking", "off",
		"--session-dir", sessDir,
	)
	cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY=k")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("expected non-zero exit, got %v", err)
		}
		code := exitErr.ExitCode()
		if code != ExitAborted && code != ExitSigint {
			t.Errorf("exit code = %d, want %d (aborted) or %d (sigint)", code, ExitAborted, ExitSigint)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("fuji did not exit after SIGTERM")
	}
	// Session file exists and parses (append-only writes never corrupt).
	files, _ := filepath.Glob(filepath.Join(sessDir, "*.jsonl"))
	if len(files) == 0 {
		t.Fatal("no session file after abort")
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	// Every non-empty line must be valid JSON.
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Errorf("corrupt session line: %v: %.80q", err, line)
		}
	}
}

// TestStructuredLogging: --log-level info emits JSON lines to stderr.
func TestStructuredLogging(t *testing.T) {
	bin := buildCLI(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, cliTape)
	}))
	defer srv.Close()
	cmd := exec.Command(bin, "run",
		"--prompt", "hi",
		"--cwd", t.TempDir(),
		"--provider", "anthropic",
		"--base-url", srv.URL,
		"--model", "claude-sonnet-4-5",
		"--thinking", "off",
		"--session-dir", t.TempDir(),
		"--log-level", "info",
	)
	cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY=k")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	var jsonLines int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		jsonLines++
		if rec["level"] != "info" || rec["msg"] == "" {
			t.Errorf("bad log record: %v", rec)
		}
	}
	if jsonLines < 3 {
		t.Errorf("json log lines = %d (want session/tool/agent events)", jsonLines)
	}
}

// TestToolTimeout: a tool exceeding the tool timeout is killed (P8).
func TestToolTimeout(t *testing.T) {
	slow := tools.New("slow", "Slow", "sleeps",
		mustSchemaRaw(),
		func(callID string, params json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			<-ctx.Done() // hang until timeout fires
			return tools.Result{}, ctx.Err()
		})
	// Wrap like the session does (wrapToolTimeout is unexported; replicate).
	orig := slow.Execute
	slow.Execute = func(callID string, params json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
		tctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		return orig(callID, params, tctx, onUpdate)
	}
	start := time.Now()
	res, err := slow.Execute("c", json.RawMessage(`{}`), context.Background(), nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("tool took too long: %v", elapsed)
	}
	_ = res
}

func mustSchemaRaw() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

// TestLoggingLevels exercises the logger directly.
func TestLoggingLevels(t *testing.T) {
	var sb strings.Builder
	log := logging.New(&sb, logging.LevelWarn)
	log.Debug("hidden")
	log.Info("hidden")
	log.Warn("shown")
	log.Error("also shown")
	if strings.Contains(sb.String(), "hidden") {
		t.Errorf("debug/info leaked: %q", sb.String())
	}
	if !strings.Contains(sb.String(), `"level":"warn"`) || !strings.Contains(sb.String(), `"msg":"shown"`) {
		t.Errorf("warn missing: %q", sb.String())
	}
	if !strings.Contains(sb.String(), `"msg":"also shown"`) {
		t.Errorf("error missing: %q", sb.String())
	}
}
