package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fuji/pkg/tools"
	"fuji/pkg/tools/find"
	"fuji/pkg/tools/ls"
	"fuji/pkg/tools/read"
	"fuji/pkg/tools/write"
)

func run(t *testing.T, def tools.Definition, params string) tools.Result {
	t.Helper()
	var raw json.RawMessage = json.RawMessage(params)
	res, err := def.Execute("call_1", raw, context.Background(), nil)
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}
	return res
}

func textOf(r tools.Result) string {
	return r.ContentText()
}

func TestReadTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	def := read.New(dir)

	// Full read.
	res := run(t, def, `{"path":"file.txt"}`)
	if !strings.Contains(textOf(res), "line1") || !strings.Contains(textOf(res), "line5") {
		t.Errorf("full read = %q", textOf(res))
	}
	// Offset + limit (a continuation notice is added when the limit stops early).
	res = run(t, def, `{"path":"file.txt","offset":3,"limit":2}`)
	if textOf(res) != "line3\nline4\n\n[2 more lines in file. Use offset=5 to continue.]" {
		t.Errorf("offset read = %q", textOf(res))
	}
	// Offset with remaining lines and no limit → plain content (fits).
	res = run(t, def, `{"path":"file.txt","offset":4}`)
	if textOf(res) != "line4\nline5\n" {
		t.Errorf("offset remaining = %q", textOf(res))
	}
	// Offset beyond EOF → error.
	res = run(t, def, `{"path":"file.txt","offset":99}`)
	if !res.IsError || !strings.Contains(textOf(res), "beyond end of file") {
		t.Errorf("offset beyond = %q (isError=%v)", textOf(res), res.IsError)
	}
	// Missing file → error.
	res = run(t, def, `{"path":"nope.txt"}`)
	if !res.IsError {
		t.Error("expected error for missing file")
	}
	// Directory → error.
	res = run(t, def, `{"path":"."}`)
	if !res.IsError {
		t.Error("expected error for directory read")
	}
}

func TestReadTruncationNotice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	// 100 lines, 3000 bytes total — exceeds default 2000-line/50KB limits?
	// Use a small custom limit via offset semantics: read 100 lines with
	// no limit → truncateHead with defaults (2000 lines/50KB) won't trigger.
	// Force by writing a large single-line file that exceeds 50KB.
	big := strings.Repeat("x", 60*1024)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	def := read.New(dir)
	res := run(t, def, `{"path":"big.txt"}`)
	if !strings.Contains(textOf(res), "Showing lines") && !strings.Contains(textOf(res), "exceeds") {
		t.Errorf("no truncation notice: %.80q", textOf(res))
	}
}

func TestWriteTool(t *testing.T) {
	dir := t.TempDir()
	def := write.New(dir)
	res := run(t, def, `{"path":"sub/nested/out.txt","content":"hello world"}`)
	if res.IsError {
		t.Fatalf("write error: %q", textOf(res))
	}
	data, err := os.ReadFile(filepath.Join(dir, "sub", "nested", "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Errorf("content = %q", data)
	}
}

func TestLsTool(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "adir"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "bfile.txt"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "Afile.txt"), []byte("x"), 0o644)
	def := ls.New(dir)
	res := run(t, def, `{"path":"."}`)
	out := textOf(res)
	// Case-insensitive sort: adir/ (a-d), Afile.txt (a-f), bfile.txt.
	idxD, idxA, idxB := strings.Index(out, "adir/"), strings.Index(out, "Afile.txt"), strings.Index(out, "bfile.txt")
	if idxA < 0 || idxD < 0 || idxB < 0 || !(idxD < idxA && idxA < idxB) {
		t.Errorf("ls output = %q", out)
	}
	// Empty dir.
	empty := filepath.Join(dir, "empty")
	_ = os.MkdirAll(empty, 0o755)
	res = run(t, def, `{"path":"empty"}`)
	if textOf(res) != "(empty directory)" {
		t.Errorf("empty = %q", textOf(res))
	}
	// Missing dir → error.
	res = run(t, def, `{"path":"missing"}`)
	if !res.IsError {
		t.Error("expected error for missing dir")
	}
}

func TestFindTool(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "src", "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "src", "a.ts"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "src", "b.ts"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "src", "sub", "c.json"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "node_modules", "junk.js"), []byte("x"), 0o644)
	def := find.New(dir)

	res := run(t, def, `{"pattern":"**/*.ts"}`)
	out := textOf(res)
	if !strings.Contains(out, "src/a.ts") || !strings.Contains(out, "src/b.ts") {
		t.Errorf("ts find = %q", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("node_modules not ignored: %q", out)
	}
	// No match.
	res = run(t, def, `{"pattern":"**/*.xyz"}`)
	if textOf(res) != "No files found matching pattern" {
		t.Errorf("no match = %q", textOf(res))
	}
	// Missing dir.
	res = run(t, def, `{"pattern":"**/*.ts","path":"missing"}`)
	if !res.IsError {
		t.Error("expected error for missing search path")
	}
}

func TestFindRespectsGitignore(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\nbuild/\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "skip.log"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "build"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "build", "out.o"), []byte("x"), 0o644)
	def := find.New(dir)
	res := run(t, def, `{"pattern":"**/*"}`)
	out := textOf(res)
	if !strings.Contains(out, "keep.txt") {
		t.Errorf("keep.txt missing: %q", out)
	}
	if strings.Contains(out, "skip.log") || strings.Contains(out, "build") {
		t.Errorf("gitignored not skipped: %q", out)
	}
}
