// Package resource implements the ResourceLoader (core-spec §4.6, ADR-007):
// discovery of skills and prompt templates from explicit --skills/--templates
// directories, project .fuji/, and user ~/.fuji/. Resources are data-only:
// parsed, never executed.
package resource

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fuji/pkg/prompt"
	"fuji/pkg/skills"
	"fuji/pkg/templates"
)

// Discovery result.
type Result struct {
	Skills    []skills.Skill
	Templates []templates.Template
	// BasePrompt is the effective base system-prompt text: the user/project
	// prompt.md override (project > user) or the embedded default.
	BasePrompt string
	// Diagnostics are non-fatal warnings (unreadable/parse-failed files).
	Diagnostics []string
}

// DiscoveryOrder is flag > project > user (ADR-007).
const (
	LayerFlag    = "flag"
	LayerProject = "project"
	LayerUser    = "user"
)

// Options for Discover.
type Options struct {
	// SkillsDirs are explicit --skills directories (highest precedence).
	SkillsDirs []string
	// TemplatesDirs are explicit --templates directories.
	TemplatesDirs []string
	// ProjectDir is the project .fuji directory (may be "").
	ProjectDir string
	// UserDir is the user ~/.fuji directory.
	UserDir string
}

// Discover loads skills and templates from all configured locations,
// higher-precedence locations winning on duplicate names (flag > project >
// user). Order follows first-seen (user layer order, replaced in place).
func Discover(opts Options) Result {
	var res Result
	skillIdx := map[string]int{}
	templateIdx := map[string]int{}

	loadSkills := func(dir string) {
		if dir == "" {
			return
		}
		for _, s := range LoadSkillsDir(dir) {
			if i, ok := skillIdx[s.Name]; ok {
				res.Skills[i] = s // higher precedence replaces in place
			} else {
				skillIdx[s.Name] = len(res.Skills)
				res.Skills = append(res.Skills, s)
			}
		}
	}
	loadTemplates := func(dir string) {
		if dir == "" {
			return
		}
		for _, t := range LoadTemplatesDir(dir) {
			if i, ok := templateIdx[t.Name]; ok {
				res.Templates[i] = t
			} else {
				templateIdx[t.Name] = len(res.Templates)
				res.Templates = append(res.Templates, t)
			}
		}
	}

	// Base system prompt: project > user override > embedded default.
	res.BasePrompt = prompt.ResolveBase(opts.ProjectDir, opts.UserDir)

	// Lowest precedence first so higher-precedence loads replace.
	loadSkills(filepath.Join(opts.UserDir, "skills"))
	loadTemplates(filepath.Join(opts.UserDir, "prompt-templates"))
	loadSkills(filepath.Join(opts.ProjectDir, "skills"))
	loadTemplates(filepath.Join(opts.ProjectDir, "prompt-templates"))
	for _, d := range opts.SkillsDirs {
		loadSkills(d)
	}
	for _, d := range opts.TemplatesDirs {
		loadTemplates(d)
	}
	return res
}

// LoadSkillsDir loads skills from a directory: a SKILL.md in a directory is
// loaded and descent stops; root-level .md files are loaded as individual
// skills; dotfiles and node_modules are skipped;
// .gitignore-style ignore files are honored.
func LoadSkillsDir(dir string) []skills.Skill {
	var out []skills.Skill
	loadFromDir(dir, true, &out, nil)
	return out
}

func loadFromDir(dir string, includeRootFiles bool, out *[]skills.Skill, ignore *ignoreMatcher) {
	if ignore == nil {
		ignore = newIgnoreMatcher()
	}
	// Root-level ignore files.
	for _, name := range []string{".gitignore", ".ignore", ".fujiignore"} {
		loadIgnoreFile(ignore, filepath.Join(dir, name), dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// SKILL.md takes precedence: if present, load it and stop descending.
	for _, e := range entries {
		if e.Name() == "SKILL.md" && !e.IsDir() {
			if s := loadSkillFile(filepath.Join(dir, e.Name()), dir); s != nil {
				*out = append(*out, *s)
			}
			return
		}
	}
	sorted := entries
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name() < sorted[j].Name() })
	for _, e := range sorted {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		full := filepath.Join(dir, name)
		rel := relPath(dir, full)
		if e.IsDir() {
			if ignore.ignores(rel + "/") {
				continue
			}
			loadFromDir(full, false, out, ignore)
			continue
		}
		if !includeRootFiles || !strings.HasSuffix(name, ".md") {
			continue
		}
		if ignore.ignores(rel) {
			continue
		}
		if s := loadSkillFile(full, dir); s != nil {
			*out = append(*out, *s)
		}
	}
}

func loadSkillFile(path, dir string) *skills.Skill {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// Root-level .md files and SKILL.md share the same format; name defaults
	// to the parent dir basename.
	s, err := skills.ParseFile(path, string(data))
	if err != nil {
		return nil
	}
	return s
}

// LoadTemplatesDir loads *.md templates non-recursively.
func LoadTemplatesDir(dir string) []templates.Template {
	var out []templates.Template
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	sorted := entries
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name() < sorted[j].Name() })
	for _, e := range sorted {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, templates.ParseFile(filepath.Join(dir, e.Name()), string(data)))
	}
	return out
}

// --- ignore rules ----------------------------------------------------------

type ignoreMatcher struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	prefix string
	re     string
	negate bool
}

func newIgnoreMatcher() *ignoreMatcher { return &ignoreMatcher{} }

func loadIgnoreFile(ig *ignoreMatcher, path, root string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || (strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "\\#")) {
			continue
		}
		negated := false
		if strings.HasPrefix(line, "!") {
			negated = true
			line = line[1:]
		} else if strings.HasPrefix(line, "\\!") {
			line = line[1:]
		}
		line = strings.TrimPrefix(line, "/")
		ig.patterns = append(ig.patterns, ignorePattern{prefix: line, re: globToRe(line), negate: negated})
	}
}

// ignores reports whether rel matches (last matching pattern wins).
func (ig *ignoreMatcher) ignores(rel string) bool {
	ignored := false
	for _, p := range ig.patterns {
		if globMatch(p.re, rel) || strings.HasPrefix(rel, strings.TrimSuffix(p.prefix, "/")+"/") {
			ignored = !p.negate
		}
	}
	return ignored
}

func relPath(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return strings.ReplaceAll(rel, "\\", "/")
}

// globToRe converts a simple gitignore glob to a prefix matcher.
func globToRe(glob string) string {
	var sb strings.Builder
	for _, r := range glob {
		switch r {
		case '*':
			sb.WriteString("[^/]*")
		case '?':
			sb.WriteString("[^/]")
		case '.':
			sb.WriteString("\\.")
		default:
			sb.WriteString(string(r))
		}
	}
	return sb.String()
}

func globMatch(re, s string) bool {
	// prefix-style match on path segments
	return prefixMatch(re, s)
}

func prefixMatch(pat, s string) bool {
	// exact or segment-prefix match
	if s == pat {
		return true
	}
	if strings.HasPrefix(s, pat+"/") {
		return true
	}
	return false
}
