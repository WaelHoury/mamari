package mamari

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitInit initializes a git repo in root and returns a helper to run git
// commands against it. Commits are authored with a fixed identity so the
// test does not depend on the host's global git config.
func gitInit(t *testing.T, root string) func(args ...string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	return run
}

func TestBuildCoChangeGraphCountsFilesChangedTogether(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)

	// Commit 1: introduce both files together.
	write(t, root, "src/a.ts", "export const a = 1\n")
	write(t, root, "src/b.ts", "export const b = 2\n")
	git("add", ".")
	git("commit", "-q", "-m", "add a and b")

	// Commit 2: change both again together.
	write(t, root, "src/a.ts", "export const a = 2\n")
	write(t, root, "src/b.ts", "export const b = 3\n")
	git("add", ".")
	git("commit", "-q", "-m", "update a and b")

	// Commit 3: change only a.
	write(t, root, "src/a.ts", "export const a = 3\n")
	git("add", ".")
	git("commit", "-q", "-m", "update a only")

	graph := buildCoChangeGraph(root)
	if graph == nil {
		t.Fatal("expected non-nil graph for a git repo")
	}
	entries := graph["src/a.ts"]
	if len(entries) != 1 || entries[0].File != "src/b.ts" || entries[0].Count != 2 {
		t.Fatalf("expected src/a.ts to co-change with src/b.ts twice, got %#v", entries)
	}
	entries = graph["src/b.ts"]
	if len(entries) != 1 || entries[0].File != "src/a.ts" || entries[0].Count != 2 {
		t.Fatalf("expected src/b.ts to co-change with src/a.ts twice, got %#v", entries)
	}
}

func TestBuildCoChangeGraphReturnsNilOutsideGitRepo(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", "export const a = 1\n")

	if graph := buildCoChangeGraph(root); graph != nil {
		t.Fatalf("expected nil graph outside a git repo, got %#v", graph)
	}
}

func TestCoChangeCacheRoundTrip(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "src/a.ts", "export const a = 1\n")
	write(t, root, "src/b.ts", "export const b = 2\n")
	git("add", ".")
	git("commit", "-q", "-m", "initial")

	graph := buildCoChangeGraph(root)
	if err := saveCoChangeCache(root, graph); err != nil {
		t.Fatalf("saveCoChangeCache: %v", err)
	}
	if _, err := os.Stat(coChangePath(root)); err != nil {
		t.Fatalf("expected cache file to exist: %v", err)
	}

	loaded, ok := loadCoChangeCache(root)
	if !ok {
		t.Fatal("expected cache hit after save")
	}
	if len(loaded["src/a.ts"]) != 1 || loaded["src/a.ts"][0].File != "src/b.ts" {
		t.Fatalf("unexpected loaded graph: %#v", loaded)
	}

	// A new commit changes HEAD, so the cache should now be considered stale.
	write(t, root, "src/c.ts", "export const c = 3\n")
	git("add", ".")
	git("commit", "-q", "-m", "add c")

	if _, ok := loadCoChangeCache(root); ok {
		t.Fatal("expected cache miss after HEAD moved")
	}
}

func TestLoadCoChangeCacheMissesOutsideGitRepo(t *testing.T) {
	root := t.TempDir()
	if _, ok := loadCoChangeCache(root); ok {
		t.Fatal("expected cache miss outside a git repo")
	}
}

func TestEnsureCoChangeGraphAndCoChangedFiles(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "src/a.ts", "export function helper(): number {\n  return 1\n}\n")
	write(t, root, "src/b.ts", "export const b = 2\n")
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	write(t, root, "src/a.ts", "export function helper(): number {\n  return 2\n}\n")
	write(t, root, "src/b.ts", "export const b = 3\n")
	git("add", ".")
	git("commit", "-q", "-m", "update both")

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	entries := idx.CoChangedFiles("src/a.ts", 0)
	if len(entries) != 1 || entries[0].File != "src/b.ts" {
		t.Fatalf("expected src/a.ts to co-change with src/b.ts, got %#v", entries)
	}

	// On-disk cache should now exist and be reusable.
	if _, err := os.Stat(filepath.Join(root, ".mamari", "cochange.json")); err != nil {
		t.Fatalf("expected cochange.json to be written: %v", err)
	}

	// A symbol with no recorded co-changes returns an empty (non-nil) slice.
	if entries := idx.CoChangedFiles("does/not/exist.ts", 0); len(entries) != 0 {
		t.Fatalf("expected empty co-change list for unknown file, got %#v", entries)
	}
}

func TestCoChangedFilesOutsideGitRepoIsGracefullyEmpty(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", "export const a = 1\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	if entries := idx.CoChangedFiles("src/a.ts", 0); len(entries) != 0 {
		t.Fatalf("expected empty co-change list outside a git repo, got %#v", entries)
	}
}
