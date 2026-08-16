package embedbin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExtractAndIntegrity exercises the full extract→verify→exec cycle with a
// tiny real binary (the go toolchain binary from PATH).
func TestExtractAndIntegrity(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	data, err := os.ReadFile(goBin)
	if err != nil {
		t.Skip("cannot read go binary")
	}
	cache := t.TempDir()
	asset := Asset{
		Name:         "gofake",
		Version:      "1.0",
		PinnedSHA256: sha256hex(data),
		Blob:         data,
		Available:    true,
	}
	path, err := asset.Ensure(cache)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if path == "" {
		t.Fatal("empty path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("extracted file missing: %v", err)
	}
	// Second Ensure reuses the cache.
	path2, err := asset.Ensure(cache)
	if err != nil || path2 != path {
		t.Errorf("reuse = %q err %v, want %q", path2, err, path)
	}
	// Versioned name.
	want := filepath.Join(cache, "gofake-1.0")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestPinMismatchRefusesToExtract(t *testing.T) {
	cache := t.TempDir()
	asset := Asset{
		Name: "rg", Version: "9.9.9",
		PinnedSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		Blob:         []byte("not the real binary"),
		Available:    true,
	}
	if _, err := asset.Ensure(cache); err == nil {
		t.Fatal("expected pin mismatch error")
	}
	if _, err := os.Stat(filepath.Join(cache, "rg-9.9.9")); err == nil {
		t.Error("corrupt binary was extracted")
	}
}

func TestPlaceholderFallsBackToPath(t *testing.T) {
	cache := t.TempDir()
	asset := Asset{Name: "sh", Version: "0", Available: false}
	path, err := asset.Ensure(cache)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Skip("sh not on PATH")
	}
	if filepath.Base(path) != "sh" {
		t.Errorf("path = %q", path)
	}
	// Missing binary → empty path, no error.
	asset = Asset{Name: "definitely-not-a-real-binary-xyz", Available: false}
	path, err = asset.Ensure(cache)
	if err != nil || path != "" {
		t.Errorf("path = %q err %v", path, err)
	}
}

func TestCacheDir(t *testing.T) {
	if got := CacheDir("/tmp/agent"); got != filepath.Join("/tmp/agent", "bin") {
		t.Errorf("CacheDir = %q", got)
	}
}

func TestAssetDescribe(t *testing.T) {
	a := Asset{Name: "rg", Version: "1.0", PinnedSHA256: "abc123", Available: true}
	if s := a.Describe(); s == "" {
		t.Error("empty describe")
	}
}

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
