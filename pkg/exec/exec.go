// Package exec provides context-aware process execution primitives:
// cancellation, timeouts, streaming output, env/cwd control (core-spec §4.10).
// It backs the bash tool and the embedded-binary subprocesses (grep, git).
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Result of a completed command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut is true when the process was killed by the context deadline.
	TimedOut bool
	// Cancelled is true when the process was killed by context cancellation
	// (e.g. Abort).
	Cancelled bool
	// Duration is the wall-clock runtime.
	Duration time.Duration
}

// Error is returned for failures other than a non-zero exit (spawn errors,
// signal kills).
type Error struct {
	Op   string
	Name string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("%s %s: %v", e.Op, e.Name, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// Options controls command execution.
type Options struct {
	Dir        string
	Env        []string // additional env (KEY=VALUE); merged over os.Environ()
	Stdin      io.Reader
	Shell      bool   // run through the shell ("sh -c" / "cmd /C")
	NoShellCmd string // when Shell is true, the command to run
}

// Command is a process runner.
type Command struct {
	Name string
	Args []string
	Opts Options

	// streaming state
	mu       sync.Mutex
	onStdout func(p []byte)
	onStderr func(p []byte)
}

// New builds a Command.
func New(name string, args ...string) *Command {
	return &Command{Name: name, Args: args}
}

// WithDir sets the working directory.
func (c *Command) WithDir(dir string) *Command { c.Opts.Dir = dir; return c }

// WithEnv appends env entries (KEY=VALUE).
func (c *Command) WithEnv(env ...string) *Command { c.Opts.Env = append(c.Opts.Env, env...); return c }

// WithShell runs the command through the shell.
func (c *Command) WithShell(shellCmd string) *Command {
	c.Opts.Shell = true
	c.Opts.NoShellCmd = shellCmd
	return c
}

// SetOutputCallbacks installs streaming callbacks for stdout/stderr bytes.
func (c *Command) SetOutputCallbacks(onStdout, onStderr func(p []byte)) *Command {
	c.onStdout = onStdout
	c.onStderr = onStderr
	return c
}

// Run executes the command, collecting output. It returns a Result; spawn
// failures return *Error. ctx controls cancellation and deadlines — when the
// context is done, the process group is killed (SIGTERM, then SIGKILL after a
// short grace period).
func (c *Command) Run(ctx context.Context) (Result, error) {
	var stdout, stderr bytes.Buffer
	return c.run(ctx, &stdout, &stderr, false)
}

// run executes with optional streaming into provided buffers and/or callbacks.
func (c *Command) run(ctx context.Context, stdoutBuf, stderrBuf *bytes.Buffer, pipe bool) (Result, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	if c.Opts.Shell {
		cmd = exec.CommandContext(ctx, shellBin(), shellArgs(c.Opts.NoShellCmd)...)
	}
	cmd.Dir = c.Opts.Dir
	cmd.Stdin = c.Opts.Stdin
	env := os.Environ()
	if len(c.Opts.Env) > 0 {
		env = mergeEnv(env, c.Opts.Env)
	}
	cmd.Env = env
	// Place the child in its own process group so cancellation / deadline can
	// terminate the child together with any descendants it spawns (shell
	// subshells, pipelines, background jobs), which would otherwise inherit the
	// output pipes and block cmd.Wait until they exit on their own.
	setupProcessGroup(cmd)
	var pgid atomic.Int32
	done := make(chan struct{})
	// Kill the whole process group when the context is cancelled. Guarded with
	// an atomic pid so the write after Start and the read here never race.
	go func() {
		select {
		case <-ctx.Done():
			if p := pgid.Load(); p != 0 {
				killProcessGroup(int(p))
			}
		case <-done:
		}
	}()
	defer close(done)
	killIfDone := func() {
		// Covers the window where the context was already done before the
		// process spawned: the watcher above may have fired with pid 0.
		if ctx.Err() != nil {
			if p := pgid.Load(); p != 0 {
				killProcessGroup(int(p))
			}
		}
	}

	if pipe && (c.onStdout != nil || c.onStderr != nil) {
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			return Result{}, &Error{Op: "pipe", Name: c.Name, Err: err}
		}
		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			return Result{}, &Error{Op: "pipe", Name: c.Name, Err: err}
		}
		if err := cmd.Start(); err != nil {
			return Result{}, &Error{Op: "start", Name: c.Name, Err: err}
		}
		pgid.Store(int32(cmd.Process.Pid))
		killIfDone()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.pump(stdoutPipe, stdoutBuf, c.onStdout)
		}()
		go func() {
			defer wg.Done()
			c.pump(stderrPipe, stderrBuf, c.onStderr)
		}()
		err = cmd.Wait()
		wg.Wait()
		return c.finish(ctx, cmd, stdoutBuf, stderrBuf, err, start)
	}

	var outW, errW io.Writer
	if c.onStdout != nil {
		outW = &callbackWriter{fn: c.onStdout, buf: stdoutBuf}
	} else {
		outW = stdoutBuf
	}
	if c.onStderr != nil {
		errW = &callbackWriter{fn: c.onStderr, buf: stderrBuf}
	} else {
		errW = stderrBuf
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	if err := cmd.Start(); err != nil {
		return Result{}, &Error{Op: "start", Name: c.Name, Err: err}
	}
	pgid.Store(int32(cmd.Process.Pid))
	killIfDone()
	err := cmd.Wait()
	return c.finish(ctx, cmd, stdoutBuf, stderrBuf, err, start)
}

