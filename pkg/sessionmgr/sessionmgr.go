// Package sessionmgr implements JSONL v3 session persistence (core-spec §4.4,
// D2): append-only files, entries forming a tree via id/parentId, a movable
// leaf pointer for branching, compaction-aware context building, and v1/v2→v3
// migration. Session files are compatible with the standard agent session log
// format.
package sessionmgr

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fuji/internal/jsonl"
	"fuji/pkg/messages"
)

// CurrentSessionVersion is the JSONL format version fuji reads and writes.
const CurrentSessionVersion = 3

// EntryType identifies a session entry.
type EntryType string

// Entry types.
const (
	EntrySession       EntryType = "session"
	EntryMessage       EntryType = "message"
	EntryThinkingLevel EntryType = "thinking_level_change"
	EntryModelChange   EntryType = "model_change"
	EntryCompaction    EntryType = "compaction"
	EntryBranchSummary EntryType = "branch_summary"
	EntryCustom        EntryType = "custom"
	EntryCustomMessage EntryType = "custom_message"
	EntryLabel         EntryType = "label"
	EntrySessionInfo   EntryType = "session_info"
)

// SessionHeader is the first line of a session file.
type SessionHeader struct {
	Type          EntryType `json:"type"`
	Version       int       `json:"version"`
	ID            string    `json:"id"`
	Timestamp     string    `json:"timestamp"`
	Cwd           string    `json:"cwd"`
	ParentSession string    `json:"parentSession,omitempty"`
}

// Entry is one session entry. Field order mirrors the standard session
// object construction.
type Entry struct {
	Type      EntryType `json:"type"`
	ID        string    `json:"id"`
	ParentID  *string   `json:"parentId"`
	Timestamp string    `json:"timestamp"`

	// session header fields
	Version       int    `json:"version,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	ParentSession string `json:"parentSession,omitempty"`

	// message
	Message *messages.AgentMessage `json:"message,omitempty"`

	// thinking_level_change
	ThinkingLevel string `json:"thinkingLevel,omitempty"`

	// model_change
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`

	// compaction / branch_summary
	Summary           string          `json:"summary,omitempty"`
	FirstKeptEntryID  string          `json:"firstKeptEntryId,omitempty"`
	FirstKeptEntryIdx *int            `json:"firstKeptEntryIndex,omitempty"` // v1 only
	TokensBefore      int             `json:"tokensBefore,omitempty"`
	FromID            string          `json:"fromId,omitempty"`
	Details           json.RawMessage `json:"details,omitempty"`
	Usage             json.RawMessage `json:"usage,omitempty"`
	FromHook          *bool           `json:"fromHook,omitempty"`

	// custom / custom_message
	CustomType string           `json:"customType,omitempty"`
	Content    messages.Content `json:"content,omitempty"`
	Display    bool             `json:"display,omitempty"`

	// label / session_info
	TargetID string `json:"targetId,omitempty"`
	Label    string `json:"label,omitempty"`
	Name     string `json:"name,omitempty"`
}

// SessionContext is the resolved LLM-facing context (core-spec §4.4).
type SessionContext struct {
	Messages      []messages.AgentMessage
	ThinkingLevel string
	Model         *ModelRef // nil when unknown
}

// ModelRef identifies the active model.
type ModelRef struct {
	Provider string
	ModelID  string
}

// SessionInfo describes a session for listing.
type SessionInfo struct {
	Path              string
	ID                string
	Cwd               string
	Name              string
	ParentSessionPath string
	Created           time.Time
	Modified          time.Time
	MessageCount      int
	FirstMessage      string
	AllMessagesText   string
}

// TreeNode is a node of the session tree (GetTree).
type TreeNode struct {
	Entry          Entry
	Children       []*TreeNode
	Label          string
	LabelTimestamp string
}

