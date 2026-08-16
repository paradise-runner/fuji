package exec

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunBasic(t *testing.T) {
	res, err := New("echo", "hello").Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d", res.ExitCode)
	}
}

func TestRunExitCode(t *testing.T) {
	res, err := New("sh", "-c", "exit 3").Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d", res.ExitCode)
	}
}

func TestRunTimeoutKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep semantics differ on windows")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := New("sleep", "10").Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = %v (stderr=%q)", res.TimedOut, res.Stderr)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took too long: %v", elapsed)
	}
}

func TestRunContextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep semantics differ on windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	res, err := New("sleep", "10").Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cancelled {
		t.Errorf("Cancelled = %v", res.Cancelled)
	}
}

func TestStreamingCallbacks(t *testing.T) {
	var mu sync.Mutex
	var chunks []string
	cmd := New("sh", "-c", "printf 'a'; sleep 0.05; printf 'b'")
	cmd.SetOutputCallbacks(func(p []byte) {
		mu.Lock()
		chunks = append(chunks, string(p))
		mu.Unlock()
	}, nil)
	res, err := cmd.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if res.Stdout != "ab" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if len(chunks) != 2 || chunks[0] != "a" || chunks[1] != "b" {
		t.Errorf("chunks = %v", chunks)
	}
}

func TestWithDirAndEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/marker.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := New("sh", "-c", "pwd; echo $FUJI_TEST_ENV").WithDir(dir).WithEnv("FUJI_TEST_ENV=hello").Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, dir) {
		t.Errorf("pwd missing: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Errorf("env missing: %q", res.Stdout)
	}
}

func TestStderrCollected(t *testing.T) {
	res, err := New("sh", "-c", "echo oops >&2; exit 1").Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stderr) != "oops" {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestRunWithTimeout(t *testing.T) {
	_, err := RunWithTimeout(context.Background(), 100*time.Millisecond, "sleep", "10")
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
}

func TestSpawnFailure(t *testing.T) {
	_, err := New("/nonexistent/definitely-not-a-binary-xyz").Run(context.Background())
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %T, want *Error", err)
	}
}
