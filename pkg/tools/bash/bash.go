// Package bash implements the bash tool: host-shell execution with streaming
// output, tail truncation, fullOutputPath spill-over for large outputs, and
// timeout/abort via context cancellation.
package bash

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fuji/internal/truncate"
	"fuji/pkg/exec"
	"fuji/pkg/messages"
	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// Params defines the bash tool parameters.
type Params struct {
	Command string `json:"command"`
	Timeout *int   `json:"timeout"` // seconds
}

// New builds the bash tool Definition. defaultTimeout applies when the
// command does not specify one (config BashTimeout; 0 = no timeout).
func New(cwd string, defaultTimeout time.Duration) tools.Definition {
	params, _ := schema.Object([]string{"command"}, map[string]schema.Schema{
		"command": schema.Str("Bash command to execute"),
		"timeout": schema.Int("Timeout in seconds (optional)"),
	}).Raw()
	return tools.Definition{
		Name: "bash", Label: "Bash",
		Description: "Execute a bash command in the session working directory. Output is streamed and truncated to the last 2000 lines or 50KB (whichever is hit first); large outputs spill to a temp file (fullOutputPath).",
		Parameters:  params,
		Execute: func(callID string, raw json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			var p Params
			if err := json.Unmarshal(raw, &p); err != nil {
				return tools.ErrorResult(err), nil
			}
			return execute(cwd, p, defaultTimeout, ctx, onUpdate)
		},
	}
}

// accumulator mirrors the output accumulator semantics.
type accumulator struct {
	mu            sync.Mutex
	chunks        []string
	outputBytes   int
	totalBytes    int
	tempFile      string
	lastLineBytes int
}

func (a *accumulator) append(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.totalBytes += len(text)
	a.lastLineBytes = len(text)
	if idx := strings.LastIndex(text, "\n"); idx >= 0 {
		a.lastLineBytes = len(text) - idx - 1
	}
	if a.totalBytes > truncate.DefaultMaxBytes {
		a.ensureTempFileLocked()
	}
	if a.tempFile != "" {
		f, err := os.OpenFile(a.tempFile, os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			_, _ = f.WriteString(text)
			_ = f.Close()
		}
	}
	a.chunks = append(a.chunks, text)
	a.outputBytes += len(text)
	maxBytes := truncate.DefaultMaxBytes * 2
	for a.outputBytes > maxBytes && len(a.chunks) > 1 {
		a.outputBytes -= len(a.chunks[0])
		a.chunks = a.chunks[1:]
	}
}

func (a *accumulator) ensureTempFileLocked() {
	if a.tempFile != "" {
		return
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	a.tempFile = filepath.Join(os.TempDir(), "fuji-bash-"+hex.EncodeToString(b)+".log")
}

func (a *accumulator) full() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.chunks, "")
}

func (a *accumulator) getLastLineBytes() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastLineBytes
}

func execute(cwd string, p Params, defaultTimeout time.Duration, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
	if _, err := os.Stat(cwd); err != nil {
		return tools.ErrorResult(fmt.Errorf("working directory does not exist: %s\nCannot execute bash commands.", cwd)), nil
	}
	// Timeout resolution.
	var timeout time.Duration
	if p.Timeout != nil && *p.Timeout > 0 {
		timeout = time.Duration(*p.Timeout) * time.Second
	} else {
		timeout = defaultTimeout
	}

	acc := &accumulator{}
	handle := func(data []byte) {
		text := sanitizeOutput(string(data))
		acc.append(text)
		if onUpdate != nil {
			acc.mu.Lock()
			snap := acc.snapshotLocked()
			acc.mu.Unlock()
			details, _ := json.Marshal(map[string]any{
				"truncation":     snap.truncation,
				"fullOutputPath": snap.fullOutputPath,
			})
			onUpdate(tools.Update{Content: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: snap.content}}, Details: details})
		}
	}
	// stdout and stderr are merged into one output stream.
	cmd := exec.New("sh", "-c", p.Command).WithDir(cwd)
	cmd.SetOutputCallbacks(handle, handle)

	runCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	res, err := cmd.Run(runCtx)

	if err == nil && runCtx.Err() == context.DeadlineExceeded {
		snap := finish(acc)
		return tools.ErrorResult(fmt.Errorf("%s\n\nCommand timed out after %d seconds", formatOutput(snap), int(timeout.Seconds()))), nil
	}
	if runCtx.Err() != nil {
		snap := finish(acc)
		return tools.ErrorResult(fmt.Errorf("%s\n\nCommand aborted", formatOutput(snap))), nil
	}
	if err != nil {
		snap := finish(acc)
		// Spawn failure.
		if res.ExitCode == 0 {
			return tools.ErrorResult(fmt.Errorf("%s\n\n%v", formatOutput(snap), err)), nil
		}
		return tools.ErrorResult(fmt.Errorf("%s\n\nCommand exited with code %d", formatOutput(snap), res.ExitCode)), nil
	}
	snap := finish(acc)
	if res.ExitCode != 0 {
		return tools.ErrorResult(fmt.Errorf("%s\n\nCommand exited with code %d", formatOutput(snap), res.ExitCode)), nil
	}
	out := formatOutput(snap)
	return tools.TextResult(out), nil
}

