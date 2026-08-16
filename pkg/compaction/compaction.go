// Package compaction implements session memory management (core-spec §4.9):
// token estimation, cut-point finding, compaction preparation, and the
// summarization prompt/driver.
package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"fuji/pkg/messages"
	"fuji/pkg/modelrt"
	"fuji/pkg/sessionmgr"
)

// EstimatedImageChars is the ESTIMATED_IMAGE_CHARS value used for image
// token estimation.
const EstimatedImageChars = 4800

// Settings mirrors DEFAULT_COMPACTION_SETTINGS.
type Settings struct {
	Enabled          bool
	ReserveTokens    int
	KeepRecentTokens int
}

// DefaultSettings returns the default compaction settings.
func DefaultSettings() Settings {
	return Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000}
}

// CalculateContextTokens sums provider usage.
func CalculateContextTokens(u messages.Usage) int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// ShouldCompact reports whether to compact: contextTokens > window - reserve.
func ShouldCompact(contextTokens, contextWindow int, settings Settings) bool {
	if !settings.Enabled {
		return false
	}
	return contextTokens > contextWindow-settings.ReserveTokens
}

// EstimateTokens estimates message tokens as chars/4 (conservative).
func EstimateTokens(m messages.AgentMessage) int {
	var chars int
	switch m.Role {
	case messages.RoleUser, messages.RoleCustom, messages.RoleToolResult:
		chars = estimateContentChars(m.Content)
	case messages.RoleAssistant:
		for _, blk := range m.Content.Blocks {
			switch v := blk.(type) {
			case messages.TextContent:
				chars += len(v.Text)
			case messages.ThinkingContent:
				chars += len(v.Thinking)
			case messages.ToolCall:
				chars += len(v.Name) + len(safeJSON(v.Arguments))
			}
		}
	case messages.RoleBashExecution:
		chars = len(m.Command) + len(m.Output)
	case messages.RoleBranchSummary, messages.RoleCompactionSummary:
		chars = len(m.Summary)
	}
	return ceilDiv(chars, 4)
}

func estimateContentChars(c messages.Content) int {
	if c.IsText() {
		return len(c.Text)
	}
	chars := 0
	for _, blk := range c.Blocks {
		switch v := blk.(type) {
		case messages.TextContent:
			chars += len(v.Text)
		case messages.ImageContent:
			chars += EstimatedImageChars
		}
	}
	return chars
}

func safeJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func ceilDiv(a, b int) int {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// ContextEstimate holds a context token estimate.
type ContextEstimate struct {
	Tokens         int
	UsageTokens    int
	TrailingTokens int
	LastUsageIndex int // -1 when none
}

// EstimateContextTokens uses the last valid assistant usage when available.
func EstimateContextTokens(msgs []messages.AgentMessage) ContextEstimate {
	usageIdx := -1
	var usage messages.Usage
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == messages.RoleAssistant && m.StopReason != messages.StopAborted && m.StopReason != messages.StopError && m.Usage != nil && CalculateContextTokens(*m.Usage) > 0 {
			usageIdx = i
			usage = *m.Usage
			break
		}
	}
	if usageIdx == -1 {
		est := 0
		for _, m := range msgs {
			est += EstimateTokens(m)
		}
		return ContextEstimate{Tokens: est, LastUsageIndex: -1}
	}
	usageTokens := CalculateContextTokens(usage)
	trailing := 0
	for i := usageIdx + 1; i < len(msgs); i++ {
		trailing += EstimateTokens(msgs[i])
	}
	return ContextEstimate{Tokens: usageTokens + trailing, UsageTokens: usageTokens, TrailingTokens: trailing, LastUsageIndex: usageIdx}
}

// --- cut-point finding -----------------------------------------------------

// CutPoint is the result of cut-point finding.
type CutPoint struct {
	FirstKeptEntryIndex int
	TurnStartIndex      int // -1 when not a split turn
	IsSplitTurn         bool
}

func validCutPoint(e sessionmgr.Entry) bool {
	switch e.Type {
	case sessionmgr.EntryMessage:
		if e.Message == nil {
			return false
		}
		switch e.Message.Role {
		case messages.RoleToolResult:
			return false
		}
		return true
	case sessionmgr.EntryBranchSummary, sessionmgr.EntryCustomMessage:
		return true
	}
	return false
}

// FindTurnStartIndex finds the user-visible message starting the turn.
func FindTurnStartIndex(entries []sessionmgr.Entry, entryIndex, startIndex int) int {
	for i := entryIndex; i >= startIndex; i-- {
		e := entries[i]
		if e.Type == sessionmgr.EntryBranchSummary || e.Type == sessionmgr.EntryCustomMessage {
			return i
		}
		if e.Type == sessionmgr.EntryMessage && e.Message != nil {
			if e.Message.Role == messages.RoleUser || e.Message.Role == messages.RoleBashExecution {
				return i
			}
		}
	}
	return -1
}

