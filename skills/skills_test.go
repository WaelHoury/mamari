package skills

import "testing"

func TestBundledCodeReviewSkillPresent(t *testing.T) {
	s, ok := Get("code-review")
	if !ok {
		t.Fatal("code-review skill must be bundled")
	}
	if len(s.Content) < 500 {
		t.Fatalf("embedded skill content looks empty/truncated (%d bytes)", len(s.Content))
	}
	// The embedded content must be the real SKILL.md, not a placeholder.
	if want := "name: code-review"; !contains(s.Content, want) {
		t.Fatalf("embedded skill missing frontmatter %q", want)
	}
}

func TestNamesSortedAndNonEmpty(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("expected at least one bundled skill")
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("Names() not sorted: %v", names)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