// Manager is the session store.
type Manager struct {
	sessionID   string
	sessionFile string
	sessionDir  string
	cwd         string
	persist     bool
	flushed     bool

	fileEntries []Entry
	byID        map[string]*Entry
	labelsByID  map[string]string
	labelTsByID map[string]string
	leafID      *string

	// OnPersistError is invoked when a write to the session file fails
	// (disk full, IO error). The session continues in-memory (degraded mode,
	// core-spec §6).
	OnPersistError func(err error)
}

// NewSessionOptions holds the options for Create.
type NewSessionOptions struct {
	ID            string
	ParentSession string
}

// Create makes a new persisted session in sessionDir (or the default encoded
// cwd directory when sessionDir is empty).
func Create(cwd, sessionDir string, opts NewSessionOptions) *Manager {
	dir := sessionDir
	if dir == "" {
		dir = GetDefaultSessionDir(cwd, "")
	}
	m := NewManager(cwd, dir, "", true)
	m.newSession(opts)
	return m
}

// InMemory creates a non-persisted session.
func InMemory(cwd string, opts NewSessionOptions) *Manager {
	m := NewManager(cwd, "", "", false)
	m.newSession(opts)
	return m
}

// Open opens an existing session file. sessionDir and cwdOverride are optional.
func Open(path, sessionDir, cwdOverride string) (*Manager, error) {
	resolved := resolvePath(path)
	cwd := cwdOverride
	if cwd == "" {
		if st, err := os.Stat(resolved); err == nil && st.Size() > 0 {
			hdr, herr := ReadSessionHeader(resolved)
			if herr != nil {
				return nil, herr
			}
			if hdr != nil && hdr.Cwd != "" {
				cwd = hdr.Cwd
			}
		}
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	dir := sessionDir
	if dir == "" {
		dir = filepath.Dir(resolved)
	}
	m := NewManager(cwd, dir, resolved, true)
	return m, nil
}

// ContinueRecent opens the most recent session for cwd, or creates a new one.
func ContinueRecent(cwd, sessionDir string) (*Manager, error) {
	dir := sessionDir
	if dir == "" {
		dir = GetDefaultSessionDir(cwd, "")
	}
	filterCwd := sessionDir != "" && dir != defaultSessionDirPath(cwd)
	filter := ""
	if filterCwd {
		filter = cwd
	}
	mostRecent := FindMostRecentSession(dir, filter)
	if mostRecent != "" {
		return Open(mostRecent, dir, cwd)
	}
	return Create(cwd, dir, NewSessionOptions{}), nil
}

// NewManager is the low-level constructor. sessionFile empty means a new
// session is created.
func NewManager(cwd, sessionDir, sessionFile string, persist bool) *Manager {
	m := &Manager{
		cwd:         resolvePath(cwd),
		sessionDir:  normalizePath(sessionDir),
		persist:     persist,
		byID:        map[string]*Entry{},
		labelsByID:  map[string]string{},
		labelTsByID: map[string]string{},
	}
	if persist && sessionDir != "" {
		_ = os.MkdirAll(sessionDir, 0o755)
	}
	if sessionFile != "" {
		m.setSessionFile(sessionFile, nil)
	} else {
		m.newSession(NewSessionOptions{})
	}
	return m
}

// --- session lifecycle -----------------------------------------------------

func (m *Manager) setSessionFile(sessionFile string, preloaded []Entry) {
	m.sessionFile = resolvePath(sessionFile)
	st, statErr := os.Stat(m.sessionFile)
	if statErr == nil && st.Size() > 0 {
		entries := preloaded
		if entries == nil {
			entries, _ = LoadEntriesFromFile(m.sessionFile)
		}
		if len(entries) == 0 {
			// Non-empty file that did not parse as a valid session.
			m.newSession(NewSessionOptions{})
			m.sessionFile = sessionFile
			m.rewriteFile()
			m.flushed = true
			return
		}
		m.fileEntries = entries
		m.sessionID = ""
		for i := range m.fileEntries {
			if m.fileEntries[i].Type == EntrySession {
				m.sessionID = m.fileEntries[i].ID
				break
			}
		}
		if m.sessionID == "" {
			m.sessionID = newSessionID()
		}
		if migrateToCurrentVersion(m.fileEntries) {
			m.rewriteFile()
		}
		m.buildIndex()
		m.flushed = true
		return
	}
	if statErr == nil && st.Size() == 0 {
		// Empty file: initialize with a valid header.
		m.newSession(NewSessionOptions{})
		m.sessionFile = sessionFile
		m.rewriteFile()
		m.flushed = true
		return
	}
	// File does not exist.
	m.newSession(NewSessionOptions{})
	m.sessionFile = sessionFile // preserve explicit --session path
}

func (m *Manager) newSession(opts NewSessionOptions) {
	if opts.ID != "" {
		assertValidSessionID(opts.ID)
		m.sessionID = opts.ID
	} else {
		m.sessionID = newSessionID()
	}
	ts := nowISO()
	header := SessionHeader{
		Type:      EntrySession,
		Version:   CurrentSessionVersion,
		ID:        m.sessionID,
		Timestamp: ts,
		Cwd:       m.cwd,
	}
	if opts.ParentSession != "" {
		header.ParentSession = opts.ParentSession
	}
	m.fileEntries = []Entry{headerToEntry(header)}
	m.byID = map[string]*Entry{}
	m.labelsByID = map[string]string{}
	m.labelTsByID = map[string]string{}
	m.leafID = nil
	m.flushed = false
	if m.persist {
		fileTs := strings.NewReplacer(":", "-", ".", "-").Replace(ts)
		m.sessionFile = filepath.Join(m.sessionDir, fileTs+"_"+m.sessionID+".jsonl")
	}
}

func headerToEntry(h SessionHeader) Entry {
	return Entry{
		Type:          EntrySession,
		ID:            h.ID,
		Timestamp:     h.Timestamp,
		Version:       h.Version,
		Cwd:           h.Cwd,
		ParentSession: h.ParentSession,
	}
}

func (m *Manager) buildIndex() {
	m.byID = map[string]*Entry{}
	m.labelsByID = map[string]string{}
	m.labelTsByID = map[string]string{}
	m.leafID = nil
	for i := range m.fileEntries {
		e := &m.fileEntries[i]
		if e.Type == EntrySession {
			continue
		}
		m.byID[e.ID] = e
		id := e.ID
		m.leafID = &id
		if e.Type == EntryLabel {
			if e.Label != "" {
				m.labelsByID[e.TargetID] = e.Label
				m.labelTsByID[e.TargetID] = e.Timestamp
			} else {
				delete(m.labelsByID, e.TargetID)
				delete(m.labelTsByID, e.TargetID)
			}
		}
	}
}

// rewriteFile rewrites the entire session file.
func (m *Manager) rewriteFile() {
	if !m.persist || m.sessionFile == "" {
		return
	}
	f, err := os.Create(m.sessionFile)
	if err != nil {
		m.notifyPersistError(err)
		return
	}
	w := jsonl.NewWriter(f)
	for _, e := range m.fileEntries {
		if err := w.AppendRaw(marshalEntry(e)); err != nil {
			m.notifyPersistError(err)
		}
	}
	if err := w.Flush(); err != nil {
		m.notifyPersistError(err)
	}
	_ = f.Close()
}

// persistEntry writes a single entry per the deferred-write contract: the
// file is only created when the first assistant message arrives; before that,
// entries are held in memory.
func (m *Manager) persistEntry(entry Entry) {
	if !m.persist || m.sessionFile == "" {
		return
	}
	hasAssistant := false
	for i := range m.fileEntries {
		if m.fileEntries[i].Type == EntryMessage &&
			m.fileEntries[i].Message != nil && m.fileEntries[i].Message.Role == messages.RoleAssistant {
			hasAssistant = true
			break
		}
	}
	if !hasAssistant {
		if m.flushed {
			m.appendToFile(entry)
		} else {
			m.flushed = false
		}
		return
	}
	if !m.flushed {
		// Create the file and write all entries so far.
		f, err := os.OpenFile(m.sessionFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			f, err = os.OpenFile(m.sessionFile, os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return
			}
		}
		w := jsonl.NewWriter(f)
		for _, e := range m.fileEntries {
			_ = w.AppendRaw(marshalEntry(e))
		}
		_ = w.Flush()
		_ = f.Close()
		m.flushed = true
	} else {
		m.appendToFile(entry)
	}
}

func (m *Manager) appendToFile(entry Entry) {
	f, err := os.OpenFile(m.sessionFile, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		m.notifyPersistError(err)
		return
	}
	w := jsonl.NewWriter(f)
	if err := w.AppendRaw(marshalEntry(entry)); err != nil {
		m.notifyPersistError(err)
		_ = f.Close()
		return
	}
	if err := w.Flush(); err != nil {
		m.notifyPersistError(err)
	}
	_ = f.Close()
}

// notifyPersistError reports a write failure (degraded mode, non-fatal).
func (m *Manager) notifyPersistError(err error) {
	if m.OnPersistError != nil {
		m.OnPersistError(err)
	}
}

func (m *Manager) appendEntry(entry Entry) {
	m.fileEntries = append(m.fileEntries, entry)
	last := &m.fileEntries[len(m.fileEntries)-1]
	m.byID[entry.ID] = last
	id := entry.ID
	m.leafID = &id
	m.persistEntry(entry)
}

// --- appends ---------------------------------------------------------------

// AppendMessage appends a message entry. Returns the entry id.
func (m *Manager) AppendMessage(msg messages.AgentMessage) string {
	entry := Entry{
		Type:      EntryMessage,
		ID:        generateID(m.byID),
		ParentID:  m.leafID,
		Timestamp: nowISO(),
		Message:   &msg,
	}
	m.appendEntry(entry)
	return entry.ID
}

// AppendThinkingLevelChange appends a thinking_level_change entry.
func (m *Manager) AppendThinkingLevelChange(level string) string {
	entry := Entry{
		Type:          EntryThinkingLevel,
		ID:            generateID(m.byID),
		ParentID:      m.leafID,
		Timestamp:     nowISO(),
		ThinkingLevel: level,
	}
	m.appendEntry(entry)
	return entry.ID
}

// AppendModelChange appends a model_change entry.
func (m *Manager) AppendModelChange(provider, modelID string) string {
	entry := Entry{
		Type:      EntryModelChange,
		ID:        generateID(m.byID),
		ParentID:  m.leafID,
		Timestamp: nowISO(),
		Provider:  provider,
		ModelID:   modelID,
	}
	m.appendEntry(entry)
	return entry.ID
}

// AppendCompaction appends a compaction entry.
func (m *Manager) AppendCompaction(summary, firstKeptEntryID string, tokensBefore int, details, usage json.RawMessage, fromHook *bool) string {
	entry := Entry{
		Type:             EntryCompaction,
		ID:               generateID(m.byID),
		ParentID:         m.leafID,
		Timestamp:        nowISO(),
		Summary:          summary,
		FirstKeptEntryID: firstKeptEntryID,
		TokensBefore:     tokensBefore,
		Details:          details,
		Usage:            usage,
		FromHook:         fromHook,
	}
	m.appendEntry(entry)
	return entry.ID
}

// AppendCustomEntry appends a custom entry (read for format compatibility only).
func (m *Manager) AppendCustomEntry(customType string, data json.RawMessage) string {
	entry := Entry{
		Type:       EntryCustom,
		ID:         generateID(m.byID),
		ParentID:   m.leafID,
		Timestamp:  nowISO(),
		CustomType: customType,
		Details:    data,
	}
	m.appendEntry(entry)
	return entry.ID
}

// AppendCustomMessageEntry appends a custom_message entry (read-only, F1).
func (m *Manager) AppendCustomMessageEntry(customType string, content messages.Content, display bool, details json.RawMessage) string {
	entry := Entry{
		Type:       EntryCustomMessage,
		ID:         generateID(m.byID),
		ParentID:   m.leafID,
		Timestamp:  nowISO(),
		CustomType: customType,
		Content:    content,
		Display:    display,
		Details:    details,
	}
	m.appendEntry(entry)
	return entry.ID
}

// AppendLabelChange appends a label entry (read for format compatibility only).
func (m *Manager) AppendLabelChange(targetID, label string) (string, error) {
	if _, ok := m.byID[targetID]; !ok {
		return "", fmt.Errorf("entry %s not found", targetID)
	}
	entry := Entry{
		Type:      EntryLabel,
		ID:        generateID(m.byID),
		ParentID:  m.leafID,
		Timestamp: nowISO(),
		TargetID:  targetID,
		Label:     label,
	}
	m.appendEntry(entry)
	if label != "" {
		m.labelsByID[targetID] = label
		m.labelTsByID[targetID] = entry.Timestamp
	} else {
		delete(m.labelsByID, targetID)
		delete(m.labelTsByID, targetID)
	}
	return entry.ID, nil
}

// --- tree traversal --------------------------------------------------------

// GetLeafID returns the current leaf id (nil when empty).
func (m *Manager) GetLeafID() *string { return m.leafID }

// GetLeafEntry returns the current leaf entry (nil when empty).
func (m *Manager) GetLeafEntry() *Entry {
	if m.leafID == nil {
		return nil
	}
	return m.byID[*m.leafID]
}

// GetEntry returns an entry by id.
func (m *Manager) GetEntry(id string) *Entry { return m.byID[id] }

// GetLabel returns the label for an entry.
func (m *Manager) GetLabel(id string) string { return m.labelsByID[id] }

// GetBranch walks from fromID (or the leaf) to root, path order.
func (m *Manager) GetBranch(fromID *string) []Entry {
	start := fromID
	if start == nil {
		start = m.leafID
	}
	var current *Entry
	if start != nil {
		current = m.byID[*start]
	}
	var path []Entry
	for current != nil {
		path = append(path, *current)
		if current.ParentID == nil {
			break
		}
		current = m.byID[*current.ParentID]
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// BuildContextEntries returns the compaction-aware active entry list.
func (m *Manager) BuildContextEntries() []Entry {
	return buildContextEntries(m.GetEntries(), m.leafID, m.byID)
}

// BuildSessionContext resolves the LLM-facing context.
func (m *Manager) BuildSessionContext() SessionContext {
	entries := m.GetEntries()
	path := buildSessionPath(entries, m.leafID, m.byID)
	thinking, model := sessionContextSettings(path)
	ctxEntries := buildContextEntries(entries, m.leafID, m.byID)
	msgs := make([]messages.AgentMessage, 0, len(ctxEntries))
	for _, e := range ctxEntries {
		msgs = append(msgs, sessionEntryToContextMessages(e)...)
	}
	return SessionContext{Messages: msgs, ThinkingLevel: thinking, Model: model}
}

// GetTree returns the session tree with labels resolved.
func (m *Manager) GetTree() []*TreeNode {
	entries := m.GetEntries()
	nodeMap := map[string]*TreeNode{}
	var roots []*TreeNode
	for _, e := range entries {
		nodeMap[e.ID] = &TreeNode{
			Entry:          e,
			Label:          m.labelsByID[e.ID],
			LabelTimestamp: m.labelTsByID[e.ID],
		}
	}
	for _, e := range entries {
		node := nodeMap[e.ID]
		if e.ParentID == nil || *e.ParentID == e.ID {
			roots = append(roots, node)
			continue
		}
		if parent, ok := nodeMap[*e.ParentID]; ok {
			parent.Children = append(parent.Children, node)
		} else {
			roots = append(roots, node) // orphan
		}
	}
	sortTree(roots)
	return roots
}

func sortTree(nodes []*TreeNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		return parseTime(nodes[i].Entry.Timestamp).Before(parseTime(nodes[j].Entry.Timestamp))
	})
	for _, n := range nodes {
		sortTree(n.Children)
	}
}

// GetHeader returns the session header.
func (m *Manager) GetHeader() *SessionHeader {
	for i := range m.fileEntries {
		if m.fileEntries[i].Type == EntrySession {
			h := entryToHeader(m.fileEntries[i])
			return &h
		}
	}
	return nil
}

// GetSessionID returns the session id.
func (m *Manager) GetSessionID() string { return m.sessionID }

// GetSessionFile returns the session file path.
func (m *Manager) GetSessionFile() string { return m.sessionFile }

// GetSessionDir returns the session directory.
func (m *Manager) GetSessionDir() string { return m.sessionDir }

// GetCwd returns the session working directory.
func (m *Manager) GetCwd() string { return m.cwd }

// IsPersisted reports whether the manager writes to disk.
func (m *Manager) IsPersisted() bool { return m.persist }

// GetEntries returns all non-header entries (shallow copy).
func (m *Manager) GetEntries() []Entry {
	var out []Entry
	for _, e := range m.fileEntries {
		if e.Type != EntrySession {
			out = append(out, e)
		}
	}
	return out
}

// --- branching -------------------------------------------------------------

// Branch moves the leaf pointer to an earlier entry.
func (m *Manager) Branch(fromID string) error {
	if _, ok := m.byID[fromID]; !ok {
		return fmt.Errorf("entry %s not found", fromID)
	}
	m.leafID = &fromID
	return nil
}

// ResetLeaf moves the leaf pointer to null (before any entries).
func (m *Manager) ResetLeaf() { m.leafID = nil }

// BranchWithSummary moves the leaf and appends a branch_summary entry.
func (m *Manager) BranchWithSummary(fromID *string, summary string, details, usage json.RawMessage, fromHook *bool) (string, error) {
	if fromID != nil {
		if _, ok := m.byID[*fromID]; !ok {
			return "", fmt.Errorf("entry %s not found", *fromID)
		}
	}
	m.leafID = fromID
	from := "root"
	if fromID != nil {
		from = *fromID
	}
	entry := Entry{
		Type:      EntryBranchSummary,
		ID:        generateID(m.byID),
		ParentID:  fromID,
		Timestamp: nowISO(),
		FromID:    from,
		Summary:   summary,
		Details:   details,
		Usage:     usage,
		FromHook:  fromHook,
	}
	m.appendEntry(entry)
	return entry.ID, nil
}

// --- context building ------------------------------------------------------

func buildEntryIndex(entries []Entry, byID map[string]*Entry) map[string]*Entry {
	if byID != nil {
		return byID
	}
	idx := map[string]*Entry{}
	for i := range entries {
		idx[entries[i].ID] = &entries[i]
	}
	return idx
}

func buildSessionPath(entries []Entry, leafID *string, byID map[string]*Entry) []Entry {
	idx := buildEntryIndex(entries, byID)
	var leaf *Entry
	if leafID != nil {
		leaf = idx[*leafID]
	}
	if leaf == nil && len(entries) > 0 {
		leaf = &entries[len(entries)-1]
	}
	if leaf == nil {
		return nil
	}
	var path []Entry
	current := leaf
	for current != nil {
		path = append(path, *current)
		if current.ParentID == nil {
			break
		}
		next, ok := idx[*current.ParentID]
		if !ok {
			break
		}
		current = next
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

func sessionContextSettings(path []Entry) (string, *ModelRef) {
	thinking := "off"
	var model *ModelRef
	for _, e := range path {
		switch e.Type {
		case EntryThinkingLevel:
			thinking = e.ThinkingLevel
		case EntryModelChange:
			model = &ModelRef{Provider: e.Provider, ModelID: e.ModelID}
		case EntryMessage:
			if e.Message != nil && e.Message.Role == messages.RoleAssistant {
				model = &ModelRef{Provider: e.Message.Provider, ModelID: e.Message.Model}
			}
		}
	}
	return thinking, model
}

// sessionEntryToContextMessages projects one entry into LLM messages.
// Foreign entries (custom, custom_message, label, session_info) contribute
// nothing (F1).
func sessionEntryToContextMessages(e Entry) []messages.AgentMessage {
	switch e.Type {
	case EntryMessage:
		if e.Message == nil {
			return nil
		}
		m := *e.Message
		// Null/missing content becomes an empty array.
		if (m.Role == messages.RoleUser || m.Role == messages.RoleAssistant || m.Role == messages.RoleToolResult) &&
			!m.Content.IsText() && m.Content.Blocks == nil {
			m.Content = messages.Content{Blocks: []messages.ContentBlock{}}
		}
		return []messages.AgentMessage{m}
	case EntryBranchSummary:
		if e.Summary != "" {
			return []messages.AgentMessage{{
				Role:      messages.RoleBranchSummary,
				Summary:   e.Summary,
				FromID:    e.FromID,
				Timestamp: parseTimeMs(e.Timestamp),
			}}
		}
	case EntryCompaction:
		return []messages.AgentMessage{{
			Role:         messages.RoleCompactionSummary,
			Summary:      e.Summary,
			TokensBefore: e.TokensBefore,
			Timestamp:    parseTimeMs(e.Timestamp),
		}}
	case EntryCustomMessage:
		// F1: custom_message entries are ignored on read (not converted).
		return nil
	}
	return nil
}

// buildContextEntries picks the latest compaction on the leaf path, replacing
// older entries from firstKeptEntryId onward.
func buildContextEntries(entries []Entry, leafID *string, byID map[string]*Entry) []Entry {
	path := buildSessionPath(entries, leafID, byID)
	var compaction *Entry
	for i := range path {
		if path[i].Type == EntryCompaction {
			compaction = &path[i]
		}
	}
	if compaction == nil {
		return path
	}
	compactionIdx := -1
	for i := range path {
		if path[i].ID == compaction.ID {
			compactionIdx = i
			break
		}
	}
	if compactionIdx < 0 {
		return path
	}
	contextEntries := []Entry{*compaction}
	foundFirstKept := false
	for i := 0; i < compactionIdx; i++ {
		if path[i].ID == compaction.FirstKeptEntryID {
			foundFirstKept = true
		}
		if foundFirstKept {
			contextEntries = append(contextEntries, path[i])
		}
	}
	contextEntries = append(contextEntries, path[compactionIdx+1:]...)
	return contextEntries
}

// --- loading ---------------------------------------------------------------

// MaxSessionLineBytes bounds a single session-file line.
const MaxSessionLineBytes = 64 << 20

// LoadEntriesFromFile reads and parses a session file. Malformed lines are
// skipped; an invalid header yields no entries.
func LoadEntriesFromFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	rd := jsonl.NewReader(f, MaxSessionLineBytes)
	var out []Entry
	for {
		ent, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Oversized/overlong lines are skipped, not fatal.
			continue
		}
		var e Entry
		if err := json.Unmarshal(ent.Data, &e); err != nil {
			continue // skip malformed lines
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, nil
	}
	if out[0].Type != EntrySession || out[0].ID == "" {
		return nil, nil
	}
	return out, nil
}

// ReadSessionHeader reads the header of a session file.
func ReadSessionHeader(path string) (*SessionHeader, error) {
	entries, err := LoadEntriesFromFile(path)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	h := entryToHeader(entries[0])
	return &h, nil
}

// FindMostRecentSession returns the most recently modified matching session
// file, or "" if none. cwd is an optional filter.
func FindMostRecentSession(sessionDir, cwd string) string {
	matches, err := List(sessionDir, cwd)
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Modified.After(matches[j].Modified)
	})
	return matches[0].Path
}

