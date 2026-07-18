package mamari

import "testing"

// The honesty guarantee: a symbol that an UNRESOLVED same-name call might reach
// is never asserted dead. With IncludeUncertain it is surfaced in the Uncertain
// list (review-before-removing) rather than silently dropped. Here a bare,
// ambiguous handler() call cannot be pinned to either handler definition, so
// both are held back — only the genuinely-unreferenced invoke() is dead.
func TestDeadCodeUncertainHoldsBackAmbiguouslyReachedSymbols(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", "export function handler() { return 1 }\n")
	write(t, root, "b.js", "export function handler() { return 2 }\n")
	write(t, root, "c.js", "function invoke(x) { return handler(x) }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true, IncludeUncertain: true})
	deadNames := map[string]bool{}
	for _, s := range resp.Symbols {
		deadNames[s.Name] = true
	}
	if deadNames["handler"] {
		t.Fatalf("handler is reachable via an unresolved call and must not be asserted dead: %+v", resp.Symbols)
	}
	if resp.UncertainSkipped < 1 {
		t.Fatalf("expected at least one uncertain-skipped symbol, got %d", resp.UncertainSkipped)
	}
	uncertainHasHandler := false
	for _, s := range resp.Uncertain {
		if s.Name == "handler" {
			uncertainHasHandler = true
		}
	}
	if !uncertainHasHandler {
		t.Fatalf("expected handler in the Uncertain list, got %+v", resp.Uncertain)
	}

	// Without IncludeUncertain the list is omitted but the count still warns.
	lean := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if len(lean.Uncertain) != 0 {
		t.Fatalf("Uncertain must be empty unless IncludeUncertain is set, got %+v", lean.Uncertain)
	}
	if lean.UncertainSkipped != resp.UncertainSkipped {
		t.Fatalf("uncertainSkipped count should not depend on IncludeUncertain: %d vs %d", lean.UncertainSkipped, resp.UncertainSkipped)
	}
}
