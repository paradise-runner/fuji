package resource_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fuji/pkg/resource"
	"fuji/pkg/skills"
	"fuji/pkg/templates"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverSkillsAndTemplates(t *testing.T) {
	root := t.TempDir()
	// --skills dir: one directory skill + one root .md skill.
	skillsDir := filepath.Join(root, "myskills")
	write(t, filepath.Join(skillsDir, "code-review", "SKILL.md"), `---
name: code-review
description: Review code for bugs and style
---
Review the code carefully.
`)
	write(t, filepath.Join(skillsDir, "root-skill.md"), `---
description: A root-level skill
---
Root skill body
`)
	// Project .fuji skills (lower precedence).
	proj := filepath.Join(root, "proj")
	write(t, filepath.Join(proj, ".fuji", "skills", "local", "SKILL.md"), `---
name: local
description: Project-local skill
---
Local body
`)
	// User templates + project templates.
	user := filepath.Join(root, "userfuji")
	write(t, filepath.Join(user, "prompt-templates", "review.md"), `---
description: Review template
---
Review file $1 with ${@:2}
`)
	write(t, filepath.Join(proj, ".fuji", "prompt-templates", "plan.md"), `Plan: $ARGUMENTS
`)

	res := resource.Discover(resource.Options{
		SkillsDirs: []string{skillsDir},
		ProjectDir: filepath.Join(proj, ".fuji"),
		UserDir:    user,
	})
	names := map[string]bool{}
	for _, s := range res.Skills {
		names[s.Name] = true
	}
	for _, want := range []string{"code-review", "myskills", "local"} {
		if !names[want] {
			t.Errorf("missing skill %q (have %v)", want, names)
		}
	}
	// Skills carry body + description.
	for _, s := range res.Skills {
		if s.Name == "code-review" {
			if !strings.Contains(s.Content, "Review the code carefully") {
				t.Errorf("code-review content = %q", s.Content)
			}
			if s.Description != "Review code for bugs and style" {
				t.Errorf("description = %q", s.Description)
			}
		}
	}
	tplNames := map[string]bool{}
	for _, t := range res.Templates {
		tplNames[t.Name] = true
	}
	if !tplNames["review"] || !tplNames["plan"] {
		t.Errorf("templates = %v", tplNames)
	}
}

func TestDiscoverPrecedence(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "userfuji")
	proj := filepath.Join(root, "proj")
	write(t, filepath.Join(user, "skills", "dup", "SKILL.md"), "---\nname: dup\ndescription: user version\n---\nUSER BODY\n")
	write(t, filepath.Join(proj, ".fuji", "skills", "dup", "SKILL.md"), "---\nname: dup\ndescription: project version\n---\nPROJECT BODY\n")
	flagDir := filepath.Join(root, "flagskills")
	write(t, filepath.Join(flagDir, "dup", "SKILL.md"), "---\nname: dup\ndescription: flag version\n---\nFLAG BODY\n")

	res := resource.Discover(resource.Options{
		SkillsDirs: []string{flagDir},
		ProjectDir: filepath.Join(proj, ".fuji"),
		UserDir:    user,
	})
	if len(res.Skills) != 1 {
		t.Fatalf("skills = %d, want 1 (flag wins)", len(res.Skills))
	}
	if !strings.Contains(res.Skills[0].Content, "FLAG BODY") {
		t.Errorf("flag version lost: %q", res.Skills[0].Content)
	}
}

func TestSkillCommandExpansion(t *testing.T) {
	skill := skills.Skill{Name: "review", Description: "d", Content: "BODY", FilePath: "/x/y/review/SKILL.md"}
	expanded, ok := skills.ExpandSkillCommand("/skill:review be thorough", []skills.Skill{skill})
	if !ok {
		t.Fatal("no expansion")
	}
	if !strings.Contains(expanded, "BODY") || !strings.Contains(expanded, "be thorough") {
		t.Errorf("expanded = %q", expanded)
	}
	if !strings.Contains(expanded, `<skill name="review"`) {
		t.Errorf("missing skill block: %q", expanded)
	}
	// Unknown skill passes through.
	text, ok := skills.ExpandSkillCommand("/skill:nope hi", []skills.Skill{skill})
	if ok || text != "/skill:nope hi" {
		t.Errorf("unknown skill: %q %v", text, ok)
	}
}

func TestTemplateExpansion(t *testing.T) {
	ts := []templates.Template{
		{Name: "review", Description: "d", Content: "Review $1 with ${@:2} and $ARGUMENTS"},
	}
	got := templates.ExpandPromptTemplate("/review file.txt extra", ts)
	if !strings.Contains(got, "Review file.txt with extra") {
		t.Errorf("expanded = %q", got)
	}
	// Non-command text untouched.
	if got := templates.ExpandPromptTemplate("hello", ts); got != "hello" {
		t.Errorf("plain = %q", got)
	}
	// Substitution details.
	if got := templates.SubstituteArgs("$1 $2 ${@:1} $@", []string{"a", "b"}); got != "a b a b a b" {
		t.Errorf("substitute = %q", got)
	}
	if got := templates.SubstituteArgs("${@:2:1}", []string{"a", "b", "c"}); got != "b" {
		t.Errorf("slice = %q", got)
	}
	// Arg parsing with quotes.
	if got := templates.ParseArgs(`one "two three" 'four five'`); len(got) != 3 || got[1] != "two three" || got[2] != "four five" {
		t.Errorf("args = %v", got)
	}
}

func TestSkillParsing(t *testing.T) {
	s, err := skills.ParseFile("/s/review/SKILL.md", "---\nname: review\ndescription: Does reviews\ndisable-model-invocation: true\n---\nBody here\n")
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("nil skill")
	}
	if s.Name != "review" || !s.DisableModelInvocation || !strings.Contains(s.Content, "Body here") {
		t.Errorf("skill = %+v", s)
	}
	// Missing description → dropped (nil, no error).
	s, err = skills.ParseFile("/s/x/SKILL.md", "---\nname: x\n---\nbody\n")
	if err != nil || s != nil {
		t.Errorf("missing description: %v %+v", err, s)
	}
}