// FindCutPoint walks backwards accumulating message tokens until
// >= keepRecentTokens, snaps to a valid cut point, then walks back past
// non-message entries.
func FindCutPoint(entries []sessionmgr.Entry, startIndex, endIndex, keepRecentTokens int) CutPoint {
	var cutPoints []int
	for i := startIndex; i < endIndex; i++ {
		if validCutPoint(entries[i]) {
			cutPoints = append(cutPoints, i)
		}
	}
	if len(cutPoints) == 0 {
		return CutPoint{FirstKeptEntryIndex: startIndex, TurnStartIndex: -1}
	}
	accumulated := 0
	cutIndex := cutPoints[0]
	for i := endIndex - 1; i >= startIndex; i-- {
		e := entries[i]
		if e.Type != sessionmgr.EntryMessage || e.Message == nil {
			continue
		}
		accumulated += EstimateTokens(*e.Message)
		if accumulated >= keepRecentTokens {
			for _, c := range cutPoints {
				if c >= i {
					cutIndex = c
					break
				}
			}
			break
		}
	}
	for cutIndex > startIndex {
		prev := entries[cutIndex-1]
		if prev.Type == sessionmgr.EntryCompaction {
			break
		}
		if prev.Type == sessionmgr.EntryMessage {
			break
		}
		cutIndex--
	}
	cutEntry := entries[cutIndex]
	isUserMessage := cutEntry.Type == sessionmgr.EntryMessage && cutEntry.Message != nil && cutEntry.Message.Role == messages.RoleUser
	turnStart := -1
	if !isUserMessage {
		turnStart = FindTurnStartIndex(entries, cutIndex, startIndex)
	}
	return CutPoint{
		FirstKeptEntryIndex: cutIndex,
		TurnStartIndex:      turnStart,
		IsSplitTurn:         !isUserMessage && turnStart != -1,
	}
}

// --- summarization prompts -------------------------------------------------

// SummarizationSystemPrompt is the system prompt for summarization.
const SummarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

// SummarizationPrompt is the summarization prompt template.
const SummarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// UpdateSummarizationPrompt is the prompt template for updating an existing
// summary with new messages.
const UpdateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// SerializeConversation renders messages for the summarization prompt.
func SerializeConversation(msgs []messages.AgentMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case messages.RoleUser:
			fmt.Fprintf(&b, "<user>\n%s\n</user>\n", m.Content.TextOf())
		case messages.RoleAssistant:
			fmt.Fprintf(&b, "<assistant>\n%s\n</assistant>\n", m.Content.TextOf())
		case messages.RoleToolResult:
			fmt.Fprintf(&b, "<tool_result tool_name=\"%s\" is_error=\"%v\">\n%s\n</tool_result>\n", m.ToolName, m.IsError, m.Content.TextOf())
		case messages.RoleBashExecution:
			text := modelrt.BashExecutionToText(m)
			fmt.Fprintf(&b, "<user>\n%s\n</user>\n", text)
		case messages.RoleCompactionSummary:
			fmt.Fprintf(&b, "<compaction_summary>\n%s\n</compaction_summary>\n", m.Summary)
		case messages.RoleBranchSummary:
			fmt.Fprintf(&b, "<branch_summary>\n%s\n</branch_summary>\n", m.Summary)
		}
	}
	return b.String()
}

// Preparation holds the state needed to prepare a compaction.
type Preparation struct {
	PreviousSummary     string
	BoundaryStart       int
	MessagesToSummarize []messages.AgentMessage
	TurnPrefixMessages  []messages.AgentMessage
	FirstKeptEntryID    string
	TokensBefore        int
	CutPoint            CutPoint
}

