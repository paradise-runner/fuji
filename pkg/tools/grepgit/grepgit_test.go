package grepgit_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fuji/pkg/tools"
	"fuji/pkg/tools/git"
	"fuji/pkg/tools/grep"
)

func pathResolver(name string) func() (string, error) {
	return func() (string, error) {
		p, err := exec.LookPath(name)
		return p, err
	}
}

func run(t *testing.T, def tools.Definition, params string) tools.Result {
	t.Helper()
	res, err := def.Execute("call_1", json.RawMessage(params), context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res
}

func setupRepo(t *testing.T) string {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc hello() {\n\tprintln(\"hello world\")\n}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "other.txt"), []byte("nothing here\nhello too\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "skip.log"), []byte("hello in log\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644)
	// rg only applies .gitignore inside git repos (no --no-require-git).
	c := exec.Command("git", "init", "-q", "-b", "main")
	c.Dir = dir
	_ = c.Run()
	return dir
}

func TestGrepBasic(t *testing.T) {
	dir := setupRepo(t)
	def := grep.New(dir, pathResolver("rg"))
	res := run(t, def, `{"pattern":"hello"}`)
	if res.IsError {
		t.Fatalf("grep error: %q", res.ContentText())
	}
	out := res.ContentText()
	if !strings.Contains(out, "main.go:4:") || !strings.Contains(out, "other.txt:2:") {
		t.Errorf("output = %q", out)
	}
	// .gitignore respected.
	if strings.Contains(out, "skip.log") {
		t.Errorf("gitignore not respected: %q", out)
	}
}

func TestGrepNoMatch(t *testing.T) {
	dir := setupRepo(t)
	def := grep.New(dir, pathResolver("rg"))
	res := run(t, def, `{"pattern":"zzzzznope"}`)
	if res.ContentText() != "No matches found" {
		t.Errorf("output = %q", res.ContentText())
	}
}

func TestGrepIgnoreCaseAndLiteral(t *testing.T) {
	dir := setupRepo(t)
	def := grep.New(dir, pathResolver("rg"))
	res := run(t, def, `{"pattern":"HELLO","ignoreCase":true}`)
	if res.IsError || !strings.Contains(res.ContentText(), "main.go") {
		t.Errorf("ignoreCase: %q", res.ContentText())
	}
	// Literal: pattern with regex chars treated literally.
	_ = os.WriteFile(filepath.Join(dir, "dots.txt"), []byte("a.b\n"), 0o644)
	res = run(t, def, `{"pattern":"a.b","literal":true}`)
	if res.IsError || !strings.Contains(res.ContentText(), "dots.txt") {
		t.Errorf("literal: %q", res.ContentText())
	}
}

func TestGrepContext(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644)
	def := grep.New(dir, pathResolver("rg"))
	res := run(t, def, `{"pattern":"three","context":1}`)
	out := res.ContentText()
	if !strings.Contains(out, "f.txt-2- two") || !strings.Contains(out, "f.txt:3: three") || !strings.Contains(out, "f.txt-4- four") {
		t.Errorf("context output = %q", out)
	}
}

func TestGrepLimit(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("match me\n", 50)
	_ = os.WriteFile(filepath.Join(dir, "big.txt"), []byte(content), 0o644)
	def := grep.New(dir, pathResolver("rg"))
	res := run(t, def, `{"pattern":"match me","limit":5}`)
	out := res.ContentText()
	if !strings.Contains(out, "5 matches limit reached") {
		t.Errorf("no limit notice: %q", out)
	}
	if strings.Count(out, "big.txt:") > 5 {
		t.Errorf("too many matches: %q", out)
	}
}

func TestGrepLineTruncation(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 2000)
	_ = os.WriteFile(filepath.Join(dir, "long.txt"), []byte(long+"\n"), 0o644)
	def := grep.New(dir, pathResolver("rg"))
	res := run(t, def, `{"pattern":"xxx"}`)
	out := res.ContentText()
	if !strings.Contains(out, "... [truncated]") {
		t.Errorf("no line truncation: %.100q", out)
	}
	if !strings.Contains(out, "Some lines truncated") {
		t.Errorf("no truncation notice: %.200q", out)
	}
}

func TestGrepMissingRg(t *testing.T) {
	dir := t.TempDir()
	def := grep.New(dir, func() (string, error) { return "", nil })
	res := run(t, def, `{"pattern":"x"}`)
	if !res.IsError || !strings.Contains(res.ContentText(), "not available") {
		t.Errorf("output = %q", res.ContentText())
	}
}

func TestGitStatusAndLog(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Skip("git init failed")
	}
	execCmd := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		if err := c.Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	execCmd("config", "user.email", "t@t")
	execCmd("config", "user.name", "T")
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644)
	execCmd("add", "a.txt")
	execCmd("commit", "-qm", "initial commit")

	def := git.New(dir, pathResolver("git"))
	res := run(t, def, `{"args":["status","--short"]}`)
	if res.IsError {
		t.Fatalf("git status error: %q", res.ContentText())
	}
	res = run(t, def, `{"args":["log","--oneline"]}`)
	if res.IsError || !strings.Contains(res.ContentText(), "initial commit") {
		t.Errorf("log = %q", res.ContentText())
	}
	res = run(t, def, `{"args":["diff"]}`)
	if res.IsError {
		t.Errorf("diff error: %q", res.ContentText())
	}
}

func TestGitErrorIncludesStderr(t *testing.T) {
	dir := t.TempDir()
	def := git.New(dir, pathResolver("git"))
	res := run(t, def, `{"args":["log"]}`)
	if !res.IsError {
		t.Fatal("expected git error outside repo")
	}
	if !strings.Contains(res.ContentText(), "not a git repository") {
		t.Errorf("error = %q", res.ContentText())
	}
}

func TestGitRejectsShellMeta(t *testing.T) {
	dir := t.TempDir()
	def := git.New(dir, pathResolver("git"))
	res := run(t, def, `{"args":["log",";","rm","-rf","/"]}`)
	if !res.IsError {
		t.Fatal("expected rejection")
	}
}
