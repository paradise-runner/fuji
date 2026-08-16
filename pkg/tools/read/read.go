// Package read implements the read tool:
// text reads with offset/limit and head truncation, image reads with MIME
// detection (no resize pipeline in v1 — V6).
package read

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fuji/internal/truncate"
	"fuji/pkg/messages"
	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// Params mirrors the read tool parameters.
type Params struct {
	Path   string `json:"path"`
	Offset *int   `json:"offset"`
	Limit  *int   `json:"limit"`
}

// New builds the read tool Definition.
func New(cwd string) tools.Definition {
	params, _ := schema.Object([]string{"path"}, map[string]schema.Schema{
		"path":   schema.Str("Path to the file to read (relative or absolute)"),
		"offset": schema.Int("Line number to start reading from (1-indexed)"),
		"limit":  schema.Int("Maximum number of lines to read"),
	}).Raw()
	return tools.Definition{
		Name: "read", Label: "Read", Description: description(),
		PromptSnippet: "Read the contents of a file",
		Parameters:    params,
		Execute: func(callID string, raw json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			var p Params
			if err := json.Unmarshal(raw, &p); err != nil {
				return tools.ErrorResult(err), nil
			}
			return execute(cwd, p)
		},
	}
}

func description() string {
	return fmt.Sprintf("Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to %d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.",
		truncate.DefaultMaxLines, truncate.DefaultMaxBytes/1024)
}

func execute(cwd string, p Params) (tools.Result, error) {
	abs := tools.ResolvePath(p.Path, cwd)
	st, err := os.Stat(abs)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("cannot read file: %v", err)), nil
	}
	if st.IsDir() {
		return tools.ErrorResult(fmt.Errorf("path is a directory, not a file: %s", p.Path)), nil
	}
	// Image detection by extension.
	if mime := mimeFor(abs); mime != "" {
		return readImage(abs, mime)
	}
	return readText(abs, p)
}

func mimeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	}
	return ""
}

func readImage(abs, mime string) (tools.Result, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("cannot read image: %v", err)), nil
	}
	text := "Read image file [" + mime + "]"
	note := "(no resize pipeline in fuji v1)"
	return tools.Result{Content: []messages.ContentBlock{
		messages.TextContent{Type: messages.ContentText, Text: text + " " + note},
		messages.ImageContent{Type: messages.ContentImage, Data: base64.StdEncoding.EncodeToString(data), MimeType: mime},
	}}, nil
}

func readText(abs string, p Params) (tools.Result, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("cannot read file: %v", err)), nil
	}
	text := string(data)
	allLines := strings.Split(text, "\n")
	totalFileLines := len(allLines)

	startLine := 0
	if p.Offset != nil && *p.Offset > 0 {
		startLine = *p.Offset - 1
	}
	if startLine >= len(allLines) {
		return tools.ErrorResult(fmt.Errorf("offset %d is beyond end of file (%d lines total)", *p.Offset, totalFileLines)), nil
	}

	var selectedContent string
	userLimitedLines := -1
	if p.Limit != nil {
		end := startLine + *p.Limit
		if end > len(allLines) {
			end = len(allLines)
		}
		selectedContent = strings.Join(allLines[startLine:end], "\n")
		userLimitedLines = end - startLine
	} else {
		selectedContent = strings.Join(allLines[startLine:], "\n")
	}

	trunc := truncate.TruncateHead(selectedContent, truncate.Options{})
	startLineDisplay := startLine + 1
	var output string
	switch {
	case trunc.FirstLineExceedsLimit:
		firstLineSize := truncate.FormatSize(len(allLines[startLine]))
		output = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
			startLineDisplay, firstLineSize, truncate.FormatSize(truncate.DefaultMaxBytes), startLineDisplay, p.Path, truncate.DefaultMaxBytes)
	case trunc.Truncated:
		endLineDisplay := startLineDisplay + trunc.OutputLines - 1
		nextOffset := endLineDisplay + 1
		output = trunc.Content
		if trunc.TruncatedBy == "lines" {
			output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]",
				startLineDisplay, endLineDisplay, totalFileLines, nextOffset)
		} else {
			output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]",
				startLineDisplay, endLineDisplay, totalFileLines, truncate.FormatSize(truncate.DefaultMaxBytes), nextOffset)
		}
	case userLimitedLines >= 0 && startLine+userLimitedLines < len(allLines):
		remaining := len(allLines) - (startLine + userLimitedLines)
		nextOffset := startLine + userLimitedLines + 1
		output = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]", trunc.Content, remaining, nextOffset)
	default:
		output = trunc.Content
	}
	return tools.TextResult(output), nil
}
