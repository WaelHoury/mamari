package mamari

import "testing"

// #2/#9: optional-chaining calls must keep the receiver so they resolve dotted.
func TestOptionalChainingPreservesReceiver(t *testing.T) {
	cases := map[string]string{
		"a?.foo()":   "a.foo",
		"a.b?.foo()": "a.b.foo",
		"a?.b.foo()": "a.b.foo",
		"a?.b?.c()":  "a.b.c",
	}
	for src, want := range cases {
		res := ParseJS("function z() { " + src + " }")
		found := false
		for _, c := range res.Calls {
			if c.Callee == want {
				found = true
			}
		}
		if !found {
			var got []string
			for _, c := range res.Calls {
				got = append(got, c.Callee)
			}
			t.Fatalf("%q: expected callee %q, got %v", src, want, got)
		}
	}
}

// #16: a comment between `new` and the constructor must not hide construction.
func TestNewWithInterveningCommentIsConstructor(t *testing.T) {
	res := ParseJS("function z() { const x = new /* c */ Widget() }")
	for _, c := range res.Calls {
		if c.Callee == "Widget" {
			if !c.Constructor {
				t.Fatalf("new /* c */ Widget() must be a constructor call")
			}
			return
		}
	}
	t.Fatalf("Widget call not found; calls=%+v", res.Calls)
}

// #3: an lcov SF path that matches no indexed file by suffix must NOT be
// mapped onto an unrelated file that merely shares its basename.
func TestCoverageNoSoleBasenameMismap(t *testing.T) {
	indexed := map[string]bool{"backend/services/index.js": true}
	// SF references a different package's index.js — no suffix match exists.
	cov := parseLCOV([]byte("SF:frontend/pages/index.js\nDA:1,1\nend_of_record\n"), "/repo", indexed)
	if _, ok := cov.byFile["backend/services/index.js"]; ok {
		t.Fatalf("coverage for frontend/pages/index.js must not map onto backend/services/index.js; got %#v", cov.byFile)
	}
	if len(cov.byFile) != 0 {
		t.Fatalf("expected no resolved files, got %#v", cov.byFile)
	}
}

// Guard: a genuine unique-suffix match still resolves (the legit case).
func TestCoverageSuffixStillResolves(t *testing.T) {
	indexed := map[string]bool{"backend/services/x.js": true}
	cov := parseLCOV([]byte("SF:services/x.js\nDA:1,1\nend_of_record\n"), "/repo", indexed)
	if _, ok := cov.byFile["backend/services/x.js"]; !ok {
		t.Fatalf("unique-suffix match must still resolve; got %#v", cov.byFile)
	}
}