// pump copies r into buf (if non-nil) while invoking fn on each chunk.
func (c *Command) pump(r io.Reader, buf *bytes.Buffer, fn func(p []byte)) {
	b := make([]byte, 32*1024)
	for {
		n, err := r.Read(b)
		if n > 0 {
			if buf != nil {
				buf.Write(b[:n])
			}
			if fn != nil {
				fn(b[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

// finish converts cmd.Run/Wait errors into a Result.
func (c *Command) finish(ctx context.Context, cmd *exec.Cmd, stdoutBuf, stderrBuf *bytes.Buffer, runErr error, start time.Time) (Result, error) {
	res := Result{
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
		Duration: time.Since(start),
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr == nil {
		return res, nil
	}
	// Process killed by signal (context deadline/cancel) or exit != 0.
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if res.ExitCode < 0 {
			// Killed by signal.
			res.Cancelled = ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled)
			res.TimedOut = ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)
		}
		return res, nil
	}
	return res, &Error{Op: "run", Name: c.Name, Err: runErr}
}

// --- helpers ---------------------------------------------------------------

// DefaultTimeout is the fallback timeout when none is provided.
const DefaultTimeout = 30 * time.Second

// RunWithTimeout runs a command with a timeout, returning ErrTimeout on
// deadline expiry.
func RunWithTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := New(name, args...).Run(ctx)
	if ctx.Err() == context.DeadlineExceeded {
		return res, ErrTimeout
	}
	return res, err
}

// ErrTimeout is returned when a RunWithTimeout deadline expires.
var ErrTimeout = errors.New("command timed out")

func mergeEnv(base, extra []string) []string {
	m := make(map[string]string, len(base)+len(extra))
	order := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		if i := indexOf(kv, '='); i > 0 {
			k := kv[:i]
			if _, seen := m[k]; !seen {
				order = append(order, k)
			}
			m[k] = kv[i+1:]
		}
	}
	for _, kv := range extra {
		if i := indexOf(kv, '='); i > 0 {
			k := kv[:i]
			if _, seen := m[k]; !seen {
				order = append(order, k)
			}
			m[k] = kv[i+1:]
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func shellBin() string {
	if os.Getenv("COMSPEC") != "" {
		return os.Getenv("COMSPEC")
	}
	return "/bin/sh"
}

func shellArgs(shellCmd string) []string {
	if shellBin() == "/bin/sh" {
		return []string{"-c", shellCmd}
	}
	return []string{"/C", shellCmd}
}

type callbackWriter struct {
	fn  func(p []byte)
	buf *bytes.Buffer
}

func (w *callbackWriter) Write(p []byte) (int, error) {
	if w.buf != nil {
		w.buf.Write(p)
	}
	w.fn(p)
	return len(p), nil
}
