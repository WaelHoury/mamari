package mamari

import "testing"

func TestLimitDoctorParseFailuresPreservesTotalAndOriginal(t *testing.T) {
	original := DoctorReport{
		ParseFailures: []DoctorParseFailure{
			{File: "a.kt"},
			{File: "b.kt"},
			{File: "c.kt"},
		},
		ParseFailureTotal: 3,
	}

	limited := LimitDoctorParseFailures(original, 2)
	if len(limited.ParseFailures) != 2 {
		t.Fatalf("limited parse failures = %d, want 2", len(limited.ParseFailures))
	}
	if limited.ParseFailureTotal != 3 || !limited.ParseFailuresTruncated {
		t.Fatalf("limited metadata = %#v, want total=3 and truncated=true", limited)
	}
	if len(original.ParseFailures) != 3 || original.ParseFailuresTruncated {
		t.Fatalf("original report was mutated: %#v", original)
	}

	full := LimitDoctorParseFailures(original, 0)
	if len(full.ParseFailures) != 3 || full.ParseFailuresTruncated {
		t.Fatalf("zero limit should preserve all failures: %#v", full)
	}
}
