// Package skills bundles Claude Code skill definitions into the mamari binary
// so `mamari install-skill` can drop them into a repo (or user) .claude/skills
// directory with no separate download. Each skill's canonical source is the
// SKILL.md checked in beside this file; go:embed keeps the binary copy in sync
// at build time (single source of truth).
package skills

import (
	_ "embed"
	"sort"
)

//go:embed code-review/SKILL.md
var codeReviewSkill string

// Skill is one bundled skill: its install name and its SKILL.md content.
type Skill struct {
	Name        string
	Content     string
	Description string
}

var embedded = map[string]Skill{
	"code-review": {
		Name:        "code-review",
		Content:     codeReviewSkill,
		Description: "Generic, mamari-grounded PR/code review for any repo and language",
	},
}

// Get returns the bundled skill by name.
func Get(name string) (Skill, bool) {
	s, ok := embedded[name]
	return s, ok
}

// Names returns the bundled skill names, sorted.
func Names() []string {
	out := make([]string, 0, len(embedded))
	for name := range embedded {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// All returns every bundled skill, sorted by name.
func All() []Skill {
	out := make([]Skill, 0, len(embedded))
	for _, name := range Names() {
		out = append(out, embedded[name])
	}
	return out
}
