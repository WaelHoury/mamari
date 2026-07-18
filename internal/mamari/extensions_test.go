package mamari

import "testing"

// TestComplexityScoring verifies that annotateComplexity (wired into
// BuildIndex) assigns a complexity score of 1 to a straight-line function and
// a higher score to a function with branches, loops, and boolean operators.
func TestComplexityScoring(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/complexity.ts", `export function simple() {
  return 1
}

export function branchy(x) {
  if (x > 0) {
    return 1
  } else if (x < 0) {
    return -1
  }
  for (let i = 0; i < 10; i++) {
    if (i % 2 === 0 && x > 0) {
      continue
    }
  }
  return 0
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	simple := findSymbolByName(idx, "simple")
	if simple.ID == "" {
		t.Fatal("expected to find symbol 'simple'")
	}
	if simple.Complexity != 1 {
		t.Fatalf("expected simple complexity 1, got %d", simple.Complexity)
	}
	branchy := findSymbolByName(idx, "branchy")
	if branchy.ID == "" {
		t.Fatal("expected to find symbol 'branchy'")
	}
	if branchy.Complexity <= simple.Complexity {
		t.Fatalf("expected branchy complexity > simple, got branchy=%d simple=%d", branchy.Complexity, simple.Complexity)
	}
	if branchy.Complexity != 6 {
		t.Fatalf("expected branchy complexity 6 (if, else if, for, if, &&), got %d", branchy.Complexity)
	}

	// list_symbols / file_outline must surface the computed score.
	summary := summarizeSymbol(branchy)
	if summary.Complexity != branchy.Complexity {
		t.Fatalf("expected summary.Complexity == symbol.Complexity, got %d vs %d", summary.Complexity, branchy.Complexity)
	}
}

// TestDeadCode verifies that an unreferenced, non-exported top-level function
// is reported by DeadCode, while a function reachable via a "calls" edge is
// not.
func TestDeadCode(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/dead.ts", `function used() {
  return 1
}

function unused() {
  return 2
}

function caller() {
  return used()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %q", resp.Status)
	}

	byName := map[string]bool{}
	for _, sym := range resp.Symbols {
		byName[sym.Name] = true
	}
	if !byName["unused"] {
		t.Fatalf("expected 'unused' to be reported as dead code, got %#v", resp.Symbols)
	}
	if byName["used"] {
		t.Fatalf("did not expect 'used' to be reported as dead code (it has a caller), got %#v", resp.Symbols)
	}
}

// TestDeadCodeExcludesExportedByDefault verifies the conservative default:
// exported symbols (potential public API) are not flagged as dead code unless
// IncludeExported is set.
func TestDeadCodeExcludesExportedByDefault(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/api.ts", `export function publicApi() {
  return 1
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{})
	for _, sym := range resp.Symbols {
		if sym.Name == "publicApi" {
			t.Fatalf("did not expect exported symbol to be flagged by default, got %#v", resp.Symbols)
		}
	}
	resp = DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	found := false
	for _, sym := range resp.Symbols {
		if sym.Name == "publicApi" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected exported symbol to be flagged with IncludeExported=true, got %#v", resp.Symbols)
	}
}

// TestTestsForAndUntestedSymbols verifies that TestsFor finds a test callback
// that transitively calls a symbol, and that UntestedSymbols flags a sibling
// symbol with no test caller while not flagging the tested one.
func TestTestsForAndUntestedSymbols(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/math.ts", `export function add(a, b) {
  return a + b
}

export function sub(a, b) {
  return a - b
}
`)
	write(t, root, "src/math.test.ts", `import { add } from './math'

test('adds numbers', () => {
  add(1, 2)
})
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	tf := TestsFor(idx, "add", 0)
	if tf.Status != "found" {
		t.Fatalf("expected status found, got %q", tf.Status)
	}
	foundTest := false
	for _, sym := range tf.Tests {
		if sym.File == "src/math.test.ts" {
			foundTest = true
		}
	}
	if !foundTest {
		t.Fatalf("expected tests_for(add) to include src/math.test.ts, got %#v", tf.Tests)
	}

	untested := UntestedSymbols(idx, UntestedSymbolsOptions{})
	byName := map[string]bool{}
	for _, sym := range untested.Symbols {
		byName[sym.Name] = true
	}
	if !byName["sub"] {
		t.Fatalf("expected 'sub' to be untested, got %#v", untested.Symbols)
	}
	if byName["add"] {
		t.Fatalf("did not expect 'add' to be untested (it has a test caller), got %#v", untested.Symbols)
	}
}

