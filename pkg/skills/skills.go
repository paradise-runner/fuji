// Package skills defines the skill resource model and system-prompt
// formatting (core-spec §4.6). Discovery and file parsing land with
// pkg/resource in P7; the type and formatting are needed by pkg/session from
// P5.
package skills

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Skill is a skill loaded from a SKILL.md file or provided programmatically.
// name, description, and filePath are inserted into the system prompt in an
// XML-formatted block (agentskills.io spec).
type Skill struct {
	// Name is the stable skill name used for lookup and model-visible listings.
	Name string
	// Description is a short model-visible description of when to use the skill.
	Description string
	// Content is the full skill instructions.
	Content string
	// FilePath is the absolute path to the skill file.
	FilePath string
	// DisableModelInvocation excludes this skill from model-visible lists
	// while still allowing explicit application invocation.
	DisableModelInvocation bool
}

// FormatSkillsForPrompt renders the "Available skills" system-prompt section.
// Returns "" when no skills are present.
func FormatSkillsForPrompt(skills []Skill) string {
	var visible []Skill
	for _, s := range skills {
		if !s.DisableModelInvocation {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Available skills\n\n")
	for _, s := range visible {
		b.WriteString("<skill name=\"")
		b.WriteString(escapeXML(s.Name))
		b.WriteString("\" description=\"")
		b.WriteString(escapeXML(s.Description))
		b.WriteString("\" file_path=\"")
		b.WriteString(escapeXML(s.FilePath))
		b.WriteString("\">\n")
		b.WriteString(s.Content)
		b.WriteString("\n</skill>\n\n")
	}
	return b.String()
}

// ExpandSkillCommand resolves "/skill:name args" syntax (the skill body
// wrapped in a <skill> block with relative-reference guidance). Returns the
// original text when no
// skill matches.
func ExpandSkillCommand(text string, skills []Skill) (string, bool) {
	if !strings.HasPrefix(text, "/skill:") {
		return text, false
	}
	spaceIndex := strings.Index(text, " ")
	var skillName, args string
	if spaceIndex == -1 {
		skillName = text[len("/skill:"):]
	} else {
		skillName = text[len("/skill:"):spaceIndex]
		args = strings.TrimSpace(text[spaceIndex+1:])
	}
	for _, s := range skills {
		if s.Name != skillName {
			continue
		}
		baseDir := filepath.Dir(s.FilePath)
		body := s.Content
		skillBlock := fmt.Sprintf("<skill name=\"%s\" location=\"%s\">\nReferences are relative to %s.\n\n%s\n</skill>",
			escapeXML(s.Name), escapeXML(s.FilePath), baseDir, body)
		if args != "" {
			return skillBlock + "\n\n" + args, true
		}
		return skillBlock, true
	}
	return text, false
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return r.Replace(s)
}
