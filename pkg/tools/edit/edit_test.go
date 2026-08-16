package edit_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fuji/internal/editdiff"
	"fuji/internal/mutqueue"
	"fuji/pkg/tools"
	"fuji/pkg/tools/edit"
)

func run(t *testing.T, def tools.Definition, params string) tools.Result {
	t.Helper()
	res, err := def.Execute("call_1", json.RawMessage(params), context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res
}

func text(r tools.Result) string { return r.ContentText() }

func TestEditSingleReplacement(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(f, []byte("hello world\nfoo bar\n"), 0o644)
	def := edit.New(dir, mutqueue.New())
	res := run(t, def, `{"path":"x.txt","edits":[{"oldText":"hello world","newText":"goodbye world"}]}`)
	if res.IsError {
		t.Fatalf("edit failed: %q", text(res))
	}
	if !strings.Contains(text(res), "Successfully replaced 1 block(s)") {
		t.Errorf("result text = %q", text(res))
	}
	data, _ := os.ReadFile(f)
	if string(data) != "goodbye world\nfoo bar\n" {
		t.Errorf("content = %q", data)
	}
}

func TestEditMultipleDisjoint(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(f, []byte("alpha\nbeta\ngamma\n"), 0o644)
	def := edit.New(dir, mutqueue.New())
	res := run(t, def, `{"path":"x.txt","edits":[{"oldText":"alpha","newText":"ALPHA"},{"oldText":"gamma","newText":"GAMMA"}]}`)
	if res.IsError {
		t.Fatalf("edit failed: %q", text(res))
	}
	data, _ := os.ReadFile(f)
	if string(data) != "ALPHA\nbeta\nGAMMA\n" {
		t.Errorf("content = %q", data)
	}
}

func TestEditNotFoundError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(f, []byte("hello\n"), 0o644)
	def := edit.New(dir, mutqueue.New())
	res := run(t, def, `{"path":"x.txt","edits":[{"oldText":"nope","newText":"x"}]}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	want := "Could not find the exact text in x.txt. The old text must match exactly including all whitespace and newlines."
	if text(res) != want {
		t.Errorf("error = %q, want %q", text(res), want)
	}
}

func TestEditDuplicateError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(f, []byte("dup\ndup\n"), 0o644)
	def := edit.New(dir, mutqueue.New())
	res := run(t, def, `{"path":"x.txt","edits":[{"oldText":"dup","newText":"DUP"}]}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(text(res), "Found 2 occurrences") || !strings.Contains(text(res), "must be unique") {
		t.Errorf("error = %q", text(res))
	}
}

func TestEditOverlapError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(f, []byte("abcdef\n"), 0o644)
	def := edit.New(dir, mutqueue.New())
	res := run(t, def, `{"path":"x.txt","edits":[{"oldText":"abc","newText":"X"},{"oldText":"bcd","newText":"Y"}]}`)
	if !res.IsError {
		t.Fatal("expected overlap error")
	}
	if !strings.Contains(text(res), "overlap") {
		t.Errorf("error = %q", text(res))
	}
}

func TestEditCRLFPreserved(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(f, []byte("line1\r\nline2\r\n"), 0o644)
	def := edit.New(dir, mutqueue.New())
	res := run(t, def, `{"path":"x.txt","edits":[{"oldText":"line1","newText":"LINE1"}]}`)
	if res.IsError {
		t.Fatalf("edit failed: %q", text(res))
	}
	data, _ := os.ReadFile(f)
	if string(data) != "LINE1\r\nline2\r\n" {
		t.Errorf("content = %q", data)
	}
}

func TestEditFuzzyMatch(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	// Smart quotes in the file; model sends straight quotes → fuzzy match.
	_ = os.WriteFile(f, []byte("He said \u201Chello\u201D\n"), 0o644)
	def := edit.New(dir, mutqueue.New())
	res := run(t, def, `{"path":"x.txt","edits":[{"oldText":"He said \"hello\"","newText":"Said: hi"}]}`)
	if res.IsError {
		t.Fatalf("edit failed: %q", text(res))
	}
	data, _ := os.ReadFile(f)
	if !strings.Contains(string(data), "Said: hi") {
		t.Errorf("content = %q", data)
	}
}

func TestEditLegacyArgs(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(f, []byte("old text\n"), 0o644)
	def := edit.New(dir, mutqueue.New())
	// Legacy flat oldText/newText form.
	res := run(t, def, `{"path":"x.txt","oldText":"old text","newText":"new text"}`)
	if res.IsError {
		t.Fatalf("edit failed: %q", text(res))
	}
	data, _ := os.ReadFile(f)
	if string(data) != "new text\n" {
		t.Errorf("content = %q", data)
	}
}

func TestEditNoChangeError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(f, []byte("same\n"), 0o644)
	def := edit.New(dir, mutqueue.New())
	res := run(t, def, `{"path":"x.txt","edits":[{"oldText":"same","newText":"same"}]}`)
	if !res.IsError {
		t.Fatal("expected no-change error")
	}
	if !strings.Contains(text(res), "No changes made") {
		t.Errorf("error = %q", text(res))
	}
}

func TestEditMissingFile(t *testing.T) {
	dir := t.TempDir()
	def := edit.New(dir, mutqueue.New())
	res := run(t, def, `{"path":"nope.txt","edits":[{"oldText":"a","newText":"b"}]}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

// TestEditdiffUnit exercises the pure apply logic directly.
func TestEditdiffUnit(t *testing.T) {
	res, err := editdiff.Apply("a\nb\nc\n", []editdiff.Edit{{OldText: "b", NewText: "B"}}, "f")
	if err != nil || res.NewContent != "a\nB\nc\n" {
		t.Fatalf("got %+v err %v", res, err)
	}
	if _, err := editdiff.Apply("abc", []editdiff.Edit{{OldText: "x", NewText: "y"}}, "f"); err == nil {
		t.Error("expected not-found")
	}
	if _, err := editdiff.Apply("ab", []editdiff.Edit{{OldText: "a", NewText: "a"}}, "f"); err == nil {
		t.Error("expected no-change")
	}
}

func TestMutqueueSerializes(t *testing.T) {
	q := mutqueue.New()
	path := "/tmp/x.txt"
	var order []string
	firstHeld := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_ = q.WithLock(path, func() error {
			order = append(order, "first-start")
			close(firstHeld)
			<-done
			order = append(order, "first-end")
			return nil
		})
	}()
	<-firstHeld // ensure the first lock is held before the second attempts
	secondDone := make(chan struct{})
	go func() {
		_ = q.WithLock(path, func() error {
			order = append(order, "second")
			close(secondDone)
			return nil
		})
	}()
	select {
	case <-secondDone:
		t.Fatal("second lock acquired while first held")
	default:
	}
	close(done)
	<-secondDone
	if len(order) != 3 || order[0] != "first-start" || order[1] != "first-end" || order[2] != "second" {
		t.Errorf("order = %v", order)
	}
}
