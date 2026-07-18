package mamari

import (
	"strings"
	"testing"
)

func buildReportFixture(t *testing.T) *Index {
	t.Helper()
	root := t.TempDir()
	// hub: called by many (blast radius), tested via hub.test.js.
	write(t, root, "hub.js", `function hub(x) {
  return x + 1
}
function caller1() { return hub(1) }
function caller2() { return hub(2) }
function caller3() { return hub(3) }
function caller4() { return hub(4) }
function caller5() { return hub(5) }
function caller6() { return hub(6) }
module.exports = { hub, caller1 }
`)
	write(t, root, "hub.test.js", `const { hub } = require('./hub')
test('hub', () => { hub(0) })
`)
	// complex: high-complexity hotspot, untested.
	write(t, root, "complex.js", `function veryComplex(a, b, c) {
  if (a) { if (b) { return 1 } else { return 2 } }
  if (b && c) { return 3 }
  if (a || c) { return 4 }
  for (const x of [1,2,3]) { if (x > a) { return x } }
  while (a < b) { a += 1; if (a === c) { break } }
  switch (c) { case 1: return 5; case 2: return 6; default: break }
  if (a > 1 && b > 2 && c > 3) { return 7 }
  if (a < 0 || b < 0 || c < 0) { return 8 }
  return 9
}
module.exports = { veryComplex }
`)
	// orphan: genuinely dead.
	write(t, root, "orphan.js", `function totallyOrphaned() {
  return 42
}
`)
	// clones: structural duplication.
	write(t, root, "cloneA.js", `function sumInvoices(items) {
  let total = 0
  for (const item of items) {
    if (item.active) {
      total = total + item.price * item.qty
    }
  }
  return total
}
module.exports = { sumInvoices }
`)
	write(t, root, "cloneB.js", `function sumOrders(rows) {
  let acc = 0
  for (const row of rows) {
    if (row.enabled) {
      acc = acc + row.cost * row.count
    }
  }
  return acc
}
module.exports = { sumOrders }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestReportAggregatesCoreSignals(t *testing.T) {
	idx := buildReportFixture(t)
	r := Report(idx, ReportOptions{})
	if r.Symbols == 0 || r.Edges == 0 || r.Files == 0 {
		t.Fatalf("inventory must be populated: %+v", r)
	}
	if r.EdgesByConfidence[ConfExact] == 0 {
		t.Fatalf("expected exact edges in fixture, got %v", r.EdgesByConfidence)
	}
	if r.DeadCode == 0 {
		t.Fatalf("totallyOrphaned should be dead; DeadCode=%d", r.DeadCode)
	}
	if r.TestableSymbols == 0 || r.UntestedSymbols == 0 {
		t.Fatalf("expected testable + untested symbols, got %d/%d", r.UntestedSymbols, r.TestableSymbols)
	}
	if r.UntestedPct <= 0 || r.UntestedPct > 100 {
		t.Fatalf("untestedPct out of range: %v", r.UntestedPct)
	}
	if r.DuplicationClusters == 0 {
		t.Fatalf("sumInvoices/sumOrders should cluster; got %d", r.DuplicationClusters)
	}
	foundComplex := false
	for _, h := range r.ComplexityHotspots {
		if h.Name == "veryComplex" {
			foundComplex = true
		}
	}
	if !foundComplex {
		t.Fatalf("veryComplex should be a complexity hotspot; got %+v", r.ComplexityHotspots)
	}
	foundHub := false
	for _, h := range r.BlastRadiusTop {
		if h.Name == "hub" && h.Value >= 6 {
			foundHub = true
		}
	}
	if !foundHub {
		t.Fatalf("hub should top blast radius with >=6 proven callers; got %+v", r.BlastRadiusTop)
	}
}

func TestReportGates(t *testing.T) {
	idx := buildReportFixture(t)
	r := Report(idx, ReportOptions{})

	// Passing gate.
	v, err := EvaluateReportGates(r, "dead<=1000,untested-pct<=100")
	if err != nil || len(v) != 0 {
		t.Fatalf("expected pass, got v=%v err=%v", v, err)
	}
	// Failing gate.
	v, err = EvaluateReportGates(r, "dead<=0")
	if err != nil || len(v) != 1 || v[0].Metric != "dead" {
		t.Fatalf("expected one dead violation, got v=%v err=%v", v, err)
	}
	// Unknown metric fails loudly (a CI gate must never silently pass).
	if _, err = EvaluateReportGates(r, "bogus<=1"); err == nil || !strings.Contains(err.Error(), "unknown metric") {
		t.Fatalf("expected unknown-metric error, got %v", err)
	}
	// Malformed expression fails loudly.
	if _, err = EvaluateReportGates(r, "dead<"); err == nil {
		t.Fatalf("expected malformed-gate error")
	}
	// Empty expression = no gating.
	if v, err := EvaluateReportGates(r, ""); err != nil || v != nil {
		t.Fatalf("empty gate must be a no-op, got v=%v err=%v", v, err)
	}
}

// NaN/Inf thresholds must fail loudly, never silently disable a gate.
func TestReportGateRejectsNonFinite(t *testing.T) {
	idx := buildReportFixture(t)
	r := Report(idx, ReportOptions{})
	for _, expr := range []string{"dead<=NaN", "dead<=Inf", "dead<=-Inf"} {
		if _, err := EvaluateReportGates(r, expr); err == nil {
			t.Fatalf("gate %q must error, not silently pass", expr)
		}
	}
}

// Blast radius counts DISTINCT callers (dedup by caller symbol), matching
// reviewCallers semantics — multiple call sites in one function are 1 caller.
func TestReportBlastRadiusCountsDistinctCallers(t *testing.T) {
	root := t.TempDir()
	write(t, root, "m.js", `function target(x) { return x }
function multi() { return target(1) + target(2) + target(3) }
function single() { return target(4) }
module.exports = { target, multi, single }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	r := Report(idx, ReportOptions{})
	for _, h := range r.BlastRadiusTop {
		if h.Name == "target" {
			if h.Value != 2 {
				t.Fatalf("target has 2 distinct callers (multi, single), got %d", h.Value)
			}
			return
		}
	}
	t.Fatalf("target missing from blast radius: %+v", r.BlastRadiusTop)
}
