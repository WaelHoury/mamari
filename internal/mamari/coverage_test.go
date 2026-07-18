package mamari

import (
	"os"
	"path/filepath"
	"testing"
)

// A symbol whose lines never executed under the suite must be flagged
// untested BY COVERAGE even when a static test-call path exists (mocks/dead
// branches sever the real path) — the authoritative-untested case.
func TestReviewCoverageOverridesStaticUntested(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "svc.js", `function compute(n) {
  return n * 2
}
function neverRun(n) {
  return n + 1
}
module.exports = { compute, neverRun }
`)
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	// Change both functions in the working tree so both are "changed" vs HEAD.
	write(t, root, "svc.js", `function compute(n) {
  return n * 3
}
function neverRun(n) {
  return n + 2
}
module.exports = { compute, neverRun }
`)
	// lcov: compute's lines executed (hits>0), neverRun's did not (hits==0).
	cov := "SF:" + filepath.Join(root, "svc.js") + "\n" +
		"DA:1,5\nDA:2,5\nDA:4,0\nDA:5,0\nend_of_record\n"
	covPath := filepath.Join(root, "lcov.info")
	if err := os.WriteFile(covPath, []byte(cov), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD", CoveragePath: covPath, Limit: 100})
	if resp.Status != "ok" {
		t.Fatalf("review status %s: %s", resp.Status, resp.Message)
	}
	if !resp.CoverageApplied {
		t.Fatalf("coverage should be applied; warnings=%v", resp.Warnings)
	}
	got := map[string]ReviewChangedSymbol{}
	for _, s := range resp.Symbols {
		got[s.Name] = s
	}
	if c, ok := got["compute"]; !ok || c.Untested {
		t.Fatalf("compute executed under tests → must be tested; got %#v (ok=%v)", c, ok)
	}
	if n, ok := got["neverRun"]; !ok || !n.Untested || n.UntestedBy != "coverage" {
		t.Fatalf("neverRun never executed → must be untested-by-coverage; got %#v (ok=%v)", n, ok)
	}
}

// A mismatched coverage path degrades gracefully to static closure with a
// warning, never an error.
func TestReviewCoverageMismatchWarnsNotFails(t *testing.T) {
	root := t.TempDir()
	git := gitInit(t, root)
	write(t, root, "a.js", "function f(){ return 1 }\nmodule.exports = { f }\n")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	write(t, root, "a.js", "function f(){ return 2 }\nmodule.exports = { f }\n")
	covPath := filepath.Join(root, "nope.info")
	_ = os.WriteFile(covPath, []byte("SF:/somewhere/unrelated.js\nDA:1,1\nend_of_record\n"), 0o644)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Review(idx, ReviewOptions{Base: "HEAD", CoveragePath: covPath, Limit: 100})
	if resp.Status != "ok" {
		t.Fatalf("status %s", resp.Status)
	}
	if resp.CoverageApplied {
		t.Fatalf("coverage matched no files; should not be marked applied")
	}
	if len(resp.Warnings) == 0 {
		t.Fatalf("expected a coverage-mismatch warning")
	}
}

func TestParseLCOVSuffixMatch(t *testing.T) {
	indexed := map[string]bool{"backend/services/x.js": true}
	cov := parseLCOV([]byte("SF:services/x.js\nDA:10,1\nend_of_record\n"), "/repo", indexed)
	if _, ok := cov.byFile["backend/services/x.js"]; !ok {
		t.Fatalf("suffix match failed; got %#v", cov.byFile)
	}
}
