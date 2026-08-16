// Package logging provides structured JSON-lines logging to stderr for fleet
// observability (core-spec P8). Levels: debug < info < warn < error. Each
// record is one JSON object per line: {ts, level, msg, ...fields}.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level is a log level.
type Level int

// Levels.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel parses a --log-level value.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug, nil
	case "info", "":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	}
	return LevelInfo, fmt.Errorf("invalid log level %q (want debug|info|warn|error)", s)
}

// Logger writes JSON lines to w (default stderr).
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
}

// New creates a logger.
func New(w io.Writer, level Level) *Logger {
	if w == nil {
		w = os.Stderr
	}
	return &Logger{w: w, level: level}
}

// Default returns a stderr logger at info level.
func Default() *Logger { return New(os.Stderr, LevelInfo) }

// SetLevel adjusts the level at runtime.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// Debug logs at debug level.
func (l *Logger) Debug(msg string, fields ...any) { l.log(LevelDebug, msg, fields...) }

// Info logs at info level.
func (l *Logger) Info(msg string, fields ...any) { l.log(LevelInfo, msg, fields...) }

// Warn logs at warn level.
func (l *Logger) Warn(msg string, fields ...any) { l.log(LevelWarn, msg, fields...) }

// Error logs at error level.
func (l *Logger) Error(msg string, fields ...any) { l.log(LevelError, msg, fields...) }

func (l *Logger) log(level Level, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if level < l.level {
		return
	}
	rec := map[string]any{
		"ts":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"level": levelName(level),
		"msg":   msg,
	}
	for i := 0; i+1 < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			continue
		}
		rec[key] = fields[i+1]
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = l.w.Write(append(data, '\n'))
}

// SortedKeys returns a record's keys sorted (test helper).
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func levelName(l Level) string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	}
	return "info"
}
