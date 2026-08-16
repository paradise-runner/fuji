// Package grep implements the grep tool: ripgrep-backed content search with
// JSON output parsing, context lines, match limiting, and line truncation.
package grep

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fuji/internal/truncate"
	"fuji/pkg/exec"
	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// DefaultLimit is the match cap.
const DefaultLimit = 100

// Params defines the grep tool parameters.
type Params struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignoreCase"`
	Literal    bool   `json:"literal"`
	Context    *int   `json:"context"`
	Limit      *int   `json:"limit"`
}

// Resolver locates the rg binary (embedded or PATH).
type Resolver func() (string, error)

// New builds the grep tool Definition. resolve locates rg.
func New(cwd string, resolve Resolver) tools.Definition {
	params, _ := schema.Object([]string{"pattern"}, map[string]schema.Schema{
		"pattern":    schema.Str("Search pattern (regex or literal string)"),
		"path":       schema.Str("Directory or file to search (default: current directory)"),
		"glob":       schema.Str("Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'"),
		"ignoreCase": schema.Bool("Case-insensitive search (default: false)"),
		"literal":    schema.Bool("Treat pattern as literal string instead of regex (default: false)"),
		"context":    schema.Int("Number of lines to show before and after each match (default: 0)"),
		"limit":      schema.Int("Maximum number of matches to return (default: 100)"),
	}).Raw()
	return tools.Definition{
		Name: "grep", Label: "grep",
		Description:   fmt.Sprintf("Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Output is truncated to %d matches or %dKB (whichever is hit first). Long lines are truncated to %d chars.", DefaultLimit, truncate.DefaultMaxBytes/1024, truncate.GreplineMaxLength),
		PromptSnippet: "Search file contents for patterns (respects .gitignore)",
		Parameters:    params,
		Execute: func(callID string, raw json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			var p Params
			if err := json.Unmarshal(raw, &p); err != nil {
				return tools.ErrorResult(err), nil
			}
			return execute(cwd, p, resolve)
		},
	}
}

type rgMatch struct {
	filePath   string
	lineNumber int
	lineText   string
}

func execute(cwd string, p Params, resolve Resolver) (tools.Result, error) {
	rgPath, err := resolve()
	if err != nil || rgPath == "" {
		return tools.ErrorResult(fmt.Errorf("ripgrep (rg) is not available and could not be downloaded")), nil
	}
	searchPath := tools.ResolvePath(p.Path, cwd)
	if p.Path == "" {
		searchPath = cwd
	}
	if _, err := os.Stat(searchPath); err != nil {
		return tools.ErrorResult(fmt.Errorf("path not found: %s", searchPath)), nil
	}
	isDir, _ := os.Stat(searchPath)
	dirMode := isDir != nil && isDir.IsDir()

	contextValue := 0
	if p.Context != nil && *p.Context > 0 {
		contextValue = *p.Context
	}
	effectiveLimit := DefaultLimit
	if p.Limit != nil && *p.Limit > 0 {
		effectiveLimit = *p.Limit
	}

	args := []string{"--json", "--line-number", "--color=never", "--hidden"}
	if p.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if p.Literal {
		args = append(args, "--fixed-strings")
	}
	if p.Glob != "" {
		args = append(args, "--glob", p.Glob)
	}
	args = append(args, "--", p.Pattern, searchPath)

	cmd := exec.New(rgPath, args...).WithDir(cwd)
	res, runErr := cmd.Run(context.Background())
	if runErr != nil && res.ExitCode != 0 && res.ExitCode != 1 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("ripgrep exited with code %d", res.ExitCode)
		}
		return tools.ErrorResult(fmt.Errorf("%s", msg)), nil
	}

	var matches []rgMatch
	linesTruncated := false
	sc := bufio.NewScanner(strings.NewReader(res.Stdout))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || len(matches) >= effectiveLimit {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Path       *struct{ Text string } `json:"path"`
				LineNumber int                    `json:"line_number"`
				Lines      *struct{ Text string } `json:"lines"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Type != "match" {
			continue
		}
		filePath := ""
		if ev.Data.Path != nil {
			filePath = ev.Data.Path.Text
		}
		lineText := ""
		if ev.Data.Lines != nil {
			lineText = strings.TrimSuffix(strings.ReplaceAll(ev.Data.Lines.Text, "\r", ""), "\n")
		}
		if filePath != "" && ev.Data.LineNumber > 0 {
			matches = append(matches, rgMatch{filePath: filePath, lineNumber: ev.Data.LineNumber, lineText: lineText})
		}
	}

	if len(matches) == 0 {
		return tools.TextResult("No matches found"), nil
	}

	// Read file lines for context rendering.
	fileCache := map[string][]string{}
	getLines := func(fp string) []string {
		if l, ok := fileCache[fp]; ok {
			return l
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			fileCache[fp] = nil
			return nil
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		fileCache[fp] = lines
		return lines
	}

	var outputLines []string
	for _, m := range matches {
		relPath := formatPath(m.filePath, searchPath, dirMode)
		if contextValue == 0 && m.lineText != "" {
			text, wasTrunc := truncate.TruncateLine(m.lineText, 0)
			if wasTrunc {
				linesTruncated = true
			}
			outputLines = append(outputLines, fmt.Sprintf("%s:%d: %s", relPath, m.lineNumber, text))
			continue
		}
		lines := getLines(m.filePath)
		if len(lines) == 0 {
			outputLines = append(outputLines, fmt.Sprintf("%s:%d: (unable to read file)", relPath, m.lineNumber))
			continue
		}
		start := m.lineNumber
		end := m.lineNumber
		if contextValue > 0 {
			start = max(1, m.lineNumber-contextValue)
			end = min(len(lines), m.lineNumber+contextValue)
		}
		for cur := start; cur <= end; cur++ {
			lineText := strings.TrimSuffix(lines[cur-1], "\r")
			text, wasTrunc := truncate.TruncateLine(lineText, 0)
			if wasTrunc {
				linesTruncated = true
			}
			if cur == m.lineNumber {
				outputLines = append(outputLines, fmt.Sprintf("%s:%d: %s", relPath, cur, text))
			} else {
				outputLines = append(outputLines, fmt.Sprintf("%s-%d- %s", relPath, cur, text))
			}
		}
	}

	rawOutput := strings.Join(outputLines, "\n")
	trunc := truncate.TruncateHead(rawOutput, truncate.Options{MaxLines: 1 << 30})
	output := trunc.Content
	var notices []string
	matchLimitReached := len(matches) >= effectiveLimit
	if matchLimitReached {
		notices = append(notices, fmt.Sprintf("%d matches limit reached. Use limit=%d for more, or refine pattern", effectiveLimit, effectiveLimit*2))
	}
	if trunc.Truncated {
		notices = append(notices, fmt.Sprintf("%s limit reached", truncate.FormatSize(truncate.DefaultMaxBytes)))
	}
	if linesTruncated {
		notices = append(notices, fmt.Sprintf("Some lines truncated to %d chars. Use read tool to see full lines", truncate.GreplineMaxLength))
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}
	return tools.TextResult(output), nil
}

func formatPath(filePath, searchPath string, isDirectory bool) string {
	if isDirectory {
		if rel, err := filepath.Rel(searchPath, filePath); err == nil && rel != "" && !strings.HasPrefix(rel, "..") {
			return tools.ToPosixPath(rel)
		}
	}
	return filepath.Base(filePath)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
