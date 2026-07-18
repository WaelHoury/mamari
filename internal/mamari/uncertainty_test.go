package mamari

import (
	"reflect"
	"strings"
	"testing"
)

func TestUnresolvedSameNameCallsSurfaceAsUncertainty(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/services.js", `export class Alpha {
  run() { return "alpha" }
}
export class Beta {
  run() { return "beta" }
}
export function invoke(service) {
  return service.run()
}
`)
	write(t, root, "test/services.test.js", `test("runs a service", () => {
  const service = getService()
  service.run()
})
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	var runIDs []string
	for id, sym := range idx.Symbols {
		if sym.Name == "run" && sym.Kind == "method" {
			runIDs = append(runIDs, id)
		}
	}
	if len(runIDs) != 2 {
		t.Fatalf("expected two ambiguous run methods, got %v", runIDs)
	}

	for _, id := range runIDs {
		trace := TraceSymbolWithOptions(idx, id, TraceSymbolOptions{Sites: true})
		if trace.Status != "found" || len(trace.PossibleCallers) == 0 || len(trace.PossibleSites) == 0 {
			t.Fatalf("expected possible unresolved callers for %s, got %#v", id, trace)
		}
		if len(trace.Callers) != 0 {
			t.Fatalf("unresolved callers must not be promoted to resolved callers: %#v", trace.Callers)
		}
		if !strings.Contains(strings.Join(trace.Warnings, " "), "not promoted") {
			t.Fatalf("expected explicit uncertainty warning, got %#v", trace.Warnings)
		}

		tests := TestsFor(idx, id, 0)
		if len(tests.Tests) != 0 || len(tests.PossibleTests) == 0 {
			t.Fatalf("expected possible, not proven, tests for %s: %#v", id, tests)
		}
	}

	dead := DeadCode(idx, DeadCodeOptions{Kinds: []string{"method"}, IncludeExported: true})
	if dead.UncertainSkipped < 2 {
		t.Fatalf("expected ambiguous methods to be omitted from dead-code claims, got %#v", dead)
	}
	for _, sym := range dead.Symbols {
		if sym.Name == "run" {
			t.Fatalf("uncertain run method reported as dead: %#v", dead)
		}
	}

	untested := UntestedSymbols(idx, UntestedSymbolsOptions{Kinds: []string{"method"}})
	if untested.UncertainSkipped < 2 {
		t.Fatalf("expected ambiguous methods to be omitted from untested claims, got %#v", untested)
	}
	for _, sym := range untested.Symbols {
		if sym.Name == "run" {
			t.Fatalf("uncertain run method reported as untested: %#v", untested)
		}
	}
}

func TestSymbolTraceBatchMatchesIndependentTraces(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/services.js", `export class Alpha {
  run() { return "alpha" }
}
export class Beta {
  run() { return "beta" }
}
export function direct(alpha) {
  return alpha.run()
}
export function uncertain(service) {
  return service.run()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	var targets []CGPSymbol
	for _, sym := range idx.Symbols {
		if sym.Name == "run" && sym.Kind == "method" {
			targets = append(targets, sym)
		}
	}
	if len(targets) != 2 {
		t.Fatalf("expected two run methods, got %#v", targets)
	}

	snap := idx.symbolGraphSnapshot()
	batch := buildSymbolTraceBatch(snap, targets, true)
	for _, target := range targets {
		want := traceSymbolFromSnapshot(target, snap, TraceSymbolOptions{Sites: true, WithEdges: true})
		got := traceSymbolFromBatch(target, snap, TraceSymbolOptions{Sites: true, WithEdges: true}, batch)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("batched trace differs for %s:\ngot:  %#v\nwant: %#v", target.ID, got, want)
		}
	}
}

func TestSymbolTraceBatchCanSkipUnusedUnresolvedCandidates(t *testing.T) {
	target := CGPSymbol{ID: "target", Name: "run", Kind: "method", Language: "javascript"}
	caller := CGPSymbol{ID: "caller", Name: "invoke", Kind: "function", Language: "javascript"}
	snap := symbolGraphSnapshot{
		Symbols: map[string]CGPSymbol{target.ID: target, caller.ID: caller},
		SymbolEdges: []CGPEdge{{
			From: caller.ID, To: "unresolved:service.run", Type: "calls", Confidence: ConfUnresolved,
		}},
	}

	withPossible := traceSymbolFromBatch(target, snap, TraceSymbolOptions{Sites: true}, buildSymbolTraceBatch(snap, []CGPSymbol{target}, true))
	if len(withPossible.PossibleCallers) != 1 || len(withPossible.PossibleSites) != 1 {
		t.Fatalf("expected unresolved candidates in full trace, got %#v", withPossible)
	}
	withoutPossible := traceSymbolFromBatch(target, snap, TraceSymbolOptions{Sites: true}, buildSymbolTraceBatch(snap, []CGPSymbol{target}, false))
	if len(withoutPossible.PossibleCallers) != 0 || len(withoutPossible.PossibleSites) != 0 {
		t.Fatalf("expected unresolved candidates to be skipped, got %#v", withoutPossible)
	}
}

func TestCompactTraceCountsButOmitsUnresolvedCandidateDetails(t *testing.T) {
	target := CGPSymbol{ID: "target", Name: "run", Kind: "method", Language: "javascript"}
	caller := CGPSymbol{ID: "caller", Name: "invoke", Kind: "function", Language: "javascript"}
	snap := symbolGraphSnapshot{
		Symbols: map[string]CGPSymbol{target.ID: target, caller.ID: caller},
		SymbolEdges: []CGPEdge{{
			From: caller.ID, To: "unresolved:service.run", Type: "calls", Confidence: ConfUnresolved,
		}},
	}
	trace := traceSymbolFromBatch(
		target,
		snap,
		TraceSymbolOptions{Sites: true, Compact: true},
		buildSymbolTraceBatch(snap, []CGPSymbol{target}, true),
	)
	if len(trace.PossibleCallers) != 0 || len(trace.PossibleSites) != 0 {
		t.Fatalf("compact trace should omit unresolved detail arrays, got %#v", trace)
	}
	if trace.PossibleCount != 1 {
		t.Fatalf("compact trace should preserve unresolved count, got %#v", trace)
	}
	if !strings.Contains(strings.Join(trace.Warnings, " "), "1 unresolved") {
		t.Fatalf("compact trace should preserve the uncertainty count, got %#v", trace.Warnings)
	}
}

func TestCompactQualifiedOverloadTraceSummarizesPossibleCallersOnce(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/main/java/example/algebra/Algebra.java", `package example.algebra;
public class Algebra {
    public static void compile(String query) {}
    public static void compile(int expression) {}
}`)
	write(t, root, "src/main/java/example/engine/QueryEngine.java", `package example.engine;
import example.algebra.Algebra;
public class QueryEngine {
    public void createPlan(String query) { Algebra.compile(query); }
}`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	trace := TraceSymbolWithOptions(idx, "Algebra.compile", TraceSymbolOptions{Compact: true})
	if trace.Status != "ambiguous" || len(trace.Candidates) != 2 {
		t.Fatalf("expected the two overloads, got %#v", trace)
	}
	if trace.PossibleCount == 0 || len(trace.PossibleCallers) != 1 || trace.PossibleCallers[0].Name != "createPlan" {
		t.Fatalf("expected one shared possible-caller summary for the overload family, got %#v", trace)
	}
	for _, detail := range trace.CandidateDetails {
		if len(detail.PossibleCallers) != 0 {
			t.Fatalf("possible callers should be emitted once at family level, got %#v", trace.CandidateDetails)
		}
	}
}
