package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/waelhoury/mamari/skills"
)

// runInstallSkill writes a mamari-bundled Claude Code skill into a
// .claude/skills/<name>/ directory so a teammate goes from "mamari installed"
// to "the skill is active" in one command — no separate file to copy.
//
// Default target is the current repo (./.claude/skills). --user installs into
// ~/.claude/skills so the skill is available across all of that developer's
// repos. An existing SKILL.md is never clobbered without --force, so a team's
// local customization is safe.
func runInstallSkill(args []string) error {
	fs := flag.NewFlagSet("install-skill", flag.ContinueOnError)
	user := fs.Bool("user", false, "install into ~/.claude/skills (all your repos) instead of ./.claude/skills")
	dir := fs.String("dir", "", "explicit target directory for .claude/skills (overrides default and --user)")
	force := fs.Bool("force", false, "overwrite an existing SKILL.md")
	list := fs.Bool("list", false, "list the skills bundled in this binary and exit")
	args = normalizeFlags(args, map[string]bool{"--dir": true}, map[string]bool{"--user": true, "--force": true, "--list": true})
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *list || fs.NArg() == 0 {
		fmt.Println("Bundled skills (install with: mamari install-skill <name>):")
		for _, s := range skills.All() {
			fmt.Printf("  %-14s %s\n", s.Name, s.Description)
		}
		if fs.NArg() == 0 && !*list {
			return fmt.Errorf("install-skill requires a skill name (see the list above)")
		}
		return nil
	}

	name := fs.Arg(0)
	skill, ok := skills.Get(name)
	if !ok {
		return fmt.Errorf("unknown skill %q; available: %s", name, strings.Join(skills.Names(), ", "))
	}

	base, err := skillsBaseDir(*dir, *user)
	if err != nil {
		return err
	}
	targetDir := filepath.Join(base, skill.Name)
	targetFile := filepath.Join(targetDir, "SKILL.md")

	if _, err := os.Stat(targetFile); err == nil && !*force {
		return fmt.Errorf("%s already exists; re-run with --force to overwrite (this preserves any local customization)", targetFile)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(targetFile, []byte(skill.Content), 0o644); err != nil {
		return err
	}

	fmt.Printf("installed skill %q -> %s\n", skill.Name, targetFile)
	if *user {
		fmt.Println("active in all your repos on your next Claude session.")
	} else {
		fmt.Println("active in this repo on your next Claude session. Commit .claude/skills/ to share it with your team.")
	}
	return nil
}

// skillsBaseDir resolves the .claude/skills directory to install into: an
// explicit --dir wins; then --user (~/.claude/skills); otherwise the current
// working directory's ./.claude/skills.
func skillsBaseDir(dir string, user bool) (string, error) {
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	if user {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve home directory for --user install: %w", err)
		}
		return filepath.Join(home, ".claude", "skills"), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, ".claude", "skills"), nil
}
