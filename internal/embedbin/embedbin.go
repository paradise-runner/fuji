// Package embedbin implements fuji's embedded-binary bootstrap (ADR-006,
// core-spec §4.5): extract-from-embed → private cache dir → exec, with
// SHA-256 integrity checking and per-release version pinning. When the real
// binary is not embedded (default dev build), tools fall back to the host
// PATH.
package embedbin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Asset describes one embedded binary.
type Asset struct {
	// Name is the executable name (e.g. "rg").
	Name string
	// Version is the pinned release version (e.g. "14.1.1").
	Version string
	// PinnedSHA256 is the hex digest of the embedded blob; empty when only a
	// placeholder is embedded.
	PinnedSHA256 string
	// Blob is the embedded file content.
	Blob []byte
	// Available reports whether Blob is the real binary (not a placeholder).
	Available bool
}

// ExecutableName returns the platform executable filename (Windows .exe).
func (a Asset) ExecutableName() string {
	if runtime.GOOS == "windows" {
		return a.Name + ".exe"
	}
	return a.Name
}

// cacheFileName is the extracted file name (versioned for pinning).
func (a Asset) cacheFileName() string {
	return fmt.Sprintf("%s-%s", a.Name, a.Version)
}

// Ensure extracts the asset to cacheDir (creating it) and returns the
// executable path. When the real binary is not embedded, it falls back to
// PATH lookup and returns "", nil if not found.
func (a Asset) Ensure(cacheDir string) (string, error) {
	if !a.Available {
		p, err := exec.LookPath(a.Name)
		if err != nil {
			return "", nil
		}
		return p, nil
	}
	if a.PinnedSHA256 == "" {
		return "", fmt.Errorf("embedded %s has no pinned SHA-256; refusing to extract", a.Name)
	}
	// Verify the embedded blob matches its pin before extraction.
	sum := sha256.Sum256(a.Blob)
	if hex.EncodeToString(sum[:]) != a.PinnedSHA256 {
		return "", fmt.Errorf("embedded %s blob failed integrity check (pin mismatch); refusing to extract", a.Name)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create binary cache: %w", err)
	}
	dest := filepath.Join(cacheDir, a.cacheFileName())
	if runtime.GOOS == "windows" {
		dest += ".exe"
	}
	// Reuse the cached copy if it still matches the pin.
	if data, err := os.ReadFile(dest); err == nil {
		h := sha256.Sum256(data)
		if hex.EncodeToString(h[:]) == a.PinnedSHA256 {
			_ = os.Chmod(dest, 0o755)
			return dest, nil
		}
	}
	// Extract atomically: write temp, verify, rename.
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, a.Blob, 0o755); err != nil {
		return "", fmt.Errorf("extract %s: %w", a.Name, err)
	}
	if data, err := os.ReadFile(tmp); err == nil {
		h := sha256.Sum256(data)
		if hex.EncodeToString(h[:]) != a.PinnedSHA256 {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("extracted %s failed integrity check", a.Name)
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("install %s: %w", a.Name, err)
	}
	return dest, nil
}

// CacheDir returns the private binary cache directory under agentDir.
func CacheDir(agentDir string) string {
	if agentDir == "" {
		return ""
	}
	return filepath.Join(agentDir, "bin")
}

// FormatAssetID returns a stable per-release asset id (used by the release
// process and the GATE M4 audit).
func FormatAssetID(name, version string) string {
	return fmt.Sprintf("%s-%s-%s-%s", name, version, runtime.GOOS, runtime.GOARCH)
}

// Describe returns a human-readable availability summary for logs.
func (a Asset) Describe() string {
	if !a.Available {
		return fmt.Sprintf("%s: not embedded (falling back to PATH)", a.Name)
	}
	return fmt.Sprintf("%s: embedded v%s (sha256 %s)", a.Name, a.Version, shortHash(a.PinnedSHA256))
}

func shortHash(h string) string {
	if len(h) >= 12 {
		return h[:12]
	}
	return h
}

// SanitizeCommand ensures a command string contains no shell metacharacters
// (used for git arg validation — fuji passes args directly to execve, never
// through a shell, so this is defense-in-depth).
func SanitizeCommand(cmd string) bool {
	return !strings.ContainsAny(cmd, ";&|`$<>(){}[]!\\\"' \t\n")
}