// TestFileOutline verifies that FileOutline returns a file's top-level
// symbols with class methods nested as children.
func TestFileOutline(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/widget.ts", `export class Widget {
  render() {
    return 1
  }
}

export function helper() {
  return 2
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := FileOutline(idx, "src/widget.ts", FileOutlineOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %q", resp.Status)
	}
	var widget *OutlineSymbol
	var helper *OutlineSymbol
	for i := range resp.Symbols {
		switch resp.Symbols[i].Name {
		case "Widget":
			widget = &resp.Symbols[i]
		case "helper":
			helper = &resp.Symbols[i]
		}
	}
	if widget == nil {
		t.Fatalf("expected top-level 'Widget' symbol, got %#v", resp.Symbols)
	}
	if helper == nil {
		t.Fatalf("expected top-level 'helper' symbol, got %#v", resp.Symbols)
	}
	foundRender := false
	for _, child := range widget.Children {
		if child.Name == "render" {
			foundRender = true
		}
	}
	if !foundRender {
		t.Fatalf("expected Widget.Children to include 'render', got %#v", widget.Children)
	}
}

// TestFileOutlineNotFound verifies the not_found status for a file that
// isn't part of the index.
func TestFileOutlineNotFound(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/widget.ts", "export function helper() {\n  return 1\n}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := FileOutline(idx, "src/does-not-exist.ts", FileOutlineOptions{})
	if resp.Status != "not_found" {
		t.Fatalf("expected status not_found, got %q", resp.Status)
	}
}

// TestNotesAddListRemove exercises the add_note/list_notes/remove_note
// lifecycle, including the atomic .mamari/notes.json sidecar persistence and
// symbol-id validation.
func TestNotesAddListRemove(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", "export function foo() {\n  return 1\n}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	sym := findSymbolByName(idx, "foo")
	if sym.ID == "" {
		t.Fatal("expected to find symbol 'foo'")
	}

	if _, err := AddNote(idx, root, "symbol:does-not-exist", "should fail"); err == nil {
		t.Fatal("expected error for unknown symbol id")
	}

	added, err := AddNote(idx, root, sym.ID, "  has a known issue  ")
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}
	if added.Status != "ok" {
		t.Fatalf("expected status ok, got %q", added.Status)
	}
	if added.Note.Text != "has a known issue" {
		t.Fatalf("expected trimmed note text, got %q", added.Note.Text)
	}

	list, err := ListNotes(root, sym.ID)
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if list.Total != 1 || len(list.Notes) != 1 {
		t.Fatalf("expected 1 note, got %#v", list)
	}
	if list.Notes[0].ID != added.Note.ID {
		t.Fatalf("expected note id %d, got %d", added.Note.ID, list.Notes[0].ID)
	}

	// Re-load fresh from disk to confirm persistence survived round-trip.
	nf, err := LoadNotes(root)
	if err != nil {
		t.Fatalf("LoadNotes: %v", err)
	}
	if len(nf.Notes) != 1 {
		t.Fatalf("expected 1 persisted note, got %#v", nf.Notes)
	}

	rm, err := RemoveNote(root, added.Note.ID)
	if err != nil {
		t.Fatalf("RemoveNote: %v", err)
	}
	if !rm.Removed {
		t.Fatalf("expected note to be removed, got %#v", rm)
	}

	list, err = ListNotes(root, sym.ID)
	if err != nil {
		t.Fatalf("ListNotes after remove: %v", err)
	}
	if list.Total != 0 {
		t.Fatalf("expected 0 notes after removal, got %#v", list)
	}

	// Removing again is a no-op, not an error.
	rm, err = RemoveNote(root, added.Note.ID)
	if err != nil {
		t.Fatalf("RemoveNote (again): %v", err)
	}
	if rm.Removed {
		t.Fatalf("expected second removal to be a no-op, got %#v", rm)
	}
}
