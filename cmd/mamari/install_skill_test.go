package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallSkillWritesAndGuards(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, ".claude", "skills")
	// Install via --dir.
	if err := runInstallSkill([]string{"code-review", "--dir", base}); err != nil {
		t.Fatalf("install: %v", err)
	}
	f := filepath.Join(base, "code-review", "SKILL.md")
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("skill not written: %v", err)
	}
	if len(b) < 500 {
		t.Fatalf("written skill too small (%d bytes)", len(b))
	}
	// Second install without --force must refuse (protects local edits).
	if err := runInstallSkill([]string{"code-review", "--dir", base}); err == nil {
		t.Fatal("expected refusal to overwrite existing skill without --force")
	}
	// --force overwrites.
	if err := runInstallSkill([]string{"code-review", "--dir", base, "--force"}); err != nil {
		t.Fatalf("force install: %v", err)
	}
	// Unknown skill errors.
	if err := runInstallSkill([]string{"does-not-exist", "--dir", base}); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}
