// Package frontmatter parses the YAML frontmatter block used by skill and
// prompt-template files (--- delimited). It supports the small YAML subset
// needed for metadata: scalar keys (string, boolean, number), quoted strings,
// and list values. Anything unsupported falls back to treating the whole file
// as body (parse errors degrade to frontmatter:{}).
package frontmatter

import (
	"strconv"
	"strings"
)

// Result is a parsed file.
type Result struct {
	Frontmatter    map[string]any
	Body           string
	HasFrontmatter bool
}

// Parse splits content on a leading "---" block.
func Parse(content string) Result {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	if !strings.HasPrefix(normalized, "---") {
		return Result{Frontmatter: map[string]any{}, Body: normalized}
	}
	endIdx := strings.Index(normalized, "\n---")
	if endIdx == -1 {
		return Result{Frontmatter: map[string]any{}, Body: normalized}
	}
	yamlString := normalized[4:endIdx]
	body := strings.TrimSpace(normalized[endIdx+4:])
	fm := parseYAMLSubset(yamlString)
	if fm == nil {
		fm = map[string]any{}
	}
	return Result{Frontmatter: fm, Body: body, HasFrontmatter: true}
}

// parseYAMLSubset parses simple "key: value" lines with list items.
func parseYAMLSubset(s string) map[string]any {
	out := map[string]any{}
	lines := strings.Split(s, "\n")
	var listKey string
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// List item under the current key.
		if strings.HasPrefix(line, "- ") && listKey != "" {
			val := parseScalar(strings.TrimSpace(strings.TrimPrefix(line, "- ")))
			existing, _ := out[listKey].([]any)
			out[listKey] = append(existing, val)
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		valStr := strings.TrimSpace(line[colon+1:])
		listKey = ""
		if strings.HasPrefix(valStr, "|") || strings.HasPrefix(valStr, ">") {
			continue // block scalars unsupported; body carries the content
		}
		if valStr == "" {
			listKey = key
			out[key] = []any{}
			continue
		}
		out[key] = parseScalar(valStr)
	}
	return out
}

func parseScalar(s string) any {
	// Comments.
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	// Quoted strings.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s[1 : len(s)-1]
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	case "null", "~":
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}
