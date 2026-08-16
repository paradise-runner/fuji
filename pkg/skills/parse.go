package skills

import (
	"path/filepath"
	"strings"

	"fuji/internal/frontmatter"
)

// MaxNameLength / MaxDescriptionLength bounds for skill metadata.
const (
	MaxNameLength        = 64
	MaxDescriptionLength = 200
)

// ParseFile parses a SKILL.md file's frontmatter + body into a Skill.
// Returns (skill, nil) on success; (nil, error) on parse failure; a skill may
// be nil with no error when the description is missing (those are dropped).
func ParseFile(filePath, content string) (*Skill, error) {
	res := frontmatter.Parse(content)
	name, _ := res.Frontmatter["name"].(string)
	desc, _ := res.Frontmatter["description"].(string)
	disableModel, _ := res.Frontmatter["disable-model-invocation"].(bool)
	if name == "" {
		name = filepath.Base(filepath.Dir(filePath))
	}
	if strings.TrimSpace(desc) == "" {
		return nil, nil
	}
	return &Skill{
		Name:                   name,
		Description:            desc,
		Content:                res.Body,
		FilePath:               filePath,
		DisableModelInvocation: disableModel,
	}, nil
}

// ParseFileByName parses a root-level .md skill file (name defaults to the
// parent dir basename; callers may override).
func ParseFileByName(filePath, content string) (*Skill, error) {
	res := frontmatter.Parse(content)
	name, _ := res.Frontmatter["name"].(string)
	desc, _ := res.Frontmatter["description"].(string)
	if name == "" {
		name = filepath.Base(filepath.Dir(filePath))
	}
	if strings.TrimSpace(desc) == "" {
		return nil, nil
	}
	disableModel, _ := res.Frontmatter["disable-model-invocation"].(bool)
	return &Skill{
		Name:                   name,
		Description:            desc,
		Content:                res.Body,
		FilePath:               filePath,
		DisableModelInvocation: disableModel,
	}, nil
}

// ValidName reports whether a skill name passes the constraints.
func ValidName(name string) bool {
	if name == "" || len(name) > MaxNameLength {
		return false
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-'
		if !ok {
			return false
		}
	}
	return !strings.HasPrefix(name, "-") && !strings.HasSuffix(name, "-") && !strings.Contains(name, "--")
}
