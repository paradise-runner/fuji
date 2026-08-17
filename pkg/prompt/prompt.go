// Package prompt owns the system-prompt base text. The built-in default is a
// data file (base.md) embedded into the binary rather than a Go string so it
// can be edited independently of the agent-loop code. Users can override the
// base by dropping their own prompt.md into an agent resource dir
// (~/.fuji/prompt.md or .fuji/prompt.md, project > user, per ADR-007).
package prompt

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed base.md
var defaultBase string

// DefaultBase returns the embedded built-in base system prompt.
func DefaultBase() string { return defaultBase }

// ResolveBase selects the effective base system prompt. It reads the
// prompt.md override from the project resource dir first, then the user
// resource dir (project > user > embedded default), mirroring ADR-007's
// data-resource precedence for skills/templates.
func ResolveBase(projectDir, userDir string) string {
	for _, dir := range []string{projectDir, userDir} {
		if dir == "" {
			continue
		}
		text, err := os.ReadFile(filepath.Join(dir, "prompt.md"))
		if err == nil && string(text) != "" {
			return string(text)
		}
	}
	return defaultBase
}
