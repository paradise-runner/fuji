// Package ls implements the ls tool: directory listing sorted alphabetically
// with '/' directory suffixes.
package ls

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fuji/internal/truncate"
	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// DefaultLimit is the ls entry cap.
const DefaultLimit = 500

// Params defines the ls tool parameters.
type Params struct {
	Path  string `json:"path"`
	Limit *int   `json:"limit"`
}

// New builds the ls tool Definition.
func New(cwd string) tools.Definition {
	params, _ := schema.Object(nil, map[string]schema.Schema{
		"path":  schema.Str("Directory to list (default: current directory)"),
		"limit": schema.Int("Maximum number of entries to return (default: 500)"),
	}).Raw()
	return tools.Definition{
		Name: "ls", Label: "List directory",
		Description: fmt.Sprintf("List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to %d entries or %dKB (whichever is hit first).", DefaultLimit, truncate.DefaultMaxBytes/1024),
		Parameters:  params,
		Execute: func(callID string, raw json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			var p Params
			if err := json.Unmarshal(raw, &p); err != nil {
				return tools.ErrorResult(err), nil
			}
			return execute(cwd, p)
		},
	}
}

func execute(cwd string, p Params) (tools.Result, error) {
	dirPath := tools.ResolvePath(p.Path, cwd)
	if p.Path == "" {
		dirPath = cwd
	}
	effectiveLimit := DefaultLimit
	if p.Limit != nil && *p.Limit > 0 {
		effectiveLimit = *p.Limit
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("cannot read directory: %v", err)), nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	var results []string
	entryLimitReached := false
	for _, e := range entries {
		if len(results) >= effectiveLimit {
			entryLimitReached = true
			break
		}
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		} else if info, err := e.Info(); err == nil && info.IsDir() {
			suffix = "/"
		}
		results = append(results, e.Name()+suffix)
	}
	if len(results) == 0 {
		return tools.TextResult("(empty directory)"), nil
	}
	rawOutput := strings.Join(results, "\n")
	trunc := truncate.TruncateHead(rawOutput, truncate.Options{MaxLines: 1 << 30})
	output := trunc.Content
	var notices []string
	if entryLimitReached {
		notices = append(notices, fmt.Sprintf("%d entries limit reached. Use limit=%d for more", effectiveLimit, effectiveLimit*2))
	}
	if trunc.Truncated {
		notices = append(notices, fmt.Sprintf("%s limit reached", truncate.FormatSize(truncate.DefaultMaxBytes)))
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}
	return tools.TextResult(output), nil
}

var _ = filepath.Separator
