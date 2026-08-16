// Package write implements the write tool:
// full-file writes with automatic parent directory creation.
package write

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// Params defines the write tool parameters.
type Params struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// New builds the write tool Definition.
func New(cwd string) tools.Definition {
	params, _ := schema.Object([]string{"path", "content"}, map[string]schema.Schema{
		"path":    schema.Str("Path to the file to write (relative or absolute)"),
		"content": schema.Str("Content to write to the file"),
	}).Raw()
	return tools.Definition{
		Name: "write", Label: "Write",
		Description:   "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
		PromptSnippet: "Write content to a file",
		Parameters:    params,
		Execute: func(callID string, raw json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			var p Params
			if err := json.Unmarshal(raw, &p); err != nil {
				return tools.ErrorResult(err), nil
			}
			abs := tools.ResolvePath(p.Path, cwd)
			dir := filepath.Dir(abs)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return tools.ErrorResult(fmt.Errorf("cannot create directory: %v", err)), nil
			}
			if err := os.WriteFile(abs, []byte(p.Content), 0o644); err != nil {
				return tools.ErrorResult(fmt.Errorf("cannot write file: %v", err)), nil
			}
			return tools.TextResult(fmt.Sprintf("Successfully wrote %d bytes to %s", len(p.Content), p.Path)), nil
		},
	}
}
