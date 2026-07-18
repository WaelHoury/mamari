package mamari

import (
	"strings"
	"testing"
)

// A gibberish query whose distinctive terms don't exist in the corpus can still return
// status=ok hits anchored on its most-common words, indistinguishable from a
// real answer without reading each hit. Such result sets must carry
// confidence="low" and a warning; queries whose terms all exist — or where
// the best hit co-matches most of the typed terms — must stay unflagged.

func buildSearchConfidenceFixture(t *testing.T) *Index {
	t.Helper()
	root := t.TempDir()
	// "match" and "process" appear across files (common), "rotation" is a
	// distinctive term that exists, and nothing contains "zzqx"/"blorf".
	write(t, root, "a/accounts.js", `export function matchAccounts(list) {
  return list.filter(x => x.match)
}
`)
	write(t, root, "b/candidates.js", `export function matchCandidates(items) {
  // match each candidate against the list
  return items.map(i => i.match)
}
`)
	write(t, root, "c/rotation.js", `export function rotateToken(session) {
  // token rotation: match the session family before rotating
  return session.rotate()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestSearchCodeGibberishTailIsLowConfidence(t *testing.T) {
	idx := buildSearchConfidenceFixture(t)
	resp := SearchCode(idx, "zzqx blorf glarp match", SearchCodeOptions{Limit: 5, BudgetTokens: 800})
	if len(resp.Hits) == 0 {
		// The hard low-signal gate may legitimately turn this exact shape
		// into an empty set; that is an acceptable (stronger) outcome.
		return
	}
	if resp.Confidence != "low" {
		t.Fatalf("expected confidence=low for hits anchored on the query's only common term, got %q (warnings=%v)",
			resp.Confidence, resp.Warnings)
	}
	var warned bool
	for _, w := range resp.Warnings {
		if strings.Contains(w, "low-confidence") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("expected a low-confidence warning, got %v", resp.Warnings)
	}
}

func TestSearchCodeFullyMatchedQueryIsNotLowConfidence(t *testing.T) {
	idx := buildSearchConfidenceFixture(t)
	resp := SearchCode(idx, "token rotation session", SearchCodeOptions{Limit: 5, BudgetTokens: 800})
	if len(resp.Hits) == 0 {
		t.Fatalf("expected hits for a fully-matched query")
	}
	if resp.Confidence != "" {
		t.Fatalf("fully-matched query must not be low confidence, got %q (warnings=%v)", resp.Confidence, resp.Warnings)
	}
}

func TestSearchCodeSingleTypoWithStrongCoMatchIsNotLowConfidence(t *testing.T) {
	idx := buildSearchConfidenceFixture(t)
	// Three of four terms exist and co-occur on one line; one term is a typo.
	resp := SearchCode(idx, "token rotation session zzqblorf", SearchCodeOptions{Limit: 5, BudgetTokens: 800})
	if len(resp.Hits) == 0 {
		t.Fatalf("expected hits despite one unmatched term")
	}
	if resp.Confidence == "low" {
		t.Fatalf("majority co-match must not be flagged low confidence (warnings=%v)", resp.Warnings)
	}
}