// PrepareCompaction prepares a compaction from the path entries.
func PrepareCompaction(pathEntries []sessionmgr.Entry, settings Settings) (*Preparation, error) {
	if len(pathEntries) == 0 || pathEntries[len(pathEntries)-1].Type == sessionmgr.EntryCompaction {
		return nil, nil
	}
	prevCompactionIdx := -1
	for i := len(pathEntries) - 1; i >= 0; i-- {
		if pathEntries[i].Type == sessionmgr.EntryCompaction {
			prevCompactionIdx = i
			break
		}
	}
	var previousSummary string
	boundaryStart := 0
	if prevCompactionIdx >= 0 {
		previousSummary = pathEntries[prevCompactionIdx].Summary
		firstKeptIdx := -1
		for i := range pathEntries {
			if pathEntries[i].ID == pathEntries[prevCompactionIdx].FirstKeptEntryID {
				firstKeptIdx = i
				break
			}
		}
		if firstKeptIdx >= 0 {
			boundaryStart = firstKeptIdx
		} else {
			boundaryStart = prevCompactionIdx + 1
		}
	}
	boundaryEnd := len(pathEntries)
	tokensBefore := EstimateContextTokens(contextMessages(pathEntries)).Tokens
	cut := FindCutPoint(pathEntries, boundaryStart, boundaryEnd, settings.KeepRecentTokens)
	if cut.FirstKeptEntryIndex < 0 || cut.FirstKeptEntryIndex >= len(pathEntries) {
		return nil, fmt.Errorf("compaction: invalid cut point")
	}
	firstKept := pathEntries[cut.FirstKeptEntryIndex]
	if firstKept.ID == "" {
		return nil, fmt.Errorf("compaction: first kept entry has no id")
	}
	historyEnd := cut.FirstKeptEntryIndex
	if cut.IsSplitTurn {
		historyEnd = cut.TurnStartIndex
	}
	prep := &Preparation{
		PreviousSummary:  previousSummary,
		BoundaryStart:    boundaryStart,
		FirstKeptEntryID: firstKept.ID,
		TokensBefore:     tokensBefore,
		CutPoint:         cut,
	}
	for i := boundaryStart; i < historyEnd; i++ {
		if m := messageFromEntry(pathEntries[i]); m != nil {
			prep.MessagesToSummarize = append(prep.MessagesToSummarize, *m)
		}
	}
	if cut.IsSplitTurn {
		for i := cut.TurnStartIndex; i < cut.FirstKeptEntryIndex; i++ {
			if m := messageFromEntry(pathEntries[i]); m != nil {
				prep.TurnPrefixMessages = append(prep.TurnPrefixMessages, *m)
			}
		}
	}
	return prep, nil
}

func contextMessages(entries []sessionmgr.Entry) []messages.AgentMessage {
	var out []messages.AgentMessage
	for _, e := range entries {
		if e.Message != nil {
			out = append(out, *e.Message)
		}
	}
	return out
}

func messageFromEntry(e sessionmgr.Entry) *messages.AgentMessage {
	if e.Type != sessionmgr.EntryMessage || e.Message == nil {
		return nil
	}
	m := *e.Message
	return &m
}

// CompactionError classifies summarization failures.
type CompactionError struct {
	Code    string
	Message string
}

func (e *CompactionError) Error() string { return e.Message }

// GenerateSummary runs the summarization LLM call via modelrt. stream is a
// StreamFn (the session's retrying wrapper).
type SummaryStreamFn func(model modelrt.Model, ctx context.Context, c modelrt.Context, opts modelrt.StreamOptions) *modelrt.Stream

// GenerateSummary produces the summary text.
func GenerateSummary(ctx context.Context, model modelrt.Model, stream SummaryStreamFn, prep *Preparation, customInstructions, thinkingLevel string) (string, messages.Usage, error) {
	basePrompt := SummarizationPrompt
	if prep.PreviousSummary != "" {
		basePrompt = UpdateSummarizationPrompt
	}
	if customInstructions != "" {
		basePrompt += "\n\nAdditional focus: " + customInstructions
	}
	llmMessages := modelrt.ConvertToLLM(prep.MessagesToSummarize)
	conversationText := SerializeConversation(llmMessages)
	var promptText strings.Builder
	promptText.WriteString("<conversation>\n")
	promptText.WriteString(conversationText)
	promptText.WriteString("\n</conversation>\n\n")
	if prep.PreviousSummary != "" {
		promptText.WriteString("<previous-summary>\n")
		promptText.WriteString(prep.PreviousSummary)
		promptText.WriteString("\n</previous-summary>\n\n")
	}
	promptText.WriteString(basePrompt)

	summMessages := []messages.AgentMessage{
		messages.UserMessage(messages.Content{Blocks: []messages.ContentBlock{
			messages.TextContent{Type: messages.ContentText, Text: promptText.String()},
		}}, nowMs()),
	}

	s := stream(model, ctx, modelrt.Context{
		SystemPrompt: SummarizationSystemPrompt,
		Messages:     summMessages,
	}, modelrt.StreamOptions{ThinkingLevel: thinkingLevel})

	var final *messages.AgentMessage
	for ev := range s.Events() {
		if ev.Type == modelrt.EvStop || ev.Type == modelrt.EvError || ev.Type == modelrt.EvAborted {
			final = ev.Message
		}
	}
	if final == nil {
		return "", messages.Usage{}, &CompactionError{Code: "summarization_failed", Message: "summarization produced no response"}
	}
	if final.StopReason == messages.StopAborted {
		return "", messages.Usage{}, &CompactionError{Code: "aborted", Message: "summarization aborted"}
	}
	if final.StopReason == messages.StopError {
		return "", messages.Usage{}, &CompactionError{Code: "summarization_failed", Message: fmt.Sprintf("summarization failed: %s", final.ErrorMessage)}
	}
	usage := messages.Usage{}
	if final.Usage != nil {
		usage = *final.Usage
	}
	return final.Content.TextOf(), usage, nil
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}
