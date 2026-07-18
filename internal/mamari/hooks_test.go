package mamari

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallPreCommitHookIsIdempotentAndPreservesExistingContent covers the
// core git-portable-index workflow: installing must write a hook script,
// adjust .gitignore so .mamari/committed/ is tracked despite the existing
// bare ".mamari/" ignore rule, and re-running install must not duplicate the
// block or clobber unrelated hook content a project already had.
func TestInstallPreCommitHookIsIdempotentAndPreservesExistingContent(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, ".gitignore", "node_modules/\n.mamari/\ndist/\n")
	git("add", ".")
	git("commit", "-q", "-m", "initial")

	hookPath, err := InstallPreCommitHook(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "mamari index --commit") {
		t.Fatalf("expected hook to invoke `mamari index --commit`, got: %s", data)
	}
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("expected hook script to be executable, mode=%v", info.Mode())
	}

	gitignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	gi := string(gitignore)
	if !strings.Contains(gi, "# .mamari/ (superseded by the mamari hooks install block below)") {
		t.Fatalf("expected the bare .mamari/ rule to be commented out, got:\n%s", gi)
	}
	if !strings.Contains(gi, ".mamari/*") || !strings.Contains(gi, "!.mamari/committed/") {
		t.Fatalf("expected the un-ignore rule for .mamari/committed/, got:\n%s", gi)
	}
	if !strings.Contains(gi, "node_modules/\n") || !strings.Contains(gi, "dist/\n") {
		t.Fatalf("expected unrelated existing .gitignore lines to survive, got:\n%s", gi)
	}

	// Re-running install must be a no-op on the hook (no duplicate block) and
	// must not re-add a second commented-out .mamari/ line.
	if _, err := InstallPreCommitHook(root); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data2), hookMarkerBegin) != 1 {
		t.Fatalf("expected exactly one install block after re-running install, got:\n%s", data2)
	}
	gitignore2, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(gitignore2), gitignoreMarkerBegin) != 1 {
		t.Fatalf("expected exactly one gitignore block after re-running install, got:\n%s", gitignore2)
	}
}

// TestInstallPreCommitHookAppendsToExistingScript covers a project that
// already has its own pre-commit hook (e.g. a linter) — install must append
// the Mamari block rather than overwrite it, and uninstall must remove only
// Mamari's block, leaving the original script intact.
func TestInstallPreCommitHookAppendsToExistingScript(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	git("add", "-A")
	git("commit", "--allow-empty", "-q", "-m", "initial")

	hookPath, err := gitHooksPath(root, "pre-commit")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "#!/bin/sh\necho running-lint\n"
	if err := os.WriteFile(hookPath, []byte(existing), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallPreCommitHook(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "echo running-lint") {
		t.Fatalf("expected the pre-existing hook content to survive install, got:\n%s", data)
	}
	if !strings.Contains(string(data), "mamari index --commit") {
		t.Fatalf("expected the mamari block to be appended, got:\n%s", data)
	}

	if err := UninstallPreCommitHook(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "mamari index --commit") {
		t.Fatalf("expected uninstall to remove the mamari block, got:\n%s", after)
	}
	if !strings.Contains(string(after), "echo running-lint") {
		t.Fatalf("expected uninstall to leave the original hook content intact, got:\n%s", after)
	}
}

// TestSaveIndexJSONRoundTripsThroughLoadIndex covers the git-portable index
// format itself: SaveIndexJSON's plain (non-gob, non-gzip) JSON output must
// be loadable by the same LoadIndex used for every other index format, and
// must preserve symbols/edges/files.
func TestSaveIndexJSONRoundTripsThroughLoadIndex(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.js", "function add(a, b) {\n  return a + b\n}\nmodule.exports = { add }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	committedPath := CommittedIndexPath(root)
	if err := SaveIndexJSON(idx, committedPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(committedPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(string(data), "mamari-index-v1\n") {
		t.Fatalf("expected plain JSON, not the binary gob format, got prefix: %.20s", data)
	}

	loaded, err := LoadIndex(committedPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Symbols) != len(idx.Symbols) {
		t.Fatalf("expected %d symbols after round trip, got %d", len(idx.Symbols), len(loaded.Symbols))
	}
	addSym := findSymbolByName(loaded, "add")
	if addSym.ID == "" {
		t.Fatalf("expected add() symbol to survive the JSON round trip")
	}
}
