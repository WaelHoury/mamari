package mamari

import (
	"strings"
	"testing"
)

// Changing a function's signature (its parameter list) must be classified as
// a "signature" change and, when it has callers, raise a signature-change
// risk reason — the highest-value review signal.
func TestReviewSignatureChangeRaisesRisk(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "lib.go", `package p

func target(a int) int { return a }

func c1() int { return target(1) }
func c2() int { return target(2) }
func c3() int { return target(3) }
func c4() int { return target(4) }
func c5() int { return target(5) }
`)
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	// Signature change: add a parameter.
	write(t, root, "lib.go", `package p

func target(a int, b int) int { return a + b }

func c1() int { return target(1) }
func c2() int { return target(2) }
func c3() int { return target(3) }
func c4() int { return target(4) }
func c5() int { return target(5) }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD", Limit: 100})
	var tgt *ReviewChangedSymbol
	for i := range resp.Symbols {
		if resp.Symbols[i].Name == "target" {
			tgt = &resp.Symbols[i]
		}
	}
	if tgt == nil {
		t.Fatalf("target not reported changed; %#v", resp.Symbols)
	}
	if tgt.ChangeKind != "signature" {
		t.Fatalf("expected changeKind=signature, got %q", tgt.ChangeKind)
	}
	sawReason := false
	for _, r := range tgt.RiskReasons {
		if len(r) >= 9 && r[:9] == "signature" {
			sawReason = true
		}
	}
	if !sawReason {
		t.Fatalf("expected a signature-changed risk reason; got %#v", tgt.RiskReasons)
	}
}

// A body-only change (same signature) must be classified "body" and NOT carry
// a signature-change reason.
func TestReviewBodyOnlyChangeNotSignature(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "lib.go", `package p

func target(a int) int {
	return a
}

func caller() int { return target(1) }
`)
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	write(t, root, "lib.go", `package p

func target(a int) int {
	x := a * 2
	return x
}

func caller() int { return target(1) }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD", Limit: 100})
	for _, s := range resp.Symbols {
		if s.Name == "target" {
			if s.ChangeKind != "body" {
				t.Fatalf("body-only change must be changeKind=body, got %q", s.ChangeKind)
			}
			for _, r := range s.RiskReasons {
				if len(r) >= 9 && r[:9] == "signature" {
					t.Fatalf("body-only change must not carry a signature reason; got %#v", s.RiskReasons)
				}
			}
			return
		}
	}
	t.Fatalf("target not reported changed")
}

// A brand-new file's symbols must be classified "new", never "signature" — a
// whole-file addition is not a caller-breaking interface change (this was the
// flaw of the pure line-intersection heuristic on large branch diffs).
func TestReviewNewFileSymbolsAreNew(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "existing.go", `package p

func existing() int { return 1 }
`)
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	// Add and COMMIT a whole new file (as a branch would), then review against
	// the commit before it — the file is an addition ('A') in the diff.
	write(t, root, "brandnew.go", `package p

func brandNewHelper(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

func brandNewCaller() int { return brandNewHelper(1, 2) }
`)
	git("add", "-A")
	git("commit", "-q", "-m", "add brandnew")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD~1", Limit: 100})
	for _, s := range resp.Symbols {
		if s.File == "brandnew.go" && s.ChangeKind == "signature" {
			t.Fatalf("new-file symbol %s must be 'new', not 'signature'", s.Name)
		}
	}
	// And at least one of them should be classified new.
	sawNew := false
	for _, s := range resp.Symbols {
		if s.File == "brandnew.go" && s.ChangeKind == "new" {
			sawNew = true
		}
	}
	if !sawNew {
		t.Fatalf("expected brandnew.go symbols classified 'new'; got %#v", resp.Symbols)
	}
}

