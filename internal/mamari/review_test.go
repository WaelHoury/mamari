package mamari

import (
	"os"
	"path/filepath"
	"testing"
)

// A working-tree change to a function should surface that function as a changed
// symbol with its resolved callers counted as proven blast radius.
func TestReviewDetectsChangedSymbolAndProvenCallers(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "lib.go", `package p

func target() int { return 1 }

func callsTarget() int { return target() }
`)
	git("add", "-A")
	git("commit", "-q", "-m", "init")

	// Modify target's body in the working tree (uncommitted), then re-index so
	// the index reflects the current tree (as watch mode would).
	write(t, root, "lib.go", `package p

func target() int {
	x := 1
	return x
}

func callsTarget() int { return target() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := Review(idx, ReviewOptions{Base: "HEAD", Callers: true})
	if resp.Status != "ok" {
		t.Fatalf("expected ok, got %s (%s)", resp.Status, resp.Message)
	}
	var target *ReviewChangedSymbol
	for i := range resp.Symbols {
		if resp.Symbols[i].Name == "target" {
			target = &resp.Symbols[i]
		}
	}
	if target == nil {
		t.Fatalf("expected 'target' among changed symbols, got %+v", resp.Symbols)
	}
	if target.ProvenCount < 1 {
		t.Fatalf("expected callsTarget as a proven caller, got provenCount=%d", target.ProvenCount)
	}
	foundCaller := false
	for _, c := range target.ProvenCallers {
		if c.Name == "callsTarget" {
			foundCaller = true
		}
	}
	if !foundCaller {
		t.Fatalf("expected callsTarget in proven callers, got %+v", target.ProvenCallers)
	}
	if resp.ProvenAffected < 1 {
		t.Fatalf("expected rollup provenAffected >= 1, got %d", resp.ProvenAffected)
	}
}

func TestReviewNoChangesOnCleanTree(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "lib.go", "package p\n\nfunc a() {}\n")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD"})
	if resp.Status != "no_changes" {
		t.Fatalf("expected no_changes on a clean tree, got %s", resp.Status)
	}
}

func TestReviewOutsideGitIsGraceful(t *testing.T) {
	root := t.TempDir()
	write(t, root, "lib.go", "package p\n\nfunc a() {}\n")
	// Ensure no inherited parent git repo interferes with the "not a repo" path.
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		t.Skip("temp dir unexpectedly has .git")
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD"})
	if resp.Status != "not_git" && resp.Status != "no_changes" {
		t.Fatalf("expected graceful not_git/no_changes outside a diffable repo, got %s", resp.Status)
	}
	if resp.Status == "ok" {
		t.Fatalf("must not report ok without a git diff")
	}
}