// List returns session infos for a session dir, optionally filtered by cwd.
func List(sessionDir, cwd string) ([]SessionInfo, error) {
	files, err := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []SessionInfo
	for _, f := range files {
		hdr, err := ReadSessionHeader(f)
		if err != nil || hdr == nil {
			continue
		}
		if cwd != "" && hdr.Cwd != "" && resolvePath(hdr.Cwd) != resolvePath(cwd) {
			continue
		}
		info, err := buildSessionInfo(f)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

func buildSessionInfo(path string) (SessionInfo, error) {
	entries, err := LoadEntriesFromFile(path)
	if err != nil || len(entries) == 0 {
		return SessionInfo{}, fmt.Errorf("no entries")
	}
	st, err := os.Stat(path)
	if err != nil {
		return SessionInfo{}, err
	}
	hdr := entryToHeader(entries[0])
	info := SessionInfo{
		Path:     path,
		ID:       hdr.ID,
		Cwd:      hdr.Cwd,
		Created:  parseTime(hdr.Timestamp),
		Modified: st.ModTime(),
	}
	if hdr.ParentSession != "" {
		info.ParentSessionPath = hdr.ParentSession
	}
	var first string
	var all strings.Builder
	for _, e := range entries {
		if e.Type == EntryMessage && e.Message != nil {
			info.MessageCount++
			if first == "" {
				first = e.Message.Content.TextOf()
			}
			all.WriteString(e.Message.Content.TextOf())
			all.WriteString("\n")
		}
	}
	info.FirstMessage = first
	info.AllMessagesText = all.String()
	return info, nil
}

// --- ids and time ----------------------------------------------------------

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func generateID(byID map[string]*Entry) string {
	for i := 0; i < 100; i++ {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		id := hex.EncodeToString(b)
		if _, exists := byID[id]; !exists {
			return id
		}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func parseTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05.000Z", s)
	if t.IsZero() {
		t, _ = time.Parse(time.RFC3339Nano, s)
	}
	return t
}

func parseTimeMs(s string) int64 { return parseTime(s).UnixMilli() }

func boolPtr(b bool) *bool { return &b }

func assertValidSessionID(id string) {
	if id == "" {
		panic("session id must not be empty")
	}
	for _, r := range id {
		ok := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			panic("session id contains invalid characters")
		}
	}
}

func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return filepath.Clean(abs)
}

func normalizePath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// GetDefaultSessionDir returns the encoded sessions directory for a cwd.
func GetDefaultSessionDir(cwd, agentDir string) string {
	if agentDir == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			agentDir = filepath.Join(home, ".fuji")
		} else {
			agentDir = ".fuji"
		}
	}
	return filepath.Join(agentDir, "sessions", defaultSessionDirPath(cwd))
}

// defaultSessionDirPath encodes cwd as:
// "--" + cwd (leading slash stripped; / \ : → -) + "--".
func defaultSessionDirPath(cwd string) string {
	resolved := resolvePath(cwd)
	trimmed := strings.TrimLeft(resolved, "/\\")
	repl := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return "--" + repl.Replace(trimmed) + "--"
}

// --- serialization ---------------------------------------------------------

func entryToHeader(e Entry) SessionHeader {
	return SessionHeader{
		Type:          EntrySession,
		Version:       e.Version,
		ID:            e.ID,
		Timestamp:     e.Timestamp,
		Cwd:           e.Cwd,
		ParentSession: e.ParentSession,
	}
}

// marshalEntry serializes an entry with the standard field order.
func marshalEntry(e Entry) []byte {
	if e.Type == EntrySession {
		b, _ := json.Marshal(entryToHeader(e))
		return b
	}
	b, _ := json.Marshal(e)
	return b
}
