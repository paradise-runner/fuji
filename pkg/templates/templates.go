// Package templates implements file-based prompt templates (core-spec §4.6):
// *.md templates with frontmatter description and positional-argument
// expansion on Prompt.
package templates

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"fuji/internal/frontmatter"
)

// Template is a file-based prompt template.
type Template struct {
	Name        string
	Description string
	Content     string
}

// ParseFile parses a *.md template file.
func ParseFile(filePath, content string) Template {
	res := frontmatter.Parse(content)
	name := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	desc, _ := res.Frontmatter["description"].(string)
	if desc == "" {
		desc = firstLineDescription(res.Body)
	}
	return Template{Name: name, Description: desc, Content: res.Body}
}

func firstLineDescription(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) != "" {
			desc := strings.TrimSpace(line)
			if len(desc) > 60 {
				desc = desc[:60] + "..."
			}
			return desc
		}
	}
	return ""
}

// ParseArgs splits an argument string honoring single/double quotes.
func ParseArgs(argsString string) []string {
	var args []string
	var current strings.Builder
	inQuote := rune(0)
	for _, c := range argsString {
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			} else {
				current.WriteRune(c)
			}
		} else if c == '"' || c == '\'' {
			inQuote = c
		} else if c == ' ' || c == '\t' {
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		} else {
			current.WriteRune(c)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

var (
	posArgRe = regexp.MustCompile(`\$(\d+)`)
	sliceRe  = regexp.MustCompile(`\$\{@:(\d+)(?::(\d+))?\}`)
)

// SubstituteArgs replaces $1, $@, $ARGUMENTS, ${@:N}, ${@:N:L}.
func SubstituteArgs(content string, args []string) string {
	result := posArgRe.ReplaceAllStringFunc(content, func(m string) string {
		numStr := m[1:]
		n, err := strconv.Atoi(numStr)
		if err != nil || n < 1 || n > len(args) {
			return ""
		}
		return args[n-1]
	})
	result = sliceRe.ReplaceAllStringFunc(result, func(m string) string {
		sub := sliceRe.FindStringSubmatch(m)
		start, _ := strconv.Atoi(sub[1])
		start--
		if start < 0 {
			start = 0
		}
		if sub[2] != "" {
			length, _ := strconv.Atoi(sub[2])
			return strings.Join(sliceArgs(args, start, length), " ")
		}
		return strings.Join(sliceArgs(args, start, -1), " ")
	})
	allArgs := strings.Join(args, " ")
	result = strings.ReplaceAll(result, "$ARGUMENTS", allArgs)
	result = strings.ReplaceAll(result, "$@", allArgs)
	return result
}

func sliceArgs(args []string, start, length int) []string {
	if start >= len(args) {
		return nil
	}
	if length < 0 || start+length > len(args) {
		return args[start:]
	}
	return args[start : start+length]
}

// FormatInvocation expands a template invocation with positional args.
func FormatInvocation(t Template, args []string) string {
	return SubstituteArgs(t.Content, args)
}

var promptTemplateRe = regexp.MustCompile(`^\/([^\s]+)(?:\s+([\s\S]*))?$`)

// ExpandPromptTemplate expands "/name args" against the template list.
// Returns the original text when no template
// matches.
func ExpandPromptTemplate(text string, ts []Template) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}
	m := promptTemplateRe.FindStringSubmatch(text)
	if m == nil {
		return text
	}
	templateName := m[1]
	argsString := ""
	if len(m) > 2 {
		argsString = m[2]
	}
	for _, t := range ts {
		if t.Name == templateName {
			return SubstituteArgs(t.Content, ParseArgs(argsString))
		}
	}
	return text
}
