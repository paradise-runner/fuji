package session

import (
	"context"
	"strings"
	"time"

	"fuji/pkg/exec"
	"fuji/pkg/messages"
)

// BashResult is the outcome of a session-level bash execution.
type BashResult struct {
	Output         string
	ExitCode       *int
	Cancelled      bool
	Truncated      bool
	FullOutputPath string
}

// BashOptions controls ExecuteBash.
type BashOptions struct {
	Timeout time.Duration
	Signal  context.Context
}

// ExecuteBash runs a command in the session cwd, streaming output chunks via
// onChunk and emitting bash_execution_update events (core-spec §4.1).
// The resulting BashExecutionMessage is appended to the session.
func (s *Session) ExecuteBash(command string, onChunk func(string)) (BashResult, error) {
	timeout := s.cfg.Config.BashTimeout
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	acc := &bashAccumulator{}
	handle := func(data []byte) {
		text := sanitizeBashOutput(string(data))
		acc.append(text)
		if onChunk != nil {
			onChunk(text)
		}
		s.emit(Event{Type: EvBashExecutionUpdate, BashID: "bash", BashDelta: text})
	}

	cmd := exec.New("sh", "-c", command).WithDir(s.cfg.Cwd)
	cmd.SetOutputCallbacks(handle, handle)
	runCtx := ctx
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	res, runErr := cmd.Run(runCtx)
	cancelled := runCtx.Err() != nil
	out, truncated, fullPath := acc.finish()

	result := BashResult{
		Output:         out,
		Cancelled:      cancelled,
		Truncated:      truncated,
		FullOutputPath: fullPath,
	}
	if !cancelled && res.ExitCode >= 0 {
		ec := res.ExitCode
		result.ExitCode = &ec
	}
	if runErr != nil && res.ExitCode == 0 && !cancelled {
		// Spawn failure.
		msg := command
		_ = msg
	}

	// Append the BashExecutionMessage to the session.
	ts := time.Now().UnixMilli()
	s.cfg.SessionManager.AppendMessage(messages.AgentMessage{
		Role:           messages.RoleBashExecution,
		Command:        command,
		Output:         out,
		ExitCode:       result.ExitCode,
		Cancelled:      cancelled,
		Truncated:      truncated,
		FullOutputPath: fullPath,
		Timestamp:      ts,
	})
	return result, nil
}

// AbortBash cancels in-flight bash execution.
func (s *Session) AbortBash() {
	s.cancel()
}

// bashAccumulator is a minimal output accumulator for session-level bash.
type bashAccumulator struct {
	chunks     []string
	totalBytes int
	tempFile   string
}

func (a *bashAccumulator) append(text string) {
	a.chunks = append(a.chunks, text)
	a.totalBytes += len(text)
}

// finish tail-truncates and spills to a temp file when needed.
func (a *bashAccumulator) finish() (output string, truncated bool, fullPath string) {
	full := strings.Join(a.chunks, "")
	if a.totalBytes <= 50*1024 && strings.Count(full, "\n") <= 2000 {
		return full, false, ""
	}
	// Simple tail: keep last ~2000 lines / 50KB via the truncate package.
	tr := tailTruncate(full)
	fullPath = writeTempFile("fuji-bash-", full)
	return tr, true, fullPath
}

func sanitizeBashOutput(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			if r != '\r' {
				sb.WriteRune(r)
			}
		case r <= 0x1f:
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
