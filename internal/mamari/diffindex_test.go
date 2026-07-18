package mamari

import "testing"

func TestDiffIndexDetectsAddedRemovedAndChangedSymbols(t *testing.T) {
	baseRoot := t.TempDir()
	write(t, baseRoot, "src/util.ts", `export function helper(): number {
  return 1
}

export function removedFn(): number {
  return 2
}
`)
	base, err := BuildIndex(baseRoot)
	if err != nil {
		t.Fatal(err)
	}

	headRoot := t.TempDir()
	write(t, headRoot, "src/util.ts", `export function helper(): number {
  const x = 1
  return x
}

export function addedFn(): number {
  return 3
}
`)
	head, err := BuildIndex(headRoot)
	if err != nil {
		t.Fatal(err)
	}

	resp := DiffIndex(base, head)
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %#v", resp)
	}

	if resp.Summary.SymbolsAdded == 0 {
		t.Errorf("expected at least one added symbol, got summary %#v", resp.Summary)
	}
	foundAdded := false
	for _, s := range resp.SymbolsAdded {
		if s.Name == "addedFn" {
			foundAdded = true
		}
	}
	if !foundAdded {
		t.Errorf("expected addedFn in SymbolsAdded, got %#v", resp.SymbolsAdded)
	}

	if resp.Summary.SymbolsRemoved == 0 {
		t.Errorf("expected at least one removed symbol, got summary %#v", resp.Summary)
	}
	foundRemoved := false
	for _, s := range resp.SymbolsRemoved {
		if s.Name == "removedFn" {
			foundRemoved = true
		}
	}
	if !foundRemoved {
		t.Errorf("expected removedFn in SymbolsRemoved, got %#v", resp.SymbolsRemoved)
	}

	if resp.Summary.SymbolsChanged == 0 {
		t.Errorf("expected at least one changed symbol, got summary %#v", resp.Summary)
	}
	foundChanged := false
	for _, c := range resp.SymbolsChanged {
		if c.New.Name == "helper" {
			foundChanged = true
			foundEndLine := false
			for _, f := range c.Fields {
				if f == "endLine" {
					foundEndLine = true
				}
			}
			if !foundEndLine {
				t.Errorf("expected 'endLine' in changed fields for helper, got %#v", c.Fields)
			}
		}
	}
	if !foundChanged {
		t.Errorf("expected helper in SymbolsChanged, got %#v", resp.SymbolsChanged)
	}
}

func TestDiffIndexIdenticalIndexesProduceNoDiff(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/util.ts", "export function helper(): number {\n  return 1\n}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := DiffIndex(idx, idx)
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %#v", resp)
	}
	if resp.Summary.SymbolsAdded != 0 || resp.Summary.SymbolsRemoved != 0 || resp.Summary.SymbolsChanged != 0 {
		t.Errorf("expected no symbol diffs, got summary %#v", resp.Summary)
	}
	if resp.Summary.EdgesAdded != 0 || resp.Summary.EdgesRemoved != 0 {
		t.Errorf("expected no edge diffs, got summary %#v", resp.Summary)
	}
	if len(resp.SymbolsAdded) != 0 || len(resp.SymbolsRemoved) != 0 || len(resp.SymbolsChanged) != 0 {
		t.Errorf("expected empty (non-nil) diff slices, got %#v", resp)
	}
	if len(resp.EdgesAdded) != 0 || len(resp.EdgesRemoved) != 0 {
		t.Errorf("expected empty (non-nil) edge slices, got %#v", resp)
	}
}

func TestDiffIndexDetectsAddedAndRemovedEdges(t *testing.T) {
	baseRoot := t.TempDir()
	write(t, baseRoot, "src/util.ts", `export function helper(): number {
  return 1
}

export function caller(): number {
  return 1
}
`)
	base, err := BuildIndex(baseRoot)
	if err != nil {
		t.Fatal(err)
	}

	headRoot := t.TempDir()
	write(t, headRoot, "src/util.ts", `export function helper(): number {
  return 1
}

export function caller(): number {
  return helper()
}
`)
	head, err := BuildIndex(headRoot)
	if err != nil {
		t.Fatal(err)
	}

	resp := DiffIndex(base, head)
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %#v", resp)
	}
	if resp.Summary.EdgesAdded == 0 {
		t.Errorf("expected at least one added edge (caller -> helper call), got summary %#v", resp.Summary)
	}
	foundCallEdge := false
	for _, e := range resp.EdgesAdded {
		if e.Type == "calls" {
			foundCallEdge = true
		}
	}
	if !foundCallEdge {
		t.Errorf("expected a 'calls' edge in EdgesAdded, got %#v", resp.EdgesAdded)
	}
}
