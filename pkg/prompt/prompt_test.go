package prompt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fuji/pkg/prompt"
)

func TestDefaultBase(t *testing.T) {
	base := prompt.DefaultBase()
	if !strings.Contains(base, "You are a coding agent") {
		t.Errorf("default base missing identity: %q", base)
	}
}

func TestResolveBase(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user")
	proj := filepath.Join(root, "proj")
	os.MkdirAll(user, 0o755)
	os.MkdirAll(proj, 0o755)

	// No overrides → embedded default.
	if b := prompt.ResolveBase(proj, user); !strings.Contains(b, "coding agent") {
		t.Errorf("default: %q", b)
	}

	// User only.
	os.WriteFile(filepath.Join(user, "prompt.md"), []byte("USER PROMPT\n"), 0o644)
	if b := prompt.ResolveBase(proj, user); b != "USER PROMPT\n" {
		t.Errorf("user override: %q", b)
	}

	// Project wins over user.
	os.WriteFile(filepath.Join(proj, "prompt.md"), []byte("PROJECT PROMPT\n"), 0o644)
	if b := prompt.ResolveBase(proj, user); b != "PROJECT PROMPT\n" {
		t.Errorf("project precedence: %q", b)
	}
}
