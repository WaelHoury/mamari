package mamari

import "testing"

func tsFixtureWithCalls(t *testing.T) *Index {
	root := t.TempDir()
	write(t, root, "src/util.ts", `export function helper(): number {
  return 1
}

export function caller(): number {
  return helper() + helper()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestRenameSymbolProducesEditPlanForDefAndCallSites(t *testing.T) {
	idx := tsFixtureWithCalls(t)

	resp, err := RenameSymbol(idx, "helper", "helperRenamed")
	if err != nil {
		t.Fatalf("RenameSymbol: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %#v", resp)
	}
	if resp.Operation != "rename_symbol" {
		t.Fatalf("expected operation rename_symbol, got %q", resp.Operation)
	}
	if resp.Symbol == nil || resp.Symbol.Name != "helper" {
		t.Fatalf("expected symbol helper, got %#v", resp.Symbol)
	}
	if resp.FilesAffected != 1 {
		t.Fatalf("expected 1 file affected, got %d", resp.FilesAffected)
	}
	// One edit for the definition site, plus one for each call to helper().
	if len(resp.Edits) < 3 {
		t.Fatalf("expected at least 3 edits (def + 2 calls), got %d: %#v", len(resp.Edits), resp.Edits)
	}
	for _, e := range resp.Edits {
		if e.File != "src/util.ts" {
			t.Errorf("unexpected file in edit: %#v", e)
		}
		if e.OldText != "helper" {
			t.Errorf("expected OldText 'helper', got %#v", e)
		}
		if e.NewText != "helperRenamed" {
			t.Errorf("expected NewText 'helperRenamed', got %#v", e)
		}
		if e.EndColumn-e.StartColumn != len("helper") {
			t.Errorf("expected edit width matching 'helper', got %#v", e)
		}
	}

	// Definition-site edit should be on line 1, where "helper" is declared.
	foundDef := false
	for _, e := range resp.Edits {
		if e.StartLine == 1 {
			foundDef = true
			if e.Confidence != ConfExact {
				t.Errorf("expected definition-site edit to be ConfExact, got %q", e.Confidence)
			}
		}
	}
	if !foundDef {
		t.Fatalf("expected an edit on line 1 (definition site), got %#v", resp.Edits)
	}
}

func TestRenameSymbolNotFound(t *testing.T) {
	idx := tsFixtureWithCalls(t)

	resp, err := RenameSymbol(idx, "doesNotExist", "whatever")
	if err != nil {
		t.Fatalf("RenameSymbol: %v", err)
	}
	if resp.Status != "not_found" {
		t.Fatalf("expected not_found, got %#v", resp)
	}
	if resp.Edits == nil || len(resp.Edits) != 0 {
		t.Fatalf("expected empty (non-nil) Edits, got %#v", resp.Edits)
	}
}

func TestRenameSymbolAmbiguous(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", "export function shared(): number {\n  return 1\n}\n")
	write(t, root, "src/b.ts", "export function shared(): number {\n  return 2\n}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := RenameSymbol(idx, "shared", "renamed")
	if err != nil {
		t.Fatalf("RenameSymbol: %v", err)
	}
	if resp.Status != "ambiguous" {
		t.Fatalf("expected ambiguous, got %#v", resp)
	}
	if len(resp.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %#v", resp.Candidates)
	}
}

func TestRenameSymbolRejectsInvalidIdentifier(t *testing.T) {
	idx := tsFixtureWithCalls(t)

	if _, err := RenameSymbol(idx, "helper", "not a valid name"); err == nil {
		t.Fatal("expected error for invalid identifier")
	}
	if _, err := RenameSymbol(idx, "helper", "123abc"); err == nil {
		t.Fatal("expected error for identifier starting with a digit")
	}
}

func TestRenameSymbolRejectsNoOpRename(t *testing.T) {
	idx := tsFixtureWithCalls(t)

	if _, err := RenameSymbol(idx, "helper", "helper"); err == nil {
		t.Fatal("expected error when newName equals the existing name")
	}
}

func TestReplaceSymbolBodyProducesSingleWholeRangeEdit(t *testing.T) {
	idx := tsFixtureWithCalls(t)

	newBody := "export function helper(): number {\n  return 42\n}"
	resp, err := ReplaceSymbolBody(idx, "helper", newBody)
	if err != nil {
		t.Fatalf("ReplaceSymbolBody: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %#v", resp)
	}
	if resp.Operation != "replace_symbol_body" {
		t.Fatalf("expected operation replace_symbol_body, got %q", resp.Operation)
	}
	if resp.FilesAffected != 1 {
		t.Fatalf("expected 1 file affected, got %d", resp.FilesAffected)
	}
	if len(resp.Edits) != 1 {
		t.Fatalf("expected exactly 1 edit, got %#v", resp.Edits)
	}
	e := resp.Edits[0]
	if e.File != "src/util.ts" {
		t.Errorf("unexpected file: %q", e.File)
	}
	if e.StartLine != 1 || e.StartColumn != 1 {
		t.Errorf("expected edit to start at 1:1, got %d:%d", e.StartLine, e.StartColumn)
	}
	if e.EndLine != 4 || e.EndColumn != 1 {
		t.Errorf("expected edit to end at 4:1 (one past the symbol's last line), got %d:%d", e.EndLine, e.EndColumn)
	}
	wantOld := "export function helper(): number {\n  return 1\n}\n"
	if e.OldText != wantOld {
		t.Errorf("expected OldText %q, got %q", wantOld, e.OldText)
	}
	if e.NewText != newBody+"\n" {
		t.Errorf("expected NewText to have trailing newline appended, got %q", e.NewText)
	}
}

func TestReplaceSymbolBodyNotFound(t *testing.T) {
	idx := tsFixtureWithCalls(t)

	resp, err := ReplaceSymbolBody(idx, "doesNotExist", "whatever")
	if err != nil {
		t.Fatalf("ReplaceSymbolBody: %v", err)
	}
	if resp.Status != "not_found" {
		t.Fatalf("expected not_found, got %#v", resp)
	}
}

func TestInsertAfterSymbolProducesZeroWidthInsertion(t *testing.T) {
	idx := tsFixtureWithCalls(t)

	resp, err := InsertAfterSymbol(idx, "helper", "// inserted comment")
	if err != nil {
		t.Fatalf("InsertAfterSymbol: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %#v", resp)
	}
	if resp.Operation != "insert_after_symbol" {
		t.Fatalf("expected operation insert_after_symbol, got %q", resp.Operation)
	}
	if len(resp.Edits) != 1 {
		t.Fatalf("expected exactly 1 edit, got %#v", resp.Edits)
	}
	e := resp.Edits[0]
	if e.StartLine != e.EndLine || e.StartColumn != e.EndColumn {
		t.Errorf("expected a zero-width range, got %#v", e)
	}
	if e.StartLine != 4 {
		t.Errorf("expected insertion at line 4 (one past helper's EndLine), got %d", e.StartLine)
	}
	if e.OldText != "" {
		t.Errorf("expected empty OldText for a pure insertion, got %q", e.OldText)
	}
	if e.NewText != "// inserted comment\n" {
		t.Errorf("expected NewText with trailing newline appended, got %q", e.NewText)
	}
}

func TestInsertAfterSymbolAmbiguous(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", "export function shared(): number {\n  return 1\n}\n")
	write(t, root, "src/b.ts", "export function shared(): number {\n  return 2\n}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := InsertAfterSymbol(idx, "shared", "// note")
	if err != nil {
		t.Fatalf("InsertAfterSymbol: %v", err)
	}
	if resp.Status != "ambiguous" {
		t.Fatalf("expected ambiguous, got %#v", resp)
	}
	if len(resp.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %#v", resp.Candidates)
	}
}
