// Package find implements the find tool: glob file discovery with gitignore
// awareness. fuji backs it with a stdlib walker (no external fd dependency;
// the embedded ripgrep is for the grep tool).
package find

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"fuji/internal/truncate"
	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// DefaultLimit is the find result cap.
const DefaultLimit = 1000

// Params defines the find tool parameters.
type Params struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Limit   *int   `json:"limit"`
}

// New builds the find tool Definition.
func New(cwd string) tools.Definition {
	params, _ := schema.Object([]string{"pattern"}, map[string]schema.Schema{
		"pattern": schema.Str("Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'"),
		"path":    schema.Str("Directory to search in (default: current directory)"),
		"limit":   schema.Int("Maximum number of results (default: 1000)"),
	}).Raw()
	return tools.Definition{
		Name: "find", Label: "Find files",
		Description:   "Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output is truncated to 1000 results or 50KB (whichever is hit first).",
		PromptSnippet: "Find files by glob pattern (respects .gitignore)",
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

func execute(cwd string, p Params) (tools.Result, error) {
	searchDir := tools.ResolvePath(p.Path, cwd)
	if p.Path == "" {
		searchDir = cwd
	}
	if st, err := os.Stat(searchDir); err != nil || !st.IsDir() {
		return tools.ErrorResult(fmt.Errorf("path not found: %s", searchDir)), nil
	}
	effectiveLimit := DefaultLimit
	if p.Limit != nil && *p.Limit > 0 {
		effectiveLimit = *p.Limit
	}

	pattern := p.Pattern
	if !strings.ContainsAny(pattern, "*?[{") {
		// Plain name: treat as exact filename match anywhere.
		pattern = "**/" + pattern
	}
	re, err := globToRegexp(pattern)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("invalid pattern: %v", err)), nil
	}

	ignore := loadGitignore(searchDir)
	var results []string
	root := searchDir
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if path == root {
			return nil
		}
		rel := strings.TrimPrefix(path, root+string(filepath.Separator))
		relPosix := tools.ToPosixPath(rel)
		base := d.Name()
		if d.IsDir() {
			if base == "node_modules" || base == ".git" {
				return filepath.SkipDir
			}
			if ignore != nil && ignore.MatchDir(relPosix) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignore != nil && ignore.Match(relPosix) {
			return nil
		}
		if re.MatchString(relPosix) || (strings.Contains(pattern, "/") && re.MatchString(relPosix)) {
			if len(results) >= effectiveLimit {
				return filepath.SkipAll
			}
			results = append(results, relPosix)
		}
		return nil
	})

	if len(results) == 0 {
		return tools.TextResult("No files found matching pattern"), nil
	}
	rawOutput := strings.Join(results, "\n")
	trunc := truncate.TruncateHead(rawOutput, truncate.Options{MaxLines: 1 << 30})
	output := trunc.Content
	var notices []string
	if len(results) >= effectiveLimit {
		notices = append(notices, fmt.Sprintf("%d results limit reached", effectiveLimit))
	}
	if trunc.Truncated {
		notices = append(notices, fmt.Sprintf("%s limit reached", truncate.FormatSize(truncate.DefaultMaxBytes)))
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}
	return tools.TextResult(output), nil
}

// globToRegexp converts a glob (with **) into an anchored regexp.
func globToRegexp(glob string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				// ** matches across directories
				if i+2 < len(glob) && (glob[i+2] == '/' || glob[i+2] == '\\') {
					sb.WriteString("(?:.*/)?")
					i += 2
				} else {
					sb.WriteString(".*")
					i++
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '.':
			sb.WriteString("\\.")
		case '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			sb.WriteString("\\")
			sb.WriteByte(c)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// gitignore is a minimal .gitignore matcher (pattern list per directory).
type gitignore struct {
	patterns []gitignorePattern
}

type gitignorePattern struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

func loadGitignore(root string) *gitignore {
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil
	}
	g := &gitignore{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := false
		if strings.HasPrefix(line, "!") {
			negate = true
			line = line[1:]
		}
		dirOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		re, err := globToRegexp("**/" + line)
		if err != nil {
			continue
		}
		g.patterns = append(g.patterns, gitignorePattern{re: re, negate: negate, dirOnly: dirOnly})
	}
	return g
}

// Match reports whether rel is ignored (last matching pattern wins).
func (g *gitignore) Match(rel string) bool {
	ignored := false
	for _, p := range g.patterns {
		if p.dirOnly {
			continue
		}
		if p.re.MatchString(rel) {
			ignored = !p.negate
		}
	}
	return ignored
}

// MatchDir reports whether a directory rel should be skipped.
func (g *gitignore) MatchDir(rel string) bool {
	for _, p := range g.patterns {
		if p.dirOnly && p.re.MatchString(rel) {
			return !p.negate
		}
	}
	return false
}
