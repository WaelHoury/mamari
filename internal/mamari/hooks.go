package mamari

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CommittedIndexDir is the opt-in, git-tracked counterpart to the default
// (gitignored) .mamari/ index directory. Kept as a separate subdirectory so
// the default local-only behavior (and the .mamari/* sidecars that should
// stay private — search.json, notes.json, cochange.json) is unaffected for
// every project that does not opt in via `mamari hooks install` /
// `index --commit`.
func CommittedIndexDir(repoRoot string) string {
	return filepath.Join(repoRoot, ".mamari", "committed")
}

// CommittedIndexPath is the committed index file's path within
// CommittedIndexDir.
func CommittedIndexPath(repoRoot string) string {
	return filepath.Join(CommittedIndexDir(repoRoot), "index.json")
}

const hookMarkerBegin = "# >>> mamari hooks install >>>"
const hookMarkerEnd = "# <<< mamari hooks install <<<"

// preCommitHookBody is the snippet InstallPreCommitHook inserts into the
// repo's pre-commit hook. It never fails the commit: a missing `mamari`
// binary or a build/index error is swallowed (`|| true`) because a stale
// committed index is far preferable to a developer's commit being blocked
// by Mamari housekeeping.
const preCommitHookBody = `if command -v mamari >/dev/null 2>&1; then
  mamari index --commit --quiet || true
  git add ` + ".mamari/committed/index.json" + ` ` + ".mamari/committed/literals.jsonl" + ` 2>/dev/null || true
fi
`

// gitHooksPath shells out to "git rev-parse --git-path hooks/<name>" so the
// resolved path is correct for worktrees and any repo that has already
// configured a custom core.hooksPath, instead of assuming ".git/hooks".
func gitHooksPath(repoRoot, name string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-path", filepath.Join("hooks", name))
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve git hooks path: %w", err)
	}
	rel := strings.TrimSpace(string(out))
	if filepath.IsAbs(rel) {
		return rel, nil
	}
	return filepath.Join(repoRoot, rel), nil
}

// InstallPreCommitHook writes (or appends to) the repo's pre-commit hook so
// it regenerates the committed index before every commit, and adjusts
// .gitignore so .mamari/committed/ is tracked. Idempotent: running it again
// is a no-op if the marker block is already present. An existing hook with
// unrelated content is preserved — the Mamari block is appended, not used to
// replace the file.
func InstallPreCommitHook(repoRoot string) (string, error) {
	hookPath, err := gitHooksPath(repoRoot, "pre-commit")
	if err != nil {
		return "", err
	}
	existing, err := os.ReadFile(hookPath)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	content := string(existing)
	if strings.Contains(content, hookMarkerBegin) {
		// Already installed; still make sure .gitignore is in sync (e.g. a
		// teammate cloned the repo and only needs the gitignore rule, since
		// the hook script itself isn't git-tracked).
		return hookPath, ensureCommittedIndexGitignoreRule(repoRoot)
	}
	if content == "" {
		content = "#!/bin/sh\n"
	} else if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "\n" + hookMarkerBegin + "\n" + preCommitHookBody + hookMarkerEnd + "\n"

	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(hookPath, []byte(content), 0o755); err != nil {
		return "", err
	}
	if err := ensureCommittedIndexGitignoreRule(repoRoot); err != nil {
		return "", err
	}
	return hookPath, nil
}

// UninstallPreCommitHook removes just the Mamari-managed block from the
// pre-commit hook (leaving any other content the hook had untouched), or
// deletes the file entirely if nothing but a shebang remains. It does not
// touch .gitignore or the committed index files themselves.
func UninstallPreCommitHook(repoRoot string) error {
	hookPath, err := gitHooksPath(repoRoot, "pre-commit")
	if err != nil {
		return err
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	content := string(data)
	start := strings.Index(content, hookMarkerBegin)
	end := strings.Index(content, hookMarkerEnd)
	if start == -1 || end == -1 || end < start {
		return nil // nothing installed by us
	}
	end += len(hookMarkerEnd)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	remaining := content[:start] + content[end:]
	if strings.TrimSpace(strings.TrimPrefix(remaining, "#!/bin/sh")) == "" {
		return os.Remove(hookPath)
	}
	return os.WriteFile(hookPath, []byte(remaining), 0o755)
}

const gitignoreMarkerBegin = "# >>> mamari hooks install >>>"
const gitignoreMarkerEnd = "# <<< mamari hooks install <<<"

// ensureCommittedIndexGitignoreRule adds the un-ignore rule for
// .mamari/committed/ to the repo's .gitignore, and neutralizes any existing
// bare ".mamari" / ".mamari/" line (which would otherwise make git skip the
// directory entirely — a later "!" negation cannot re-include files inside
// a directory that's ignored by an earlier whole-directory pattern).
// Idempotent.
func ensureCommittedIndexGitignoreRule(repoRoot string) error {
	path := filepath.Join(repoRoot, ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(data)
	if strings.Contains(content, gitignoreMarkerBegin) {
		return nil
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == ".mamari" || trimmed == ".mamari/" {
			lines = append(lines, "# "+line+" (superseded by the mamari hooks install block below)")
			continue
		}
		lines = append(lines, line)
	}
	rebuilt := strings.Join(lines, "\n")
	if rebuilt != "" && !strings.HasSuffix(rebuilt, "\n") {
		rebuilt += "\n"
	}
	rebuilt += "\n" + gitignoreMarkerBegin + "\n" +
		"# Keeps Mamari's local-only cache ignored while tracking the\n" +
		"# shareable, git-portable index (see `mamari hooks install`).\n" +
		".mamari/*\n" +
		"!.mamari/committed/\n" +
		gitignoreMarkerEnd + "\n"

	return os.WriteFile(path, []byte(rebuilt), 0o644)
}
