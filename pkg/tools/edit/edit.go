// Package edit implements the edit tool: unique-match exact-text replacement
// with unified-diff semantics, BOM/line-ending preservation, fuzzy fallback,
// and file mutation queue serialization.
package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"fuji/internal/editdiff"
	"fuji/internal/mutqueue"
	"fuji/pkg/messages"
	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// Params defines the edit tool parameters.
type Params struct {
	Path  string `json:"path"`
	Edits []struct {
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
	} `json:"edits"`
}

// New builds the edit tool Definition. The queue serializes mutations.
func New(cwd string, queue *mutqueue.Queue) tools.Definition {
	if queue == nil {
		queue = mutqueue.New()
	}
	editSchema := schema.Object(nil, map[string]schema.Schema{
		"oldText": schema.Str("Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."),
		"newText": schema.Str("Replacement text for this targeted edit."),
	})
	params, _ := schema.Object([]string{"path", "edits"}, map[string]schema.Schema{
		"path":  schema.Str("Path to the file to edit (relative or absolute)"),
		"edits": schema.Arr("One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.", editSchema),
	}).Raw()
	return tools.Definition{
		Name: "edit", Label: "Edit",
		Description:   "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
		PromptSnippet: "Make precise file edits with exact text replacement, including multiple disjoint edits in one call",
		PromptGuidelines: []string{
			"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
			"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.",
		},
		Parameters: params,
		Execute: func(callID string, raw json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			var p Params
			if err := json.Unmarshal(raw, &p); err != nil {
				return tools.ErrorResult(err), nil
			}
			// Compatibility: some models send edits as a JSON string.
			if len(p.Edits) == 0 {
				var legacy struct {
					Path    string `json:"path"`
					OldText string `json:"oldText"`
					NewText string `json:"newText"`
					Edits   string `json:"edits"`
				}
				if err := json.Unmarshal(raw, &legacy); err == nil && legacy.Path != "" && (legacy.OldText != "" || legacy.Edits != "") {
					p.Path = legacy.Path
					if legacy.Edits != "" {
						_ = json.Unmarshal([]byte(legacy.Edits), &p.Edits)
					}
					if legacy.OldText != "" && legacy.NewText != "" {
						p.Edits = append(p.Edits, struct {
							OldText string `json:"oldText"`
							NewText string `json:"newText"`
						}{legacy.OldText, legacy.NewText})
					}
				}
			}
			if len(p.Edits) == 0 {
				return tools.ErrorResult(fmt.Errorf("edit tool input is invalid. edits must contain at least one replacement.")), nil
			}
			abs := tools.ResolvePath(p.Path, cwd)
			var result tools.Result
			err := queue.WithLock(abs, func() error {
				res, err := executeFile(abs, p)
				result = res
				return err
			})
			if err != nil {
				return tools.ErrorResult(err), nil
			}
			return result, nil
		},
	}
}

func executeFile(abs string, p Params) (tools.Result, error) {
	if _, err := os.Stat(abs); err != nil {
		return tools.ErrorResult(fmt.Errorf("could not edit file: %s. %v", p.Path, err)), nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("could not read file: %v", err)), nil
	}
	rawContent := string(data)
	bom, content := editdiff.StripBOM(rawContent)
	originalEnding := editdiff.DetectLineEnding(content)
	normalized := editdiff.NormalizeToLF(content)

	edits := make([]editdiff.Edit, len(p.Edits))
	for i, e := range p.Edits {
		edits[i] = editdiff.Edit{OldText: e.OldText, NewText: e.NewText}
	}
	res, err := editdiff.Apply(normalized, edits, p.Path)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	final := bom + editdiff.RestoreLineEndings(res.NewContent, originalEnding)
	if err := os.WriteFile(abs, []byte(final), 0o644); err != nil {
		return tools.ErrorResult(fmt.Errorf("could not write file: %v", err)), nil
	}
	diff, firstChanged := editdiff.GenerateDiff(res.BaseContent, res.NewContent)
	details, _ := json.Marshal(map[string]any{
		"diff":             diff,
		"firstChangedLine": firstChanged,
	})
	text := fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(p.Edits), p.Path)
	return tools.Result{
		Content: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: text}},
		Details: details,
	}, nil
}