// A formatting-only reformat of the declaration line (spaces around
// punctuation, alignment) is not a signature change: no caller can break.
// Previously this produced a spurious high-risk signature-change alarm.
func TestReviewWhitespaceOnlyDeclChangeIsBody(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "lib.js", `const target = (error, options = { fallbackMessage : 'x', prefix: '' }) => {
  return options.prefix + error
}
module.exports = { target }
`)
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	// Reformat only: drop the space before the colon, change nothing else.
	write(t, root, "lib.js", `const target = (error, options = { fallbackMessage: 'x', prefix: '' }) => {
  return options.prefix + error
}
module.exports = { target }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD", Limit: 100})
	for _, s := range resp.Symbols {
		if s.Name != "target" {
			continue
		}
		if s.ChangeKind == "signature" {
			t.Fatalf("whitespace-only reformat classified as signature change: %#v", s.RiskReasons)
		}
		return
	}
	t.Fatalf("target not reported changed; %#v", resp.Symbols)
}

// The signature-change risk message must quote DIRECT callers (the call
// sites that must pass new arguments), not the depth-2 transitive blast
// radius. ProvenCount keeps the full blast radius.
func TestReviewSignatureChangeMessageCountsDirectCallersOnly(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	// one direct caller (wrapper), three transitive callers of the wrapper.
	content := func(sig string) string {
		return `package p

func target(` + sig + `) int { return 1 }

func wrapper() int { return target(` + map[string]string{"a int": "1", "a int, b int": "1, 2"}[sig] + `) }

func t1() int { return wrapper() }
func t2() int { return wrapper() }
func t3() int { return wrapper() }
`
	}
	write(t, root, "lib.go", content("a int"))
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	write(t, root, "lib.go", content("a int, b int"))
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD", Limit: 100})
	for _, s := range resp.Symbols {
		if s.Name != "target" {
			continue
		}
		if s.ChangeKind != "signature" {
			t.Fatalf("expected signature change, got %q", s.ChangeKind)
		}
		for _, r := range s.RiskReasons {
			if strings.HasPrefix(r, "signature changed") {
				if !strings.Contains(r, "1 direct caller") {
					t.Fatalf("signature message must count the 1 direct caller, got %q (provenCount=%d)", r, s.ProvenCount)
				}
				if s.ProvenCount < 4 {
					t.Fatalf("provenCount should keep the transitive blast radius (wrapper+t1..t3), got %d", s.ProvenCount)
				}
				return
			}
		}
		t.Fatalf("no signature-changed reason: %#v", s.RiskReasons)
	}
	t.Fatalf("target not reported changed")
}

// `git diff <base>` is blind to untracked files, but the review-before-push
// workflow routinely includes brand-new files never `git add`ed. The index
// scans the filesystem, so their symbols exist — review must fold indexed
// untracked files in as fully-changed ("new") rather than silently skipping them.
func TestReviewIncludesUntrackedIndexedFiles(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "lib.go", `package p

func existing() int { return 1 }
`)
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	// A brand-new file, never git-added, plus an untracked non-code file that
	// must NOT flip the tree to "changed" on its own.
	write(t, root, "brandnew.go", `package p

func freshHelper() int { return existing() }
`)
	write(t, root, "NOTES.txt", "scratch notes\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD", Limit: 100})
	if resp.Status != "ok" {
		t.Fatalf("expected ok, got %s (%s)", resp.Status, resp.Message)
	}
	var fresh *ReviewChangedSymbol
	for i := range resp.Symbols {
		if resp.Symbols[i].Name == "freshHelper" {
			fresh = &resp.Symbols[i]
		}
	}
	if fresh == nil {
		t.Fatalf("untracked brandnew.go symbol missing from review: %#v", resp.Symbols)
	}
	if fresh.ChangeKind != "new" {
		t.Fatalf("untracked file symbol should classify as new, got %q", fresh.ChangeKind)
	}
}

// An untracked file the index does not know (non-code junk) must not turn a
// clean tree into a reviewable change set.
func TestReviewUntrackedNonCodeFileStaysNoChanges(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "lib.go", `package p

func existing() int { return 1 }
`)
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "NOTES.txt", "scratch notes\n")
	resp := Review(idx, ReviewOptions{Base: "HEAD"})
	if resp.Status != "no_changes" {
		t.Fatalf("expected no_changes with only non-code untracked files, got %s (files=%d)", resp.Status, resp.ChangedFiles)
	}
}