type snapshot struct {
	content        string
	truncation     any
	fullOutputPath string
}

func (a *accumulator) snapshotLocked() snapshot {
	full := strings.Join(a.chunks, "")
	tr := truncate.TruncateTail(full, truncate.Options{})
	snap := snapshot{content: tr.Content}
	if tr.Truncated {
		a.ensureTempFileLocked()
		snap.fullOutputPath = a.tempFile
		trJSON, _ := json.Marshal(tr)
		snap.truncation = json.RawMessage(trJSON)
	}
	return snap
}

func finish(acc *accumulator) snapshot {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	full := strings.Join(acc.chunks, "")
	tr := truncate.TruncateTail(full, truncate.Options{})
	snap := snapshot{content: tr.Content}
	if tr.Truncated {
		acc.ensureTempFileLocked()
		snap.fullOutputPath = acc.tempFile
		trJSON, _ := json.Marshal(tr)
		snap.truncation = json.RawMessage(trJSON)
		// Persist full output to the temp file.
		if acc.tempFile != "" {
			_ = os.WriteFile(acc.tempFile, []byte(full), 0o644)
		}
	}
	return snap
}

// formatOutput appends the truncation notices.
func formatOutput(snap snapshot) string {
	text := snap.content
	if text == "" {
		text = "(no output)"
	}
	if snap.truncation == nil {
		return text
	}
	tr, _ := snap.truncation.(json.RawMessage)
	var t truncate.Result
	_ = json.Unmarshal(tr, &t)
	startLine := t.TotalLines - t.OutputLines + 1
	endLine := t.TotalLines
	switch {
	case t.LastLinePartial:
		text += fmt.Sprintf("\n\n[Showing last %s of line %d. Full output: %s]", truncate.FormatSize(t.OutputBytes), endLine, snap.fullOutputPath)
	case t.TruncatedBy == "lines":
		text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]", startLine, endLine, t.TotalLines, snap.fullOutputPath)
	default:
		text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]", startLine, endLine, t.TotalLines, truncate.FormatSize(truncate.DefaultMaxBytes), snap.fullOutputPath)
	}
	return text
}

// sanitizeOutput strips ANSI codes and removes \r and binary output.
func sanitizeOutput(s string) string {
	s = stripANSI(s)
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r == 0x09 || r == 0x0a || r == 0x0d:
			if r != 0x0d {
				sb.WriteRune(r)
			}
		case r <= 0x1f:
			// strip control chars
		case r >= 0xfff9 && r <= 0xfffb:
			// strip Unicode format chars
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// stripANSI removes ANSI escape sequences (subset of the reference regex).
func stripANSI(s string) string {
	if !strings.ContainsAny(s, "\x1b\x9b") {
		return s
	}
	var sb strings.Builder
	inEscape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEscape {
			if c >= 0x40 && c <= 0x7e {
				inEscape = false
			}
			continue
		}
		switch c {
		case 0x1b:
			// CSI sequence: ESC [ ... final byte
			inEscape = true
			if i+1 < len(s) && s[i+1] == '[' {
				i++
			}
		case 0x9b:
			inEscape = true
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}
