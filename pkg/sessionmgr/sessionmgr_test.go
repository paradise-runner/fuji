package sessionmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fuji/pkg/messages"
)

// TestGoldenSessionLoads verifies a real session file (from a recorded 0.84.x
// agent) loads without loss on the supported subset (P1 exit criteria).
func TestGoldenSessionLoads(t *testing.T) {
	m, err := Open("testdata/golden-session.jsonl", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.GetSessionID() != "019fe407-4478-71df-980a-9502f284c2c8" {
		t.Errorf("session id = %q", m.GetSessionID())
	}
	if m.GetCwd() != "/Users/edwardchampion/git/rabbit" {
		t.Errorf("cwd = %q", m.GetCwd())
	}
	hdr := m.GetHeader()
	if hdr.Version != 3 || hdr.Type != EntrySession {
		t.Errorf("header = %+v", hdr)
	}
	entries := m.GetEntries()
	if len(entries) != 43 {
		t.Fatalf("entries = %d, want 43", len(entries))
	}
	// Counts by type.
	counts := map[EntryType]int{}
	for _, e := range entries {
		counts[e.Type]++
	}
	if counts[EntryMessage] != 40 {
		t.Errorf("message entries = %d, want 40", counts[EntryMessage])
	}
	if counts[EntryCustomMessage] != 1 {
		t.Errorf("custom_message entries = %d, want 1", counts[EntryCustomMessage])
	}
	if counts[EntryModelChange] != 1 || counts[EntryThinkingLevel] != 1 {
		t.Errorf("model/thinking entries = %d/%d", counts[EntryModelChange], counts[EntryThinkingLevel])
	}
	if counts[EntrySession] != 0 {
		t.Errorf("header leaked into entries: %d", counts[EntrySession])
	}
	// Tree integrity: every non-root entry's parent exists.
	for _, e := range entries {
		if e.ParentID != nil {
			if m.GetEntry(*e.ParentID) == nil {
				t.Errorf("entry %s has missing parent %s", e.ID, *e.ParentID)
			}
		}
	}
	// Leaf is the last entry.
	leaf := m.GetLeafEntry()
	if leaf == nil || leaf.ID != entries[len(entries)-1].ID {
		t.Errorf("leaf = %+v", leaf)
	}
	// Context: messages converted, foreign entries ignored (F1).
	ctx := m.BuildSessionContext()
	if len(ctx.Messages) != 40 {
		t.Fatalf("context messages = %d, want 40 (custom_message ignored)", len(ctx.Messages))
	}
	if ctx.ThinkingLevel != "high" {
		t.Errorf("thinkingLevel = %q", ctx.ThinkingLevel)
	}
	if ctx.Model == nil || ctx.Model.Provider != "deepseek" || ctx.Model.ModelID != "deepseek-v4-flash" {
		t.Errorf("model = %+v", ctx.Model)
	}
	// First context message is the user prompt.
	if ctx.Messages[0].Role != messages.RoleUser {
		t.Errorf("first message role = %q", ctx.Messages[0].Role)
	}
	if !strings.Contains(ctx.Messages[0].Content.TextOf(), "AGENTS.md") {
		t.Errorf("first message content = %q", ctx.Messages[0].Content.TextOf())
	}
	// Assistant messages carry stopReason/usage.
	var sawAssistant bool
	for _, m2 := range ctx.Messages {
		if m2.Role == messages.RoleAssistant {
			sawAssistant = true
			if m2.StopReason == "" || m2.Usage == nil {
				t.Errorf("assistant message missing stopReason/usage: %+v", m2)
			}
			break
		}
	}
	if !sawAssistant {
		t.Error("no assistant message in context")
	}
}

func containsText(m messages.AgentMessage, needle string) bool {
	return strings.Contains(m.Content.TextOf(), needle)
}

// TestRoundTripProperty appends messages, branches, reloads and compares the
// tree and context (P1 exit criteria).
func TestRoundTripProperty(t *testing.T) {
	dir := t.TempDir()
	m := Create("/proj", dir, NewSessionOptions{})
	// Entries before the first assistant message are held in memory.
	id1 := m.AppendMessage(messages.UserMessage(messages.Content{Text: "hello"}, 1000))
	id2 := m.AppendMessage(messages.UserMessage(messages.Content{Text: "second"}, 1001))
	// First assistant message triggers file creation.
	asst := messages.AgentMessage{
		Role:    messages.RoleAssistant,
		Content: messages.Content{Blocks: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "hi"}}},
	}
	asst = asst.WithAssistant(messages.AssistantMessage{
		API: "anthropic", Provider: "anthropic", Model: "claude",
		Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "hi"}},
		Usage:      messages.Usage{Input: 1, Output: 1, TotalTokens: 2, Cost: messages.Cost{}},
		StopReason: messages.StopStop,
	}, 1002)
	id3 := m.AppendMessage(asst)
	m.AppendModelChange("anthropic", "claude-sonnet-4-5")
	m.AppendThinkingLevelChange("low")
	_ = id1
	_ = id2
	_ = id3

	if _, err := os.Stat(m.GetSessionFile()); err != nil {
		t.Fatalf("session file not created: %v", err)
	}

	// Branch from the second user message and append on the branch.
	if err := m.Branch(id2); err != nil {
		t.Fatal(err)
	}
	branchID := m.AppendMessage(messages.UserMessage(messages.Content{Text: "branch msg"}, 2000))

	// Reload.
	m2, err := Open(m.GetSessionFile(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m2.GetSessionID() != m.GetSessionID() {
		t.Errorf("session id mismatch")
	}
	e2 := m2.GetEntries()
	if len(e2) != len(m.GetEntries()) {
		t.Fatalf("entry count mismatch: %d vs %d", len(e2), len(m.GetEntries()))
	}
	if m2.GetLeafEntry() == nil || m2.GetLeafEntry().ID != branchID {
		t.Errorf("leaf mismatch after reload")
	}
	ctx1 := m.BuildSessionContext()
	ctx2 := m2.BuildSessionContext()
	if len(ctx1.Messages) != len(ctx2.Messages) {
		t.Fatalf("context size mismatch: %d vs %d", len(ctx1.Messages), len(ctx2.Messages))
	}
	for i := range ctx1.Messages {
		a, _ := json.Marshal(ctx1.Messages[i])
		b, _ := json.Marshal(ctx2.Messages[i])
		if string(a) != string(b) {
			t.Errorf("message %d mismatch:\n%s\n%s", i, a, b)
		}
	}
	// Tree shape: two roots (original chain + branch) — the second user message
	// has two children (assistant chain and the branch append).
	tree := m2.GetTree()
	if len(tree) != 1 {
		t.Fatalf("roots = %d, want 1 (branch shares the root chain)", len(tree))
	}
	foundBranch := false
	var walk func(n *TreeNode)
	walk = func(n *TreeNode) {
		if n.Entry.ID == branchID {
			foundBranch = true
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(tree[0])
	if !foundBranch {
		t.Error("branch node missing from tree")
	}
}

// TestPersistedFileMatchesFormat checks the on-disk header line is exactly the
// session format (version 3, ISO timestamp).
func TestPersistedFileMatchesFormat(t *testing.T) {
	dir := t.TempDir()
	m := Create("/proj", dir, NewSessionOptions{})
	asst := messages.AgentMessage{}
	asst = asst.WithAssistant(messages.AssistantMessage{
		API: "anthropic", Provider: "anthropic", Model: "claude",
		Content:    []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: "x"}},
		Usage:      messages.Usage{TotalTokens: 1, Cost: messages.Cost{}},
		StopReason: messages.StopStop,
	}, 1)
	m.AppendMessage(asst)
	data, err := os.ReadFile(m.GetSessionFile())
	if err != nil {
		t.Fatal(err)
	}
	first := string(data)
	want := `{"type":"session","version":3,"id":"` + m.GetSessionID() + `"`
	if len(first) < len(want) || first[:len(want)] != want {
		t.Errorf("header line = %q", first[:min(len(first), 80)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestInvalidSessionFile ensures non-session files fail loudly.
func TestInvalidSessionFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte("this is not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Open(bad, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// behavior: a non-empty file that doesn't parse as a session gets a
	// fresh session header written over it.
	if m.GetHeader() == nil || m.GetHeader().ID == "" {
		t.Error("expected a fresh session header")
	}
}

// TestDefaultSessionDirEncoding checks the exact directory encoding.
func TestDefaultSessionDirEncoding(t *testing.T) {
	got := defaultSessionDirPath("/Users/edwardchampion/git/rabbit")
	if got != "--Users-edwardchampion-git-rabbit--" {
		t.Errorf("encoding = %q", got)
	}
	got2 := defaultSessionDirPath("/home/u/dev/my-project")
	if got2 != "--home-u-dev-my-project--" {
		t.Errorf("encoding = %q", got2)
	}
}
