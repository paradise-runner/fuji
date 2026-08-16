package tools

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolvePath resolves a tool path argument relative to cwd, expanding "~"
// and accepting absolute paths.
func ResolvePath(raw, cwd string) string {
	if raw == "" {
		return cwd
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, "~\\") {
		if home, err := os.UserHomeDir(); err == nil {
			raw = filepath.Join(home, raw[2:])
		}
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Clean(filepath.Join(cwd, raw))
}

// WithinCwd reports whether path is within base (used for cwd confinement).
func WithinCwd(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// ToPosixPath converts separators to '/'.
func ToPosixPath(p string) string {
	return strings.ReplaceAll(p, string(filepath.Separator), "/")
}
