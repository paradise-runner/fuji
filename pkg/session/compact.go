package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"fuji/internal/truncate"
	"fuji/pkg/compaction"
	"fuji/pkg/loop"
	"fuji/pkg/messages"
)

// tailTruncate tail-truncates large bash output.
func tailTruncate(full string) string {
	r := truncate.TruncateTail(full, truncate.Options{})
	return r.Content
}

func writeTempFile(prefix, content string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	path := filepath.Join(os.TempDir(), prefix+hex.EncodeToString(b)+".log")
	_ = os.WriteFile(path, []byte(content), 0o644)
	return path
}

// CompactionSettings returns the session's compaction settings.
func (s *Session) CompactionSettings() compaction.Settings {
	return compaction.Settings{
		Enabled:          s.cfg.Config.AutoCompactEnabled,
		ReserveTokens:    s.cfg.Config.CompactionReserve,
		KeepRecentTokens: s.cfg.Config.KeepRecentTokens,
	}
}

// Compact performs manual compaction (core-spec §4.9). Runs the summarization
// LLM call, appends the compaction entry, and reloads context.
func (s *Session) Compact(customInstructions string) (CompactionResult, error) {
	s.mu.Lock()
	if s.IsStreaming() {
		s.mu.Unlock()
		return CompactionResult{}, errCompactingWhileStreaming
	}
	s.mu.Unlock()
	return s.runCompaction("manual", customInstructions, false)
}

// runCompaction is the shared compaction driver. willRetry indicates the
// caller intends to retry the overflowed request afterward.
func (s *Session) runCompaction(reason, customInstructions string, willRetry bool) (CompactionResult, error) {
	s.mu.Lock()
	if s.compactionRunning {
		s.mu.Unlock()
		return CompactionResult{}, &SessionError{"compaction already in progress"}
	}
	s.compactionRunning = true
	s.state = StateCompacting
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.compactionRunning = false
		s.state = StateIdle
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.abortCompaction = cancel
	s.mu.Unlock()
	defer cancel()

	s.emit(Event{Type: EvCompactionStart, CompactionReason: reason})

	entries := s.cfg.SessionManager.BuildContextEntries()
	prep, err := compaction.PrepareCompaction(entries, s.CompactionSettings())
	if err != nil {
		s.emit(Event{Type: EvCompactionEnd, CompactionReason: reason, CompactionError: err.Error(), CompactionAborted: false})
		return CompactionResult{}, err
	}
	if prep == nil {
		s.emit(Event{Type: EvCompactionEnd, CompactionReason: reason, CompactionResult: &CompactionResult{}, CompactionAborted: false})
		return CompactionResult{}, nil
	}

	summary, usage, err := compaction.GenerateSummary(ctx, s.model, s.retryingStreamFn, prep, customInstructions, s.thinking)
	if err != nil {
		s.emit(Event{Type: EvCompactionEnd, CompactionReason: reason, CompactionError: err.Error(), CompactionAborted: ctx.Err() != nil})
		return CompactionResult{}, err
	}

	usageRaw, _ := marshalUsage(usage)
	entryID := s.cfg.SessionManager.AppendCompaction(summary, prep.FirstKeptEntryID, prep.TokensBefore, nil, usageRaw, nil)

	// Reload context after compaction.
	sc := s.cfg.SessionManager.BuildSessionContext()
	s.mu.Lock()
	s.messages = sc.Messages
	s.mu.Unlock()

	result := CompactionResult{
		Summary:          summary,
		FirstKeptEntryID: prep.FirstKeptEntryID,
		TokensBefore:     prep.TokensBefore,
		EntryID:          entryID,
	}
	s.emit(Event{Type: EvCompactionEnd, CompactionReason: reason, CompactionResult: &result, CompactionAborted: false})
	return result, nil
}

// checkCompaction implements the between-turn compaction hook. Returns true
// when compaction ran (the run should stop after this turn).
func (s *Session) checkCompaction(assistant *messages.AgentMessage, skipAborted bool) bool {
	settings := s.CompactionSettings()
	if !settings.Enabled || assistant == nil {
		return false
	}
	if skipAborted && assistant.StopReason == messages.StopAborted {
		return false
	}
	contextWindow := s.model.ContextWindow
	if contextWindow <= 0 {
		return false
	}
	// Threshold check: use assistant usage when valid, else estimate.
	var contextTokens int
	if assistant.Usage != nil && assistant.StopReason != messages.StopError {
		contextTokens = compaction.CalculateContextTokens(*assistant.Usage)
	}
	if contextTokens == 0 {
		est := compaction.EstimateContextTokens(s.Messages())
		if est.LastUsageIndex == -1 {
			return false
		}
		contextTokens = est.Tokens
	}
	if compaction.ShouldCompact(contextTokens, contextWindow, settings) {
		_, err := s.runCompaction("threshold", "", false)
		return err == nil
	}
	return false
}

// compactionShouldStopHook is wired into the loop's ShouldStopAfterTurn.
func (s *Session) compactionShouldStopHook(sc loop.ShouldStopContext) bool {
	if sc.Message == nil {
		return false
	}
	return s.checkCompaction(sc.Message, true)
}

// --- helpers ---------------------------------------------------------------

func marshalUsage(u messages.Usage) ([]byte, error) {
	return json.Marshal(u)
}

var errCompactingWhileStreaming = &SessionError{"cannot compact while streaming"}

// SessionError is a user-facing session error.
type SessionError struct{ Msg string }

func (e *SessionError) Error() string { return e.Msg }
